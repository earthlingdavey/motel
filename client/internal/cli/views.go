package cli

import (
	"encoding/json"
	"time"

	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The one place a box's strings meet the renderer: each wire value is
// turned into a view, and every string in it goes through
// termtext.From on the way. render cannot take a wire type, so nothing
// skips this.

var tx = termtext.From

// statusView names the app asked for: the client takes a status only
// when the box's app is that one (and its other head values only in
// their grammars, client's hold).
func statusView(st wire.Status, asked string) render.Status {
	// The app's state first, in the TUI's row's words and order
	// (model/apps.go); the phase only beside a deploy in flight.
	state := "running"
	wd := st.Watchdog
	switch {
	case st.CurrentVersion == "":
		state = "not deployed"
	case wd != nil && wd.State == "throttled":
		state = "throttled"
	case !st.Running:
		state = "stopped"
	case wd != nil && wd.State == "backoff":
		state = "backoff"
	}
	v := render.Status{
		App:     tx(asked),
		State:   tx(state),
		Version: tx(st.CurrentVersion),
		Unit:    tx(st.Unit),
		PID:     st.PID,
	}
	if d := st.Deploying; d != nil || st.Phase != "idle" {
		f := &render.InFlight{}
		if st.Phase != "idle" {
			f.Phase = tx(st.Phase)
		}
		if d != nil {
			f.Version, f.By, f.Started = tx(d.Version), tx(d.By), d.StartedAt
		}
		v.Deploying = f
	}
	if wd := st.Watchdog; wd != nil {
		v.Watchdog = &render.Watchdog{State: tx(wd.State), Failures: wd.Failures, Restarts: wd.RestartsInWindow, LastFailure: tx(wd.LastFailure)}
	}
	if m := st.Metrics; m != nil {
		mv := &render.Metrics{Window: tx(m.Window), Error: tx(m.Error), MemMax: m.MemoryMax}
		if m.Memory != nil {
			mv.MemNow = m.Memory.Now
			if m.Memory.Max != nil {
				mv.MemMax = m.Memory.Max
			}
		}
		if m.CPU != nil {
			cpu := m.CPU.Avg
			mv.CPU = &cpu
		}
		if r := m.Requests; r != nil {
			total := r.Total
			mv.Requests, mv.PerSec, mv.Share5xx, mv.P95 = &total, r.PerSecond, r.Share5xx, r.P95
		}
		v.Metrics = mv
	}
	if st.LastDeploy != nil {
		d := deployView(*st.LastDeploy)
		v.Last = &d
	}
	for _, s := range st.AvailableVersions {
		v.Versions = append(v.Versions, tx(s))
	}
	return v
}

func deployView(d wire.DeployResult) render.Deploy {
	v := render.Deploy{
		Version:  tx(d.Version),
		Status:   tx(d.Status),
		Phase:    tx(d.Phase),
		Error:    tx(d.Error),
		By:       tx(d.By),
		Note:     tx(d.Note),
		Started:  d.StartedAt,
		Finished: d.FinishedAt,
	}
	for _, p := range d.Phases {
		v.Phases = append(v.Phases, render.Phase{Name: tx(p.Name), Seconds: p.Seconds})
	}
	if dt := d.Detail; dt != nil {
		v.Exit = tx(dt.Exit)
		if pr := dt.Probe; pr != nil {
			v.Probe = &render.Probe{Status: pr.Status, Location: tx(pr.Location), Body: termtext.Lines(pr.Body)}
		}
		for _, l := range dt.LogTail {
			// A tail line is one journal line, but the app wrote it
			// and may have put a line break in it.
			v.LogTail = append(v.LogTail, termtext.Lines(l)...)
		}
		v.LogTailCut = dt.LogTailTruncated
		v.LogTailError = tx(dt.LogTailError)
	}
	return v
}

func summaryViews(ds []wire.DeploySummary) []render.Summary {
	out := make([]render.Summary, 0, len(ds))
	for _, d := range ds {
		out = append(out, render.Summary{
			Version:  tx(d.Version),
			Status:   tx(d.Status),
			Phase:    tx(d.Phase),
			By:       tx(d.By),
			Finished: rawTime(d.FinishedAt),
		})
	}
	return out
}

// rawTime shows a summary's time, which the box carries as the record
// has it: a timestamp when it is one, and the client's own word
// otherwise (the response filter may have replaced it), never what it
// holds, which could fake the table's columns.
func rawTime(raw json.RawMessage) termtext.Text {
	if len(raw) == 0 {
		return termtext.Text{}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return tx(t.UTC().Format("2006-01-02 15:04"))
		}
	}
	return tx("(unreadable)")
}

// boxView names the host asked for, which the client checked the
// box's box_webhook against.
func boxView(b wire.BoxStatus, asked string) render.Box {
	return render.Box{Host: tx(asked), Commit: tx(b.Commit), SHA256: tx(b.SHA256), Recipient: tx(b.Recipient), Edited: b.OutOfBand}
}

