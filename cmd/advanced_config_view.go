package cmd

import (
	"fmt"
	"strings"

	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/provider"
	tui "github.com/grindlemire/go-tui"
)

// statusSpinners is the frame cycle every in-flight status line shares, so the
// Local Proxy row and the connection check animate at the same cadence.
var statusSpinners = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// span wraps text with a style for tui.WithRichText rows.
func span(text string, st tui.Style) tui.TextSpan {
	return tui.TextSpan{Text: text, Style: st}
}

// line builds one no-wrap row element from styled spans. Rows never wrap so
// the line number of every label stays stable for the mouse hit test.
func line(spans ...tui.TextSpan) *tui.Element {
	return tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Row), tui.WithWrap(false), tui.WithRichText(spans...))
}

// plainLine is line() for unstyled text.
func plainLine(text string) *tui.Element {
	return line(span(text, tui.NewStyle()))
}

func spinnerAt(frame int) string {
	if frame < 0 {
		frame = -frame
	}
	return statusSpinners[frame%len(statusSpinners)]
}

// renderModelFetchProgress builds the connection-check in-progress block: a
// spinner frame, the label, and the hint line.
func renderModelFetchProgress(progress, frame int, oauth bool) []*tui.Element {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	spin := spinnerAt(frame)
	label := locale.T("正在连接...", "Connecting...")
	if oauth {
		label = locale.T("正在通过 OAuth 连接...", "Connecting via OAuth...")
	}
	return []*tui.Element{
		plainLine(""),
		line(span(fmt.Sprintf("%s %s", spin, label), stSelected)),
		spanLine(locale.T("请稍候，正在验证连接", "Please wait while the connection is verified"), stGray),
	}
}

// credentialField renders one credential row: the label line and the value
// line(s). The API key can span multiple lines (its editor accepts Enter);
// each line is its own element so the mouse hit-test rows stay stable. A
// focused field swaps the label for the accent style and the "> " cursor
// prefix, matching the stepper rows below.
func credentialField(label, value string, focused bool) []*tui.Element {
	prefix := "  "
	labelStyle := stPurple
	valueStyle := stGray
	if focused {
		prefix = "> "
		labelStyle = stSelected
		valueStyle = stSelected
	}
	rows := []*tui.Element{
		line(span(prefix, labelStyle), span(label, labelStyle)),
	}
	for _, v := range strings.Split(value, "\n") {
		rows = append(rows, line(span("  ", tui.NewStyle()), span(v, valueStyle)))
	}
	rows = append(rows, plainLine(""))
	return rows
}

// renderPageHeader renders the page title row(s): title + badge (+ protocol
// family until a detection pins it) and a divider rule.
func (m *AdvancedConfigModel) renderPageHeader(title, badge string) []*tui.Element {
	// Leading spaces keep the lipgloss MarginLeft(1) look; go-tui styles have
	// no margin, so the separation lives in the span text itself.
	spans := []tui.TextSpan{span(title, stTitle), span(" "+badge, stBadge)}
	if !m.live().modelPoolFromDiscovery && !m.usesOAuth() {
		// Must be a third span, not a later AddChild: block children appended to
		// a flex Row mis-layout and the text overlaps (see CLAUDE.md go-tui notes).
		spans = append(spans, span(locale.T(" 协议: ", " Protocol: ")+m.getProtocolFamily(), stProtoBadge))
	}
	head := line(spans...)
	dividerWidth := max(m.panelWidth()-6, 16)
	return []*tui.Element{
		head,
		spanLine(strings.Repeat("─", dividerWidth), stDivider),
		plainLine(""),
	}
}

// truncateMiddle keeps endpoint/model names on one line for the review page.
// Width is measured in terminal cells (ANSI-aware, Unicode-aware) via
// tui.StringWidth.
func truncateMiddle(s string, max int) string {
	s = strings.TrimSpace(s)
	if max < 8 || tui.StringWidth(s) <= max {
		return s
	}
	runess := []rune(s)
	ellipsis := "…"
	budget := max - tui.StringWidth(ellipsis)
	if budget < 2 {
		return ellipsis
	}
	leftBudget := budget / 2
	rightBudget := budget - leftBudget

	var left string
	for _, r := range runess {
		cand := left + string(r)
		if tui.StringWidth(cand) > leftBudget {
			break
		}
		left = cand
	}
	var right string
	for i := len(runess) - 1; i >= 0; i-- {
		cand := string(runess[i]) + right
		if tui.StringWidth(cand) > rightBudget {
			break
		}
		right = cand
	}
	return left + ellipsis + right
}

