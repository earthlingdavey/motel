package main

import (
	"path/filepath"
	"testing"

	"github.com/smallhoursorg/motel/box"
	"github.com/smallhoursorg/motel/liveswap"
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

// The box writes its own secrets to the file notify reads by default,
// in the directory whose files liveswap withholds NOTIFY_ variables
// from: each module restates the path, held equal here.
func TestBoxSecretsPathsAgree(t *testing.T) {
	if box.SecretsDir != liveswap.BoxSecretsDir {
		t.Errorf("box writes env files in %q, liveswap withholds from %q", box.SecretsDir, liveswap.BoxSecretsDir)
	}
	if box.BoxSecretsFile != notify.DefaultSecretsFile {
		t.Errorf("box writes its own secrets to %q, notify reads %q", box.BoxSecretsFile, notify.DefaultSecretsFile)
	}
	if filepath.Dir(notify.DefaultSecretsFile) != liveswap.BoxSecretsDir {
		t.Errorf("notify's file %q is outside %q", notify.DefaultSecretsFile, liveswap.BoxSecretsDir)
	}
}
