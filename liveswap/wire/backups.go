package wire

import "time"

// Backups is what `motel-backup status` decides of the box's backups,
// and GET /_motel/backups's body: the lines it prints, and the facts of
// each, for whoever reads it as JSON (`motel-backup status --json`).
// backups/status names it Result, and makes it (status.Report); it is
// here so that a client decodes the answer into the type the box
// marshals without linking anything of the backups module. Its JSON is
// a contract (backups/README.md "Status"); the record it is read from
// is not.
type Backups struct {
	// At is when it was decided, which every age is reckoned from.
	At time.Time `json:"at"`
	// Healthy is what the exit status says: 0, where it is.
	Healthy bool `json:"healthy"`
	// CouldNotTell is why status could not look, where it could not:
	// the exit status 3.
	CouldNotTell string       `json:"could_not_tell,omitempty"`
	Apps         []BackupApp  `json:"apps"`
	Running      []BackupUnit `json:"running"`
	// UnrecognisedUnits are units the manager holds whose name says
	// nothing here.
	UnrecognisedUnits []string `json:"unrecognised_units"`
	// Lines are what the text says, in its order.
	Lines []BackupLine `json:"lines"`
}

// BackupLine is one line of the text.
type BackupLine struct {
	// App is the app the line is of; none for the run, the repository
	// and a unit of no app.
	App string `json:"app,omitempty"`
	// Level is problem for a line that makes status unhealthy, warning
	// for a warning: line, info for any other.
	Level string `json:"level"`
	Text  string `json:"text"`
}

// BackupApp is what status decides of one app.
type BackupApp struct {
	Name string `json:"name"`
	// Healthy is that no line of this app is a problem.
	Healthy bool `json:"healthy"`
	// Result is how the last run ended for it (backups/record's Class);
	// empty where the record holds no result for it.
	Result string `json:"result"`
	// LastCompleteBackup is when it was made; Fresh that it is no more
	// than status.StaleAfter old and no listing since has missed it.
	LastCompleteBackup *time.Time `json:"last_complete_backup"`
	Fresh              bool       `json:"fresh"`
	// RestoreProven is when a drill last proved a restore of it;
	// RestoreProvenLately that it is no more than status.ProvenOldAfter
	// ago and no drill since failed.
	RestoreProven       *time.Time `json:"restore_proven"`
	RestoreProvenLately bool       `json:"restore_proven_lately"`
}

// BackupUnit is one unit of a run, a restore, a drill or a setup that
// the manager holds, as said.
type BackupUnit struct {
	Role  string     `json:"role"`
	App   string     `json:"app,omitempty"`
	State string     `json:"state"`
	Since *time.Time `json:"since,omitempty"`
}