// Render builds the whole page as a go-tui element tree. Three exits: the
// slot picker overlay (filter focused), the models.dev picker overlay, and
// the main configuration page. A nil app is tolerated so tests can render
// offline.
func (m *AdvancedConfigModel) Render(app *tui.App) *tui.Element {
	m.syncTerminalSize(app)
	// Model picker overlay: when the filter input has focus, render only the
	// filtered model list (search + availability) instead of the main page.
	if m.filterFocused {
		return m.viewModelPicker()
	}
	// models.dev picker overlay: render only the provider catalog instead of the
	// main page.
	if m.modelsDevPicker {
		return m.viewModelsDevPicker()
	}
	return m.viewMainPage()
}

// bodyRows builds the full page body (header through the action bar) as an
// ordered list of single-line elements. viewMainPage wraps it in the panel;
// keepCursorVisible reuses it to locate the cursor row's precise body line for
// scrolling. Section renderers are read-only — they only build elements.
func (m *AdvancedConfigModel) bodyRows() []*tui.Element {
	rows := m.renderPageHeader(locale.T("Provider 配置", "Provider Configuration"), m.pageBadge())
	rows = append(rows, m.viewConnectionSection()...)
	rows = append(rows, m.viewDetectionSection()...)
	rows = append(rows, m.viewMappingSection()...)
	rows = append(rows, m.viewRuntimeSection()...)
	rows = append(rows, m.viewActionSection()...)
	return rows
}

func (m *AdvancedConfigModel) viewMainPage() *tui.Element {
	return m.wrapMainPanel(m.bodyRows())
}

// pageBadge labels the header: the connection family this page edits.
func (m *AdvancedConfigModel) pageBadge() string {
	if m.usesOAuth() {
		return "OAuth"
	}
	return "Config"
}

// viewConnectionSection renders the Connection block: subscription metadata
// for OAuth providers, or the Source stepper plus endpoint/key/protocol rows
// for Custom and models.dev sources.
func (m *AdvancedConfigModel) viewConnectionSection() []*tui.Element {
	rows := []*tui.Element{m.connectionTitleRow()}
	if m.usesOAuth() {
		// OAuth rows are subscription metadata. There is no Auto Configure
		// button: the subscription already owns its endpoint, credential, and
		// model catalog, so there is nothing left to detect.
		return append(rows,
			kvRow("Provider", span(m.p.OAuthProvider, stCyan)),
			kvRow("Fast", span(providerFastSummary(*m.p), stCyan)),
			kvRow(locale.T("鉴权", "Auth"), span(providerAuthLabel(*m.p), stAvailable)),
			m.localProxyRow(),
		)
	}

	copiedHint := ""
	if m.keyCopied {
		copiedHint = "  " + locale.T("✓ 已复制", "✓ copied")
	}
	urlCopiedHint := ""
	if m.urlCopied {
		urlCopiedHint = "  " + locale.T("✓ 已复制", "✓ copied")
	}
	// Endpoint and API Key render identically: unfocused they are grey text
	// (so the two fields match), and only when focused do they show the live
	// input view with its cursor. Double-clicking a value row copies the full
	// value. Trailing blank lines from the textarea's fixed height are
	// trimmed so the field does not consume extra rows in the panel.
	const idleWidth = 60

	// Source stepper: single-value ‹ › toggle between Custom and models.dev,
	// styled like the other steppers below (purple when idle, accent when
	// focused, cycling with ←→).
	sourceVal := "models.dev"
	if m.source == sourceCustom {
		sourceVal = locale.T("自定义", "Custom")
	}
	rows = append(rows, stepperRow(locale.T("来源", "Source"), "‹ "+sourceVal+" ›", m.cursor == m.mainRowIndex(rowSource)))

	if m.usesModelsDev() {
		// models.dev: endpoint/protocol come from metadata (read-only). The
		// Provider row opens the catalog picker; only the API key is editable.
		label := strings.TrimSpace(m.p.Name)
		if catalogID := provider.ModelsDevCatalogID(*m.p); label != "" && !strings.EqualFold(catalogID, label) {
			label += "  (" + catalogID + ")"
		}
		name := truncateMiddle(label, idleWidth)
		if name == "" {
			name = locale.T("选择 Provider", "Select provider")
		}
		rows = append(rows, stepperRow("Provider", "‹ "+name+" ›", m.cursor == m.mainRowIndex(rowProvider)))
		rows = append(rows, kvRow(locale.T("端点", "Endpoint"), span(truncateMiddle(m.p.Endpoint, idleWidth), stCyan)))
	} else {
		urlValue := truncateMiddle(m.urlText.Get(), idleWidth) + urlCopiedHint
		rows = append(rows, credentialField(locale.T("端点 URL", "Endpoint URL"), urlValue, m.cursor == m.mainRowIndex(rowEndpoint))...)
	}

	keyValue := truncateMiddle(m.keyText.Get(), idleWidth) + copiedHint
	rows = append(rows, credentialField("API Key", keyValue, m.cursor == m.mainRowIndex(rowAPIKey))...)

	// Protocol moved up from Runtime: Chat/Responses/Anthropic is selectable
	// for a manual Custom gateway; fixed and per-model runtimes stay read-only.
	if m.usesModelsDev() {
		rows = append(rows, kvRow(locale.T("协议", "Protocol"), span("auto/mixed", stAvailable)))
	} else if m.canSelectCustomProtocol() {
		value := customProtocolLabel(m.p.Type)
		rows = append(rows, stepperRow(locale.T("协议", "Protocol"), "‹ "+value+" ›", m.cursor == m.mainRowIndex(rowProtocol)))
	} else {
		rows = append(rows, kvRow(locale.T("协议", "Protocol"), span(m.getProtocol(), stAvailable)))
	}
	// Auth row: how the upstream verifies requests (API key / OAuth binding).
	// For OAuth providers the Connection block is subscription metadata, and
	// the Auth row above already carries this information.
	if m.canSelectCustomProtocol() && provider.IsAnthropicType(m.p.Type) {
		value := m.p.AnthropicAuth
		if value == "" {
			value = "Auto"
		}
		rows = append(rows, stepperRow(locale.T("鉴权", "Auth"), "‹ "+value+" ›", m.cursor == m.mainRowIndex(rowAuth)))
	} else {
		value := providerAuthLabel(*m.p)
		if strings.TrimSpace(m.p.Type) == "" {
			value = "Auto"
		}
		rows = append(rows, kvRow(locale.T("鉴权", "Auth"), span(value, stAvailable)))
	}

	// Custom alone needs a manual Auto Configure action; models.dev verifies
	// credentials automatically and OAuth uses its managed runtime.
	if !m.usesModelsDev() {
		rows = append(rows, plainLine(""), m.actionButtonRow(locale.T("Auto Configure", "Auto Configure"), rowTest))
	}
	return rows
}

