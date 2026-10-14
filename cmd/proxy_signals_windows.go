//go:build windows

package cmd

import "os"

var proxySignals = []os.Signal{os.Interrupt}
