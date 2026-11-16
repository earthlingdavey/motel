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
	"time"

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

// TestAppOwnNameInClear: an app whose env_file holds its own name
// (DB_NAME=<app>) is named in clear where a body names the app, in
// GET /<app>, a failed deploy's body and the list's row, which is whole
// and not withheld; the name is still replaced in text, and the app's
// other values, one holding the name inside it among them, stay
// redacted everywhere, ?deploy= included.
func TestAppOwnNameInClear(t *testing.T) {
	const app = "billingapp"
	r := newAppsRig(t, app)
	ma := r.apps[app]
	must(t, os.WriteFile(ma.spec.envFile, []byte("DB_NAME="+app+"\nOTHER_SECRET=s3cr3t-other-91ab\nNAMED_TOKEN=tok-"+app+"-7c41e0\n"), 0o600)) // gitleaks:allow — made up for this test
	must(t, ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1", by: "github:example/" + app + "@refs/heads/main"}))
	ma.runner.(*fakeRunner).setStartErr(errors.New("boom s3cr3t-other-91ab tok-" + app + "-7c41e0 in " + app))
	// Its deployer holds a value too, so the list row, which carries no
	// error, carries one.
	failed := ma.Deploy(context.Background(), deployRequest{url: "https://x/2.tgz", version: "v2", by: "local:tok-" + app + "-7c41e0"})
	if failed == nil {
		t.Fatal("the failing deploy succeeded")
	}
	_, body, rd := deployOutcome(ma, failed)
	raw, err := json.Marshal(body)
	must(t, err)
	failure := rd.redactJSON(raw)

	get := func(target string) string {
		t.Helper()
		w := sendTo(t, r.h, http.MethodGet, target, token(r.people.carol))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
		}
		return w.Body.String()
	}
	row := string(r.list(t)[app])
	for what, b := range map[string]string{
		"GET /" + app:  get("/" + app),
		"?deploy=v2":   get("/" + app + "?deploy=v2"),
		"failure body": failure,
		"list row":     row,
	} {
		for _, secret := range []string{"s3cr3t-other-91ab", "tok-" + app + "-7c41e0"} {
			if strings.Contains(b, secret) {
				t.Errorf("%s holds %q: %s", what, secret, b)
			}
		}
		if !strings.Contains(b, "[redacted:NAMED_TOKEN]") {
			t.Errorf("%s: the value holding the name is not replaced whole: %s", what, b)
		}
	}
	for what, b := range map[string]string{"GET /" + app: get("/" + app), "failure body": failure} {
		if !strings.Contains(b, `"app":"`+app+`"`) || !strings.Contains(b, "in [redacted:DB_NAME]") || !strings.Contains(b, "github:example/[redacted:DB_NAME]@") {
			t.Errorf("%s: the app is not named in clear, or its name is not replaced in text: %s", what, b)
		}
	}
	if !strings.HasPrefix(row, `{"name":"`+app+`",`) || !strings.Contains(row, `"summary":{`) || strings.Contains(row, `"error"`) {
		t.Errorf("the list row: %s", row)
	}
}

// TestHoldOwnName: rule 5 leaves the app's name standing in the
// body's own "app" pair alone, never in a string that spells the pair,
// and with a name that is also an outcome word, its outcome pairs too.
func TestHoldOwnName(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"billingapp", `{"app":"billingapp","error":"billingapp: \"app\":\"billingapp\"","path":"/srv/billingapp/run"}`,
			`{"app":"billingapp","error":"[redacted:DB_NAME]: \"app\":\"[redacted:DB_NAME]\"","path":"/srv/[redacted:DB_NAME]/run","redacted_env":["DB_NAME"]}`},
		{"starting", `{"app":"starting","phase":"starting","error":"starting"}`,
			`{"app":"starting","error":"[redacted:DB_NAME]","phase":"starting","redacted_env":["DB_NAME"]}`},
		{"billingapp", `{"app":"billingapp"}`, `{"app":"billingapp"}`}, // only the pair: nothing was replaced
	} {
		rd := NewRedactor([]string{"DB_NAME=" + tc.name}, nil)
		rd.holdOwnName(tc.name)
		if got := rd.redactJSON([]byte(tc.body)); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.body, got, tc.want)
		}
	}
}

// TestNewestDeploySummaryIsTheListsFirst: the one record the summary
// reads is the first of the full list, whatever the records: written
// in turn, two in one tick (by version, "v1" before "v1-x"), the
// newest not a record, a link, a directory, none at all.
func TestNewestDeploySummaryIsTheListsFirst(t *testing.T) {
	rig := newTestRig(t)
	d := rig.spec.dirs
	same := func(what string) {
		t.Helper()
		got, ok := newestDeploySummary(d)
		all := listDeploySummaries(d)
		if ok != (len(all) > 0) || (ok && !sameSummary(got, all[0])) {
			t.Errorf("%s: newest %+v (%v), the list's first of %+v", what, got, ok, all)
		}
	}
	same("no records directory")
	at := time.Unix(1_700_000_000, 0)
	record := func(version, status string, when time.Time) {
		t.Helper()
		must(t, writeDeployRecord(d, version, []byte(`{"version":"`+version+`","status":"`+status+`"}`)))
		must(t, os.Chtimes(deployRecordPath(d.deploys, version), when, when))
	}
	record("v1", "succeeded", at)
	same("one record")
	record("v2", "failed", at.Add(time.Minute))
	same("a newer one")
	record("v1-x", "succeeded", at.Add(2*time.Minute))
	record("v1", "succeeded", at.Add(2*time.Minute))
	same("two in one tick")
	must(t, os.WriteFile(filepath.Join(d.deploys, "v9.json"), []byte(`{"version":"v8","status":"succeeded"}`), 0o600))
	must(t, os.Chtimes(filepath.Join(d.deploys, "v9.json"), at.Add(time.Hour), at.Add(time.Hour)))
	same("the newest not a record")
	must(t, os.Symlink(deployRecordPath(d.deploys, "v1"), filepath.Join(d.deploys, "v7.json")))
	must(t, os.Mkdir(filepath.Join(d.deploys, "v6.json"), 0o700))
	same("a link and a directory")
	if got, _ := newestDeploySummary(d); got.Version != "v1-x" {
		t.Errorf("the newest is %q, want v1-x", got.Version)
	}
}
