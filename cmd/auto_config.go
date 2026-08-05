package cmd

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/claude-code-launch/ccl/internal/protocol"
	"github.com/claude-code-launch/ccl/internal/provider"
)

// versionNumber matches a numeric model version such as the "4.5" in grok-4.5
// or the "5" in gpt-5. It drives the fallback ordering for pools whose names
// carry no tier keyword (grok, newer openai, etc).
var versionNumber = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// AutoRecommendation is the deterministic result of model slot auto-fill. It is
// a pure value: applying it to a Provider (and writing [1m] markers) happens in
// the caller, so the recommendation can be unit-tested without a Bubble Tea
// model and re-rendered without mutating the draft.
type AutoRecommendation struct {
	Opus     string
	Sonnet   string
	Haiku    string
	Custom   string
	Subagent string
	// OneMSlots marks the slots that should carry the [1m] context marker.
	// Only allowlist-confirmed models are auto-enabled; advisory window reports
	// are surfaced to the user but never auto-opened.
	OneMSlots map[string]bool

	// chatPoolSize is the number of distinct chat models in the pool. It gates
	// slot-exclusion relaxation: with fewer distinct chat models than strong
	// slots, slots share models instead of leaving holes.
	chatPoolSize int
}

// RecommendModels picks slot models for a provider after a successful protocol
// and model-list detection. It never mutates current.
//
// Precedence:
//  1. Preserve a slot whose current model is still in models (updating an
//     existing provider must not overwrite a working mapping).
//  2. Otherwise recommend by model-name semantics.
//  3. Tie-break by catalog metadata (context window, rate multiplier).
//
// Metadata is optional; when a model has no ModelInfo entry the name heuristics
// alone decide. models is the exact pool from the upstream /models catalog, and
// the returned IDs always come from it (or are empty).
func RecommendModels(current provider.Provider, models []string, metadata map[string]protocol.ModelInfo) AutoRecommendation {
	pool := normalizePool(models)
	rec := AutoRecommendation{OneMSlots: make(map[string]bool)}

	// Fast path: preserve every slot that still resolves. Anything empty after
	// this pass is filled below.
	currentSlots := advancedSlotRefs(&current)
	preserved := make(map[string]bool)
	for _, slot := range currentSlots {
		model := strings.TrimSpace(stripOneMSuffix(*slot.ptr))
		if model == "" {
			continue
		}
		if !inModelPool(pool, model) {
			continue
		}
		switch slot.key {
		case "opus":
			rec.Opus = model
		case "sonnet":
			rec.Sonnet = model
		case "haiku":
			rec.Haiku = model
		case "custom":
			rec.Custom = model
		case "subagent":
			rec.Subagent = model
		}
		preserved[slot.key] = true
	}

	// A tiny chat pool forces slot reuse: with one model every slot shares it;
	// with two the strong tiers share the heavy model and Haiku takes the light
	// one. Scoring ties on small pools are order-dependent, so these sizes are
	// assigned explicitly. Anything already preserved is left untouched.
	chatPool := chatModels(pool)
	rec.setChatPoolSize(len(chatPool))
	fill := func(current string, pick func() string) string {
		if current != "" {
			return current
		}
		return pick()
	}

	switch len(chatPool) {
	case 1:
		m := chatPool[0]
		rec.Opus = fill(rec.Opus, func() string { return m })
		rec.Sonnet = fill(rec.Sonnet, func() string { return m })
		rec.Haiku = fill(rec.Haiku, func() string { return m })
		rec.Custom = fill(rec.Custom, func() string { return m })
	case 2:
		heavy, light := splitHeavyLight(chatPool, metadata)
		rec.Opus = fill(rec.Opus, func() string { return heavy })
		rec.Sonnet = fill(rec.Sonnet, func() string { return heavy })
		rec.Custom = fill(rec.Custom, func() string { return heavy })
		rec.Haiku = fill(rec.Haiku, func() string { return light })
	default:
		rec.Opus = fill(rec.Opus, func() string { return recommendForTier(pool, metadata, tierOpus, rec) })
		rec.Sonnet = fill(rec.Sonnet, func() string { return recommendForTier(pool, metadata, tierSonnet, rec) })
		rec.Haiku = fill(rec.Haiku, func() string { return recommendForTier(pool, metadata, tierHaiku, rec) })
		rec.Custom = fill(rec.Custom, func() string { return firstNonEmpty(rec.Sonnet, rec.Opus) })
	}
	// Subagent stays Auto (empty) unless a distinct lighter model exists.
	if rec.Subagent == "" {
		rec.Subagent = recommendSubagent(pool, metadata, rec)
	}

	rec.applyOneM(current, preserved)
	return rec
}

