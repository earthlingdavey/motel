package rowbreak

// This package keeps a test file for make test's time too: without one,
// go test -cover given several packages at once instrumented it into
// the other packages' test binaries, the door's among them (the package
// comment has the numbers).

import (
	"slices"
	"testing"
)

// TestAt: where At says tview would begin a row, as the byte offset of
// the character before it. An emoji modifier after an emoji base, or
// after an unassigned pictograph, is one: before a hyphen and a digit
// (where uniseg alone would take no break, but tview, a tag after the
// pair, sees only the hyphen), before another character, at the row's
// end, and inside a row. Nothing in ASCII, an empty row, or an emoji
// base without a modifier. The door's tests hold what At says against
// tview, drawn.
func TestAt(t *testing.T) {
	for _, c := range []struct {
		row  string
		want []int
	}{
		{"\U0001f44d\U0001f3fd-0", []int{4}},
		{"a\U0001faff\U0001f3fd-0", []int{5}},
		{"\U0001f44d\U0001f3fdz", []int{4}},
		{"\U0001f44d\U0001f3fd", []int{4}},
		{"ok \U0001f44d\U0001f3fd ok", []int{7}},
		{"plain words, -0, $(1) and [x]", nil},
		{"", nil},
		{"\U0001f44d ok", nil},
	} {
		if got := At(c.row); !slices.Equal(got, c.want) {
			t.Errorf("At(%q) = %v, want %v", c.row, got, c.want)
		}
	}
}
