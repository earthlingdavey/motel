// Package termtext is the one way a string a box sent reaches the
// terminal. Every such string is hostile: a status field, an error
// sentence, a deploy's log tail or a probe body can carry terminal
// escapes (OSC 52 writes the clipboard, OSC 8 hides a link, CSI moves
// the cursor over what was printed) or bidi controls that reorder
// what a reader sees. A Text holds a string with all of those made
// visible, and only From makes one, so a renderer that takes Text for
// what a box sent cannot print it raw: the compiler holds the rule,
// and the rule is written once.
package termtext

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Text is a string made safe to print on a terminal: it holds no
// control character of any kind, no bidi control and no line or
// paragraph separator, and it is valid UTF-8. The zero Text is the
// empty string.
type Text struct {
	s string
}

// From sanitises s. Every C0 control (tab and newline among them), DEL
// and every C1 control becomes a visible escape, \x1b or \u009b; every
// bidi control (U+061C, U+200E, U+200F, U+202A to U+202E, U+2066 to
// U+2069) and U+2028 and U+2029 become \u202e and the like; a byte
// that is not UTF-8 becomes U+FFFD. Everything else is kept as it is.
// A multi-line field is split first, with Lines, so that its line
// breaks stay line breaks and nothing else does.
func From(s string) Text {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || unsafeRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return Text{s}
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteRune(utf8.RuneError)
		case r < 0x80 && unsafeRune(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case unsafeRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return Text{b.String()}
}

// Lines splits s at each line feed (a CR before one goes with it) and
// sanitises each line: a log tail, a probe body. Empty s is no lines.
func Lines(s string) []Text {
	if s == "" {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	out := make([]Text, len(parts))
	for i, p := range parts {
		out[i] = From(strings.TrimSuffix(p, "\r"))
	}
	return out
}

// String is the sanitised string.
func (t Text) String() string { return t.s }

// Cut is the text cut to at most n runes, with "…" in place of what
// was cut: a box's sentence quoted in motelctl's own message stays a
// quotation, not a page.
func (t Text) Cut(n int) Text {
	if utf8.RuneCountInString(t.s) <= n {
		return t
	}
	r := []rune(t.s)
	return Text{string(r[:n]) + "…"}
}

// Empty says whether the text is the empty string.
func (t Text) Empty() bool { return t.s == "" }

// unsafeRune is every rune From escapes.
func unsafeRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	// Unicode's whole Bidi_Control set: the Arabic letter mark, the
	// left-to-right and right-to-left marks, the embeddings and
	// overrides, the isolates.
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x2028, r == 0x2029:
		return true
	}
	return false
}
