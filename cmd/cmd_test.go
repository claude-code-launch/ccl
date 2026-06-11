package cmd

import (
	"bytes"
	"testing"

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

	out, err := executeCommand(RootCmd(), "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "No providers added yet"; !contains(out, want) {
		t.Errorf("expected output to contain %q, got %q", want, out)
	}
}
func TestCmd_Set(t *testing.T) {

	out, err := executeCommand(SetCMD(), "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "No providers added yet"; !contains(out, want) {
		t.Errorf("expected output to contain %q, got %q", want, out)
	}
}
