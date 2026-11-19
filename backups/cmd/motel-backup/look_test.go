package main

import (
	"path/filepath"
	"testing"

	"github.com/smallhoursorg/motel/backups/look"
)

// TestPathsAreMotelBackups: moteld looks at the backups (GET
// /_motel/backups) by look.OnTheBox, and status by this program's own
// configuration, so the two are the same files.
func TestPathsAreMotelBackups(t *testing.T) {
	cfg := config()
	got := look.Paths{EnvFile: cfg.EnvFile, OldEnvFile: cfg.OldEnvFile, Record: filepath.Join(cfg.StateDir, "status.json")}
	if got != look.OnTheBox {
		t.Errorf("motel-backup looks at %+v, moteld at %+v", got, look.OnTheBox)
	}
}
