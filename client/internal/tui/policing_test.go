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

const mod = "github.com/smallhoursorg/motel/client/"

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

func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	return out
}

func imports(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range goFiles(t, dir) {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			out[p] = name
		}
	}
	return out
}

// allows is each package's exact set of imports, the standard
// library's among them: the ones it uses today, and nothing more. A
// package the TUI reaches may import nothing else, so nothing reads a
// file, the environment or the build, runs a program, or reaches the
// network: what needs any of that is in cli/tui.go, which starts the
// TUI. model draws nothing (no tview, no tcell); only the screens and
// the door use tview; nothing reaches the client, sshkey or local.
var allows = map[string][]string{
	"tui": {"context", "runtime/debug", "slices", "strconv", "strings", "sync", "sync/atomic", "time",
		"github.com/rivo/tview", "github.com/gdamore/tcell/v2",
		mod + "internal/termtext", mod + "internal/tui/model", mod + "internal/tui/door"},
	"door": {"strings", "github.com/rivo/tview", "github.com/gdamore/tcell/v2",
		mod + "internal/termtext", mod + "internal/tui/model"},
	"model": {"bytes", "context", "encoding/json", "errors", "regexp", "slices", "strconv", "strings", "sync", "time",
		mod + "internal/termtext", "github.com/smallhoursorg/motel/liveswap/wire"},
	"demo": {"context", "embed", "encoding/json", "fmt", "io/fs", "regexp", "strconv", "sync", "time",
		mod + "internal/tui/model", "github.com/smallhoursorg/motel/liveswap/wire"},
	"termtext": {"fmt", "strings", "unicode", "unicode/utf8"},
	"wire":     {"encoding/json", "time"},
}

// importBreaks is every import of pkg (a key of allows) that is not on
// its list.
func importBreaks(pkg string, imps map[string]string) []string {
	var out []string
	for p, file := range imps {
		ok := false
		for _, a := range allows[pkg] {
			ok = ok || p == a
		}
		if !ok {
			out = append(out, file+" imports "+p)
		}
	}
	return out
}

// TestTUIImports holds each of the TUI's packages, and each package of
// this repository they reach, to its own list: none reaches the
// client, a transport, a program, the environment, a file or a
// signal, so the TUI can make no request of its own and read no key.
func TestTUIImports(t *testing.T) {
	seen := map[string]bool{}
	var walk func(path, dir string)
	walk = func(path, dir string) {
		if seen[path] {
			return
		}
		seen[path] = true
		name := filepath.Base(path)
		if _, ok := allows[name]; !ok {
			t.Errorf("the TUI reaches %s, which is on no list", path)
			return
		}
		imps := imports(t, dir)
		for _, b := range importBreaks(name, imps) {
			t.Error(b)
		}
		for p := range imps {
			if d, ok := srcDir(p); ok {
				walk(p, d)
			}
		}
	}
	walk(mod+"internal/tui", ".")
	walk(mod+"internal/tui/demo", "demo")
	for _, want := range []string{mod + "internal/tui/model", mod + "internal/tui/door", mod + "internal/termtext", "github.com/smallhoursorg/motel/liveswap/wire"} {
		if !seen[want] {
			t.Errorf("the walk never reached %s: the test reads the wrong tree", want)
		}
	}
	// And it finds what it is for: a dial through syslog, a process,
	// a key file, the environment, a model that draws.
	for _, c := range []struct{ pkg, imp string }{
		{"model", "log/syslog"}, // a TCP dial: syslog.Dial("tcp", …)
		{"tui", "os"},           // os.StartProcess, os.Open of a key, os.Getenv("MOTEL_TOKEN")
		{"door", "os/exec"},     // a program
		{"model", "net/http"},   // a request of its own
		{"model", "github.com/rivo/tview"},
		{"model", "github.com/gdamore/tcell/v2"},
		{"demo", mod + "internal/client"},
		{"tui", mod + "internal/sshkey"},
		// What reads a file, the environment or the build.
		{"model", "io/ioutil"},
		{"model", "go/build"},
		{"model", "text/template"},
		{"model", "debug/buildinfo"},
		{"model", "runtime/debug"},
		{"model", "embed"},
		{"door", "fmt"},
	} {
		if len(importBreaks(c.pkg, map[string]string{c.imp: "probe.go"})) == 0 {
			t.Errorf("TestTUIImports lets %s import %s", c.pkg, c.imp)
		}
	}
}

