package client

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// TestReadSeries: GET /_motel/apps/<app>/series's answer is taken only
// with its store's word, a window of the box's set, and, with the
// store's answer, a step and an end; each panel's name and unit a word
// (a marker of the box's filter shown as the client's words), each name
// once; each panel's points no more than its window has steps, oldest
// first, each inside the window. A store's word the client does not
// know is taken, to be said as not known; a panel it does not know too.
func TestReadSeries(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	const whole = `{"store":"on","window":"1h","step_seconds":60,"end":"2026-11-19T14:00:00Z","panels":[` +
		`{"name":"memory_bytes","unit":"bytes","points":[[1795093200,1],[1795096740,2],[1795096800,3]]},` +
		`{"name":"5xx_share","unit":"share","points":[]}]}`
	read := func(body string) (wire.Series, error) {
		var s wire.Series
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
		return s, cl.Read(ans, &s)
	}
	if s, err := read(whole); err != nil || len(s.Panels) != 2 || len(s.Panels[0].Points) != 3 {
		t.Fatalf("the whole answer: %v, %+v", err, s)
	}
	for _, c := range []struct{ name, body string }{
		{"a store this client does not know", `{"store":"later","window":"1h","panels":[]}`},
		{"no metrics line", `{"store":"off","window":"14d","panels":[]}`},
		{"unanswered", `{"store":"unanswered","window":"7d","panels":[]}`},
		{"a panel this client does not know", strings.Replace(whole, `"5xx_share"`, `"disk_bytes"`, 1)},
		{"a window whole: 61 points of the hour", `{"store":"on","window":"1h","step_seconds":60,"end":"2026-11-19T14:00:00Z","panels":[{"name":"cpu_cores","unit":"cores","points":[` + points(1795093200, 60, 61) + `]}]}`},
	} {
		if _, err := read(c.body); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, c := range []struct{ name, body, want string }{
		{"a marker in a name", strings.Replace(whole, `"memory_bytes"`, `"[redacted:PANEL]"`, 1), "a panel's name or unit in it is not a word)"},
		{"no store", strings.Replace(whole, `"store":"on",`, "", 1), "it has no store)"},
		{"no panels", `{"store":"on","window":"1h","step_seconds":60,"end":"2026-11-19T14:00:00Z"}`, "it has no panels)"},
		{"a store not a word", strings.Replace(whole, `"store":"on"`, `"store":"on\u001b[2J"`, 1), "its store is not a word)"},
		{"a window not the box's", strings.Replace(whole, `"window":"1h"`, `"window":"2h"`, 1), "its window is not one a box keeps)"},
		{"no step", strings.Replace(whole, `"step_seconds":60,`, "", 1), "its step or its end is not one a window has)"},
		{"no end", strings.Replace(whole, `"end":"2026-11-19T14:00:00Z",`, "", 1), "its step or its end is not one a window has)"},
		{"a step longer than the window", strings.Replace(whole, `"step_seconds":60`, `"step_seconds":3601`, 1), "its step or its end is not one a window has)"},
		{"a step that wraps a Duration", strings.Replace(whole, `"step_seconds":60`, `"step_seconds":9223372037`, 1), "its step or its end is not one a window has)"},
		{"a step that does not divide the window", strings.Replace(whole, `"step_seconds":60`, `"step_seconds":7`, 1), "its step or its end is not one a window has)"},
		{"a point off the step's grid", strings.Replace(whole, `[1795096740,2]`, `[1795096741,2]`, 1), "a point in it is not one of its window, oldest first)"},
		{"a step of a second over the hour", strings.Replace(whole, `"step_seconds":60`, `"step_seconds":1`, 1), "its step or its end is not one a window has)"},
		{"a name not a word", strings.Replace(whole, `"memory_bytes"`, `"memory bytes\u001b[2J"`, 1), "a panel's name or unit in it is not a word)"},
		{"a unit not a word", strings.Replace(whole, `"unit":"bytes"`, `"unit":"Bytes!"`, 1), "a panel's name or unit in it is not a word)"},
		{"a panel twice", strings.Replace(whole, `"5xx_share"`, `"memory_bytes"`, 1), "it holds a panel twice)"},
		{"points from a store that did not answer", strings.Replace(whole, `"store":"on"`, `"store":"unanswered"`, 1), "it holds points from a store that did not answer)"},
		{"a point before the window", strings.Replace(whole, `[1795093200,1]`, `[1795093199,1]`, 1), "a point in it is not one of its window, oldest first)"},
		{"a point after the end", strings.Replace(whole, `[1795096800,3]`, `[1795096801,3]`, 1), "a point in it is not one of its window, oldest first)"},
		{"points out of order", strings.Replace(whole, `[1795096740,2]`, `[1795093100,2]`, 1), "a point in it is not one of its window, oldest first)"},
		{"a time twice", strings.Replace(whole, `[1795096740,2]`, `[1795093200,2]`, 1), "a point in it is not one of its window, oldest first)"},
		{"a time not a second", strings.Replace(whole, `[1795096740,2]`, `[1795096740.5,2]`, 1), "a point in it is not one of its window, oldest first)"},
		{"62 points of the hour", `{"store":"on","window":"1h","step_seconds":60,"end":"2026-11-19T14:00:00Z","panels":[{"name":"cpu_cores","unit":"cores","points":[` + points(1795093200, 58, 62) + `]}]}`, "a panel in it holds more points than its window has steps)"},
	} {
		_, err := read(c.body)
		if err == nil {
			t.Errorf("%s: taken", c.name)
			continue
		}
		f := failure(t, err)
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not moteld's answer: "+c.want) {
			t.Errorf("%s: exit %d %q", c.name, f.Exit, f.Message)
		}
	}
}

// points is n points from t, every apart seconds, as JSON pairs.
func points(t, apart, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("[" + strconv.Itoa(t+i*apart) + ",1]")
	}
	return b.String()
}
