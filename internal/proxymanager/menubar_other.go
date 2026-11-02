//go:build !darwin

package proxymanager

import "context"

func RefreshCodex(context.Context) error { return nil }

func (m *Manager) startMenuBar(string) func()               { return func() {} }
func (m *Manager) RunMenuBar(context.Context, string) error { return nil }
func ShowMenuError(string)                                  {}
