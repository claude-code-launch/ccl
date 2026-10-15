package process

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
)

func ConfigureGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x00000200 // CREATE_NEW_PROCESS_GROUP
}

func KillTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill.exe", "/pid", strconv.Itoa(cmd.Process.Pid), "/t", "/f").Run()
		_ = cmd.Process.Kill()
	}
}

func SignalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}
