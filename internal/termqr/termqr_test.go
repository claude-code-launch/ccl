package termqr

import (
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

func TestWriteEncodesEveryModuleWithAQuietZone(t *testing.T) {
	const text = "https://www.google.com/device"
	var out strings.Builder
	if err := Write(&out, text, "  "); err != nil {
		t.Fatal(err)
	}
	code, _ := qrcode.New(text, qrcode.Low)
	bitmap := code.Bitmap()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != (len(bitmap)+1)/2 {
		t.Fatalf("%d lines for %d module rows", len(lines), len(bitmap))
	}
	for i, line := range lines {
		cells := []rune(strings.TrimSuffix(strings.TrimPrefix(line, "  "+colors), reset))
		if len(cells) != len(bitmap[0]) {
			t.Fatalf("line %d has %d cells, want %d", i, len(cells), len(bitmap[0]))
		}
		for x, cell := range cells {
			top, bottom := bitmap[2*i][x], 2*i+1 < len(bitmap) && bitmap[2*i+1][x]
			want := map[[2]bool]rune{{true, true}: '█', {true, false}: '▀', {false, true}: '▄', {false, false}: ' '}[[2]bool{top, bottom}]
			if cell != want {
				t.Fatalf("line %d cell %d = %q, want %q", i, x, cell, want)
			}
		}
	}
	// The quiet zone keeps the first and last rows light.
	if strings.ContainsAny(lines[0], "█▀▄") {
		t.Fatalf("first line is not quiet: %q", lines[0])
	}
}
