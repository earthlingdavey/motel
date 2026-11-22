package box

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The box's refusals and failures are wire.Error, whose text is a
// wire.ErrText: an error's text, which no filter runs over once it is
// in an answer. The box makes one from a string that is not a constant
// in one place, errorBody, over sentences of its catalogue, its own
// state and the push's id (handler.go): never by a conversion, and never
// anywhere else, so a new maker of such text is a decision written down
// here (liveswap's TestErrTextMakers holds liveswap's).
func TestBoxErrTextMakers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "wire" && (sel.Sel.Name == "OwnText" || sel.Sel.Name == "ErrText" || sel.Sel.Name == "AppText") {
						where := "outside a function"
						if fn != nil {
							where = fn.Name.Name
						}
						got = append(got, name+": "+sel.Sel.Name+" in "+where)
					}
				}
				return true
			})
		}
	}
	if strings.Join(got, "; ") != "handler.go: OwnText in errorBody" {
		t.Errorf("the box makes error text in %v; want handler.go's errorBody alone", got)
	}
}
