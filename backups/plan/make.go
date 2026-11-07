package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// motel is the binary that adapts the Caddyfile. Absolute: a
// relative name is looked up on whatever PATH the caller has. A
// variable only so a test can stand a script in for it.
var motel = "/usr/bin/motel"

// Program is the path the plan unit runs motel from: what setup
// checks is installed before asking anyone for a secret.
func Program() string { return motel }

// kinds are the values a variable can be given here, in the order they
// are tried: a name of its own, then the other things a Caddyfile asks
// for — a number, a duration, a path. Two of each, because seeing
// whether the plan depends on a variable takes two values it adapts
// with. The first is made from the variable's name, so that when the
// adapter quotes a value it cannot use, the variable is known.
func kinds(name string) []string {
	own := "hsb-placeholder-" + slug(name)
	return []string{own + ".invalid", own + "-2.invalid", "1", "2", "1s", "2s", "/" + own, "/" + own + "-2"}
}

// slug is a variable's name as a made-up value carries it.
func slug(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, name)
}

// Inspection is a Caddyfile's plan, and what a person about to make
// that Caddyfile live should hear besides.
type Inspection struct {
	Plan *Plan
	// Undeclared are the apps that declare no backup, sorted: a new
	// app's block with the two lines forgotten is otherwise silent.
	Undeclared []string
	// UndeclaredByEnv are the environment variables that name an app
	// which declares no backup: what it is called on the server is not
	// known here, so it is not in Undeclared under a made-up name.
	UndeclaredByEnv []string
}

// adapted is what one adapt of a Caddyfile came to: the plan as the
// Caddyfile spells it under the environment given, not yet validated —
// a placeholder is not a valid root, and a plan that turns invalid under
// one has changed like any other — the apps that declare no backup, and
// what the adapter wrote, each stream whole.
type adapted struct {
	plan       *Plan
	undeclared []string
	out, said  string
}