// connectionTitleRow renders the section heading with the live connection
// status on the same line: "Connection  ✓ Connected · Chat · 3 models".
// The status appears only once a probe has actually succeeded; while dirty,
// detecting, or failed it stays hidden so the header never shows stale state.
func (m *AdvancedConfigModel) connectionTitleRow() *tui.Element {
	title := span(locale.T("连接", "Connection"), stTitle)
	if m.live().detecting {
		return line(title)
	}
	if m.usesModelsDev() && (!m.live().modelPoolFromDiscovery || !m.live().keyVerified) {
		return line(title)
	}
	if m.live().detectionError != nil {
		return line(title)
	}
	if m.live().modelPoolFromDiscovery {
		status := fmt.Sprintf(locale.T("  ✓ 已连接 · %s · %d 个模型", "  ✓ Connected · %s · %d models"), provider.ProtocolLabelForProvider(*m.p), len(m.live().modelPool))
		return line(title, span(status, stAvailable))
	}
	return line(title)
}

// localProxyRow reports the session-only runtime started for a subscription.
// It is the page's one progress surface for that start: the panel renders
// before the runtime is up, so this row carries the spinner until it answers.
func (m *AdvancedConfigModel) localProxyRow() *tui.Element {
	label := locale.T("本地代理", "Local Proxy")
	switch {
	case m.runtimeLoading:
		return kvRow(label, span(
			fmt.Sprintf("%s %s", spinnerAt(m.runtimeFrame), locale.T("启动中…", "Starting...")),
			stSelected,
		))
	case m.runtimeErr != nil:
		return kvRow(label, span(locale.T("启动失败（仅本次会话）", "Start failed (this session only)"), stUnavailable))
	case m.runtimeCatalogFallback:
		// Up, but serving a guess. Saying so is the point: otherwise the page
		// looks healthy while the model list underneath it is wrong.
		return kvRow(label, span(locale.T("已就绪 · 模型列表获取失败，沿用已保存的", "Ready · model list unavailable, keeping the saved one"), stUnavailable))
	default:
		return kvRow(label, span(locale.T("已就绪（仅本次会话）", "Ready (this session only)"), stAvailable))
	}
}