// handOver are the tview calls that take text to show.
var handOver = map[string]bool{
	"SetText": true, "SetTitle": true, "SetLabel": true, "SetPlaceholder": true, "Write": true, "WriteString": true,
	"NewTableCell": true, "SetCellSimple": true, "Print": true, "PrintSimple": true, "AddButtons": true,
	"String": true, "SetAutocompleteFunc": true, "AddOption": true, "SetOptions": true,
}

// primitives are the tview widgets the screens use. Any other (a
// button, a dropdown, a tree node, a list, a modal, a form) takes text
// by ways the door does not cover, and is not made here.
var primitives = map[string]bool{
	"NewApplication": true, "NewTextView": true, "NewTable": true, "NewFlex": true, "NewPages": true, "NewInputField": true,
}

// doorBreaks is every place in src that hands tview text without the
// door: a call above, a tview value built by hand, a widget not on the
// list, a Text field set, a List's AddItem with text, fmt.
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
			if sel, ok := n.Type.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "tview" {
					say(n, "builds a tview."+sel.Sel.Name+" by hand")
				}
			}
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Text" {
					say(n, "sets a Text field")
				}
			}
		case *ast.CallExpr:
			fun, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := fun.X.(*ast.Ident); ok && x.Name == "door" {
				return true // the door's own setters, which take door.Shown
			}
			if x, ok := fun.X.(*ast.Ident); ok && x.Name == "tview" && strings.HasPrefix(fun.Sel.Name, "New") && !primitives[fun.Sel.Name] {
				say(n, "makes a tview."+fun.Sel.Name)
			}
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
		return true
	})
	return out
}

// TestOneDoor holds by syntax what the compiler cannot: no file of the
// screens' package hands tview text, builds a tview value by hand, makes
// a widget off the list, sets a Text field, or formats with fmt. The
// door's setters take door.Shown, which only package door can make
// (its field is unexported), so text reaches a widget only through
// door.Inert.
func TestOneDoor(t *testing.T) {
	for _, name := range goFiles(t, ".") {
		for _, b := range doorBreaks(t, name, nil) {
			t.Error(b)
		}
	}
	// And the check finds what it is for.
	for _, bad := range []string{
		`u.apps.SetCell(1, 1, &tview.TableCell{Text: r.Name})`,
		`u.title.SetText(r.Name)`,
		`u.apps.SetCell(1, 1, tview.NewTableCell(r.Name))`,
		`c := door.Cell(x); c.Text = r.Name`,
		`l.AddItem(r.Name, "", 0, nil)`,
		`u.over.SetTitle(r.Name)`,
		`b := tview.NewButton(r.Name)`,
		`d := tview.NewDropDown().SetOptions([]string{r.Name}, nil)`,
		`n := tview.NewTreeNode(r.Name)`,
		`m := tview.NewModal()`,
	} {
		src := "package tui\nfunc f() {\n" + bad + "\n}\n"
		if len(doorBreaks(t, "probe.go", src)) == 0 {
			t.Errorf("TestOneDoor misses %s", bad)
		}
	}
}

// clientMethods are model.Client's methods.
var clientMethods = map[string]bool{"WhoAmI": true, "Apps": true, "Status": true, "Deploy": true, "BoxStatus": true}

// clientCalls is every call of a model.Client method's name in src.
func clientCalls(t *testing.T, name string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && clientMethods[sel.Sel.Name] {
				out = append(out, fset.Position(c.Pos()).String()+": calls "+sel.Sel.Name)
			}
		}
		return true
	})
	return out
}

// TestReadsOnlyThroughActions: a model.Client method is called in the
// actions table's file alone, so no read is made that the table does
// not name with its command.
func TestReadsOnlyThroughActions(t *testing.T) {
	for _, dir := range []string{".", "model", "door", "demo"} {
		for _, name := range goFiles(t, dir) {
			if name == filepath.Join("model", "actions.go") {
				continue
			}
			for _, c := range clientCalls(t, name, nil) {
				t.Error(c + ": a read outside model.Actions")
			}
		}
	}
	if len(clientCalls(t, "probe.go", "package model\nfunc f() { _, _ = c.Status(ctx, \"blog\") }\n")) == 0 {
		t.Error("TestReadsOnlyThroughActions misses a direct read")
	}
}
