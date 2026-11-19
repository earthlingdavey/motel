// Package look is the look at the backups that any account on the box
// may take, root or not: whether backups are set up, the record the
// last run left, and which of their units the system manager holds,
// which status.Report decides from. `motel-backup status` looks so, and
// so does moteld for GET /_motel/backups, as the motel user, so the two
// say the same of the same box. Only root's look adds anything: what
// it finds of the credential file (Warnings), which moteld never reads.
package look

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/smallhoursorg/motel/backups/engine"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/status"
	"github.com/smallhoursorg/motel/backups/unit"
)

// Paths are the files a look reads: EnvFile only by its name (Lstat:
// whether it is there, and since when), OldEnvFile likewise, and the
// record, which is for everyone to read.
type Paths struct {
	EnvFile, OldEnvFile, Record string
}

// OnTheBox are the paths as packaged, which motel-backup's own
// configuration names too (TestPathsAreMotelBackups).
var OnTheBox = Paths{
	EnvFile:    "/etc/motel-backup/repository.env",
	OldEnvFile: "/etc/motel/backup.env",
	Record:     "/var/lib/motel-backup/status.json",
}

// Look is the Result of a look at now, and the error that kept it from
// looking, where one did (the Result then says why, as could_not_tell).
// warnings, when not nil, is asked once backups are known to be set up,
// for what root's look finds of the credential file; their lines come
// first, as status says them.
func Look(ctx context.Context, p Paths, now time.Time, warnings func() []string) (status.Result, error) {
	// Only a file that is not there says so. Any other answer — a
	// directory this user may not search — is not "not set up".
	env, err := os.Lstat(p.EnvFile)
	if errors.Is(err, fs.ErrNotExist) {
		err = fmt.Errorf("backups are not set up: %s is not there%s", p.EnvFile, engine.OldEnvFileNote(engine.Config{OldEnvFile: p.OldEnvFile}))
		return status.CouldNotTell(now, nil, err.Error()), err
	} else if err != nil {
		err = fmt.Errorf("whether backups are set up cannot be told from here: %w", err)
		return status.CouldNotTell(now, nil, err.Error()), err
	}
	var warned []string
	if warnings != nil {
		warned = warnings()
	}
	st, err := record.Read(p.Record)
	if err != nil {
		err = fmt.Errorf("the record of the last run could not be read: %w", err)
		return status.CouldNotTell(now, warned, err.Error()), err
	}
	in := status.Input{Record: st, Now: now, SetUp: env.ModTime(), Warnings: warned}
	var active []unit.Active
	active, in.RunningErr = unit.ListActive(ctx, engine.UnitPattern)
	for _, a := range active {
		if role, app, ok := engine.ParseUnitName(a.Name); ok {
			in.Running = append(in.Running, status.Running{Role: role, App: app, State: a.State, Since: a.Since})
		} else {
			in.Unrecognised = append(in.Unrecognised, a.Name)
		}
	}
	return status.Report(in), nil
}
