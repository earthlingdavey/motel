package proof

import (
	"context"
	"errors"
)

// MaxChain bounds the first-parent chain a bundle carries
// (DESIGN-box.md, "Store rules").
const MaxChain = 500

// Chain is step 14's walk: first parents from head until baseline,
// through the bundled parent objects in the order the workflow listed
// them (`parents/0001` first), linked by hash, not by position: each
// entry's computed id must equal the previous commit's first `parent`
// header, and the walk ends when a first `parent` header equals the
// baseline — whose object is not bundled. An entry past that point, or
// one that is not the first parent of the commit before it, is a
// malformed bundle. It returns the chain, head first — every commit on
// it must then carry a signature the box accepts (VerifyChain). HEAD
// is always on the chain, so it is never empty: when HEAD is the
// baseline the chain is HEAD alone, its signature is still checked
// against the installed list, and no parent may be bundled. A walk
// that runs out of parents, or of bundled objects, before it reaches
// the baseline is a history that no longer contains the commit the box
// runs on its first-parent line, refused with the message that names
// the three ways that happens. A chain of more than MaxChain commits,
// HEAD counted, is refused with the message that names the reset; the
// workflow checks the same bound before it posts.
func Chain(head *Commit, parents []*Commit, baseline string) ([]*Commit, error) {
	chain := []*Commit{head}
	cur := head
	for i, p := range parents {
		if cur.ID == baseline || (len(cur.Parents) > 0 && cur.Parents[0] == baseline) {
			return nil, refuse("bundle: parents/%04d is past the end of the chain", i+1)
		}
		if len(cur.Parents) == 0 { // a root, and no baseline met: not past the end, but off the history
			return nil, descendRefusal(head.ID, baseline)
		}
		if p.ID != cur.Parents[0] {
			return nil, refuse("bundle: parents/%04d is not the first parent of %s", i+1, oid(cur.ID))
		}
		if len(chain) == MaxChain {
			return nil, refuse("the chain from %s to %s is longer than %d commits; run moteld box baseline %s as root on the box", oid(baseline), oid(head.ID), MaxChain, oid(head.ID))
		}
		chain = append(chain, p)
		cur = p
	}
	if cur.ID == baseline || (len(cur.Parents) > 0 && cur.Parents[0] == baseline) {
		return chain, nil
	}
	// A bundle holds at most MaxChain-1 parents, so a history longer
	// than the cap fills the chain exactly and ends here, still naming
	// a parent: that is the cap, not a missing baseline.
	if len(chain) == MaxChain && len(cur.Parents) > 0 {
		return nil, refuse("the chain from %s to %s is longer than %d commits; run moteld box baseline %s as root on the box", oid(baseline), oid(head.ID), MaxChain, oid(head.ID))
	}
	return nil, descendRefusal(head.ID, baseline)
}

func descendRefusal(head, baseline string) error {
	return refuse("%s does not descend from the commit this box runs (%s) along main's first-parent line. "+
		"If the box already runs a later commit, nothing is wrong. "+
		"If a branch merged main into itself before it was fast-forwarded, rebase it onto main and force-push. "+
		"Only if main was rewritten below %s does moteld box baseline %s, as root on the box, reset trust to %s, whose ancestors the box will then never examine",
		oid(head), oid(baseline), oid(baseline), oid(head), oid(head))
}

// VerifyChain is steps 12 and 14 together: every commit of the chain
// (head first, as Chain returns it) must verify against the installed
// signers, within one deadline for the whole chain over and above each
// child's. It returns the principal whose key signed head. Head's own
// refusals are the named ones (not signed, OpenPGP, not a signer, a
// signature that does not verify). A commit between the baseline and
// head is refused by what its verdict was: unsigned, or signed by
// anything but an SSH key, gets the message that says to rebase it
// away and force-push, no baseline change — an unsigned commit a
// leaked credential pushed would otherwise ride into the box under the
// next signed one; signed by a key the box did not list when it last
// applied gets the message that names the principal the incoming file
// gives that key, because the commit that adds the key must apply
// before the commits it signs, or says the new file does not list it
// either; a listed key whose signature does not verify keeps its own
// message. An error that is not a verdict — ssh-keygen could not run,
// the deadline passed — is returned as it is, never as a refusal.
func VerifyChain(ctx context.Context, v *Verifier, chain []*Commit, installed, incoming Signers, baseline string) (string, error) {
	// Chain never returns an empty chain; a caller that hands one over
	// would be skipping HEAD's signature, so this fails closed.
	if len(chain) == 0 {
		return "", errors.New("verify: an empty chain; HEAD is always on the chain")
	}
	budget := v.ChainTimeout
	if budget == 0 {
		budget = defaultChainTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// The installed allowed_signers file is rendered once for the chain;
	// the incoming one is needed at most once, on the way out.
	allowedInstalled, err := installed.AllowedSigners()
	if err != nil {
		return "", err
	}
	principal := ""
	for i, c := range chain {
		p, err := v.verify(ctx, c, installed, allowedInstalled)
		if err == nil {
			if i == 0 {
				principal = p
			}
			continue
		}
		var r *Refusal
		if i == 0 || !errors.As(err, &r) {
			return "", err
		}
		switch r.code {
		case codeUnlisted:
			// Only a name is wanted, and the incoming list is not the
			// authority, so no verifier runs against it: the key the
			// signature carries is looked up, nothing more.
			name := "not in the new Caddyfile either"
			if key, _, err := SignatureKey(c.Signature); err == nil {
				if p, ok := incoming.PrincipalFor(key); ok {
					name = p
				}
			}
			return "", refuse("%s, between the commit this box runs and %s, is signed by a key this box did not list when it last applied (%s); the commit that adds the key must apply first — force main back to it, let it apply, then push the rest",
				oid(c.ID), oid(chain[0].ID), name)
		case codeAltered:
			return "", err
		default:
			return "", refuse("%s, between the commit this box runs and %s, is not signed; every commit on main must be — rebase it out and force-push; the box still runs %s",
				oid(c.ID), oid(chain[0].ID), oid(baseline))
		}
	}
	return principal, nil
}