// viewDetectionSection renders the connection-check feedback: the in-flight
// spinner and error status lines. Custom's Auto Configure action lives under
// Auth; models.dev verification is automatic.
func (m *AdvancedConfigModel) viewDetectionSection() []*tui.Element {
	if m.live().detecting {
		return renderModelFetchProgress(m.live().detectProgress, m.live().detectFrame, m.usesOAuth())
	}

	// models.dev providers are pre-configured from metadata (endpoint, model
	// pool, and per-model protocol table are already in place), so the only
	// missing piece is the API key. It must be verified against the real
	// endpoint before the page reports a live connection —
	// a non-empty string is not proof of validity.
	if m.usesModelsDev() {
		if !m.live().modelPoolFromDiscovery {
			return []*tui.Element{spanLine(locale.T("尚未选择 Provider", "No provider selected yet"), stGray)}
		}
		switch {
		case strings.TrimSpace(m.keyText.Get()) == "":
			return []*tui.Element{spanLine(locale.T("已选择 Provider · 请输入 API Key", "Provider selected · enter your API key"), stGray)}
		case m.live().detectionError != nil:
			return []*tui.Element{
				spanLine(locale.T("验证失败，无法连接", "Verification failed; cannot connect"), stUnavailable),
				spanLine(m.live().detectionError.Error(), stUnavailable),
				spanLine(locale.T("在 API Key 行按 Enter 重试", "Press Enter on API Key to retry"), stGray),
				plainLine(""),
			}
		case m.live().keyVerified:
			// The header line already carries the connected status.
			return nil
		default:
			return []*tui.Element{spanLine(locale.T("Key 尚未验证", "Key not verified yet"), stGray)}
		}
	}

	if m.live().detectionError != nil {
		return []*tui.Element{
			spanLine(locale.T("检测失败，无法继续", "Detection failed; cannot continue"), stUnavailable),
			spanLine(m.live().detectionError.Error(), stUnavailable),
			plainLine(""),
		}
	}
	// Connected: the header line carries the status; nothing extra here.
	return nil
}

// actionButtonRow renders the left-aligned Auto Configure action row with the
// same plain-text affordance as Check:
// purple when idle, "> " + accent when selected. No background button box —
// the two action rows must read as one visual family.
func (m *AdvancedConfigModel) actionButtonRow(label string, kind configRowKind) *tui.Element {
	prefix := "  "
	prefixStyle := tui.NewStyle()
	style := stPurple
	if m.cursor == m.mainRowIndex(kind) {
		prefix = "> "
		prefixStyle = stSelected
		style = stSelected
	}
	return line(
		span(prefix, prefixStyle),
		span(label, style),
	)
}

// viewMappingSection renders Model Mapping: the five model slots with their
// [1M] / availability badges, the optional availability test, and the
// provider-wide Context & Compact stepper. Rows grey out until the
// connection is ready.
func (m *AdvancedConfigModel) viewMappingSection() []*tui.Element {
	rows := []*tui.Element{
		plainLine(""),
		spanLine(locale.T("模型映射", "Model Mapping"), stTitle),
	}
	ready := m.connectionReady()
	renderMappingRow := func(kind configRowKind, label, display, modelID string, oneM bool) {
		val := span(truncateMiddle(display, 52), stPurple)
		if !ready {
			// Connection not ready: grey out the row, no focus affordance.
			val = span(truncateMiddle(display, 52), stGray)
		} else if m.cursor == m.mainRowIndex(kind) {
			val = span(truncateMiddle(display, 52), stSelected)
		}
		prefix := "  "
		prefixStyle := tui.NewStyle()
		if ready && m.cursor == m.mainRowIndex(kind) {
			prefix = "> "
			prefixStyle = stSelected
		}
		// Availability badge, shown only after the optional test ran. The
		// badge is a span inside the row's single RichText — adding it as a
		// second flex child makes the row compress and overlap the label.
		badgeText := "    "
		badgeStyle := tui.NewStyle()
		if status, ok := m.modelAvailability[modelID]; ok && status != modelAvailabilityUnknown {
			switch status {
			case modelAvailabilityAvailable:
				badgeText = "✓ "
				badgeStyle = stAvailable
			case modelAvailabilityUnavailable:
				badgeText = "✗ "
				badgeStyle = stUnavailable
			case modelAvailabilityInconclusive:
				badgeText = "? "
				badgeStyle = stGray
			}
		} else if oneM && ready {
			badgeText = "[1M]"
			badgeStyle = stOneM
		}
		row := line(
			span(prefix, prefixStyle),
			span(fmt.Sprintf("%-10s ", label), tui.NewStyle()),
			span(truncateMiddle(display, 52), styleOf(val)),
			span(" "+badgeText, badgeStyle),
		)
		rows = append(rows, row)
	}
	renderMappingRow(rowOpus, "Opus", m.modelDisplayLabel(m.p.OpusModel), m.p.OpusModel, m.live().oneMSlots["opus"])
	renderMappingRow(rowSonnet, "Sonnet", m.modelDisplayLabel(m.p.SonnetModel), m.p.SonnetModel, m.live().oneMSlots["sonnet"])
	renderMappingRow(rowHaiku, "Haiku", m.modelDisplayLabel(m.p.HaikuModel), m.p.HaikuModel, m.live().oneMSlots["haiku"])
	renderMappingRow(rowFable, "Fable", m.modelDisplayLabel(m.p.FableModel), m.p.FableModel, m.live().oneMSlots["fable"])
	renderMappingRow(rowCustom, "Custom", m.modelDisplayLabel(m.p.CustomModelID), m.p.CustomModelID, m.live().oneMSlots["custom"])
	renderMappingRow(rowSubagent, "Subagent", m.subagentDisplayLabel(), m.p.SubagentModel, m.live().oneMSlots["subagent"])

	// Check — optional; each probe consumes quota, so the
	// user opts in explicitly. Results are shown next to the model rows above.
	// A blank line separates it from the model slots, matching the action rows.
	rows = append(rows, plainLine(""))
	testPrefix := "  "
	testPrefixStyle := tui.NewStyle()
	testLabel := locale.T("检查", "Check")
	testStyle := stPurple
	testReady := ready && (!m.usesModelsDev() || m.live().keyVerified)
	if !testReady {
		testStyle = stGray
	} else if m.modelTesting {
		spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		spin := spinners[m.modelTestFrame%len(spinners)]
		testLabel = fmt.Sprintf("%s %s", spin, locale.T("正在测试模型可用性...", "Testing model availability..."))
	} else if m.cursor == m.mainRowIndex(rowTestModels) {
		testPrefix = "> "
		testPrefixStyle = stSelected
		testStyle = stSelected
	}
	rows = append(rows, line(
		span(testPrefix, testPrefixStyle),
		span(testLabel, testStyle),
	))
	if m.modelTesting {
		rows = append(rows, spanLine("    "+locale.T("测试进行中 · 按 esc 取消", "Testing in progress · press esc to cancel"), stGray))
	} else if len(m.modelAvailability) > 0 {
		available, unavailable, inconclusive := m.availabilityCounts()
		rows = append(rows, spanLine(fmt.Sprintf("    "+locale.T("%d 个可用 · %d 个不可用 · %d 个未确认", "%d available · %d unavailable · %d inconclusive"), available, unavailable, inconclusive), stGray))
	} else if m.cursor == m.mainRowIndex(rowTestModels) {
		// go-tui has no tooltip API: the quota warning behaves like a hover hint
		// and only renders while the row is selected.
		rows = append(rows, spanLine(locale.T("    ⚠ 会为每个模型发送测试请求，限流时可能重试，消耗额度", "    ⚠ probes each model; may retry on rate limits and consume quota"), stGray))
	}

	return rows
}