// backupsView names the host asked for. Every name, result and level
// in b was held to its grammar (client's hold); the times are the
// client's own words.
func backupsView(b wire.Backups, asked string) render.Backups {
	when := func(t *time.Time, none string) string {
		if t == nil {
			return none
		}
		return t.UTC().Format("2006-01-02 15:04")
	}
	v := render.Backups{Host: tx(asked), At: b.At, Healthy: b.Healthy && b.CouldNotTell == "", Claim: model.OwnClaim}
	if b.CouldNotTell != "" {
		v.Why = termtext.Quote(b.CouldNotTell)
	}
	for _, a := range b.Apps {
		v.Apps = append(v.Apps, render.BackupApp{Name: tx(a.Name), Result: tx(a.Result),
			Last: when(a.LastCompleteBackup, "none"), Proven: when(a.RestoreProven, "never"), Fresh: a.Fresh, Lately: a.RestoreProvenLately})
	}
	for _, l := range b.Lines {
		v.Lines = append(v.Lines, render.BackupLine{Level: tx(l.Level), Text: tx(model.LineText(l))})
	}
	return v
}

// boxAlertsView names the host asked for. A head is the client's words,
// its times absolute (UTC), and quotes a word the client does not
// know; every other value is the box's text, labelled and capped where
// it is printed.
func boxAlertsView(b wire.BoxAlerts, asked string) render.BoxAlerts {
	at := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") + " UTC" }
	v := render.BoxAlerts{Host: tx(asked), On: b.Monitor == wire.MonitorOn, MoreAlerts: b.MoreAlerts, MoreSilences: b.MoreSilences, Advice: model.SilenceAdvice}
	for _, a := range b.Alerts {
		head := model.SeverityWord(a.Severity) + "  "
		switch a.State {
		case "active":
			head += "firing"
		case "unprocessed":
			head += "just received, not yet judged; firing"
		case "suppressed":
			switch {
			case a.SilencedUntil != nil && a.Inhibited:
				head += "silenced until " + at(*a.SilencedUntil) + ", and held while a deploy runs; firing"
			case a.SilencedUntil != nil:
				head += "silenced until " + at(*a.SilencedUntil) + "; firing"
			case a.Inhibited:
				head += "held while a deploy of its app runs; firing"
			default:
				head += "held quiet by Alertmanager; firing"
			}
		default:
			head += "in a state not known, " + termtext.Quote(a.State).String() + "; firing"
		}
		head += " since " + at(a.StartsAt)
		al := render.BoxAlert{Head: termtext.From(head), Name: tx(a.Name), App: tx(a.App), Instance: tx(a.Instance), Summary: tx(a.Summary)}
		if model.SeverityWord(a.Severity) != a.Severity {
			al.Severity = tx(a.Severity) // a word not motel's: the box's, on a line of its own
		}
		v.Alerts = append(v.Alerts, al)
	}
	for _, s := range b.Silences {
		head := s.State + "  from "
		if s.State != wire.SilenceActive && s.State != wire.SilencePending {
			head = "in a state not known (" + termtext.Quote(s.State).String() + "): from "
		}
		head += at(s.StartsAt) + " until " + at(s.EndsAt)
		v.Silences = append(v.Silences, render.BoxSilence{Head: termtext.From(head),
			Matchers: termtext.From(model.Matchers(s.Matchers).Text()), By: tx(s.CreatedBy), Comment: tx(s.Comment), ID: tx(s.ID)})
	}
	return v
}

// whoamiView is the box's answer beside what this side knows: the box
// asked, the key that signed and where it came from, and the clock the
// expiry is told against. The key and box shown are this side's; the
// box's own are kept to be quoted when they differ.
func whoamiView(me wire.WhoAmI, asked string, key sshkey.Key, now time.Time) render.WhoAmI {
	fp := key.Public.Fingerprint()
	v := render.WhoAmI{
		Name:      tx(me.Name),
		Box:       tx(asked),
		Release:   tx(me.Version),
		Protocol:  me.Protocol,
		Key:       tx(fp),
		KeyShown:  tx(key.Shown),
		KeySource: tx(key.Source),
		BoxKey:    termtext.Quote(me.Key),
		BoxName:   termtext.Quote(me.Box),
		HasExpiry: true, // Read takes no whoami without it
		Expires:   me.TokenExpiresAt,
		Now:       now,
	}
	// The TUI's rule too: one function, so the two cannot differ.
	v.KeyDiffers, v.BoxDiffers = model.AboutAnother(me, fp, asked)
	for _, r := range me.Roles {
		// viewer is the one role a box grants today (liveswap/access,
		// lines.go), and is said as a word. Any other word the box
		// names, a later motel's role among them, is the box's own and
		// is quoted as such: the client took it by its shape alone
		// (client's roleRe), and a box chooses it.
		role := tx(r.Role)
		if r.Role != "viewer" {
			role = termtext.Quote(r.Role)
		}
		v.Roles = append(v.Roles, render.Role{Role: role, Scope: tx(r.Scope)})
	}
	return v
}

func appsView(a wire.Apps) []termtext.Text {
	out := make([]termtext.Text, 0, len(a.Apps))
	for _, e := range a.Apps {
		out = append(out, tx(e.Name))
	}
	return out
}
