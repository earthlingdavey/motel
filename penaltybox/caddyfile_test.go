package penaltybox

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy" // registers the reverse_proxy directive
)

func TestCaddyfileUnmarshalFullBlock(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		header      X-Custom-Level
		key         {http.request.header.X-Client}
		min_level   3
		window      10s
		limit       12
		penalty_ttl 1m
		strip       false
		status      503
		max_keys    5000
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.Header != "X-Custom-Level" {
		t.Errorf("header = %q", h.Header)
	}
	if h.Key != "{http.request.header.X-Client}" {
		t.Errorf("key = %q", h.Key)
	}
	if h.MinLevel != 3 {
		t.Errorf("min_level = %d", h.MinLevel)
	}
	if time.Duration(h.Window) != 10*time.Second {
		t.Errorf("window = %v", time.Duration(h.Window))
	}
	if h.Limit != 12 {
		t.Errorf("limit = %d", h.Limit)
	}
	if time.Duration(h.PenaltyTTL) != time.Minute {
		t.Errorf("penalty_ttl = %v", time.Duration(h.PenaltyTTL))
	}
	if h.Strip == nil || *h.Strip {
		t.Error("strip should be explicitly false")
	}
	if h.Status != 503 {
		t.Errorf("status = %d", h.Status)
	}
	if h.MaxKeys != 5000 {
		t.Errorf("max_keys = %d", h.MaxKeys)
	}
}

func TestCaddyfileUnmarshalEmptyBlockLeavesDefaultsToProvision(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	// All zero — Provision applies the documented defaults.
	if h.Header != "" || h.Key != "" || h.MinLevel != 0 || h.Window != 0 ||
		h.Limit != 0 || h.PenaltyTTL != 0 || h.Strip != nil || h.Status != 0 || h.MaxKeys != 0 {
		t.Errorf("empty directive must leave zero values, got %+v", h)
	}
}

func TestCaddyfileBareStripMeansTrue(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		strip
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.Strip == nil || !*h.Strip {
		t.Error("bare strip should mean true")
	}
}

func TestCaddyfileWindowSupportsDayUnits(t *testing.T) {
	// caddy.ParseDuration supports "d" — a reason we don't use
	// time.ParseDuration directly.
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		penalty_ttl 1d
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(h.PenaltyTTL) != 24*time.Hour {
		t.Errorf("penalty_ttl = %v, want 24h", time.Duration(h.PenaltyTTL))
	}
}

func TestCaddyfileErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"positional arg", `hint_penaltybox extra`},
		{"unknown subdirective", `hint_penaltybox {
			bogus 1
		}`},
		{"bad duration", `hint_penaltybox {
			window banana
		}`},
		{"bad min_level", `hint_penaltybox {
			min_level two
		}`},
		{"bad limit", `hint_penaltybox {
			limit many
		}`},
		{"bad strip", `hint_penaltybox {
			strip maybe
		}`},
		{"missing arg", `hint_penaltybox {
			header
		}`},
		{"extra arg", `hint_penaltybox {
			limit 1 2
		}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h Handler
			if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(c.input)); err == nil {
				t.Error("expected parse error")
			}
		})
	}
}

func TestProvisionAppliesDefaults(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	var h Handler
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	}()

	if h.Header != "X-Rate-Limit-Level" || h.Key != "{http.vars.client_ip}" || h.MinLevel != 2 ||
		time.Duration(h.Window) != time.Minute || h.Limit != 30 ||
		time.Duration(h.PenaltyTTL) != 5*time.Minute || h.Status != 429 || h.MaxKeys != 100_000 {
		t.Errorf("unexpected defaults: %+v", h)
	}
	if !h.stripOn {
		t.Error("strip should default to on")
	}
	if err := h.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

// TestCaddyfileDirectiveOrder pins where Caddy's directive sorting puts
// hint_penaltybox relative to the reverse_proxy it watches, as the
// README's Caddyfile section documents. The directive is registered
// Before reverse_proxy (caddyfile.go), but Caddy's default order puts
// handle, handle_path and route earlier still. So at site level
// hint_penaltybox sorts after such a block, and the reverse_proxy in
// it answers without calling the next handler: the module never runs.
// Inside a handle block it sorts before the proxy, whatever the source
// order; inside a route block the written order holds. (Where it sorts
// against Souin's cache is pinned by the motel build, which compiles
// cache in: cmd/moteld.)
func TestCaddyfileDirectiveOrder(t *testing.T) {
	cases := []struct {
		name string
		site string
		// wantFirst: hint_penaltybox runs before reverse_proxy.
		wantFirst bool
	}{
		{"site level beside reverse_proxy", `
	reverse_proxy localhost:8000
	hint_penaltybox`, true},
		{"site level beside handle", `
	hint_penaltybox
	handle {
		reverse_proxy localhost:8000
	}`, false},
		{"site level beside handle_path", `
	hint_penaltybox
	handle_path /api/* {
		reverse_proxy localhost:8000
	}`, false},
		{"site level beside route", `
	hint_penaltybox
	route {
		reverse_proxy localhost:8000
	}`, false},
		{"inside handle", `
	handle {
		reverse_proxy localhost:8000
		hint_penaltybox
	}`, true},
		{"inside route, first", `
	route {
		hint_penaltybox
		reverse_proxy localhost:8000
	}`, true},
		// The written order holds inside route even when sorting would
		// put hint_penaltybox first: route is positional, not sorted.
		{"inside route, proxy first", `
	route {
		reverse_proxy localhost:8000
		hint_penaltybox
	}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			order := adaptedHandlerOrder(t, "example.com {"+c.site+"\n}\n")
			box := slices.Index(order, "hint_penaltybox")
			proxy := slices.Index(order, "reverse_proxy")
			if box < 0 || proxy < 0 {
				t.Fatalf("handler order %v lacks hint_penaltybox or reverse_proxy", order)
			}
			if got := box < proxy; got != c.wantFirst {
				t.Errorf("handler order %v: hint_penaltybox before reverse_proxy = %v, want %v", order, got, c.wantFirst)
			}
		})
	}
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
// the order Caddy chains them when every matcher matches: routes in
// list order, a subroute's routes in place (a subroute compiles its
// routes in front of the next handler). It ignores matchers, route
// groups (which make handle blocks mutually exclusive), terminal flags
// and error routes, so it answers only "which handler comes first in
// the chain for a request every matcher matches", for one-site
// Caddyfiles.
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
