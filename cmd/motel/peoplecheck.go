package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// peopleConfig is what the gate reads of an adapted config about
// people: the box block's lines, whether the monitor block has a
// channel, and each server's trusted proxies.
type peopleConfig struct {
	Apps struct {
		Box *struct {
			People []json.RawMessage `json:"people"`
			Grants []struct {
				From []string `json:"from"`
			} `json:"grants"`
		} `json:"box"`
		Monitor struct {
			Channels []json.RawMessage `json:"channels"`
		} `json:"monitor"`
		HTTP struct {
			Servers map[string]struct {
				TrustedProxies       json.RawMessage `json:"trusted_proxies"`
				TrustedProxiesStrict int             `json:"trusted_proxies_strict"`
			} `json:"servers"`
		} `json:"http"`
	} `json:"apps"`
}

// readPeople is cfg's people, and whether it declares any (a block of
// signers alone declares no one).
func readPeople(cfg []byte) (c peopleConfig, declares bool) {
	if json.Unmarshal(cfg, &c) != nil || c.Apps.Box == nil {
		return c, false
	}
	return c, len(c.Apps.Box.People) > 0 || len(c.Apps.Box.Grants) > 0
}

// proxies says whether a grant has a from list and a server has
// trusted_proxies, and whether every such server is strict.
func (c peopleConfig) proxies() (fromBehindProxies, strict bool) {
	from := false
	for _, g := range c.Apps.Box.Grants {
		from = from || len(g.From) > 0
	}
	trusting, strict := false, true
	for _, s := range c.Apps.HTTP.Servers {
		if len(s.TrustedProxies) > 0 && string(s.TrustedProxies) != "null" {
			trusting = true
			strict = strict && s.TrustedProxiesStrict > 0
		}
	}
	return from && trusting, strict
}

// checkPeople refuses, at validate, reload and run, a grant's address
// list on a box with a server that trusts proxies without
// trusted_proxies_strict: Caddy's client_ip is then the left-most
// address in X-Forwarded-For, which the client chooses, and the list
// would hold nothing. The serving process fails closed on its own
// (liveswap's clientAddr), for a config this gate never saw.
func checkPeople(cfg []byte) error {
	c, declares := readPeople(cfg)
	if !declares {
		return nil
	}
	if behind, strict := c.proxies(); behind && !strict {
		return errors.New("box: " + access.FromProxiesRefusal)
	}
	return nil
}

// warnPeople says, at validate, what the box module's own reading of
// its block cannot see, because it sees the block alone: people with
// no monitor channel to tell of them, and a grant's address list
// behind strictly trusted proxies, whose ranges then decide the
// address. The serving process says the first at each load too, from
// the installed file (box's loadRegistry).
func warnPeople(cfg []byte, stderr io.Writer) {
	c, declares := readPeople(cfg)
	if !declares {
		return
	}
	if len(c.Apps.Monitor.Channels) == 0 {
		_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.NoChannelWarning)
	}
	if behind, strict := c.proxies(); behind && strict {
		_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.FromProxiesWarning)
	}
}
