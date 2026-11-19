// Package render writes the human output: lines for a person to read,
// which are not a contract and may change in any release (--json is
// the contract). It takes views whose strings from a box are
// termtext.Text, and it does not import the wire types, so nothing a
// box sent can reach the terminal without passing termtext.From
// (TestRenderNeverSeesTheWire).
package render

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Style is how the output is dressed: colour only on a terminal, and
// never with NO_COLOR set.
type Style struct {
	Color bool
}

// word dresses one of a status's own words: green for what went well,
// red for what did not, as is when not known.
func (s Style) word(t termtext.Text) string {
	if !s.Color {
		return t.String()
	}
	// The words a box sends where word is used: a deploy's status, and
	// the watchdog's states that are well and that are not (the client
	// package holds each to its vocabulary). A phase is not coloured.
	switch t.String() {
	case "succeeded", "watching":
		return "\x1b[32m" + t.String() + "\x1b[0m"
	case "failed", "backoff", "throttled":
		return "\x1b[31m" + t.String() + "\x1b[0m"
	}
	return t.String()
}

// Status is GET /<app>'s body, as a person reads it.
type Status struct {
	App termtext.Text
	// State is the client's word for the app: running, stopped, or not
	// deployed. A deploy in flight is Deploying's line.
	State     termtext.Text
	Deploying *InFlight
	Version   termtext.Text
	Unit      termtext.Text
	PID       int
	Watchdog  *Watchdog
	Metrics   *Metrics
	Last      *Deploy
	// Versions are the releases on the box, the running one first.
	Versions []termtext.Text
}

// InFlight is a deploy the app is running: its version (empty when the
// box names none) and its phase, who started it and when.
type InFlight struct {
	Version termtext.Text
	Phase   termtext.Text
	By      termtext.Text
	Started time.Time
}

// Watchdog is the app's watchdog.
type Watchdog struct {
	State       termtext.Text
	Failures    int
	Restarts    int
	LastFailure termtext.Text
}

// Metrics is the summary over the range.
type Metrics struct {
	Window   termtext.Text
	Error    termtext.Text
	MemNow   *float64
	MemMax   *float64
	CPU      *float64
	Requests *uint64
	PerSec   float64
	Share5xx *float64
	P95      *float64
}

// Deploy is one deploy's record.
type Deploy struct {
	Version  termtext.Text
	Status   termtext.Text
	Phase    termtext.Text
	Error    termtext.Text
	By       termtext.Text
	Note     termtext.Text
	Started  time.Time
	Finished time.Time
	Phases   []Phase
	Exit     termtext.Text
	Probe    *Probe
	LogTail  []termtext.Text
	// LogTailCut says a cap dropped lines; LogTailError why there is
	// no tail.
	LogTailCut   bool
	LogTailError termtext.Text
}

// Phase is one phase a deploy passed through.
type Phase struct {
	Name    termtext.Text
	Seconds float64
}

// Probe is the last non-2xx health probe answer.
type Probe struct {
	Status   int
	Location termtext.Text
	Body     []termtext.Text
}

// Summary is one row of the deploys list.
type Summary struct {
	Version  termtext.Text
	Status   termtext.Text
	Phase    termtext.Text
	By       termtext.Text
	Finished termtext.Text
}

// Box is the box webhook's GET /.
type Box struct {
	Host      termtext.Text
	Commit    termtext.Text
	SHA256    termtext.Text
	Recipient termtext.Text
}

// WhoAmI is GET /_motel/whoami's body, beside what this side knows:
// the box asked and the key that signed. The box's own key and box
// fields are shown only as the box's, in a warning, when they differ.
type WhoAmI struct {
	// Name is the name the box lists the key under.
	Name termtext.Text
	// Box is the box this command asked.
	Box      termtext.Text
	Release  termtext.Text
	Protocol int
	// Key is the fingerprint of the key this command signed with;
	// KeyShown and KeySource name it and say how it was chosen.
	Key       termtext.Text
	KeyShown  termtext.Text
	KeySource termtext.Text
	// BoxKey and BoxName are the box's key and box fields as
	// termtext.Quote quotes them, said when KeyDiffers or BoxDiffers.
	BoxKey, BoxName        termtext.Text
	KeyDiffers, BoxDiffers bool
	Roles                  []Role
	// Expires is when the token stops being taken, when HasExpiry; Now
	// is the clock it is told against.
	HasExpiry    bool
	Expires, Now time.Time
}

// Role is one role held on one scope: "box", or "app:<name>".
type Role struct {
	Role  termtext.Text
	Scope termtext.Text
}

// LineMax is the most of a box's free-text value, or of one line of a
// log tail or a probe body, that a labelled line prints, in runes: the
// rest is cut and the cut marked "…[cut]", so no value runs on for
// rows of a terminal and puts text that looks like a label at a row's
// start. (--json has the whole value.)
const LineMax = 200

// capped is t cut to LineMax runes, the cut marked.
func capped(t termtext.Text) string {
	s := t.String()
	n := 0
	for i := range s {
		if n == LineMax {
			return s[:i] + "…[cut]"
		}
		n++
	}
	return s
}

