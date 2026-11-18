// motel reads a box over HTTPS from a laptop or CI: an app's
// status and deploys, and the box's own state, with a token the
// person's own ssh key signs, or with a deploy token in MOTEL_TOKEN.
// It links no Caddy: the box's wire types come from liveswap/wire,
// which imports only the standard library (TestLinksNoCaddy).
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/smallhoursorg/motel/client/internal/cli"
	"github.com/smallhoursorg/motel/client/internal/local"
)

// version is the release, set at build time with
// -ldflags "-X main.version=v…"; else the module's version from the
// build info, "(devel)" for a build from a checkout.
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], env())
	stop()
	os.Exit(code)
}

func env() cli.Env {
	dir, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	v := version
	if v == "" {
		v = "(devel)"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
			v = bi.Main.Version
		}
	}
	return cli.Env{
		Getenv: os.Getenv,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Color:  terminal(os.Stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
		Dir:    dir,
		Home:   home,
		Now:    time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		Run:     local.Exec{},
		Version: v,
	}
}

// terminal says whether f is a terminal: a character device.
func terminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
