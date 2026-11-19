package model

import (
	"regexp"
	"strings"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// AboutAnother is the whoami command's rule, and the TUI's: an answer
// naming another key than the one that signed (by fingerprint), or
// another box than the one asked, is not about this key here, and
// nothing in it is shown as who you are.
func AboutAnother(me wire.WhoAmI, fingerprint, host string) (keyDiffers, boxDiffers bool) {
	return me.Key != fingerprint, me.Box != host
}

// aboutAnother is whoami's answer held to the whoami command's rule: a
// failure that stops every read when it is about another key or box,
// in the client's words with the box's values quoted. A session with no
// key (a test's) holds nothing.
func (s *Session) aboutAnother(me wire.WhoAmI) error {
	if s.Key.Fingerprint == "" {
		return nil
	}
	keyDiffers, boxDiffers := AboutAnother(me, s.Key.Fingerprint, s.Host)
	var says []string
	if keyDiffers {
		says = append(says, "the box answered about another key than the one this TUI signed with ("+s.Key.Fingerprint+"); it names "+termtext.Quote(me.Key).String()+".")
	}
	if boxDiffers {
		says = append(says, "the box answered as another box than the one asked ("+s.Host+"); it names "+termtext.Quote(me.Box).String()+".")
	}
	if len(says) == 0 {
		return nil
	}
	return &Failure{Kind: FailAboutAnother, Message: strings.Join(says, " ") + " An answer that is not about this key here is not one to act on, so nothing more is asked of " + s.Host + ", and nothing it said is shown as yours."}
}

// versionRe is the client's version grammar (client's CheckVersion, the
// box's liveswap/names.go versionRe, with its 64-byte bound):
// TestVersionGrammarIsTheClients holds the two the same.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

// ValidVersion says whether v can be a version, before anything is
// asked.
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// shellSafe are the characters an argument may hold and be printed
// bare; anything else is single-quoted.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// ShellQuote is s as one argument to a POSIX shell: bare when it holds
// only safe characters, else in single quotes, each ' in it written as
// the quotation closed, a backslash and the ', and the quotation opened
// again.
//
// A leading = or ~ is quoted too: zsh expands =cmd, and every shell ~.
func ShellQuote(s string) string {
	if shellSafe.MatchString(s) && !strings.HasPrefix(s, "=") && !strings.HasPrefix(s, "~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellLine is a command line from its arguments, each quoted.
func ShellLine(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = ShellQuote(a)
	}
	return strings.Join(q, " ")
}
