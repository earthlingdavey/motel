package notify

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// rulesStore is a store that answers the rules' queries: each value
// query by its rule's name, and the state and active-since queries; it
// counts what it was asked, at which times, and can wait or fail.
type rulesStore struct {
	mu      sync.Mutex
	byQuery map[string]string // a value query → its rule's name
	values  map[string][]series
	states  []series
	since   []series
	q       *StoreQueries
	err     error
	wait    chan struct{} // closed to let a query answer; nil answers at once
	asked   []string
	times   []time.Time
	running int
	most    int
}

func newRulesStore(l RulesListing, values map[string][]series, states, since []series) *rulesStore {
	f := &rulesStore{byQuery: map[string]string{}, values: values, states: states, since: since, q: l.Store}
	for _, a := range valueAsks(l) {
		// Two rules with one query have one answer: the one the test
		// gave numbers.
		if _, ok := values[a.name]; ok || f.byQuery[a.query] == "" {
			f.byQuery[a.query] = a.name
		}
	}
	return f
}

func (f *rulesStore) instantAt(ctx context.Context, q string, t time.Time) ([]series, error) {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	f.times = append(f.times, t)
	f.running++
	f.most = max(f.most, f.running)
	wait, err := f.wait, f.err
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	switch q {
	case f.q.StateQuery:
		return f.states, nil
	case f.q.ActiveSinceQuery:
		return f.since, nil
	}
	name, ok := f.byQuery[q]
	if !ok {
		return nil, errors.New("a query no rule makes: " + q)
	}
	return f.values[name], nil
}

