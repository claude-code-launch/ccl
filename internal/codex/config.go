package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/claude-code-launch/ccl/internal/fsutil"
	"github.com/claude-code-launch/ccl/internal/process"
	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

type ConfigChange struct {
	Scope   []string `json:"scope,omitempty"`
	Key     string   `json:"key"`
	Before  *string  `json:"before"`
	Applied *string  `json:"applied"`
}
type Overlay struct {
	Path          string         `json:"path"`
	CatalogPath   string         `json:"catalog_path"`
	BaseURL       string         `json:"base_url"`
	Changes       []ConfigChange `json:"changes"`
	ProviderBlock string         `json:"provider_block,omitempty"`
}

const IndependentProviderID = "ccl_proxy"

func Home() (string, error) {
	if directory := strings.TrimSpace(os.Getenv("CODEX_HOME")); directory != "" {
		return filepath.Abs(directory)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// Plan writes only the proxy-owned catalog, never the user's configuration.
// The lifecycle manager journals Changes before Apply touches config.toml.
func Plan(ctx context.Context, g *Gateway, directory string) (*Overlay, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	path := filepath.Join(home, "config.toml")
	data, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	fields, err := configFields(data)
	if err != nil {
		return nil, err
	}
	selected, _ := fields["model_provider"].(string)
	var profileScope []string
	if profile, _ := fields["profile"].(string); profile != "" {
		profiles, _ := fields["profiles"].(map[string]any)
		active, _ := profiles[profile].(map[string]any)
		if active == nil {
			return nil, fmt.Errorf("Codex default profile does not exist")
		}
		if override, ok := active["model_provider"].(string); ok {
			selected = override
		}
		if override, _ := active["model_catalog_json"].(string); override != "" {
			if !g.options.Independent {
				return nil, fmt.Errorf("Codex's default profile overrides model_catalog_json; use its root catalog setting for the shared proxy")
			}
		}
		profileScope = []string{"profiles", profile}
	}
	if !g.options.Independent && selected != "" && selected != "openai" {
		return nil, fmt.Errorf("Codex uses a custom model_provider; select Codex's native OpenAI provider before enabling its CCL proxy")
	}
	if g.options.Independent {
		providers, _ := fields["model_providers"].(map[string]any)
		if _, exists := providers[IndependentProviderID]; exists {
			return nil, fmt.Errorf("Codex provider %q already exists; rename that provider before enabling independent CCL mode", IndependentProviderID)
		}
		g.options.Desktop = false
	}
	if original, _ := fields["openai_base_url"].(string); original != "" {
		target, err := url.Parse(original)
		if err != nil || target.Host == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
			return nil, fmt.Errorf("Codex openai_base_url is not a valid HTTP endpoint")
		}
		g.options.OpenAIURL, g.options.ChatGPTURL = original, original
	}
	native, err := nativeCatalog(ctx, home, fields)
	if err != nil {
		return nil, err
	}
	g.inheritClientToolMode(native)
	combined, err := MergeCatalog(native, g.Models())
	if err != nil {
		return nil, err
	}
	if g.options.Independent {
		combined = Catalog{Models: g.Models()}
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	catalogPath := filepath.Join(directory, "codex-models-"+randomID("")+".json")
	raw, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(catalogPath, append(raw, '\n'), 0o600); err != nil {
		return nil, err
	}
	base := g.BaseURL()
	overlay := &Overlay{Path: path, CatalogPath: catalogPath, BaseURL: base}
	values := map[string]string{"openai_base_url": base, "model_catalog_json": catalogPath}
	if g.options.Independent {
		delete(values, "openai_base_url")
		model, _ := fields["model"].(string)
		if len(profileScope) > 0 {
			if selected, _ := scopeFields(fields, profileScope)["model"].(string); selected != "" {
				model = selected
			}
		}
		if _, ok := g.routes[model]; !ok {
			model, _ = g.models[0]["slug"].(string)
		}
		g.legacyCompaction = make(map[string]Route)
		for _, entry := range native.Models {
			if slug, _ := entry["slug"].(string); slug != "" && !strings.HasPrefix(strings.ToLower(slug), "ccl/") {
				g.legacyCompaction[slug] = g.routes[model]
			}
		}
		values["model_provider"], values["model"] = IndependentProviderID, model
		quote := func(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
		overlay.ProviderBlock = fmt.Sprintf("\n# CCL temporary provider\n[model_providers.%s]\nname = \"CCL\"\nbase_url = %s\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\nexperimental_bearer_token = %s\n# End CCL temporary provider\n", IndependentProviderID, quote(base), quote(g.Token()))
	}
	addChanges := func(scope []string, source map[string]any, values map[string]string) error {
		for key, value := range values {
			var before *string
			if previous, exists := source[key]; exists {
				v, ok := previous.(string)
				if !ok {
					return fmt.Errorf("Codex %s must be a string", key)
				}
				before = &v
			}
			v := value
			overlay.Changes = append(overlay.Changes, ConfigChange{Scope: scope, Key: key, Before: before, Applied: &v})
		}
		return nil
	}
	if err := addChanges(nil, fields, values); err != nil {
		_ = os.Remove(catalogPath)
		return nil, err
	}
	if g.options.Independent && len(profileScope) > 0 {
		if err := addChanges(profileScope, scopeFields(fields, profileScope), values); err != nil {
			_ = os.Remove(catalogPath)
			return nil, err
		}
	}
	sort.Slice(overlay.Changes, func(i, j int) bool {
		return strings.Join(overlay.Changes[i].Scope, ".")+overlay.Changes[i].Key < strings.Join(overlay.Changes[j].Scope, ".")+overlay.Changes[j].Key
	})
	return overlay, nil
}

func readConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("Codex configuration exceeds 4 MiB")
	}
	return data, nil
}
func configFields(data []byte) (map[string]any, error) {
	fields := make(map[string]any)
	if err := toml.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("invalid Codex TOML configuration")
	}
	return fields, nil
}

