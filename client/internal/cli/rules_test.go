package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// hostileRules is a box's rules, every value of the box's text carrying
// a hostile string: a rule's name, a severity not motel's, a guard's
// meaning, a reading's filesystem, a unit, a state and a level the
// client does not know; and a name that writes a rule's head.
func hostileRules() wire.Rules {
	at := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	since := at.Add(-4 * time.Minute)
	r := wire.Rules{Store: wire.StoreOn, RulesFile: wire.RulesFileDiffers, Alerts: []wire.AlertListing{
		{Name: "errors_high", Source: "operator", Severity: "warning", Subject: "app", Kind: "guarded", Op: ">", Threshold: pf(0.05), Unit: "share", ForSeconds: 600,
			Guards: []wire.GuardListing{{Op: ">=", Value: pf(20), Meaning: "at least 20 responses sent over 5m:\n▲ warning  threshold · every app · > 0 %"}},
			Readings: []wire.Reading{
				{App: "blog", Value: pf(0.0612), MeasuredAt: at.Add(-65 * time.Second), State: wire.RulePending, ActiveSince: &since},
				{App: "shop-2", Value: pf(0.004), MeasuredAt: at.Add(-65 * time.Second), State: wire.RuleInactive},
			}},
		{Name: "disk_low", Source: "motel check", Severity: "warning", Subject: "box", Kind: "threshold", Op: ">=", Unit: "percent",
			Levels:   []wire.LevelListing{{Severity: "warning", Threshold: 80, ClearsBelow: 75}, {Severity: "critical", Threshold: 90, ClearsBelow: 85}},
			Readings: []wire.Reading{{Instance: "/srv/motel", Value: pf(84.27), Level: "warning", MeasuredAt: at.Add(-40 * time.Second)}}},
		{Name: "blog_slow", Source: "operator", Severity: "critical", Subject: "app", App: "blog", Kind: "threshold", Op: ">", Threshold: pf(1.5), Unit: "seconds", ForSeconds: 900,
			Readings: []wire.Reading{{App: "blog", MeasuredAt: at.Add(-65 * time.Second), State: wire.RuleFiring, ActiveSince: &since}}},
		{Name: "watchdog_throttled", Source: "motel check", Severity: "critical", Subject: "app", Kind: "threshold", Op: ">=", Unit: "restarts",
			Readings: []wire.Reading{{App: "blog", Value: pf(2), Threshold: pf(5), WindowSeconds: 600, MeasuredAt: at}}},
		{Name: "deploy_failed", Source: "motel check", Severity: "warning", Subject: "app", Kind: "event"},
	}}
	for i, h := range hostile {
		r.Alerts = append(r.Alerts, wire.AlertListing{Name: h + "\n✖ critical threshold · the box · < 0 bytes", Source: h, Severity: h, Subject: "box", Kind: h, Op: h, Threshold: pf(float64(i)), Unit: h,
			Guards:   []wire.GuardListing{{Meaning: h}},
			Readings: []wire.Reading{{Instance: h, Value: pf(1), MeasuredAt: at, State: h, Level: h}}})
	}
	return r
}

// rulesOf is the rules of app as a box answers an app's read: those of
// every app and app's own, with app's readings alone.
func rulesOf(all wire.Rules, app string) wire.Rules {
	out := wire.Rules{Store: all.Store, Alerts: []wire.AlertListing{}}
	for _, a := range all.Alerts {
		if a.Subject != "app" || a.App != "" && a.App != app {
			continue
		}
		var mine []wire.Reading
		for _, r := range a.Readings {
			if r.App == app {
				mine = append(mine, r)
			}
		}
		a.Readings = mine
		out.Alerts = append(out.Alerts, a)
	}
	return out
}