// viewRuntimeSection renders the Runtime block: the OAuth read-only protocol
// display (when applicable), the Context & Compact stepper, the
// Fast and Status Line steppers, plus the active-provider checkbox.
func (m *AdvancedConfigModel) viewRuntimeSection() []*tui.Element {
	rows := []*tui.Element{
		plainLine(""),
		spanLine(locale.T("运行时", "Runtime"), stTitle),
	}
	ready := m.connectionReady()
	renderEditable := func(kind configRowKind, label, value string) {
		rows = append(rows, stepperRow(label, value, ready && m.cursor == m.mainRowIndex(kind)))
	}
	if m.usesOAuth() {
		// OAuth subscriptions keep the read-only protocol display here: their
		// Connection block is subscription metadata with no protocol concept.
		rows = append(rows, kvRow(locale.T("协议", "Protocol"), span(m.getProtocol(), stAvailable)))
	}
	// Context & Compact — per-slot [1m] via Space on the model rows above; the
	// provider-wide fallback cycles with ←→ (shown as ‹ › like other editable
	// values).
	renderEditable(rowContext, locale.T("上下文与压缩", "Context & Compact"), "‹ "+m.compactSummary()+" ›")
	renderEditable(rowFast, "Fast", formatFastLabel(m.p.FastMode))
	// Status Line — ccl writes its own status line through Claude Code's
	// --settings, which outranks ~/.claude/settings.json, so this is the only
	// place a user can keep a personal one.
	renderEditable(rowStatusline, locale.T("状态栏", "Status Line"), formatStatuslineLabel(m.p.StatuslineDisabled))

	// Active checkbox.
	activeBox := "[ ]"
	if m.IsActiveChosen {
		activeBox = "[x]"
	}
	activeLabel := locale.T("设为当前激活 Provider", "Set as active provider")
	activeSelected := m.cursor == m.mainRowIndex(rowActive)
	boxStyle := stPurple
	labelStyle := stPurple
	prefix := "  "
	prefixStyle := tui.NewStyle()
	if activeSelected {
		prefix = "> "
		prefixStyle = stSelected
		boxStyle = stSelected
		labelStyle = stSelected
	}
	return append(rows, line(
		span(prefix, prefixStyle),
		span(activeBox+" ", boxStyle),
		span(activeLabel, labelStyle),
	))
}

