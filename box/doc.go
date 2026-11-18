// Package box is the config channel: the operator's repository is the
// only writer of /etc/motel/Caddyfile, and a commit reaches the box
// as a bundle — the file, the commit object, the trees on its path and
// the first-parent chain — that the box proves before root applies it.
// The design is box/DESIGN-box.md; its behaviour specification is
// normative for this code.
//
// This package reads the signed file the way root must read it: from
// its raw tokens, expanding nothing and following nothing (tokens.go).
// The proof core — commit, tree and blob hashing, the chain, the
// signer list and the ssh-keygen verdict — is the pure-Go subpackage
// proof. The Caddyfile surface is the `box` global option (app.go,
// caddyfile.go) and the `box_webhook` directive (handler.go), which
// answers `GET /` from the box's state (exchange.go), admits pushes
// (push.go, admission.go, validate.go) and answers result polls;
// `moteld box webhook` (cmd.go) prints the address a file names.
// The root applier, `moteld box apply`, is apply.go: the trust
// chain's root half (check.go), the install transaction and its
// recovery (txn.go, durable.go), results (results.go), Retention
// (retention.go), the service manager (systemd.go) and the diff a
// result carries (diff.go). The console's commands share its
// transaction and lock: `moteld init` (init_cmd.go) and `moteld box
// baseline` (baseline_cmd.go).
package box
