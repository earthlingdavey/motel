package client

import (
	"net/http"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// TestReadBackups: GET /_motel/backups's answer is taken only with its
// at, apps and lines, and with every value the client prints as a token
// in its grammar: an app's name, a result of a backup run's shape, a
// level of the three. Free text (a line's, could_not_tell) is not held; it is
// printed labelled and capped.
func TestReadBackups(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	const whole = `{"at":"2026-11-19T07:01:00Z","healthy":false,"apps":[{"name":"blog","healthy":false,"result":"data missing","last_complete_backup":null,"fresh":false,"restore_proven":null,"restore_proven_lately":false}],` +
		`"running":[{"role":"\u001b[2J","state":"x y"}],"unrecognised_units":["\u001b]0;x"],"lines":[{"level":"info","text":"last run \u001b[2J"},{"app":"blog","level":"problem","text":"blog: data missing"}]}`
	read := func(body string) (wire.Backups, error) {
		var b wire.Backups
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
		return b, cl.Read(ans, &b)
	}
	if b, err := read(whole); err != nil || len(b.Apps) != 1 || len(b.Lines) != 2 {
		t.Fatalf("the whole answer: %v, %+v", err, b)
	}
	// A newer box's class is a word of the shape: taken, and shown.
	if _, err := read(strings.Replace(whole, `"data missing"`, `"not yet verified"`, 1)); err != nil {
		t.Errorf("a class of the shape: %v", err)
	}
	for _, c := range []struct{ name, body, want string }{
		{"a marker in a line's app", strings.Replace(whole, `"app":"blog"`, `"app":"[redacted:APP]"`, 1), "a name in it is no app name)"},
		{"no at", strings.Replace(whole, `"at":"2026-11-19T07:01:00Z",`, "", 1), "it has no at)"},
		{"no lines", strings.Replace(whole, `,"lines":[{"level":"info","text":"last run \u001b[2J"},{"app":"blog","level":"problem","text":"blog: data missing"}]`, "", 1), "it has no lines)"},
		{"apps not a list", strings.Replace(whole, `"apps":[`, `"apps":{"x":[`, 1), ""},
		{"a name outside the grammar", strings.Replace(whole, `"name":"blog"`, `"name":"Blog\u001b[2J"`, 1), "a name in it is no app name)"},
		{"a result not of the shape", strings.Replace(whole, `"data missing"`, `"ok\u001b[31m"`, 1), "an app's result in it is not one a backup run says)"},
		{"a level not of the three", strings.Replace(whole, `"level":"info"`, `"level":"fine"`, 1), "a line's level in it is not info, warning or problem)"},
		{"a line's app outside the grammar", strings.Replace(whole, `"app":"blog"`, `"app":"x y"`, 1), "a name in it is no app name)"},
	} {
		_, err := read(c.body)
		f := failure(t, err)
		if f.Exit != ExitUnknown || c.want != "" && !strings.Contains(f.Message, "not moteld's answer: "+c.want) {
			t.Errorf("%s: exit %d %q", c.name, f.Exit, f.Message)
		}
	}
}
