//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

var proxySignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
