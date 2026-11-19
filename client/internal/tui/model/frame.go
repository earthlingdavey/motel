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
// a flag. A record whose version is outside the version grammar (one
// the box's filter hid, which the deploys table lists in the client's
// words) has none: no command could ask for it.
func (s *Session) CommandArgs(v View) ([]string, bool) { return s.commandArgs(v, false) }

func (s *Session) commandArgs(v View, json bool) ([]string, bool) {
	k, reads := screenReads[v.Screen]
	a, ok := ActionFor(k)
	if !reads || !ok || hiddenVersion(v) {
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
	// sshkey.SourceFlag and SourceEnv: the person named it, and a shell
	// without the TUI's MOTEL_KEY would find another key, or none.
	if s.Key.Source == "--key" || s.Key.Source == "MOTEL_KEY" {
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

// hiddenVersion says whether v is a record whose version no command
// can name: only one the box's filter hid reaches a screen so, since
// the client refuses any other outside the grammar, and so does the :
// line.
func hiddenVersion(v View) bool { return v.Screen == ScreenRecord && !ValidVersion(v.Version) }

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

// readErr is the failure of the newest read behind v, if it failed.
func (s *Session) readErr(v View) error {
	switch v.Screen {
	case ScreenApps:
		return s.ListErr
	case ScreenApp:
		if a := s.apps[v.App]; a != nil {
			return a.err
		}
	case ScreenRecord:
		_, err, _ := s.Record(v.App, v.Version)
		return err
	case ScreenMe:
		return s.MeErr
	case ScreenBox:
		return s.BoxErr
	}
	return nil
}

// NoAnswer is what the JSON overlay over v says when no answer to it
// is held (Body's false): it changes as a read ends, when Body does
// not, so the screens make the overlay again when it does.
func (s *Session) NoAnswer(v View) string {
	switch err := s.readErr(v); {
	case Forbidden(err):
		// A 403 or a 404 drops what was held (readStatus, readDeploy):
		// the box did answer, so not "yet". Each is said in the
		// screen's own words for it.
		return "no answer to show: the box refused this read, as the screen says"
	case NotFound(err):
		return "no answer to show: the box answered this read 404, as the screen says"
	}
	return "no answer yet to show"
}

// Cells is how many cells of the screen a text takes. The screens'
// library knows (a wide character takes two); model draws nothing, so
// the screens hand it in.
type Cells func(termtext.Text) int

// AsIs says whether the screens draw a text as it is, character for
// character. They do not always: the door writes some characters as
// escapes (an emoji modifier as \U0001f3fd), and nothing is drawn for
// a character of no width (a soft hyphen, a zero-width space), so a
// command line so drawn, selected and copied, is another command. The
// screens hand it in, as Cells.
type AsIs func(termtext.Text) bool

// Overlay is the command and JSON overlay for v: the command, then,
// when json, the box's answer. The JSON is indented once per answer and
// width, not at every frame: a large body would make every frame slow.
// width is the room inside the overlay's border, in cells; every row is
// laid out to it by lay. A command line wider than it is cut there, the
// cut marked, and the overlay says not to copy it and what prints it
// whole, in place of asking to select it; so it does of a command line
// the screens do not draw as it is (asIs), which copied would be
// another command.
func (s *Session) Overlay(v View, json bool, width int, cells Cells, asIs AsIs) []Line {
	c, ok := s.Command(v)
	switch {
	case !ok && hiddenVersion(v):
		return lay([]oRow{words("the box's filter hid this deploy's version: no motel command can ask for its record")}, width, cells)
	case !ok:
		return lay([]oRow{words("this screen reads nothing from the box")}, width, cells)
	}
	body, at, have := s.Body(v)
	if !json {
		cmdJSON := s.commandJSON(v)
		cmds := []oRow{command("  " + c), command("  " + cmdJSON)}
		rows := append([]oRow{words("the same read from a shell:"), blank}, append(cmds, blank)...)
		// Of the command, its --json form and a row's: each cut at the
		// window's edge, and each the screens do not draw as it is.
		cut := [3]bool{!cmds[0].fits(width, cells), !cmds[1].fits(width, cells)}
		other := [3]bool{!drawnAsIs(c, asIs), !drawnAsIs(cmdJSON, asIs)}
		if v.Screen == ScreenApps {
			// A row's command as commandArgs makes it, --key among its
			// flags, the app left as the placeholder <app>, unquoted.
			args, _ := s.commandArgs(View{Screen: ScreenApp}, false)
			line := ShellLine(args[:len(args)-1]) + " <app>"
			tmpl := command("  " + line)
			cut[2], other[2] = !tmpl.fits(width, cells), !drawnAsIs(line, asIs)
			rows = append(rows, words("This screen is that list, and a row for each app: each row is"), tmpl, blank)
		}
		bad := func(i int) bool { return cut[i] || other[i] }
		if bad(0) || bad(1) || bad(2) {
			// P prints the first command alone (Printable).
			var w []string
			if cut[0] || cut[1] || cut[2] {
				w = append(w, "a line above is cut at the window's edge, marked …: do not copy it.")
			}
			if other[0] || other[1] || other[2] {
				w = append(w, "a line above is not drawn as it runs (a character written as an escape, or one not drawn at all): do not copy it.")
			}
			if bad(0) || bad(1) {
				p := "P quits and prints the first command whole below your prompt"
				if bad(1) {
					p += "; the second is it with --json among its flags"
				}
				w = append(w, p+".")
			}
			if bad(2) {
				w = append(w, "c then P on an app's own screen prints a row's command whole.")
			}
			rows = append(rows, warn(strings.Join(w, " ")))
		} else {
			rows = append(rows, words("select it with the mouse, or press P to quit and print it below your prompt"))
		}
		if have {
			rows = append(rows, words("y: the box's answer as JSON ("+size(len(body))+", fetched "+hms(at)+" UTC)"))
		}
		return lay(rows, width, cells)
	}
	if !have {
		return lay([]oRow{words(s.NoAnswer(v))}, width, cells)
	}
	key := jsonKey{v, at, len(body), width}
	if s.json.key != key || s.json.lines == nil {
		s.json.key, s.json.lines = key, nil
		lines, more, indented, deep := jsonLines(body)
		head := "indented here; its strings as received, made safe to show;"
		if !indented {
			head = "not JSON that indents: shown line by line as it came, made safe to show;"
		}
		cmd := command("  " + c)
		rows := []oRow{words("the box's answer to"), cmd}
		var w []string
		if !cmd.fits(width, cells) {
			w = append(w, "the line above is cut at the window's edge, marked …: do not copy it.")
		}
		if !drawnAsIs(c, asIs) {
			w = append(w, "the line above is not drawn as it runs (a character written as an escape, or one not drawn at all): do not copy it.")
		}
		if len(w) > 0 {
			// c works with this overlay open, and P there prints the
			// command (Printable).
			rows = append(rows, warn(strings.Join(append(w, "c then P prints it whole."), " ")))
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
		if deep {
			rows = append(rows, warn("a line below is nested deeper than "+strconv.Itoa(IndentMax)+" levels and indented as at "+strconv.Itoa(IndentMax)+", here and as P prints it"))
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

// drawnAsIs says whether a command line is drawn as it is, character
// for character: made safe to show, it is unchanged, and the screens
// draw it so (asIs).
func drawnAsIs(line string, asIs AsIs) bool {
	t := termtext.From(line)
	return t.String() == line && asIs(t)
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
		return ok && !hiddenVersion(v)
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
	s.seen(now)
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
	case s.Me != nil && s.MeErr != nil && s.Engine.Stopped() == Running:
		// whoami's newest read failed while the other reads answer: that
		// it failed, then the age of the answer held, before it, so the
		// mark is cut after the host and not before (at 60 columns
		// little else of the answer fits). The mark names whoami, so it
		// is not read as the release's or the person's. "failed" is true
		// of each way it can: no answer, a refusal, an answer the client
		// cannot use. Who am I says which.
		left = append(left, own(" │ "), ownC("whoami failed", Warn), own("; "+Ago(now.Sub(s.Me.At))+" old: "))
		if r := s.Me.V.Version; r != "" {
			left = append(left, own(r+" · "))
		}
		left = append(append(left, own(s.Me.V.Name+" · ")), s.scopes()...)
	case s.Me != nil:
		if r := s.Me.V.Version; r != "" {
			left = append(left, own(" · "+r))
		}
		left = append(append(left, own(" │ "+s.Me.V.Name+" · ")), s.scopes()...)
	case s.MeErr != nil && s.Engine.Stopped() == Running:
		// The box answers the other reads, but not whoami as the client
		// takes it (a release that is not N.N.N): signed in, as no one
		// known.
		left = append(left, ownC(" │ who you are is not known: ", Warn), Seg{T: Capped(failureText(s.MeErr), 2*LineMax)})
	default:
		left = append(left, own(" │ not signed in"))
	}
	return left, s.freshness(now)
}

// scopes says the roles held: "viewer of the box", "viewer of blog, shop".
func (s *Session) scopes() Line {
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
		return Line{own("no grant here")}
	}
	var out Line
	for i, r := range roles {
		if i > 0 {
			out = append(out, own("; "))
		}
		out = append(out, roleSeg(r), own(" of "+strings.Join(byRole[r], ", ")))
	}
	return out
}

// roleSeg is a role as the whoami command says it: viewer, the one role
// a box grants today, as a word; any other is the box's own word, which
// the client took by its shape alone, and is quoted.
func roleSeg(role string) Seg {
	if role == "viewer" {
		return own(role)
	}
	return quoted(role)
}

func (s *Session) freshness(now time.Time) Line {
	s.seen(now)
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
		// 57 cells: every word here fits the smallest window, 60, the
		// left side cut before it (TestTitleWordsAt60).
		return Line{ownC("STOPPED: answered about another key or box · r asks again", Bad)}
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
	case h.Waiting >= WaitingAfter:
		// A read not answered yet: not live while the box is waited for.
		return Line{ownC("waiting "+Ago(h.Waiting)+" for an answer · "+ago, Warn)}
	case !s.backAt.IsZero() && now.Sub(s.backAt) < 5*time.Second:
		return Line{ownC("back · "+hms(s.backAt), Good)}
	case s.refused && s.last.IsZero():
		return Line{ownC("REFUSED: no read answered yet", Bad)}
	case s.refused:
		// The last read was refused, and the screen says why: the word
		// does not say live of answers that stopped coming.
		return Line{ownC("refused · last answer "+ago, Warn)}
	case s.last.IsZero():
		return Line{own("asking…")}
	}
	return Line{own("live · " + ago)}
}

// MeView is who am I, its lines whole.
func (s *Session) MeView(now time.Time) []Line { return s.MeViewFit(now, 0, nil) }

// MeViewFit is who am I laid to width cells (laid); 0 leaves each line
// one line.
func (s *Session) MeViewFit(now time.Time, width int, cells Cells) []Line {
	switch {
	case s.Me == nil && s.MeErr != nil:
		return Sentence(Line{own("who you are is not known: "), Seg{T: Capped(failureText(s.MeErr), 4*LineMax)}}, width, cells)
	case s.Me == nil:
		return Sentence(Line{own("asking " + s.Host + " who your key is…")}, width, cells)
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
	// The lines that hold the box's values (the name, the release and
	// protocol, the grants) are cut; the rest are the client's words.
	out := []Line{cutTo(field("you are", own(me.Name)), width, cells)}
	out = append(out, Sentence(field("your key", own(s.Key.Fingerprint)), width, cells)...)
	k := "             " + s.Key.Shown + " (" + s.Key.Source + ")"
	if s.Key.Hardware {
		k += " · a hardware key: each signing needs a touch"
	}
	out = append(out, Sentence(Line{own(k)}, width, cells)...)
	rel := "moteld " + me.Version
	if me.Version == "" {
		rel = "a moteld build that names no release"
	}
	out = append(out, cutTo(field("box", own(s.Host+" · "+rel+" · protocol "+strconv.Itoa(me.Protocol)+" · client motel "+s.Version)), width, cells))
	if s.TokenExpires != nil {
		if exp := s.TokenExpires(); !exp.IsZero() {
			if left := exp.Sub(now); left > 0 {
				out = append(out, laid(width, cells, field("token", own("good until "+hms(exp)+" UTC ("+Ago(left)+" left)")),
					Line{own("             signed again a minute before it ends, and only while you use the screen")})...)
			} else {
				out = append(out, Sentence(field("token", own("expired at "+hms(exp)+" UTC; signed again on the next read")), width, cells)...)
			}
		}
	}
	out = append(out, nil, Line{ownC("from here you may", Label)})
	if len(me.Roles) == 0 {
		out = append(out, cutTo(Line{own("  nothing yet: signed in as " + me.Name + ", you hold no grant here")}, width, cells))
	}
	for _, r := range me.Roles {
		on := "the box"
		if n, ok := strings.CutPrefix(r.Scope, "app:"); ok {
			on = "app " + n
		}
		verb := roleSeg(r.Role)
		if r.Role == "viewer" {
			verb = own("view")
		}
		out = append(out, cutTo(Line{own("  "), verb, own(pad("", 10-len([]rune(verb.T.String()))) + " " + on)}, width, cells))
	}
	out = append(out, laid(width, cells, nil,
		field("you may not", own("roll back, relaunch, deploy or push: no person may yet. Deploys come from CI.")),
		Line{own("             Read logs, alerts and backups: this client does not offer them yet.")}, nil,
		Line{own("What the box knows of you is in its Caddyfile (person and grant lines), which the operator keeps. `motel key` prints the line to send them.")})...)
	if s.MeErr != nil {
		out = append(append(out, nil), Sentence(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(s.MeErr), 2*LineMax)}}, width, cells)...)
	}
	return out
}

// Answered says whether the box has answered any read this session: a
// stop after one is a stop, not a first run.
func (s *Session) Answered() bool { return !s.last.IsZero() }

// MeAsOf is how old who am I's answer is, for its crumb.
func (s *Session) MeAsOf(now time.Time) Line {
	s.seen(now)
	if s.Me == nil {
		return nil
	}
	return s.asOf(s.Me.At, now)
}

// RecordAsOf is how old a deploy's record is, for its crumb: it is read
// on opening it, and by ctrl-r, and kept when a later read fails, but
// for a 403 or a 404, which the screen says alone (RecordView).
func (s *Session) RecordAsOf(app, version string, now time.Time) Line {
	s.seen(now)
	if g, err, ok := s.Record(app, version); ok && g != nil && !Forbidden(err) && !NotFound(err) {
		l := s.asOf(g.At, now)
		if err != nil {
			l = append(l, ownC(" · the last read failed", Warn))
		}
		return l
	}
	return nil
}

// BoxAsOf is how old the box's answer is, for its crumb.
func (s *Session) BoxAsOf(now time.Time) Line {
	s.seen(now)
	if s.BoxSt == nil {
		return nil
	}
	return s.asOf(s.BoxSt.At, now)
}

// BoxView is the box screen, its lines whole.
func (s *Session) BoxView(now time.Time) []Line { return s.BoxViewFit(now, 0, nil) }

// BoxViewFit is the box screen laid to width cells (laid); 0 leaves each
// line one line.
func (s *Session) BoxViewFit(now time.Time, width int, cells Cells) []Line {
	switch {
	case s.BoxSt == nil && s.BoxErr != nil:
		return Sentence(Line{{T: Capped(failureText(s.BoxErr), 4*LineMax)}}, width, cells)
	case s.BoxSt == nil:
		return Sentence(Line{own("asking " + s.Host + " what it runs…")}, width, cells)
	}
	// The box's answer alone, whose age the crumb gives (BoxAsOf). The
	// release is whoami's: who am I shows it under whoami's own age, and
	// the title bar beside the session's freshness word.
	b := s.BoxSt.V
	// The box's values are cut; the rest are the client's words. The
	// Caddyfile's sha256 stands alone after its label, so that at 80
	// columns it is whole.
	out := []Line{cutTo(field("runs commit", own(b.Commit)), width, cells), cutTo(field("Caddyfile", own(b.SHA256)), width, cells)}
	if b.OutOfBand {
		out = append(out, laid(width, cells, field("", ownC("is not the file that commit installed", Warn)))...)
	}
	if b.Recipient != "" {
		out = append(out, cutTo(field("secrets key", boxT(b.Recipient)), width, cells))
	}
	out = append(out, laid(width, cells, field("deploy host", own(s.Host)), nil,
		Line{own("Not shown here, root's over ssh: moteld's journal (journalctl -u moteld), the last push's log (journalctl -u motel-box-apply), systemctl --failed, host load (btop).")})...)
	if s.BoxErr != nil {
		out = append(append(out, nil), Sentence(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(s.BoxErr), 2*LineMax)}}, width, cells)...)
	}
	return out
}

