package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// apiAfter is the api fixture after its deploy in flight ended: no
// deploying, the phase idle, and edit's change to what it holds.
func apiAfter(t *testing.T, edit func(st map[string]any)) string {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal([]byte(apiStatus(t)), &st); err != nil {
		t.Fatal(err)
	}
	delete(st, "deploying")
	st["phase"] = "idle"
	edit(st)
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// endedIn is api's last deploy, and its newest record, as the deploy in
// flight's outcome: status, the phase it failed in, and the phases.
func endedIn(st map[string]any, status, phase string, phases []any) {
	endedAs(st, "4c2d9e7b1a05", status, phase, phases)
}

// flownThen is the phases the fixture's deploy in flight had entered,
// as the box times them, and then names, each of 1.5 s: the phases of
// its outcome.
func flownThen(t *testing.T, names ...string) []any {
	t.Helper()
	var st struct {
		Deploying struct{ Phases []any }
	}
	if err := json.Unmarshal([]byte(apiStatus(t)), &st); err != nil {
		t.Fatal(err)
	}
	return append(st.Deploying.Phases, phasesTo(names...)...)
}

func endedAs(st map[string]any, version, status, phase string, phases []any) {
	d := map[string]any{"version": version, "status": status, "deployed_by": "github:example/api@refs/heads/main",
		"started_at": "@-20s", "finished_at": "@-1s", "phases": phases}
	rec := map[string]any{"version": version, "status": status, "started_at": "@-20s", "finished_at": "@-1s"}
	if phase != "" {
		d["phase"], rec["phase"] = phase, phase
	}
	st["last_deploy"] = d
	st["deploys"] = append([]any{rec}, st["deploys"].([]any)...)
}

func phasesTo(names ...string) []any {
	var out []any
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "started_at": "@-10s", "seconds": 1.5})
	}
	return out
}

func rowOf(h *harness, label string) string {
	for _, r := range screenRows(h) {
		if strings.HasPrefix(r.text, label+" ") {
			return r.text
		}
	}
	return ""
}

// TestFollowADeploy (#580): on an app's screen a deploy in flight lists
// its phases so far, with the box's seconds; when a read no longer shows
// it, the screen says how it ended, beside what serves, in its place,
// and the last four phases it passed through; d opens the newest
// record, the failure's.
func TestFollowADeploy(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(st map[string]any)
		ended  string
		phases string
		newest string // the record d opens: the newest, whichever it is
	}{
		{"failed", func(st map[string]any) {
			endedIn(st, "failed", "soaking", flownThen(t))
		}, "ended       4c2d9e7b1a05 failed in soaking · 3b8812aa0c4f serves · d opens its record",
			"phases      … extracting 0.7s › preparing 3.9s › starting 1.0s › soaking 18.0s ✖", "4c2d9e7b1a05"},
		{"succeeded", func(st map[string]any) {
			endedIn(st, "succeeded", "", flownThen(t, "promoting", "draining", "stopping_old"))
			st["current_version"] = "4c2d9e7b1a05"
		}, "ended       4c2d9e7b1a05 succeeded · it serves",
			"phases      … soaking 18.0s › promoting 1.5s › draining 1.5s › stopping_old 1.5s", "4c2d9e7b1a05"},
		// A push whose upload was never staged: the box keeps no outcome.
		{"no outcome", func(map[string]any) {}, "ended       4c2d9e7b1a05 ended with no outcome from the box · 3b8812aa0c4f serves", "", "3b8812aa0c4f"},
		// A later deploy of the same version, begun and ended before the
		// read: its phases were not begun when the one seen was in them.
		// A relaunch of the running version that failed: the old
		// instance serves, not the deploy.
		{"a relaunch that failed", func(st map[string]any) {
			endedIn(st, "failed", "soaking", flownThen(t))
			st["current_version"] = "4c2d9e7b1a05"
		}, "ended       4c2d9e7b1a05 failed in soaking · 4c2d9e7b1a05 serves · d opens its record",
			"phases      … extracting 0.7s › preparing 3.9s › starting 1.0s › soaking 18.0s ✖", "4c2d9e7b1a05"},
		{"a later deploy's outcome", func(st map[string]any) {
			endedIn(st, "succeeded", "", phasesTo("extracting", "soaking"))
		}, "ended       4c2d9e7b1a05 ended with no outcome from the box · 3b8812aa0c4f serves", "", "4c2d9e7b1a05"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 100, 30, nil)
			h.open(app("api"))
			if got, want := rowOf(h, "phases"), "phases      … extracting 0.7s › preparing 3.9s › starting 1.0s › soaking 18.0s"; got != want {
				t.Fatalf("in flight, the phases are drawn\n%q, want\n%q", got, want)
			}
			if got := rowOf(h, "ended"); got != "" {
				t.Fatalf("in flight, an ended line: %q", got)
			}
			h.fake.Body("status/api.json", apiAfter(t, c.edit))
			h.press(tcell.KeyCtrlR)
			if got := rowOf(h, "deploying"); got != "" {
				t.Errorf("ended, the deploying line stays: %q", got)
			}
			if got := rowOf(h, "ended"); got != c.ended {
				t.Errorf("the end is drawn\n%q, want\n%q", got, c.ended)
			}
			if got := rowOf(h, "phases"); got != c.phases {
				t.Errorf("ended, the phases are drawn\n%q, want\n%q", got, c.phases)
			}
			h.press("d")
			top := h.top()
			want := record("api", c.newest)
			if top != want {
				t.Errorf("d opened %+v, want %+v", top, want)
			}
		})
	}
}

