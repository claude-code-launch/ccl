// Package acp runs the upstream claude-agent-acp implementation unchanged on
// stdio, decorating only its public session methods with CCL configuration.
package acp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/claude-code-launch/ccl/internal/process"
)

//go:embed adapter.mjs
var adapterScript []byte

type LaunchLease interface {
	Generation() uint64
	Options(cwd string) (json.RawMessage, error)
	Release()
}

type Config struct {
	NodePath      string
	AdapterPath   string
	Args          []string
	Env           []string
	AcquireLaunch func(context.Context) (LaunchLease, error)
	LegacyStore   string
	Stderr        io.Writer
}

// Serve owns the upstream process. All ACP input/output connects directly to
// it: CCL never decodes or reconstructs a JSON-RPC frame or capability list.
func Serve(ctx context.Context, in io.Reader, out io.Writer, cfg Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.NodePath == "" || cfg.AdapterPath == "" || cfg.AcquireLaunch == nil {
		return fmt.Errorf("ACP requires Node.js, claude-agent-acp and provider configuration")
	}
	logs := newCredentialLog(cfg.Stderr, cfg.Env)
	defer logs.flush()
	control, err := startConfigurationService(cfg.AcquireLaunch, cfg.LegacyStore, logs.options)
	if err != nil {
		return err
	}
	defer control.Close()
	dir, err := os.MkdirTemp("", "ccl-acp-adapter-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "adapter.mjs")
	if err := os.WriteFile(script, adapterScript, 0o600); err != nil {
		return err
	}
	args := append([]string{script, cfg.AdapterPath}, cfg.Args...)
	cmd := process.Command(cfg.NodePath, args...)
	cmd.Env = append(append([]string(nil), cfg.Env...),
		"CCL_ACP_CONTROL_URL="+control.URL(), "CCL_ACP_CONTROL_TOKEN="+control.token)
	cmd.Stdout, cmd.Stderr = out, logs
	cmd.WaitDelay = time.Second
	process.ConfigureGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start claude-agent-acp: %w", err)
	}
	inputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdin, in)
		_ = stdin.Close()
		close(inputDone)
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = stdin.Close() // EOF lets the adapter dispose all SDK queries.
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			process.KillTree(cmd)
			<-done
		}
		err = ctx.Err()
	}
	_ = stdin.Close()
	if closer, ok := in.(io.Closer); ok {
		go func() { _ = closer.Close() }()
		select {
		case <-inputDone:
		case <-time.After(50 * time.Millisecond):
			// A blocking OS stdin read cannot always be interrupted by Close.
			// Do not keep the CLI alive after its adapter already exited.
		}
	}
	return err
}
