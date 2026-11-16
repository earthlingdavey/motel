package cli

import (
	"encoding/json"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/render"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
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
	v := render.Status{
		App:     tx(asked),
		Phase:   tx(st.Phase),
		Version: tx(st.CurrentVersion),
		Unit:    tx(st.Unit),
		PID:     st.PID,
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
	return render.Box{Host: tx(asked), Commit: tx(b.Commit), SHA256: tx(b.SHA256), Recipient: tx(b.Recipient)}
}

// whoamiView is the box's answer beside what this side knows: the box
// asked, the key that signed and where it came from, and the clock the
// expiry is told against. The key and box shown are this side's; the
// box's own are kept to be quoted when they differ.
func whoamiView(me wire.WhoAmI, asked string, key sshkey.Key, now time.Time) render.WhoAmI {
	fp := key.Public.Fingerprint()
	v := render.WhoAmI{
		Name:       tx(me.Name),
		Box:        tx(asked),
		Release:    tx(me.Version),
		Protocol:   me.Protocol,
		Key:        tx(fp),
		KeyShown:   tx(key.Shown),
		KeySource:  tx(key.Source),
		BoxKey:     termtext.Quote(me.Key),
		BoxName:    termtext.Quote(me.Box),
		KeyDiffers: me.Key != fp,
		BoxDiffers: me.Box != asked,
		HasExpiry:  true, // Read takes no whoami without it
		Expires:    me.TokenExpiresAt,
		Now:        now,
	}
	for _, r := range me.Roles {
		v.Roles = append(v.Roles, render.Role{Role: tx(r.Role), Scope: tx(r.Scope)})
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
