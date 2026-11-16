package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const mod = "github.com/smallhoursorg/motel/motelctl/"

// srcDir is a package's directory from its import path, for the
// module's packages and liveswap/wire next door.
func srcDir(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, mod):
		return filepath.Join("..", "..", strings.TrimPrefix(path, mod)), true
	case path == "github.com/smallhoursorg/motel/liveswap/wire":
		return filepath.Join("..", "..", "..", "liveswap", "wire"), true
	}
	return "", false
}

func imports(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	out := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			out[p] = name
		}
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	return out
}

// allowed is what a TUI package may import: the standard library but
// for its transports and programs, tview and tcell, termtext, the wire
// types, and the TUI's own packages. The client, sshkey and local are
// not among them: the TUI reads the box only through model.Client,
// which the one file that starts it (cli/tui.go) builds.
func allowed(p string) bool {
	switch {
	case p == "net/http", p == "os/exec", p == "net", strings.HasPrefix(p, "net/"), p == "crypto/tls", p == "syscall", p == "unsafe", p == "plugin":
		return false
	case !strings.Contains(strings.Split(p, "/")[0], "."):
		return true // the standard library
	case p == "github.com/rivo/tview", strings.HasPrefix(p, "github.com/gdamore/tcell/v2"):
		return true
	case p == mod+"internal/termtext", p == "github.com/smallhoursorg/motel/liveswap/wire":
		return true
	case strings.HasPrefix(p, mod+"internal/tui"):
		return true
	}
	return false
}

// TestTUIImports holds the TUI to its allow-list, through every package
// of this repository it reaches: none of them may reach the client, a
// transport or a program, so the TUI can make no request of its own.
func TestTUIImports(t *testing.T) {
	seen := map[string]bool{}
	var walk func(path, dir string)
	walk = func(path, dir string) {
		if seen[path] {
			return
		}
		seen[path] = true
		for p, file := range imports(t, dir) {
			if !allowed(p) {
				t.Errorf("%s imports %s, which the TUI may not reach", file, p)
				continue
			}
			if d, ok := srcDir(p); ok {
				walk(p, d)
			}
		}
	}
	walk(mod+"internal/tui", ".")
	walk(mod+"internal/tui/model", "model")
	walk(mod+"internal/tui/demo", "demo")
	for _, want := range []string{mod + "internal/tui/model", mod + "internal/termtext", "github.com/smallhoursorg/motel/liveswap/wire"} {
		if !seen[want] {
			t.Errorf("the walk never reached %s: the test reads the wrong tree", want)
		}
	}
}

// handOver are the tview calls that take text to show.
var handOver = map[string]bool{
	"SetText": true, "SetTitle": true, "SetLabel": true, "SetPlaceholder": true, "Write": true, "WriteString": true,
	"NewTableCell": true, "SetCellSimple": true, "Print": true, "PrintSimple": true, "NewModal": true, "AddButtons": true,
	"String": true, "SetAutocompleteFunc": true,
}

// doorBreaks is every place in src (a file other than boxtext.go) that
// hands tview text without the door: a call above, a table cell built
// by hand, a Text field set, a conversion to shown, a List's AddItem
// with text, fmt.
func doorBreaks(t *testing.T, name string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	say := func(n ast.Node, what string) { out = append(out, fset.Position(n.Pos()).String()+": "+what) }
	for _, imp := range f.Imports {
		if imp.Path.Value == `"fmt"` {
			say(imp, "imports fmt")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "TableCell" {
				say(n, "builds a table cell")
			}
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Text" {
					say(n, "sets a Text field")
				}
			}
		case *ast.CallExpr:
			switch fun := n.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "shown" {
					say(n, "converts to shown")
				}
			case *ast.SelectorExpr:
				if handOver[fun.Sel.Name] {
					say(n, "calls "+fun.Sel.Name)
				}
				// A Flex's AddItem takes a widget and a size; any other
				// (a List's, a Form's) takes text.
				if fun.Sel.Name == "AddItem" {
					if len(n.Args) != 4 {
						say(n, "AddItem with text")
					} else if b, ok := n.Args[1].(*ast.BasicLit); !ok || b.Kind != token.INT {
						say(n, "AddItem with text")
					}
				}
			}
		}
		return true
	})
	return out
}

// TestOneDoor holds by syntax what the compiler cannot: no file of
// this package but boxtext.go hands tview text, builds a table cell,
// sets a Text field, converts to shown, or formats with fmt. Its
// helpers take shown, which only boxtext.go makes, so text reaches a
// widget only through inert.
func TestOneDoor(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "boxtext.go" {
			continue
		}
		for _, b := range doorBreaks(t, name, nil) {
			t.Error(b)
		}
	}
	// And the check finds what it is for.
	for _, bad := range []string{
		`u.apps.SetCell(1, 1, &tview.TableCell{Text: r.Name})`,
		`u.title.SetText(r.Name)`,
		`setText(u.title, shown(r.Name))`,
		`u.apps.SetCell(1, 1, tview.NewTableCell(r.Name))`,
		`c := cell(x); c.Text = r.Name`,
		`l.AddItem(r.Name, "", 0, nil)`,
		`u.over.SetTitle(r.Name)`,
	} {
		src := "package tui\nfunc f() {\n" + bad + "\n}\n"
		if len(doorBreaks(t, "probe.go", src)) == 0 {
			t.Errorf("TestOneDoor misses %s", bad)
		}
	}
	if _, err := os.Stat("boxtext.go"); err != nil {
		t.Fatal(err)
	}
}
