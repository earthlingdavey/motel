package notify

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/liveswap"
)

// listingLines are a config with every kind of rule line: each
// threshold metric, every-app and pinned, a floor raised, a severity
// raised, two disk_fill horizons and a post_deploy.
var listingLines = []string{
	"rule errors_high threshold 5xx_share > 0.05 for 10m",
	"rule shop_slow threshold p95 > 1.5s for 15m app shop min_requests 50 severity critical",
	"rule blog_quiet threshold request_rate < 0.1 for 1h app blog",
	"rule hits_low threshold cache_hit_share < 0.5 for 30m",
	"rule mem threshold memory_share > 0.8 for 5m",
	"rule busy threshold cpu_cores > 1.5 for 10m app shop",
	"rule median threshold p50 > 200ms for 10m",
	"rule disk_filling disk_fill within 7d severity critical",
	"rule disk_soon disk_fill within 2h",
	"rule regress post_deploy {\n\t\tmax_5xx_share 0.02\n\t}",
}

var listingAt = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// listingReads are numbers as the detectors would hold them.
func listingReads() (polled, chainRead, []liveswap.WatchdogBudget) {
	seen := polled{
		// /var/lib/caddy's statfs did not come back this poll: its
		// reading is the last one's. 79.96 is under 80, and reads so.
		disks: []diskReading{{path: "/srv/motel", usedPct: 79.96, level: SeverityWarning, at: listingAt.Add(-20 * time.Second)},
			{path: "/var/lib/caddy", usedPct: 41.26, at: listingAt.Add(-80 * time.Second)}},
		backups: map[string]time.Time{"shop": listingAt.Add(-4*time.Hour - 90*time.Second), "blog": listingAt.Add(-time.Hour)},
		// The record read a while ago: its age is as of that read.
		backupsAt: listingAt.Add(-2 * time.Minute),
	}
	chain := chainRead{failingFor: 90 * time.Second, pushedAt: listingAt.Add(-10 * time.Second), rulesBad: 1, rulesAt: listingAt.Add(-40 * time.Second)}
	budgets := []liveswap.WatchdogBudget{{App: "shop", Restarts: 2, Budget: 5, Window: 10 * time.Minute, At: listingAt}}
	return seen, chain, budgets
}

func listingJSON(t *testing.T, l RulesListing) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(l); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestRulesListingGolden pins the listing's JSON, every field, for a
// config with every kind of rule and every check read.
func TestRulesListingGolden(t *testing.T) {
	seen, chain, budgets := listingReads()
	l, err := listRules(rulesApp(t, listingLines...), seen, chain, budgets)
	if err != nil {
		t.Fatal(err)
	}
	got := listingJSON(t, l)
	golden := filepath.Join("testdata", "rules-listing.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the listing differs from %s (run with -update and read the diff):\n%s", golden, got)
	}
}

// TestRulesListingWithoutMetrics: with no metrics line there is no
// store, and none of its rules or its chain: the listing is motel's
// own checks, exactly.
func TestRulesListingWithoutMetrics(t *testing.T) {
	a := withEngine(t).active
	seen, chain, budgets := listingReads()
	l, err := listRules(a, seen, chain, budgets)
	if err != nil {
		t.Fatal(err)
	}
	if l.Store != nil {
		t.Fatalf("a store with no metrics line: %+v", l.Store)
	}
	var names []string
	for _, e := range l.Alerts {
		names = append(names, e.Name)
		if e.Source != sourceCheck || e.ValueQuery != "" {
			t.Errorf("%s: %+v", e.Name, e)
		}
	}
	want := "backup_run_failed backup_stale backup_unreadable cert_failed ChannelFailed deploy_failed deploy_refused deploy_succeeded disk_low drill_failed relaunch_refused rollback watchdog_throttled"
	slices.SortFunc(names, func(x, y string) int { return strings.Compare(strings.ToLower(x), strings.ToLower(y)) })
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("names:\n got %s\nwant %s", got, want)
	}
}

