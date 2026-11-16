// motelctl, the laptop client: reads a box over HTTPS with a token the
// person's own ssh key signs. Its own module so that it links no
// Caddy: it takes the box's wire types from liveswap/wire, which
// imports only the standard library, and the tests that run it against
// the real handlers live in the liveswap and box modules.
module github.com/smallhoursorg/motel/motelctl

// The Go toolchain is pinned by the golang image in docker-compose.yml —
// no toolchain directive needed here.
go 1.27.1

// Built only from this repo, like the product module: wire is a
// package of the liveswap module next door.
replace github.com/smallhoursorg/motel/liveswap => ../liveswap

require (
	github.com/gdamore/tcell/v2 v2.13.10
	github.com/rivo/tview v0.42.0
	github.com/smallhoursorg/motel/liveswap v0.0.0-00010101000000-000000000000
	github.com/spf13/cobra v1.10.2
	golang.org/x/crypto v0.58.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/gdamore/encoding v1.0.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/spf13/pflag v1.1.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/telemetry v0.0.0-20260924152758-ed294f943157 // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
)

// govulncheck is a tool dependency (never compiled into the product,
// invisible to importers) so that Dependabot bumps it like any require:
// the Makefile invokes `go tool govulncheck` with no version literal.
tool golang.org/x/vuln/cmd/govulncheck
