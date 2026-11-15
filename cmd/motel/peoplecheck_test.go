package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// TestWarnPeople: validate says what the box block's own reading cannot
// see. People with no monitor channel are warned of, a block of signers
// alone is not, and a monitor block with a channel quiets it; a grant's
// from list beside a server's trusted_proxies is warned of, and either
// alone is not.
func TestWarnPeople(t *testing.T) {
	const (
		people   = `"people":[{"name":"bob","type":"ssh-ed25519","key":"x"}]`
		grant    = `"grants":[{"name":"bob","role":"viewer","scope":"box"}]`
		fromList = `"grants":[{"name":"bob","role":"viewer","scope":"box","from":["198.51.100.0/24"]}]`
		channel  = `"monitor":{"channels":[{"name":"ops","kind":"ntfy","url":"{secret.NOTIFY_OPS_URL}"}],"operators":["ops"]}`
		proxies  = `"http":{"servers":{"srv0":{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]}}}}`
		plain    = `"http":{"servers":{"srv0":{}}}`
	)
	cfg := func(parts ...string) []byte {
		return []byte(`{"apps":{` + strings.Join(parts, ",") + `}}`)
	}
	box := func(lines ...string) string {
		return `"box":{"signers":[{"principal":"ops","type":"ssh-ed25519","key":"x"}]` + strings.Repeat(",", min(1, len(lines))) + strings.Join(lines, ",") + `}`
	}
	for _, c := range []struct {
		name             string
		cfg              []byte
		channel, proxies bool
	}{
		{"no box block", cfg(plain), false, false},
		{"signers alone", cfg(box(), plain), false, false},
		{"a person, no monitor block", cfg(box(people), plain), true, false},
		{"a grant, no monitor block", cfg(box(people, grant), plain), true, false},
		{"a person and a channel", cfg(box(people), channel, plain), false, false},
		{"a from list behind trusted proxies", cfg(box(people, fromList), channel, proxies), false, true},
		{"a from list, no trusted proxies", cfg(box(people, fromList), channel, plain), false, false},
		{"trusted proxies, no from list", cfg(box(people, grant), channel, proxies), false, false},
	} {
		var stderr bytes.Buffer
		warnPeople(c.cfg, &stderr)
		if got := strings.Contains(stderr.String(), access.NoChannelWarning); got != c.channel {
			t.Errorf("%s: no-channel warning %v: %s", c.name, got, stderr.String())
		}
		if got := strings.Contains(stderr.String(), access.FromProxiesWarning); got != c.proxies {
			t.Errorf("%s: trusted_proxies warning %v: %s", c.name, got, stderr.String())
		}
	}
}