// Inspect adapts the Caddyfile and returns its Plan, with the apps that
// declare no backup.
//
// An import glob that matches nothing is no error to the adapter, only a
// warning [measured]; here it is refused. A run adapts the Caddyfile
// inside a view that holds its directory and nothing else, where a glob
// reaching outside matches nothing — and so does one through a
// directory the run's account may not list, or a link out — and the
// apps declared there would simply not be in the plan. The adapter's
// own warning is exact where a reading of the Caddyfile here would not
// be: it comes after its snippets, heredocs and {$NAME}, from any depth
// of import. What it does not see is a glob that matches something and
// misses something else: a wildcard directory with a link out beside
// real ones.
//
// It does not have the environment motel adapts it in, and is not
// given it: that environment is where the server's secrets are. But the
// Caddyfile's own {$NAME} is substituted in the text by whoever adapts,
// before any module sees a token, and that matters twice.
//
// A variable with no default, unset, leaves nothing where its value
// would be, and `email {$ACME_EMAIL}` is then a parse error: the most
// ordinary production Caddyfile does not adapt in an empty environment
// [measured; set to an empty string it fails the same way]. So every
// such variable is given a placeholder — a made-up value, of whichever
// kind the adapter accepts there. The structure of what is adapted is
// the server's own; only those values differ, and none of them is
// anywhere a plan looks.
//
// Unless one is: a root or a backup path written with a variable means
// one thing to the server and another here, and the difference would
// show up as an app with "no data yet", for ever. So every variable is
// then given a second value, and if the plan comes out different, Make
// refuses, naming it. One that no second value adapts with is refused
// where a trial failed at an import: that is what
// `import sites/{$ENV:prod}.caddy` looks like from here, and the file
// the server imports may say anything. Where every trial was refused
// by a directive checking its argument's form — `protocols
// {$TLS_MIN:tls1.2}`, an `lb_policy`, a `redir` code — the trials say
// nothing of where else the variable is written, so each place it is
// written is tried on its own (probe): a copy of the Caddyfile with
// that one occurrence made a value of its own and the others their
// defaults, adapted by the adapter. One the adapter refuses, quoting
// the value and not through an import, is the directive's own; one
// that adapts and changes the plan is refused as a root's or a backup
// path's would be; one standing where
// a directive's name is read is refused before any is tried; and when
// none does either, the variable moves nothing a run does (#394). Caddy
// substitutes {$NAME} in the text before it reads a token, by the
// shape of match envRe is, so the occurrences found here are the ones
// it substitutes, whatever line, heredoc or quoted string they are in.
// Words that are neither the parser's about an import nor a
// directive's own are refused as before.
//
// What this does not show: the substitution is textual, so a value
// with a newline in it is more lines of Caddyfile, and can declare a
// backup the file does not [measured]. Every value tried here is one
// token; no set of them shows that no value restructures the file, and
// the server's values are not known here. A declaration that exists
// only through the server's environment is not in the plan.
func Inspect(ctx context.Context, caddyfile, configDir string) (*Inspection, error) {
	// adapt returns what adapting file under env came to, or what the
	// adapter said.
	adapt := func(env map[string]string, file string) (adapted, error) {
		//nolint:gosec // a constant path outside tests; the Caddyfile path is the caller's, a root-owned constant, or a copy of it made here
		cmd := exec.CommandContext(ctx, motel, "adapt", "--adapter", "caddyfile", "--config", file)
		cmd.Env = []string{} // never nil: nil means "inherit"
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		a := adapted{out: stdout.String(), said: stderr.String()}
		if runErr != nil {
			return a, fmt.Errorf("%s adapt: %w: %s", motel, runErr, lastLine(a.said))
		}
		if globs := emptyGlobs(stderr.Bytes()); len(globs) > 0 {
			return a, &emptyGlobError{globs}
		}
		var err error
		a.plan, a.undeclared, err = extractAll(stdout.Bytes())
		return a, err
	}

	// What is beside the live Caddyfile is what a run's view holds; beside
	// a copy checked anywhere else, it could be a whole tree.
	names, needs, within, err := envNames(caddyfile, filepath.Clean(filepath.Dir(caddyfile)) == filepath.Clean(configDir))
	if err != nil {
		return nil, err
	}

	// The base: nothing set but what has to be, each of those at the
	// first kind of value the adapter takes there. One variable moves on
	// by one kind a round — the one whose value the adapter quoted, if
	// it quoted one — so this ends.
	env, at := map[string]string{}, map[string]int{}
	for _, n := range needs {
		env[n] = kinds(n)[0]
	}
	quotes := func(s string) bool { // s holds some variable's value of the round
		return slices.ContainsFunc(needs, func(n string) bool { return strings.Contains(s, env[n]) })
	}
	var base *Plan
	var undeclared []string // as the base adapt has them
	for {
		a, err := adapt(env, caddyfile)
		if err == nil {
			base, undeclared = a.plan, a.undeclared
			break
		}
		said := a.said
		// An import that fails with no made-up value in its own words — a
		// glob matching nothing, a path that is not there — is no
		// variable's doing: a literal one, or a default's file. Its own
		// words, not all of stderr: a literal glob's warning sits beside
		// the directive's error that is this round's.
		words, atImport := importWords(err, said)
		if atImport && !quotes(words) {
			return nil, err
		}
		moved := false
		for pass := 0; pass < 2 && !moved; pass++ {
			for _, n := range needs {
				quoted := strings.Contains(said, env[n])
				if (pass == 0) != quoted || at[n]+2 >= len(kinds(n)) {
					continue
				}
				at[n] += 2 // the next kind; the odd ones are each kind's second value
				env[n], moved = kinds(n)[at[n]], true
				break
			}
		}
		if !moved {
			hint := ""
			switch {
			case len(needs) == 0:
			case atImport:
				// A default would not settle it: with one, the variable is
				// refused below as an import's. Named: those whose value the
				// import's words hold, which is where it did not adapt.
				blamed := slices.DeleteFunc(slices.Clone(needs), func(n string) bool { return !strings.Contains(words, env[n]) })
				hint = fmt.Sprintf(" (it was tried with made-up values for %s, which have no default, and the import's path is made of one: what the server imports there cannot be read here, since a backup reads the Caddyfile without motel's environment — write the import literally)", strings.Join(blamed, ", "))
			default:
				hint = fmt.Sprintf(" (it was tried with made-up values for %s, which have no default: a backup reads the Caddyfile without motel's environment; a default the Caddyfile adapts with, `{$NAME:value}`, settles it)", strings.Join(needs, ", "))
			}
			return nil, fmt.Errorf("%w%s", err, hint)
		}
	}
	// A plan with no app declaring a backup has no root: a run backs
	// nothing up, and nothing downstream is to read a placeholder, or a
	// path, as where anything lives — whatever the root says, {env.X}
	// included, which the adapter prints as written.
	if len(base.Apps) == 0 {
		base.Root = ""
	}
	if err := base.Validate(); err != nil {
		if len(needs) == 0 {
			return nil, err
		}
		// Fall through: a root that is a placeholder is about to be named
		// as depending on its variable, which is the better thing to say.
	}

	var decisive, imported, opaque, byEnv []string
	displaced := map[string]bool{} // undeclared names that some variable's value changes
	// judge says whether an adapt with the variable at another value
	// changes what a run does, keeping what it says of undeclared apps.
	// An app that declares no backup and is named through the variable
	// changes nothing a run does, so it is no refusal; but its name here
	// is a default's, or a placeholder. Every trial is held against the
	// base's names as they were: the names a variable displaces are taken
	// out after the last one.
	judge := func(name string, trial adapted) bool {
		if !slices.Equal(undeclared, trial.undeclared) {
			if !slices.Contains(byEnv, name) {
				byEnv = append(byEnv, name)
			}
			for _, n := range undeclared {
				if !slices.Contains(trial.undeclared, n) {
					displaced[n] = true
				}
			}
		}
		return changesARun(base, trial.plan)
	}
	for _, name := range names {
		if strings.ContainsAny(name, "=\x00") {
			continue // cannot be set, so the server does not have it set either
		}
		// The first value that adapts decides; until one does, what the
		// trials failed at is kept, for the refusal it may come to.
		oneAdapted, changes, atImport, elsewhere := false, false, false, false
		for _, value := range kinds(name) {
			if value == env[name] {
				continue
			}
			trialEnv := map[string]string{name: value}
			for k, v := range env {
				if k != name {
					trialEnv[k] = v
				}
			}
			trial, err := adapt(trialEnv, caddyfile)
			if err != nil {
				imp, dir := failedAtImport(trial.said), failedAtDirective(trial.said)
				atImport = atImport || imp
				elsewhere = elsewhere || (!imp && !dir)
				continue
			}
			oneAdapted, changes = true, judge(name, trial)
			break
		}
		switch {
		case changes:
			decisive = append(decisive, name)
		case oneAdapted:
		case atImport:
			// The parser meets an import before any directive sees an
			// argument, so this is the import's, whatever else the
			// variable is in.
			imported = append(imported, name)
		case elsewhere:
			opaque = append(opaque, name)
		default:
			// Every value was refused by a directive checking its form,
			// which says nothing of where else the variable is written:
			// each place is tried on its own.
			verdict, err := probe(caddyfile, name, within[name], env, adapt, func(trial adapted) bool { return judge(name, trial) })
			if err != nil {
				return nil, err
			}
			switch verdict {
			case moves:
				decisive = append(decisive, name)
			case unknown:
				opaque = append(opaque, name)
			case inert:
			}
		}
	}
	if len(decisive) > 0 {
		return nil, fmt.Errorf("the liveswap root or a backup path depends on the environment variable(s) %s through the Caddyfile's {$NAME}; a backup reads the Caddyfile without motel's environment, so it would look somewhere else than the server does — write those values literally", strings.Join(decisive, ", "))
	}
	if len(imported) > 0 {
		return nil, fmt.Errorf("the Caddyfile imports through the environment variable(s) %s: with any other value the import names a file that is not there, so what the server imports when they are set cannot be known here; a backup reads the Caddyfile without motel's environment — write the import literally", strings.Join(imported, ", "))
	}
	if len(opaque) > 0 {
		return nil, fmt.Errorf("the Caddyfile does not adapt with the environment variable(s) %s set to any value tried, and where else they are read could not be told here — a value written in a snippet or an imported file, or carried to one by its argument, a word standing where a directive's name is read, a file the adapter reaches past the Caddyfile's directory or beside a copy checked away from its own, or words that are neither a directive's nor an import's — so what the server reads when they are set cannot be known; a backup reads the Caddyfile without motel's environment — write the root, app names, backup paths and imports literally", strings.Join(opaque, ", "))
	}
	if err := base.Validate(); err != nil {
		return nil, err
	}
	undeclared = slices.DeleteFunc(undeclared, func(n string) bool { return displaced[n] })
	return &Inspection{Plan: base, Undeclared: undeclared, UndeclaredByEnv: byEnv}, nil
}

