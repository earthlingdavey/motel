package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// warnPeople says, at validate, what the box module's own reading of
// its block cannot see, because it sees the block alone: people with
// no monitor channel to tell of them, and a grant's address list on a
// server that trusts proxies, whose X-Forwarded-For then decides the
// address the list is held to. The serving process says the first at
// each load too, from the installed file (box's loadRegistry).
func warnPeople(cfg []byte, stderr io.Writer) {
	var c struct {
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
					TrustedProxies json.RawMessage `json:"trusted_proxies"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if json.Unmarshal(cfg, &c) != nil || c.Apps.Box == nil {
		return
	}
	box := c.Apps.Box
	if len(box.People) == 0 && len(box.Grants) == 0 {
		return // a block of signers alone declares no one
	}
	if len(c.Apps.Monitor.Channels) == 0 {
		_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.NoChannelWarning)
	}
	from := false
	for _, g := range box.Grants {
		from = from || len(g.From) > 0
	}
	proxies := false
	for _, s := range c.Apps.HTTP.Servers {
		proxies = proxies || (len(s.TrustedProxies) > 0 && string(s.TrustedProxies) != "null")
	}
	if from && proxies {
		_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.FromProxiesWarning)
	}
}
