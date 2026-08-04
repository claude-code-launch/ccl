package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/claude-code-launch/ccl/internal/config"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

type authGroupOptions struct {
	oauthProvider string
	members       string
	providerName  string
}

var (
	authGroupSelectPrompt = runSelect
	authGroupNamePrompt   = runTextPrompt
)

func newAuthGroupCommand() *cobra.Command {
	opts := authGroupOptions{}
	removeYes := false
	cmd := &cobra.Command{
		Use:   "group [name]",
		Short: "Manage homogeneous OAuth account groups",
		Long: `Create a provider backed by multiple accounts on the same subscription backend.

The generated provider reuses the group name by default (ccl oauth group gg ->
provider "gg"). Rename it with ccl mv or --provider-name; ccl identifies groups
by configuration type, not by name. Model mapping is configured once with
ccl map/ccl set, while CPA round-robins across the selected tokens.

Examples:
  ccl oauth group
  ccl oauth group grok1
  ccl oauth group grok1 --provider-name grok-pool
  ccl oauth group grok1 --provider grok --members grok-a,grok-b
  ccl oauth group ls
  ccl oauth group cp grok1 grok2
  ccl oauth group mv grok1 production
  ccl oauth group rm grok1`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runAuthGroupChoose(cmd.OutOrStdout(), opts)
			}
			return runAuthGroupEdit(cmd.OutOrStdout(), args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.oauthProvider, "provider", "", "OAuth backend for a new group (gpt, gemini, grok, copilot, qoder, kimi, kiro, claude)")
	cmd.Flags().StringVar(&opts.members, "members", "", "Comma-separated provider names or credential filenames (non-interactive)")
	cmd.Flags().StringVar(&opts.providerName, "provider-name", "", "Provider name exposed to ccl use (default: group name)")
	removeCmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an auth group and generated provider",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuthGroupRemove(cmd.OutOrStdout(), args[0], removeYes)
		},
	}
	removeCmd.Flags().BoolVarP(&removeYes, "yes", "y", false, "Delete without confirmation")
	cmd.AddCommand(
		&cobra.Command{
			Use:     "ls",
			Aliases: []string{"list"},
			Short:   "List auth groups",
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runAuthGroupList(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:   "cp <source> <target>",
			Short: "Copy an auth group and its model mapping",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runAuthGroupCopy(cmd.OutOrStdout(), args[0], args[1])
			},
		},
		&cobra.Command{
			Use:   "mv <source> <target>",
			Short: "Rename an auth group and generated provider",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runAuthGroupMove(cmd.OutOrStdout(), args[0], args[1])
			},
		},
		removeCmd,
	)
	return cmd
}

func runAuthGroupChoose(out io.Writer, opts authGroupOptions) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	name, selected, err := chooseAuthGroupName(cfg)
	if err != nil {
		return err
	}
	if !selected {
		return nil
	}
	return runAuthGroupEdit(out, name, opts)
}

func chooseAuthGroupName(cfg *provider.Config) (string, bool, error) {
	names := make([]string, 0, len(cfg.AuthGroups))
	for name := range cfg.AuthGroups {
		names = append(names, name)
	}
	sort.Strings(names)

	createLabel := locale.T("+ 新建 Auth Group", "+ Create new auth group")
	items := []string{createLabel}
	labels := make(map[string]string, len(names))
	for _, name := range names {
		group := cfg.AuthGroups[name]
		providerName := groupProviderName(cfg, name)
		label := fmt.Sprintf("%s  ·  %s  ·  %s  ·  %d account(s)",
			name, providerName, group.OAuthProvider, len(group.Credentials))
		if providerName == cfg.ActiveProvider {
			label += " " + locale.T("(当前使用)", "(active)")
		}
		items = append(items, label)
		labels[label] = name
	}
	chosen, err := authGroupSelectPrompt(
		locale.T("选择 Auth Group 或新建:", "Select an auth group or create new:"),
		items,
	)
	if err != nil {
		return "", false, err
	}
	if chosen == "" {
		return "", false, nil
	}
	if chosen != createLabel {
		name, ok := labels[chosen]
		if !ok {
			return "", false, fmt.Errorf("selected auth group no longer exists")
		}
		return name, true, nil
	}
	rawName, err := authGroupNamePrompt(
		locale.T("输入新 Auth Group 名称:", "Enter the new auth group name:"),
		"grok1",
	)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(rawName) == "" {
		return "", false, nil
	}
	name, err := normalizeAuthGroupName(rawName)
	if err != nil {
		return "", false, err
	}
	if _, exists := cfg.AuthGroups[name]; exists {
		return "", false, fmt.Errorf("auth group %q already exists", name)
	}
	return name, true, nil
}

