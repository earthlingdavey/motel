package box

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/liveswap"
)

const (
	// maxDiff caps the diff a record and a result carry (DESIGN-box.md,
	// "Caps": 64 KiB, cut with a note): the record is one atomic write.
	maxDiff = 64 << 10
	// maxDiffRedacted is how much of the diff is handed to the redactor:
	// four times the cap, cut at a line end, so the cut after redaction
	// never lands inside a token the redactor saw only half of, and a
	// 2 MiB diff of two 1 MiB files costs the redactor no more than this.
	maxDiffRedacted = 4 * maxDiff
	// maxEdits bounds the edit distance the line diff searches. Its
	// memory is quadratic in the distance; past it the changed middle
	// is shown as one hunk, still a correct diff.
	maxEdits = 512
	// diffContext is the unchanged lines shown around a change.
	diffContext = 3
)

// caddyfileDiff is the `diff` field: a unified diff of the installed
// file against the incoming one, redacted with liveswap's shape rules
// (layer 2; layer 1's env_file values are the service's, which root
// does not have), then cut at maxDiff on a line end with a note.
func caddyfileDiff(prev, next []byte) string {
	d, cut := renderDiff(prev, next, maxDiffRedacted)
	if d == "" {
		return ""
	}
	if cut {
		d = cutAtLine(d, maxDiffRedacted)
	}
	d, _ = liveswap.NewRedactor(nil).Redact(d)
	if cut || len(d) > maxDiff {
		d = cutAtLine(d, maxDiff-len(diffCutNote)) + diffCutNote
	}
	return d
}

const diffCutNote = "\n… (diff cut at 64 KiB)\n"

// cutAtLine is s cut to at most n bytes, at the end of a line when one
// ends inside the first n bytes, else at a rune boundary.
func cutAtLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if i := strings.LastIndexByte(s[:n], '\n'); i >= 0 {
		return s[:i+1]
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type editKind byte

const (
	editEqual  editKind = ' '
	editDelete editKind = '-'
	editInsert editKind = '+'
)

type edit struct {
	kind editKind
	a, b int // line index in the old and the new file (the one this edit consumes)
}

// renderDiff is a unified diff of a against b, "" when they are equal,
// stopped once the text passes limit bytes (no limit when it is 0): cut
// reports that it stopped, the text then ending at a line end somewhere
// past the limit. Lines are compared whole, newline included, so a last
// line without one differs from the same text with one and is marked as
// git marks it.
func renderDiff(a, b []byte, limit int) (d string, cut bool) {
	if bytes.Equal(a, b) {
		return "", false
	}
	al, bl := splitKeepNL(a), splitKeepNL(b)
	edits := diffLines(al, bl)
	var out strings.Builder
	out.WriteString("--- installed\n+++ pushed\n")
	for i := 0; i < len(edits); {
		// A hunk starts diffContext lines before a change and runs until
		// more than 2×diffContext equal lines separate two changes.
		for i < len(edits) && edits[i].kind == editEqual {
			i++
		}
		if i == len(edits) {
			break
		}
		start := max(i-diffContext, 0)
		end := i
		for end < len(edits) {
			if edits[end].kind != editEqual {
				end++
				continue
			}
			run := end
			for run < len(edits) && edits[run].kind == editEqual {
				run++
			}
			if run == len(edits) || run-end > 2*diffContext {
				end = min(end+diffContext, len(edits))
				break
			}
			end = run
		}
		if !writeHunk(&out, al, bl, edits[start:end], limit) {
			return out.String(), true
		}
		i = end
	}
	return out.String(), false
}

// writeHunk writes one hunk, or stops and says false once the text
// passes limit.
func writeHunk(out *strings.Builder, al, bl []string, hunk []edit, limit int) bool {
	aStart, bStart := hunk[0].a, hunk[0].b
	var aLen, bLen int
	for _, e := range hunk {
		switch e.kind {
		case editEqual:
			aLen++
			bLen++
		case editDelete:
			aLen++
		case editInsert:
			bLen++
		}
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
	for _, e := range hunk {
		line := ""
		switch e.kind {
		case editEqual, editDelete:
			line = al[e.a]
		case editInsert:
			line = bl[e.b]
		}
		out.WriteByte(byte(e.kind))
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
		if limit > 0 && out.Len() > limit {
			return false
		}
	}
	return true
}

// hunkRange is a unified diff's "start,count", 1-based; an empty range
// names the line before it, as diff(1) does.
func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func splitKeepNL(b []byte) []string {
	var lines []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			lines = append(lines, string(b))
			break
		}
		lines = append(lines, string(b[:i+1]))
		b = b[i+1:]
	}
	return lines
}

// diffLines is the edit script from a to b: the common prefix and
// suffix as they stand, the middle by Myers' algorithm when its edit
// distance is at most maxEdits, else deleted whole and inserted whole.
func diffLines(a, b []string) []edit {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var edits []edit
	for i := 0; i < pre; i++ {
		edits = append(edits, edit{editEqual, i, i})
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	mid, ok := myers(am, bm, maxEdits)
	if !ok {
		mid = mid[:0]
		for i := range am {
			mid = append(mid, edit{editDelete, i, 0})
		}
		for j := range bm {
			mid = append(mid, edit{editInsert, len(am), j})
		}
	}
	for _, e := range mid {
		edits = append(edits, edit{e.kind, e.a + pre, e.b + pre})
	}
	for k := 0; k < suf; k++ {
		edits = append(edits, edit{editEqual, len(a) - suf + k, len(b) - suf + k})
	}
	return edits
}

// myers is the shortest edit script from a to b (Myers, "An O(ND)
// difference algorithm", 1986), or false when it is longer than limit.
// Each edit carries the positions in both files where it stands.
func myers(a, b []string, limit int) ([]edit, bool) {
	n, m := len(a), len(b)
	maxD := min(n+m, limit)
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	var trace [][]int
	for d := 0; d <= maxD; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: an insertion
			} else {
				x = v[off+k-1] + 1 // right: a deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, off, n, m, d), true
			}
		}
	}
	return nil, false
}

func backtrack(trace [][]int, off, x, y, d int) []edit {
	var rev []edit
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, edit{editEqual, x, y})
		}
		if x == prevX {
			y--
			rev = append(rev, edit{editInsert, x, y})
		} else {
			x--
			rev = append(rev, edit{editDelete, x, y})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, edit{editEqual, x, y})
	}
	out := make([]edit, len(rev))
	for i, e := range rev {
		out[len(rev)-1-i] = e
	}
	return out
}