// TestFollowAnotherDeploy: two deploys of one version, one ended and
// the next begun between two reads, are told apart by when each was
// taken, and the first said to have ended beside the second; a deploy
// seen to begin after one ended drops that one's end.
func TestFollowAnotherDeploy(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("api"))
	// The open app's next read, on its turn.
	next := func() { h.now = h.now.Add(model.FocusEvery); h.pump() }
	again := strings.Replace(apiStatus(t), `"started_at": "@-25s",
  "phases"`, `"started_at": "@-2s",
  "phases"`, 1)
	if again == apiStatus(t) {
		t.Fatal("the fixture's deploy in flight is not where the test looks")
	}
	h.fake.Body("status/api.json", again)
	next()
	if got, want := rowOf(h, "ended"), "ended       4c2d9e7b1a05 ended with no outcome from the box · 3b8812aa0c4f serves"; got != want {
		t.Errorf("the first deploy's end beside the second is drawn\n%q, want\n%q", got, want)
	}
	if rowOf(h, "deploying") == "" {
		t.Error("no deploying line for the second")
	}
	h.fake.Body("status/api.json", apiAfter(t, func(st map[string]any) {
		endedIn(st, "failed", "soaking", phasesTo("soaking"))
	}))
	next()
	if rowOf(h, "ended") == "" {
		t.Fatal("no ended line for the second")
	}
	h.fake.Body("status/api.json", apiStatus(t))
	next()
	if got := rowOf(h, "ended"); got != "" {
		t.Errorf("a deploy seen to begin keeps the last one's end: %q", got)
	}
}

// TestFollowAfterARefusal: a 403 drops the deploy seen in flight with
// the status, so a status answered after it says no end of a deploy it
// did not see end.
func TestFollowAfterARefusal(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("api"))
	h.fake.Set("api", &model.Failure{Kind: model.FailForbidden, Message: "no"})
	h.press(tcell.KeyCtrlR)
	h.fake.Set("api", nil)
	h.fake.Body("status/api.json", apiAfter(t, func(st map[string]any) {
		endedIn(st, "failed", "soaking", phasesTo("soaking"))
	}))
	h.now = h.now.Add(model.RecordAgainAfter)
	h.press(tcell.KeyCtrlR)
	if rowOf(h, "deployed") == "" {
		t.Fatal("the status after the refusal was not read")
	}
	if got := rowOf(h, "ended"); got != "" {
		t.Errorf("after a refusal, an end of a deploy not seen to end: %q", got)
	}
}

// TestFollowEndGoesStale: the end of a deploy seen in flight goes when
// the box's last deploy is another's, one begun and ended between two
// reads and never seen in flight; the deployed line says that one.
func TestFollowEndGoesStale(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("api"))
	h.fake.Body("status/api.json", apiAfter(t, func(st map[string]any) {
		endedIn(st, "failed", "soaking", flownThen(t))
	}))
	h.press(tcell.KeyCtrlR)
	if rowOf(h, "ended") == "" {
		t.Fatal("no ended line")
	}
	// The same answer again keeps it.
	h.now = h.now.Add(model.FocusEvery)
	h.pump()
	if rowOf(h, "ended") == "" {
		t.Fatal("the ended line went with nothing new")
	}
	h.fake.Body("status/api.json", apiAfter(t, func(st map[string]any) {
		endedAs(st, "5e6f7a8b9c0d", "succeeded", "", phasesTo("soaking"))
		st["current_version"] = "5e6f7a8b9c0d"
	}))
	h.now = h.now.Add(model.FocusEvery)
	h.pump()
	if got := rowOf(h, "ended"); got != "" {
		t.Errorf("another deploy's outcome keeps the end of the one before it: %q", got)
	}
}

// TestFollowFirstDeployWithNoOutcome: a new app's first deploy that
// ends with no outcome (a push whose upload was never staged) is said
// to have ended, not lost under "its first deploy comes from CI".
func TestFollowFirstDeployWithNoOutcome(t *testing.T) {
	h := start(t, 100, 30, nil)
	first := func(st map[string]any) {
		for _, k := range []string{"current_version", "pid", "unit", "socket", "command", "last_deploy", "deploys", "watchdog"} {
			delete(st, k)
		}
		st["running"], st["available_versions"] = false, []any{}
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(apiStatus(t)), &st); err != nil {
		t.Fatal(err)
	}
	first(st)
	b, _ := json.Marshal(st)
	h.fake.Body("status/api.json", string(b))
	h.open(app("api"))
	h.fake.Body("status/api.json", apiAfter(t, first))
	h.press(tcell.KeyCtrlR)
	if got, want := rowOf(h, "ended"), "ended       4c2d9e7b1a05 ended with no outcome from the box · nothing serves"; got != want {
		t.Errorf("the end is drawn\n%q, want\n%q", got, want)
	}
}

// TestDemoDeployStaysOne: the fake box's times are its first read's, as
// a box's stay put, so the same deploy in flight read again a read
// later is the same deploy: no ended line beside it (Copilot on #600:
// --demo's fixtures moved with the wall clock at every read).
func TestDemoDeployStaysOne(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("api"))
	for range 3 {
		h.now = h.now.Add(model.FocusEvery)
		h.pump()
	}
	if got := rowOf(h, "ended"); got != "" {
		t.Errorf("the deploy in flight, read again, is said to have ended: %q", got)
	}
	if got, want := rowOf(h, "deploying"), "deploying   4c2d9e7b1a05 soaking · started 40s ago"; !strings.HasPrefix(got, want) {
		t.Errorf("the deploy in flight is drawn %q, want it to begin %q (its age grows)", got, want)
	}
}
