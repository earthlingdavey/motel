package model_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// shownByIndent is JSONShown as json.Indent makes it, for bodies small
// and shallow enough for that: what the overlay and P showed before
// IndentMax.
func shownByIndent(body []byte) []string {
	var b bytes.Buffer
	if json.Indent(&b, body, "", "  ") != nil {
		b.Reset()
		b.Write(body)
	}
	var out []string
	for _, l := range termtext.Lines(b.String()) {
		out = append(out, l.String())
	}
	return out
}

// TestIndentIsJSONIndent: a body nested no deeper than IndentMax is
// shown on the overlay and printed by P as json.Indent indents it,
// every fixture and the shapes json.Indent treats apart (empty objects
// and arrays, white space between values and after the value, escapes
// and brackets inside strings, a value that is not an object, a body
// that is not JSON).
func TestIndentIsJSONIndent(t *testing.T) {
	bodies := []string{
		`{}`, `[]`, `{ }`, "[ \n ]", `1`, `1 `, "true\n", `"a"`, "\"a\" \n\n", " \t\n{\"a\":1}", "{\"a\":1}\n", "{\"a\":1}\r\n\n ",
		`{"a":[],"b":{},"c":[{}],"d":[[],[1]],"e":null}`,
		`{"s":"a \" ] } , : [ { \\","t":"\\\"","u":"A\n"}`,
		`[1, 2 ,3 , {"x" : [ true,false , null ]}]`,
		`{"a":` + strings.Repeat("[", model.IndentMax-1) + "1,2" + strings.Repeat("]", model.IndentMax-1) + `}`,
		`not json`, `{"a":1`, "{\"a\":1}\n{}", "",
	}
	err := fs.WalkDir(os.DirFS("../demo/fixtures"), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, err := os.ReadFile(filepath.Join("../demo/fixtures", p))
			if err != nil {
				return err
			}
			bodies = append(bodies, string(b))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		want := shownByIndent([]byte(body))
		if got := model.JSONShown([]byte(body)); got != strings.Join(append(append([]string{}, want...), ""), "\n") {
			t.Errorf("P prints %q as\n%q\nwant\n%q", body, got, strings.Join(want, "\n"))
		}
		lines, cut, _ := model.JSONLines([]byte(body))
		var got []string
		for _, l := range lines {
			got = append(got, l.String())
		}
		if cut != 0 || strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("the overlay shows %q as\n%q (%d cut)\nwant\n%q", body, got, cut, want)
		}
	}
}

// TestDeepJSONBounded: a small body nested thousands deep (the review's)
// is indented no deeper than IndentMax, on the overlay and as P prints
// it, in under 50 MB and a second each; P still prints every line, and
// the overlay counts the lines it does not draw.
func TestDeepJSONBounded(t *testing.T) {
	for _, c := range []struct{ depth, elems int }{{2000, 2000}, {9000, 5000}, {9000, 20000}} {
		body := []byte(`{"apps":[],"x":` + strings.Repeat("[", c.depth) + strings.TrimSuffix(strings.Repeat("1,", c.elems), ",") + strings.Repeat("]", c.depth) + `}`)
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("the client's decoder refuses it: %v", err)
		}
		all := 2*c.depth + c.elems + 3
		var lines []termtext.Text
		var cut int
		var shown string
		for _, run := range []struct {
			what string
			f    func()
		}{
			{"the overlay", func() { lines, cut, _ = model.JSONLines(body) }},
			{"P", func() { shown = model.JSONShown(body) }},
		} {
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			at := time.Now()
			run.f()
			took := time.Since(at)
			runtime.ReadMemStats(&m1)
			if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; mb >= 50 || took >= time.Second {
				t.Errorf("%d bytes nested %d deep: %s took %v and %d MB", len(body), c.depth, run.what, took, mb)
			}
		}
		if len(lines) != model.OverlayLines || cut != all-model.OverlayLines {
			t.Errorf("depth %d: the overlay has %d lines and %d more, want %d and %d", c.depth, len(lines), cut, model.OverlayLines, all-model.OverlayLines)
		}
		printed := strings.Split(strings.TrimSuffix(shown, "\n"), "\n")
		if len(printed) != all {
			t.Errorf("depth %d: P prints %d lines, want %d", c.depth, len(printed), all)
		}
		indent := strings.Repeat(" ", 2*model.IndentMax)
		for _, l := range append(printed, textsOf(lines)...) {
			if strings.HasPrefix(l, indent+" ") {
				t.Fatalf("depth %d: a line indented past %d levels: %d spaces", c.depth, model.IndentMax, len(l)-len(strings.TrimLeft(l, " ")))
			}
		}
		if closing := printed[c.depth+c.elems+2]; closing != indent+"]" || printed[12] != indent+"[" {
			t.Errorf("depth %d: the deep lines are not indented as at %d: %q, %q", c.depth, model.IndentMax, printed[12], closing)
		}
	}
}

