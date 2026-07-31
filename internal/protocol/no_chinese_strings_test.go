package protocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// TestLibraryPackagesHoldNoChineseStringLiterals pins the layering convention:
// no package under internal/ imports internal/locale, so a Chinese string there
// reaches an English user untranslated. Translation happens in cmd/, which owns
// the presentation layer. Comments are exempt — only literals are checked.
func TestLibraryPackagesHoldNoChineseStringLiterals(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Skipf("cannot resolve internal/ directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "locale")); err != nil {
		t.Skipf("not running from the repository tree: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// locale itself necessarily holds Chinese: it is the translation table.
		if filepath.Base(filepath.Dir(path)) == "locale" {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !containsHan(lit.Value) {
				return true
			}
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, fset.Position(lit.Pos()).String()+" "+lit.Value+" (in internal/"+rel+")")
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("internal/ packages cannot translate, so these literals reach English users as Chinese.\n"+
			"Either phrase them in English, or move the message to cmd/ and wrap it in locale.T:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