func runAuthGroupList(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	authDir, err := oauthproxy.AuthDir()
	if err != nil {
		return fmt.Errorf("resolve OAuth auth directory: %w", err)
	}
	configPath := config.ConfigPath()
	fmt.Fprintln(out, "Group configuration:")
	fmt.Fprintf(out, "  Name : %s\n", filepath.Base(configPath))
	fmt.Fprintf(out, "  Path : %s\n", configPath)
	if len(cfg.AuthGroups) == 0 {
		fmt.Fprintln(out, "\nNo auth groups. Create one with `ccl oauth group <name>`.")
		return nil
	}
	names := make([]string, 0, len(cfg.AuthGroups))
	for name := range cfg.AuthGroups {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(out, "\nAuth groups:")
	for _, name := range names {
		group := cfg.AuthGroups[name]
		fmt.Fprintf(out, "  %s\n", name)
		fmt.Fprintf(out, "    Provider    : %s\n", groupProviderName(cfg, name))
		fmt.Fprintf(out, "    Backend     : %s\n", group.OAuthProvider)
		fmt.Fprintf(out, "    Credentials : %d\n", len(group.Credentials))
		if len(group.Credentials) == 0 {
			fmt.Fprintln(out, "      (none)")
			continue
		}
		members := append([]string{}, group.Credentials...)
		sort.Strings(members)
		for _, member := range members {
			member = strings.TrimSpace(member)
			fmt.Fprintf(out, "      - %s\n", filepath.Join(authDir, filepath.Base(member)))
		}
	}
	return nil
}

func runAuthGroupEdit(out io.Writer, rawName string, opts authGroupOptions) error {
	name, err := normalizeAuthGroupName(rawName)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load ccl config: %w", err)
	}
	credentials, err := oauthproxy.ListCredentials()
	if err != nil {
		return err
	}
	credentials = enabledCredentials(credentials)
	credentials = applyCredentialProviderAliases(credentials, cfg)
	if len(credentials) == 0 {
		return fmt.Errorf("no OAuth credentials found; run `ccl oauth import` or `ccl oauth <provider>` first")
	}

	group, exists := cfg.AuthGroups[name]
	if opts.oauthProvider != "" {
		target, validateErr := oauthproxy.ValidateLoginProvider(opts.oauthProvider)
		if validateErr != nil {
			return validateErr
		}
		group.OAuthProvider = target
	}
	if group.OAuthProvider == "" && opts.members == "" {
		group.OAuthProvider = inferGroupProvider(name, credentials)
	}

	if opts.members != "" {
		members, providerName, resolveErr := resolveGroupMembers(cfg, credentials, group.OAuthProvider, strings.Split(opts.members, ","))
		if resolveErr != nil {
			return resolveErr
		}
		group.OAuthProvider = providerName
		group.Credentials = members
	} else {
		edited, saved, editErr := runAuthGroupEditor(name, group, credentials)
		if editErr != nil {
			return editErr
		}
		if !saved {
			fmt.Fprintln(out, "Auth group edit cancelled.")
			return nil
		}
		group = edited
	}
	if len(group.Credentials) == 0 {
		return fmt.Errorf("auth group %q must contain at least one credential", name)
	}

	if cfg.AuthGroups == nil {
		cfg.AuthGroups = make(map[string]provider.AuthGroup)
	}
	group.Credentials = uniqueSortedStrings(group.Credentials)
	cfg.AuthGroups[name] = group
	if opts.providerName != "" {
		targetProviderName, normalizeErr := normalizeGroupProviderName(opts.providerName)
		if normalizeErr != nil {
			return normalizeErr
		}
		if err := renameGroupProvider(cfg, name, targetProviderName); err != nil {
			return err
		}
	}
	if err := ensureGroupProvider(cfg, name); err != nil {
		return err
	}
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save auth group: %w", err)
	}
	action := "Updated"
	if !exists {
		action = "Created"
	}
	fmt.Fprintf(out, "%s auth group %q with %d account(s).\n", action, name, len(group.Credentials))
	providerName := groupProviderName(cfg, name)
	fmt.Fprintf(out, "Use it with `ccl use %s`; configure shared models with `ccl map %s`.\n",
		providerName, providerName)
	return nil
}

