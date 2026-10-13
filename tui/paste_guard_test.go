package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every text input and text area the package makes has bubbles' Paste
// disabled, and a limit of characters, in the function that makes it: Paste reads the clipboard by
// starting xclip, xsel or wl-paste, on the server, and kosh starts no
// other process. The check reads the package's Go files that are not
// tests.
func TestEveryTextInputHasNoPaste(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			for _, ctor := range []string{"textinput.New()", "textarea.New()"} {
				if !strings.Contains(body, ctor) {
					continue
				}
				found++
				if !strings.Contains(body, "KeyMap.Paste.SetEnabled(false)") {
					t.Errorf("%s: %s calls %s and does not disable its Paste (KeyMap.Paste.SetEnabled(false))", name, fn.Name.Name, ctor)
				}
				// And it has an end: inputMaxChars for a line, editorMaxText
				// for the editor's text area.
				limit := map[string]string{"textinput.New()": "CharLimit = inputMaxChars", "textarea.New()": "CharLimit = editorMaxText"}[ctor]
				if !strings.Contains(body, limit) {
					t.Errorf("%s: %s calls %s and does not limit what it takes (%s)", name, fn.Name.Name, ctor, limit)
				}
			}
		}
	}
	if found == 0 {
		t.Error("no text input was found: the check looks in the wrong place")
	}
}
