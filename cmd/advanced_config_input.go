package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/claude-code-launch/ccl/internal/claude"
	"github.com/claude-code-launch/ccl/internal/locale"
	"github.com/claude-code-launch/ccl/internal/oauthproxy"
	"github.com/claude-code-launch/ccl/internal/provider"
	tui "github.com/grindlemire/go-tui"
)

// rowClickLabels maps a configuration row to the label prefixes a click (or
// the cursor-line lookup) must match on its rendered line. Both the English
// and the Chinese rendering of each label are listed: the page renders labels
// through locale.T, so in a Chinese session only the zh form appears on
// screen and a single-language table would silently stop matching. Only rows
// that make sense to click are listed.
type rowClickLabel struct {
	en, zh string
}

var rowClickLabels = map[configRowKind]rowClickLabel{
	rowSource:     {en: "Source", zh: "来源"},
	rowEndpoint:   {en: "Endpoint URL", zh: "端点 URL"},
	rowAPIKey:     {en: "API Key"},
	rowProvider:   {en: "Provider"},
	rowTest:       {en: "Auto Configure"},
	rowProtocol:   {en: "Protocol", zh: "协议"},
	rowAuth:       {en: "Auth", zh: "鉴权"},
	rowFast:       {en: "Fast"},
	rowOpus:       {en: "Opus"},
	rowSonnet:     {en: "Sonnet"},
	rowHaiku:      {en: "Haiku"},
	rowFable:      {en: "Fable"},
	rowCustom:     {en: "Custom"},
	rowSubagent:   {en: "Subagent"},
	rowTestModels: {en: "Check", zh: "检查"},
	rowContext:    {en: "Context & Compact", zh: "上下文与压缩"},
	rowStatusline: {en: "Status Line", zh: "状态栏"},
	rowActive:     {en: "Set as active provider", zh: "设为当前激活 Provider"},
	// The Save button also renders as "Save Provider" when activation is not
	// chosen; matchRowLabel matches prefixes, so the shorter shared prefix of
	// both variants ("Save ") is what must stay clickable.
	rowSave:   {en: "Save & Activate", zh: "保存并激活"},
	rowCancel: {en: "Cancel", zh: "取消"},
}

// handleModelsDevPickerKey handles a key press while the models.dev overlay is
// open. Enter applies the selected provider and closes the overlay; esc/ctrl+c
// cancel; ↑↓ move the cursor; any other printable key filters the list.
func (m *AdvancedConfigModel) handleModelsDevPickerKey(ke tui.KeyEvent) {
	if ke.Key == tui.KeyEscape || ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.closeModelsDevPicker()
		return
	}
	switch ke.Key {
	case tui.KeyUp:
		if m.modelsDevCursor > 0 {
			m.modelsDevCursor--
		}
		if m.modelsDevCursor < m.modelsDevWindow {
			m.modelsDevWindow = m.modelsDevCursor
		}
	case tui.KeyDown:
		if m.modelsDevCursor < len(m.modelsDevFiltered)-1 {
			m.modelsDevCursor++
		}
		if m.modelsDevCursor >= m.modelsDevWindow+selectViewHeight {
			m.modelsDevWindow = m.modelsDevCursor - selectViewHeight + 1
		}
	case tui.KeyEnter:
		if len(m.modelsDevFiltered) > 0 && m.modelsDevCursor >= 0 && m.modelsDevCursor < len(m.modelsDevFiltered) {
			m.applyModelsDevProvider(m.modelsDevFiltered[m.modelsDevCursor])
			m.closeModelsDevPicker()
		}
	case tui.KeyBackspace:
		t := m.modelsDevText.Get()
		if t != "" {
			m.modelsDevText.Set(removeLastRune(t))
		}
		m.updateModelsDevFilter()
	default:
		if ke.IsRune() && ke.Rune != 0 {
			m.modelsDevText.Set(m.modelsDevText.Get() + string(ke.Rune))
			m.updateModelsDevFilter()
		}
	}
}

