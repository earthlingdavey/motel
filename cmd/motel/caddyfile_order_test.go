package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// TestStarterObserveFirst holds the starter's example route to its
// written order with liveswap_observe on: the observer outside the
// cache and penaltybox, so that it counts what they answer too.
func TestStarterObserveFirst(t *testing.T) {
	order := adaptedHandlerOrder(t, starterExampleSite(t))
	got := slices.DeleteFunc(order, func(h string) bool {
		return h != "liveswap_observe" && h != "cache" && h != "hint_penaltybox" && h != "reverse_proxy"
	})
	want := []string{"liveswap_observe", "hint_penaltybox", "cache", "reverse_proxy"}
	if !slices.Equal(got, want) {
		t.Errorf("handler order %v, want %v", got, want)
	}
}

// TestPenaltyboxCacheOrder pins where Caddy's directive sorting puts
// hint_penaltybox against Souin's cache in this build, which compiles
// both in. Souin registers cache before rewrite and penaltybox
// registers hint_penaltybox before reverse_proxy, so wherever Caddy
// sorts (site level, or inside a handle block) cache runs first and a
// cache hit never reaches the penalty box. Inside route the written
// order holds, which is why penaltybox/README.md and the starter
// packaging/Caddyfile put hint_penaltybox first in a route.
func TestPenaltyboxCacheOrder(t *testing.T) {
	site := func(body string) string { return "example.com {" + body + "\n}\n" }
	cases := []struct {
		name  string
		input string
		want  []string // relative order of cache, hint_penaltybox, reverse_proxy
	}{
		{"site level", site(`
	hint_penaltybox
	cache
	reverse_proxy localhost:8000`),
			[]string{"cache", "hint_penaltybox", "reverse_proxy"}},
		{"inside handle", site(`
	handle {
		hint_penaltybox
		cache
		reverse_proxy localhost:8000
	}`),
			[]string{"cache", "hint_penaltybox", "reverse_proxy"}},
		{"inside route", site(`
	route {
		hint_penaltybox
		cache
		reverse_proxy localhost:8000
	}`),
			[]string{"hint_penaltybox", "cache", "reverse_proxy"}},
		{"starter example", starterExampleSite(t),
			[]string{"hint_penaltybox", "cache", "reverse_proxy"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			order := adaptedHandlerOrder(t, c.input)
			got := slices.DeleteFunc(order, func(h string) bool {
				return h != "cache" && h != "hint_penaltybox" && h != "reverse_proxy"
			})
			if !slices.Equal(got, c.want) {
				t.Errorf("handler order %v, want %v", got, c.want)
			}
		})
	}
}

// starterExampleSite returns the shipped starter Caddyfile's commented
// example site (myapp.example.com) as a user gets it by uncommenting
// it with its optional hint_penaltybox and cache lines switched on, so
// the test checks the file itself rather than a copy of it.
func starterExampleSite(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../packaging/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	var site []string
	for _, line := range strings.Split(string(data), "\n") {
		if len(site) == 0 && line != "# myapp.example.com {" {
			continue
		}
		line = strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ")
		// The optional directives are commented once more in the block:
		// "# cache" (with its argument, for liveswap_observe), alone or
		// before a trailing comment, but not prose
		// that happens to start with the word.
		body := strings.TrimLeft(line, "\t")
		for _, d := range []string{"liveswap_observe myapp", "hint_penaltybox", "cache"} {
			rest, ok := strings.CutPrefix(body, "# "+d)
			if ok && (rest == "" || strings.HasPrefix(strings.TrimLeft(rest, " "), "#")) {
				line = line[:len(line)-len(body)] + strings.TrimPrefix(body, "# ")
			}
		}
		site = append(site, line)
		if line == "}" {
			return strings.Join(site, "\n") + "\n"
		}
	}
	t.Fatal("packaging/Caddyfile has no commented-out myapp.example.com site ending in \"# }\"")
	return ""
}

// adaptedRoute and adaptedHandler are the slice of Caddy's HTTP route
// JSON that adaptedHandlerOrder walks.
type adaptedRoute struct {
	Handle []adaptedHandler `json:"handle"`
}

type adaptedHandler struct {
	Handler string         `json:"handler"`
	Routes  []adaptedRoute `json:"routes"`
}

// adaptedHandlerOrder adapts a Caddyfile and lists its HTTP handlers in
// the order Caddy chains them for a request every matcher matches:
// routes in list order, a subroute's routes in place. It ignores
// matchers, route groups, terminal flags and error routes, so it suits
// one-site Caddyfiles only. (penaltybox's caddyfile_test.go has the
// same walker; that module cannot import this one's tests.)
func adaptedHandlerOrder(t *testing.T, input string) []string {
	t.Helper()
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("caddyfile adapter not registered")
	}
	out, _, err := adapter.Adapt([]byte(input), nil)
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []adaptedRoute `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode adapted config: %v", err)
	}
	if n := len(cfg.Apps.HTTP.Servers); n != 1 {
		t.Fatalf("adapted config has %d servers, want 1:\n%s", n, out)
	}
	var order []string
	for _, srv := range cfg.Apps.HTTP.Servers {
		order = appendHandlerOrder(order, srv.Routes)
	}
	return order
}

func appendHandlerOrder(order []string, routes []adaptedRoute) []string {
	for _, r := range routes {
		for _, h := range r.Handle {
			if h.Handler == "subroute" {
				order = appendHandlerOrder(order, h.Routes)
				continue
			}
			order = append(order, h.Handler)
		}
	}
	return order
}
