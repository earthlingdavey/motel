// Package notify is who motel tells, about what, and the box's numbers.
// motel detects; Debian's Alertmanager delivers. The Caddy app
// `monitor`, from the Caddyfile's `monitor` block, renders
// Alertmanager's whole configuration, and motel's detectors (among
// them the watchdog, the deploy handler, the post-deploy comparison,
// the backup record, CertMagic's events, the filesystems, a person's
// key used from somewhere new) post alerts to it over a unix socket.
// The block's `metrics` line also keeps the box's and each app's
// numbers: motel measures, Debian's VictoriaMetrics stores, vmalert
// evaluates the rules. The commands are `moteld monitor` (send, test,
// silence, rules, setup).
//
// liveswap reports to its Notifier interface and knows nothing of this
// package, which imports liveswap and the backups module's record and
// status, never the other way round. The design is
// notify/DESIGN-notify.md, and for the numbers notify/DESIGN-metrics.md;
// both are normative for this code.
package notify