// activateRow fires the action for a button row on click or Enter: Auto
// Configure starts the connection check, Check starts the
// per-model probes. Async work is delivered through the model's channels and
// consumed on the main loop by the Watchers below.
func (m *AdvancedConfigModel) activateRow(kind configRowKind) {
	switch kind {
	case rowProvider:
		m.openModelsDevPicker()
		fetchModelsDevAsync(m.mdDone)
	case rowSource:
		m.switchSource(m.otherSource())
	case rowTest:
		// Start detection with the current input values (OAuth uses the session
		// runtime endpoint/key already injected by configureOAuthRuntime).
		if !m.usesOAuth() {
			m.p.Endpoint = m.urlText.Get()
			m.p.APIKey = m.keyText.Get()
			m.live().probeEndpoint = m.p.Endpoint
			m.live().probeAPIKey = m.p.APIKey
			// The inputs being detected become the new baseline only if the
			// detection succeeds; until then keep the dirty state honest.
			m.refreshConnectionDirty()
		}
		m.urlFocused = false
		m.keyFocused = false
		m.live().detectionError = nil
		// AutoClaw needs no network model detection: its managed Chat route has a
		// fixed catalog, so a generic /models probe would either hit the wrong
		// wire protocol or overwrite the fixed runtime metadata. Deliver the
		// built-in catalog through the regular detection-success path instead.
		if provider.IsAutoClawType(m.p.Type) {
			m.live().probeEndpoint = m.p.Endpoint
			m.live().probeAPIKey = m.keyText.Get()
			m.live().detecting = true
			m.live().detectProgress = 5
			m.live().detectFrame = 0
			setDebugf("autoclaw detection skipped: built-in catalog endpoint=%q", m.live().probeEndpoint)
			go func() {
				m.fetchDone <- modelFetchDoneMsg{
					endpoint:            m.live().probeEndpoint,
					apiKey:              m.live().probeAPIKey,
					detectedType:        m.p.Type,
					detectedEndpoint:    m.p.Endpoint,
					anthropicAuth:       "",
					discoveredModelsRaw: strings.Join(oauthproxy.AutoClawModelIDs(), ","),
					modelInfos:          oauthproxy.AutoClawModelCatalog(),
				}
			}()
			return
		}
		m.live().detecting = true
		m.live().detectProgress = 5
		m.live().detectFrame = 0
		setDebugf("start detection endpoint=%q api_key_len=%d oauth=%t", m.live().probeEndpoint, len(m.live().probeAPIKey), m.usesOAuth())
		if m.canSelectCustomProtocol() {
			fetchModelsAsync(m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey, m.p.Type, m.p.AnthropicAuth)
		} else {
			fetchModelsAsync(m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey)
		}
	case rowTestModels:
		if !m.connectionReady() || m.usesModelsDev() && !m.live().keyVerified {
			return
		}
		if len(m.live().modelPool) == 0 {
			setDebugf("model availability test skipped: empty pool")
			return
		}
		m.modelTestID++
		testID := m.modelTestID
		ctx, cancel := context.WithCancel(context.Background())
		m.modelTesting = true
		m.modelTestCancel = cancel
		m.modelTestFrame = 0
		m.modelTestCanceled = false
		setDebugf("model availability test started model_count=%d", len(m.live().modelPool))
		testModelsAsync(m.availDone, ctx, testID, m.live().modelPool, m.live().probeEndpoint, m.live().probeAPIKey, probeWireType(*m.p), m.p.AnthropicAuth, m.p.ModelProtocols, m.availabilitySmokeTestModel(), m.usesModelsDev() && m.live().keyVerified)
	}
}

