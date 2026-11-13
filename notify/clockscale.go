package notify

import (
	"bytes"
	"regexp"
	"time"
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
	d, err := time.ParseDuration(s)
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

// storeExec is the store's command line in its drop-in.
var storeExec = []byte("ExecStart=/usr/bin/victoria-metrics ")

// storeLatencyOffset is the store's -search.latencyOffset left at its
// default, which postdeploy.go's storeLag reads past.
const storeLatencyOffset = "30s"

func scaleUnit(b []byte, div int) []byte {
	if div == 1 {
		return b
	}
	out := unitClocks.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := unitClocks.FindSubmatch(m)
		return append(append([]byte{}, sub[1]...), scaleText(string(sub[2]), div)...)
	})
	// The store's own clock: within the offset an instant query reads
	// nothing new, so it is divided with the rest, as a flag the drop-in
	// does not otherwise carry.
	lines := bytes.Split(out, []byte("\n"))
	for i, l := range lines {
		if bytes.HasPrefix(l, storeExec) {
			lines[i] = append(append([]byte{}, l...), " -search.latencyOffset="+scaleText(storeLatencyOffset, div)...)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
