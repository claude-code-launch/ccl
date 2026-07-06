package cmd

import (
	"bytes"
	"testing"

	"github.com/claude-code-launch/ccl/internal/provider"
	"github.com/spf13/cobra"
)

func executeCommand(root *cobra.Command, args ...string) (string, error) {
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	// 执行命令
	_, err := root.ExecuteC()
	return buf.String(), err
}

// 简单字符串包含函数
func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}

func TestCmd_Doctor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := executeCommand(RootCmd(), "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "No providers added yet"; !contains(out, want) {
		t.Errorf("expected output to contain %q, got %q", want, out)
	}
}
func TestCmd_Set(t *testing.T) {
	cmd := SetCMD()
	if cmd.Use != "set [name]" {
		t.Fatalf("unexpected set command use: %q", cmd.Use)
	}
}

func TestMapAutoAssignsAvailableModelsInOrder(t *testing.T) {
	p := testProviderWithOldMappings()
	assigned := applySequentialSlotMapping(sequentialSlotPointers(&p), []string{
		"model-a",
		"model-b",
		"model-c",
		"model-d",
		"model-e",
	})

	if assigned != 4 {
		t.Fatalf("expected 4 assigned slots, got %d", assigned)
	}
	if p.OpusModel != "model-a" || p.SonnetModel != "model-b" || p.HaikuModel != "model-c" || p.CustomModelID != "model-d" {
		t.Fatalf("models were not assigned sequentially: %+v", p)
	}
}

func TestMapAutoClearsUnassignedTrailingSlots(t *testing.T) {
	p := testProviderWithOldMappings()
	assigned := applySequentialSlotMapping(sequentialSlotPointers(&p), []string{"model-a", "model-b"})

	if assigned != 2 {
		t.Fatalf("expected 2 assigned slots, got %d", assigned)
	}
	if p.OpusModel != "model-a" || p.SonnetModel != "model-b" {
		t.Fatalf("first slots not assigned sequentially: %+v", p)
	}
	if p.HaikuModel != "" || p.CustomModelID != "" {
		t.Fatalf("unassigned trailing slots should be cleared: %+v", p)
	}
}

func testProviderWithOldMappings() provider.Provider {
	return provider.Provider{
		OpusModel:     "old-opus",
		SonnetModel:   "old-sonnet",
		HaikuModel:    "old-haiku",
		CustomModelID: "old-custom",
		LockModel:     "old-lock",
	}
}