// setChatPoolSize records how many distinct chat models are available so slot
// exclusion can relax when the pool is too small to support distinct picks.
func (r *AutoRecommendation) setChatPoolSize(n int) {
	r.chatPoolSize = n
}

// splitHeavyLight divides a two-model chat pool into the strong model (shared by
// Opus/Sonnet/Custom) and the light one (Haiku/Subagent).
func splitHeavyLight(chatPool []string, metadata map[string]protocol.ModelInfo) (heavy, light string) {
	if len(chatPool) < 2 {
		if len(chatPool) == 1 {
			return chatPool[0], chatPool[0]
		}
		return "", ""
	}
	a, b := chatPool[0], chatPool[1]
	scoreA := scoreModelForSlot(a, metadata[strings.ToLower(a)], tierOpus)
	scoreB := scoreModelForSlot(b, metadata[strings.ToLower(b)], tierOpus)
	if scoreA == scoreB {
		// Same name semantics: prefer the larger advertised window.
		if metadata[strings.ToLower(b)].ContextWindow > metadata[strings.ToLower(a)].ContextWindow {
			a, b = b, a
		}
	} else if scoreB > scoreA {
		a, b = b, a
	}
	return a, b
}

// applyOneM auto-enables [1m] only for allowlist-confirmed models, preserving an
// existing marker when the model did not change. Advisory windows (≥900K from the
// catalog) never auto-enable; the UI surfaces them as "1M reported".
func (r *AutoRecommendation) applyOneM(current provider.Provider, preserved map[string]bool) {
	slots := []struct {
		key   string
		model string
	}{
		{"opus", r.Opus},
		{"sonnet", r.Sonnet},
		{"haiku", r.Haiku},
		{"custom", r.Custom},
		{"subagent", r.Subagent},
	}
	for _, s := range slots {
		if s.model == "" {
			continue
		}
		if recommendedOneMModel(s.model) {
			r.OneMSlots[s.key] = true
			continue
		}
		// Preserve a marker the user already had on an unchanged model.
		if preserved[s.key] {
			for _, slot := range advancedSlotRefs(&current) {
				if slot.key == s.key && hasOneMSuffix(*slot.ptr) {
					r.OneMSlots[s.key] = true
				}
			}
		}
	}
}

// recommendForTier scores every pool model for a tier and returns the best.
func recommendForTier(pool []string, metadata map[string]protocol.ModelInfo, tier modelTier, rec AutoRecommendation) string {
	if best, ok := bestChatModel(pool, metadata, tier, excludeForTier(rec, tier)); ok {
		return best
	}
	// Small pool: relax the no-reuse rule rather than leaving a slot empty.
	// With one model every slot shares it; with two the strong tiers share the
	// heavy model and Haiku takes the light one.
	best, _ := bestChatModel(pool, metadata, tier, nil)
	return best
}

