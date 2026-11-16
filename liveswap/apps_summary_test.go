package liveswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// appsRig is a webhook running the named apps, each driven by the fakes
// a test rig uses and with an env_file of one value, none deployed yet.
// The people are newTestPeople's: carol views the box.
type appsRig struct {
	h      *Handler
	apps   map[string]*managedApp
	fetch  map[string]*fakeFetcher
	people *testPeople
}

func newAppsRig(t *testing.T, names ...string) *appsRig {
	t.Helper()
	r := &appsRig{apps: map[string]*managedApp{}, fetch: map[string]*fakeFetcher{}, people: newTestPeople(t)}
	app := &App{managed: map[string]*managedApp{}, observed: new(sync.Map)}
	for _, name := range names {
		spec := testSpec(t)
		spec.name = name
		spec.dirs = newAppDirs(shortTempDir(t), name)
		spec.envFile = filepath.Join(t.TempDir(), name+".env")
		must(t, os.WriteFile(spec.envFile, []byte("SECRET_TOKEN=value-of-"+name+"-7c41e0\n"), 0o600))
		ma := newManagedApp(name)
		ma.spec = spec
		ma.verifiers = deploytrust.Verifiers(spec.trust, nil)
		r.fetch[name] = &fakeFetcher{}
		ma.runner, ma.prober, ma.fetch, ma.clock, ma.store = &fakeRunner{}, &fakeProber{}, r.fetch[name], newFakeClock(), &fakeStore{}
		ma.logger = zap.NewNop()
		ma.wdCtx, ma.wdCancel = context.WithCancel(context.Background())
		t.Cleanup(ma.wdCancel)
		app.managed[name] = ma
		r.apps[name] = ma
	}
	r.h = &Handler{app: app, logger: zap.NewNop(), limiter: deploytrust.NewLimiter(newFakeClock()), people: r.people.reg}
	return r
}

