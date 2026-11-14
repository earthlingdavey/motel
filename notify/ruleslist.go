package notify

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/backups/status"
	"github.com/smallhoursorg/motel/liveswap"
)

// `motel monitor rules` (DESIGN-metrics.md, "How close each alert
// is"): GET /notify/rules on the admin socket lists every alert motel
// can raise on the loaded config (silenceableAlerts, the one list of
// names), each with what a screen needs to show how close it is to
// firing. A rule the store evaluates comes with its value's MetricsQL,
// which the client runs itself on the store's socket; a check motel's
// own detectors make comes with the number the detector last read. No
// annotation is listed: an operator's text, and what vmalert sends to
// a channel, is not a status screen's.

// RulesListing is `motel monitor rules --json`.
type RulesListing struct {
	// Store is how to read the store-backed rules; absent with no
	// metrics line, and so are those rules.
	Store  *StoreQueries  `json:"store,omitempty"`
	Alerts []AlertListing `json:"alerts"`
}

// StoreQueries are the store's socket and how vmalert's evaluations
// line up with a query a client makes there.
type StoreQueries struct {
	Socket string `json:"socket"`
	// QueryOffsetSeconds is how far behind its run vmalert stamps an
	// evaluation, at most: its interval and its -rule.evalDelay. A value
	// query at now minus it reads no later than what vmalert compared.
	QueryOffsetSeconds int `json:"query_offset_seconds"`
	// StateQuery and ActiveSinceQuery are pending or firing, and since
	// when: vmalert writes both series for an alert that is not
	// inactive, with a gap at each evaluation that the lookback covers.
	StateQuery       string `json:"state_query"`
	ActiveSinceQuery string `json:"active_since_query"`
}