func textsOf(ts []termtext.Text) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out
}

// TestDeepJSONSaid: the overlay says when a line it draws is indented
// less than its depth, and does not otherwise: the 1 inside the object
// and depth arrays is depth+1 levels deep.
func TestDeepJSONSaid(t *testing.T) {
	for _, depth := range []int{model.IndentMax - 1, model.IndentMax} {
		s, f, now := session(t)
		f.Body("apps.json", `{"apps":[{"name":"blog"}],"x":`+strings.Repeat("[", depth)+"1"+strings.Repeat("]", depth)+`}`)
		for range 3 {
			step(t, s, f, now)
		}
		said := strings.Contains(strings.Join(overlayText(s, model.View{Screen: model.ScreenApps}), " "), "nested deeper than "+strconv.Itoa(model.IndentMax)+" levels")
		if said != (depth+1 > model.IndentMax) {
			t.Errorf("nested %d deep: the overlay says it is indented less: %v", depth, said)
		}
	}
}

// record is the session's record of app's version, read from the fake.
func record(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time, app, version string) model.RecordView {
	t.Helper()
	rec := model.Job{Kind: model.KDeploy, App: app, Version: version}
	s.Engine.Focus(app, *now)
	s.Engine.Once(rec, *now)
	for step(t, s, f, now) != rec {
	}
	return s.RecordView(app, version, *now)
}

// TestEmptyLogLinesKept: an empty line of a log tail is a line, shown
// and counted.
func TestEmptyLogLinesKept(t *testing.T) {
	s, f, now := session(t)
	f.Body("deploys/blog/v2.json", `{"version":"v2","status":"failed","phase":"starting","started_at":"@-20s","finished_at":"@-10s",
		"detail":{"exit":"1","log_tail":["","starting","","",""]}}`)
	v := record(t, s, f, now, "blog", "v2")
	if got := textsOf(v.Tail); strings.Join(got, "|") != "|starting|||" {
		t.Errorf("the tail is %q, want five lines, four empty", got)
	}
	if v.TailTitle.Text() != "log tail: 5 lines" {
		t.Errorf("the tail's title is %q", v.TailTitle.Text())
	}
}

// TestProbeLocationQuoted: the probe's location, the failing app's own
// header, stands inside the client's sentence as a quotation: the
// client's ", body:" after it cannot be one the app wrote.
func TestProbeLocationQuoted(t *testing.T) {
	for _, loc := range []string{`/x, body: fine`, `/x", body: fine`} {
		s, f, now := session(t)
		q, _ := json.Marshal(loc)
		f.Body("deploys/blog/v2.json", `{"version":"v2","status":"failed","phase":"starting","started_at":"@-20s","finished_at":"@-10s",
			"detail":{"exit":"1","probe":{"status":500,"location":`+string(q)+`,"body":"down"}}}`)
		v := record(t, s, f, now, "blog", "v2")
		want := "probe       500 from " + termtext.Quote(loc).String() + ", body:"
		found := false
		for _, l := range v.Head {
			if strings.HasPrefix(l.Text(), "probe ") {
				found = true
				if l.Text() != want {
					t.Errorf("the probe's line is %q, want %q", l.Text(), want)
				}
			}
		}
		if !found {
			t.Errorf("no probe line in %q", textsOfLines(v.Head))
		}
	}
}

func textsOfLines(ls []model.Line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Text()
	}
	return out
}

// TestStripBesideStaleRows: one app stopped, then an hour with no
// answer: the strip's line of that stale row says it is stale and how
// old, and the summary counts the stale rows beside a row that needs a
// look.
func TestStripBesideStaleRows(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "docs"}, {"name": "mailer"}, {"name": "blog"}]}`)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 8 {
		step(t, s, f, &now)
	}
	v := s.AppsView(now, "", 0, 0)
	if len(v.Strip) != 1 || strings.Contains(v.Strip[0].Text(), "stale") || strings.Contains(v.Summary.Text(), "stale") {
		t.Fatalf("answering: strip %q, summary %q", textsOfLines(v.Strip), v.Summary.Text())
	}
	for _, k := range []string{"docs", "mailer", "blog", "apps", "whoami"} {
		f.Set(k, fail(model.FailNoAnswer))
	}
	for now.Before(t0.Add(time.Hour)) {
		step(t, s, f, &now)
	}
	v = s.AppsView(now, "", 0, 0)
	if len(v.Strip) != 1 || !strings.HasPrefix(v.Strip[0].Text(), "✖ mailer       1h old, stale: stopped: ") {
		t.Errorf("the strip of a stale row: %q", textsOfLines(v.Strip))
	}
	if got := v.Summary.Text(); got != "3 apps · 1 need a look · 3 stale · by attention" {
		t.Errorf("the summary: %q", got)
	}
}