// list is GET /_motel/apps as carol, by name, each entry as sent.
func (r *appsRig) list(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	w := sendTo(t, r.h, http.MethodGet, "/_motel/apps", token(r.people.carol))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /_motel/apps: %d %s", w.Code, w.Body)
	}
	var body struct {
		Apps []json.RawMessage `json:"apps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	rows := map[string]json.RawMessage{}
	for _, row := range body.Apps {
		var e wire.AppEntry
		if err := json.Unmarshal(row, &e); err != nil {
			t.Fatalf("%v: %s", err, row)
		}
		rows[e.Name] = row
	}
	return rows
}

// TestAppsSummaryIsTheStatus: each entry's summary is, field by field,
// what GET /<app> answers at the same moment, for an app that serves,
// one never deployed, one whose newest deploy failed, and one the
// watchdog throttled.
func TestAppsSummaryIsTheStatus(t *testing.T) {
	r := newAppsRig(t, "serving", "fresh", "failing", "throttled")
	for _, name := range []string{"serving", "failing", "throttled"} {
		must(t, r.apps[name].Deploy(context.Background(), deployRequest{url: "https://x/" + name + ".tgz", version: "v1", by: "local:/k.pub"}))
	}
	r.fetch["failing"].err = errors.New("the download failed")
	if err := r.apps["failing"].Deploy(context.Background(), deployRequest{url: "https://x/failing2.tgz", version: "v2"}); err == nil {
		t.Fatal("the failing deploy succeeded")
	}
	wd := &r.apps["throttled"].wd
	wd.mu.Lock()
	wd.state = wdStateThrottled
	wd.mu.Unlock()

	rows := r.list(t)
	if len(rows) != 4 {
		t.Fatalf("rows %v, want four", rows)
	}
	for name, want := range map[string]struct {
		running  bool
		newest   string // the newest deploy's status, "" for none
		watchdog string
	}{
		"serving":   {true, "succeeded", ""},
		"fresh":     {false, "", ""},
		"failing":   {true, "failed", ""},
		"throttled": {true, "succeeded", wdStateThrottled},
	} {
		w := sendTo(t, r.h, http.MethodGet, "/"+name, token(r.people.carol))
		var s wire.Status
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != http.StatusOK {
			t.Fatalf("GET /%s: %d %s", name, w.Code, w.Body)
		}
		var e wire.AppEntry
		must(t, json.Unmarshal(rows[name], &e))
		sum := e.Summary
		if sum == nil || e.Error != "" {
			t.Fatalf("%s: no summary: %s", name, rows[name])
		}
		if sum.Phase != s.Phase || sum.Running != s.Running || sum.CurrentVersion != s.CurrentVersion {
			t.Errorf("%s: summary %s, status %s", name, rows[name], w.Body)
		}
		wdState := ""
		if s.Watchdog != nil {
			wdState = s.Watchdog.State
		}
		if sum.Watchdog != wdState {
			t.Errorf("%s: summary's watchdog %q, status's %q", name, sum.Watchdog, wdState)
		}
		switch {
		case len(s.Deploys) == 0 && sum.NewestDeploy != nil:
			t.Errorf("%s: a newest deploy the status lists none of: %s", name, rows[name])
		case len(s.Deploys) > 0 && (sum.NewestDeploy == nil || !sameSummary(*sum.NewestDeploy, s.Deploys[0])):
			t.Errorf("%s: newest deploy %s, the status's first %+v", name, rows[name], s.Deploys[0])
		}
		// And the cases are the ones named, so the comparison above is not
		// of four alike.
		newest := ""
		if sum.NewestDeploy != nil {
			newest = sum.NewestDeploy.Status
		}
		if sum.Running != want.running || newest != want.newest || sum.Watchdog != want.watchdog {
			t.Errorf("%s: %s, want running %v, newest %q, watchdog %q", name, rows[name], want.running, want.newest, want.watchdog)
		}
	}
}

// sameSummary compares two deploy summaries field by field, the raw
// ones by their bytes.
func sameSummary(a, b wire.DeploySummary) bool {
	return a.Version == b.Version && a.Status == b.Status && a.Phase == b.Phase && a.By == b.By &&
		bytes.Equal(a.SHA256, b.SHA256) && bytes.Equal(a.StartedAt, b.StartedAt) && bytes.Equal(a.FinishedAt, b.FinishedAt)
}

// TestAppsRowWithheld: an app whose filter withholds (its env_file
// unreadable) is a row of its name and the filter's sentence; the list
// is a 200 and the other app's row is whole.
func TestAppsRowWithheld(t *testing.T) {
	r := newAppsRig(t, "whole", "withheld")
	for name, ma := range r.apps {
		must(t, ma.Deploy(context.Background(), deployRequest{url: "https://x/" + name + ".tgz", version: "v1"}))
	}
	r.apps["withheld"].spec.envFile = filepath.Join(t.TempDir(), "missing.env")
	rows := r.list(t)
	var whole, withheld wire.AppEntry
	must(t, json.Unmarshal(rows["whole"], &whole))
	must(t, json.Unmarshal(rows["withheld"], &withheld))
	if whole.Summary == nil || whole.Error != "" || whole.Summary.CurrentVersion != "v1" {
		t.Errorf("the other app's row: %s", rows["whole"])
	}
	if withheld.Summary != nil || !strings.HasPrefix(withheld.Error, "response withheld: the app's env_file could not be read") {
		t.Errorf("the withheld row: %s", rows["withheld"])
	}
	if strings.Contains(string(rows["withheld"]), "v1") {
		t.Errorf("the withheld row holds its status: %s", rows["withheld"])
	}
}

// TestAppsRowAsksDecide: each row's summary is given only when Decide
// allows the app's status, as GET /<app> is; a row it refuses is the
// name alone, and the list is still the 200 box.apps allowed.
func TestAppsRowAsksDecide(t *testing.T) {
	r := newAppsRig(t, "one", "two")
	saved := decide
	t.Cleanup(func() { decide = saved })
	var asked []string
	decide = func(p access.Principal, a access.Action, resource string) error {
		if a == access.AppStatus {
			asked = append(asked, resource)
			if resource == "two" {
				return errors.New("refused")
			}
		}
		return saved(p, a, resource)
	}
	rows := r.list(t)
	if strings.Join(asked, ",") != "one,two" {
		t.Errorf("Decide was asked about app.status on %v, want one,two", asked)
	}
	if got := string(rows["two"]); got != `{"name":"two","error":"forbidden"}` {
		t.Errorf("the refused row: %s", got)
	}
	if !strings.Contains(string(rows["one"]), `"summary":`) {
		t.Errorf("the allowed row: %s", rows["one"])
	}
}
