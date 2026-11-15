package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func parseDir(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	out := map[string]*ast.File{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	return out
}

// TestTUIImports: the TUI makes no request of its own (no package of
// it imports net/http, or any net package); the screens see the box
// only through model (no wire types, no client); model draws nothing.
func TestTUIImports(t *testing.T) {
	banned := map[string][]string{
		".":     {"net", "net/http", "net/url", "crypto/tls", "github.com/smallhoursorg/motel/liveswap/wire", "github.com/smallhoursorg/motel/motelctl/internal/client", "fmt", "log", "reflect", "unsafe", "encoding/json", "text/template"},
		"model": {"net", "net/http", "net/url", "crypto/tls", "github.com/rivo/tview", "github.com/gdamore/tcell/v2"},
		"demo":  {"net", "net/http", "net/url", "crypto/tls", "github.com/rivo/tview", "github.com/gdamore/tcell/v2"},
	}
	for dir, bad := range banned {
		for name, f := range parseDir(t, dir) {
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				for _, b := range bad {
					if p == b {
						t.Errorf("%s imports %s", name, p)
					}
				}
			}
		}
	}
}

// handOver are the tview calls that take text: in this package only
// boxtext.go makes them.
var handOver = map[string]bool{
	"SetText": true, "SetTitle": true, "SetLabel": true, "SetPlaceholder": true, "Write": true,
	"NewTableCell": true, "SetCellSimple": true, "Print": true, "PrintSimple": true, "AddItem": false,
	"String": true, "SetAutocompleteFunc": false, "NewModal": true, "AddButtons": true,
}

// TestOneDoor holds what the compiler cannot: no file of this package
// but boxtext.go hands tview text, calls String (which would skip the
// tag escape), or formats with fmt. A string from a box can then reach
// a widget only through inert.
func TestOneDoor(t *testing.T) {
	fset := token.NewFileSet()
	for name := range parseDir(t, ".") {
		if filepath.Base(name) == "boxtext.go" {
			continue
		}
		f, _ := parser.ParseFile(fset, name, nil, 0)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && handOver[sel.Sel.Name] {
				t.Errorf("%s: %s outside boxtext.go", fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}