func FindExecutable() (string, error) {
	if found, err := exec.LookPath("codex"); err == nil {
		return found, nil
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	if runtime.GOOS == "darwin" {
		for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
			for _, app := range []string{"Codex.app", "ChatGPT.app"} {
				candidates = append(candidates, filepath.Join(root, app, "Contents", "Resources", "codex"))
			}
		}
	}
	if runtime.GOOS == "windows" {
		for _, app := range []string{"Codex", "ChatGPT"} {
			candidates = append(candidates, filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", app, "resources", "codex.exe"))
		}
		matches, _ := filepath.Glob(filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WindowsApps", "OpenAI.Codex_*", "codex.exe"))
		candidates = append(candidates, matches...)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("Codex executable not found; install Codex CLI or open the desktop app once to create its model cache")
}

func nativeCatalog(ctx context.Context, home string, fields map[string]any) (Catalog, error) {
	if path, ok := fields["model_catalog_json"].(string); ok && path != "" {
		if !filepath.IsAbs(path) {
			return Catalog{}, fmt.Errorf("Codex model_catalog_json must be an absolute path")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Catalog{}, fmt.Errorf("read existing Codex model catalog: %w", err)
		}
		catalog, err := ParseCatalog(data)
		if err != nil {
			return Catalog{}, err
		}
		return catalog, nil
	}
	if data, err := os.ReadFile(filepath.Join(home, "models_cache.json")); err == nil {
		if catalog, err := ParseCatalog(data); err == nil {
			return catalog, nil
		}
	}
	executable, err := FindExecutable()
	if err != nil {
		return Catalog{}, err
	}
	scratch, err := os.MkdirTemp("", "ccl-codex-catalog-")
	if err != nil {
		return Catalog{}, err
	}
	defer os.RemoveAll(scratch)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := process.Command(executable, "debug", "models", "--bundled")
	command.WaitDelay = time.Second
	command.Env = make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "CODEX_HOME", "OPENAI_API_KEY", "CODEX_API_KEY":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "CODEX_HOME="+scratch)
	command.Dir = scratch
	var output limitedBuffer
	command.Stdout = &output
	if err := command.Start(); err != nil {
		return Catalog{}, fmt.Errorf("read bundled Codex catalog: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return Catalog{}, fmt.Errorf("Codex must support debug models --bundled (update Codex)")
		}
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-done
		return Catalog{}, ctx.Err()
	}
	return ParseCatalog(output.Bytes())
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 16<<20 {
		return 0, fmt.Errorf("Codex catalog exceeds limit")
	}
	return b.Buffer.Write(data)
}

// editRoot changes string-valued root keys by byte ranges. Comments, tables,
// key quoting, CRLFs and every unrelated field keep their original bytes.
func editRoot(data []byte, values map[string]*string) ([]byte, error) {
	return editScope(data, nil, values)
}

func scopeFields(fields map[string]any, scope []string) map[string]any {
	for _, key := range scope {
		fields, _ = fields[key].(map[string]any)
	}
	return fields
}

func editScope(data []byte, scope []string, values map[string]*string) ([]byte, error) {
	if _, err := configFields(data); err != nil {
		return nil, err
	}
	type edit struct {
		start, end  int
		replacement []byte
	}
	var edits []edit
	seen := make(map[string]bool)
	parser := unstable.Parser{}
	parser.Reset(data)
	var currentScope []string
	insertAt := 0
	foundScope := len(scope) == 0
	for parser.NextExpression() {
		node := parser.Expression()
		if node.Kind == unstable.Table || node.Kind == unstable.ArrayTable {
			currentScope = nil
			keys := node.Key()
			for keys.Next() {
				currentScope = append(currentScope, string(keys.Node().Data))
			}
			if slices.Equal(currentScope, scope) {
				foundScope = true
				keys = node.Key()
				keys.Next()
				start := int(keys.Node().Raw.Offset)
				end := bytes.IndexByte(data[start:], '\n')
				if end < 0 {
					insertAt = len(data)
				} else {
					insertAt = start + end + 1
				}
			}
		}
		if !slices.Equal(currentScope, scope) || node.Kind != unstable.KeyValue {
			continue
		}
		key := node.Key()
		if !key.Next() {
			continue
		}
		first := key.Node()
		name := string(first.Data)
		if key.Next() {
			continue
		}
		value, change := values[name]
		if !change {
			continue
		}
		if node.Value().Kind != unstable.String {
			return nil, fmt.Errorf("Codex %s must be a string", name)
		}
		start := int(first.Raw.Offset)
		end := int(node.Value().Raw.Offset + node.Value().Raw.Length)
		var replacement []byte
		if value != nil {
			raw, _ := json.Marshal(*value)
			replacement = append(append([]byte(nil), data[start:int(node.Value().Raw.Offset)]...), raw...)
		}
		edits = append(edits, edit{start: start, end: end, replacement: replacement})
		seen[name] = true
	}
	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("cannot parse Codex configuration")
	}
	if !foundScope {
		return nil, fmt.Errorf("Codex configuration table changed during startup")
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	result := append([]byte(nil), data...)
	for _, item := range edits {
		result = append(append(append([]byte(nil), result[:item.start]...), item.replacement...), result[item.end:]...)
	}
	var prefix strings.Builder
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] && values[key] != nil {
			raw, _ := json.Marshal(*values[key])
			fmt.Fprintf(&prefix, "%s = %s\n", key, raw)
		}
	}
	insert := []byte(prefix.String())
	if len(insert) > 0 && insertAt > 0 && result[insertAt-1] != '\n' {
		insert = append([]byte{'\n'}, insert...)
	}
	result = append(append(append([]byte(nil), result[:insertAt]...), insert...), result[insertAt:]...)
	if _, err := configFields(result); err != nil {
		return nil, err
	}
	return result, nil
}

