package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// hostileBackups is what a box says of its backups, every free-text
// value carrying a hostile string: the lines' text, which is printed
// labelled and capped. Names, results and levels are held to their
// grammars (client's TestReadBackups) and are not hostile here.
func hostileBackups() wire.Backups {
	at := time.Date(2026, 11, 15, 9, 59, 52, 0, time.UTC)
	last, proven := at.Add(-42*time.Minute), at.Add(-72*time.Hour)
	lines := []wire.BackupLine{{Level: "info", Text: "last run 2026-11-15 09:17 UTC"}}
	for _, h := range hostile {
		lines = append(lines, wire.BackupLine{App: "shop-2", Level: "problem", Text: "shop-2: failed: " + h})
	}
	lines = append(lines, wire.BackupLine{Level: "warning", Text: "warning: " + strings.Repeat("w", 300) + strings.Repeat(" ", 60) + "problem  forged"})
	return wire.Backups{
		At: at,
		Apps: []wire.BackupApp{
			{Name: "blog", Healthy: true, Result: "ok", LastCompleteBackup: &last, Fresh: true, RestoreProven: &proven, RestoreProvenLately: true},
			{Name: "shop-2", Result: "data missing"},
		},
		Running: []wire.BackupUnit{{Role: hostile[0], State: hostile[1]}}, UnrecognisedUnits: []string{hostile[2]},
		Lines: lines,
	}
}

// backup status: the box's verdict is the exit, as motel-backup
// status's is (0 healthy, 1 not, 3 could not tell); --json is the box's
// body on 0 and 1, and the client's own sentence on 3; the human
// output holds nothing a box could use on a terminal, says the verdict
// as the box's and that it is the box's own claim, and is what the
// golden says.
func TestBackupStatus(t *testing.T) {
	fb := newFakeBox(t)
	// --json prints the box's body made safe as JSON's own escapes (emit).
	body := func() string { return string(withNewline(termtext.JSON(fb.bodies["/_motel/backups"]))) }
	r := runKey(t, fb, nil, "backup", "status", "--json")
	if r.code != 1 || r.stdout != body() || !strings.Contains(r.stderr, "says its backups are not healthy: the box's own claim") {
		t.Fatalf("not healthy, --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(fb.auth[len(fb.auth)-1], "Motel m1.") {
		t.Errorf("Authorization %q, want a person's token", fb.auth[len(fb.auth)-1])
	}
	r = runKey(t, fb, nil, "backup", "status")
	if r.code != 1 {
		t.Fatalf("not healthy: exit %d, stderr %q", r.code, r.stderr)
	}
	if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
		t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
	}
	if strings.Contains(r.stdout, "\n                    problem  forged") || !strings.Contains(r.stdout, "This is the box's own claim") {
		t.Errorf("the output:\n%s", r.stdout)
	}
	golden(t, "hostile-backups", r.stdout)

	fb.backups.Healthy = true
	if r := runKey(t, fb, nil, "backup", "status", "--json"); r.code != 0 || r.stdout != body() {
		t.Errorf("healthy, --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := runKey(t, fb, nil, "backup", "status"); r.code != 0 || r.stderr != "" || !strings.HasPrefix(r.stdout, "127.0.0.1 says its backups are healthy (at 2026-11-15 09:59:52 UTC)\n") {
		t.Errorf("healthy: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	// Could not tell: the box's sentence, quoted, in the client's own.
	fb.backups = wire.Backups{At: time.Date(2026, 11, 15, 9, 59, 52, 0, time.UTC), CouldNotTell: "backups are not set up: " + hostile[3],
		Apps: []wire.BackupApp{}, Running: []wire.BackupUnit{}, UnrecognisedUnits: []string{}, Lines: []wire.BackupLine{}}
	r = runKey(t, fb, nil, "backup", "status", "--json")
	var e struct{ Error string }
	if r.code != 3 || json.Unmarshal([]byte(r.stdout), &e) != nil || !strings.HasPrefix(e.Error, `127.0.0.1 could not tell how its backups are; the box says: "backups are not set up: title \x1b]0;pwned`) {
		t.Errorf("could not tell, --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	r = runKey(t, fb, nil, "backup", "status")
	if r.code != 3 || !strings.HasPrefix(r.stdout, `127.0.0.1 could not tell how its backups are (at 2026-11-15 09:59:52 UTC); the box says: "backups are not set up: `) ||
		unsafeOutput.FindString(r.stdout+r.stderr) != "" {
		t.Errorf("could not tell: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	// A name outside the app grammar is not moteld's answer, and none
	// of the answer is printed.
	fb.backups = hostileBackups()
	fb.backups.Apps[0].Name = "Blog " + hostile[0]
	if r := runKey(t, fb, nil, "backup", "status"); r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: a name in it is no app name") {
		t.Errorf("a name outside the grammar: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	// It is asked with a person's key alone: a deploy token is not sent.
	asked := len(fb.auth)
	if r := runKey(t, fb, map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_KEY": ""}, "backup", "status"); r.code != 2 || len(fb.auth) != asked {
		t.Errorf("with MOTEL_TOKEN: exit %d, %d requests, stderr %q", r.code, len(fb.auth)-asked, r.stderr)
	}
}