// excludeForTier keeps two strong tiers from landing on the same model, but only
// when a distinct candidate exists. Haiku always prefers its own lighter pick.
func excludeForTier(rec AutoRecommendation, tier modelTier) map[string]bool {
	exclude := map[string]bool{}
	switch tier {
	case tierOpus:
		// A pool whose models carry no strong tier keyword (grok-4.5, gpt-5.6)
		// cannot keep Opus and Sonnet distinct by name: Opus takes the highest
		// version. Sonnet reuses it (bare-version scoring) below.
		if rec.chatPoolSize >= 3 && hasStrongTierName(rec.Sonnet) {
			exclude[rec.Sonnet] = true
			exclude[rec.Haiku] = true
		}
	case tierSonnet:
		// For a bare-version pool, Sonnet must NOT exclude Opus: both share the
		// highest-version model. Named tiers keep them distinct.
		if rec.chatPoolSize >= 3 && !hasStrongTierName(rec.Opus) {
			break
		}
		if rec.chatPoolSize >= 3 {
			exclude[rec.Opus] = true
		}
	case tierHaiku:
		exclude[rec.Opus] = true
		exclude[rec.Sonnet] = true
	}
	return exclude
}

// hasStrongTierName reports whether a model name carries a keyword that pins it
// to the Opus class (opus/pro/max/ultra/...), as opposed to a bare version.
func hasStrongTierName(model string) bool {
	return scoreOpus(model) >= 80
}

// chatModels returns the pool filtered to conversational models.
func chatModels(pool []string) []string {
	out := make([]string, 0, len(pool))
	for _, m := range pool {
		if isChatModel(m) {
			out = append(out, m)
		}
	}
	return out
}

func bestChatModel(pool []string, metadata map[string]protocol.ModelInfo, tier modelTier, exclude map[string]bool) (string, bool) {
	best := ""
	bestScore := -1
	bestVersion := -1
	for _, model := range pool {
		if exclude[model] {
			continue
		}
		if !isChatModel(model) {
			continue
		}
		score := scoreModelForSlot(model, metadata[strings.ToLower(model)], tier)
		if score > bestScore {
			best = model
			bestScore = score
			bestVersion = modelMagnitude(model)
			continue
		}
		if score != bestScore {
			continue
		}
		// A bare-version tie (grok-4.5 vs grok-4.3) is resolved deterministically:
		// strong tiers prefer the higher version, Haiku the lower.
		mag := modelMagnitude(model)
		if (tier != tierHaiku && mag > bestVersion) || (tier == tierHaiku && mag < bestVersion) {
			best = model
			bestVersion = mag
		}
	}
	return best, bestScore >= 0
}

