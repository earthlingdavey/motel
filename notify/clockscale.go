package notify

import (
	"bytes"
	"regexp"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// clockText is a duration as notify writes it into a rendered file
// (rules.yml, Alertmanager's config), divided by clockDiv: s itself at 1.
func clockText(s string) string { return scaleText(s, clockDiv) }

// clockUnit is a unit or scrape config of the store's as setup writes
// it, its clocks divided by clockDiv: b itself at 1.
func clockUnit(b []byte) []byte { return scaleUnit(b, clockDiv) }

func scaleText(s string, div int) string {
	if div == 1 {
		return s
	}
	d, err := caddy.ParseDuration(s) // takes d, as the store and Alertmanager do
	if err != nil {
		panic("notify: no duration to divide: " + s)
	}
	if d == 0 {
		return s
	}
	return promDuration(d / time.Duration(div))
}

// unitClocks are the clocks in the store's units and scrape config: the
// flag or key, then its duration.
var unitClocks = regexp.MustCompile(`(-configCheckInterval=|-evaluationInterval=|scrape_interval: )([0-9a-z]+)`)

// storeExec and vmalertExec are the store's and vmalert's command lines
// in their units.
var (
	storeExec   = []byte("ExecStart=/usr/bin/victoria-metrics ")
	vmalertExec = []byte("ExecStart=/usr/bin/vmalert ")
)

// The two flags the units leave at their defaults, which are one clock:
// the store's -search.latencyOffset, which postdeploy.go's storeLag
// reads past, and vmalert's -rule.evalDelay, which vmalert's help says
// should equal it (an evaluation reads that far back).
const (
	storeLatencyOffset = "30s"
	vmalertEvalDelay   = "30s"
)

func scaleUnit(b []byte, div int) []byte {
	if div == 1 {
		return b
	}
	out := unitClocks.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := unitClocks.FindSubmatch(m)
		return append(append([]byte{}, sub[1]...), scaleText(string(sub[2]), div)...)
	})
	// The defaults' clock: within the offset an instant query reads
	// nothing new, and an evaluation reads the delay back, so both are
	// divided with the rest, as flags the units do not otherwise carry.
	lines := bytes.Split(out, []byte("\n"))
	for i, l := range lines {
		switch {
		case bytes.HasPrefix(l, storeExec):
			lines[i] = append(append([]byte{}, l...), " -search.latencyOffset="+scaleText(storeLatencyOffset, div)...)
		case bytes.HasPrefix(l, vmalertExec):
			lines[i] = append(append([]byte{}, l...), " -rule.evalDelay="+scaleText(vmalertEvalDelay, div)...)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
