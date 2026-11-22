package wire

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// textClasses are the values a `text` tag may have (the package doc):
// moteld's own, or text that passes as written because it is the
// caller's, the operator's or another program's. The app's text and an
// error's text are classed by their types, AppText and ErrText, and
// carry no tag.
var textClasses = map[string]bool{"moteld": true, "passes": true}

// classed is every field of the package's struct types that holds text
// on the wire, by Type.Field, with its class: "AppText", "ErrText", or
// the tag's. A field that holds text and has neither a type that says
// so nor a tag is "", which the test reports.
func classed(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				fields(t, ts.Name.Name, st, out)
			}
			return false
		})
	}
	return out
}

// fields adds st's text fields to out under prefix, and those of every
// struct type written inline in it.
func fields(t *testing.T, prefix string, st *ast.StructType, out map[string]string) {
	for _, fld := range st.Fields.List {
		var tag reflect.StructTag
		if fld.Tag != nil {
			s, err := strconv.Unquote(fld.Tag.Value)
			if err != nil {
				t.Fatal(err)
			}
			tag = reflect.StructTag(s)
		}
		if len(fld.Names) == 0 || tag.Get("json") == "-" {
			continue // embedded (its own type is walked), or not on the wire
		}
		for _, id := range fld.Names {
			if !id.IsExported() {
				continue
			}
			key := prefix + "." + id.Name
			holds, typed, inline := text(fld.Type)
			if inline != nil {
				fields(t, key, inline, out)
			}
			class, tagged := tag.Lookup("text")
			switch {
			case typed != "" && tagged:
				t.Errorf("%s is %s, which classes it, and has a text tag too", key, typed)
			case typed != "":
				out[key] = typed
			case holds && !textClasses[class]:
				out[key] = ""
			case holds:
				out[key] = class
			case tagged:
				t.Errorf("%s holds no text and has a text tag %q", key, class)
			}
		}
	}
}

// text reports whether a field of type e holds text on the wire: a
// string, json.RawMessage (bytes a record stored), or a slice, array,
// pointer or map of them (a map's key is text too); typed, the text
// type that classes it; inline, a struct type written in place.
func text(e ast.Expr) (holds bool, typed string, inline *ast.StructType) {
	switch x := e.(type) {
	case *ast.Ident:
		switch x.Name {
		case "string":
			return true, "", nil
		case "AppText", "ErrText":
			return true, x.Name, nil
		}
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "json" && x.Sel.Name == "RawMessage" {
			return true, "", nil
		}
	case *ast.StarExpr:
		return text(x.X)
	case *ast.ArrayType:
		return text(x.Elt)
	case *ast.MapType:
		kh, kt, _ := text(x.Key)
		vh, vt, vi := text(x.Value)
		if kt == "" {
			kt = vt
		}
		return kh || vh, kt, vi
	case *ast.StructType:
		return false, "", x
	}
	return false, "", nil
}

// Every string field of every wire type is classed (the package doc):
// moteld's own or passing as written by its tag, the app's text or an
// error's by its type. Nothing filters an answer by default, so a field
// nobody classed would be sent as it stands with nobody having said
// whose text it holds: this fails for one.
func TestEveryStringFieldIsClassed(t *testing.T) {
	got := classed(t)
	var none []string
	for k, v := range got {
		if v == "" {
			none = append(none, k)
		}
	}
	sort.Strings(none)
	for _, k := range none {
		t.Errorf(`%s holds text and is not classed: type it AppText or ErrText, or tag it text:"moteld" or text:"passes"`, k)
	}
	// The walk sees the fields it must: one of each class, and the
	// kinds of type a field can hold text in.
	for k, want := range map[string]string{
		"DeployDetail.LogTail":   "AppText", // a slice
		"ProbeDetail.Body":       "AppText", // the type itself
		"Watchdog.LastFailure":   "ErrText", // the type itself
		"Error.Error":            "ErrText", // every refusal: never a plain string
		"Status.App":             "moteld",  // a string
		"Status.Command":         "passes",  // a slice of strings
		"DeploySummary.SHA256":   "moteld",  // json.RawMessage
		"RequestSummary.ByClass": "moteld",  // a map's key
		"LogLine.Text":           "AppText", // another file
		"DeployResult.Error":     "ErrText", // the record's
		"Status.LastDeploy":      "",        // a pointer to a struct: not text
	} {
		if v, ok := got[k]; want == "" && ok || want != "" && v != want {
			t.Errorf("%s: classed %q (present %v), want %q", k, v, ok, want)
		}
	}
}

// The test fails for a field nobody classed, and for a tag that names
// no class: the walk over a type written for this test.
func TestUnclassedFieldFails(t *testing.T) {
	src := `package wire
type T struct {
	A string ` + "`json:\"a\"`" + `
	B []string ` + "`json:\"b\" text:\"moteld\"`" + `
	C AppText ` + "`json:\"c\"`" + `
	D map[string]int ` + "`json:\"d\"`" + `
	E string ` + "`json:\"e\" text:\"other\"`" + `
	F string ` + "`json:\"-\"`" + `
	G struct {
		H string ` + "`json:\"h\"`" + `
	} ` + "`json:\"g\"`" + `
}`
	f, err := parser.ParseFile(token.NewFileSet(), "t.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	st := f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType)
	fields(t, "T", st, out)
	want := map[string]string{"T.A": "", "T.B": "moteld", "T.C": "AppText", "T.D": "", "T.E": "", "T.G.H": ""}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("got %v, want %v", out, want)
	}
}
