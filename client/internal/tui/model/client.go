// Package model is the TUI with no library under it: what the screens
// read from a box, when they read it, and what they show, as views of
// termtext.Text. It reads the box only through Client, one method per
// motel command, and only through the reads in Actions, each of
// which names its command (TestActionsAreCommands). It imports neither
// the client package nor any transport, nor a terminal library: the one
// file that starts the TUI (cli/tui.go) gives it a Client over the
// client package's reads (TestTUIImports).
package model

import (
	"context"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// Client is one method per command the TUI reads with: whoami, apps,
// status <app>, deploys <app> <version>, box status. Each returns the box's value
// and its body as received. An error is a *Failure, or the context's
// own when it ended.
type Client interface {
	WhoAmI(ctx context.Context) (Got[wire.WhoAmI], error)
	// Apps is the names the box lists: the client package reads the
	// list's shape, and the TUI never does.
	Apps(ctx context.Context) (Got[[]string], error)
	Status(ctx context.Context, app string) (Got[wire.Status], error)
	Deploy(ctx context.Context, app, version string) (Got[wire.DeployResult], error)
	BoxStatus(ctx context.Context) (Got[wire.BoxStatus], error)
}

// Got is one answer: the value, the body as received, and when it
// came (stamped by the session on its clock).
type Got[T any] struct {
	V    T
	Body []byte
	At   time.Time
}

// Failure is a read that did not get its answer: what the client said
// of it, and what it means to the engine.
type Failure struct {
	Kind Fail
	// Message is the client's own sentence, with whatever the box sent
	// in it already made safe or quoted.
	Message string
	// RetryAfter is a 429's wait, zero when the box gave none.
	RetryAfter time.Duration
}

func (f *Failure) Error() string { return f.Message }

// Fail is a kind of failure.
type Fail int

// The kinds.
const (
	// FailNoAnswer: the box did not answer, or something in front of it
	// answered for it (a gateway's 5xx): every read backs off.
	FailNoAnswer Fail = iota
	// FailThis: this read alone (a 409, a record whose version the
	// box's filter hid); asked again in its turn.
	FailThis
	// FailForbidden and FailNotFound: a 403, a 404; not asked again
	// until the person asks (ctrl-r).
	FailForbidden
	FailNotFound
	// FailSlowed: a 429.
	FailSlowed
	// FailSignedOut: a 401 that stood. FailNotSigned: no token could be
	// signed. FailCertificate: a certificate that did not verify. Each
	// stops every read.
	FailSignedOut
	FailNotSigned
	FailCertificate
	// FailAboutAnother: whoami answered about another key than the one
	// that signed, or as another box than the one asked (the whoami
	// command's rule, AboutAnother). Nothing a box says is about this
	// key then: every read stops, as for a refusal, until r.
	FailAboutAnother
	// FailUnusable: the box answered, and the answer could not be used
	// (a value that did not read, a body over the cap, not moteld's
	// shape). Said on its row or screen; nothing else backs off, and the
	// box is not said to be silent.
	FailUnusable
)
