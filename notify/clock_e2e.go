//go:build e2e

package notify

// clockDiv, in the motel built for the metrics suite's box alone
// (e2e/Dockerfile, metrics-builder): its minutes are ten seconds, so the
// suite waits out each push, evaluation, re-read and group_wait in a
// sixth of the time (DESIGN-metrics.md, "e2e rows"). Six keeps the
// ratios between the waits, and two samples in a minute's wait as the
// shipped timings have: ten seconds over a five-second push or scrape.
// The windows a rule or a view reads history over are not divided, so
// their ratios to the waits are not kept. `make build` refuses a motel
// built with it.
const clockDiv = 6