// TestRulesListingNamesAreSilenceable: the listing is silence's closed
// set, name for name, with the metrics line and without.
func TestRulesListingNamesAreSilenceable(t *testing.T) {
	for _, a := range []*App{withEngine(t).active, rulesApp(t, listingLines...)} {
		l, err := listRules(a, polled{}, chainRead{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range l.Alerts {
			names = append(names, e.Name)
		}
		if want := silenceableAlerts(a); !slices.Equal(names, want) {
			t.Fatalf("listed %v\nsilenceable %v", names, want)
		}
	}
}

// joined is a listed rule's condition as a client would put it back
// together from the listing alone.
func joined(e AlertListing) string {
	s := e.ValueQuery + " " + e.Op + " " + numberText(*e.Threshold)
	for _, g := range e.Guards {
		s += " and "
		if g.Match != "" {
			s += g.Match + " "
		}
		s += g.Query
		if g.Op != "" {
			s += " " + g.Op + " " + numberText(*g.Value)
		}
	}
	return s
}

// TestListedRulesAreTheRenderedOnes: each rule rules.yml holds in the
// store's and the operator's groups is listed, and its listed parts,
// joined, are its expr byte for byte, with its for and severity; so
// what a screen shows and what vmalert fires on cannot drift apart.
func TestListedRulesAreTheRenderedOnes(t *testing.T) {
	a := rulesApp(t, listingLines...)
	l, err := listRules(a, polled{}, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]AlertListing{}
	for _, e := range l.Alerts {
		listed[e.Name] = e
	}
	n := 0
	for _, g := range parseRules(t, renderRules(a)).Groups {
		if g.Name == "motel-channel-failed" {
			continue
		}
		for _, r := range g.Rules {
			e, ok := listed[r.Alert]
			if !ok || e.ValueQuery == "" {
				t.Errorf("%s is rendered but not listed with its value: %+v", r.Alert, e)
				continue
			}
			n++
			if got := joined(e); got != r.Expr {
				t.Errorf("%s:\n listed   %s\n rendered %s", r.Alert, got, r.Expr)
			}
			if promSeconds(r.For) != e.ForSeconds || e.ForSeconds == 0 || r.Labels["severity"] != e.Severity {
				t.Errorf("%s: for %s severity %s, listed %d %s", r.Alert, r.For, r.Labels["severity"], e.ForSeconds, e.Severity)
			}
		}
	}
	if n != 11 {
		t.Fatalf("checked %d rules, want the 2 fixed and the 9 lines'", n)
	}
}

// TestRulesListingHoldsNoSecret: no channel URL or secret, and no
// annotation text (an operator's words, which go to the channels), is
// in the listing: only motel's own query text and names.
func TestRulesListingHoldsNoSecret(t *testing.T) {
	a := rulesApp(t, listingLines...)
	seen, chain, budgets := listingReads()
	l, err := listRules(a, seen, chain, budgets)
	if err != nil {
		t.Fatal(err)
	}
	got := string(listingJSON(t, l)) + string(rulesText(l, listingAt))
	for k, v := range testSecrets() {
		if strings.Contains(got, v) || strings.Contains(got, k) {
			t.Errorf("the listing holds %s", k)
		}
	}
	for _, g := range []vmGroup{fixedRules(), operatorRules(a), channelFailedRules(a)} {
		for _, r := range g.Rules {
			for _, v := range r.Annotations {
				if strings.Contains(got, v) {
					t.Errorf("the listing holds %s's annotation %q", r.Alert, v)
				}
			}
		}
	}
	for _, s := range []string{"annotation", "summary", "{{", "http", "hooks"} {
		if strings.Contains(got, s) {
			t.Errorf("the listing holds %q", s)
		}
	}
}

// TestVMAlertTiming: the offset is read from the unit setup writes,
// and every group evaluates at its -evaluationInterval, which the
// offset assumes.
func TestVMAlertTiming(t *testing.T) {
	saved := installedVMAlert
	installedVMAlert = filepath.Join(t.TempDir(), "none")
	t.Cleanup(func() { installedVMAlert = saved })
	interval, delay, err := vmalertTiming()
	if err != nil || interval != time.Minute || delay != 5*time.Second {
		t.Fatalf("%s %s %v", interval, delay, err)
	}
	for _, g := range parseRules(t, renderRules(rulesApp(t, listingLines...))).Groups {
		if g.Interval != promDuration(interval) {
			t.Errorf("group %s evaluates every %s, not vmalert's %s", g.Name, g.Interval, interval)
		}
	}
	q, err := storeQueries(&MetricsStore{})
	if err != nil || q.QueryOffsetSeconds != 65 || q.StateQuery != "last_over_time(ALERTS[2m])" || q.Socket != DefaultStoreSocket {
		t.Fatalf("%+v %v", q, err)
	}
}

// TestVMAlertTimingUnreadable: an installed unit that cannot be read is
// an error, not the embedded unit's timing in its place.
func TestVMAlertTimingUnreadable(t *testing.T) {
	saved := installedVMAlert
	installedVMAlert = t.TempDir() // a directory: read fails, and not as absent
	t.Cleanup(func() { installedVMAlert = saved })
	if _, _, err := vmalertTiming(); err == nil || !strings.Contains(err.Error(), "reading vmalert's unit") {
		t.Fatalf("an unreadable unit: %v", err)
	}
	if _, err := storeQueries(&MetricsStore{}); err == nil {
		t.Fatal("storeQueries hid the unreadable unit")
	}
}

func TestPromSeconds(t *testing.T) {
	for _, d := range []time.Duration{time.Second, 90 * time.Second, 10 * time.Minute, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour} {
		if got := promSeconds(promDuration(d)); got != int(d.Seconds()) {
			t.Errorf("%s: %d", promDuration(d), got)
		}
	}
	if promSeconds("") != 0 || promSeconds("xm") != 0 {
		t.Error("not a duration")
	}
}

// TestListedSeveritiesAreRaised: the severity listed for a detector's
// alert is the one its emitter raises it at first.
func TestListedSeveritiesAreRaised(t *testing.T) {
	te := newTestEngine(t)
	te.deploySucceeded = true
	s := lifecycleSink{e: te.engine}
	for _, m := range lifecycleRaised {
		s.Lifecycle(liveswap.LifecycleEvent{Name: m.name, App: "blog", Release: "v1", Summary: "x"})
	}
	got := map[string]string{}
	for _, a := range te.drain() {
		got[a.Labels["alertname"]] = a.Labels["severity"]
	}
	for _, m := range lifecycleRaised {
		if got[m.name] != m.severity {
			t.Errorf("%s: raised %q, listed %s", m.name, got[m.name], m.severity)
		}
	}
	for _, m := range storeRaised {
		if sev := storeEvent(m.name, true).Severity; sev != m.severity {
			t.Errorf("%s: raised %s, listed %s", m.name, sev, m.severity)
		}
	}
	// The backup record's three that one record raises, and disk_low.
	path := filepath.Join(t.TempDir(), "status.json")
	writeRecord(t, path, &record.Status{Error: "x", Apps: map[string]*record.App{
		"blog": {Class: record.OK, LastOK: &record.Snapshot{ID: "a", Time: listingAt.Add(-4 * time.Hour)}},
	}, LastDrill: &record.Drill{Time: listingAt, Detail: "could not begin"}})
	var b backupPoller
	evs := b.poll(listingAt, path)
	d := diskPoller{stat: func(string) (diskNumbers, error) { return diskNumbers{usedPct: 81, dev: 1}, nil }}
	evs = append(evs, d.poll([]string{"/srv"})...)
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs = append(evs, (&backupPoller{}).poll(listingAt, path)...)
	evs = append(evs, postDeployEvent(t))
	n := 0
	for _, ev := range evs {
		m, ok := raisedBy(ev.Name)
		if !ok || !ev.Firing && ev.Kind == kindState {
			continue
		}
		n++
		if ev.Severity != m.severity {
			t.Errorf("%s: raised %s, listed %s", ev.Name, ev.Severity, m.severity)
		}
	}
	if n != 6 {
		t.Fatalf("checked %d of the record's, the disk's and post_deploy's", n)
	}
}

// postDeployEvent is a regression's event, as the comparator makes it.
func postDeployEvent(t *testing.T) Event {
	t.Helper()
	return regressedEvent(comparison{app: "blog", release: "v2"}, Rule{Name: "regress"}, time.Minute, verdict{}, 1)
}

// TestPollersKeepWhatTheyRead: each poll leaves its numbers for the
// listing, and the engine copies them under its lock.
func TestPollersKeepWhatTheyRead(t *testing.T) {
	te := newTestEngine(t)
	path := filepath.Join(t.TempDir(), "status.json")
	writeRecord(t, path, &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.OK, LastOK: &record.Snapshot{ID: "a", Time: te.at.Add(-time.Hour)}},
		"wiki": {Class: record.Pending},
	}})
	te.backupPath, te.diskPaths = path, []string{"/srv", "/srv/b"}
	te.disk.stat = func(p string) (diskNumbers, error) { return diskNumbers{usedPct: 91, dev: 7}, nil }
	te.poll(t.Context())
	te.mu.Lock()
	seen := te.seen
	te.mu.Unlock()
	if len(seen.disks) != 1 || seen.disks[0] != (diskReading{path: "/srv", usedPct: 91, level: SeverityCritical, at: te.at, dev: 7}) {
		t.Errorf("disks: %+v", seen.disks)
	}
	if len(seen.backups) != 1 || !seen.backups["blog"].Equal(te.at.Add(-time.Hour)) || !seen.backupsAt.Equal(te.at) {
		t.Errorf("backups: %+v at %s", seen.backups, seen.backupsAt)
	}
	// The record taken off: nothing of it is listed any more.
	te.backupPath = ""
	te.advance(time.Minute)
	te.poll(t.Context())
	te.mu.Lock()
	seen = te.seen
	te.mu.Unlock()
	if seen.backups != nil {
		t.Errorf("the record off: %+v", seen.backups)
	}
}

