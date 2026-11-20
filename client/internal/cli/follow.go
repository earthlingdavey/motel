package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// FollowEvery is how often status --follow reads the status: the TUI's
// open app's interval. The box's deploy in flight holds every phase so
// far with its time, so a slower read loses no phase, only tells the
// outcome later; 0.2 requests a second beside the TUI's 1.5 keeps a
// person under the box's 2.
const FollowEvery = model.FocusEvery

// follow is status --follow, after its first read: st, answered ans.
// With a deploy in flight it reads the status every FollowEvery,
// printing each phase the box lists once, until the box no longer shows
// that deploy; then it prints its outcome, and exits 0 when it
// succeeded, ExitDeployFailed when it failed, ExitUnknown when the box
// gives no outcome of it. With none in flight it is the status, exit 0.
// --json prints the last status read alone.
func (r *runner) follow(ctx context.Context, c *client.Client, app client.App, rng string, st wire.Status, ans *client.Answer) error {
	if !r.json {
		if err := render.WriteStatus(r.env.Stdout, r.style(), statusView(st, app.Name)); err != nil {
			return err
		}
	}
	f := st.Deploying
	switch {
	case f == nil && st.Phase != "idle":
		return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
			"%s is in %s, and the box names no deploy in flight beside it (a moteld before v0.8.0): there is no deploy to follow", app.Name, tx(st.Phase))}
	case f == nil:
		if r.json {
			return r.emit(ans.Body)
		}
		_, err := fmt.Fprintf(r.env.Stderr, "motel: no deploy of %s is in flight: nothing to follow.\n", app.Name)
		return err
	}
	p := &phasePrinter{r: r}
	if !r.json {
		if _, err := fmt.Fprintf(r.env.Stdout, "\nfollowing the deploy of %s, read every %d s (ctrl-c stops following, not the deploy)\n", tx(f.Version), int(FollowEvery.Seconds())); err != nil {
			return err
		}
	}
	if err := p.print(f.Phases); err != nil {
		return err
	}
	for {
		if err := r.env.Sleep(ctx, FollowEvery); err != nil {
			return err
		}
		var err error
		st, ans, err = c.Status(ctx, app, rng)
		if err != nil {
			return followFailed(err)
		}
		if now := st.Deploying; now != nil && model.SameDeploy(*f, *now) {
			// The deploy as last seen: its outcome must begin with every
			// phase it was seen in (model.OutcomeOf).
			f = now
			if err := p.print(now.Phases); err != nil {
				return err
			}
			continue
		}
		return r.followed(app.Name, *f, st, ans, p)
	}
}

// followed is the end of the deploy f of name, which st, answered ans,
// is the first status read not to show in flight.
func (r *runner) followed(name string, f wire.DeployInFlight, st wire.Status, ans *client.Answer, p *phasePrinter) error {
	d := model.OutcomeOf(f, st)
	if d == nil {
		return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
			"the deploy of %s ended, and the box gives no outcome of it: a push whose upload was never staged ends so, as does one whose outcome another deploy's replaced before this read", tx(f.Version))}
	}
	if err := p.print(d.Phases); err != nil {
		return err
	}
	// What serves, as the versions line of status marks it:
	// "v42 (running)", or "not deployed".
	view := statusView(st, name)
	serves := view.State.String()
	if !view.Version.Empty() {
		serves = view.Version.String() + " (" + serves + ")"
	}
	if r.json {
		if d.Status == "failed" {
			return &client.Failure{Exit: client.ExitDeployFailed, Message: deployFailed(d, serves), Body: ans.Body}
		}
		return r.emit(ans.Body)
	}
	if _, err := fmt.Fprintln(r.env.Stdout); err != nil {
		return err
	}
	if err := render.WriteDeploy(r.env.Stdout, r.style(), tx(name), deployView(*d)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(r.env.Stdout, "now     %s\n", serves); err != nil {
		return err
	}
	if d.Status == "failed" {
		return &client.Failure{Exit: client.ExitDeployFailed, Message: deployFailed(d, serves)}
	}
	return nil
}

func deployFailed(d *wire.DeployResult, serves string) string {
	w := "failed"
	if d.Phase != "" {
		w += " in " + tx(d.Phase).String()
	}
	return fmt.Sprintf("the deploy of %s %s; now %s", tx(d.Version), w, serves)
}

// followFailed is a read made while following that got no status: the
// read's own failure, saying that the deploy's outcome is not known.
func followFailed(err error) error {
	err = fail(err)
	var f *client.Failure
	if !errors.As(err, &f) {
		return err
	}
	g := *f
	g.Message += " (following stopped before the deploy ended, which may still run)"
	return &g
}

// phasePrinter prints each phase a deploy lists once, with the time
// the box says it began: the list only grows while the deploy runs, and
// its record's is the same list.
type phasePrinter struct {
	r       *runner
	printed int
}

func (p *phasePrinter) print(phases []wire.PhaseTiming) error {
	for ; p.printed < len(phases); p.printed++ {
		if p.r.json {
			continue
		}
		ph := phases[p.printed]
		if _, err := fmt.Fprintf(p.r.env.Stdout, "%s UTC  %s\n", ph.StartedAt.UTC().Format("15:04:05"), tx(ph.Name)); err != nil {
			return err
		}
	}
	return nil
}
