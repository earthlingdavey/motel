package liveswap

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// generated is a token an older moteld's entropy layer masked.
const generated = "Xk29fQa7Lm3Zp8Rt5Wn1Yc6Vb4Hd0Jg2" // gitleaks:allow — made up for this test

// fakeBackups is a source that counts its looks.
type fakeBackups struct {
	mu    sync.Mutex
	looks int
	// deadline is whether the last look's context had one.
	deadline bool
}

func (f *fakeBackups) Backups(ctx context.Context) wire.Backups {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looks++
	_, f.deadline = ctx.Deadline()
	at := time.Date(2026, 11, 19, 7, 1, 0, 0, time.UTC)
	return wire.Backups{
		At: at, Healthy: false,
		Apps:    []wire.BackupApp{{Name: "blog", Healthy: true, Result: "ok", LastCompleteBackup: &at, Fresh: true}, {Name: "demo", Result: "failed"}},
		Running: []wire.BackupUnit{}, UnrecognisedUnits: []string{},
		Lines: []wire.BackupLine{
			{Level: "info", Text: "last run 2026-11-19 07:01 UTC"},
			{App: "demo", Level: "problem", Text: "demo: failed: the repository said " + generated},
		},
	}
}

func (f *fakeBackups) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.looks
}

// TestBackupsRoute: GET /_motel/backups is a viewer of the box's alone.
// A viewer of one app, a person with no grant and a deploy token are
// refused before anything is looked at, a key no line lists gets the
// flat 401; the box's viewer gets the source's answer as it gave it
// (motel-backup's words, another program's, which an older moteld
// passed through the heuristic layers), 200 whatever it says, from one
// look every 10 s however many ask; and a server with no source says it
// does not read them.
func TestBackupsRoute(t *testing.T) {
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	h.people = people.reg
	src := &fakeBackups{}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	h.backupsCache = &backupsCache{src: src, now: func() time.Time { return now }}

	const viewerBox = `{"error":"this needs the viewer role on the box","needs":"viewer","scope":"box"}` + "\n"
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}` + "\n"
	for _, c := range []struct {
		name, auth string
		code       int
		body       string
	}{
		{"a viewer of demo", token(people.alice), http.StatusForbidden, viewerBox},
		{"a person with no grant", token(people.dave), http.StatusForbidden, viewerBox},
		{"a key no line lists", token(people.stranger), http.StatusUnauthorized, ""},
		{"a global deploy source", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"an app's deploy source", "Bearer " + appToken(t), http.StatusUnauthorized, ""},
		{"no token", "", http.StatusUnauthorized, ""},
	} {
		var auth []string
		if c.auth != "" {
			auth = []string{c.auth}
		}
		w := sendTo(t, h, "GET", "/_motel/backups", auth...)
		if w.Code != c.code || c.body != "" && w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
	}
	if n := src.count(); n != 0 {
		t.Errorf("the refusals looked at the backups %d times, want none", n)
	}

	carol := token(people.carol)
	w := sendTo(t, h, "GET", "/_motel/backups", carol)
	var got wire.Backups
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("the box's viewer: %d %s", w.Code, w.Body)
	}
	wantNoMotelHeader(t, w)
	if got.Healthy || len(got.Apps) != 2 || got.Apps[0].Name != "blog" || got.Apps[1].Name != "demo" || len(got.Lines) != 2 {
		t.Errorf("the box's viewer: %s", w.Body)
	}
	// No filter ran: the line is the source's.
	if !strings.Contains(got.Lines[1].Text, generated) || strings.Contains(w.Body.String(), "masked") {
		t.Errorf("the line is not the source's: %s", w.Body)
	}
	if !src.deadline {
		t.Error("the look was given no deadline")
	}
	// The same answer for 10 s, to every viewer of the box.
	now = now.Add(backupsEvery - time.Second)
	if w2 := sendTo(t, h, "GET", "/_motel/backups", carol); w2.Body.String() != w.Body.String() || src.count() != 1 {
		t.Errorf("within 10 s: %d looks, body %s", src.count(), w2.Body)
	}
	now = now.Add(time.Second)
	if sendTo(t, h, "GET", "/_motel/backups", carol); src.count() != 2 {
		t.Errorf("at 10 s: %d looks, want 2", src.count())
	}
	// A clock set back is not an answer from the future: looked again.
	now = now.Add(-time.Hour)
	if sendTo(t, h, "GET", "/_motel/backups", carol); src.count() != 3 {
		t.Errorf("after the clock was set back: %d looks, want 3", src.count())
	}
	// A query, a path below it: the 404 of every row under /_motel/ (a
	// person's token is good on a GET alone: a POST is the flat 401).
	for _, c := range []struct{ method, target string }{{"GET", "/_motel/backups?app=blog"}, {"GET", "/_motel/backups?"}, {"GET", "/_motel/backups/blog"}} {
		if w := sendTo(t, h, c.method, c.target, carol); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d %s, want 404", c.method, c.target, w.Code, w.Body)
		}
	}

	// No source: said, as could not tell, and not kept.
	h.backupsCache = &backupsCache{now: func() time.Time { return now }}
	w = sendTo(t, h, "GET", "/_motel/backups", carol)
	got = wire.Backups{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.CouldNotTell != noBackupsSource || got.Healthy ||
		got.Apps == nil || got.Lines == nil || got.Running == nil || got.UnrecognisedUnits == nil {
		t.Errorf("no source: %d %s", w.Code, w.Body)
	}
	h.backupsCache.src = src
	if sendTo(t, h, "GET", "/_motel/backups", carol); src.count() != 4 {
		t.Errorf("a source after none: %d looks, want 4", src.count())
	}
}

// TestOneBoxHandleMaker: a box handle is made in one place, allowBox,
// after Decide allowed the action on the box; no other code of the
// package writes one with anything in it.
func TestOneBoxHandleMaker(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	made := regexp.MustCompile(`boxHandle\{[^}]`)
	var where []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for range made.FindAllIndex(b, -1) {
			where = append(where, f)
		}
	}
	if len(where) != 1 || where[0] != "motel_backups.go" {
		t.Errorf("a box handle is made in %v, want once, in motel_backups.go's allowBox", where)
	}
}