// handleFocusRow carries out the click semantics of a configuration row: the
// first click selects it (moves the cursor); a second click on the
// already-selected row performs its action. Endpoint and API Key focus their
// text inputs on first click so typing lands there.
func (m *AdvancedConfigModel) handleFocusRow(row configRowKind) {
	if row == rowCopyKey || row == rowCopyURL {
		// A single click on a value row focuses its input; a double-click
		// (second click on the same row within the window) copies the value.
		now := time.Now()
		double := row == m.lastCopyClickRow && now.Sub(m.lastCopyClickAt) < 500*time.Millisecond
		m.lastCopyClickRow = row
		m.lastCopyClickAt = now
		if !double {
			focus := rowAPIKey
			if row == rowCopyURL {
				focus = rowEndpoint
			}
			m.cursor = m.mainRowIndex(focus)
			m.urlFocused = false
			m.keyFocused = false
			m.markDirty()
			return
		}
		if row == rowCopyKey {
			m.keyCopied = true
			m.lastKeyCopyAt = now
			if err := clipboard.WriteAll(m.keyText.Get()); err != nil {
				setDebugf("copy key to clipboard failed: %v", err)
			}
			setDebugf("key copied to clipboard")
			m.markDirty()
			return
		}
		m.urlCopied = true
		m.lastUrlCopyAt = now
		if err := clipboard.WriteAll(m.urlText.Get()); err != nil {
			setDebugf("copy url to clipboard failed: %v", err)
		}
		setDebugf("url copied to clipboard")
		m.markDirty()
		return
	}
	idx := m.mainRowIndex(row)
	if idx < 0 {
		return
	}
	wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
	alreadySelected := m.cursor == idx && !m.textInputHasKeyboard()
	m.cursor = idx
	m.keepCursorVisible()
	m.urlFocused = false
	m.keyFocused = false
	switch row {
	case rowEndpoint:
		m.urlFocused = true
		m.refreshConnectionDirty()
	case rowAPIKey:
		m.keyFocused = true
		m.refreshConnectionDirty()
	case rowTest, rowTestModels, rowProvider, rowSource:
		if alreadySelected {
			m.activateRow(row)
		}
	case rowSave:
		if alreadySelected && m.requestSave() {
			m.quit()
		}
	case rowCancel:
		if alreadySelected {
			setDebugf("click cancel requested")
			m.quit()
		}
	default:
		m.filterFocused = false
	}
	if wasKeyRow && row != rowAPIKey {
		m.startModelsDevVerification()
	}
	m.markDirty()
}