// saveBlockedReason explains a greyed-out Save button. Without it a blocked
// press does nothing visible, and the only way out the page leaves is Esc, which
// reads as "configuration canceled".
func (m *AdvancedConfigModel) saveBlockedReason() string {
	switch {
	case m.usesOAuth():
		return locale.T("本地代理就绪后才能保存", "Saving is available once the local proxy is ready")
	case m.usesModelsDev() && m.live().detecting:
		return locale.T("正在验证 API Key，通过后才能保存", "Verifying the API key; saving is available once it passes")
	case m.usesModelsDev() && m.live().detectionError != nil:
		return locale.T("API Key 验证未通过，暂不能保存（在 API Key 行按 Enter 重试）", "API key verification failed; cannot save yet (press Enter on API Key to retry)")
	case m.usesModelsDev():
		return locale.T("API Key 尚未验证，暂不能保存", "API key not verified yet; cannot save")
	default:
		return locale.T("尚未检测连接，请先运行 Auto Configure", "Connection not detected yet; run Auto Configure first")
	}
}

// viewActionSection renders the Save/Cancel action bar, the save-gating
// warnings, and the key hint footer.
func (m *AdvancedConfigModel) viewActionSection() []*tui.Element {
	applyLabel := locale.T("保存并激活", "Save & Activate")
	if !m.IsActiveChosen {
		applyLabel = locale.T("保存 Provider", "Save Provider")
	}
	cancelLabel := locale.T("取消", "Cancel")
	applyDisabled := !m.canSave()
	applyPrefix := "  "
	applyPrefixStyle := tui.NewStyle()
	applyStyle := stPurple
	if applyDisabled {
		// Not connected (or a dirty connection not yet re-tested): the button
		// is greyed out and not focusable.
		applyStyle = stGray
	} else if m.cursor == m.mainRowIndex(rowSave) {
		applyPrefix = "> "
		applyPrefixStyle = stSelected
		applyStyle = stSelected
	}
	cancelPrefix := "  "
	cancelPrefixStyle := tui.NewStyle()
	cancelStyle := stPurple
	if m.cursor == m.mainRowIndex(rowCancel) {
		cancelPrefix = "> "
		cancelPrefixStyle = stSelected
		cancelStyle = stSelected
	}
	rows := []*tui.Element{
		plainLine(""),
		line(
			span(applyPrefix, applyPrefixStyle),
			span(applyLabel, applyStyle),
			span("     ", tui.NewStyle()),
			span(cancelPrefix, cancelPrefixStyle),
			span(cancelLabel, cancelStyle),
		),
	}
	if m.live().connectionDirty && !m.usesOAuth() {
		rows = append(rows, spanLine(locale.T("连接已修改，保存前请重新检测", "Connection changed; re-test before saving"), stGray))
	} else if applyDisabled {
		rows = append(rows, spanLine(m.saveBlockedReason(), stGray))
	}
	if m.customDraft != nil && m.customDraft.saveGuardPending {
		rows = append(rows, spanLine(locale.T(
			"保存将覆盖同名 models.dev Provider（逐模型协议表会丢失）；再次点击保存确认，或切回 models.dev",
			"Saving replaces the same-named models.dev provider (its per-model protocol table is lost); press Save again to confirm, or switch back to models.dev",
		), stUnavailable))
	}
	return append(rows, spanLine(locale.T(
		"↑↓ 选择 · ←→ 调整 · enter 确认 · 模型行 enter 筛选",
		"↑↓ select · ←→ adjust · enter confirm · enter on a model row to filter",
	), stGray))
}

// wrapMainPanel wraps the page body in the rounded, scrollable panel plus the
// outer content chrome (scroll indicator and language tip).
func (m *AdvancedConfigModel) wrapMainPanel(rows []*tui.Element) *tui.Element {
	panel := tui.New(
		tui.WithDisplay(tui.DisplayFlex),
		tui.WithDirection(tui.Column),
		tui.WithWrap(false),
		tui.WithBorder(tui.BorderRounded),
		tui.WithPaddingTRBL(0, 2, 0, 2),
	)
	for _, r := range m.scrollWindow(rows) {
		panel.AddChild(r)
	}

	content := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false), tui.WithPaddingTRBL(1, 0, 1, 0))
	// The indicator mirrors scrollWindow's actual offset: both derive from
	// scrollBodyBudget, so the indicator appears exactly when the body was cut.
	if m.height > 0 && len(rows) > scrollBodyBudget(m.height) && m.scrollWindowOffset(rows) > 0 {
		content.AddChild(spanLine(locale.T("▲ 上滚 · ↑ 查看", "▲ scrolled up · ↑ to view"), stGray))
	}
	content.AddChild(panel)
	content.AddChild(plainLine(""))
	content.AddChild(spanLine(locale.T(
		"💡 提示: 使用 `ccl lang` 更改终端显示语言",
		"💡 Tip: Change the TUI display language with `ccl lang`",
	), stGray))
	return content
}

