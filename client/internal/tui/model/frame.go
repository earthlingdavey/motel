package model

import (
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Screen is a view the TUI shows.
type Screen int

// The screens that read the box, and help.
const (
	ScreenApps Screen = iota
	ScreenApp
	ScreenRecord
	ScreenMe
	ScreenBox
	ScreenHelp
	ScreenRefused
	// ScreenStopHelp is help opened over a stop screen: it leaves, asks
	// again or quits, and opens nothing, since nothing is asked.
	ScreenStopHelp
)

// View is the screen open and what it is of.
type View struct {
	Screen       Screen
	App, Version string
}

// StopLine says why every read stopped, in the client's words, for the
// top of the screen; nil while they run.
func (s *Session) StopLine() Line {
	if s.StopErr == nil || s.Engine.Stopped() == Running {
		return nil
	}
	return Line{ownC("not asking: ", Bad), Seg{T: Capped(failureText(s.StopErr), 4*LineMax)}}
}

// askAgain is " ctrl-r asks again." when it would, and nothing while
// every read is stopped or a 429's wait runs: no hint for a key that
// does nothing.
func (s *Session) askAgain(now time.Time) Line {
	if !s.Engine.CanAskAgain(now) {
		return nil
	}
	return Line{own(" ctrl-r asks again.")}
}

// screenReads is the read behind each screen that has one.
var screenReads = map[Screen]Kind{ScreenApps: KApps, ScreenApp: KStatus, ScreenRecord: KDeploy, ScreenMe: KWhoAmI, ScreenBox: KBox}

// CommandArgs is the motel command line that reads what v shows, as
// its arguments: the command's words, --box so it works pasted
// anywhere, --key only when the person named it, then -- and the
// positional arguments, so a version that begins with - is not read as
// a flag.
func (s *Session) CommandArgs(v View) ([]string, bool) { return s.commandArgs(v, false) }

func (s *Session) commandArgs(v View, json bool) ([]string, bool) {
	k, reads := screenReads[v.Screen]
	a, ok := ActionFor(k)
	if !reads || !ok {
		return nil, false
	}
	args := []string{"motel"}
	var pos []string
	for _, w := range strings.Fields(a.Command) {
		switch w {
		case "<app>":
			pos = append(pos, v.App)
		case "<version>":
			pos = append(pos, v.Version)
		default:
			args = append(args, w)
		}
	}
	args = append(args, "--box", s.boxArg())
	if s.Key.Source == "--key" { // sshkey.SourceFlag: the person named it
		args = append(args, "--key", s.Key.Shown)
	}
	if json {
		args = append(args, "--json")
	}
	if len(pos) > 0 {
		args = append(append(args, "--"), pos...)
	}
	return args, true
}

// boxArg is the box as the printed command's --box gives it.
func (s *Session) boxArg() string {
	if s.Addr != "" {
		return s.Addr
	}
	return s.Host
}

// commandJSON is the command with --json, among its flags.
func (s *Session) commandJSON(v View) string {
	args, _ := s.commandArgs(v, true)
	return ShellLine(args)
}

// Command is CommandArgs as a line for a POSIX shell: every argument
// not made only of safe characters is quoted.
func (s *Session) Command(v View) (string, bool) {
	args, ok := s.CommandArgs(v)
	if !ok {
		return "", false
	}
	return ShellLine(args), true
}

// Body is the answer behind v as received, and when it came.
func (s *Session) Body(v View) ([]byte, time.Time, bool) {
	switch v.Screen {
	case ScreenApps:
		if s.List != nil {
			return s.List.Body, s.List.At, true
		}
	case ScreenApp:
		if a := s.apps[v.App]; a != nil && a.st != nil {
			return a.st.Body, a.st.At, true
		}
	case ScreenRecord:
		if g, _, ok := s.Record(v.App, v.Version); ok && g != nil {
			return g.Body, g.At, true
		}
	case ScreenMe:
		if s.Me != nil {
			return s.Me.Body, s.Me.At, true
		}
	case ScreenBox:
		if s.BoxSt != nil {
			return s.BoxSt.Body, s.BoxSt.At, true
		}
	}
	return nil, time.Time{}, false
}

// Cells is how many cells of the screen a text takes. The screens'
// library knows (a wide character takes two); model draws nothing, so
// the screens hand it in.
type Cells func(termtext.Text) int

// Overlay is the command and JSON overlay for v: the command, then,
// when json, the box's answer. The JSON is indented once per answer and
// width, not at every frame: a large body would make every frame slow.
// width is the room inside the overlay's border, in cells; every row is
// laid out to it by lay. A command line wider than it is cut there, the
// cut marked, and the overlay says not to copy it and what prints it
// whole, in place of asking to select it.
func (s *Session) Overlay(v View, json bool, width int, cells Cells) []Line {
	c, ok := s.Command(v)
	if !ok {
		return lay([]oRow{words("this screen reads nothing from the box")}, width, cells)
	}
	body, at, have := s.Body(v)
	if !json {
		cmds := []oRow{command("  " + c), command("  " + s.commandJSON(v))}
		rows := append([]oRow{words("the same read from a shell:"), blank}, append(cmds, blank)...)
		first, second, row := !cmds[0].fits(width, cells), !cmds[1].fits(width, cells), false
		if v.Screen == ScreenApps {
			// A row's command as commandArgs makes it, --key among its
			// flags, the app left as the placeholder <app>, unquoted.
			args, _ := s.commandArgs(View{Screen: ScreenApp}, false)
			tmpl := command("  " + ShellLine(args[:len(args)-1]) + " <app>")
			row = !tmpl.fits(width, cells)
			rows = append(rows, words("This screen is that list, and a row for each app: each row is"), tmpl, blank)
		}
		if first || second || row {
			// P prints the first command alone (Printable).
			w := "a line above is cut at the window's edge, marked …: do not copy it."
			if first || second {
				w += " P quits and prints the first command whole below your prompt"
				if second {
					w += "; the second is it with --json among its flags"
				}
				w += "."
			}
			if row {
				w += " c then P on an app's own screen prints a row's command whole."
			}
			rows = append(rows, warn(w))
		} else {
			rows = append(rows, words("select it with the mouse, or press P to quit and print it below your prompt"))
		}
		if have {
			rows = append(rows, words("y: the box's answer as JSON ("+size(len(body))+", fetched "+hms(at)+" UTC)"))
		}
		return lay(rows, width, cells)
	}
	if !have {
		return lay([]oRow{words("no answer yet to show")}, width, cells)
	}
	key := jsonKey{v, at, len(body), width}
	if s.json.key != key || s.json.lines == nil {
		s.json.key, s.json.lines = key, nil
		lines, more, indented := JSONLines(body)
		head := "indented here; its strings as received, made safe to show;"
		if !indented {
			head = "not JSON that indents: shown line by line as it came, made safe to show;"
		}
		cmd := command("  " + c)
		rows := []oRow{words("the box's answer to"), cmd}
		if !cmd.fits(width, cells) {
			rows = append(rows, warn("the line above is cut at the window's edge, marked …: do not copy it. c then P prints it whole."))
		}
		rows = append(rows, words(head),
			words("lines over "+strconv.Itoa(TailMax)+" characters, and answers over "+strconv.Itoa(OverlayLines)+" lines, are cut for display"),
			words("("+size(len(body))+", fetched "+hms(at)+" UTC); P prints it whole, made safe on a terminal"))
		answer := make([]oRow, 0, len(lines))
		wide := false
		for _, l := range lines {
			r := oRow{Seg{T: l}, oAnswer}
			wide = wide || !r.fits(width, cells)
			answer = append(answer, r)
		}
		if wide {
			// There is no scrolling sideways: the screen cuts such a line
			// at its edge, with no mark of its own.
			rows = append(rows, warn("a line below is wider than the window and cut at its edge, unmarked: P prints every line whole"))
		}
		rows = append(append(rows, blank), answer...)
		if more > 0 {
			rows = append(rows, blank, warn("… "+strconv.Itoa(more)+" more lines not drawn here: P quits and prints the whole answer"))
		}
		s.json.lines = lay(rows, width, cells)
	}
	return s.json.lines
}

// oRow is a row of an overlay, and how it is laid out (lay).
type oRow struct {
	s Seg
	k oKind
}

type oKind int

const (
	// oWords is the client's own words: laid on more rows to fit, never
	// cut.
	oWords oKind = iota
	// oCommand is a command line: never laid on two rows, which would
	// copy with a line feed in it, so one too wide is cut, the cut
	// marked, and the overlay says so (Overlay).
	oCommand
	// oAnswer is a line of the box's answer, drawn as it is: the screen
	// cuts one too wide at its edge, and the overlay says so (Overlay).
	oAnswer
)

var blank = words("")

func words(s string) oRow   { return oRow{own(s), oWords} }
func warn(s string) oRow    { return oRow{ownC(s, Warn), oWords} }
func command(s string) oRow { return oRow{own(s), oCommand} }

func (r oRow) fits(width int, cells Cells) bool { return cells(r.s.T) <= width }

// lay is every row of an overlay laid out to width cells: the one
// place an overlay's rows are fitted to the window.
func lay(rows []oRow, width int, cells Cells) []Line {
	out := make([]Line, 0, len(rows))
	for _, r := range rows {
		switch {
		case r.k == oAnswer || r.fits(width, cells):
			out = append(out, Line{r.s})
		case r.k == oCommand:
			out = append(out, cutCells(r.s, width, cells))
		default:
			out = append(out, wrapCells(r.s, width, cells)...)
		}
	}
	return out
}

// cutCells is s cut to width cells, the cut marked with "…".
func cutCells(s Seg, width int, cells Cells) Line {
	mark := termtext.From("…")
	room := width - cells(mark)
	t := s.T.String()
	end := 0
	for i := range t {
		if cells(termtext.From(t[:i])) > room {
			break
		}
		end = i
	}
	return Line{{T: termtext.From(t[:end] + "…"), C: s.C}}
}

// wrapCells is the client's words of s laid on rows of at most width
// cells, broken at spaces.
func wrapCells(s Seg, width int, cells Cells) []Line {
	var out []Line
	row := ""
	for _, w := range strings.Fields(s.T.String()) {
		if row != "" && cells(termtext.From(row+" "+w)) > width {
			out = append(out, Line{{T: termtext.From(row), C: s.C}})
			row = ""
		}
		if row != "" {
			row += " "
		}
		row += w
	}
	return append(out, Line{{T: termtext.From(row), C: s.C}})
}

// jsonKey names the answer, and the width, an overlay's JSON was laid
// out from.
type jsonKey struct {
	v     View
	at    time.Time
	len   int
	width int
}

// Printable is what P prints to stdout once the screen is gone, for v
// with the overlay that is open (0 none, 1 command, 2 JSON), and false
// where there is nothing to print: the command line; the box's answer
// as the command's --json prints it for this view (termtext.JSON of
// its body: the same value, with the characters a terminal acts on
// written as JSON's escapes, and a line feed after it when it has
// none), which is printed so only where stdout is not a terminal
// (PrintShown); or, on the refusal screen of a refused key, the person
// line.
func (s *Session) Printable(v View, overlay int) ([]byte, bool) {
	if !s.CanPrint(v, overlay) {
		return nil, false
	}
	switch {
	case v.Screen == ScreenRefused:
		return []byte(s.PersonLine() + "\n"), true
	case overlay == 1:
		c, _ := s.Command(v)
		return []byte(c + "\n"), true
	}
	body, _, _ := s.Body(v)
	out := append([]byte(nil), termtext.JSON(body)...)
	if len(out) == 0 || out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, true
}

// PrintShown is what P prints of the box's answer when stdout is a
// terminal: JSONShown, made safe; false where P prints only the
// client's own words (the command line, the person line), which need no
// second form.
func (s *Session) PrintShown(v View, overlay int) (string, bool) {
	if overlay != 2 || !s.CanPrint(v, overlay) {
		return "", false
	}
	body, _, _ := s.Body(v)
	return JSONShown(body), true
}

// CanPrint says, cheaply, whether P would print anything: the footer
// asks it at every frame.
func (s *Session) CanPrint(v View, overlay int) bool {
	switch {
	case v.Screen == ScreenRefused:
		return s.Engine.Stopped() == SignedOut
	case overlay == 1:
		_, ok := screenReads[v.Screen]
		return ok
	case overlay == 2:
		_, ok := screenReads[v.Screen]
		_, _, have := s.Body(v)
		return ok && have
	}
	return false
}

func size(n int) string {
	if n < 1000 {
		return strconv.Itoa(n) + " B"
	}
	return num(float64(n)/1000, 1) + " KB"
}

// Title is the title bar at now: who and where on the left, and the
// freshness word on the right, which is never colour alone.
func (s *Session) Title(now time.Time) (Line, Line) {
	left := Line{ownC("motel", Strong)}
	if s.Demo {
		left = append(left, ownC(" · SAMPLE DATA", Warn))
	}
	left = append(left, own(" · "+s.Host))
	switch {
	case s.Engine.Stopped() == NotThisKey || s.Me != nil && !s.Trusted():
		// The command's rule: an answer about another key or box names
		// no one here, and nothing said before it is kept (forget).
		left = append(left, ownC(" │ the box answered about another key or box", Warn))
	case s.Me != nil:
		if r := s.Me.V.Version; r != "" {
			left = append(left, own(" · "+r))
		}
		left = append(left, own(" │ "+s.Me.V.Name+" · "+s.scopes()))
	default:
		left = append(left, own(" │ not signed in"))
	}
	return left, s.freshness(now)
}

// scopes says the roles held: "viewer of the box", "viewer of blog, shop".
func (s *Session) scopes() string {
	byRole := map[string][]string{}
	var roles []string
	for _, r := range s.Me.V.Roles {
		if _, ok := byRole[r.Role]; !ok {
			roles = append(roles, r.Role)
		}
		on := "the box"
		if n, ok := strings.CutPrefix(r.Scope, "app:"); ok {
			on = n
		}
		byRole[r.Role] = append(byRole[r.Role], on)
	}
	if len(roles) == 0 {
		return "no grant here"
	}
	var parts []string
	for _, r := range roles {
		parts = append(parts, r+" of "+strings.Join(byRole[r], ", "))
	}
	return strings.Join(parts, "; ")
}

func (s *Session) freshness(now time.Time) Line {
	h := s.Engine.Health(now)
	if h.Back {
		s.backAt = now
	}
	ago := "never"
	if !s.last.IsZero() {
		ago = Ago(now.Sub(s.last)) + " ago"
	}
	switch {
	case s.Signing && s.Key.Hardware:
		return Line{ownC("TOUCH YOUR KEY to sign", Warn)}
	case h.Stop == SignedOut:
		return Line{ownC("SIGNED OUT: the box refused the key (401) · r asks again", Bad)}
	case h.Stop == NotSigned:
		return Line{ownC("NOT SIGNED: r signs again", Bad)}
	case h.Stop == BadCertificate:
		return Line{ownC("STOPPED: the certificate did not verify · r tries again", Bad)}
	case h.Stop == NotThisKey:
		return Line{ownC("STOPPED: the box answered about another key or box · r asks again", Bad)}
	case h.Paused:
		return Line{ownC("PAUSED (idle 10m): any key resumes", Warn)}
	case h.SlowIn > 0:
		// The box's word, an hour if it says so: until when, counting
		// down.
		return Line{ownC("slowed by the box (429) until "+hms(now.Add(h.SlowIn))+" UTC · "+Ago(h.SlowIn)+" left", Warn)}
	case h.Misses >= StaleAfter:
		l := "STALE · " + ago
		if !s.last.IsZero() {
			l += ": no answer since " + hms(s.last)
		}
		return Line{ownC(l+", retry in "+Ago(h.RetryIn), Bad)}
	case h.Misses > 0:
		return Line{ownC("slow · "+ago, Warn)}
	case !s.backAt.IsZero() && now.Sub(s.backAt) < 5*time.Second:
		return Line{ownC("back · "+hms(s.backAt), Good)}
	case !s.refusedAt.IsZero() && s.last.IsZero():
		return Line{ownC("REFUSED: no read answered yet", Bad)}
	case !s.refusedAt.IsZero() && !s.refusedAt.Before(s.last):
		// The last read was refused, and the screen says why: the word
		// does not say live of answers that stopped coming.
		return Line{ownC("refused · last answer "+ago, Warn)}
	case s.last.IsZero():
		return Line{own("asking…")}
	}
	return Line{own("live · " + ago)}
}

// MeView is who am I.
func (s *Session) MeView(now time.Time) []Line {
	if s.Me == nil {
		return []Line{{own("asking " + s.Host + " who your key is…")}}
	}
	me := s.Me.V
	if keyDiffers, boxDiffers := AboutAnother(me, s.Key.Fingerprint, s.Host); keyDiffers || boxDiffers {
		var out []Line
		if keyDiffers {
			out = append(out, Line{ownC("warning  ", Warn), own("the box answered about another key than the one this TUI signed with (" + s.Key.Fingerprint + "); it names "), quoted(me.Key)})
		}
		if boxDiffers {
			out = append(out, Line{ownC("warning  ", Warn), own("the box answered as another box than the one asked (" + s.Host + "); it names "), quoted(me.Box)})
		}
		return append(out, nil, Line{own("An answer that is not about this key here is not one to act on: nothing in it is shown as who you are.")})
	}
	out := []Line{field("you are", own(me.Name))}
	out = append(out, field("your key", own(s.Key.Fingerprint)))
	k := "             " + s.Key.Shown + " (" + s.Key.Source + ")"
	if s.Key.Hardware {
		k += " · a hardware key: each signing needs a touch"
	}
	out = append(out, Line{own(k)})
	rel := "moteld " + me.Version
	if me.Version == "" {
		rel = "a moteld build that names no release"
	}
	out = append(out, field("box", own(s.Host+" · "+rel+" · protocol "+strconv.Itoa(me.Protocol)+" · client motel "+s.Version)))
	if s.TokenExpires != nil {
		if exp := s.TokenExpires(); !exp.IsZero() {
			if left := exp.Sub(now); left > 0 {
				out = append(out, field("token", own("good until "+hms(exp)+" UTC ("+Ago(left)+" left)")),
					Line{own("             signed again a minute before it ends, and only while you use the screen")})
			} else {
				out = append(out, field("token", own("expired at "+hms(exp)+" UTC; signed again on the next read")))
			}
		}
	}
	out = append(out, nil, Line{ownC("from here you may", Label)})
	if len(me.Roles) == 0 {
		out = append(out, Line{own("  nothing yet: signed in as " + me.Name + ", you hold no grant here")})
	}
	for _, r := range me.Roles {
		on := "the box"
		if n, ok := strings.CutPrefix(r.Scope, "app:"); ok {
			on = "app " + n
		}
		verb := r.Role
		if verb == "viewer" {
			verb = "view"
		}
		out = append(out, Line{own("  " + pad(verb, 10) + " " + on)})
	}
	out = append(out, nil,
		field("you may not", own("roll back, relaunch, deploy or push: no person may yet. Deploys come from CI.")),
		Line{own("             Read logs, alerts and backups: this client does not offer them yet.")}, nil,
		Line{own("What the box knows of you is in its Caddyfile (person and grant lines), which the")},
		Line{own("operator keeps. `motel key` prints the line to send them.")})
	return out
}

// Answered says whether the box has answered any read this session: a
// stop after one is a stop, not a first run.
func (s *Session) Answered() bool { return !s.last.IsZero() }

// MeAsOf is how old who am I's answer is, for its crumb.
func (s *Session) MeAsOf(now time.Time) Line {
	if s.Me == nil {
		return nil
	}
	return s.asOf(s.Me.At, now)
}

// RecordAsOf is how old a deploy's record is, for its crumb: it is read
// once, as a record does not change, and ctrl-r reads it again.
func (s *Session) RecordAsOf(app, version string, now time.Time) Line {
	if g, _, ok := s.Record(app, version); ok && g != nil {
		return s.asOf(g.At, now)
	}
	return nil
}

// BoxAsOf is how old the box's answer is, for its crumb.
func (s *Session) BoxAsOf(now time.Time) Line {
	if s.BoxSt == nil {
		return nil
	}
	return s.asOf(s.BoxSt.At, now)
}

// BoxView is the box screen.
func (s *Session) BoxView(now time.Time) []Line {
	switch {
	case s.BoxSt == nil && s.BoxErr != nil:
		return []Line{{Seg{T: Capped(failureText(s.BoxErr), 4*LineMax)}}}
	case s.BoxSt == nil:
		return []Line{{own("asking " + s.Host + " what it runs…")}}
	}
	// The box's answer alone, whose age the crumb gives (BoxAsOf): the
	// release is whoami's, shown with its own in who am I and the title
	// bar.
	b := s.BoxSt.V
	out := []Line{field("runs commit", own(b.Commit)), field("Caddyfile", own("sha256 "+b.SHA256))}
	if b.Recipient != "" {
		out = append(out, field("secrets key", boxT(b.Recipient)))
	}
	out = append(out, field("deploy host", own(s.Host)), nil,
		Line{own("Not shown here, root's over ssh: moteld's journal (journalctl -u moteld),")},
		Line{own("the last push's log (journalctl -u motel-box-apply), systemctl --failed,")},
		Line{own("host load (btop).")})
	if s.BoxErr != nil {
		out = append(out, nil, Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(s.BoxErr), 2*LineMax)}})
	}
	return out
}

// RefusedView is the first-run and refusal screen: the client's own
// message for what happened, and for a refused key the facts to send
// the operator.
func (s *Session) RefusedView() []Line {
	err := s.StopErr
	if err == nil {
		err = s.MeErr
	}
	out := []Line{{Seg{T: termtext.From(failureText(err))}}, nil}
	h := s.Engine.Health(s.Now())
	if h.Stop == SignedOut {
		out = append(out, field("your key", own(s.Key.Fingerprint)),
			Line{own("            " + s.Key.Shown + " (" + s.Key.Source + ")")}, nil,
			Line{own("To be added, send the operator this line (motel key prints it too):")},
			Line{own("  person <name> " + s.Key.Line)},
			Line{own("  (one line when sent; P quits and prints it whole below your prompt)")}, nil)
	}
	return append(out, Line{own("r  try again (once; each refusal counts against the box's budget)     q  quit")})
}

// PersonLine is what P prints on the refusal screen.
func (s *Session) PersonLine() string { return "person <name> " + s.Key.Line }
