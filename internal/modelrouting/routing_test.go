package modelrouting

import "testing"

func TestSplitCSV(t *testing.T) {
	t.Parallel()

	got := SplitCSV(" a, ,b,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("SplitCSV = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SplitCSV[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if got := SplitCSV(""); len(got) != 0 {
		t.Fatalf("empty SplitCSV = %v, want nil/empty", got)
	}
}

func TestMapModelExplicitOverride(t *testing.T) {
	t.Parallel()

	if got := MapModel("claude-3-5-sonnet", "my-custom-model", []string{"gpt-4o"}); got != "my-custom-model" {
		t.Fatalf("explicit override = %q, want my-custom-model", got)
	}
}

func TestMapModelConfiguredPool(t *testing.T) {
	t.Parallel()

	pool := "gpt-4o-mini,deepseek-reasoner,gpt-4o"
	if got := MapModel("claude-3-opus", pool, nil); got != "deepseek-reasoner" {
		t.Fatalf("configured pool opus = %q, want deepseek-reasoner", got)
	}
	if got := MapModel("claude-3-5-sonnet", pool, nil); got != "gpt-4o" {
		t.Fatalf("configured pool sonnet = %q, want gpt-4o", got)
	}
	if got := MapModel("claude-3-5-haiku", pool, nil); got != "gpt-4o-mini" {
		t.Fatalf("configured pool haiku = %q, want gpt-4o-mini", got)
	}
}

func TestMapModelAvailablePoolAndExactMatch(t *testing.T) {
	t.Parallel()

	models := []string{"gpt-4o-mini", "deepseek-chat", "deepseek-reasoner", "gpt-4o"}
	if got := MapModel("DeepSeek-Reasoner", "", models); got != "deepseek-reasoner" {
		t.Fatalf("exact match = %q, want deepseek-reasoner", got)
	}
	if got := MapModel("claude-3-opus", "", models); got != "deepseek-reasoner" {
		t.Fatalf("opus tier = %q, want deepseek-reasoner", got)
	}
	if got := MapModel("claude-3-5-sonnet", "", models); got != "gpt-4o" {
		t.Fatalf("sonnet tier = %q, want gpt-4o", got)
	}
	if got := MapModel("claude-3-5-haiku", "", models); got != "gpt-4o-mini" {
		t.Fatalf("haiku tier = %q, want gpt-4o-mini", got)
	}
}

func TestMapModelFallsBackToFirstPoolEntry(t *testing.T) {
	t.Parallel()

	// No keyword match for any tier heuristic — first entry wins.
	models := []string{"vendor-special-a", "vendor-special-b"}
	if got := MapModel("claude-3-5-sonnet", "", models); got != "vendor-special-a" {
		t.Fatalf("first-pool fallback = %q, want vendor-special-a", got)
	}
}

func TestMapModelEmptyPoolReturnsEmpty(t *testing.T) {
	t.Parallel()

	if got := MapModel("claude-3-5-sonnet", "", nil); got != "" {
		t.Fatalf("nil pool = %q, want empty", got)
	}
	if got := MapModel("claude-3-5-sonnet", "", []string{"", "  "}); got != "" {
		t.Fatalf("blank pool = %q, want empty", got)
	}
	if got := MapModel("claude-3-5-sonnet", " , , ", nil); got != "" {
		t.Fatalf("blank configured pool = %q, want empty", got)
	}
	// Must not invent a DeepSeek-specific default for unrelated gateways.
	if got := MapModel("claude-3-opus", "", nil); got == "deepseek-chat" {
		t.Fatalf("empty pool must not hardcode deepseek-chat")
	}
}

func TestScoreModelForTierKeywords(t *testing.T) {
	t.Parallel()

	if scoreModelForTier("deepseek-reasoner", TierOpus) < scoreModelForTier("gpt-4o", TierOpus) {
		t.Fatal("reasoner should outrank gpt-4o for opus")
	}
	if scoreModelForTier("gpt-4o", TierSonnet) < scoreModelForTier("gpt-4o-mini", TierSonnet) {
		t.Fatal("gpt-4o should outrank mini for sonnet")
	}
	if scoreModelForTier("gpt-4o-mini", TierHaiku) < scoreModelForTier("gpt-4o", TierHaiku) {
		t.Fatal("mini should outrank gpt-4o for haiku")
	}
	if scoreModelForTier("", TierSonnet) != 0 {
		t.Fatal("empty model should score 0")
	}
}

func TestRequestedTier(t *testing.T) {
	t.Parallel()

	if got := requestedTier("claude-3-opus"); got != TierOpus {
		t.Fatalf("opus tier = %q", got)
	}
	if got := requestedTier("claude-3-5-haiku"); got != TierHaiku {
		t.Fatalf("haiku tier = %q", got)
	}
	if got := requestedTier("claude-3-5-sonnet"); got != TierSonnet {
		t.Fatalf("sonnet tier = %q", got)
	}
}
