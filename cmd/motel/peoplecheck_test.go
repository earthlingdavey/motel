package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// TestWarnPeople: validate says what the box block's own reading cannot
// see. People with no monitor channel are warned of, a block of signers
// alone is not, and a monitor block with a channel quiets it. A grant's
// from list beside a server that trusts proxies (by range or on its
// unix socket) is refused unless that server sets
// trusted_proxies_strict, and warned of, naming the headers the
// address comes from, when it does; a server taking the PROXY protocol
// is warned of. Two servers, or two grants, are each decided by any
// one of them, in either order.
func TestWarnPeople(t *testing.T) {
	const (
		people  = `"people":[{"name":"bob","type":"ssh-ed25519","key":"x"}]`
		grant   = `{"name":"bob","role":"viewer","scope":"box"}`
		from    = `{"name":"bob","role":"viewer","scope":"app","app":"a","from":["198.51.100.0/24"]}`
		channel = `"monitor":{"channels":[{"name":"ops","kind":"ntfy","url":"{secret.NOTIFY_OPS_URL}"}],"operators":["ops"]}`
		loose   = `{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]}}`
		strict  = `{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]},"trusted_proxies_strict":1}`
		realIP  = `{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]},"trusted_proxies_strict":1,"client_ip_headers":["X-Real-IP"]}`
		unix    = `{"trusted_proxies_unix":true}`
		unixS   = `{"trusted_proxies_unix":true,"trusted_proxies_strict":1}`
		minus   = `{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]},"trusted_proxies_strict":-1}`
		unixM   = `{"trusted_proxies_unix":true,"trusted_proxies_strict":-1}`
		two     = `{"trusted_proxies":{"source":"static","ranges":["10.0.0.0/8"]},"trusted_proxies_strict":2}`
		null    = `{"trusted_proxies":null}`
		noTrust = `{"trusted_proxies_strict":1}`
		pp      = `{"listener_wrappers":[{"wrapper":"proxy_protocol","allow":["10.0.0.0/8"]},{"wrapper":"tls"}]}`
		plain   = `{}`
	)
	cfg := func(parts ...string) []byte {
		return []byte(`{"apps":{` + strings.Join(parts, ",") + `}}`)
	}
	box := func(grants ...string) string {
		return `"box":{"signers":[{"principal":"ops","type":"ssh-ed25519","key":"x"}],` + people + `,"grants":[` + strings.Join(grants, ",") + `]}`
	}
	signers := `"box":{"signers":[{"principal":"ops","type":"ssh-ed25519","key":"x"}]}`
	servers := func(s ...string) string {
		out := []string{}
		for i, v := range s {
			out = append(out, fmt.Sprintf(`"srv%d":%s`, i, v))
		}
		return `"http":{"servers":{` + strings.Join(out, ",") + `}}`
	}
	for _, c := range []struct {
		name    string
		cfg     []byte
		channel bool
		refuse  bool
		warn    []string // fragments a warning must hold
	}{
		{"no box block", cfg(servers(loose)), false, false, nil},
		{"signers alone", cfg(signers, servers(loose)), false, false, nil},
		{"a person, no monitor block", cfg(box(), servers(plain)), true, false, nil},
		{"a grant, no monitor block", cfg(box(grant), servers(plain)), true, false, nil},
		{"a person and a channel", cfg(box(), channel, servers(plain)), false, false, nil},
		{"a from list behind trusted proxies, not strict", cfg(box(from), channel, servers(loose)), false, true, nil},
		{"a from list behind strictly trusted proxies", cfg(box(from), channel, servers(strict)), false, false, []string{"server srv0", "in X-Forwarded-For"}},
		{"a from list, strict, another header", cfg(box(from), channel, servers(realIP)), false, false, []string{"in X-Real-IP", "sets each of those headers itself"}},
		{"a from list behind a unix socket's proxy, not strict", cfg(box(from), channel, servers(unix)), false, true, nil},
		{"a from list behind a unix socket's proxy, strict", cfg(box(from), channel, servers(unixS)), false, false, []string{"trusts proxies with trusted_proxies_strict"}},
		{"a from list on a PROXY protocol listener", cfg(box(from), channel, servers(pp)), false, false, []string{"server srv0 takes the PROXY protocol", "allow list"}},
		{"a from list, no trusted proxies", cfg(box(from), channel, servers(plain)), false, false, nil},
		{"trusted proxies, no from list", cfg(box(grant), channel, servers(loose)), false, false, nil},
		{"a strict server, then a loose one", cfg(box(from), channel, servers(strict, loose)), false, true, []string{"server srv0 trusts proxies"}},
		{"a loose server, then a strict one", cfg(box(from), channel, servers(loose, strict)), false, true, []string{"server srv1 trusts proxies"}},
		{"a from list, then a grant without one", cfg(box(from, grant), channel, servers(loose)), false, true, nil},
		{"strict as -1, which Caddy reads as not strict", cfg(box(from), channel, servers(minus)), false, true, nil},
		{"a unix socket's proxy, strict as -1", cfg(box(from), channel, servers(unixM)), false, true, nil},
		{"strict as 2, which Caddy reads as strict", cfg(box(from), channel, servers(two)), false, false, []string{"server srv0 trusts proxies"}},
		{"trusted_proxies null", cfg(box(from), channel, servers(null)), false, false, nil},
		{"strict on a server that trusts no proxy", cfg(box(from), channel, servers(noTrust)), false, false, nil},
		{"strict and PROXY protocol servers, no from list", cfg(box(grant), channel, servers(strict, pp)), false, false, nil},
		{"a grant without a from list, then one with", cfg(box(grant, from), channel, servers(loose)), false, true, nil},
	} {
		err := checkPeople(c.cfg)
		if (err != nil) != c.refuse || (err != nil && !strings.Contains(err.Error(), "add trusted_proxies_strict")) {
			t.Errorf("%s: refused %v", c.name, err)
		}
		var stderr bytes.Buffer
		warnPeople(c.cfg, &stderr)
		if got := strings.Contains(stderr.String(), access.NoChannelWarning); got != c.channel {
			t.Errorf("%s: no-channel warning %v: %s", c.name, got, stderr.String())
		}
		for _, w := range c.warn {
			if !strings.Contains(stderr.String(), w) {
				t.Errorf("%s: no warning saying %q: %s", c.name, w, stderr.String())
			}
		}
		if len(c.warn) == 0 && strings.Contains(stderr.String(), "a grant has a from list") {
			t.Errorf("%s: warned %s", c.name, stderr.String())
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