// TestPusherRead: how long the pushes have failed as of the last, and
// the reads without vmalert; nothing for another config's store.
func TestPusherRead(t *testing.T) {
	m := &MetricsStore{}
	p := newPusher(func(Event) {})
	p.active, p.failingSince, p.pushedAt, p.rulesBad, p.rulesReadAt = m, listingAt, listingAt.Add(2*time.Minute), 2, listingAt
	if got := p.read(m); got != (chainRead{failingFor: 2 * time.Minute, pushedAt: listingAt.Add(2 * time.Minute), rulesBad: 2, rulesAt: listingAt}) {
		t.Fatalf("%+v", got)
	}
	if got := p.read(&MetricsStore{}); got != (chainRead{}) {
		t.Fatalf("another store: %+v", got)
	}
}

// TestServeRules: GET only, the listing of the active config, and the
// refusal with no monitor block.
func TestServeRules(t *testing.T) {
	te := withEngine(t)
	saved := watchdogBudgets
	watchdogBudgets = func(*App) []liveswap.WatchdogBudget {
		return []liveswap.WatchdogBudget{{App: "shop", Restarts: 1, Budget: 5, Window: time.Minute, At: listingAt}}
	}
	t.Cleanup(func() { watchdogBudgets = saved })
	te.seen = polled{disks: []diskReading{{path: "/srv", usedPct: 12, at: listingAt}}}
	w, err := callAdmin(t, serveRules, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	var l RulesListing
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(l.Alerts, func(e AlertListing) bool { return e.Name == alertDiskLow })
	j := slices.IndexFunc(l.Alerts, func(e AlertListing) bool { return e.Name == liveswap.EventWatchdogThrottled })
	if l.Store != nil || i < 0 || len(l.Alerts[i].Readings) != 1 || l.Alerts[i].Readings[0].Value != 12 || j < 0 || len(l.Alerts[j].Readings) != 1 {
		t.Fatalf("%s", w.Body.String())
	}
	if _, err := callAdmin(t, serveRules, http.MethodPost, ""); apiStatus(err) != http.StatusMethodNotAllowed {
		t.Errorf("POST: %v", err)
	}
	te.active = nil
	if _, err := callAdmin(t, serveRules, http.MethodGet, ""); apiStatus(err) != http.StatusServiceUnavailable {
		t.Errorf("no monitor block: %v", err)
	}
}

func runRules(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	c := rulesCommand()
	c.SilenceUsage = true
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(append(args, "--address", addr))
	err := c.Execute()
	return out.String(), err
}

// TestRulesCommandOutput pins the table, and --json is the route's
// listing as it is.
func TestRulesCommandOutput(t *testing.T) {
	te := withEngine(t)
	te.active.Metrics = rulesApp(t, listingLines...).Metrics
	saved := watchdogBudgets
	_, _, budgets := listingReads()
	watchdogBudgets = func(*App) []liveswap.WatchdogBudget { return budgets }
	t.Cleanup(func() { watchdogBudgets = saved })
	te.seen, _, _ = listingReads()
	savedNow := rulesNow
	rulesNow = func() time.Time { return listingAt.Add(5 * time.Minute) }
	t.Cleanup(func() { rulesNow = savedNow })
	addr := adminServe(t)
	out, err := runRules(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	want := `NAME                     SOURCE       KIND       SEVERITY  SCOPE      CONDITION                                                            NOW
ChannelFailed            motel check  event      warning   box        -                                                                    -
app_memory_high          builtin      threshold  warning   every app  > 0.9 share for 10m                                                  (the store)
backup_run_failed        motel check  event      warning   box        -                                                                    -
backup_stale             motel check  threshold  critical  box        > 3h                                                                 blog 58m, shop 3h59m30s
backup_unreadable        motel check  event      warning   box        -                                                                    -
blog_quiet               operator     guarded    warning   app blog   < 0.1 requests/s for 1h, 2 guards                                    (the store)
busy                     operator     threshold  warning   app shop   > 1.5 cores for 10m                                                  (the store)
cert_failed              motel check  event      warning   box        -                                                                    -
deploy_failed            motel check  event      warning   every app  -                                                                    -
deploy_refused           motel check  event      warning   every app  -                                                                    -
deploy_succeeded         motel check  event      info      every app  -                                                                    -
disk_filling             operator     guarded    critical  box        < 0 bytes for 3h30m, 2 guards                                        (the store)
disk_low                 motel check  threshold  warning   box        >= 80% warning, >= 90% critical, each clears 5 points under          /srv/motel 79.9% (warning) (as of 5m20s ago), /var/lib/caddy 41.2% (as of 6m20s ago)
disk_soon                operator     guarded    warning   box        < 0 bytes for 30m, 2 guards                                          (the store)
drill_failed             motel check  event      critical  box        -                                                                    -
errors_high              operator     guarded    warning   every app  > 0.05 share for 10m, 2 guards                                       (the store)
hits_low                 operator     guarded    warning   every app  < 0.5 share for 30m, 2 guards                                        (the store)
median                   operator     guarded    warning   every app  > 200ms for 10m, 2 guards                                            (the store)
mem                      operator     threshold  warning   every app  > 0.8 share for 5m                                                   (the store)
metrics_rules_down       motel check  threshold  warning   box        >= 3 reads                                                           nothing read
metrics_store_disk_fill  builtin      guarded    warning   box        < 0 bytes for 30m, 1 guard                                           (the store)
metrics_store_down       motel check  threshold  warning   box        >= 5m                                                                nothing read
metrics_store_read_only  motel check  event      critical  box        -                                                                    -
post_deploy_regressed    operator     event      warning   every app  -                                                                    -
relaunch_refused         motel check  event      critical  every app  -                                                                    -
rollback                 motel check  event      warning   every app  -                                                                    -
shop_slow                operator     guarded    critical  app shop   > 1.5s for 15m, 2 guards                                             (the store)
watchdog_throttled       motel check  threshold  critical  every app  a further restart needed while >= its budget in its window is spent  shop 2 of 5 in 10m (as of 5m ago)

NOW for the store's rules: run each value_query (--json) on /run/motel-metrics/vm.sock at now - 65s;
pending and firing are last_over_time(ALERTS[2m]) (firing over pending; a resolved alert shows within the lookback).
`
	if out != want {
		t.Fatalf("text:\n%s\nwant\n%s", out, want)
	}
	out, err = runRules(t, addr, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var l RulesListing
	if err := json.Unmarshal([]byte(out), &l); err != nil || len(l.Alerts) != 28 || l.Store == nil {
		t.Fatalf("--json: %v %s", err, out)
	}
	te.active = nil
	if out, err := runRules(t, addr, "--json"); err == nil || !strings.Contains(out, `{"error":"this box's Caddyfile has no monitor block with channels"}`) {
		t.Fatalf("--json refused: %v %s", err, out)
	}
}

func TestRulesCommandNotAdmitted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reaches any socket")
	}
	dir, err := os.MkdirTemp("", "rules")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "admin.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, err = runRules(t, "unix/"+path)
	if err == nil || !strings.Contains(err.Error(), "admits root and the motel user: run motel monitor rules as root") {
		t.Fatalf("err = %v", err)
	}
}

// TestDiskReadingOfAPathNotMeasured: a filesystem whose statfs fails
// keeps its last reading, with that reading's own time, as its alert
// keeps firing; a reload that stops measuring a path drops it.
func TestDiskReadingOfAPathNotMeasured(t *testing.T) {
	te := newTestEngine(t)
	fail := false
	te.diskPaths = []string{"/srv", "/var"}
	te.disk.stat = func(p string) (diskNumbers, error) {
		if fail && p == "/srv" {
			return diskNumbers{}, os.ErrPermission
		}
		if p == "/srv" {
			return diskNumbers{usedPct: 92, dev: 1}, nil
		}
		return diskNumbers{usedPct: 10, dev: 2}, nil
	}
	te.poll(t.Context())
	first := te.at
	fail = true
	te.advance(time.Minute)
	te.poll(t.Context())
	want := []diskReading{{path: "/srv", usedPct: 92, level: SeverityCritical, at: first, dev: 1}, {path: "/var", usedPct: 10, at: te.at, dev: 2}}
	if !slices.Equal(te.seen.disks, want) {
		t.Fatalf("after a failed statfs: %+v", te.seen.disks)
	}
	// A path of /srv's filesystem measured now: /srv's carried reading
	// gives way, so the filesystem is listed once.
	te.diskPaths = []string{"/srv", "/srv/b", "/var"}
	te.disk.stat = func(p string) (diskNumbers, error) {
		switch p {
		case "/srv":
			return diskNumbers{}, os.ErrPermission
		case "/srv/b":
			return diskNumbers{usedPct: 93, dev: 1}, nil
		}
		return diskNumbers{usedPct: 10, dev: 2}, nil
	}
	te.advance(time.Minute)
	te.poll(t.Context())
	if len(te.seen.disks) != 2 || te.seen.disks[0].path != "/srv/b" || te.seen.disks[1].path != "/var" {
		t.Fatalf("a filesystem measured through another path: %+v", te.seen.disks)
	}
	want = te.seen.disks[1:]
	a := &App{diskPaths: []string{"/var"}}
	te.setConfig(a, te.p, nil, te.logger)
	if !slices.Equal(te.seen.disks, want) {
		t.Fatalf("after a reload without /srv: %+v", te.seen.disks)
	}
}

// TestShortDurationRoundsDown: to the second, so a value under its
// threshold, or over it, reads on its side.
func TestShortDurationRoundsDown(t *testing.T) {
	for d, want := range map[time.Duration]string{
		4*time.Minute + 31*time.Second: "4m31s", 59600 * time.Millisecond: "59.6s", 3*time.Hour + 59*time.Minute + 59*time.Second: "3h59m59s",
		3*time.Hour + 40*time.Second: "3h40s", 26 * time.Hour: "1d2h", 0: "0s",
		3*time.Hour + 500*time.Millisecond: "3h0.5s", 500 * time.Millisecond: "500ms", 1500 * time.Millisecond: "1.5s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("%s: %s, want %s", d, got, want)
		}
	}
}

