//go:build linux

// Package harden makes the process that imports it non-dumpable as
// early as Go allows. It is a leaf package on purpose: it depends on
// syscall alone, and Go (1.21+) initializes packages in import-path
// order among those whose dependencies are ready, so this init runs
// right after syscall's — before os, fmt, Caddy and every package
// that depends on any of them, which is every package that does real
// work. What can still precede it: packages that depend on nothing
// beyond syscall's own closure and whose import path sorts before
// github.com/smallhoursorg — leaf tables and the like, which cannot
// read /proc without os. Measured on motel with
// GODEBUG=inittrace=1: the 17th of 460 initializers, immediately after
// syscall (16th) and before os (29th), preceded by two such leaves (a
// cel-go operator table, a pgx buffer pool); an xcaddy build with
// other modules may add leaves of that kind, never os-dependent ones.
// TestInitRunsBeforeOS pins syscall < harden < os for whatever binary
// runs it. Keep this package free of imports beyond syscall — a single
// fmt would move it behind the whole os/fmt dependency graph.
package harden

import "syscall"

// init runs Process before main and before every heavier initializer.
// It has to be this early: app units outlive motel restarts, so an
// app of the same UID is already running when a new supervisor process
// starts, and any later hook (App.Start, Provision) would leave the
// whole Go initialization window open — and is never reached at all
// when the liveswap block has been removed while units keep running.
// A failure is fatal: PR_SET_DUMPABLE=0 cannot fail on a real kernel,
// so failing closed costs nothing, and a supervisor whose /proc is
// readable by the apps it supervises must not run.
//
// What remains open is the interval between execve and this init —
// the Go runtime's own start-up plus the initializers of syscall's
// dependencies, well under a millisecond — during which a same-UID
// process racing on /proc could still read the new supervisor's
// environment, or attach to it: Debian 13 leaves Yama's ptrace_scope
// at 0, and an attach made in the window survives this call (with
// ptrace_scope=1, Yama forbids a non-descendant's PTRACE_ATTACH at any
// time, and the window is a read race alone). Only the kernel closes
// the window, and only from the app's side: app units in their own
// PID namespace (PrivatePIDs= on the units, #35) cannot see the
// supervisor at all — a namespace on motel.service would not help,
// since a parent PID namespace sees its children's processes — or an
// exec under AT_SECURE. DESIGN-threat-model.md records it as a
// residual.
func init() {
	if err := Process(); err != nil {
		// No fmt here: importing it would put fmt, os and their whole
		// dependency graph ahead of this init (see the package comment).
		panic("liveswap/harden: cannot mark the process non-dumpable (" + err.Error() + "): refusing to run a supervisor whose /proc is readable by the apps it supervises")
	}
}

// Process marks the calling process non-dumpable (PR_SET_DUMPABLE=0).
// Idempotent and cheap; init calls it, tests pin the result.
//
// Every liveswap app runs as the same UID as the supervisor, and the
// kernel gates /proc/<pid>/{environ,root,cwd,mem,fd,maps} on
// ptrace_may_access, which any same-UID process passes while the
// target is dumpable — whatever mount namespace the reader sits in. A
// compromised app could so read the supervisor's environment (ACME
// DNS tokens) or walk the host filesystem through /proc/<pid>/root. A
// non-dumpable target needs CAP_SYS_PTRACE instead, which apps under
// NoNewPrivileges never hold: this closes the supervisor-secret route
// on every host, with or without a sandbox (DESIGN-threat-model.md,
// "The shared-UID rule").
//
// Cost: the kernel writes no core dump of the process (Go prints its
// own goroutine trace on a fatal error) and the per-process files
// under /proc/<pid> become root-owned; the process's own reads of
// /proc/self are unaffected. execve resets the flag, so a child
// motel (`motel start`) sets it again through its own init.
func Process() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
