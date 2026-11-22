package main

import (
	"testing"

	"github.com/smallhoursorg/motel/backups/look"
	"github.com/smallhoursorg/motel/notify"
)

// TestBackupAlertsLookWhereStatusLooks: notify's backup poller judges
// the record and the credential file that GET /_motel/backups and
// `motel-backup status` look at (TestPathsAreMotelBackups holds the
// second to look.OnTheBox), so the backup alerts and the client's screen
// say the same of the same box.
func TestBackupAlertsLookWhereStatusLooks(t *testing.T) {
	if notify.DefaultBackupRecord != look.OnTheBox.Record || notify.DefaultBackupEnvFile != look.OnTheBox.EnvFile {
		t.Errorf("notify reads %s and %s, the look %+v", notify.DefaultBackupRecord, notify.DefaultBackupEnvFile, look.OnTheBox)
	}
}
