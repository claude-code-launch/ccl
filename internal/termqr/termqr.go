// Package termqr draws QR codes in a terminal, so a device authorization URL
// can be opened from a phone on hosts without a browser (SSH, Cloud Shell).
package termqr

import (
	"io"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	// Explicit black on white: scanners need dark modules on a light
	// background, which a dark terminal theme would otherwise invert.
	colors = "\x1b[30;47m"
	reset  = "\x1b[0m"
)

// Write renders text as a QR code using half blocks, two module rows per
// line, including the standard quiet zone. Each line is indented by indent.
func Write(w io.Writer, text, indent string) error {
	code, err := qrcode.New(text, qrcode.Low)
	if err != nil {
		return err
	}
	bitmap := code.Bitmap()
	var out strings.Builder
	for y := 0; y < len(bitmap); y += 2 {
		out.WriteString(indent)
		out.WriteString(colors)
		for x := range bitmap[y] {
			top := bitmap[y][x]
			bottom := y+1 < len(bitmap) && bitmap[y+1][x]
			switch {
			case top && bottom:
				out.WriteString("█")
			case top:
				out.WriteString("▀")
			case bottom:
				out.WriteString("▄")
			default:
				out.WriteString(" ")
			}
		}
		out.WriteString(reset)
		out.WriteByte('\n')
	}
	_, err = io.WriteString(w, out.String())
	return err
}