func (f *rulesStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func one(v float64, labels ...string) series {
	s := series{labels: map[string]string{}, points: []point{{t: listingAt, v: v}}}
	for i := 0; i+1 < len(labels); i += 2 {
		s.labels[labels[i]] = labels[i+1]
	}
	return s
}

// alertSeries is an ALERTS (state "" for ALERTS_FOR_STATE) series of
// vmalert's, with the rule's labels beside the subject's.
func alertSeries(name, state string, v float64, labels ...string) series {
	s := one(v, labels...)
	s.labels["alertname"], s.labels["severity"], s.labels["source"] = name, "warning", "rules"
	if state != "" {
		s.labels["alertstate"] = state
	}
	return s
}

// listingStore is the store the golden listing's numbers come from:
// shop's memory over the cap and firing, blog's under it; shop's errors
// over and pending; the store's disk with no label; a filesystem; and
// shop_slow firing with no value at the time read.
func listingStore(l RulesListing) *rulesStore {
	at := float64(listingAt.Unix())
	return newRulesStore(l, map[string][]series{
		ruleAppMemoryHigh:       {one(0.93104, "app", "shop"), one(0.55, "app", "blog")},
		"errors_high":           {one(0.0612345678, "app", "shop"), one(0, "app", "blog")},
		"disk_filling":          {one(-1234567890.5, "path", "/srv/motel")},
		ruleStoreDiskFill:       {one(53687091200)},
		"hits_low":              {},
		"metrics_store_unknown": {},
	}, []series{
		alertSeries(ruleAppMemoryHigh, wire.RuleFiring, 1, "app", "shop"),
		alertSeries(ruleAppMemoryHigh, wire.RulePending, 1, "app", "shop"),
		alertSeries("errors_high", wire.RulePending, 1, "app", "shop"),
		alertSeries("shop_slow", wire.RuleFiring, 1, "app", "shop"),
		// Another alert's, which no store rule of the listing is.
		alertSeries("ChannelFailed", wire.RuleFiring, 1),
	}, []series{
		alertSeries(ruleAppMemoryHigh, "", at-720, "app", "shop"),
		alertSeries("errors_high", "", at-240, "app", "shop"),
		alertSeries("shop_slow", "", at-1800, "app", "shop"),
	})
}

// TestEvaluate: each store rule's readings are one per series of its
// value, rounded, at now less the query offset; vmalert's state and
// since when for that subject (firing over pending), inactive with
// none; a subject pending or firing with no value at that time keeps
// its state with no number; a rule with no series has none; the state
// queries are asked at the store's present.
func TestEvaluate(t *testing.T) {
	l, err := listRules(rulesApp(t, listingLines...), polled{}, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := listingStore(l)
	now := listingAt.Add(65*time.Second + 400*time.Millisecond)
	got, err := evaluate(context.Background(), f, l.Store, valueAsks(l), now)
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[string]bool{}
	for _, a := range valueAsks(l) {
		distinct[a.query] = true
	}
	if n := len(distinct); f.count() != n+2 || n != len(valueAsks(l))-1 {
		t.Errorf("asked %d queries, want the %d values (mem's and app_memory_high's one) and the two state queries", f.count(), n)
	}
	for i, q := range f.asked {
		want := listingAt
		if q == l.Store.StateQuery || q == l.Store.ActiveSinceQuery {
			want = time.Time{}
		}
		if !f.times[i].Equal(want) {
			t.Errorf("%s asked at %v, want %v", q, f.times[i], want)
		}
	}
	text := func(r Reading) string {
		s := r.App + r.Instance + " " + r.State + " " + r.MeasuredAt.Format(time.RFC3339)
		if r.Value != nil {
			s += " " + strconv.FormatFloat(*r.Value, 'g', -1, 64)
		}
		if r.ActiveSince != nil {
			s += " since " + r.ActiveSince.Format(time.RFC3339)
		}
		return s
	}
	for name, want := range map[string][]string{
		ruleAppMemoryHigh: {"blog inactive 2026-11-13T12:00:00Z 0.55", "shop firing 2026-11-13T12:00:00Z 0.93104 since 2026-11-13T11:48:00Z"},
		"errors_high":     {"blog inactive 2026-11-13T12:00:00Z 0", "shop pending 2026-11-13T12:00:00Z 0.0612346 since 2026-11-13T11:56:00Z"},
		"disk_filling":    {"/srv/motel inactive 2026-11-13T12:00:00Z -1.23457e+09"},
		ruleStoreDiskFill: {" inactive 2026-11-13T12:00:00Z 5.36871e+10"},
		"shop_slow":       {"shop firing 2026-11-13T12:00:00Z since 2026-11-13T11:30:00Z"},
		"hits_low":        nil,
		"busy":            nil,
		// mem's query is app_memory_high's: its numbers, not its state.
		"mem": {"blog inactive 2026-11-13T12:00:00Z 0.55", "shop inactive 2026-11-13T12:00:00Z 0.93104"},
	} {
		var have []string
		for _, r := range got[name] {
			have = append(have, text(r))
		}
		if strings.Join(have, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, have, want)
		}
	}
	if _, ok := got["ChannelFailed"]; ok {
		t.Errorf("a state of an alert no store rule is was read as one: %+v", got["ChannelFailed"])
	}
}

// TestEvaluateFailsWhole: one query that fails is the whole
// evaluation's failure, and a store that does not answer within the
// budget ends it then; at most rulesAtOnce queries run at once.
func TestEvaluateFailsWhole(t *testing.T) {
	l, err := listRules(rulesApp(t, listingLines...), polled{}, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := listingStore(l)
	f.err = errors.New("store down")
	if got, err := evaluate(context.Background(), f, l.Store, valueAsks(l), listingAt); err == nil || got != nil {
		t.Errorf("a failing store: %v %v", got, err)
	}
	f = listingStore(l)
	delete(f.byQuery, valueAsks(l)[0].query)
	if _, err := evaluate(context.Background(), f, l.Store, valueAsks(l), listingAt); err == nil {
		t.Error("one value query failing did not fail the evaluation")
	}
	f = listingStore(l)
	f.wait = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := evaluate(ctx, f, l.Store, valueAsks(l), listingAt); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("a store that hangs: %v after %v", err, time.Since(start))
	}
	if f.most > rulesAtOnce || f.most == 0 {
		t.Errorf("%d queries ran at once, want at most %d", f.most, rulesAtOnce)
	}
}

// TestRulesViewShared: every reader within rulesEvery shares one
// evaluation, one in flight included; a failure is kept as long; an
// evaluation older than that, or of other queries, is made again; a
// reader that gives up is told the store did not answer, and the
// evaluation it left still fills the entry.
func TestRulesViewShared(t *testing.T) {
	l, err := listRules(rulesApp(t, listingLines...), polled{}, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	asks := valueAsks(l)
	per := queriesOf(asks)
	now := listingAt
	v := &rulesView{now: func() time.Time { return now }}
	f := listingStore(l)
	f.wait = make(chan struct{})
	var wg sync.WaitGroup
	oks := make([]bool, 8)
	for i := range oks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, oks[i] = v.get(context.Background(), zap.NewNop(), f, l.Store, asks)
		}()
	}
	// The one evaluation's queries are under way, and every reader waits.
	deadline := time.Now().Add(5 * time.Second)
	for f.count() < rulesAtOnce && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := v.get(gone, zap.NewNop(), f, l.Store, asks); ok {
		t.Error("a reader that gave up was told the store answered")
	}
	close(f.wait)
	wg.Wait()
	for i, ok := range oks {
		if !ok {
			t.Errorf("reader %d: not answered", i)
		}
	}
	if n := f.count(); n != per {
		t.Fatalf("eight readers made %d queries, want one evaluation's %d", n, per)
	}
	now = now.Add(rulesEvery - time.Second)
	if got, ok := v.get(context.Background(), zap.NewNop(), f, l.Store, asks); !ok || len(got[ruleAppMemoryHigh]) != 2 || f.count() != per {
		t.Errorf("within rulesEvery: %v, %d queries", ok, f.count())
	}
	now = now.Add(time.Second)
	f.err = errors.New("store down")
	if _, ok := v.get(context.Background(), zap.NewNop(), f, l.Store, asks); ok || f.count() != 2*per {
		t.Errorf("after rulesEvery: %v, %d queries", ok, f.count())
	}
	f.err = nil
	now = now.Add(rulesEvery - time.Second)
	if _, ok := v.get(context.Background(), zap.NewNop(), f, l.Store, asks); ok || f.count() != 2*per {
		t.Errorf("a failure is kept for rulesEvery: %v, %d queries", ok, f.count())
	}
	// Other queries (another config's rules) are another evaluation,
	// and each set keeps its own: two configs read in turn evaluate once
	// each per rulesEvery, not at every read.
	if _, ok := v.get(context.Background(), zap.NewNop(), f, l.Store, asks[1:]); !ok || f.count() != 2*per+queriesOf(asks[1:]) {
		t.Errorf("other queries: %v, %d queries", ok, f.count())
	}
	now = now.Add(time.Second)
	for range 3 {
		v.get(context.Background(), zap.NewNop(), f, l.Store, asks)
		v.get(context.Background(), zap.NewNop(), f, l.Store, asks[1:])
	}
	if f.count() != 3*per+queriesOf(asks[1:]) {
		t.Errorf("two sets read in turn: %d queries, want one more evaluation of the first", f.count())
	}
}

