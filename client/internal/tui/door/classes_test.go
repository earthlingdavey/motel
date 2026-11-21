package door

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// reps is one code point of each Unicode line breaking class (UAX #14)
// and each grapheme cluster break class (UAX #29), and of
// Extended_Pictographic, as Unicode 15.0.0 gives them (LineBreak.txt,
// GraphemeBreakProperty.txt, emoji-data.txt): the version uniseg
// v0.4.7's tables, and so tview's, are made from. Neither uniseg nor
// x/text says a code point's class, so they are picked by hand, each
// with the classes it stands for. Left out: SG (a surrogate, which no
// Go string holds) and the classes Unicode 15.1 added (AK, AP, AS, VF,
// VI, and the InCB values), which uniseg v0.4.7 does not know.
// And the classes termtext writes as escapes, so that tview never gets
// one: BK, CR, LF and NL (a line feed, a carriage return, a form feed,
// NEL, the line and paragraph separators), Control's C0 and C1
// controls, and CM's C0 controls. Their escapes are ASCII, which 'a'
// and the punctuation below stand for.
var reps = []struct {
	r       rune
	classes string
}{
	{'a', "AL, Other"},
	{' ', "SP"},
	{0x0301, "CM, Extend"},
	{0x034f, "GL (the combining grapheme joiner), Extend"},
	{0x200c, "CM (zero width non-joiner), Extend"},
	{0x200b, "ZW, Control"},
	{0x2060, "WJ, Control"},
	{0xfeff, "WJ (the byte order mark), Control"},
	{0x00ad, "BA (the soft hyphen), Control"},
	{0x200d, "ZWJ (both)"},
	{0x00a0, "GL"},
	{0x2014, "B2"},
	{0x2010, "BA"},
	{0x00b4, "BB"},
	{'-', "HY"},
	{0xfffc, "CB"},
	{'}', "CL"},
	{')', "CP"},
	{'!', "EX"},
	{0x2026, "IN"},
	{0x3005, "NS"},
	{'(', "OP"},
	{'"', "QU"},
	{',', "IS"},
	{'0', "NU"},
	{'%', "PO"},
	{'$', "PR"},
	{'/', "SY"},
	{'[', "OP (the one the door makes inert)"},
	{0x00a7, "AI"},
	{0x3041, "CJ"},
	{0x1f44d, "EB, Extended_Pictographic"},
	{0x1f3fd, "EM, Extend"},
	{0x2764, "AL, Extended_Pictographic"},
	{0x1faff, "ID (unassigned), Extended_Pictographic"},
	{0xac00, "H2, LV"},
	{0xac01, "H3, LVT"},
	{0x05d0, "HL"},
	{0x4e00, "ID"},
	{0x1100, "JL, L"},
	{0x1161, "JV, V"},
	{0x11a8, "JT, T"},
	{0x1f1e6, "RI (both)"},
	{0x0e01, "SA"},
	{0x0378, "XX (unassigned), Other"},
	{0x0600, "AL, Prepend (a format character)"},
	{0x0d4e, "AL, Prepend (a letter)"},
	{0x0903, "CM, SpacingMark"},
}