// AlertListing is one alert name.
type AlertListing struct {
	Name string `json:"name"`
	// Source is builtin (a store rule motel ships), operator (a rule
	// line) or motel check (motel's own detectors).
	Source string `json:"source"`
	// Severity is the one it is first raised at.
	Severity string `json:"severity"`
	// Subject is box or app; App the one app a rule line is pinned to.
	Subject string `json:"subject"`
	App     string `json:"app,omitempty"`
	// Kind is threshold, guarded (a threshold with guards) or event
	// (nothing to be close to).
	Kind string `json:"kind"`

	// A store rule's: ValueQuery Op Threshold is its condition, held
	// for ForSeconds.
	ValueQuery string         `json:"value_query,omitempty"`
	Op         string         `json:"op,omitempty"`
	Threshold  *float64       `json:"threshold,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	ForSeconds int            `json:"for_seconds,omitempty"`
	Guards     []GuardListing `json:"guards,omitempty"`

	// A check's: disk_low's two levels, and the detector's last numbers.
	Levels   []LevelListing `json:"levels,omitempty"`
	Readings []Reading      `json:"readings,omitempty"`
}

// GuardListing is a term the rule's condition also needs: Query Op
// Value, or with no Op, Query having a series at all.
type GuardListing struct {
	Query string   `json:"query"`
	Op    string   `json:"op,omitempty"`
	Value *float64 `json:"value,omitempty"`
	// Match is how the guard's series are matched to the value's, such
	// as "on (app)", or "on ()" for a guard with no labels, which holds
	// for every series of the value; none for all their labels.
	Match   string `json:"match,omitempty"`
	Meaning string `json:"meaning"`
}

// LevelListing is one of disk_low's: fired at Threshold, cleared under
// ClearsBelow.
type LevelListing struct {
	Severity    string  `json:"severity"`
	Threshold   float64 `json:"threshold"`
	ClearsBelow float64 `json:"clears_below"`
}

// Reading is a check's number at MeasuredAt: for one filesystem or one
// backup (Instance, the alert's instance) or one app (App).
type Reading struct {
	Instance string  `json:"instance,omitempty"`
	App      string  `json:"app,omitempty"`
	Value    float64 `json:"value"`
	// Threshold and WindowSeconds are the watchdog's, which differ by app.
	Threshold     *float64 `json:"threshold,omitempty"`
	WindowSeconds float64  `json:"window_seconds,omitempty"`
	// Level is disk_low's severity firing now, which its gap decides.
	Level      string    `json:"level,omitempty"`
	MeasuredAt time.Time `json:"measured_at"`
}

const (
	sourceBuiltin  = "builtin"
	sourceOperator = "operator"
	sourceCheck    = "motel check"

	kindThreshold = "threshold"
	kindGuarded   = "guarded"
)

// listUnits are the units a listing names, by the rules' own.
var listUnits = map[string]string{
	unitShare: "share", unitLatency: "seconds", unitRate: "requests/s", unitCores: "cores", unitBytes: "bytes",
}

// polled is what the engine's pollers measured last (engine.poll).
type polled struct {
	disks     []diskReading
	backups   map[string]time.Time // app → its last complete backup
	backupsAt time.Time
}

// chainRead is the pusher's: how long every push has failed as of the
// last one, and the reads in a row that found vmalert not up.
type chainRead struct {
	failingFor time.Duration
	pushedAt   time.Time
	rulesBad   int
	rulesAt    time.Time
}

// read is the pusher's chain for the config's store m: nothing when
// another config's store has the pusher.
func (p *pusher) read(m *MetricsStore) chainRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m == nil || p.active != m {
		return chainRead{}
	}
	c := chainRead{pushedAt: p.pushedAt, rulesBad: p.rulesBad, rulesAt: p.rulesReadAt}
	if !p.failingSince.IsZero() {
		c.failingFor = p.pushedAt.Sub(p.failingSince)
	}
	return c
}

// watchdogBudgets are the config's apps' restart budgets: a var so
// that a test needs no liveswap.
var watchdogBudgets = func(a *App) []liveswap.WatchdogBudget {
	if a.ls == nil {
		return nil
	}
	return a.ls.WatchdogBudgets()
}

// installedVMAlert is where setup writes vmalert's unit: the one that
// runs, which a binary newer than the last setup may differ from.
var installedVMAlert = filepath.Join(unitDir, vmalertUnit)

// vmalertTiming is the -evaluationInterval and -rule.evalDelay of the
// vmalert unit setup wrote (installedVMAlert), or of the one it writes
// (units/metrics/motel-vmalert.service) on a box with none yet.
func vmalertTiming() (interval, delay time.Duration, err error) {
	unit, rerr := os.ReadFile(installedVMAlert) //nolint:gosec // setup's unit path, fixed
	switch {
	case errors.Is(rerr, fs.ErrNotExist):
		unit = unitVMAlert // no setup yet: the unit it would write
	case rerr != nil:
		// Not the embedded unit's timing in its place: the running
		// vmalert may differ, and a plausible offset would hide that.
		return 0, 0, fmt.Errorf("reading vmalert's unit: %w", rerr)
	}
	flag := func(name string) (time.Duration, error) {
		for _, f := range strings.Fields(string(unit)) {
			if v, ok := strings.CutPrefix(f, "-"+name+"="); ok {
				return time.ParseDuration(v)
			}
		}
		return 0, errors.New("motel-vmalert.service has no -" + name)
	}
	if interval, err = flag("evaluationInterval"); err != nil {
		return 0, 0, err
	}
	delay, err = flag("rule.evalDelay")
	return interval, delay, err
}

func storeQueries(m *MetricsStore) (*StoreQueries, error) {
	interval, delay, err := vmalertTiming()
	if err != nil {
		return nil, err
	}
	// Two intervals: one did not do (a 90 s lookback found neither
	// series once a minute, measured on Debian's 1.112.0).
	look := "[" + promDuration(2*interval) + "]"
	return &StoreQueries{
		Socket: m.store(), QueryOffsetSeconds: int((interval + delay).Seconds()),
		StateQuery: "last_over_time(ALERTS" + look + ")", ActiveSinceQuery: "last_over_time(ALERTS_FOR_STATE" + look + ")",
	}, nil
}

// listRules is the listing of the config a, with the detectors'
// numbers as last read.
func listRules(a *App, seen polled, chain chainRead, budgets []liveswap.WatchdogBudget) (RulesListing, error) {
	l := RulesListing{Alerts: []AlertListing{}}
	rules := map[string]vmRule{}
	if a.Metrics != nil {
		q, err := storeQueries(a.Metrics)
		if err != nil {
			return RulesListing{}, err
		}
		l.Store = q
		for _, g := range []vmGroup{fixedRules(), operatorRules(a)} {
			for _, r := range g.Rules {
				rules[r.Alert] = r
			}
		}
	}
	for _, name := range silenceableAlerts(a) {
		e := AlertListing{Name: name, Subject: "box"}
		if r, ok := rules[name]; ok {
			// The rule's subject label, as alertScope reads it, from
			// the render in hand.
			if subject, ok := strings.CutPrefix(r.Labels["subject"], "app:"); ok {
				e.Subject = "app"
				if !strings.Contains(subject, "{{") {
					e.App = subject
				}
			}
			storeRuleListing(&e, r)
		} else if m, ok := raisedBy(name); ok {
			if app, _, _ := alertScope(a, name); app {
				e.Subject = "app"
			}
			e.Source, e.Severity, e.Kind = sourceCheck, m.severity, kindEvent
			if name == eventPostDeployRegressed {
				e.Source = sourceOperator
			}
			if m.measured {
				e.Kind = kindThreshold
				checkListing(&e, seen, chain, budgets)
			}
		} else {
			return RulesListing{}, fmt.Errorf("%s is an alert motel raises that the listing does not know", name)
		}
		l.Alerts = append(l.Alerts, e)
	}
	return l, nil
}

func storeRuleListing(e *AlertListing, r vmRule) {
	e.Source, e.Severity = sourceBuiltin, r.Labels["severity"]
	if r.Labels["source"] == sourceRules {
		e.Source = sourceOperator
	}
	e.Kind = kindEvent
	if r.term == nil {
		return
	}
	t := r.term
	e.Kind, e.ValueQuery, e.Op, e.Threshold, e.Unit = kindThreshold, t.value, t.op, ptr(t.threshold), listUnits[r.unit]
	e.ForSeconds = promSeconds(r.For)
	for _, g := range t.guards {
		gl := GuardListing{Query: g.value, Op: g.op, Match: g.match, Meaning: g.meaning}
		if g.op != "" {
			gl.Value = ptr(g.threshold)
		}
		e.Guards = append(e.Guards, gl)
	}
	if len(e.Guards) > 0 {
		e.Kind = kindGuarded
	}
}

// checkListing is a measured check's condition and numbers.
func checkListing(e *AlertListing, seen polled, chain chainRead, budgets []liveswap.WatchdogBudget) {
	switch e.Name {
	case alertDiskLow:
		e.Op, e.Unit = ">=", "percent"
		e.Levels = []LevelListing{
			{Severity: SeverityWarning, Threshold: diskWarn, ClearsBelow: diskWarn - diskGap},
			{Severity: SeverityCritical, Threshold: diskCrit, ClearsBelow: diskCrit - diskGap},
		}
		for _, d := range seen.disks {
			e.Readings = append(e.Readings, Reading{Instance: d.path, Value: d.usedPct, Level: d.level, MeasuredAt: d.at})
		}
	case "backup_stale":
		e.Op, e.Threshold, e.Unit = ">", ptr(status.StaleAfter.Seconds()), "seconds"
		for _, app := range slices.Sorted(maps.Keys(seen.backups)) {
			age := seen.backupsAt.Sub(seen.backups[app])
			e.Readings = append(e.Readings, Reading{Instance: app, Value: age.Seconds(), MeasuredAt: seen.backupsAt})
		}
	case liveswap.EventWatchdogThrottled:
		e.Op, e.Unit = ">=", "restarts"
		for _, b := range budgets {
			e.Readings = append(e.Readings, Reading{App: b.App, Value: float64(b.Restarts), Threshold: ptr(float64(b.Budget)),
				WindowSeconds: b.Window.Seconds(), MeasuredAt: b.At})
		}
	case eventStoreDown:
		e.Op, e.Threshold, e.Unit = ">=", ptr(storeDownAfter.Seconds()), "seconds"
		if !chain.pushedAt.IsZero() {
			e.Readings = []Reading{{Value: chain.failingFor.Seconds(), MeasuredAt: chain.pushedAt}}
		}
	case eventRulesDown:
		e.Op, e.Threshold, e.Unit = ">=", ptr(float64(rulesDownReads)), "reads"
		if !chain.rulesAt.IsZero() {
			e.Readings = []Reading{{Value: float64(chain.rulesBad), MeasuredAt: chain.rulesAt}}
		}
	}
}

func ptr(v float64) *float64 { return &v }

// promSeconds reads promDuration's text back: a whole number and one
// of d, h, m, s. 0 for none.
func promSeconds(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0
	}
	switch s[len(s)-1] {
	case 'd':
		return n * 86400
	case 'h':
		return n * 3600
	case 'm':
		return n * 60
	case 's':
		return n
	}
	return 0
}

// serveRules is GET /notify/rules: the listing of the config notify's
// Start configured, with what its detectors last read. It reads
// nothing new: the pollers' and the pusher's last numbers, and the
// watchdogs' counts in memory.
func serveRules(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: errors.New("GET only")}
	}
	e := theEngine
	e.mu.Lock()
	active, seen := e.active, e.seen
	e.mu.Unlock()
	if active == nil || active.cfg == nil {
		return caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errors.New("this box's Caddyfile has no monitor block with channels")}
	}
	l, err := listRules(active, seen, thePusher.read(active.Metrics), watchdogBudgets(active))
	if err != nil {
		return err
	}
	return writeJSON(w, l)
}

// rulesText is the listing as a table: one line an alert, its
// condition, and a check's readings.
func rulesText(l RulesListing, now time.Time) []byte {
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSOURCE\tKIND\tSEVERITY\tSCOPE\tCONDITION\tNOW")
	for _, e := range l.Alerts {
		scope := e.Subject
		if e.App != "" {
			scope = "app " + e.App
		} else if e.Subject == "app" {
			scope = "every app"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, e.Source, e.Kind, e.Severity, scope, condition(e), readings(e, now))
	}
	_ = tw.Flush()
	if l.Store != nil {
		_, _ = fmt.Fprintf(&b, "\nNOW for the store's rules: run each value_query (--json) on %s at now - %ds;\npending and firing are %s (firing over pending; a resolved alert shows within the lookback).\n",
			l.Store.Socket, l.Store.QueryOffsetSeconds, l.Store.StateQuery)
	}
	return b.Bytes()
}

// amount is v in unit, as the table writes it.
func amount(v float64, unit string) string {
	switch unit {
	case "seconds":
		if d := time.Duration(v * float64(time.Second)); d >= time.Minute {
			return shortDuration(d)
		}
		return secondsText(v)
	case "percent":
		// Down, so that 79.96 is not read as the 80 it is under.
		return numberText(math.Floor(v*10)/10) + "%"
	case "":
		return numberText(v)
	}
	return numberText(v) + " " + unit
}

func condition(e AlertListing) string {
	var c string
	switch {
	case len(e.Levels) > 0:
		var ls []string
		for _, lv := range e.Levels {
			ls = append(ls, e.Op+" "+amount(lv.Threshold, e.Unit)+" "+lv.Severity)
		}
		c = strings.Join(ls, ", ") + fmt.Sprintf(", each clears %s points under", numberText(e.Levels[0].Threshold-e.Levels[0].ClearsBelow))
	case e.Threshold != nil:
		c = e.Op + " " + amount(*e.Threshold, e.Unit)
	case e.Name == liveswap.EventWatchdogThrottled:
		// Each app's own budget; spent is not firing: it fires when a
		// further restart is needed and the spent budget refuses it
		// (liveswap/watchdog.go), so a healthy instance after the last
		// restart the budget allowed is not throttled.
		c = "a further restart needed while " + e.Op + " its budget in its window is spent"
	default:
		return "-"
	}
	if e.ForSeconds > 0 {
		c += " for " + shortDuration(time.Duration(e.ForSeconds)*time.Second)
	}
	if n := len(e.Guards); n == 1 {
		c += ", 1 guard"
	} else if n > 1 {
		c += fmt.Sprintf(", %d guards", n)
	}
	return c
}

// staleAfter is how old a check's reading may be before the table
// says how old it is: twice its detector's cadence (a statfs that
// hangs, an unreadable record, no push).
var staleAfter = map[string]time.Duration{
	alertDiskLow: 2 * time.Minute, "backup_stale": 2 * backupEvery,
	eventStoreDown: 2 * pushEvery, eventRulesDown: 4 * pushEvery, liveswap.EventWatchdogThrottled: time.Minute,
}

func readings(e AlertListing, now time.Time) string {
	if e.ValueQuery != "" {
		return "(the store)"
	}
	var rs []string
	for _, r := range e.Readings {
		v := amount(r.Value, e.Unit)
		if r.Threshold != nil {
			v = numberText(r.Value) + " of " + numberText(*r.Threshold) + " in " + shortDuration(time.Duration(r.WindowSeconds*float64(time.Second)))
		}
		if who := r.Instance + r.App; who != "" {
			v = who + " " + v
		}
		if r.Level != "" {
			v += " (" + r.Level + ")"
		}
		if old := now.Sub(r.MeasuredAt); old > staleAfter[e.Name] {
			v += " (as of " + shortDuration(old) + " ago)"
		}
		rs = append(rs, v)
	}
	switch {
	case len(rs) > 0:
		return strings.Join(rs, ", ")
	case e.Kind == kindEvent:
		return "-"
	}
	return "nothing read"
}

// shortDuration is d to the millisecond, its zero parts left out:
// 3h59m30s, 10m, 1d2h, 3h0.5s, 500ms. Cut down to the millisecond, never
// rounded up, so that a value is read on its side of a threshold, under
// `>` or `>=` alike: 3h and half a second past `> 3h` is not "3h".
func shortDuration(d time.Duration) string {
	d = d.Truncate(time.Millisecond)
	if d < time.Second {
		return d.String() // 0s, 500ms
	}
	var b strings.Builder
	for _, u := range []struct {
		d time.Duration
		s string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if n := d / u.d; n > 0 {
			b.WriteString(strconv.Itoa(int(n)) + u.s)
			d -= n * u.d
		}
	}
	if d > 0 {
		b.WriteString(strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s")
	}
	return b.String()
}