func runAuthGroupCopy(out io.Writer, rawSource, rawTarget string) error {
	source, err := normalizeAuthGroupName(rawSource)
	if err != nil {
		return err
	}
	target, err := normalizeAuthGroupName(rawTarget)
	if err != nil {
		return err
	}
	if source == target {
		return fmt.Errorf("source and target auth groups must be different")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	group, ok := cfg.AuthGroups[source]
	if !ok {
		return fmt.Errorf("auth group %q not found", source)
	}
	if _, exists := cfg.AuthGroups[target]; exists {
		return fmt.Errorf("auth group %q already exists", target)
	}
	targetProviderName := defaultGroupProviderName(target)
	if _, exists := cfg.Providers[targetProviderName]; exists {
		return fmt.Errorf("provider %q already exists", targetProviderName)
	}
	group.Credentials = append([]string{}, group.Credentials...)
	cfg.AuthGroups[target] = group
	sourceProviderName := groupProviderName(cfg, source)
	if sourceProvider, ok := cfg.Providers[sourceProviderName]; ok {
		cloned := cloneProvider(sourceProvider, targetProviderName)
		cloned.AuthGroup = target
		cloned.OAuthAccountCredentials = append([]string{}, group.Credentials...)
		cfg.Providers[targetProviderName] = cloned
	} else if err := ensureGroupProvider(cfg, target); err != nil {
		return err
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "Copied auth group %q -> %q.\n", source, target)
	return nil
}

func runAuthGroupMove(out io.Writer, rawSource, rawTarget string) error {
	source, err := normalizeAuthGroupName(rawSource)
	if err != nil {
		return err
	}
	target, err := normalizeAuthGroupName(rawTarget)
	if err != nil {
		return err
	}
	if source == target {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	group, ok := cfg.AuthGroups[source]
	if !ok {
		return fmt.Errorf("auth group %q not found", source)
	}
	if _, exists := cfg.AuthGroups[target]; exists {
		return fmt.Errorf("auth group %q already exists", target)
	}
	sourceProviderName := groupProviderName(cfg, source)
	targetProviderName := sourceProviderName
	if sourceProviderName == defaultGroupProviderName(source) {
		targetProviderName = defaultGroupProviderName(target)
	}
	if targetProviderName != sourceProviderName {
		if _, exists := cfg.Providers[targetProviderName]; exists {
			return fmt.Errorf("provider %q already exists", targetProviderName)
		}
	}
	cfg.AuthGroups[target] = group
	delete(cfg.AuthGroups, source)
	for providerName, p := range cfg.Providers {
		if p.AuthGroup == source && providerName != sourceProviderName {
			p.AuthGroup = target
			p.OAuthAccountCredentials = append([]string{}, group.Credentials...)
			cfg.Providers[providerName] = p
		}
	}
	oldProvider, hadProvider := cfg.Providers[sourceProviderName]
	if hadProvider {
		delete(cfg.Providers, sourceProviderName)
		oldProvider.Name = targetProviderName
		oldProvider.AuthGroup = target
		oldProvider.OAuthAccountCredentials = append([]string{}, group.Credentials...)
		cfg.Providers[targetProviderName] = oldProvider
	} else if err := ensureGroupProvider(cfg, target); err != nil {
		return err
	}
	if cfg.ActiveProvider == sourceProviderName {
		cfg.ActiveProvider = targetProviderName
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "Renamed auth group %q -> %q.\n", source, target)
	return nil
}

func runAuthGroupRemove(out io.Writer, rawName string, force bool) error {
	name, err := normalizeAuthGroupName(rawName)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.AuthGroups[name]; !ok {
		return fmt.Errorf("auth group %q not found", name)
	}
	if !force {
		fmt.Fprintf(out, "Delete auth group %q and provider %q? Credential files will be kept. (y/N): ", name, groupProviderName(cfg, name))
		var answer string
		_, _ = fmt.Scanln(&answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(out, "Auth group deletion cancelled.")
			return nil
		}
	}
	delete(cfg.AuthGroups, name)
	for providerName, p := range cfg.Providers {
		if p.AuthGroup == name {
			delete(cfg.Providers, providerName)
			if cfg.ActiveProvider == providerName {
				cfg.ActiveProvider = ""
			}
		}
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "Deleted auth group %q (credential files were kept).\n", name)
	return nil
}

func normalizeAuthGroupName(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "group-")
	if value == "" {
		return "", fmt.Errorf("auth group name cannot be empty")
	}
	if strings.ContainsAny(value, `/\:`) || strings.Contains(value, "..") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("invalid auth group name %q", value)
	}
	return value, nil
}