// modelMagnitude returns a monotonic magnitude for a model's numeric version so
// ties can be broken deterministically. Non-versioned models sort as 0.
func modelMagnitude(model string) int {
	m := versionNumber.FindStringSubmatch(model)
	if m == nil {
		return 0
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	minor := 0
	if m[2] != "" {
		if parsed, err := strconv.Atoi(m[2]); err == nil {
			minor = parsed
		}
	}
	return major*100 + minor
}

// recommendSubagent picks the lightest chat model that is not already a main
// slot, or returns empty to keep Claude Code's Auto behavior.
func recommendSubagent(pool []string, metadata map[string]protocol.ModelInfo, rec AutoRecommendation) string {
	taken := map[string]bool{rec.Opus: true, rec.Sonnet: true, rec.Haiku: true, rec.Custom: true}
	// Exclude the main slots only when the pool still has a distinct lighter
	// candidate. With one or two chat models, reuse keeps subagent functional.
	if rec.chatPoolSize < 3 {
		taken = map[string]bool{}
	}
	best := ""
	bestScore := -1
	for _, model := range pool {
		if taken[model] {
			continue
		}
		if !isChatModel(model) {
			continue
		}
		// Subagent wants the cheapest/fastest: invert the Haiku scoring.
		score := scoreModelForSlot(model, metadata[strings.ToLower(model)], tierHaiku)
		if score > bestScore {
			best = model
			bestScore = score
		}
	}
	return best
}

// modelTier is the semantic role a slot plays; the scoring tables are per tier.
type modelTier int

const (
	tierOpus modelTier = iota
	tierSonnet
	tierHaiku
	tierCustom
)

// scoreModelForSlot returns a 0..100-ish score for how well model fits tier.
// Metadata only breaks ties between equal name-score candidates.
func scoreModelForSlot(model string, info protocol.ModelInfo, tier modelTier) int {
	name := strings.ToLower(model)
	if info.ID != "" {
		if d := strings.TrimSpace(info.DisplayName); d != "" {
			name = strings.ToLower(d)
		}
	}

	var score int
	switch tier {
	case tierOpus:
		score = scoreOpus(name)
	case tierSonnet:
		score = scoreSonnet(name)
	case tierHaiku:
		score = scoreHaiku(name)
	case tierCustom:
		score = scoreSonnet(name)
		if score < 40 {
			score = scoreOpus(name)
		}
	}

	// Metadata tie-breaks only: never lets a small-window model beat a
	// high-scoring name match.
	if score > 0 {
		if info.ContextWindow >= 900000 {
			score += 8
		} else if info.ContextWindow >= 200000 {
			score += 4
		}
		if info.RateMultiplier != nil && *info.RateMultiplier < 1.0 {
			score += 3
		}
	}
	return score
}

func scoreOpus(name string) int {
	switch {
	case strings.Contains(name, "opus"):
		return 100
	case strings.Contains(name, "max") || strings.Contains(name, "ultra"):
		return 96
	case strings.Contains(name, "reasoning") || strings.Contains(name, "reasoner"):
		return 90
	case strings.Contains(name, "thinking"):
		return 88
	case strings.Contains(name, "pro"):
		return 80
	case strings.Contains(name, "large"):
		return 74
	case strings.Contains(name, "coder-pro") || strings.Contains(name, "coding-pro"):
		return 70
	}
	return versionScore(name, 1)
}

func scoreSonnet(name string) int {
	switch {
	case strings.Contains(name, "sonnet"):
		return 100
	case strings.Contains(name, "pro"):
		return 85
	case strings.Contains(name, "standard") || strings.Contains(name, "general"):
		return 80
	case strings.Contains(name, "coder"):
		return 72
	case strings.Contains(name, "chat"):
		return 60
	case strings.Contains(name, "plus") || strings.Contains(name, "fast"):
		return 50
	}
	return versionScore(name, 0)
}

func scoreHaiku(name string) int {
	switch {
	case strings.Contains(name, "haiku"):
		return 100
	case strings.Contains(name, "flash"):
		return 92
	case strings.Contains(name, "mini"):
		return 88
	case strings.Contains(name, "lite") || strings.Contains(name, "small"):
		return 80
	case strings.Contains(name, "fast"):
		return 70
	case strings.Contains(name, "nano"):
		return 65
	}
	return versionScore(name, -1)
}

// versionScore gives a relative magnitude for models that carry a numeric
// version but no tier keyword. dir picks which end is "stronger" for the tier:
// Opus wants the highest version, Haiku the lowest.
func versionScore(name string, dir int) int {
	m := versionNumber.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	major, err1 := strconv.Atoi(m[1])
	minor := 0
	if err2 := err1; m[2] != "" {
		minor, err2 = strconv.Atoi(m[2])
		if err2 != nil {
			minor = 0
		}
	}
	if err1 != nil {
		return 0
	}
	// Base ~20 plus up to ~9.9 for the version, so a named tier always beats a
	// bare version and ordering stays monotonic in the version.
	magnitude := major + minor/10
	if dir < 0 {
		magnitude = 50 - magnitude // smaller version -> larger score
	}
	return 20 + magnitude
}

// isChatModel excludes non-conversational entries from every slot.
func isChatModel(model string) bool {
	low := strings.ToLower(model)
	for _, bad := range []string{
		"preview", "deprecated", "embedding", "embed", "image", "audio", "tts", "stt",
		"whisper", "dall-e", "dalle", "moderation", "rerank", "re-rank", "segmentation",
	} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	return true
}

// normalizePool trims, de-duplicates, and sorts the catalog pool so a stable
// recommendation does not depend on upstream ordering.
func normalizePool(models []string) []string {
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func inModelPool(pool []string, model string) bool {
	for _, m := range pool {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