// scrollWindowOffset reports the body offset scrollWindow would slice at: it is
// the model's scrollOffset, anchored to the tail once the cursor reaches the
// action bar. The indicator logic uses it so the two can never disagree.
func (m *AdvancedConfigModel) scrollWindowOffset(rows []*tui.Element) int {
	if m.height <= 0 {
		return 0
	}
	maxBody := scrollBodyBudget(m.height)
	if len(rows) <= maxBody {
		return 0
	}
	offset := m.scrollOffset
	if offset < 0 {
		offset = 0
	}
	if m.cursor >= m.mainRowIndex(rowSave) {
		offset = len(rows) - maxBody
	}
	if offset+maxBody > len(rows) {
		offset = len(rows) - maxBody
	}
	if offset < 0 {
		offset = 0
	}
	return offset
}

// scrollWindow slices the page body rows to the visible window when the body
// exceeds the terminal height. The offset mirrors keepCursorVisible (the model
// field), anchored to the tail when the action bar would fall off.
func (m *AdvancedConfigModel) scrollWindow(rows []*tui.Element) []*tui.Element {
	// No terminal size yet (offline render / first frame): the min-clamped
	// budget below would slice the body to 6 rows, so return everything.
	if m.height <= 0 {
		return rows
	}
	maxBody := scrollBodyBudget(m.height)
	offset := m.scrollWindowOffset(rows)
	if offset == 0 && len(rows) <= maxBody {
		return rows
	}
	window := rows[offset : offset+maxBody]
	if len(window) == 0 {
		return []*tui.Element{plainLine("")}
	}
	return window
}

// kvRow renders a read-only key/value line ("  Key  Value") with styled value.
// The label column shares stepperLabelPad with stepperRow so read-only and
// editable rows stay aligned.
func kvRow(key string, value tui.TextSpan) *tui.Element {
	labelText := key + strings.Repeat(" ", max(stepperLabelPad(key)-tui.StringWidth(key), 0)) + " "
	return line(
		span("  ", tui.NewStyle()),
		span(labelText, tui.NewStyle()),
		value,
	)
}

// stepperRow renders an editable value row: a label plus a ‹ value › style value
// that highlights when the cursor is on the row. The label is padded to a fixed
// column measured in terminal cells (tui.StringWidth, not bytes) so CJK labels
// like "Context & Compact" keep the value column aligned with kvRow.
func stepperRow(label, value string, selected bool) *tui.Element {
	labelPad := stepperLabelPad(label)
	valueStyle := stPurple
	prefix := "  "
	prefixStyle := tui.NewStyle()
	if selected {
		valueStyle = stSelected
		prefix = "> "
		prefixStyle = stSelected
	}
	labelText := label + strings.Repeat(" ", max(labelPad-tui.StringWidth(label), 0)) + " "
	return line(
		span(prefix, prefixStyle),
		span(labelText, valueStyle),
		span(value, valueStyle),
	)
}

// stepperLabelPad returns the label column width shared by stepperRow and the
// other field rows: wide enough for the longest label ("Tool Search") at 12
// cells, or the label itself plus two cells of breathing room when it is
// longer. Measured with tui.StringWidth so wide runes count correctly. The
// longest labels today are the model rows ("Sonnet"/"Subagent") at 8 cells,
// so 12 keeps every current label from pushing the value column out.
func stepperLabelPad(label string) int {
	const base = 12
	if w := tui.StringWidth(label); w+2 > base {
		return w + 2
	}
	return base
}

// spanLine renders one single-style text line (no wrap so hit-test rows stay
// stable).
func spanLine(text string, st tui.Style) *tui.Element {
	return tui.New(tui.WithText(text), tui.WithTextStyle(st), tui.WithWrap(false))
}

// styleOf is a tiny helper keeping mapping-row construction readable.
func styleOf(s tui.TextSpan) tui.Style { return s.Style }