func normalizeGroupProviderName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("group provider name cannot be empty")
	}
	if strings.ContainsAny(value, `/\:`) || strings.Contains(value, "..") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("invalid group provider name %q", value)
	}
	return value, nil
}

func renameGroupProvider(cfg *provider.Config, groupName, target string) error {
	source := groupProviderName(cfg, groupName)
	if source == target {
		return nil
	}
	if _, exists := cfg.Providers[target]; exists {
		return fmt.Errorf("provider %q already exists", target)
	}
	if p, exists := cfg.Providers[source]; exists && strings.TrimSpace(p.AuthGroup) == groupName {
		delete(cfg.Providers, source)
		p.Name = target
		p.AuthGroup = groupName
		cfg.Providers[target] = p
		if cfg.ActiveProvider == source {
			cfg.ActiveProvider = target
		}
		return nil
	}
	// Seed the type marker so ensureGroupProvider chooses this custom name
	// instead of creating the default group-name provider.
	cfg.Providers[target] = provider.Provider{Name: target, AuthGroup: groupName}
	return nil
}

func enabledCredentials(credentials []oauthproxy.CredentialInfo) []oauthproxy.CredentialInfo {
	out := make([]oauthproxy.CredentialInfo, 0, len(credentials))
	for _, credential := range credentials {
		if !credential.Disabled {
			out = append(out, credential)
		}
	}
	return out
}

func applyCredentialProviderAliases(credentials []oauthproxy.CredentialInfo, cfg *provider.Config) []oauthproxy.CredentialInfo {
	for i := range credentials {
		for _, p := range cfg.Providers {
			if strings.EqualFold(p.OAuthAccountCredential, credentials[i].FileName) && p.OAuthProvider != "" {
				credentials[i].OAuthProvider = p.OAuthProvider
				break
			}
		}
	}
	return credentials
}

func inferGroupProvider(name string, credentials []oauthproxy.CredentialInfo) string {
	lower := strings.ToLower(name)
	for _, candidate := range []string{"grok", "gemini", "copilot", "qoder", "kimi", "kiro", "claude", "gpt"} {
		if strings.Contains(lower, candidate) {
			return candidate
		}
	}
	providers := uniqueCredentialProviders(credentials)
	if len(providers) == 1 {
		return providers[0]
	}
	return providers[0]
}