func withConfig(ctx context.Context, path string, edit func([]byte, map[string]any) ([]byte, error)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := fsutil.AcquireFileLock(ctx, path+".ccl-lock")
	if err != nil {
		return err
	}
	defer unlock()
	data, err := readConfig(path)
	if err != nil {
		return err
	}
	fields, err := configFields(data)
	if err != nil {
		return err
	}
	updated, err := edit(data, fields)
	if err != nil {
		return err
	}
	if bytes.Equal(data, updated) {
		return nil
	}
	return fsutil.WriteFileAtomic(path, updated, 0o600)
}

func (o *Overlay) Apply(ctx context.Context) error {
	if o == nil {
		return nil
	}
	return withConfig(ctx, o.Path, func(data []byte, fields map[string]any) ([]byte, error) {
		if o.ProviderBlock != "" {
			providers, _ := fields["model_providers"].(map[string]any)
			if _, exists := providers[IndependentProviderID]; exists {
				return nil, fmt.Errorf("CCL provider table changed during startup")
			}
		}
		for _, change := range o.Changes {
			current, exists := scopeFields(fields, change.Scope)[change.Key]
			if (change.Before == nil && exists) || (change.Before != nil && current != *change.Before) {
				return nil, fmt.Errorf("Codex %s changed during proxy startup; retry", change.Key)
			}
		}
		var err error
		for _, change := range o.Changes {
			data, err = editScope(data, change.Scope, map[string]*string{change.Key: change.Applied})
			if err != nil {
				return nil, err
			}
		}
		return append(data, []byte(o.ProviderBlock)...), nil
	})
}
func (o *Overlay) Restore(ctx context.Context) error {
	if o == nil {
		return nil
	}
	err := withConfig(ctx, o.Path, func(data []byte, fields map[string]any) ([]byte, error) {
		if o.ProviderBlock != "" {
			data = bytes.Replace(data, []byte(o.ProviderBlock), nil, 1)
		}
		for _, change := range o.Changes {
			current, exists := scopeFields(fields, change.Scope)[change.Key]
			if change.Applied == nil && !exists || change.Applied != nil && current == *change.Applied {
				if len(change.Scope) > 0 && scopeFields(fields, change.Scope) == nil {
					continue
				}
				var err error
				data, err = editScope(data, change.Scope, map[string]*string{change.Key: change.Before})
				if err != nil {
					return nil, err
				}
			}
		}
		return data, nil
	})
	if err != nil {
		return err
	}
	// A user may have deliberately kept the generated catalog after changing
	// the proxy URL. Keep referenced files rather than leaving a broken path.
	data, err := readConfig(o.Path)
	if err != nil {
		return err
	}
	fields, err := configFields(data)
	if err != nil {
		return err
	}
	if !referencesCatalog(fields, o.CatalogPath) {
		if err := os.Remove(o.CatalogPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func referencesCatalog(value any, path string) bool {
	switch item := value.(type) {
	case string:
		return item == path
	case map[string]any:
		for _, child := range item {
			if referencesCatalog(child, path) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if referencesCatalog(child, path) {
				return true
			}
		}
	}
	return false
}