// handleKey routes one key press. The models.dev picker and the two modal
// states (connection check, model test) own the keyboard while active; after
// that the slot picker filter and the endpoint/key inputs take printable
// keys, and the remaining keys move the page cursor.
func (m *AdvancedConfigModel) handleKey(ke tui.KeyEvent) {
	if m.modelsDevPicker {
		m.handleModelsDevPickerKey(ke)
		m.markDirty()
		return
	}

	if ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.quit()
		return
	}

	// q 是 quit 的单字母别名，仅当没有文本输入框持有键盘时生效（q 也是
	// 合法的输入字符，见下方的 runeAlias 说明）。
	if ke.IsRune() && ke.Mod == 0 && ke.Rune == 'q' && !m.textInputHasKeyboard() {
		m.quit()
		return
	}

	// 模态：连接检查/模型测试进行中。esc 取消操作（或退出等待），其余按键
	// 等待结束。ctrl+c 已在上面处理。
	if m.live().detecting && !m.usesModelsDev() {
		if ke.Key == tui.KeyEscape {
			m.live().detecting = false
			m.live().detectionError = fmt.Errorf("%s", locale.T("已取消连接检查", "connection check canceled"))
			m.focusDetectionAction()
			setDebugf("connection check canceled by user")
			m.markDirty()
		}
		return
	}
	if m.modelTesting {
		if ke.Key == tui.KeyEscape {
			if m.modelTestCancel != nil {
				m.modelTestCancel()
			}
			m.modelTesting = false
			m.modelTestCancel = nil
			m.modelTestCanceled = true
			setDebugf("model availability test canceled test_id=%d", m.modelTestID)
			m.markDirty()
		}
		return
	}

	// 文本输入框拥有键盘时，单字母导航别名让位给输入本身：q/h/j/k/l 是合法
	// 的输入字符。方向键没有这个歧义。
	runeAlias := ke.IsRune() && ke.Mod == 0 && strings.ContainsRune("q hjkl", ke.Rune) && ke.Rune != ' '
	inputHasKeyboard := m.textInputHasKeyboard()

	// vim 单字母别名 h/j/k/l：无输入焦点时映射到方向键。
	if m.handleNavAlias(ke, inputHasKeyboard) {
		return
	}

	switch ke.Key {
	case tui.KeyEscape:
		if m.filterFocused {
			m.filterFocused = false
			setDebugf("esc closed slot picker active_slot=%d cursor=%d", m.activeSlot, m.cursor)
			m.markDirty()
			return
		}
		setDebugf("esc quit cursor=%d endpoint_set=%t api_key_len=%d", m.cursor, strings.TrimSpace(m.urlText.Get()) != "", len(m.keyText.Get()))
		m.quit()
		return

	case tui.KeyUp:
		if inputHasKeyboard && ke.IsRune() {
			return // navigation alias yielded to text input
		}
		if m.filterFocused {
			if m.slotListCursor > 0 {
				m.slotListCursor--
				if m.slotListCursor < m.filterWindowStart {
					m.filterWindowStart = m.slotListCursor
				}
			}
			m.markDirty()
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.urlFocused, m.keyFocused = false, false
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = len(rows) - 1
		}
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return

	case tui.KeyDown:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			if m.slotListCursor < len(m.filteredPool)-1 {
				m.slotListCursor++
				if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
					m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
				}
			}
			m.markDirty()
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.urlFocused, m.keyFocused = false, false
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		if m.cursor < len(rows)-1 {
			m.cursor++
		} else {
			m.cursor = 0
		}
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return

	case tui.KeyLeft:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			return
		}
		if m.isModelRow(m.currentRow()) {
			m.toggleOneMAtRow(m.currentRow())
		} else {
			switch m.currentRow() {
			case rowSource, rowContext, rowProtocol, rowAuth, rowFast, rowStatusline, rowActive:
				m.adjustReviewField(-1)
			}
		}
		m.markDirty()
		return

	case tui.KeyRight:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			return
		}
		if m.isModelRow(m.currentRow()) {
			m.toggleOneMAtRow(m.currentRow())
		} else {
			switch m.currentRow() {
			case rowSource, rowContext, rowProtocol, rowAuth, rowFast, rowStatusline, rowActive:
				m.adjustReviewField(1)
			}
		}
		m.markDirty()
		return

	case tui.KeyEnter:
		if m.usesModelsDev() && m.currentRow() == rowAPIKey {
			m.keyFocused = false
			m.retryModelsDevVerification()
			m.focusDetectionAction()
			m.keepCursorVisible()
			m.markDirty()
			return
		}
		// The API key textarea inserts newlines with Enter, so while it is
		// focused the key must fall through to the text routing below rather
		// than advance the page cursor.
		if !m.keyFocused {
			m.handleEnter()
			m.markDirty()
			return
		}

	case tui.KeyTab:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		if m.filterFocused {
			if m.slotListCursor < len(m.filteredPool)-1 {
				m.slotListCursor++
				if m.slotListCursor >= m.filterWindowStart+filterViewHeight {
					m.filterWindowStart = m.slotListCursor - filterViewHeight + 1
				}
			}
			m.markDirty()
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		m.cursor = (m.cursor + 1) % len(rows)
		m.urlFocused, m.keyFocused = false, false
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return
	}

	// shift+tab：主页面反向移动光标（filter 打开时不响应）。
	if ke.Key == tui.KeyTab && ke.Mod == tui.ModShift {
		if m.filterFocused || inputHasKeyboard && ke.IsRune() {
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.cursor--
		if m.cursor < 0 {
			m.cursor = len(rows) - 1
		}
		m.keepCursorVisible()
		m.markDirty()
		return
	}

	// space：模型行上切换 1M 标记（输入框聚焦时作为空格字符输入）。
	if ke.Key == tui.KeyRune && ke.Rune == ' ' && ke.Mod == 0 {
		if m.filterFocused || inputHasKeyboard && runeAlias {
			// falls through to text routing below
		} else {
			m.toggleOneMAtRow(m.currentRow())
			m.markDirty()
			return
		}
	}

	// 文本输入路由：与旧实现的输入框焦点同步规则一致——光标在 Endpoint/
	// API Key 行或 filter 聚焦时，可打印字符进入对应文本。
	if ke.IsRune() && ke.Mod == 0 && ke.Rune != 0 {
		ch := string(ke.Rune)
		switch {
		case m.filterFocused:
			m.filterText.Set(m.filterText.Get() + ch)
			m.updateFilteredPool()
		case m.currentRow() == rowEndpoint && !m.usesOAuth():
			m.urlFocused = true
			m.keyFocused = false
			m.urlText.Set(m.urlText.Get() + ch)
			m.refreshConnectionDirty()
		case m.currentRow() == rowAPIKey && !m.usesOAuth():
			m.urlFocused = false
			m.keyFocused = true
			m.keyText.Set(m.keyText.Get() + ch)
			m.refreshConnectionDirty()
			if m.usesModelsDev() {
				m.invalidateModelsDevKeyIfChanged()
			}
		default:
			// 光标在按钮或只读行上时，取消两个输入框的焦点。
			if !inputHasKeyboard {
				m.urlFocused = false
				m.keyFocused = false
			}
		}
		m.markDirty()
		return
	}

	if ke.Key == tui.KeyBackspace {
		switch {
		case m.filterFocused:
			t := m.filterText.Get()
			if t != "" {
				m.filterText.Set(removeLastRune(t))
			}
			m.updateFilteredPool()
		case m.currentRow() == rowEndpoint && !m.usesOAuth():
			t := m.urlText.Get()
			if t != "" {
				m.urlText.Set(removeLastRune(t))
			}
			m.refreshConnectionDirty()
		case m.currentRow() == rowAPIKey && !m.usesOAuth():
			before := m.keyText.Get()
			t := before
			if t != "" {
				m.keyText.Set(removeLastRune(t))
			}
			m.refreshConnectionDirty()
			if m.usesModelsDev() && m.keyText.Get() != before {
				m.invalidateModelsDevKeyIfChanged()
			}
		}
		m.markDirty()
		return
	}

	// 其余按键（非打印导航在上方处理过）：确保输入框焦点不残留。
	if !inputHasKeyboard {
		m.urlFocused = false
		m.keyFocused = false
		m.markDirty()
	}
}