func resolveGroupMembers(cfg *provider.Config, credentials []oauthproxy.CredentialInfo, oauthProvider string, rawMembers []string) ([]string, string, error) {
	target := strings.ToLower(strings.TrimSpace(oauthProvider))
	if target != "" {
		validated, err := oauthproxy.ValidateLoginProvider(target)
		if err != nil {
			return nil, "", err
		}
		target = validated
	}
	byFile := make(map[string]oauthproxy.CredentialInfo, len(credentials))
	for _, credential := range credentials {
		byFile[strings.ToLower(credential.FileName)] = credential
	}
	var files []string
	for _, member := range rawMembers {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		file := member
		if p, ok := cfg.Providers[member]; ok && p.OAuthAccountCredential != "" {
			file = p.OAuthAccountCredential
		}
		credential, ok := byFile[strings.ToLower(file)]
		if !ok {
			return nil, "", fmt.Errorf("OAuth credential or provider %q not found", member)
		}
		memberProvider := credential.OAuthProvider
		// Existing Copilot providers retain the public identity that cannot be
		// inferred from the shared codex JSON type.
		for _, p := range cfg.Providers {
			if strings.EqualFold(p.OAuthAccountCredential, credential.FileName) && p.OAuthProvider != "" {
				memberProvider = p.OAuthProvider
				break
			}
		}
		if target == "" {
			target = memberProvider
		}
		targetBackend, _ := oauthproxy.BackendProvider(target)
		if !strings.EqualFold(targetBackend, credential.Backend) {
			return nil, "", fmt.Errorf("credential %q belongs to %s, but group backend is %s", member, credential.Backend, targetBackend)
		}
		files = append(files, credential.FileName)
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("no group members selected")
	}
	return uniqueSortedStrings(files), target, nil
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// authGroupEditorModel is intentionally compact: with dozens of OAuth accounts
// the UI only shows backend, a select-all toggle with counts, and save.
// Model mapping remains in ccl set/map for the generated group provider.
//
// Cursor rows:
//
//	0 backend
//	1 select all / deselect all (with selected/total count)
//	2 save
type authGroupEditorModel struct {
	name          string
	group         provider.AuthGroup
	credentials   []oauthproxy.CredentialInfo
	providers     []string
	selected      map[string]bool
	cursor        int
	saved         bool
	cancelled     bool
	providerIndex int
}

const authGroupEditorSaveCursor = 2

func runAuthGroupEditor(name string, group provider.AuthGroup, credentials []oauthproxy.CredentialInfo) (provider.AuthGroup, bool, error) {
	providers := uniqueCredentialProviders(credentials)
	if existing := strings.TrimSpace(group.OAuthProvider); existing != "" && groupBackendAvailable(existing, credentials) {
		found := false
		for _, value := range providers {
			if value == existing {
				found = true
				break
			}
		}
		if !found {
			providers = append(providers, existing)
			sort.Strings(providers)
		}
	}
	if len(providers) == 0 {
		return group, false, fmt.Errorf("no supported OAuth credentials available")
	}
	index := 0
	for i, value := range providers {
		if value == group.OAuthProvider {
			index = i
			break
		}
	}
	selected := make(map[string]bool)
	for _, file := range group.Credentials {
		selected[strings.ToLower(file)] = true
	}
	// New groups start empty; pre-select every account for the inferred backend
	// so one Save is enough when the user wants the whole pool.
	if len(selected) == 0 {
		backend, _ := oauthproxy.BackendProvider(providers[index])
		for _, credential := range credentials {
			if strings.EqualFold(credential.Backend, backend) {
				selected[strings.ToLower(credential.FileName)] = true
			}
		}
	}
	m := &authGroupEditorModel{
		name: name, group: group, credentials: credentials, providers: providers,
		selected: selected, providerIndex: index, cursor: 1,
	}
	program := tea.NewProgram(m)
	finalModel, err := program.Run()
	if err != nil {
		return group, false, err
	}
	final := finalModel.(*authGroupEditorModel)
	if !final.saved {
		return group, false, nil
	}
	final.group.OAuthProvider = final.providers[final.providerIndex]
	final.group.Credentials = nil
	backend, _ := oauthproxy.BackendProvider(final.group.OAuthProvider)
	for _, credential := range final.credentials {
		if final.selected[strings.ToLower(credential.FileName)] && strings.EqualFold(credential.Backend, backend) {
			final.group.Credentials = append(final.group.Credentials, credential.FileName)
		}
	}
	return final.group, true, nil
}

func groupBackendAvailable(oauthProvider string, credentials []oauthproxy.CredentialInfo) bool {
	backend, err := oauthproxy.BackendProvider(oauthProvider)
	if err != nil {
		return false
	}
	for _, credential := range credentials {
		if strings.EqualFold(credential.Backend, backend) {
			return true
		}
	}
	return false
}

func (m *authGroupEditorModel) Init() tea.Cmd { return nil }

func (m *authGroupEditorModel) visibleCredentials() []oauthproxy.CredentialInfo {
	backend, _ := oauthproxy.BackendProvider(m.providers[m.providerIndex])
	var visible []oauthproxy.CredentialInfo
	for _, credential := range m.credentials {
		if strings.EqualFold(credential.Backend, backend) {
			visible = append(visible, credential)
		}
	}
	return visible
}

func (m *authGroupEditorModel) allVisibleSelected() bool {
	visible := m.visibleCredentials()
	if len(visible) == 0 {
		return false
	}
	for _, credential := range visible {
		if !m.selected[strings.ToLower(credential.FileName)] {
			return false
		}
	}
	return true
}

func (m *authGroupEditorModel) toggleSelectAllVisible() {
	selected := !m.allVisibleSelected()
	for _, credential := range m.visibleCredentials() {
		m.selected[strings.ToLower(credential.FileName)] = selected
	}
}

func (m *authGroupEditorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.cancelled = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < authGroupEditorSaveCursor {
				m.cursor++
			}
		case "left", "h", "[":
			if m.cursor == 0 {
				m.providerIndex = (m.providerIndex - 1 + len(m.providers)) % len(m.providers)
			}
		case "right", "l", "]":
			if m.cursor == 0 {
				m.providerIndex = (m.providerIndex + 1) % len(m.providers)
			}
		case "a", "ctrl+a":
			m.toggleSelectAllVisible()
		case " ", "space", "enter":
			// bubbletea/v2 reports the space bar as "space".
			switch m.cursor {
			case 0:
				m.providerIndex = (m.providerIndex + 1) % len(m.providers)
			case 1:
				m.toggleSelectAllVisible()
			case authGroupEditorSaveCursor:
				if m.selectedCountForBackend() > 0 {
					m.saved = true
					return m, tea.Quit
				}
			}
		case "s":
			if m.selectedCountForBackend() > 0 {
				m.saved = true
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m *authGroupEditorModel) selectedCountForBackend() int {
	count := 0
	for _, credential := range m.visibleCredentials() {
		if m.selected[strings.ToLower(credential.FileName)] {
			count++
		}
	}
	return count
}

func (m *authGroupEditorModel) View() tea.View {
	var body strings.Builder
	body.WriteString(titleStyle.Render("Auth Group: " + m.name))
	body.WriteString("\n\n")

	backendLabel := fmt.Sprintf("Backend  ‹ %s ›", m.providers[m.providerIndex])
	if m.cursor == 0 {
		body.WriteString("> ")
		body.WriteString(selectedStyle.Render(backendLabel))
	} else {
		body.WriteString("  ")
		body.WriteString(purpleText.Render(backendLabel))
	}
	body.WriteString("\n")

	visible := len(m.visibleCredentials())
	selected := m.selectedCountForBackend()
	checked := "[ ]"
	selectAllLabel := locale.T("全选账号", "Select all accounts")
	if m.allVisibleSelected() {
		checked = "[x]"
		selectAllLabel = locale.T("取消全选", "Deselect all")
	} else if selected > 0 {
		checked = "[~]"
		selectAllLabel = locale.T("全选账号", "Select all accounts")
	}
	accountsLine := fmt.Sprintf("%s %s  %d/%d", checked, selectAllLabel, selected, visible)
	if m.cursor == 1 {
		body.WriteString("> ")
		body.WriteString(selectedStyle.Render(accountsLine))
	} else {
		body.WriteString("  ")
		body.WriteString(purpleText.Render(accountsLine))
	}
	body.WriteString("\n\n")

	saveLabel := fmt.Sprintf("[ Save group ]  %d selected", selected)
	if m.cursor == authGroupEditorSaveCursor {
		body.WriteString("> ")
		body.WriteString(selectedStyle.Render(saveLabel))
	} else {
		body.WriteString("  ")
		body.WriteString(purpleText.Render(saveLabel))
	}
	body.WriteString("\n\n")
	body.WriteString(grayText.Render(locale.T(
		"↑↓ 移动 · ←→ 切换后端 · space 全选/全不选 · s 保存 · esc 取消",
		"↑↓ move · ←→ backend · space select/deselect all · s save · esc cancel",
	)))
	return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render(body.String()))
}

func uniqueCredentialProviders(credentials []oauthproxy.CredentialInfo) []string {
	seen := make(map[string]bool)
	var values []string
	for _, credential := range credentials {
		if credential.OAuthProvider != "" && !seen[credential.OAuthProvider] {
			seen[credential.OAuthProvider] = true
			values = append(values, credential.OAuthProvider)
		}
	}
	sort.Strings(values)
	return values
}

func init() {
	authCmd.AddCommand(newAuthGroupCommand())
}
