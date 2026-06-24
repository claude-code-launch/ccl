package cmd

import "fmt"

// Lang represents the user's language choice for interactive prompts.
type Lang struct {
	code string // "cn" or "en"
}

// T returns the Chinese or English string based on the language setting.
func (l *Lang) T(cn, en string) string {
	if l.code == "cn" {
		return cn
	}
	return en
}

// Tf is like T but with fmt.Sprintf formatting.
func (l *Lang) Tf(cn, en string, args ...any) string {
	if l.code == "cn" {
		return fmt.Sprintf(cn, args...)
	}
	return fmt.Sprintf(en, args...)
}