// changesARun says whether a variable's value changes what a run does:
// which apps are backed up and what of them, and — only where some app
// is — the root they are found under. A root that depends on the
// environment is refused only where an app declares a backup (the
// owner, 2026-09-20: decide in 4c): on a box that uses no backups a run
// backs nothing up, and the root is not its business.
func changesARun(base, trial *Plan) bool {
	if !reflect.DeepEqual(base.Apps, trial.Apps) {
		return true
	}
	return len(base.Apps) > 0 && base.Root != trial.Root
}

// A verdict is what probe says of a variable every made-up value fails
// at a directive's check of.
type verdict int

const (
	inert   verdict = iota // no occurrence reaches anything a run does
	moves                  // some occurrence moves what a run does
	unknown                // where the variable reaches could not be told here
)

// mentions says whether the adapter wrote s anywhere.
func (a adapted) mentions(s string) bool {
	return strings.Contains(a.said, s) || strings.Contains(a.out, s)
}

// probe tries each occurrence of {$NAME} on its own, in a copy of the
// Caddyfile's directory: every entry a link to the real one, but the
// files that hold the variable, which are written with that one
// occurrence made a value of its own and the others their defaults.
// The copy is adapted with the variable set, in the environment, to a
// second value of its own: an occurrence the copy does not hold — a
// file the adapter reaches by an absolute path, or one not read here
// (a copy checked away from its directory is read alone) — reads that
// value, and its appearance anywhere the adapter wrote says the copy
// was not the whole of what was adapted. That is asked first of the
// copy with every occurrence at its default, which has to adapt to the
// plan the Caddyfile did: an adapt that fails at an occurrence writes
// no config for the value to show in. Then an adapt the adapter
// refuses quoting the first value, not through an import, is the
// directive's own refusal of that occurrence, consumed where it is
// written (throughAnImport); one quoting it from a snippet or an
// imported file read the value somewhere it may be read again — a
// snippet's argument, an import with arguments expanded a second time
// — and where else it went is not knowable here; one that adapts is
// judged as a trial is; and one that fails any other way, or at an
// import — which a trial would have met first — is not knowable. An
// occurrence standing where a directive's or a subdirective's name is
// read — `{$KIND:files} app.db` in a backup block — is not tried: a
// name is checked by form as an argument is, so the value would be
// refused as an unknown one at its own line, while another value
// names another directive, and the server's is not known here.
//
// The copy is written under the temporary directory: the plan unit's
// own (PrivateTmp), or the operator's at validate. It costs one adapt
// an occurrence, for a variable most Caddyfiles never have. What it
// could not set up is an error, named; what it could not tell is
// unknown, which Inspect refuses as such.
func probe(caddyfile, name string, files []string, env map[string]string, adapt func(map[string]string, string) (adapted, error), judge func(adapted) bool) (verdict, error) {
	dir, err := filepath.Abs(filepath.Dir(caddyfile))
	if err != nil {
		return unknown, fmt.Errorf("the Caddyfile's directory, to copy for {$%s}: %w", name, err)
	}
	type occurrence struct {
		file string
		m    []int // envRe's match, as FindAllSubmatchIndex gives it
	}
	texts := map[string][]byte{}
	rels := map[string]string{} // each file's path relative to dir
	var occurrences []occurrence
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // a file envNames read a moment ago
		if err != nil {
			return unknown, fmt.Errorf("reading %s again for {$%s}: %w", f, name, err)
		}
		abs, err := filepath.Abs(f)
		if err != nil {
			return unknown, err
		}
		rel, err := filepath.Rel(dir, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return unknown, fmt.Errorf("%s holds {$%s} and is not under the Caddyfile's directory %s", f, name, dir)
		}
		texts[f], rels[f] = raw, rel
		for _, m := range envRe.FindAllSubmatchIndex(raw, -1) {
			if string(raw[m[2]:m[3]]) != name {
				continue
			}
			if leadsItsLine(raw, m[0]) {
				return unknown, nil // where a directive's name is read
			}
			occurrences = append(occurrences, occurrence{f, m})
		}
	}
	// The markers: a value of the occurrence's own, and a second for the
	// environment — neither written in any file read, nor a value the
	// base adapted with: an occurrence whose default is the marker would
	// be tried as the control was, and show nothing of where it reaches.
	appears := func(s string) bool {
		for _, raw := range texts {
			if bytes.Contains(raw, []byte(s)) {
				return true
			}
		}
		for _, v := range env {
			if strings.Contains(v, s) {
				return true
			}
		}
		return false
	}
	var own, fromEnv string
	for n := 1; ; n++ {
		own = fmt.Sprintf("hsb-probe-%s-%d.invalid", slug(name), n)
		fromEnv = fmt.Sprintf("hsb-probe-%s-%d-env.invalid", slug(name), n)
		if !appears(own) && !appears(fromEnv) {
			break
		}
	}
	tmp, err := os.MkdirTemp("", "hsb-probe-")
	if err != nil {
		return unknown, fmt.Errorf("a directory to try the Caddyfile's {$%s} in, one occurrence at a time: %w", name, err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // a temporary directory of this run's
	if tmp, err = filepath.Abs(tmp); err != nil {
		return unknown, err // the adapter spells imported files from the absolute config path
	}
	written := map[string]bool{}
	for _, rel := range rels {
		written[rel] = true
	}
	if err := mirror(dir, tmp, written); err != nil {
		return unknown, fmt.Errorf("copying the Caddyfile's directory to try {$%s} in: %w", name, err)
	}
	probeEnv := map[string]string{name: fromEnv}
	for k, v := range env {
		if k != name {
			probeEnv[k] = v
		}
	}
	// write file f into the copy, occurrence i as the value of its own
	// and the rest as their defaults; none at -1.
	write := func(f string, i int) error {
		raw := texts[f]
		var out bytes.Buffer
		last := 0
		for j, o := range occurrences {
			if o.file != f {
				continue
			}
			out.Write(raw[last:o.m[0]])
			switch {
			case j == i:
				out.WriteString(own)
			case o.m[4] >= 0:
				out.Write(raw[o.m[4]+1 : o.m[5]]) // the default, after its ":"
			default:
				out.WriteString(env[name]) // no default: the value the base adapted with
			}
			last = o.m[1]
		}
		out.Write(raw[last:])
		if err := os.WriteFile(filepath.Join(tmp, rels[f]), out.Bytes(), 0o600); err != nil {
			return fmt.Errorf("writing the copy of %s to try {$%s} in: %w", f, name, err)
		}
		return nil
	}
	for f := range texts {
		if err := write(f, -1); err != nil {
			return unknown, err
		}
	}
	copied := filepath.Join(tmp, filepath.Base(caddyfile)) // dir is the Caddyfile's own
	if control, err := adapt(probeEnv, copied); err != nil || judge(control) || control.mentions(fromEnv) {
		return unknown, nil // the copy is not what the Caddyfile is, or not the whole of it
	}
	for i, o := range occurrences {
		if i > 0 && occurrences[i-1].file != o.file {
			if err := write(occurrences[i-1].file, -1); err != nil {
				return unknown, err
			}
		}
		if err := write(o.file, i); err != nil {
			return unknown, err
		}
		trial, err := adapt(probeEnv, copied)
		switch {
		case trial.mentions(fromEnv):
			return unknown, nil // an occurrence the copy does not hold was read
		case err != nil && failedAtImport(trial.said):
			return unknown, nil // an import's path, which a trial would have met first: the file changed under the run
		case err != nil && strings.Contains(trial.said, own) && !throughAnImport(trial.said):
			continue // the directive's own refusal of this occurrence
		case err != nil:
			return unknown, nil // refused through an import, or for something else: where the value went is not told
		case judge(trial):
			return moves, nil
		}
	}
	return inert, nil
}

// chainStart is the offset in raw where the logical line holding off
// begins: its physical line's, or, for a line the lexer chains onto the
// one before, where the chain starts.
func chainStart(raw []byte, off int) int {
	start := bytes.LastIndexByte(raw[:off], '\n') + 1
	for start > 0 {
		prev := bytes.LastIndexByte(raw[:start-1], '\n') + 1
		if !chains(raw[prev : start-1]) {
			break
		}
		start = prev
	}
	return start
}

// chains says whether Caddy's lexer (caddyconfig/caddyfile/lexer.go,
// 2.11.7) reads the newline after line as escaped, chaining the next
// line onto it. A `\` sets the escape; a space (unicode.IsSpace, as the
// lexer has it) ends a token in progress and the escape with it, since
// the lexer returns the token there and the escape is the call's own,
// but keeps the escape where no token has begun; a comment keeps it to
// the line's end, the lexer reading the escape before the comment; and
// any other character uses it up. So `root \`, `root \ ` and
// `# C:\certs` chain, and `root a\b`, `root \\` and `files uploads\ `
// do not. Quoted strings and heredocs are not followed: a line in one
// chains or not as its characters say, and what the next line holds is
// the string's, a value either way.
func chains(line []byte) bool {
	escaped, comment, inToken := false, false, false
	for _, r := range string(line) {
		switch {
		case !escaped && r == '\\':
			escaped = true
		case unicode.IsSpace(r):
			if inToken {
				escaped, inToken = false, false
			}
		case comment:
		case r == '#' && !inToken:
			comment = true
		default:
			escaped, inToken = false, true
		}
	}
	return escaped
}

// leadsItsLine says whether the token at off in raw is the first of its
// logical line, or the first after a `}` on it: where Caddy's parser
// reads a directive's name, and a module its subdirective's — a `{`
// ends its line, by the parser; a token after a nested block's `}` on
// the same line is read as a name by a module's dispenser, whose
// NextBlock closes no block then [measured]; and a quoted name is a
// name, its quotes the lexer's (`"files" app.db`). A comment on
// an earlier line of the chain is no token, however it ends, so it
// counts for nothing; one on the occurrence's own line holds the
// occurrence, which then leads nothing. What else stands first on a
// line — a site address, a snippet's name, a line of a heredoc or of a
// quoted string spanning lines — is read as a name here too; a value a
// directive checks by form follows the directive's own word.
func leadsItsLine(raw []byte, off int) bool {
	blank := func(b []byte) bool {
		return len(bytes.TrimFunc(b, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("\\}\"`", r) })) == 0
	}
	own := bytes.LastIndexByte(raw[:off], '\n') + 1
	for _, line := range bytes.Split(raw[chainStart(raw, off):own], []byte("\n")) {
		if i := bytes.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if !blank(line) {
			return false
		}
	}
	return blank(raw[own:off])
}