// TestRulesReadAtAReadOnlyStore: the read-only answer that starts the
// count again is the count's time.
func TestRulesReadAtAReadOnlyStore(t *testing.T) {
	m := &MetricsStore{}
	st := &fakeStore{ro: true}
	p := newPusher(func(Event) {})
	p.now = func() time.Time { return listingAt }
	p.active, p.store, p.rulesBad, p.rulesReadAt = m, st, 2, listingAt.Add(-5*time.Minute)
	p.checkReadOnly(t.Context())
	if got := p.read(m); got.rulesBad != 0 || !got.rulesAt.Equal(listingAt) {
		t.Fatalf("%+v", got)
	}
}

// TestVMAlertTimingOfTheInstalledUnit: the unit that runs, not the one
// this binary would write, which a later setup installs.
func TestVMAlertTimingOfTheInstalledUnit(t *testing.T) {
	saved := installedVMAlert
	installedVMAlert = filepath.Join(t.TempDir(), "motel-vmalert.service")
	t.Cleanup(func() { installedVMAlert = saved })
	if err := os.WriteFile(installedVMAlert, []byte("ExecStart=/usr/bin/vmalert -evaluationInterval=2m -rule.evalDelay=30s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := storeQueries(&MetricsStore{})
	if err != nil || q.QueryOffsetSeconds != 150 || q.StateQuery != "last_over_time(ALERTS[4m])" {
		t.Fatalf("%+v %v", q, err)
	}
}

// TestEveryMeasuredAlertIsListedAsOne: each alert the table marks
// measured comes with its op, unit and threshold (or levels, or the
// watchdog's per app), so none falls to an empty condition.
func TestEveryMeasuredAlertIsListedAsOne(t *testing.T) {
	seen, chain, budgets := listingReads()
	l, err := listRules(rulesApp(t, listingLines...), seen, chain, budgets)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Alerts {
		m, ok := raisedBy(e.Name)
		if !ok || !m.measured {
			continue
		}
		if e.Kind != kindThreshold || e.Op == "" || e.Unit == "" || e.Threshold == nil && e.Levels == nil && e.Name != liveswap.EventWatchdogThrottled {
			t.Errorf("%s: %+v", e.Name, e)
		}
		if strings.HasPrefix(condition(e), " ") || len(e.Readings) == 0 {
			t.Errorf("%s: condition %q, readings %+v", e.Name, condition(e), e.Readings)
		}
	}
}

// TestBackupReadingsFollowTheRecord: a reload that points at another
// record drops the first one's ages.
func TestBackupReadingsFollowTheRecord(t *testing.T) {
	te := newTestEngine(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "a.json")
	writeRecord(t, first, &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.OK, LastOK: &record.Snapshot{ID: "a", Time: te.at.Add(-time.Hour)}},
	}})
	te.backupPath = first
	te.poll(t.Context())
	if len(te.seen.backups) != 1 {
		t.Fatalf("first record: %+v", te.seen.backups)
	}
	te.setConfig(&App{BackupRecord: filepath.Join(dir, "b.json")}, te.p, nil, te.logger)
	if te.seen.backups != nil {
		t.Fatalf("after the reload: %+v", te.seen.backups)
	}
	te.advance(time.Minute) // inside backupEvery: b.json is not read yet
	te.poll(t.Context())
	if te.seen.backups != nil {
		t.Fatalf("the poll after the reload: %+v", te.seen.backups)
	}
}

