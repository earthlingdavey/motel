package main

import (
	"testing"

	"github.com/smallhoursorg/motel/box"
	"github.com/smallhoursorg/motel/notify"
)

// The box handler names a push's secrets/_box/ to `motel validate` by a
// variable notify reads: neither module imports the other, so the name
// is written in each and held equal here, where both are linked.
func TestNotifyPushedSecretsEnvAgrees(t *testing.T) {
	if box.NotifyPushedSecretsEnv != notify.PushedSecretsEnv {
		t.Fatalf("box names %q, notify reads %q", box.NotifyPushedSecretsEnv, notify.PushedSecretsEnv)
	}
}