// throughAnImport says whether the adapter's refusal is of a token an
// import brought in: Caddy ends it with the import chain (2.11.7,
// caddyfile.Dispenser.WrapErr). A snippet or a file imported with
// arguments is expanded once an import; the adapter refuses the first
// expansion it meets, and the others, which may carry the value to a
// path, are not seen — so such a refusal is not the directive's own of
// the occurrence where it is written. One not through an import is:
// a token is read where it stands and the markers are written nowhere
// else, so whatever line the adapter names — a directive that checks
// at its block's end names another — the directive that refused the
// value is the one that consumed it.
func throughAnImport(said string) bool { return strings.Contains(said, " import chain [") }

// mirror makes dst a copy of src in which every entry is a link to
// src's own, but the files named in written (paths relative to src),
// which are left for the caller to write, and the directories on the
// way to them, which are made so they can be.
func mirror(src, dst string, written map[string]bool) error {
	dirs := map[string]bool{}
	for rel := range written {
		for d := filepath.Dir(rel); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := os.ReadDir(filepath.Join(src, rel))
		if err != nil {
			return err
		}
		for _, e := range entries {
			r := filepath.Join(rel, e.Name())
			switch {
			case written[r]:
			case dirs[r]:
				if err := os.Mkdir(filepath.Join(dst, r), 0o700); err != nil {
					return err
				}
				if err := walk(r); err != nil {
					return err
				}
			default:
				if err := os.Symlink(filepath.Join(src, r), filepath.Join(dst, r)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("")
}

// Make is the plan alone: what a run needs of an Inspection.
func Make(ctx context.Context, caddyfile string) (*Plan, error) {
	i, err := Inspect(ctx, caddyfile, filepath.Dir(caddyfile))
	if err != nil {
		return nil, err
	}
	return i.Plan, nil
}

// emptyGlobError is the adapter's word that an import glob matches no
// file.
type emptyGlobError struct{ patterns []string }

func (e *emptyGlobError) Error() string {
	return fmt.Sprintf("the Caddyfile imports %s, which matches no file: a backup run reads the Caddyfile inside a view that holds its own directory and nothing else, where an import from outside it matches nothing and what is declared there would not be backed up — so every import glob has to match a file, and what is imported has to be under the Caddyfile's directory, readable by others", strings.Join(e.patterns, ", "))
}

// emptyGlobs are the import patterns the adapter says match no file, as
// the adapter logs them: after its own substitutions, and a relative
// one relative to the file it is in.
//
// It rests on three things, each of which, gone, would make the refusal
// vanish without a word — and a run plan without what its view hides:
// the wording of Caddy's warning (caddyconfig/caddyfile/parse.go); its
// being JSON, which Caddy's default logger writes only when stderr is
// not a terminal — Inspect gives the adapter a buffer, never one; and
// nothing of the operator's shell choosing the logger's format — the
// adapter runs with an empty environment. The first is held to the real
// adapter, built from this repository, by
// TestIntegrationTheAdapterWarnsOfAnEmptyGlobAsTheRefusalReadsIt, so a
// Caddy bump that changes it fails `make test-integration`.
func emptyGlobs(stderr []byte) (patterns []string) {
	for _, line := range bytes.Split(stderr, []byte("\n")) {
		var w struct {
			Msg     string `json:"msg"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(line, &w) == nil && w.Msg == "No files matching import glob pattern" {
			patterns = append(patterns, w.Pattern)
		}
	}
	return patterns
}

// isImportLine says whether a line of the adapter's is its parser's
// about an import: a file not there, one it could not read, a pattern
// it could not use. The wording is Caddy's
// (caddyconfig/caddyfile/parse.go), held to the real adapter by
// TestIntegrationTheAdapterRefusesAnImportAndADirectiveAsInspectReadsThem.
// Were it to change, a variable in an import would be refused all the
// same, as neither an import's nor a directive's words.
func isImportLine(line string) bool {
	return strings.Contains(line, "File to import not found") ||
		strings.Contains(line, "Could not import ") ||
		strings.Contains(line, "Could not read imported file") ||
		strings.Contains(line, "Failed to use import pattern")
}

// importWords is what an adapt that failed at an import said of it: the
// glob patterns matching nothing, or the parser's error line, which is
// the last. Nothing where it failed at something else, whatever
// warnings came before.
func importWords(err error, said string) (string, bool) {
	var empty *emptyGlobError
	if errors.As(err, &empty) {
		return strings.Join(empty.patterns, " "), true
	}
	if last := lastLine(said); isImportLine(last) {
		if i := strings.LastIndex(last, ", at "); i >= 0 {
			last = last[:i] // the parser's location: its line number is a digit, as a value tried may be
		}
		return last, true
	}
	return "", false
}

// failedAtImport says whether a trial adapt failed at an import whose
// path its made-up value is in: the parser's error, or a glob that
// matches nothing (the warning emptyGlobs reads; the adapter exits 0 on
// that alone, and 1 when a directive then fails too, the warning still
// there). Anywhere on stderr, where importWords reads the last line
// alone: the base adapted with no such warning, so one in a trial's is
// the trial's value's, while in the base loop a literal glob's warning
// may sit beside the directive's error that is the round's.
func failedAtImport(said string) bool {
	return len(emptyGlobs([]byte(said))) > 0 || isImportLine(lastLine(said))
}

// failedAtDirective says whether the adapter's words are a directive's
// own about its tokens: the prefix Caddy's HTTP adapter puts on every
// error a directive's or global option's setup returns
// (caddyconfig/httpcaddyfile). Held to the real adapter by the same
// test; were it to change, a variable a directive checks by form would
// be refused as before #394 — loudly, by the push it refuses.
func failedAtDirective(said string) bool {
	return strings.Contains(said, "parsing caddyfile tokens for '")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// A name runs to the first ":" (where a default starts) or "}", and has
// no white space in it: text that only looks like one — `{$` and a `}`
// lines apart, in a page's JavaScript — would cost a trial adapt each.
// It is the shape Caddy substitutes (caddyconfig/caddyfile/parse.go,
// replaceEnvVars), so what it finds is what the adapter substitutes.
var envRe = regexp.MustCompile(`\{\$([^}:\s]+)(:[^}]*)?\}`)

// envNames returns every {$NAME} in the Caddyfile and, with beside, in
// the files beside it and below — the directory a run's view holds, so
// every file the adapter can read there, imported or not — and those
// among them written somewhere with no default, and the files each is
// written in. A copy checked anywhere else is read alone. Nothing here
// reads a Caddyfile's syntax: a name from a file nothing imports costs
// one trial adapt that changes nothing, and one that is missed has no
// default given, which the adapter says loudly. A file here that may
// not be read (another app's env file) is passed over: one the
// Caddyfile imports that could not be read fails the adapt. However
// large a file is, it is read: a variable in it is as able to move the
// root.
func envNames(caddyfile string, beside bool) (all, noDefault []string, within map[string][]string, err error) {
	caddyfile = filepath.Clean(caddyfile) // as the walk below spells it, so it is read once
	names := map[string]bool{}            // true: written somewhere with no default
	within = map[string][]string{}
	read := func(file string) error {
		raw, err := os.ReadFile(file) //nolint:gosec // the Caddyfile and the files beside it, read for {$NAME} alone
		if err != nil {
			return err
		}
		for _, m := range envRe.FindAllSubmatch(raw, -1) {
			n := string(m[1])
			names[n] = names[n] || len(m[2]) == 0
			if !slices.Contains(within[n], file) {
				within[n] = append(within[n], file)
			}
		}
		return nil
	}
	if err := read(caddyfile); err != nil {
		return nil, nil, nil, fmt.Errorf("reading the Caddyfile for {$NAME}: %w", err)
	}
	if beside {
		_ = filepath.WalkDir(filepath.Dir(caddyfile), func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || p == caddyfile {
				return nil // one it may not list or read is passed over, as above; the Caddyfile is read
			}
			_ = read(p)
			return nil
		})
	}
	all, noDefault = sorted(names)
	return all, noDefault, within, nil
}

// sorted is envNames' names, and those with no default, each sorted.
func sorted(names map[string]bool) (all, noDefault []string) {
	for n, bare := range names {
		all = append(all, n)
		if bare {
			noDefault = append(noDefault, n)
		}
	}
	sort.Strings(all)
	sort.Strings(noDefault)
	return all, noDefault
}
