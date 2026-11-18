package render

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The renderer never sees a wire type, so every string from a box
// reaches it as a termtext.Text, made by termtext.From in the one
// place the views are built (the cli package). A renderer that took a
// wire value could print its strings raw.
func TestRenderNeverSeesTheWire(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(path, "/wire") || strings.Contains(path, "client/internal/client") {
				t.Errorf("%s imports %s: the renderer takes views of termtext.Text, never what a box sent", f, path)
			}
		}
	}
}