// TestClassesOnOneRow: every pair and every triple of reps, each between
// < and >, a few thousand at a time as one text: in every kind of
// widget the door serves (kinds) on one row, and in the kinds that hold
// the box's text (boxWidths) as many cells wide as Width says.
// TestOneRow draws every code point and the pairs known to be bad; this
// draws every class beside every other, before and after, which blind
// fuzzing would take long to reach. Styles are FuzzInert's: no rep is a
// tag. Seen to fail: with Inert's uniseg guard taken out, on an
// Extended_Pictographic character then a skin tone (U+1FAFF U+1F3FD
// after a Hangul vowel, U+1F44D U+1F3FD after a regional indicator:
// not on one row); with Width measuring the text before the door, on
// the same (drawn wider than Width says, by the escape).
func TestClassesOnOneRow(t *testing.T) {
	var pairs, triples []string
	for _, x := range reps {
		for _, y := range reps {
			pairs = append(pairs, "<"+string(x.r)+string(y.r)+">")
			for _, z := range reps {
				triples = append(triples, "<"+string(x.r)+string(y.r)+string(z.r)+">")
			}
		}
	}
	const batch, parts = 4000, 4
	for _, k := range kinds() {
		hand := measured[k.name]
		cases := pairs
		if triplesIn[k.name] {
			cases = append(slices.Clip(pairs), triples...)
		}
		for p := range min(parts, (len(cases)+batch-1)/batch) {
			t.Run(k.name+"/"+strconv.Itoa(p+1), func(t *testing.T) {
				t.Parallel()
				for i := p * batch; i < len(cases); i += parts * batch {
					b := cases[i:min(i+batch, len(cases))]
					text := strings.Join(b, "")
					notOne := func() {
						t.Errorf("the cases from %s to %s are not drawn on one row: %s", runes(b[0]), runes(b[len(b)-1]), culprit(b, func(c string) bool { return k.one([]string{c}) }))
					}
					if !boxWidths[k.name] {
						if !k.one(b) {
							notOne()
						}
						continue
					}
					// Drawn once, as wide as it is, for both checks.
					want := Width(termtext.From(hand(text)))
					scr := k.drawn(text, 16+want)
					if !k.oneOn(scr, 16+want) {
						notOne()
					}
					if from, to, ok := span(scr, k.top); !ok || to-from != want {
						t.Errorf("the cases from %s to %s are drawn %d cells wide, and Width says %d: %s", runes(b[0]), runes(b[len(b)-1]), to-from, want, culprit(b, func(c string) bool {
							f, to, ok := span(k.drawn(c, 16+Width(termtext.From(hand(c)))), k.top)
							return ok && to-f == Width(termtext.From(hand(c)))
						}))
					}
				}
			})
		}
	}
}

// triplesIn are the kinds the triples are drawn in: the ones that can
// begin a row (a text view, as the screens hand it box lines and lines,
// and an input field's text) and the ones measured (boxWidths). Every
// kind draws the pairs: the triples in all nine (of 54 reps then, the
// escaped classes among them) took the door's tests from 91 s to 143 s
// under -race; so, 117 s.
var triplesIn = map[string]bool{"text view, box lines": true, "text view, a line": true, "completion taken": true, "table cell": true}

// boxWidths are the measured kinds that hold the box's text, where
// the screens cut by Width (a text view's lines, the overlays among
// them, and a table's cells); the label, the printed line and the
// TUI's own lines hold only its own words, and FuzzInert measures
// them.
var boxWidths = map[string]bool{"text view, box lines": true, "table cell": true}

// culprit names the first of cases that does not hold alone, or says
// none does.
func culprit(cases []string, holds func(string) bool) string {
	for _, c := range cases {
		if !holds(c) {
			return runes(c) + " alone does not"
		}
	}
	return "each alone does"
}

// TestClassesBesideATag: every pair of reps with a tag between them, in
// a Line: the first at the end of one segment and the second at the
// start of the next, dressed then plain (the tag that closes a
// segment), plain then dressed (the one that opens it), and one dressed
// class then another (both); a few hundred pairs at a time as one
// Line, each pair alone between zero-width spaces, which begin and end
// a grapheme cluster whatever is beside them. Each is held as checkLine
// holds a line. Seen to fail: with Line's unbreak taken out, on the
// thumbs-up then the skin tone across a tag (not on one row); with Line
// writing no closing tag, in every batch (a style left open); with
// whole taken out of Line, on a spacing mark across a tag (5 cells
// drawn, Width 4); with no zero-width space before a tag after a
// Prepend character, on an Arabic number sign before a tag and a
// zero-width space (7 cells drawn, Width 3, a style left open).
func TestClassesBesideATag(t *testing.T) {
	type pair struct {
		a, b   model.Class
		colour bool
	}
	for _, p := range []pair{{model.Bad, model.Plain, true}, {model.Plain, model.Bad, true}, {model.Dim, model.Strong, false}} {
		name := className[p.a] + " then " + className[p.b]
		t.Run(name, func(t *testing.T) {
			var l model.Line
			for _, x := range reps {
				for _, y := range reps {
					l = append(l, model.Seg{T: termtext.From("\u200b")})
					l = append(l, model.Seg{T: termtext.From("q" + string(x.r)), C: p.a}, model.Seg{T: termtext.From(string(y.r) + "q"), C: p.b})
				}
			}
			const batch = 3 * 300
			for i := 0; i < len(l); i += batch {
				b := l[i:min(i+batch, len(l))]
				if !t.Run(strconv.Itoa(i/batch), func(t *testing.T) { checkLine(t, b, p.colour) }) {
					for j := 0; j < len(b); j += 3 {
						if !t.Run("alone", func(t *testing.T) { checkLine(t, b[j+1:j+3], p.colour) }) {
							break
						}
					}
				}
			}
		})
	}
}
