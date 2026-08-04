package cmd

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestModelReportLabelUsesFriendlyCatalogMetadata(t *testing.T) {
	rate := 0.5
	metadata := indexModelInfos([]protocol.ModelInfo{{
		ID: "qmodel_38max", DisplayName: "Qwen3.8-Max", RateMultiplier: &rate,
		IsNew: true, PromotionAvailable: true,
	}})
	got := modelReportLabel("qmodel_38max", metadata)
	want := "Qwen3.8-Max (qmodel_38max) · 0.5x · new · off-peak discount"
	if got != want {
		t.Fatalf("model label = %q, want %q", got, want)
	}
}

func TestModelReportLabelKeepsFriendlyCapitalizationWithoutDuplicateID(t *testing.T) {
	rate := 1.0
	metadata := indexModelInfos([]protocol.ModelInfo{{ID: "auto", DisplayName: "Auto", RateMultiplier: &rate}})
	if got := modelReportLabel("auto", metadata); got != "Auto · 1x" {
		t.Fatalf("model label = %q", got)
	}
}

func TestProviderCatalogModelLabelPairsFriendlyNameWithTechnicalID(t *testing.T) {
	names := map[string]string{"cmodel": "Cantus"}
	if got := providerCatalogModelLabel("cmodel[1m]", names); got != "Cantus (cmodel) · 1M" {
		t.Fatalf("provider model label = %q", got)
	}
	if got := providerCatalogModelLabel("unknown", names); got != "unknown" {
		t.Fatalf("unknown model label = %q", got)
	}
}

func TestMappedModelOutputLabelUsesFetchedMetadata(t *testing.T) {
	metadata := indexModelInfos([]protocol.ModelInfo{{ID: "dfmodel", DisplayName: "DeepSeek-V4-Flash"}})
	if got := mappedModelOutputLabel("dfmodel", metadata); got != "DeepSeek-V4-Flash (dfmodel)" {
		t.Fatalf("mapped model label = %q", got)
	}
}