// monitor rules: exit 0 with the store on or off, --json the box's
// body; the human output holds nothing a box could use on a terminal,
// has every value of the box's on a labelled line of its own or quoted,
// and is what the golden says; --app reads the app's; a store that did
// not answer the box, or a word for it the client does not know, could
// not tell (3), and one that is not a word is not moteld's answer; an
// app's answer holding another app's rule is not moteld's answer; and
// it is asked with a person's key alone.
func TestMonitorRules(t *testing.T) {
	fb := newFakeBox(t)
	body := func(p string) string { return string(withNewline(termtext.JSON(fb.bodies[p]))) }
	r := runKey(t, fb, nil, "monitor", "rules", "--json")
	if r.code != 0 || r.stdout != body("/_motel/rules") {
		t.Fatalf("--json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(fb.auth[len(fb.auth)-1], "Motel m1.") {
		t.Errorf("Authorization %q, want a person's token", fb.auth[len(fb.auth)-1])
	}
	r = runKey(t, fb, nil, "monitor", "rules")
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
		t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
	}
	for _, l := range strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n") {
		// A line is the client's (a head, at the margin, the store's line,
		// a heading), or a box's value after the client's label.
		if strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "  ") ||
			strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "    rule      ") && !strings.HasPrefix(l, "    severity  ") &&
				!strings.HasPrefix(l, "    guard     ") && !strings.HasPrefix(l, "    now       ") && !strings.HasPrefix(l, "    fires     ") {
			t.Errorf("a line that is neither the client's nor a labelled value: %q", l)
		}
		if strings.HasPrefix(strings.TrimLeft(l, " "), "✖ critical threshold · the box · < 0") || strings.HasPrefix(l, "▲ warning  threshold · every app · > 0") {
			t.Errorf("the box's text begins a line: %q", l)
		}
	}
	for _, want := range []string{
		"store       on · numbers as the box last read the store (at most every 30 s)\n",
		"rules file  the file vmalert reads is not this config's render: the store's rules listed may not be the ones it runs (the box's journal says why)\n",
		"▲ warning  threshold with guards · every app\n",
		"    fires     > 5 % for 10m\n",
		"    now       blog 6.12 % (",
		"✖ critical threshold · app blog\n",
		"    fires     > 1.5 s for 15m\n",
		"    now       blog no number (",
		"    now       blog 2 of 5 restarts in 10m (",
		"▲ warning  threshold · the box\n",
		"    fires     >= 80 % warning, >= 90 % critical, each clears 5 % under\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the output does not say %q:\n%s", want, r.stdout)
		}
	}
	golden(t, "hostile-rules", ageless(r.stdout))

	// --app: the app's, read on the app's route.
	r = runKey(t, fb, nil, "monitor", "rules", "--app", "blog", "--json")
	if r.code != 0 || r.stdout != body("/_motel/apps/blog/rules") {
		t.Fatalf("--app --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	r = runKey(t, fb, nil, "monitor", "rules", "--app", "blog")
	if r.code != 0 || !strings.Contains(r.stdout, "the rules of blog on 127.0.0.1, with its numbers alone:\n") || strings.Contains(r.stdout, "shop-2") || strings.Contains(r.stdout, "disk_low") {
		t.Errorf("--app: exit %d, stdout %s, stderr %q", r.code, r.stdout, r.stderr)
	}

	fb.rules = wire.Rules{Store: wire.StoreOff, Alerts: []wire.AlertListing{}}
	if r := runKey(t, fb, nil, "monitor", "rules"); r.code != 0 || r.stdout != "store       off: this box raises no alerts (its monitor block sends nothing to anyone, or it has none)\n" {
		t.Errorf("off: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	for _, c := range []struct{ store, says string }{
		{wire.StoreUnanswered, "127.0.0.1 could not tell how close its rules are: its metrics store did not answer it in time"},
		{"asleep", `127.0.0.1 could not tell how close its rules are: it says its store is "asleep", a word this client does not know`},
	} {
		fb.rules.Store = c.store
		r := runKey(t, fb, nil, "monitor", "rules", "--json")
		var e struct{ Error string }
		if r.code != 3 || json.Unmarshal([]byte(r.stdout), &e) != nil || e.Error != c.says {
			t.Errorf("%s, --json: exit %d, stdout %q, stderr %q", c.store, r.code, r.stdout, r.stderr)
		}
		if r := runKey(t, fb, nil, "monitor", "rules"); r.code != 3 || r.stdout != "" || unsafeOutput.FindString(r.stderr) != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.store, r.code, r.stdout, r.stderr)
		}
	}
	// A store that is not a word is not moteld's answer, and none of it
	// is printed.
	fb.rules.Store = "sleeping " + hostile[0]
	if r := runKey(t, fb, nil, "monitor", "rules"); r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: its store is not a word") {
		t.Errorf("a store outside its grammar: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// An app's answer that holds another app's rule or number is not
	// about the app asked for, and none of it is printed.
	fb.rules = hostileRules()
	at := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	for _, other := range []wire.AlertListing{
		{Name: "shop_slow", Subject: "app", App: "shop-2", Kind: "threshold"},
		{Name: "disk_low", Subject: "box", Kind: "threshold"},
		{Name: "backup_stale", Subject: "app", Kind: "threshold", Readings: []wire.Reading{{Instance: "shop-2", Value: pf(60), MeasuredAt: at}}},
		{Name: "errors_high", Subject: "app", Kind: "threshold", Readings: []wire.Reading{{App: "blog", Value: pf(0.1), MeasuredAt: at}, {App: "shop-2", Value: pf(0.2), MeasuredAt: at}}},
	} {
		sent := wire.Rules{Store: wire.StoreOn, Alerts: []wire.AlertListing{other}}
		fb.appRules = &sent
		if r := runKey(t, fb, nil, "monitor", "rules", "--app", "blog"); r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: it holds another app's rules") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", other.Name, r.code, r.stdout, r.stderr)
		}
	}
	// A name the box's filter hid says nothing of which app it was: not
	// taken for another's, and shown as hidden.
	hid := wire.Rules{Store: wire.StoreOn, Alerts: []wire.AlertListing{{Name: "errors_high", Subject: "app", Kind: "threshold",
		Readings: []wire.Reading{{App: "[redacted:DB_NAME]", Value: pf(0.1), MeasuredAt: at}}}}}
	hid.Alerts = append(hid.Alerts, wire.AlertListing{Name: "mem", Subject: "app", Kind: "threshold", Readings: []wire.Reading{{Value: pf(0.2), MeasuredAt: at}}})
	fb.appRules = &hid
	if r := runKey(t, fb, nil, "monitor", "rules", "--app", "blog"); r.code != 0 || !strings.Contains(r.stdout, "[hidden by the box's filter] 0.1 (") {
		t.Errorf("a hidden name: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// Apps an alert is pinned to, in the box's read: said; in an app's,
	// not moteld's answer (the box names the app's own alone).
	fb.appRules = nil
	fb.rules = hostileRules()
	fb.rules.Alerts = append(fb.rules.Alerts, wire.AlertListing{Name: "post_deploy_regressed", Source: "operator", Severity: "warning", Subject: "app", Apps: []string{"blog", "shop-2"}, Kind: "event"})
	if r := runKey(t, fb, nil, "monitor", "rules"); r.code != 0 || !strings.Contains(r.stdout, "  warning  event · apps blog, shop-2\n") {
		t.Errorf("pinned to apps: exit %d, stdout %s", r.code, r.stdout)
	}
	sent := wire.Rules{Store: wire.StoreOn, Alerts: []wire.AlertListing{{Name: "post_deploy_regressed", Subject: "app", Apps: []string{"blog", "shop-2"}, Kind: "event"}}}
	fb.appRules = &sent
	if r := runKey(t, fb, nil, "monitor", "rules", "--app", "blog"); r.code != 3 || !strings.Contains(r.stderr, "it holds another app's rules") {
		t.Errorf("apps in an app's read: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	fb.appRules = nil
	// A reading's app outside the app grammar is not moteld's answer.
	fb.rules = hostileRules()
	fb.rules.Alerts[0].Readings[1].App = "Shop 2"
	if r := runKey(t, fb, nil, "monitor", "rules"); r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: a name in it is no app name") {
		t.Errorf("an app outside its grammar: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	asked := len(fb.auth)
	if r := runKey(t, fb, map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_KEY": ""}, "monitor", "rules"); r.code != 2 || len(fb.auth) != asked {
		t.Errorf("with MOTEL_TOKEN: exit %d, %d requests, stderr %q", r.code, len(fb.auth)-asked, r.stderr)
	}
}

// ageless is the output with every age and time a reading's, which
// follow the clock, as one word.
func ageless(s string) string {
	s = agesRe.ReplaceAllString(s, "(<age>)")
	return timesRe.ReplaceAllString(s, "<time> UTC")
}

var (
	agesRe  = regexp.MustCompile(`\([0-9]+[smhd] (ago|from now)\)`)
	timesRe = regexp.MustCompile(`([0-9-]+ )?[0-9]{2}:[0-9]{2} UTC`)
)