// printer writes lines and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) line(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format+"\n", args...)
	}
}

// WriteStatus writes an app's status.
func WriteStatus(w io.Writer, s Style, st Status) error {
	p := &printer{w: w}
	head := st.App.String() + "  " + st.State.String()
	if !st.Version.Empty() {
		head += " " + st.Version.String()
	}
	if !st.Unit.Empty() {
		head += "  unit " + st.Unit.String()
	}
	if st.PID != 0 {
		head += "  pid " + strconv.Itoa(st.PID)
	}
	p.line("%s", head)
	if d := st.Deploying; d != nil {
		var words []string
		for _, t := range []termtext.Text{d.Version, d.Phase} {
			if !t.Empty() {
				words = append(words, t.String())
			}
		}
		line := strings.Join(words, " ")
		if !d.Started.IsZero() {
			line += ", started " + d.Started.UTC().Format("2006-01-02 15:04 UTC")
		}
		if !d.By.Empty() {
			line += "  by " + capped(d.By)
		}
		p.line("deploying    %s", line)
	}
	if wd := st.Watchdog; wd != nil {
		line := s.word(wd.State)
		if wd.Failures > 0 {
			line += fmt.Sprintf(", %d failed checks in a row", wd.Failures)
		}
		if wd.Restarts > 0 {
			line += fmt.Sprintf(", %d restarts in its window", wd.Restarts)
		}
		if !wd.LastFailure.Empty() {
			line += ": " + capped(wd.LastFailure)
		}
		p.line("watchdog     %s", line)
	}
	if m := st.Metrics; m != nil {
		writeMetrics(p, m)
	}
	if d := st.Last; d != nil {
		line := d.Version.String() + " " + s.word(d.Status)
		if !d.Phase.Empty() {
			line += " in " + d.Phase.String()
		}
		if !d.Finished.IsZero() {
			line += " " + d.Finished.UTC().Format("2006-01-02 15:04 UTC")
		}
		if !d.By.Empty() {
			line += "  by " + capped(d.By)
		}
		p.line("last deploy  %s", line)
		if !d.Error.Empty() {
			p.line("error        %s", capped(d.Error))
		}
		if !d.Note.Empty() {
			p.line("note         %s", capped(d.Note))
		}
	}
	if len(st.Versions) > 0 {
		vs := make([]string, len(st.Versions))
		for i, v := range st.Versions {
			vs[i] = v.String()
			if i == 0 && v == st.Version {
				vs[i] += " (running)"
			}
		}
		p.line("versions     %s", strings.Join(vs, " "))
	}
	return p.err
}

func writeMetrics(p *printer, m *Metrics) {
	window := ""
	if !m.Window.Empty() {
		window = "  (" + m.Window.String() + ")"
	}
	if !m.Error.Empty() {
		p.line("metrics      none: %s", capped(m.Error))
		return
	}
	if m.MemNow != nil || m.MemMax != nil {
		var parts []string
		if m.MemNow != nil {
			parts = append(parts, mib(*m.MemNow)+" now")
		}
		if m.MemMax != nil {
			parts = append(parts, mib(*m.MemMax)+" max")
		}
		p.line("memory       %s%s", strings.Join(parts, ", "), window)
	}
	if m.CPU != nil {
		p.line("cpu          %s cores on average%s", strconv.FormatFloat(*m.CPU, 'f', -1, 64), window)
	}
	if m.Requests != nil {
		line := fmt.Sprintf("%d, %s a second", *m.Requests, strconv.FormatFloat(m.PerSec, 'f', -1, 64))
		if m.Share5xx != nil {
			line += fmt.Sprintf(", 5xx %s%%", strconv.FormatFloat(*m.Share5xx*100, 'f', -1, 64))
		}
		if m.P95 != nil {
			line += fmt.Sprintf(", p95 %s ms", strconv.FormatFloat(*m.P95, 'f', -1, 64))
		}
		p.line("requests     %s%s", line, window)
	}
}

func mib(b float64) string {
	return strconv.FormatFloat(b/(1<<20), 'f', 0, 64) + " MiB"
}

// WriteDeploys writes the deploys list, newest first, as the box
// ordered it. Not in colour: tabwriter would count a colour's escape
// as width, and the columns would not line up. Every cell but BY is a
// word of its grammar (the client's hold), with no white space, or the
// client's own words (a time it could not read, a version the box's
// filter replaced part of); BY, the one cell that holds what a box
// wrote with spaces, is last and capped, so it cannot fake a column.
func WriteDeploys(w io.Writer, rows []Summary) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no deploy recorded")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	p := &printer{w: tw}
	p.line("VERSION\tSTATUS\tPHASE\tFINISHED (UTC)\tBY")
	for _, r := range rows {
		p.line("%s\t%s\t%s\t%s\t%s", r.Version, r.Status, r.Phase, r.Finished, capped(r.By))
	}
	if p.err != nil {
		return p.err
	}
	return tw.Flush()
}