// viewModelPicker renders the filtered model selection overlay. It is shown
// whenever the filter input owns the keyboard; selecting a model (enter) or
// pressing esc returns to the main configuration page.
func (m *AdvancedConfigModel) viewModelPicker() *tui.Element {
	slotName := []string{"Opus", "Sonnet", "Haiku", "Fable", "Custom", "Subagent"}[m.activeSlot]
	root := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false), tui.WithPadding(1))
	root.AddChild(tui.New(
		tui.WithRichText(span(fmt.Sprintf(locale.T("配置槽位 [%s] 模型筛选", "Select Model for Slot [%s]"), slotName), stTitle)),
	))
	filterText := m.filterText.Get()
	root.AddChild(line(
		span(locale.T("🔍 过滤模型: ", "🔍 Filter model: "), stFilter),
		span(filterText, tui.NewStyle()),
	))

	start := m.filterWindowStart
	end := start + filterViewHeight
	if end > len(m.filteredPool) {
		end = len(m.filteredPool)
	}
	if start > 0 {
		root.AddChild(spanLine(fmt.Sprintf("   ↑ ... %d more above ...", start), stGray))
	}
	for i := start; i < end; i++ {
		mod := m.filteredPool[i]
		display := mod
		if stringInSlice(mod, m.live().modelPool) {
			display = m.modelDisplayLabel(mod)
		}
		status := ""
		if stringInSlice(mod, m.live().modelPool) {
			badge := m.availabilitySpan(mod)
			status = "  " + badge.Text
		}
		rowStyle := stGray
		prefix := "   "
		prefixStyle := tui.NewStyle()
		if i == m.slotListCursor {
			rowStyle = stSelected
			prefix = " > "
			prefixStyle = stSelected
		}
		if status != "" {
			root.AddChild(line(span(prefix, prefixStyle), span(display, rowStyle), span(status, stGray)))
		} else {
			root.AddChild(line(span(prefix, prefixStyle), span(display, rowStyle)))
		}
	}
	if end < len(m.filteredPool) {
		root.AddChild(spanLine(fmt.Sprintf("   ↓ ... %d more below ...", len(m.filteredPool)-end), stGray))
	}
	root.AddChild(spanLine(fmt.Sprintf("  %d/%d", m.slotListCursor+1, len(m.filteredPool)), stSelected))
	root.AddChild(plainLine(""))
	root.AddChild(spanLine(locale.T("状态来自可用性测试 · 键盘输入过滤 · ↑↓ 选择 · enter 锁定 · esc 取消", "Status comes from availability test · type to filter · ↑↓ scroll · enter lock · esc cancel"), stGray))
	return panelWrap(root, m.panelWidth())
}

// viewModelsDevPicker renders the models.dev provider overlay opened from the
// Connection section. It is filterable and scrolls like the slot picker.
func (m *AdvancedConfigModel) viewModelsDevPicker() *tui.Element {
	root := tui.New(tui.WithDisplay(tui.DisplayFlex), tui.WithDirection(tui.Column), tui.WithWrap(false), tui.WithPadding(1))
	root.AddChild(tui.New(tui.WithText(locale.T("从 models.dev 选择 Provider", "Choose a provider from models.dev")), tui.WithTextStyle(stTitle)))
	root.AddChild(plainLine(""))
	root.AddChild(line(
		span(locale.T("🔍 过滤: ", "🔍 Filter: "), stFilter),
		span(m.modelsDevText.Get(), tui.NewStyle()),
	))
	root.AddChild(plainLine(""))

	if m.modelsDevLoading {
		root.AddChild(spanLine(locale.T("正在加载 models.dev 目录...", "Loading the models.dev catalog..."), stSelected))
	} else if m.modelsDevError != nil {
		root.AddChild(spanLine(locale.T("拉取失败", "Fetch failed"), stUnavailable))
		root.AddChild(spanLine(m.modelsDevError.Error(), stUnavailable))
	} else if len(m.modelsDevFiltered) == 0 {
		root.AddChild(spanLine(locale.T("(无匹配)", "(no match)"), stGray))
	} else {
		start := m.modelsDevWindow
		end := start + selectViewHeight
		if end > len(m.modelsDevFiltered) {
			end = len(m.modelsDevFiltered)
		}
		if start > 0 {
			root.AddChild(spanLine(fmt.Sprintf("   ↑ ... %d more above ...", start), stGray))
		}
		for i := start; i < end; i++ {
			p := m.modelsDevFiltered[i]
			display := p.Name
			if p.ID != "" && p.ID != p.Name {
				display = fmt.Sprintf("%s  (%s)", p.Name, p.ID)
			}
			prefix := "  "
			prefixStyle := tui.NewStyle()
			rowStyle := tui.NewStyle()
			if i == m.modelsDevCursor {
				prefix = "▸ "
				prefixStyle = stSelected
				rowStyle = stSelected
			}
			root.AddChild(line(span(prefix, prefixStyle), span(display, rowStyle)))
		}
		if end < len(m.modelsDevFiltered) {
			root.AddChild(spanLine(fmt.Sprintf("   ↓ ... %d more below ...", len(m.modelsDevFiltered)-end), stGray))
		}
	}

	root.AddChild(plainLine(""))
	root.AddChild(spanLine(locale.T("输入过滤 · ↑↓ 选择 · enter 确认 · esc 取消", "type to filter · ↑↓ choose · enter confirm · esc cancel"), stGray))
	return panelWrap(root, m.panelWidth())
}

// panelWrap wraps an overlay body in the shared rounded panel.
func panelWrap(body *tui.Element, width int) *tui.Element {
	panel := tui.New(
		tui.WithDisplay(tui.DisplayFlex),
		tui.WithDirection(tui.Column),
		tui.WithWrap(false),
		tui.WithBorder(tui.BorderRounded),
		tui.WithPaddingTRBL(0, 2, 0, 2),
		tui.WithMaxWidth(width),
	)
	panel.AddChild(body)
	return panel
}
