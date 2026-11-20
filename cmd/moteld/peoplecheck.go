package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// peopleConfig is what the gate reads of an adapted config about
// people: the box block's lines, whether the monitor block has a
// channel, and how each server learns a client's address.
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
			Servers map[string]addressSource `json:"servers"`
		} `json:"http"`
	} `json:"apps"`
}

// addressSource is what a server's client_ip is made of.
type addressSource struct {
	TrustedProxies       json.RawMessage `json:"trusted_proxies"`
	TrustedProxiesStrict int             `json:"trusted_proxies_strict"`
	TrustedProxiesUnix   bool            `json:"trusted_proxies_unix"`
	ClientIPHeaders      []string        `json:"client_ip_headers"`
	ListenerWrappers     []struct {
		Wrapper string `json:"wrapper"`
	} `json:"listener_wrappers"`
}

// trusts reports whether the server takes a client's address from a
// header a peer sends: it trusts proxies by range or on its unix socket.
func (s addressSource) trusts() bool {
	return s.TrustedProxiesUnix || (len(s.TrustedProxies) > 0 && string(s.TrustedProxies) != "null")
}

// headers are the headers the server reads a client's address from:
// Caddy's default when the config names none.
func (s addressSource) headers() []string {
	if len(s.ClientIPHeaders) == 0 {
		return []string{"X-Forwarded-For"}
	}
	return s.ClientIPHeaders
}

func (s addressSource) proxyProtocol() bool {
	return slices.ContainsFunc(s.ListenerWrappers, func(w struct {
		Wrapper string `json:"wrapper"`
	}) bool {
		return w.Wrapper == "proxy_protocol"
	})
}

// readPeople is cfg's people, whether it declares any (a block of
// signers alone declares no one), and whether any grant has a from
// list.
func readPeople(cfg []byte) (c peopleConfig, declares, from bool) {
	if json.Unmarshal(cfg, &c) != nil || c.Apps.Box == nil {
		return c, false, false
	}
	for _, g := range c.Apps.Box.Grants {
		from = from || len(g.From) > 0
	}
	return c, len(c.Apps.Box.People) > 0 || len(c.Apps.Box.Grants) > 0, from
}

// servers are the config's servers, by name, in order.
func (c peopleConfig) servers() []string {
	return slices.Sorted(maps.Keys(c.Apps.HTTP.Servers))
}

// checkPeople refuses, at validate, reload and run, a grant's address
// list on a box with any server that trusts proxies without
// trusted_proxies_strict: Caddy's client_ip is then the left-most
// address in the client IP header, which the client chooses, and the
// list would hold nothing. The serving process fails closed on its own
// (liveswap's clientAddr), for a config this gate never saw.
func checkPeople(cfg []byte) error {
	c, declares, from := readPeople(cfg)
	if !declares || !from {
		return nil
	}
	for _, name := range c.servers() {
		if s := c.Apps.HTTP.Servers[name]; s.trusts() && s.TrustedProxiesStrict <= 0 { // Caddy's own test of strict is > 0
			return errors.New("box: " + access.FromProxiesRefusal)
		}
	}
	return nil
}

// warnPeople says, at validate, what the box module's own reading of
// its block cannot see, because it sees the block alone: people with
// no monitor channel to tell of them; a server whose forwarded requests
// are all one network to the record of where each key was used (it
// trusts proxies without trusted_proxies_strict); and each server whose
// client's address comes from something a peer sends, strictly trusted
// proxies (the headers named) and the PROXY protocol: for a grant's
// address list when one has a list, and otherwise for the record of
// networks alone. The serving process says the first at each load too,
// from the installed file (box's loadRegistry).
func warnPeople(cfg []byte, stderr io.Writer) {
	c, declares, from := readPeople(cfg)
	if !declares {
		return
	}
	if len(c.Apps.Monitor.Channels) == 0 {
		_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.NoChannelWarning)
	}
	for _, name := range c.servers() {
		if s := c.Apps.HTTP.Servers[name]; s.trusts() && s.TrustedProxiesStrict <= 0 { // Caddy's own test of strict is > 0
			_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.ForwardedNetworksWarning(name))
		}
	}
	for _, name := range c.servers() {
		s := c.Apps.HTTP.Servers[name]
		strict := s.trusts() && s.TrustedProxiesStrict > 0
		switch {
		case from && strict:
			_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.FromProxiesWarning(name, s.headers()))
		case strict:
			_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.NetworksFromHeadersWarning(name, s.headers()))
		}
		switch {
		case from && s.proxyProtocol():
			_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.FromProxyProtocolWarning(name))
		case s.proxyProtocol():
			_, _ = fmt.Fprintf(stderr, "WARNING: box: %s\n", access.NetworksFromProxyProtocolWarning(name))
		}
	}
}
