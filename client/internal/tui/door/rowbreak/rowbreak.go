// Package rowbreak says where tview would begin a row inside a text the
// door hands it, for the door to write the character before each such
// break as an escape (door's unbreak).
//
// It is a package of its own for the time make test takes, and for no
// other reason. Under go test -race -cover the loop below, a step for
// each grapheme cluster, is counted at every step when it is the door's
// own code: the door's tests then took 112 s at 4 CPUs, against 93 s
// apart. And when go test is given several packages at once, a package
// with no test file was instrumented into the other packages' test
// binaries: this package without one, the door's tests used 470 s of
// CPU under go test ./client/... at 4 CPUs, 336 s with one (the door on
// main, 340 to 347 s). That second effect was measured, not read in the
// go command; so this package keeps a test file (rowbreak_test.go).
package rowbreak

import (
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// At is the byte offset in row of the last character, above ASCII, of
// each cluster after which tview would begin a row, inside row or at its
// end ("x" stands for what follows it), with a tag after the cluster or
// not. tview's step (strings.go, v0.42.0) judges the break after a
// cluster a tag follows by that cluster and the one character after the
// tag, as if the text ended there, and the door's Line may write a tag
// after any cluster: so At asks uniseg that of each. Without a tag tview
// judges the break by the text after it too, and uniseg reads a second
// character after a cluster only to take a break away (LB25: none
// before a hyphen a digit follows), so each break tview begins with no
// tag is found too: the door's TestOneRow, TestClassesOnOneRow and
// FuzzInert hold that, drawn. So a thumbs-up and skin tone before "-0"
// is written as an escape, as before any other character, in the door's
// Line and Inert alike, and its Width agrees. A break after ASCII is
// left as it is: no character of ASCII ends a row but the ones termtext
// escapes.
func At(row string) (at []int) {
	row += "x"
	state := -1
	for i, rest := 0, row; rest != ""; {
		pre := state
		var c string
		c, rest, _, state = uniseg.StepString(rest, state)
		if r, n := utf8.DecodeLastRuneInString(c); rest != "" && r >= 0x80 {
			_, m := utf8.DecodeRuneInString(rest)
			if _, _, b, _ := uniseg.StepString(row[i:i+len(c)+m], pre); b&uniseg.MaskLine == uniseg.LineMustBreak {
				at = append(at, i+len(c)-n)
			}
		}
		i += len(c)
	}
	return at
}
