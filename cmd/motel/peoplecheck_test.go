package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// TestWarnPeople: validate says what the box block's own reading cannot
// see. People with no monitor channel are warned of, a block of signers
// alone is not, and a monitor block with a channel quiets it. A grant's
// from list beside a server's trusted_proxies is refused unless the
// server sets trusted_proxies_strict, and warned of when it does;
// either alone is neither.
func TestWarnPeople(t *testing.T) {
	const (
		people   = `"people":[{"name":"bob","type":"ssh-ed25519","key":"x"}]`
		grant    = `"grants":[{"name":"bob","role":"viewer","scope":"box"}]`
		fromList = `"grants":[{"name":"bob","role":"viewer","scope":"box","from":["198.51.100.0/24"]}]`
		channel  = `"monitor":{"channels":[{"name":"ops","kind":"ntfy","url":"{secret.NOTIFY_OPS_URL}"}],"operators":["ops"]}`
		proxies  = `"http":{"servers":{"srv0":{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]}}}}`
		strict   = `"http":{"servers":{"srv0":{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]},"trusted_proxies_strict":1}}}`
		plain    = `"http":{"servers":{"srv0":{}}}`
	)
	cfg := func(parts ...string) []byte {
		return []byte(`{"apps":{` + strings.Join(parts, ",") + `}}`)
	}
	box := func(lines ...string) string {
		return `"box":{"signers":[{"principal":"ops","type":"ssh-ed25519","key":"x"}]` + strings.Repeat(",", min(1, len(lines))) + strings.Join(lines, ",") + `}`
	}
	for _, c := range []struct {
		name                     string
		cfg                      []byte
		channel, proxies, refuse bool
	}{
		{"no box block", cfg(plain), false, false, false},
		{"signers alone", cfg(box(), plain), false, false, false},
		{"a person, no monitor block", cfg(box(people), plain), true, false, false},
		{"a grant, no monitor block", cfg(box(people, grant), plain), true, false, false},
		{"a person and a channel", cfg(box(people), channel, plain), false, false, false},
		{"a from list behind trusted proxies, not strict", cfg(box(people, fromList), channel, proxies), false, false, true},
		{"a from list behind strictly trusted proxies", cfg(box(people, fromList), channel, strict), false, true, false},
		{"a from list, no trusted proxies", cfg(box(people, fromList), channel, plain), false, false, false},
		{"trusted proxies, no from list", cfg(box(people, grant), channel, proxies), false, false, false},
		{"signers alone behind trusted proxies", cfg(box(), proxies), false, false, false},
	} {
		err := checkPeople(c.cfg)
		if (err != nil) != c.refuse || (err != nil && !strings.Contains(err.Error(), "trusted_proxies_strict")) {
			t.Errorf("%s: refused %v", c.name, err)
		}
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

// TestTheGateRefusesFromBehindLooseProxies: through the gate itself, a
// Caddyfile with a from list behind trusted_proxies without
// trusted_proxies_strict stops validate and reload, naming the remedy.
func TestTheGateRefusesFromBehindLooseProxies(t *testing.T) {
	config, _ := dirs(t)
	file := config + "/Caddyfile"
	for _, c := range []struct {
		strict string
		stop   bool
	}{{"", true}, {"\t\ttrusted_proxies_strict\n", false}} {
		write(t, file, "{\n\tadmin off\n\tservers {\n\t\ttrusted_proxies static 10.0.0.0/8\n"+c.strict+"\t}\n\tbox {\n\t\tdeploy_trust local {\n\t\t\tpublic_key /k\n\t\t}\n\t\tsigner ops ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJTxbCB1QdiskXvEbkxkjl5p09A5ZsEN9HC/xOJOxi8j\n\t\tgrant ops viewer box from 198.51.100.0/24\n\t}\n}\n\n:8080 {\n\trespond ok\n}\n", 0o644)
		for _, cmd := range []string{"validate", "reload"} {
			var stderr bytes.Buffer
			stop, _ := gateArgs([]string{cmd, "--config", file}, &stderr)
			if stop != c.stop || (c.stop && !strings.Contains(stderr.String(), "add trusted_proxies_strict")) {
				t.Errorf("%s, strict %q: stopped %v: %s", cmd, c.strict, stop, stderr.String())
			}
		}
	}
}
