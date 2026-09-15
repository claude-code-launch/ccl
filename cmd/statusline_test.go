package cmd

import (
	"io"
	"os"
	"testing"
)

func TestFormatStatusline(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "model effort and context",
			payload: `{"model":{"id":"glm-5.3-free","display_name":"GLM-5.3-Flash"},"effort":{"level":"xhigh"},"context_window":{"used_percentage":32.4}}`,
			want:    "GLM-5.3-Flash · xhigh · 32%",
		},
		{
			// Claude Code reports "Auto" when ccl has not pinned a model; the
			// resolved ID is the useful value.
			name:    "Auto display name falls back to the id",
			payload: `{"model":{"id":"glm-5.3-free","display_name":"Auto"},"effort":{"level":"xhigh"},"context_window":{"used_percentage":16.6}}`,
			want:    "glm-5.3-free · xhigh · 17%",
		},
		{
			// A structured suffix on the display name must not reach the line.
			name:    "display name with an effort blob is cut at the brace",
			payload: `{"model":{"display_name":"z-ai/glm-5.3-free({\"level\": \"xhigh\"})"},"context_window":{"used_percentage":32.4}}`,
			want:    "z-ai/glm-5.3-free · 32%",
		},
		{
			name:    "effort as a bare string",
			payload: `{"model":{"id":"opus"},"effort":"high","context_window":{"used_percentage":7}}`,
			want:    "opus · high · 7%",
		},
		{
			name:    "zero percent is reported",
			payload: `{"model":{"id":"opus"},"context_window":{"used_percentage":0}}`,
			want:    "opus · 0%",
		},
		{
			name:    "missing context window is omitted",
			payload: `{"model":{"id":"haiku"},"effort":{"level":"low"}}`,
			want:    "haiku · low",
		},
		{
			name:    "nothing to show",
			payload: `{}`,
			want:    "",
		},
		{
			name:    "malformed payload renders nothing",
			payload: `not json`,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatStatusline([]byte(tc.payload)); got != tc.want {
				t.Errorf("formatStatusline() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The command Claude Code runs is `ccl statusline`, so the stdin/stdout wiring
// on runStatusline is what the user actually sees.
func TestRunStatuslineReadsStdinWritesStdout(t *testing.T) {
	payload := `{"model":{"id":"glm-5.3-free"},"effort":{"level":"xhigh"},"context_window":{"used_percentage":32.4}}`

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdin: %v", err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}

	originalStdin, originalStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinReader, stdoutWriter
	defer func() {
		os.Stdin, os.Stdout = originalStdin, originalStdout
		stdinReader.Close()
		stdoutReader.Close()
	}()

	go func() {
		_, _ = stdinWriter.WriteString(payload)
		stdinWriter.Close()
	}()

	runStatusline()
	stdoutWriter.Close()

	output, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got, want := string(output), "glm-5.3-free · xhigh · 32%\n"; got != want {
		t.Errorf("statusline output = %q, want %q", got, want)
	}
}
