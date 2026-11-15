package liveswap

import "github.com/smallhoursorg/motel/liveswap/wire"

// The webhook's bodies are package wire's types, which a client
// decodes into without linking Caddy (liveswap/wire). These are their
// names inside liveswap, as the code and its comments have always
// called them; testdata/wire pins the bytes.
type (
	statusSnapshot   = wire.Status
	capsSnapshot     = wire.Caps
	deploySummary    = wire.DeploySummary
	deployResult     = wire.DeployResult
	phaseTiming      = wire.PhaseTiming
	deployDetail     = wire.DeployDetail
	probeDetail      = wire.ProbeDetail
	watchdogSnapshot = wire.Watchdog
)