// TestRulesViewBudget: an evaluation that does not answer within
// rulesBudget is the store unanswered, for a reader that would have
// waited longer.
func TestRulesViewBudget(t *testing.T) {
	l, err := listRules(rulesApp(t, listingLines...), polled{}, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := listingStore(l)
	f.wait = make(chan struct{})
	defer close(f.wait)
	v := &rulesView{now: time.Now}
	start := time.Now()
	_, ok := v.get(context.Background(), zap.NewNop(), f, l.Store, valueAsks(l))
	if took := time.Since(start); ok || took < rulesBudget || took > rulesBudget+3*time.Second {
		t.Errorf("a store that hangs: answered %v after %v, want unanswered after %v", ok, took, rulesBudget)
	}
}

// TestAttachValues: the store rules take their readings, the checks keep
// theirs, and the store's word says whether it answered.
func TestAttachValues(t *testing.T) {
	seen, chain, budgets := listingReads()
	l, err := listRules(rulesApp(t, listingLines...), seen, chain, budgets)
	if err != nil {
		t.Fatal(err)
	}
	unanswered := l
	unanswered.Store = new(StoreQueries)
	*unanswered.Store = *l.Store
	unanswered.Alerts = append([]AlertListing(nil), l.Alerts...)
	attachValues(&unanswered, map[string][]Reading{ruleAppMemoryHigh: {{App: "x"}}}, false)
	for _, e := range unanswered.Alerts {
		if e.ValueQuery != "" && e.Readings != nil {
			t.Errorf("%s: readings of an evaluation that did not answer", e.Name)
		}
	}
	if unanswered.Store.Values != wire.StoreUnanswered {
		t.Errorf("values %q", unanswered.Store.Values)
	}
	attachValues(&l, map[string][]Reading{ruleAppMemoryHigh: {{App: "blog"}}, "backup_stale": {{App: "x"}}}, true)
	for _, e := range l.Alerts {
		switch {
		case e.Name == ruleAppMemoryHigh && (len(e.Readings) != 1 || e.Readings[0].App != "blog"):
			t.Errorf("%s: %+v", e.Name, e.Readings)
		case e.Name == "backup_stale" && (len(e.Readings) != 2 || e.Readings[0].Instance != "blog"):
			t.Errorf("a check's readings were replaced: %+v", e.Readings)
		}
	}
	if l.Store.Values != wire.StoreOn {
		t.Errorf("values %q", l.Store.Values)
	}
	none := RulesListing{Alerts: []AlertListing{}}
	attachValues(&none, nil, true)
	if none.Store != nil {
		t.Error("a listing with no store was given one")
	}
}

// TestInstantAtLeavesOutNoNumber: a point that is NaN or an infinity is
// left out of the rules' answers, where the other queries' parse still
// refuses it.
func TestInstantAtLeavesOutNoNumber(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"app":"a"},"value":[1,"NaN"]},{"metric":{"app":"b"},"value":[1,"+Inf"]},{"metric":{"app":"c"},"value":[1,"0.5"]}]}}`
	got, err := parseAnswer([]byte(body), true)
	if err != nil || len(got) != 3 || len(got[0].points) != 0 || len(got[1].points) != 0 || len(got[2].points) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := parseQueryAnswer([]byte(body)); err == nil {
		t.Error("the other queries' parse took a NaN")
	}
	if _, err := parseAnswer([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"x"]}]}}`), true); err == nil {
		t.Error("a value that is no float was taken")
	}
	if v := roundSignificant(5.740305648246404e-05); v != 5.74031e-05 || roundSignificant(math.Inf(1)) != math.Inf(1) {
		t.Errorf("rounded %v", v)
	}
}