// laid is each of ls laid on rows of width cells (Sentence). A line that
// holds a box's value is cut to width before it is given here (cutTo),
// so it fits, and box text never begins a row.
func laid(width int, cells Cells, ls ...Line) []Line {
	var out []Line
	for _, l := range ls {
		out = append(out, Sentence(l, width, cells)...)
	}
	return out
}

// cutTo is l cut to width cells, … its last cell (Line.Cut); 0 leaves it
// whole.
func cutTo(l Line, width int, cells Cells) Line {
	if width <= 0 {
		return l
	}
	return l.Cut(width, cells)
}

// Sentence is a sentence of the client's (a failure's, which may quote
// the box) laid on rows of at most width cells, broken at spaces as lay
// breaks the overlays' words, and a word wider than a row broken where
// the row ends; a line that fits, or a width of 0, is the line as it
// is. The spaces the line begins with, and those between two words on
// one row, are kept (a label's padding). Every row after the first is
// indented two spaces more than the line begins: a row that goes on
// inside a quotation then never begins where a line of the screen's
// own does, so it reads as the sentence's, and the quotation as one.
func Sentence(l Line, width int, cells Cells) []Line {
	if width <= 0 || l.Width(cells) <= width {
		return []Line{l}
	}
	type piece struct {
		s string
		c Class
	}
	// The words, each its pieces of one class or more and the spaces
	// before it, in order.
	type word struct {
		gap string
		ps  []piece
	}
	var words []word
	var w word
	for _, sg := range l {
		for i, p := range strings.Split(sg.T.String(), " ") {
			if i > 0 {
				if len(w.ps) > 0 {
					words, w = append(words, w), word{}
				}
				w.gap += " "
			}
			if p != "" {
				w.ps = append(w.ps, piece{p, sg.C})
			}
		}
	}
	if len(w.ps) > 0 {
		words = append(words, w)
	}
	if len(words) == 0 {
		return []Line{l}
	}
	lead := words[0].gap
	indent := lead + "  "
	var out []Line
	row := Line{}
	if lead != "" {
		row = Line{own(lead)}
	}
	add := func(p piece) {
		if n := len(row); n > 0 && row[n-1].C == p.c {
			row[n-1].T = termtext.From(row[n-1].T.String() + p.s)
			return
		}
		row = append(row, Seg{T: termtext.From(p.s), C: p.c})
	}
	fits := func(extra string) bool { return cells(termtext.From(row.Text()+extra)) <= width }
	empty := func() bool { return strings.TrimLeft(row.Text(), " ") == "" }
	next := func() {
		out, row = append(out, row), Line{own(indent)}
	}
	for _, w := range words {
		whole := ""
		for _, p := range w.ps {
			whole += p.s
		}
		if !empty() && !fits(w.gap+whole) {
			next()
		}
		if !empty() {
			add(piece{w.gap, Plain})
		}
		for _, p := range w.ps {
			// A word wider than a row is broken where the row ends.
			for rest := p.s; rest != ""; {
				end := 0
				for i := range rest {
					if !fits(rest[:i]) {
						break
					}
					end = i
				}
				if fits(rest) {
					end = len(rest)
				}
				if end == 0 {
					if empty() {
						// Not one character fits beside the indent: one
						// goes anyway.
						end = len(rest)
						for i := range rest {
							if i > 0 {
								end = i
								break
							}
						}
					} else {
						next()
						continue
					}
				}
				add(piece{rest[:end], p.c})
				rest = rest[end:]
			}
		}
	}
	return append(out, row)
}

// RefusedView is the first-run and refusal screen: the client's own
// message for what happened, and for a refused key the facts to send
// the operator.
func (s *Session) RefusedView(now time.Time) []Line {
	err := s.StopErr
	if err == nil {
		err = s.MeErr
	}
	out := []Line{{Seg{T: termtext.From(failureText(err))}}, nil}
	h := s.Engine.Health(now)
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
