package wire

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// What the package doc promises, and the reason the package exists: a
// program that reads these bodies must not have to link Caddy, nor
// anything else outside the standard library — not even liveswap,
// which links it. The standard library imports only itself, so the
// direct imports decide the whole set (as backupdecl's test reasons).
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files found: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, which is not in the standard library", f, p)
			}
		}
	}
}

// A client decodes a status and loses nothing: liveswap's every-field
// status, decoded into Status and marshalled again, is the same bytes.
func TestStatusRoundTrips(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "testdata", "wire", "status-every-field.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Status
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(got)+"\n" != string(want) {
		t.Fatalf("round trip:\n got %s\nwant %s", got, want)
	}
}
