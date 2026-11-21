package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRulesSays: the rules screen says each rule in the client's words,
// its head (mark, severity, kind, scope, condition) and under it the
// box's words and its numbers, each on a row of its own after the
// client's label and cut to the window, so a rule's name or a guard's
// meaning that writes a row of the screen's own shape, or a terminal
// escape, never begins a row; a mark repeats only what the box said
// (firing, pending, a disk's level), never a number against its
// threshold; and a word the client does not know is said as not known.
func TestRulesSays(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxRulesOpen(true, *now)
	forged := strings.Repeat("x", 70) + "✖ critical threshold · every app · > 5 % for 10m"
	f.Body("box-rules.json", `{"store":"on","rules_file":"differs","alerts":[`+
		`{"name":"errors_high","source":"operator","severity":"warning","subject":"app","kind":"guarded","op":">","threshold":0.05,"unit":"share","for_seconds":600,`+
		`"guards":[{"op":">=","value":20,"meaning":"at least 20 `+forged+` \u001b]0;pwned\u0007"}],`+
		`"readings":[{"app":"shop","value":0.0612,"measured_at":"@-65s","state":"pending","active_since":"@-4m"},{"app":"wiki","value":0.9,"measured_at":"@-65s","state":"inactive"}]},`+
		`{"name":"`+forged+`","source":"operator","severity":"page","subject":"box","kind":"threshold","op":"<","threshold":0,"unit":"bytes","for_seconds":12600,`+
		`"readings":[{"instance":"/srv/motel","value":-1234570000,"measured_at":"@-65s","state":"firing","active_since":"@-1h"}]},`+
		`{"name":"disk_low","source":"motel check","severity":"warning","subject":"box","kind":"threshold","op":">=","unit":"percent",`+
		`"levels":[{"severity":"warning","threshold":80,"clears_below":75},{"severity":"critical","threshold":90,"clears_below":85}],`+
		`"readings":[{"instance":"/srv/motel","value":91.27,"level":"critical","measured_at":"@-40s"}]},`+
		`{"name":"mem","source":"operator","severity":"warning","subject":"app","app":"shop","kind":"threshold","op":">","threshold":0.8,"unit":"share","for_seconds":300,`+
		`"readings":[{"app":"shop","measured_at":"@-65s","state":"asleep"}]},`+
		`{"name":"rollback","source":"motel check","severity":"warning","subject":"app","kind":"event"}]}`)
	for range 4 { // whoami, the list, the watch's alerts, the rules
		step(t, s, f, now)
	}
	if s.BoxRulesSt == nil {
		t.Fatalf("not read: %v", s.BoxRulesErr)
	}
	v := s.RulesViewFit("", *now, 60, runes)
	text := joined(v)
	for _, l := range v {
		row := l.Text()
		if l.Width(runes) > 60 {
			t.Errorf("a row wider than 60: %q", row)
		}
		// A guard's meaning goes on at column 6, where no row of the
		// screen's begins.
		if strings.Contains(row, "xxx") && !strings.HasPrefix(row, "    rule      ") && !strings.HasPrefix(row, "    guard     ") && !strings.HasPrefix(row, "      ") ||
			strings.HasPrefix(strings.TrimLeft(row, " "), "✖ critical threshold") || strings.ContainsAny(row, "\x1b\x07") {
			t.Errorf("a row the box's words begin, or with an escape: %q", row)
		}
	}
	whole := joined(s.BoxRulesView(*now))
	for _, want := range []string{
		"store       on · numbers as the box last read the store (at most every 30 s)\n",
		"rules file  the file vmalert reads is not this config's render: the store's rules listed may not be the ones it runs (the box's journal says why)\n",
		"\nRULES, as the box alerts on them\n",
		"▲ warning  threshold with guards · every app\n",
		"    fires     > 5 % for 10m\n",
		"    rule      errors_high\n",
		"    guard     >= 20: at least 20 xxx",
		"    now       shop 6.12 % (1m ago) · pending since 13:56 UTC (4m ago)\n",
		"    now       wiki 90 % (1m ago) · inactive\n",
		"? other    threshold · the box\n",
		"    fires     < 0 bytes for 3h 30m\n",
		"    severity  page\n",
		"    now       /srv/motel -1.15 GiB (1m ago) · firing since 13:00 UTC (1h ago)\n",
		"✖ warning  threshold · the box\n",
		"    fires     >= 80 % warning, >= 90 % critical, each clears 5 % under\n",
		"    now       /srv/motel 91.2 % (40s ago) · firing at critical\n",
		"? warning  threshold · app shop\n",
		"    fires     > 80 % for 5m\n",
		"    now       shop no number (1m ago) · state not known, the box said \"asleep\"\n",
		"  warning  event · every app\n",
		"    fires     an event: nothing to be close to\n",
		"    rule      rollback\n",
	} {
		if !strings.Contains(whole, want) {
			t.Errorf("the screen does not say %q:\n%s", want, whole)
		}
	}
	// A number over its threshold that the box does not hold pending or
	// firing is not marked: the client judges nothing.
	if strings.Contains(text, "▲ warning  threshold · app") || strings.Contains(s.RulesViewFit("", *now, 0, runes)[0].Text(), "▲") {
		t.Errorf("a mark the box did not say:\n%s", text)
	}

	// No store, none raising anything, a store that did not answer and a
	// word not known: said, the rules listed as they are.
	for body, want := range map[string]string{
		`{"store":"off","rules_file":"differs","alerts":[{"name":"rollback","source":"motel check","severity":"warning","subject":"app","kind":"event"}]}`: "store       off: this box has no metrics store, so no rule of the store's is listed\nrules file  the file vmalert reads is not this config's render: vmalert may run rules not listed here (the box's journal says why)\n",
		`{"store":"off","alerts":[]}`: "store       off: this box raises no alerts (its monitor block sends nothing to anyone, or it has none)\n",
		`{"store":"off","alerts":[{"name":"rollback","source":"motel check","severity":"warning","subject":"app","kind":"event"}]}`: "store       off: this box has no metrics store, so no rule of the store's is listed\n",
		`{"store":"unanswered","alerts":[]}`:                "store       not known: the store did not answer the box in time, so the store's rules have no numbers\n",
		`{"store":"later","alerts":[],"rules_file":"gone"}`: "store       not known: the box said \"later\", so the store's rules' numbers are not known\nrules file  not this config's, maybe: the box said \"gone\" of the file vmalert reads\n",
	} {
		*now = now.Add(model.BoxEvery)
		f.Body("box-rules.json", body)
		for range 3 {
			step(t, s, f, now)
		}
		if got := joined(s.BoxRulesView(*now)); !strings.HasPrefix(got, want) {
			t.Errorf("%s: the screen says\n%s", body, got)
		}
	}

	// A read that gets no answer keeps the answer before it, and the
	// crumb says the last read failed; a 403 drops it and says how to ask
	// again.
	*now = now.Add(model.BoxEvery)
	f.Set("box-rules", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	for range 3 {
		step(t, s, f, now)
	}
	if s.BoxRulesSt == nil || !strings.HasSuffix(s.RulesAsOf("", *now).Text(), " · the last read failed") || !strings.Contains(joined(s.BoxRulesView(*now)), "the last read failed: no answer") {
		t.Errorf("after a read that got no answer: held %v, the crumb says %q", s.BoxRulesSt != nil, s.RulesAsOf("", *now).Text())
	}
	*now = now.Add(model.BoxEvery)
	f.Set("box-rules", &model.Failure{Kind: model.FailForbidden, Message: "not allowed"})
	for range 3 {
		step(t, s, f, now)
	}
	if s.BoxRulesSt != nil || !strings.Contains(joined(s.BoxRulesView(*now)), "ctrl-r") {
		t.Errorf("after a 403: held %v, the screen says\n%s", s.BoxRulesSt != nil, joined(s.BoxRulesView(*now)))
	}
}

// TestAppRulesSays: an app's rules screen holds the app's answer alone,
// apart from the box's and from another app's.
func TestAppRulesSays(t *testing.T) {
	s, f, now := session(t)
	s.Engine.RulesOpen("shop", *now)
	for range 4 {
		step(t, s, f, now)
	}
	text := joined(s.RulesViewFit("shop", *now, 0, runes))
	if !strings.Contains(text, "RULES that apply to shop, with its numbers alone\n") || !strings.Contains(text, "    now       shop 6.12 % (1m ago) · pending since ") {
		t.Errorf("shop's rules:\n%s", text)
	}
	// The rules file's word, on an app's screen too.
	*now = now.Add(model.BoxEvery)
	f.Body("rules/shop.json", `{"store":"on","rules_file":"missing","alerts":[]}`)
	for range 3 {
		step(t, s, f, now)
	}
	if got := joined(s.RulesViewFit("shop", *now, 0, runes)); !strings.Contains(got, "rules file  the file vmalert reads is not there: the store's rules listed may not be the ones it runs") {
		t.Errorf("shop's rules file:\n%s", got)
	}
	if got := joined(s.RulesViewFit("wiki", *now, 0, runes)); !strings.HasPrefix(got, "asking ") {
		t.Errorf("wiki's, never read: %s", got)
	}
	if got := joined(s.BoxRulesView(*now)); !strings.HasPrefix(got, "asking ") {
		t.Errorf("the box's, never read: %s", got)
	}
	if s.RulesAsOf("shop", *now) == nil || s.RulesAsOf("", *now) != nil {
		t.Error("the crumbs' ages")
	}
}

// TestOldBoxRulesNotRefused: a box older than the rules reads answers
// them 404, which their screens say as a box that does not have them;
// the title bar does not call it a refusal.
func TestOldBoxRulesNotRefused(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "shop"}]}`)
	nf := &model.Failure{Kind: model.FailNotFound, Message: "deploy.example.com answered 404"}
	f.Set("box-rules", nf)
	f.Set("rules:shop", nf)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for _, c := range []struct {
		open func()
		kind model.Kind
	}{
		{func() { s.Engine.BoxRulesOpen(true, now) }, model.KBoxRules},
		{func() { s.Engine.BoxRulesOpen(false, now); s.Engine.RulesOpen("shop", now) }, model.KRules},
	} {
		c.open()
		read := false
		for range 8 {
			if step(t, s, f, &now).Kind == c.kind {
				read = true
				break
			}
		}
		if !read {
			t.Fatalf("kind %d not read", c.kind)
		}
		// Right after the 404, before another read's answer could say the
		// box answered.
		a, b := s.Title(now)
		if title := a.Text() + b.Text(); strings.Contains(title, "refused") {
			t.Errorf("kind %d: the title bar says %q after the rules' 404", c.kind, title)
		}
	}
	if v := joined(s.BoxRulesView(now)); !strings.Contains(v, "its moteld is older than this read (GET /_motel/rules)") {
		t.Errorf(":rules after a 404 says\n%s", v)
	}
	if v := joined(s.RulesViewFit("shop", now, 0, runes)); !strings.Contains(v, "a moteld older than this read, or no app of that name now") {
		t.Errorf(":rules shop after a 404 says\n%s", v)
	}
}

// TestRulesReadWhileOpen: the rules are read only while their screen is
// open, the box's or one app's, at once on opening it the first time
// and then every BoxEvery; another app's rules screen reads that app's
// and not the one left; a refused read is asked again only by ctrl-r.
func TestRulesReadWhileOpen(t *testing.T) {
	e := model.NewEngine(t0)
	refuse := map[model.Job]bool{}
	answer := func(j model.Job, _ time.Time) error {
		if refuse[j] {
			return &model.Failure{Kind: model.FailForbidden, Message: "not allowed"}
		}
		return nil
	}
	count := func(calls []call, j model.Job) int {
		n := 0
		for _, c := range calls {
			if c.j == j {
				n++
			}
		}
		return n
	}
	box, shop, wiki := model.Job{Kind: model.KBoxRules}, model.Job{Kind: model.KRules, App: "shop"}, model.Job{Kind: model.KRules, App: "wiki"}
	at := t0
	run := func(d time.Duration) []call {
		calls := sim(e, []string{"shop", "wiki"}, at, d, 10*time.Millisecond, answer)
		at = at.Add(d)
		return calls
	}
	if c := run(2 * time.Minute); count(c, box)+count(c, shop)+count(c, wiki) != 0 {
		t.Fatalf("rules read with no rules screen open: %v", c)
	}
	e.BoxRulesOpen(true, at)
	c := run(95 * time.Second)
	if n := count(c, box); n != 4 || c[0].j != box && c[1].j != box {
		t.Errorf("the box's rules open 95 s: %d reads, the first two %v", n, c[:2])
	}
	e.BoxRulesOpen(false, at)
	e.RulesOpen("shop", at)
	c = run(65 * time.Second)
	if count(c, box) != 0 || count(c, shop) != 3 || count(c, wiki) != 0 {
		t.Errorf("shop's open: box %d, shop %d, wiki %d", count(c, box), count(c, shop), count(c, wiki))
	}
	refuse[wiki] = true
	e.RulesOpen("wiki", at)
	c = run(2 * time.Minute)
	if count(c, shop) != 0 || count(c, wiki) != 1 {
		t.Errorf("wiki's open, refused: shop %d, wiki %d", count(c, shop), count(c, wiki))
	}
	refuse[wiki] = false
	e.Again(at, wiki)
	if c = run(10 * time.Second); count(c, wiki) != 1 {
		t.Errorf("ctrl-r on a refused read: %d", count(c, wiki))
	}
	e.RulesOpen("", at)
	if c = run(2 * time.Minute); count(c, box)+count(c, shop)+count(c, wiki) != 0 {
		t.Errorf("rules read once their screens closed: %v", c)
	}
}

// TestRuleWords: numbers are written plainly at every size, never in an
// exponent's form; the watchdog's condition is the box's (a spent
// budget is not firing); a reading of both an app and a filesystem
// names both; and a guard's meaning, motel's sentence, is laid whole on
// rows two columns in from its label's, never cut at the window.
func TestRuleWords(t *testing.T) {
	for _, c := range []struct {
		v    float64
		unit string
		want string
	}{
		{1000.4 * (1 << 20), "bytes", "1000 MiB"}, {0.9996, "seconds", "1000 ms"}, {12345, "requests/s", "12345 requests/s"}, {12345, "reads", "12345 reads"}, {1048575, "bytes", "1024 KiB"}, {1e308, "", "1e+308"}, {123.456789, "cores", "123.5 cores"},
		{0.0001234, "", "0.0001234"}, {-1234570000, "bytes", "-1.15 GiB"}, {0.0612, "share", "6.12 %"}, {79.96, "percent", "79.9 %"},
	} {
		if got := model.Amount(c.v, c.unit); got != c.want {
			t.Errorf("%v %s: %q, want %q", c.v, c.unit, got, c.want)
		}
	}
	s, f, now := session(t)
	s.Engine.BoxRulesOpen(true, *now)
	meaning := "the 6h line predicts it full too: a one-off write tilts the long line alone, and so on to the end"
	f.Body("box-rules.json", `{"store":"on","alerts":[`+
		`{"name":"watchdog_throttled","source":"motel check","severity":"critical","subject":"app","kind":"threshold","op":">=","unit":"restarts",`+
		`"readings":[{"app":"shop","value":5,"threshold":5,"window_seconds":600,"measured_at":"@-5s"}]},`+
		`{"name":"usage","source":"operator","severity":"warning","subject":"app","kind":"guarded","op":">","threshold":0.9,"unit":"share",`+
		`"guards":[{"op":"<","value":0,"meaning":"`+meaning+`"}],"readings":[{"app":"shop","instance":"/srv/a","value":0.41,"measured_at":"@-65s","state":"inactive"}]}]}`)
	for range 4 { // whoami, the list, the watch's alerts, the rules
		step(t, s, f, now)
	}
	text := joined(s.RulesViewFit("", *now, 60, runes))
	for _, want := range []string{
		"    fires     a further restart needed while its budget in\n      its window is spent\n",
		"    now       shop 5 of 5 restarts in 10m (5s ago)\n",
		"    now       /srv/a shop 41 % (1m ago) · inactive\n",
		"    guard     < 0: the 6h line predicts it full too: a\n      one-off write",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the screen does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "…") && strings.Contains(text, "guard") && !strings.Contains(strings.Join(strings.Fields(text), " "), "and so on to the end") {
		t.Errorf("a guard's meaning cut:\n%s", text)
	}
}

// TestRulesUnreadStore: when the box's store did not answer, a rule the
// store evaluates says its number is not known and is marked so, never
// a quiet "no number"; a check keeps its reading.
func TestRulesUnreadStore(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxRulesOpen(true, *now)
	f.Body("box-rules.json", `{"store":"unanswered","alerts":[`+
		`{"name":"app_memory_high","source":"builtin","severity":"warning","subject":"app","kind":"threshold","op":">","threshold":0.9,"unit":"share","for_seconds":600},`+
		`{"name":"disk_low","source":"motel check","severity":"warning","subject":"box","kind":"threshold","op":">=","unit":"percent","readings":[{"instance":"/srv","value":40,"measured_at":"@-40s"}]}]}`)
	for range 4 { // whoami, the list, the watch's alerts, the rules
		step(t, s, f, now)
	}
	text := joined(s.BoxRulesView(*now))
	for _, want := range []string{
		"? warning  threshold · every app\n    rule      app_memory_high\n    fires     > 90 % for 10m\n    now       not known: the store's numbers were not read\n",
		"    now       /srv 40 % (40s ago)\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the screen does not say %q:\n%s", want, text)
		}
	}
}
