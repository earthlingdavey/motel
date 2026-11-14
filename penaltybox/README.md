# penaltybox

A Caddy v2 module that turns an origin's **rate-limit hint header** into
edge-side throttling using the classic **penalty box** pattern: every
origin response labeled `X-Rate-Limit-Level: 2` or `3` counts against a
per-client sliding-window budget (by default weighted, so a level-3
response costs 3 units); a client that exceeds the budget is put in a
penalty box and gets `429` + `Retry-After` — before its requests reach
the origin — until the box expires. The hint header is stripped before
the response reaches the client.

If you know [Fastly's penalty boxes][fastly-concepts] or HAProxy's
[stick tables][haproxy-docs], you already understand this module — it is
the deliberate Caddy counterpart to those recipes, with matching
vocabulary. Caddy needs a compiled-in module for this because it has no
edge scripting runtime, and existing rate-limit plugins are
request-side only: they cannot see an **origin response** header.

## Concept map

| Concept            | Fastly ERL                                   | HAProxy                                       | `penaltybox`             |
| ------------------ | -------------------------------------------- | --------------------------------------------- | ----------------------------------- |
| Per-client counter | `ratecounter` declaration                    | stick-table `store gpc0,gpc0_rate(60s)`       | in-memory sliding-window counter    |
| Count on response  | [`ratelimit.check_rate`][fastly-check-rate] in `vcl_fetch` | `http-response sc-inc-gpc0(0) if { ... }`     | ResponseWriter shim after `next`    |
| Weighted increment | `delta` parameter = level                    | not supported (increments by 1)               | `delta = level` (Fastly-style) on the default budget; 1 per response in a `tier` |
| Penalty box        | [`penaltybox` declaration][fastly-penaltybox] + TTL | modeled via rate threshold on the table       | boxed map with per-entry TTL        |
| Enforce on request | [`ratelimit.penaltybox_has`][fastly-pb-has] in `vcl_recv` | `http-request deny if { sc0_gpc0_rate gt N }` | box check at top of `ServeHTTP`     |
| Client key         | `client.ip` (or any entry string)            | `track-sc0 src`                               | `{client_ip}` placeholder (default) |
| Strip the header   | `unset resp.http.X-Rate-Limit-Level`         | `http-response del-header`                    | module strips by default            |
| Window constraint  | 1, 10, or 60 seconds                         | arbitrary `gpc0_rate(period)`                 | free-form; default 60s              |
| Penalty TTL        | 1m–1h, minute granularity                    | table `expire`                                | free-form; default 5m               |
| Box re-offense     | TTL fixed once boxed                         | effectively extends while rate stays high     | TTL fixed once boxed (Fastly-style) |
| Clustered state    | platform-global                              | stick-table peers protocol                    | per-instance (see Trade-offs)       |

## The wire contract

The module consumes a response header (default `X-Rate-Limit-Level`)
with the strict value set `"1"`, `"2"`, or `"3"` — the **recommended
throttle strictness** of the response, never enforcement:

- **Absent header, or any other value (garbage, `"0"`, `"4"`, padded,
  multi-valued) = level 1.** Malformed input never errors, and it can
  never raise a response's level: like an absent header, it counts
  only when `min_level` is 1.
- Levels at or above `min_level` (default 2) count against the
  client's budget. On the default budget a response adds `level`
  units, so a level-3 login attempt costs 3; in a
  [`tier`](#per-tier-budgets) each response adds 1. Levels below
  `min_level` (level 1 by default) cost nothing and allocate nothing.

Any application can emit this header; the module is not specific to any
CMS. It pairs with an app that labels sensitive routes (logins, presign
endpoints, expensive queries) with higher levels.

## Install

```sh
xcaddy build --with github.com/smallhoursorg/motel/penaltybox
```

Or with the Caddy builder image:

```dockerfile
FROM caddy:2.11.4-builder AS builder
RUN xcaddy build --with github.com/smallhoursorg/motel/penaltybox

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

## Caddyfile

```caddyfile
example.com {
	route {
		hint_penaltybox {
			header      X-Rate-Limit-Level  # default; the wire contract
			key         {client_ip}         # default
			min_level   2                   # default; ignore level-1 responses
			window      60s                 # sliding window
			limit       30                  # weighted units per window before boxing
			penalty_ttl 5m                  # box duration (fixed, never extended)
			strip       true                # default
			status      429                 # default
			max_keys    100000              # default; tracked-client cap
		}
		reverse_proxy localhost:8000
	}
}
```

Put `hint_penaltybox` first in a `route` block, as above. That is the
one placement that always works: inside `route` Caddy keeps the
written order, so the module sees every request before `cache` or the
proxy can answer it. With `handle` blocks, put the `route` inside the
`handle`.

Anywhere else (inside a `handle` block too), Caddy sorts directives
by its directive order. This one sorts just before `reverse_proxy`,
which puts it after `cache` (Souin registers `cache` before
`rewrite`) and after any `handle`, `handle_path` or `route` block:

- Beside `cache`, a cache hit is answered before the module runs, so
  hits are neither counted nor box-checked (see
  [Compatibility with Souin](#compatibility-with-souin-http-cache)).
- At site level beside `handle { reverse_proxy ... }`, a request that
  block handles reaches its proxy first, and the proxy answers without
  calling the next handler: the module never runs, nothing is counted
  or boxed, and the hint reaches the client unstripped.

All options and defaults:

| Option        | Default              | Meaning                                                              |
| ------------- | -------------------- | -------------------------------------------------------------------- |
| `header`      | `X-Rate-Limit-Level` | Origin response header carrying the hint level                       |
| `key`         | `{client_ip}`        | Client identity; respects the server's `trusted_proxies` config. A key whose whole value is one IPv6 address counts under its /64; IPv4 (also mapped or NAT64 well-known) per address; anything else verbatim (see [Client keys](#client-keys-ipv6-by-64)). A key that resolves to an empty string fails open: the request is not counted or boxed, but its hint header is still stripped when `strip` is on (see [Semantics](#semantics-and-trade-offs-read-this)) |
| `min_level`   | `2`                  | Lowest level that counts toward the budget (1–3)                     |
| `window`      | `60s`                | Sliding window; free-form duration (Fastly's 1s/10s/60s is the interoperability convention) |
| `limit`       | `30`                 | Weighted units per window; *exceeding* (not reaching) it boxes       |
| `penalty_ttl` | `5m`                 | Box duration; Fastly allows 1m–1h — mirror that range for doc parity |
| `strip`       | `true`               | Remove the hint header from every final response before the client sees it. A hint sent as a trailer or on a 1xx interim response is neither stripped nor counted, so send it as a header |
| `status`      | `429`                | Status for boxed clients (4xx/5xx)                                   |
| `max_keys`    | `100000`             | Cap on tracked clients, split evenly across 64 shards (rounded down, at least 1 each); a full shard evicts its oldest-idle unboxed client, or its oldest-idle client outright when all are boxed. Below 128 (one slot per shard) it loads with a warning (see [Semantics](#semantics-and-trade-offs-read-this)) |

### Per-tier budgets

A single `(window, limit)` pair cannot express a two-tier policy like
"5 login attempts per 15 minutes" *and* "30 elevated operations per
minute" — tuning for one strangles the other. `tier` blocks give each
hint level its own budget:

```caddyfile
hint_penaltybox {
	tier 3 {
		window      15m
		limit       5     # five level-3 responses — logins, say
		penalty_ttl 30m   # security offenses earn longer boxes
	}
	tier 2 {
		window      60s
		limit       30
		penalty_ttl 5m
	}
}
```

Semantics:

- **Within a tier, one response costs 1**, whatever its level —
  `limit 5` means five level-3 responses, not weighted units. That
  holds for levels a tier takes by fallback (below) too: with only
  `tier 2` configured, a level-3 response costs 1 there, where the
  default budget would charge 3.
- **Budgets are independent.** Each counted response goes to exactly
  one budget: its level's tier, else the fallback below. With tiers 2
  and 3 both configured, level-2 traffic never consumes tier 3's
  budget, and vice versa — this is the point of the feature.
- **Fallback:** a counted level without its own tier uses the nearest
  configured tier below it (a level-3 response is at least as sensitive
  as level 2); if none, the default top-level budget, which keeps the
  original weighted semantics.
- **Boxing is whole-client:** crossing any tier's limit boxes the key
  for all traffic, with that tier's `penalty_ttl`. `Retry-After` is the
  remaining time of the longest active box.
- **Omitted tier fields inherit** the top-level `window`, `limit`, and
  `penalty_ttl`.
- Configs without `tier` blocks behave exactly as before.

The vendor parallel holds: Fastly expresses this as multiple
`ratecounter`/`penaltybox` pairs, HAProxy as separate `gpc` counters.

JSON config uses the same fields under `http.handlers.hint_penaltybox`
(tiers keyed by level):

```json
{
	"handler": "hint_penaltybox",
	"min_level": 2,
	"window": "60s",
	"limit": 30,
	"penalty_ttl": "5m",
	"tiers": {
		"3": { "window": "15m", "limit": 5, "penalty_ttl": "30m" }
	}
}
```

Do **not** key on a raw `X-Forwarded-For` read: the default
`{client_ip}` already honors the server's
[`trusted_proxies`](https://caddyserver.com/docs/caddyfile/options#trusted-proxies)
configuration, which is where XFF trust belongs.

## Semantics and trade-offs (read this)

- **Reactive by design.** A client gets a budget's worth of full-cost
  requests before the box closes. Sustained abuse (credential stuffing,
  presign farming) is the threat this addresses — not the first hit.
- **Boxing is strict-greater.** With `limit 30`, thirty units is still
  allowed; the response that pushes past 30 gets through (its hint is
  what triggers boxing) and the *next* request is rejected.
- **`Retry-After` is honest**: the ceiling of the *remaining* box
  seconds, not the configured TTL.
- **The box TTL is fixed** (Fastly semantics). Traffic during the box
  neither counts nor extends the penalty. The budget that boxed the
  client restarts from zero; with `tier` blocks, the client's other
  budgets keep their windows, which go on sliding through the box.
- **State is per-instance and in-memory.** N Caddy instances ≈ N× the
  effective threshold. A config reload resets counters and boxes
  (fails open). Distributed state is a possible future addition — the
  counter store sits behind a small interface for exactly that reason.
- **Memory is hard-bounded.** `max_keys` is split evenly across 64
  shards, rounded down with a minimum of 1 per shard, so at most
  64 × max(⌊`max_keys`/64⌋, 1) clients are tracked: 99,968 for the
  default `100000`, 960 for `1000`, and 64 for any value below 64. A
  client takes a slot only when it gets a counted response
  (level ≥ `min_level`); an attacker rotating IPs exhausts the cap
  into evictions, not into unbounded memory. To admit a new client a
  full shard first drops unboxed entries idle for longer than the
  longest window, then evicts its oldest-idle unboxed client, whose
  count is lost. An actively boxed client is evicted only when every
  client in its shard is boxed, and that eviction lifts its box. The
  hash that picks a key's shard is seeded at random each time the
  store is built (every config load), so a client cannot choose keys
  that pile into one shard and evict other clients' counters there:
  flushing a given client's count takes on the order of `max_keys`
  new keys, spread across all shards.
- **A small `max_keys` fails open.** The cap is a memory bound, not a
  promise that every client is tracked. Below 128 every shard holds
  one client: when two clients' counted responses interleave in one
  shard, each evicts the other and its count starts again, so an
  abuser sharing a shard with any other active client may never reach
  `limit`, and a box lasts only until another client is counted in
  that shard. With 64 shards, a given client more likely than not
  shares its shard once about 45 clients are counted at once. A value
  below 128 loads (refusing it would fail a config that works) but
  logs a warning. Size `max_keys` well above the number of clients
  that get counted responses within one window.
- **An empty key fails open.** A `key` that resolves to an empty
  string is not counted and never boxed: the request passes to the
  next handler, and with `strip true` its hint header is still
  stripped from the response. With a header-based key such as
  `{http.request.header.CF-Connecting-IP}`, a client that reaches
  Caddy without that header is never limited.
  The default key is the connection's address unless a trusted proxy
  supplies one, so a client cannot empty it by leaving a header out.
  `{client_ip}` is a Caddyfile shorthand: in JSON config write
  `{http.vars.client_ip}` (the default when `key` is omitted), because
  a JSON `"key": "{client_ip}"` is an unknown placeholder that
  resolves to an empty string, and every request fails open.

### Client keys: IPv6 by /64

The whole resolved `key` value is looked at, whatever placeholder
produced it: `{client_ip}`, or a header such as
`{http.request.header.CF-Connecting-IP}`. If it parses as exactly one
IP address it is masked; otherwise it is counted verbatim:

| Resolved value                         | Counted under                    |
| -------------------------------------- | -------------------------------- |
| One IPv6 address (`2001:db8:1:2::a`)   | its /64 (`2001:db8:1:2::/64`)    |
| One IPv4 address (`192.0.2.1`)         | that address                     |
| IPv4-mapped IPv6 (`::ffff:192.0.2.1`)  | the IPv4 address (`192.0.2.1`)   |
| NAT64 well-known prefix (`64:ff9b::192.0.2.1`) | the IPv4 address (`192.0.2.1`) |
| Link-local with a zone (`fe80::1%eth0`) | its /64, zone dropped (`fe80::/64`) |
| Anything else: a composite such as `{client_ip}\|{host}`, a `host:port` such as `{remote}` gives, any other header value | the string, verbatim |

A single IPv6 host is routinely handed a whole /64, so keying per
address would let one client spread its traffic across addresses and
never fill a budget. Keying the /64 means every address in it shares
one budget and one box. An IPv4 client keeps one key whether it
arrives natively or through a translator using the well-known NAT64
prefix (RFC 6052, `64:ff9b::/96`).

The trade-offs:

- **A shared /64 is one client.** A LAN with SLAAC (an office, a home
  network) shares one /64 the way the same LAN behind NAT shares one
  IPv4 address, and a provider that hands each customer a /128 out of
  a shared /64 puts those customers behind one key: one abusive
  neighbour boxes the others for `penalty_ttl`.
- **Many IPv4 clients can share one /64.** A NAT64 translator using a
  network-specific prefix (anything other than `64:ff9b::/96`,
  including the local-use `64:ff9b:1::/48`) and Teredo
  (`2001::/32`) cannot be told apart from ordinary IPv6, so every
  IPv4 client behind them lands in the translator's /64.
- **A bigger allocation still buys more budgets.** A client holding a
  /56 has 256 /64s, so 256 budgets; a /48 has 65,536. Per /64 closes
  the per-address hole, not this one, and `max_keys` bounds the memory
  either way.
- **The /64 is fixed**; there is no option to change it. Only a key
  that resolves to exactly one address is masked, so a composite key
  such as `{client_ip}|{host}` stays per address — build such a key
  only if per-address IPv6 counting is what you want.

## Compatibility with Souin (HTTP cache)

The e2e suite builds and tests the module alongside
[Souin](https://github.com/darkweak/souin) with
[Otter](https://github.com/darkweak/storages) storage. Place
`hint_penaltybox` **before** `cache` in the route:

```caddyfile
route {
	hint_penaltybox { ... }
	cache
	reverse_proxy localhost:8000
}
```

With that order (all verified by `make e2e`):

- Boxed clients get `429` before the cache — no free cached reads while
  boxed.
- Souin stores the upstream response *including* the hint header, so
  **cache hits replay the hint through the module**: hits count toward
  the budget exactly like origin responses, and a client hammering a
  cached level-3 URL still gets boxed.
- The header is stripped from every client-facing response, cache hit
  or miss — it lives only inside the cache store (when the origin sends
  it as a header of the final response, not as a trailer or on a 1xx;
  see `strip`).

(If `cache` runs before `hint_penaltybox`, cache hits bypass the
module entirely: stored responses are already stripped, but boxed
clients can keep reading cached pages and hits never count. Outside a
`route` block that is the order Caddy picks, whatever the written
order: see [Caddyfile](#caddyfile).)

## Development

penaltybox lives in the [motel](../) monorepo; the make targets at
the repo root cover it (no local Go toolchain needed — everything runs
in Docker):

```sh
make test              # unit tests, all modules (race detector, coverage)
make test-integration  # caddytest harness against an in-process Caddy
make e2e               # both module suites against the motel binary (incl. Souin+Otter)
make lint vet tidy
```

CI runs all of these on every PR that changes code, except `tidy`, which writes files and
so is local-only; in its place CI runs `make tidy-check`, which fails if
tidy would change go.mod or go.sum.

## References

- Fastly: [rate-limiting concepts][fastly-concepts],
  [`ratelimit.check_rate`][fastly-check-rate],
  [`penaltybox` declaration][fastly-penaltybox],
  [`ratelimit.penaltybox_has`][fastly-pb-has]
- HAProxy: [docs.haproxy.org][haproxy-docs] (`stick-table`,
  `http-response sc-inc-gpc0`, `sc0_gpc0_rate`)
- Caddy: [Extending Caddy](https://caddyserver.com/docs/extending-caddy),
  [mholt/caddy-ratelimit](https://github.com/mholt/caddy-ratelimit)
  (prior art; request-side only — the gap this module fills),
  [xcaddy](https://github.com/caddyserver/xcaddy)

[fastly-concepts]: https://www.fastly.com/documentation/guides/concepts/rate-limiting/
[fastly-check-rate]: https://www.fastly.com/documentation/reference/vcl/functions/rate-limiting/ratelimit-check-rate/
[fastly-penaltybox]: https://www.fastly.com/documentation/reference/vcl/declarations/penaltybox/
[fastly-pb-has]: https://www.fastly.com/documentation/reference/vcl/functions/rate-limiting/ratelimit-penaltybox-has/
[haproxy-docs]: https://docs.haproxy.org/

## License

Apache-2.0
