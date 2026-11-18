// The backup engine: reads what each app declares (liveswap/backupdecl),
// and runs sqlite3 and restic in sandboxed transient system units. Its
// own module so the program that starts those units — root-equivalent
// by definition — links neither Caddy nor anything Caddy depends on.
module github.com/smallhoursorg/motel/backups

go 1.27.1

require (
	github.com/coreos/go-systemd/v22 v22.7.0
	github.com/godbus/dbus/v5 v5.2.4
	github.com/smallhoursorg/motel/liveswap v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.50.0
)

require (
	github.com/google/go-cmp v0.7.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/telemetry v0.0.0-20261009151841-97e8ea77aa2a // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
)

// Built only from this repo, like the product module: backupdecl is a
// package of the liveswap module next door.
replace github.com/smallhoursorg/motel/liveswap => ../liveswap

// govulncheck is a tool dependency (never compiled into the product,
// invisible to importers) so that Dependabot bumps it like any require:
// the Makefile invokes `go tool govulncheck` with no version literal.
tool golang.org/x/vuln/cmd/govulncheck
