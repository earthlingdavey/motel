package box

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// unifiedDiff is renderDiff whole.
func unifiedDiff(a, b []byte) string {
	d, _ := renderDiff(a, b, 0)
	return d
}

// applyUnified applies a unified diff as patch(1) would, checking every
// context and deleted line: the oracle for unifiedDiff.
func applyUnified(a []byte, d string) ([]byte, error) {
	al := splitKeepNL(a)
	lines := strings.SplitAfter(d, "\n")
	if len(lines) < 2 || lines[0] != "--- installed\n" || lines[1] != "+++ pushed\n" {
		return nil, fmt.Errorf("no header")
	}
	var out []string
	pos := 0
	for i := 2; i < len(lines) && lines[i] != ""; {
		var as, an, bs, bn int
		if _, err := fmt.Sscanf(rangeFix(lines[i]), "@@ -%d,%d +%d,%d @@\n", &as, &an, &bs, &bn); err != nil {
			return nil, fmt.Errorf("hunk header %q: %w", lines[i], err)
		}
		start := as - 1
		if an == 0 {
			start = as
		}
		if start < pos || start > len(al) {
			return nil, fmt.Errorf("hunk at %d out of order", as)
		}
		out = append(out, al[pos:start]...)
		pos = start
		i++
		for i < len(lines) && lines[i] != "" && !strings.HasPrefix(lines[i], "@@") {
			kind, text := lines[i][0], lines[i][1:]
			if i+1 < len(lines) && lines[i+1] == "\\ No newline at end of file\n" {
				text = strings.TrimSuffix(text, "\n")
				i++
			}
			switch kind {
			case ' ', '-':
				if pos >= len(al) || al[pos] != text {
					return nil, fmt.Errorf("line %d does not match", pos+1)
				}
				if kind == ' ' {
					out = append(out, text)
				}
				pos++
			case '+':
				out = append(out, text)
			default:
				return nil, fmt.Errorf("bad line %q", lines[i])
			}
			i++
		}
	}
	out = append(out, al[pos:]...)
	return []byte(strings.Join(out, "")), nil
}

// rangeFix writes a one-line range "-3" as "-3,1", so one Sscanf format
// reads every header.
func rangeFix(h string) string {
	fields := strings.Fields(h)
	if len(fields) < 4 {
		return h
	}
	for _, k := range []int{1, 2} {
		if !strings.Contains(fields[k], ",") {
			fields[k] += ",1"
		}
	}
	return strings.Join(fields, " ") + "\n"
}

func TestUnifiedDiff(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"x\n", "x\n", ""},
		{"a\nb\nc\n", "a\nB\nc\n", "--- installed\n+++ pushed\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"},
		{"", "new\n", "--- installed\n+++ pushed\n@@ -0,0 +1 @@\n+new\n"},
		{"old\n", "", "--- installed\n+++ pushed\n@@ -1 +0,0 @@\n-old\n"},
		{"a\nb", "a\nb\n", "--- installed\n+++ pushed\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"},
	}
	for _, c := range cases {
		if got := unifiedDiff([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("%q → %q:\n got %q\nwant %q", c.a, c.b, got, c.want)
		}
	}
	// Two changes far apart are two hunks.
	var a, b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&a, "line %d\n", i)
		switch i {
		case 5, 30:
			fmt.Fprintf(&b, "LINE %d\n", i)
		default:
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	d := unifiedDiff([]byte(a.String()), []byte(b.String()))
	if n := strings.Count(d, "\n@@ "); n != 2 {
		t.Errorf("%d hunks:\n%s", n, d)
	}
	roundTrip(t, []byte(a.String()), []byte(b.String()))
}

func roundTrip(t testing.TB, a, b []byte) {
	t.Helper()
	d := unifiedDiff(a, b)
	if (d == "") != bytes.Equal(a, b) {
		t.Fatalf("empty diff iff equal: %q", d)
	}
	if d == "" {
		return
	}
	got, err := applyUnified(a, d)
	if err != nil {
		t.Fatalf("%v\n%s", err, d)
	}
	if !bytes.Equal(got, b) {
		t.Fatalf("round trip:\n got %q\nwant %q\ndiff:\n%s", got, b, d)
	}
}

// Past maxEdits the middle is one hunk, still a diff that applies.
func TestUnifiedDiffFallback(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 2*maxEdits; i++ {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	roundTrip(t, []byte(a.String()), []byte(b.String()))
}

// The diff a record carries is redacted (the shape rules) and cut at
// 64 KiB on a line end with a note.
func TestCaddyfileDiff(t *testing.T) {
	token := "ghp_" + strings.Repeat("aB3dE5fG7h", 4)[:36]
	d := caddyfileDiff([]byte("a\n"), []byte("a\nenv GITHUB_TOKEN "+token+"\n"))
	if strings.Contains(d, token) || !strings.Contains(d, "redacted") {
		t.Errorf("not redacted:\n%s", d)
	}
	var big strings.Builder
	for i := 0; big.Len() < 3*maxDiff; i++ {
		big.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	d = caddyfileDiff(nil, []byte(big.String()))
	if len(d) > maxDiff || !strings.HasSuffix(d, diffCutNote) {
		t.Errorf("%d bytes, ends %q", len(d), d[len(d)-40:])
	}
	if i := strings.Index(d, diffCutNote); !strings.HasSuffix(d[:i+1], "\n") {
		t.Error("not cut at a line end")
	}
}

func FuzzDiff(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), []byte("a\nB\nc\n"))
	f.Add([]byte(""), []byte("x"))
	f.Add([]byte("x\ny"), []byte("x\ny\n"))
	f.Add([]byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n"), []byte("1\n2\nx\n4\n5\n6\n7\ny\n9\n"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		roundTrip(t, a, b)
		if d := caddyfileDiff(a, b); len(d) > maxDiff {
			t.Fatalf("diff of %d bytes", len(d))
		}
	})
}