// handleNavAlias maps the vim single-letter navigation aliases (h/j/k/l) onto
// the same key events the arrow keys produce. It runs before the text-input
// routing but only when no text input owns the keyboard, so the aliases stay
// typeable inside the endpoint/API key/filter inputs.
func (m *AdvancedConfigModel) handleNavAlias(ke tui.KeyEvent, inputHasKeyboard bool) bool {
	if inputHasKeyboard || !ke.IsRune() || ke.Mod != 0 {
		return false
	}
	switch ke.Rune {
	case 'k':
		m.handleKey(tui.KeyEvent{Key: tui.KeyUp})
	case 'j':
		m.handleKey(tui.KeyEvent{Key: tui.KeyDown})
	case 'h':
		m.handleKey(tui.KeyEvent{Key: tui.KeyLeft})
	case 'l':
		m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	default:
		return false
	}
	return true
}

// handleEnter performs the Enter action of the row under the cursor. It runs
// only when no text input owns the keyboard (the key textarea inserts
// newlines instead).
func (m *AdvancedConfigModel) handleEnter() {
	if m.filterFocused {
		// Model picker selection.
		if len(m.filteredPool) == 0 {
			return
		}
		if m.slotListCursor < 0 || m.slotListCursor >= len(m.filteredPool) {
			m.slotListCursor = 0
		}
		selectedModel := m.filteredPool[m.slotListCursor]
		if selectedModel == locale.T("(设置为未设置/清空)", "(clear/unset)") || selectedModel == locale.T("(无匹配模型)", "(no match)") {
			selectedModel = ""
		}
		*advancedSlotRefs(m.p)[m.activeSlot].ptr = selectedModel
		slotKey := modelSlotKeys[m.activeSlot]
		if slotKey == "subagent" && m.p.Env != nil {
			delete(m.p.Env, claude.SubagentModelEnv)
		}
		// A slot whose model was just changed must not keep a [1m] marker
		// the backend rules out for the new model — toggleOneMAtRow refuses
		// to enable one there, so leaving an enabled marker would be
		// inconsistent and would send a non-1M model with the [1m] suffix.
		if m.live().oneMSlots[slotKey] && m.oneMSlotBlocked(selectedModel) {
			m.live().oneMSlots[slotKey] = false
			setDebugf("slot model changed to a non-1M model; cleared 1M marker slot=%s model=%q", slotKey, selectedModel)
		}
		m.filterFocused = false
		m.live().autoConfigured = false
		setDebugf("slot selected active_slot=%d model=%q slots=%s", m.activeSlot, selectedModel, slotDebugSummary(*m.p))
		return
	}

	switch m.currentRow() {
	case rowSource:
		m.switchSource(m.otherSource())
	case rowEndpoint:
		m.cursor = m.mainRowIndex(rowAPIKey)
		m.urlFocused = false
		m.keyFocused = true
		setDebugf("enter endpoint -> api key endpoint=%q", m.urlText.Get())
	case rowAPIKey:
		// Custom advances to Auto Configure; models.dev verifies the key in
		// the background and advances to the first model slot.
		if m.usesModelsDev() {
			m.retryModelsDevVerification()
		}
		m.focusDetectionAction()
		m.urlFocused = false
		m.keyFocused = false
		setDebugf("enter api key -> next api_key_len=%d", len(m.keyText.Get()))
	case rowProvider:
		m.activateRow(rowProvider)
	case rowTest:
		m.activateRow(rowTest)
	case rowProtocol, rowAuth, rowFast, rowStatusline:
		m.adjustReviewField(1)
	case rowOpus, rowSonnet, rowHaiku, rowFable, rowCustom, rowSubagent:
		if !m.connectionReady() {
			return
		}
		m.activeSlot = slotForRow(m.currentRow())
		m.filterFocused = true
		m.filterText.Set("")
		m.slotListCursor = 0
		m.updateFilteredPool()
		setDebugf("open slot picker active_slot=%d filtered_count=%d", m.activeSlot, len(m.filteredPool))
	case rowTestModels:
		m.activateRow(rowTestModels)
	case rowContext:
		// Context & Compact is edited inline; nothing to open yet.
		setDebugf("context row selected")
	case rowActive:
		m.IsActiveChosen = !m.IsActiveChosen
		setDebugf("active choice toggled active_chosen=%t", m.IsActiveChosen)
	case rowSave:
		if m.requestSave() {
			m.quit()
		}
	case rowCancel:
		setDebugf("cancel requested")
		m.quit()
	}
	m.keepCursorVisible()
}