// WriteDeploy writes one deploy's whole record.
func WriteDeploy(w io.Writer, s Style, app termtext.Text, d Deploy) error {
	p := &printer{w: w}
	head := app.String() + " " + d.Version.String() + "  " + s.word(d.Status)
	if !d.Phase.Empty() {
		head += " in " + d.Phase.String()
	}
	if !d.Started.IsZero() {
		head += "  " + d.Started.UTC().Format("2006-01-02 15:04:05")
		if !d.Finished.IsZero() {
			head += " to " + d.Finished.UTC().Format("15:04:05") + fmt.Sprintf(" UTC (%s)", d.Finished.Sub(d.Started).Round(time.Second))
		} else {
			head += " UTC"
		}
	}
	p.line("%s", head)
	if !d.By.Empty() {
		p.line("by      %s", capped(d.By))
	}
	if !d.Error.Empty() {
		p.line("error   %s", capped(d.Error))
	}
	if !d.Note.Empty() {
		p.line("note    %s", capped(d.Note))
	}
	if len(d.Phases) > 0 {
		parts := make([]string, len(d.Phases))
		for i, ph := range d.Phases {
			parts[i] = ph.Name.String() + " " + strconv.FormatFloat(ph.Seconds, 'f', 1, 64) + " s"
		}
		p.line("phases  %s", strings.Join(parts, " · "))
	}
	if !d.Exit.Empty() {
		p.line("exit    %s", capped(d.Exit))
	}
	if pr := d.Probe; pr != nil {
		line := strconv.Itoa(pr.Status)
		if !pr.Location.Empty() {
			line += "  location " + capped(pr.Location)
		}
		p.line("probe   %s", line)
		for _, l := range pr.Body {
			p.line("  %s", capped(l))
		}
	}
	if len(d.LogTail) > 0 {
		cut := ""
		if d.LogTailCut {
			cut = ", the earlier ones cut"
		}
		p.line("log tail (%d lines%s):", len(d.LogTail), cut)
		for _, l := range d.LogTail {
			p.line("  %s", capped(l))
		}
	}
	if !d.LogTailError.Empty() {
		p.line("log tail: none: %s", capped(d.LogTailError))
	}
	return p.err
}

// WriteBox writes the box webhook's GET /.
func WriteBox(w io.Writer, b Box) error {
	p := &printer{w: w}
	commit := b.Commit.String()
	short := commit
	if r := []rune(short); len(r) > 12 {
		short = string(r[:12])
	}
	p.line("%s  runs %s", b.Host, short)
	p.line("commit           %s", commit)
	p.line("Caddyfile sha256 %s", b.SHA256)
	if !b.Recipient.Empty() {
		p.line("key for secrets  %s", capped(b.Recipient))
	}
	return p.err
}

// WriteWhoAmI writes who the box takes the key to be. The box's words
// are labelled as the box's fields are (its name for the key is
// "name"); the key and the box are this side's, and a box that names
// others is quoted in a warning.
func WriteWhoAmI(w io.Writer, me WhoAmI) error {
	p := &printer{w: w}
	// An answer about another key, or from another box, is said as
	// that alone: nothing else in it is about this key here.
	if me.KeyDiffers || me.BoxDiffers {
		if me.KeyDiffers {
			p.line("warning  the box names another key than the one this command signed with (%s): %s", me.Key, me.BoxKey)
		}
		if me.BoxDiffers {
			p.line("warning  the box answers as another box than the one this command asked (%s): %s", me.Box, me.BoxName)
		}
		return p.err
	}
	p.line("name     %s", me.Name)
	release := "moteld " + me.Release.String()
	if me.Release.Empty() {
		release = "moteld, a build that names no release"
	}
	p.line("box      %s, %s, protocol %d", me.Box, release, me.Protocol)
	p.line("key      %s (%s, %s)", me.Key, me.KeyShown, me.KeySource)
	if len(me.Roles) == 0 {
		p.line("roles    none from this address")
	} else {
		roles := make([]string, len(me.Roles))
		for i, r := range me.Roles {
			roles[i] = r.Role.String() + " " + scopeWords(r.Scope)
		}
		p.line("roles    %s", strings.Join(roles, ", "))
	}
	if me.HasExpiry {
		at := me.Expires.UTC().Format("2006-01-02 15:04:05")
		// A token lives at most ten minutes: a time further off is said
		// as such, never as a duration that a far date would overflow.
		switch left := me.Expires.Sub(me.Now).Round(time.Second); {
		case left <= 0:
			p.line("token    expired %s UTC", at)
		case left > time.Hour:
			p.line("token    expires %s UTC (more than an hour from now, longer than a token lives)", at)
		default:
			p.line("token    expires %s UTC (in %s)", at, left)
		}
	}
	return p.err
}

// scopeWords says a role's scope, which the client took only as
// "box" or "app:<name>" (client's hold): "of the box", "of app <name>".
func scopeWords(scope termtext.Text) string {
	if name, ok := strings.CutPrefix(scope.String(), "app:"); ok {
		return "of app " + name
	}
	return "of the box"
}

// WriteNames writes one name a line.
func WriteNames(w io.Writer, names []termtext.Text) error {
	p := &printer{w: w}
	for _, n := range names {
		p.line("%s", n)
	}
	return p.err
}
