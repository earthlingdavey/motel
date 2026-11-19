// Package proof is the box channel's proof core: it decides whether a
// bundle — a Caddyfile, the commit object that is claimed to contain
// it, the trees on the path between them, and the first-parent chain
// down to the commit the box runs — is what it claims to be. Every
// check is from bytes: ids are computed, never read; a signature is
// ssh-keygen's verdict on the commit object with its gpgsig header
// removed, against the signer list of the Caddyfile the box runs.
//
// The package is stdlib plus golang.org/x/crypto/ssh (key parsing) and
// liveswap/access's Ed25519 point check (UnusableEd25519, which a
// person line uses too; it brings filippo.io/edwards25519), and imports
// nothing of Caddy's, so the applier's root-side checks and the
// handler's pre-check (DESIGN-box.md, steps 5 and 11–14) run the same
// code with the same messages.
package proof

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Refusal is a verdict the box can show whole: one line, in the words
// DESIGN-box.md "Refusals" lists, naming the step that failed. Any
// text a bundle chose that a message quotes — the path — passes Bound
// first; everything else a message carries (ids, principals) the proof
// derived itself. An error that is not a Refusal is the box's own
// trouble (ssh-keygen could not be run), not a verdict on the push.
//
// IDs are the object ids Msg names — HEAD, the baseline, a commit on
// the chain, a tree — so that an answer can hold them out of liveswap's
// response filter, whose entropy layer masks any 40-hex run: a refusal
// that masked the commit it is about would name nothing (DESIGN-box.md,
// "Handler contract").
type Refusal struct {
	Msg  string
	IDs  []string
	code refusalCode
}

func (r *Refusal) Error() string { return r.Msg }

// refusalCode tells apart the signature verdicts a chain must treat
// differently (VerifyChain); every other refusal is codeNone.
type refusalCode int

const (
	codeNone     refusalCode = iota
	codeUnsigned             // no gpgsig, or one that is not an SSH signature
	codeUnlisted             // an SSH signature by a key the list does not carry
	codeAltered              // a listed key's signature that does not verify over the payload
)

func refuse(format string, args ...any) error {
	return refuseCode(codeNone, format, args...)
}

func refuseCode(code refusalCode, format string, args ...any) error {
	return &Refusal{Msg: fmt.Sprintf(format, args...), IDs: idsIn(args), code: code}
}

// oid marks a refusal's argument as an object id — one the proof
// computed, or read from an object it parsed — and so one the refusal
// names in IDs. Marked by type, not recognised by shape: a principal or
// a quoted name may look like an id and is not one (it stays filtered).
type oid string

// inCommit is " in <sha>" in a refusal of the layout of secrets, the
// sha an id the refusal names as an oid is; empty, for a working tree
// (`moteld box describe`), it is "".
type inCommit oid

func (c inCommit) String() string {
	if c == "" {
		return ""
	}
	return " in " + string(c)
}

// idsIn is every oid argument in the id grammar, once each.
func idsIn(args []any) []string {
	var ids []string
	for _, a := range args {
		s, ok := a.(oid)
		if c, isIn := a.(inCommit); isIn {
			s, ok = oid(c), true
		}
		if ok && IsID(string(s)) && !slices.Contains(ids, string(s)) {
			ids = append(ids, string(s))
		}
	}
	return ids
}

// maxBound is liveswap's maxRefusalLen: the most bytes of caller-chosen
// text one refusal line carries.
const maxBound = 300

// Bound is liveswap's boundRefusal (deploytrust.go), kept the same so
// that a path or an address in a box refusal is bounded exactly as a
// token's claims are in a deploy's: Go-quoted to ASCII first if the
// text is not printable UTF-8 (so a newline or a control byte cannot
// split or forge a journal line), and only then cut, at a rune
// boundary, so the cut can neither leave a partial rune nor grow the
// string it bounds. A cut-then-quote is not a bound.
func Bound(s string) string {
	if s = printable(s); len(s) > maxBound {
		s = cutRunes(s, maxBound) + "..."
	}
	return s
}

// BoundTail is Bound keeping the end of s, not its start: for an error
// that wraps its cause last, as Caddy's module chain does (`loading
// http app module: provision http: server srv1: ...: provision
// liveswap: app <name>: <why>`), whose path alone runs past the bound
// (#324).
func BoundTail(s string) string {
	if s = printable(s); len(s) > maxBound {
		s = "..." + tailRunes(s, maxBound)
	}
	return s
}

// printable is s Go-quoted to ASCII if it is not printable UTF-8.
func printable(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return !strconv.IsPrint(r) && r != ' ' }) {
		return strconv.QuoteToASCII(s)
	}
	return s
}

// cutRunes is s cut to at most n bytes at a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailRunes is the end of s, at most n bytes, from a rune boundary.
func tailRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
