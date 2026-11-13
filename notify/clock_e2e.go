//go:build e2e

package notify

// clockDiv, in the motel built for the metrics suite's box alone
// (e2e/Dockerfile, metrics-builder): its minutes are ten seconds, so the
// suite waits out each push, evaluation, re-read and group_wait in a
// sixth of the time (DESIGN-metrics.md, "e2e rows"). Six keeps every
// ratio between the clocks, and two samples in each window as the
// shipped timings have: a minute's window over a 30 s push or scrape is
// ten seconds over five. No package build carries this tag.
const clockDiv = 6