// standIn makes f the store every config's rules are evaluated against,
// with the process's evaluation made afresh, until the test ends.
func standIn(t *testing.T, f rulesQuerier) {
	t.Helper()
	savedStore, savedView := storeOf, theRuleValues
	storeOf = func(*MetricsStore) rulesQuerier { return f }
	theRuleValues = &rulesView{now: time.Now}
	t.Cleanup(func() { storeOf, theRuleValues = savedStore, savedView })
}

// queriesOf is how many queries one evaluation of asks makes: each
// distinct value query, and the two state queries.
func queriesOf(asks []valueAsk) int {
	distinct := map[string]bool{}
	for _, a := range asks {
		distinct[a.query] = true
	}
	return len(distinct) + 2
}

// TestPostDeployPins: post_deploy_regressed, one name for every
// post_deploy line, is listed for the one app its lines pin it to, or
// with the apps when they pin it to several, so an app's read can leave
// it out for any other; a line of every app makes it every app's.
func TestPostDeployPins(t *testing.T) {
	for _, c := range []struct {
		lines []string
		app   string
		apps  []string
		scope string
	}{
		{[]string{"rule shop_regress post_deploy app shop {\n\t\tmax_5xx_share 0.02\n\t}"}, "shop", []string{"shop"}, "app shop"},
		{[]string{"rule shop_regress post_deploy app shop {\n\t\tmax_5xx_share 0.02\n\t}", "rule blog_regress post_deploy app blog {\n\t\tmax_5xx_share 0.02\n\t}"}, "", []string{"blog", "shop"}, "apps blog, shop"},
		{[]string{"rule shop_regress post_deploy app shop {\n\t\tmax_5xx_share 0.02\n\t}", "rule regress post_deploy {\n\t\tmax_5xx_share 0.02\n\t}"}, "", nil, "every app"},
	} {
		l, err := listRules(rulesApp(t, c.lines...), polled{}, chainRead{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(l.Alerts, func(e AlertListing) bool { return e.Name == eventPostDeployRegressed })
		if i < 0 || l.Alerts[i].App != c.app || !slices.Equal(l.Alerts[i].PinnedTo, c.apps) {
			t.Errorf("%q: %+v", c.lines, l.Alerts[i])
			continue
		}
		if row := rowOf(string(rulesText(l, listingAt)), eventPostDeployRegressed); !strings.Contains(row, "  "+c.scope+"  ") {
			t.Errorf("%q: the table's row %q, want the scope %q", c.lines, row, c.scope)
		}
	}
}

func rowOf(table, name string) string {
	for _, l := range strings.Split(table, "\n") {
		if strings.HasPrefix(l, name+" ") {
			return l
		}
	}
	return ""
}

// TestRulesSourceIsTheCommand: the people's source gives the listing
// the admin route answers, byte for byte, from the same one evaluation
// of the store: the command and the people read one shape and one set
// of numbers.
func TestRulesSourceIsTheCommand(t *testing.T) {
	te := withEngine(t)
	te.active.Metrics = rulesApp(t, listingLines...).Metrics
	rulesInForce(t, te.active)
	l, err := listRules(te.active, polled{}, chainRead{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := listingStore(l)
	standIn(t, f)
	got, err := rulesSource{a: te.active}.Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w, err := callAdmin(t, serveRules, http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	var admin RulesListing
	if err := json.Unmarshal(w.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(admin)
	b, _ := json.Marshal(got)
	if string(a) != string(b) || got.Store.Values != wire.StoreOn {
		t.Errorf("the source:\n%s\nthe admin route:\n%s", b, a)
	}
	if n := f.count(); n != queriesOf(valueAsks(l)) {
		t.Errorf("the two made %d queries, want one evaluation's %d", n, queriesOf(valueAsks(l)))
	}
}
