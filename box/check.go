package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/smallhoursorg/motel/box/proof"
)

// refusal is a verdict on the push, the whole catalogue text: the result
// is `refused`. Any other error from check is the box's own (ssh-keygen
// could not run, the chain deadline passed, the box's own files do not
// read): never a refusal, and the result is `failed`.
// ids are the object ids msg names (proof.Refusal's IDs).
type refusal struct {
	msg string
	ids []string
}

func (r *refusal) Error() string { return r.msg }

func refused(format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...)}
}

// runningRefused is a walk refusal of the file the box runs, as check
// (step 10) and `GET /` both answer it: an empty signer list refuses by
// name, with the way back; any other reason is named as the box's.
func runningRefused(ref *Refusal) error {
	if ref.Reason == reasonNoSigner {
		return refused("%s", msgNoSigner)
	}
	return fmt.Errorf("the Caddyfile this box runs %s", ref.Reason)
}

// verdict is what steps 10 to 15 established about a bundle.
type verdict struct {
	// signer is the installed principal whose key verified HEAD.
	signer string
	// host is the box_webhook host, the installed file's and the
	// incoming one's alike.
	host string
	// apps are the incoming file's app names.
	apps []string
	// prevSum and newSum are the installed and the incoming file's
	// digests; outOfBand is step 10's flag: the installed file is not
	// the one applied.json recorded.
	prevSum, newSum string
	outOfBand       bool
	// secrets are the commit's secrets beside the file, proved (13),
	// still encrypted: what step 15a opens, in the applier alone.
	secrets *proof.Secrets
	// next is the incoming file's walk (11): what the handler's
	// consistency checks read (step 6, last).
	next *Shape
}

// check is steps 10 to 15 of the trust chain (DESIGN-box.md), from
// bytes: the bundle as read once, the installed file as read once, the
// baseline. It is what the applier runs as root and what the handler's
// pre-verification runs as motel (step 5), so the two cannot
// disagree about a verdict or its words.
//
// The checks run cheapest first — the two readings of the files and the
// identity (10, 11, 15's presence rules), the file proof (13), the
// chain's linkage (14) — so that a malformed bundle never starts
// ssh-keygen; then every signature on the chain, HEAD's first (12 and
// 14); then the guard that the verifying key stays listed (15). When
// more than one step would refuse, the first so found is the one named.
func check(ctx context.Context, v *proof.Verifier, b *proof.Bundle, installed []byte, base *applied) (*verdict, error) {
	// 10. The installed file: the signer list and the host, from its
	// raw tokens. An empty list refuses by name; a file the walk refuses
	// for any other reason is the box's trouble, not the push's.
	cur, err := Walk(installed)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			return nil, runningRefused(ref)
		}
		return nil, err
	}
	vd := &verdict{prevSum: digest(installed), newSum: digest(b.Caddyfile)}
	vd.outOfBand = vd.prevSum != base.SHA256

	// 11 and 15's presence rules: the incoming file reads as a box's
	// (box block, signers, deploy_trust, one bare-hostname box_webhook
	// site), names this box's host, and is this box's path.
	next, err := Walk(b.Caddyfile)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			return nil, refused("the new Caddyfile %s", ref.Reason)
		}
		return nil, err
	}
	if next.Host != cur.Host {
		return nil, refused("this file is for %s; this box is %s", next.Host, cur.Host)
	}
	if b.Path != base.Path {
		// The recorded path is the layout's already: the bundle's file
		// is the one in the wrong place. Otherwise init records the
		// layout's path, once the file is there.
		fix := "move it there"
		if base.Path != cur.Host+"/Caddyfile" {
			fix = "moteld init, run again from that directory, records it"
		}
		return nil, refused("this box's file is %s; the bundle is %s — a box's file is %s/Caddyfile at the top of the repository: %s", proof.Bound(base.Path), proof.Bound(b.Path), cur.Host, fix)
	}
	vd.host, vd.apps, vd.next = cur.Host, next.Apps, next

	// 13. The file is the committed file, and the secrets beside it the
	// committed secrets ("Proving the secrets").
	if err := proof.ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
		return nil, asRefusal(err)
	}
	if vd.secrets, err = provedSecrets(b); err != nil {
		return nil, err
	}
	// 14, the linkage: HEAD down to the baseline, by hash, within the cap.
	chain, err := proof.Chain(b.Commit, b.Parents, base.SHA)
	if err != nil {
		return nil, asRefusal(err)
	}
	// 12 and 14, the signatures: every commit on the chain, against the
	// installed list.
	signer, err := proof.VerifyChain(ctx, v, chain, cur.Signers, next.Signers, base.SHA)
	if err != nil {
		return nil, asRefusal(err)
	}
	vd.signer = signer

	// 15. Never cut the branch you sit on: the key that verified HEAD is
	// still listed in the incoming file, by key, whatever it is named.
	key, _, err := proof.SignatureKey(b.Commit.Signature)
	if err != nil {
		return nil, err // VerifyChain read this same signature: unreachable
	}
	if _, ok := next.Signers.PrincipalFor(key); !ok {
		return nil, refused("the new Caddyfile drops the key that signed this commit (%s); add the new key in one push, let it apply, then remove the old one", signer)
	}
	return vd, nil
}

// provedSecrets is step 13's proof of the secrets beside the bundle's
// file, as a refusal if it fails: check and the fast path both ask
// here, so the two cannot disagree about a commit with secrets.
func provedSecrets(b *proof.Bundle) (*proof.Secrets, error) {
	s, err := proof.ProveSecrets(b.Commit, b.Trees, b.Blobs, b.Path, secretNames)
	if err != nil {
		return nil, asRefusal(err)
	}
	return s, nil
}

// unchanged is the fast path's question (step 4): is the bundle the
// commit and files the box runs, in a shape root would accept? HEAD is
// the baseline, its file the installed bytes, its secrets tree the one
// applied.json records, and every env file applied.json lists reads back
// at its recorded digest (env, which reads the installed file; one that
// cannot be read counts as changed, never as an error) — and the steps of
// check that need no signature hold: 11's path, 13's file and secrets
// proofs, 14's linkage (HEAD alone, parents/ empty). No signature: HEAD's
// id is the baseline's, so its bytes are the commit the box already
// runs. A bundle it does not answer goes on to check, which names the
// step that refuses: a bundle in a shape root refuses is never answered
// no_change. After a console edit or the loss of an env file the
// digests differ and root reinstalls HEAD's files.
func unchanged(b *proof.Bundle, installed []byte, base *applied, env func(app string) (string, error)) bool {
	if b.Commit.ID != base.SHA || b.Path != base.Path || digest(b.Caddyfile) != digest(installed) {
		return false
	}
	if proof.ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile) != nil {
		return false
	}
	s, err := provedSecrets(b)
	if err != nil || s.Tree != base.SecretsTree || len(s.Apps) != len(base.Secrets) {
		return false
	}
	// The tree's apps are the ones applied.json lists, each at its
	// digest on disk: anything else goes to root, which decides.
	for _, a := range s.Apps {
		if _, listed := base.Secrets[a.Name]; !listed {
			return false
		}
	}
	for app, want := range base.Secrets {
		if d, err := env(app); err != nil || d != want {
			return false
		}
	}
	_, err = proof.Chain(b.Commit, b.Parents, base.SHA)
	return err == nil
}

// asRefusal turns the proof's verdicts into refusals and passes every
// other error — the box's — through as it is.
func asRefusal(err error) error {
	var r *proof.Refusal
	if errors.As(err, &r) {
		return &refusal{msg: r.Msg, ids: r.IDs}
	}
	return err
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
