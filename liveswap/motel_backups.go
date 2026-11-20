package liveswap

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// BackupsSource answers GET /_motel/backups: what `motel-backup status`
// says of the box's backups as any account looks at them (the record,
// whether backups are set up, the units the manager holds), never root's
// look. The backups module imports this one, so it cannot be imported
// here: moteld, which links both, installs the source (SetBackupsSource),
// as notify installs MetricsSource. It is called with a context that
// ends within backupsBudget.
type BackupsSource interface {
	Backups(ctx context.Context) wire.Backups
}

// SetBackupsSource installs the process's source of GET /_motel/backups,
// before the first config loads. With none, the route answers that this
// server does not read the box's backups.
func SetBackupsSource(s BackupsSource) {
	theBackups.mu.Lock()
	defer theBackups.mu.Unlock()
	theBackups.src, theBackups.at = s, time.Time{}
}

// backupsEvery is how long an answer is kept, for every person: a look
// asks the system manager once over D-Bus and reads a small file, and
// the rate (2 a second for each person) would otherwise be its rate.
// The answer's own `at` says when it was decided.
const backupsEvery = 10 * time.Second

// backupsBudget bounds one look: the manager answers in milliseconds,
// and a request must not wait on one that does not.
const backupsBudget = 2 * time.Second

// noBackupsSource is the answer of a server no source was installed in:
// liveswap built into another Caddy than moteld.
const noBackupsSource = "this server does not read the box's backups: it is not moteld"

// backupsCache is the one answer every request is given until it is
// backupsEvery old. One look at a time: a request that finds it old
// waits for the look under way rather than starting another.
type backupsCache struct {
	mu  sync.Mutex
	src BackupsSource
	now func() time.Time
	at  time.Time // when the answer held was looked for; zero: none
	got wire.Backups
}

// theBackups is the process's: one cache for every config and every
// handler, so a reload does not look again.
var theBackups = &backupsCache{}

func (c *backupsCache) get(ctx context.Context) wire.Backups {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	t := now()
	if !c.at.IsZero() && t.Sub(c.at) < backupsEvery && !t.Before(c.at) {
		return c.got
	}
	if c.src == nil {
		// Not kept: a source installed later is asked at once.
		return wire.Backups{At: t.UTC().Truncate(time.Second), CouldNotTell: noBackupsSource,
			Apps: []wire.BackupApp{}, Running: []wire.BackupUnit{}, UnrecognisedUnits: []string{}, Lines: []wire.BackupLine{}}
	}
	// The look is the box's, not this request's: a person who goes away
	// does not end it for the next.
	lookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupsBudget)
	defer cancel()
	c.got, c.at = c.src.Backups(lookCtx), t
	return c.got
}

// boxHandle is the box a box-wide read reads. Only allowBox makes one,
// after Decide allowed the action on the box, so a function that reads
// what is the box's alone (its backups) cannot be reached by a person
// nobody asked Decide about (TestOneBoxHandleMaker).
type boxHandle struct {
	backups *backupsCache
	// alerts is the config's source of what Alertmanager holds
	// (SetAlertsSource); nil with no monitor block that raises anything.
	alerts AlertsSource
}

// allowBox asks Decide whether p may take a on the box, and is the one
// place a boxHandle is made.
func (h *Handler) allowBox(p access.Principal, a access.Action) (boxHandle, error) {
	if err := decide(p, a, ""); err != nil {
		return boxHandle{}, err
	}
	c := h.backupsCache
	if c == nil {
		c = theBackups
	}
	return boxHandle{backups: c, alerts: h.app.alertsSource}, nil
}

// backupsStatus is what the box says of its backups.
func (b boxHandle) backupsStatus(ctx context.Context) wire.Backups { return b.backups.get(ctx) }

// backups is GET /_motel/backups, for a viewer of the box: the box's
// own claim about its backups, `motel-backup status --json` as any
// account on the box sees it, with HTTP 200 whether they are healthy,
// not, or could not be told. The body holds every app's name, data
// paths and snapshot ids, and no secret (backups/record): it passes
// the filter with no app's values, as the apps list's frame does, since
// no one app's filter may see another's, and the shape and entropy
// layers run over it.
func (h *Handler) backups(w http.ResponseWriter, r *http.Request, p *Person) error {
	box, err := h.allowBox(p.Principal, access.BoxBackups)
	if err != nil {
		// serveMotel asked Decide about the row's action already: the two
		// falling apart.
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err), nil)
	}
	body := box.backupsStatus(r.Context())
	safe := make([]string, 0, len(body.Apps))
	for _, a := range body.Apps {
		if appNameRe.MatchString(a.Name) {
			safe = append(safe, a.Name)
		}
	}
	return RespondJSON(w, http.StatusOK, body, NewRedactor(nil, safe))
}
