package cmd

import "unicode/utf8"

// removeLastRune deletes a complete Unicode code point, never a UTF-8 byte.
func removeLastRune(text string) string {
	_, size := utf8.DecodeLastRuneInString(text)
	return text[:len(text)-size]
}
