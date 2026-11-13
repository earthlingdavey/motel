//go:build !e2e

package notify

// clockDiv divides notify's clocks: every timing motel waits on, and
// every one it writes into the rules, Alertmanager's config and the
// store's units. 1 here, so every timing is the one documented and the
// rendered files are byte for byte what they would be without it; the
// e2e build's is clock_e2e.go's.
const clockDiv = 1
