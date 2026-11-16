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
	"unicode"
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

// QuoteMax is the most of a value a quotation holds: runes of what
// was sent, before any escape is written.
const QuoteMax = 200

// Quote is the one way motelctl quotes a value someone else sent (a
// box's sentence, a box's field, a certificate's reason) inside its own
// words. Every run of white space becomes one space and the ends are
// trimmed, so no run of blanks can wrap the value to the start of a
// row; s is cut to QuoteMax runes of what is left, on a rune boundary
// and before any escape is written, so a cut never splits one; every "
// and \ is escaped with a \; a look-alike quote mark, an invisible
// character (Unicode's Cf, and the Hangul fillers), and a combining mark
// that would land on the opening quote are written as \u escapes;
// everything From escapes is made visible; all between double quotes,
// with "…" after the closing quote when s was cut. The closing quote is
// the only unescaped quote mark of any look, so nothing s holds can end
// the quotation and write motelctl's next words. (A \ is escaped before
// From runs, so a \ the sender wrote reads \\, and an escape From wrote
// reads \x1b: the two stay apart.)
func Quote(s string) Text {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	cut := false
	n := 0
	for i := range s {
		if n == QuoteMax {
			s, cut = s[:i], true
			break
		}
		n++
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\' || r == '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == utf8.RuneError && size <= 1:
			b.WriteString(s[i : i+size]) // From writes U+FFFD
		case lookAlikeQuote(r), unicode.Is(unicode.Cf, r), hangulFiller(r), i == 0 && unicode.Is(unicode.M, r):
			if r > 0xffff {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	out := `"` + From(b.String()).s + `"`
	if cut {
		out += "…"
	}
	return Text{out}
}

// lookAlikeQuote is a quote mark a reader could take for ": the curly
// and low double quotes, the reversed ones, the double prime and its
// reverse, the fullwidth quote, the CJK double primes.
func lookAlikeQuote(r rune) bool {
	switch r {
	case 0x201c, 0x201d, 0x201e, 0x201f, 0x2033, 0x2036, 0xff02, 0x301d, 0x301e, 0x301f:
		return true
	}
	return false
}

// hangulFiller is a Hangul filler: a letter by category that prints as
// nothing.
func hangulFiller(r rune) bool {
	return r == 0x115f || r == 0x1160 || r == 0x3164 || r == 0xffa0
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
	// overrides, the isolates. And the six deprecated format characters
	// after the isolates (symmetric swapping, Arabic form shaping, the
	// digit shapes), which some terminals still act on.
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x206f:
		return true
	case r == 0x2028, r == 0x2029:
		return true
	}
	return false
}