// TestTableSaysHowOldAReadingIs: a reading older than twice its
// check's cadence says so; a fresh one does not.
func TestTableSaysHowOldAReadingIs(t *testing.T) {
	e := AlertListing{Name: alertDiskLow, Kind: kindThreshold, Unit: "percent",
		Readings: []Reading{{Instance: "/srv", Value: 50, MeasuredAt: listingAt}}}
	if got := readings(e, listingAt.Add(time.Minute)); got != "/srv 50%" {
		t.Errorf("fresh: %q", got)
	}
	if got := readings(e, listingAt.Add(7*time.Minute)); got != "/srv 50% (as of 7m ago)" {
		t.Errorf("old: %q", got)
	}
}

// TestWatchdogListingSaysWhenItFires: a spent budget is not firing:
// the condition names the further restart it needs, and a healthy
// instance after the last restart the budget allowed reads as close
// (5 of 5), not throttled. A window of 1.5 s or 500 ms is kept whole.
func TestWatchdogListingSaysWhenItFires(t *testing.T) {
	budgets := []liveswap.WatchdogBudget{
		{App: "blog", Restarts: 5, Budget: 5, Window: 1500 * time.Millisecond, At: listingAt},
		{App: "shop", Restarts: 1, Budget: 2, Window: 500 * time.Millisecond, At: listingAt},
	}
	l, err := listRules(withEngine(t).active, polled{}, chainRead{}, budgets)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(l.Alerts, func(e AlertListing) bool { return e.Name == liveswap.EventWatchdogThrottled })
	e := l.Alerts[i]
	if c := condition(e); c != "a further restart needed while >= its budget in its window is spent" {
		t.Errorf("condition: %q", c)
	}
	if e.Readings[0].WindowSeconds != 1.5 || e.Readings[1].WindowSeconds != 0.5 {
		t.Errorf("windows: %+v", e.Readings)
	}
	if got := readings(e, listingAt); got != "blog 5 of 5 in 1.5s, shop 1 of 2 in 500ms" {
		t.Errorf("readings: %q", got)
	}
	b, _ := json.Marshal(e.Readings[1])
	if !strings.Contains(string(b), `"window_seconds":0.5`) {
		t.Errorf("JSON: %s", b)
	}
}

// TestBackupAgePastItsThresholdReadsPastIt: 3 h and half a second
// against `> 3h` is not printed as 3h.
func TestBackupAgePastItsThresholdReadsPastIt(t *testing.T) {
	seen := polled{backups: map[string]time.Time{"blog": listingAt.Add(-3*time.Hour - 500*time.Millisecond)}, backupsAt: listingAt}
	l, err := listRules(withEngine(t).active, seen, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(l.Alerts, func(e AlertListing) bool { return e.Name == "backup_stale" })
	if got := readings(l.Alerts[i], listingAt); got != "blog 3h0.5s" {
		t.Errorf("readings: %q", got)
	}
}
