package look

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLook: what a look says of a box whose backups are not set up, of
// one set up whose record cannot be read, and of one set up with no
// run yet; and that root's warnings are asked only once backups are
// known to be set up, and come first.
func TestLook(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := Paths{EnvFile: filepath.Join(dir, "repository.env"), OldEnvFile: filepath.Join(dir, "backup.env"), Record: filepath.Join(dir, "status.json")}
	asked := 0
	warn := func() []string { asked++; return []string{"a line of the credential file"} }

	r, err := Look(context.Background(), p, now, warn)
	if err == nil || !strings.HasPrefix(r.CouldNotTell, "backups are not set up: ") || r.Healthy || asked != 0 || len(r.Lines) != 0 {
		t.Errorf("not set up: %v, %+v, warnings asked %d", err, r, asked)
	}
	// One from an earlier version is named, and not read.
	if err := os.WriteFile(p.OldEnvFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _ := Look(context.Background(), p, now, nil); !strings.Contains(r.CouldNotTell, p.OldEnvFile+" is from before this version") {
		t.Errorf("an old credential file: %q", r.CouldNotTell)
	}

	if err := os.WriteFile(p.EnvFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.Record, 0o755); err != nil { // a record that cannot be read
		t.Fatal(err)
	}
	r, err = Look(context.Background(), p, now, warn)
	if err == nil || !strings.HasPrefix(r.CouldNotTell, "the record of the last run could not be read: ") || asked != 1 ||
		len(r.Lines) != 1 || r.Lines[0].Text != "warning: a line of the credential file" {
		t.Errorf("an unreadable record: %v, %+v, warnings asked %d", err, r, asked)
	}

	if err := os.Remove(p.Record); err != nil {
		t.Fatal(err)
	}
	// Set up just now, no run yet: pending, and healthy. Whether the
	// manager answered is said either way, and changes nothing here.
	r, err = Look(context.Background(), p, time.Now(), nil)
	if err != nil || r.CouldNotTell != "" || !r.Healthy || len(r.Lines) == 0 || r.Lines[0].Text != "no run yet: pending first run" {
		t.Errorf("no run yet: %v, %+v", err, r)
	}
}