// rowAtLineAt resolves a clicked screen row to a configuration row kind. x is
// the column offset (-1 to ignore). The X column disambiguates multiple labels
// on one rendered line (Save & Activate vs Cancel): the click maps to whichever
// label's character range contains it.
func rowAtLineAt(lines []string, y, x int) (configRowKind, bool) {
	if y < 0 || y >= len(lines) {
		return rowCancel, false
	}
	for _, off := range []int{0, -1} {
		row := y + off
		if row < 0 || row >= len(lines) {
			continue
		}
		// Strip ANSI only; keep the leading border/space columns so label
		// offsets line up with the click's X column.
		text := stripANSI(lines[row])
		// Copy rows (key / URL value) are only hit when clicked directly on their
		// own row; the off-by-one fallback (a value-row click resolving to its
		// label row) must not trigger them.
		kind, ok := matchRowLabel(text, x, off == 0)
		if ok {
			return kind, true
		}
	}
	return rowCancel, false
}

// stripANSI removes SGR/CSI escape sequences from a rendered line, leaving the
// printable text. go-tui renders through its own escape builder, so the panel
// code owns a minimal stripper instead of pulling in x/ansi.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) {
			// CSI sequences: ESC [ ... letter
			if s[i+1] == '[' {
				j := i + 2
				for j < len(s) && !isANSIFinalByte(s[j]) {
					j++
				}
				if j < len(s) {
					j++ // include the final byte
				}
				i = j
				continue
			}
			// Two-byte escapes (ESC c, ESC \, ...)
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// isANSIFinalByte reports whether c terminates a CSI sequence (ANSI "final"
// byte range 0x40-0x7E).
func isANSIFinalByte(c byte) bool {
	return c >= 0x40 && c <= 0x7e
}

// matchRowLabel matches a rendered line against the clickable labels. A label
// matches only when it begins a field — the line, after stripping the leading
// border/cursor/space/checkbox prefix, starts with the label. This keeps prose
// like "detection uses the API Key you entered" from matching. The label's
// start column is kept so a click's X can disambiguate Save vs Cancel, which
// share one line.
func matchRowLabel(text string, x int, allowButton bool) (configRowKind, bool) {
	// Strip leading border, cursor arrow, and whitespace to find the first field.
	trimmed := strings.TrimLeft(text, " │|>")
	lead := len(text) - len(trimmed)
	// Account for a checkbox "[x] "/"[ ] " before the label.
	rest := trimmed
	if strings.HasPrefix(rest, "[") {
		if idx := strings.Index(rest, "] "); idx >= 0 {
			rest = rest[idx+2:]
			lead = len(text) - len(rest)
		}
	}
	// Find every label that starts the trimmed field. Both the English and the
	// Chinese rendering of a label can lead the field, so both count as the row.
	var matched configRowKind
	var matchedIdx int
	hasMatch := false
	for kind := range rowClickLabels {
		for _, label := range rowClickLabelPrefixes(kind) {
			if strings.HasPrefix(rest, label) {
				idx := lead
				if !hasMatch || idx < matchedIdx {
					matched = kind
					matchedIdx = idx
					hasMatch = true
				}
				break
			}
		}
	}
	if !hasMatch {
		// No leading label: a value row. The endpoint value copies the URL; the
		// API key value (now plaintext and possibly spanning multiple lines)
		// copies the key. Both only respond to a direct click (allowButton), not
		// the off-by-one label fallback. The leading border/space is already
		// stripped in rest. Prose and hints must never resolve to a copy row,
		// so only URL prefixes and dense credential tokens (a key, after the
		// textarea's trailing padding is trimmed) count as value rows.
		if !allowButton {
			return rowCancel, false
		}
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			return rowCopyURL, true
		}
		key := strings.TrimSpace(rest)
		if key != "" && (strings.HasPrefix(key, "sk-") || strings.HasPrefix(key, "-----BEGIN") || !strings.ContainsAny(key, " \t")) {
			return rowCopyKey, true
		}
		return rowCancel, false
	}
	// A single label at the field start is unambiguous.
	if x < 0 {
		return matched, true
	}
	// With column info, Save and Cancel on the same line are distinct. The click
	// belongs to the label whose field start is at or before x (Save then Cancel).
	best := matched
	bestIdx := matchedIdx
	for kind := range rowClickLabels {
		for _, label := range rowClickLabelPrefixes(kind) {
			if kind == matched {
				break
			}
			idx := strings.Index(rest, label)
			if idx < 0 {
				continue
			}
			absIdx := lead + idx
			if absIdx <= x && (bestIdx > x || absIdx > bestIdx) {
				best = kind
				bestIdx = absIdx
			}
			break
		}
	}
	return best, true
}

// rowClickLabelPrefixes returns every rendered label variant for a row kind.
func rowClickLabelPrefixes(kind configRowKind) []string {
	label, ok := rowClickLabels[kind]
	if !ok {
		return nil
	}
	prefixes := make([]string, 0, 3)
	if label.en != "" {
		prefixes = append(prefixes, label.en)
	}
	if label.zh != "" {
		prefixes = append(prefixes, label.zh)
	}
	// "Save Provider" is the other rendering of the Save button.
	if kind == rowSave {
		prefixes = append(prefixes, "Save Provider", "保存 Provider")
	}
	return prefixes
}

// KeyMap routes every key through handleKey. go-tui matches one binding per
// event, so a single AnyKey stop binding covers the whole page (the picker,
// the modals, and the inputs are disambiguated inside handleKey).
func (m *AdvancedConfigModel) KeyMap() tui.KeyMap {
	return tui.KeyMap{
		tui.OnStop(tui.AnyKey, func(ke tui.KeyEvent) {
			m.handleKey(ke)
			m.markDirty()
		}),
	}
}

// HandleMouse maps a click to the configuration row under the pointer. The
// rendered frame is re-rendered offline (tui.Sprint at the current terminal
// width) so the hit test works on exactly what the user sees. Only left-button
// presses are handled; the wheel is deliberately ignored (the page anchors its
// scroll window to the cursor instead).
func (m *AdvancedConfigModel) HandleMouse(me tui.MouseEvent) bool {
	if m.modelsDevPicker || m.filterFocused {
		return false
	}
	if me.Button != tui.MouseLeft || me.Action != tui.MousePress {
		return false
	}
	lines := strings.Split(tui.Sprint(m.Render(nil), tui.WithPrintWidth(max(m.width, 1))), "\n")
	row, ok := rowAtLineAt(lines, me.Y, me.X)
	if !ok {
		return false
	}
	m.handleFocusRow(row)
	return true
}

// Watchers bridges the async goroutines onto the main loop: the four result
// channels, the two spinner/cleanup timers, and the one-shot auto-detect that
// used to live in Init(). go-tui calls Watchers() after the first render, so
// the channels are guaranteed to be watched before any fetch can complete.
func (m *AdvancedConfigModel) Watchers() []tui.Watcher {
	m.startAutoDetect()
	return []tui.Watcher{
		tui.Watch(m.fetchDone, m.handleFetchDone),
		tui.Watch(m.verifyDone, m.handleVerifyDone),
		tui.Watch(m.mdDone, m.handleModelsDevDone),
		tui.Watch(m.availDone, m.handleAvailabilityDone),
		tui.Watch(m.oauthDone, m.handleOAuthRuntimeDone),
		tui.OnTimer(120*time.Millisecond, m.handleFetchTick),
		tui.OnTimer(120*time.Millisecond, m.handleAvailabilityTick),
	}
}
