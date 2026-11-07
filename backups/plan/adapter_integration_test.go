//go:build integration

package plan

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The plan step's refusal of an import glob that matches nothing rests
// on the words of Caddy's own warning, in the form Caddy logs it when
// its stderr is not a terminal (emptyGlobs). Were a Caddy bump to word
// or shape that line otherwise, the refusal would be gone, and with it
// the one thing that stops a run planning without the apps a view
// cannot see — silently. So it is held to the real adapter, built from
// this repository, here and not only in e2e.
func TestIntegrationTheAdapterWarnsOfAnEmptyGlobAsTheRefusalReadsIt(t *testing.T) {
	realAdapter(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	site := "http://:8080 {\n\trespond hi\n}\n"
	outside := filepath.Join(t.TempDir(), "apps", "*.caddy")
	if err := os.WriteFile(file, []byte("import "+outside+"\n"+site), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Make(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), outside+", which matches no file") {
		t.Fatalf("Make of a Caddyfile whose import glob matches nothing: %v", err)
	}

	// And one that matches is taken: the refusal is of the warning, not
	// of every import.
	if err := os.MkdirAll(filepath.Join(dir, "sites"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sites", "a.caddy"), []byte("# nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("import sites/*.caddy\n"+site), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("Make of a glob that matches: %v", err)
	}
}

// A variable no made-up value adapts with is refused where a trial
// failed at an import and let through where every trial was refused by
// a directive checking its argument's form (#394). Which is which is
// read off the adapter's words, as emptyGlobs reads its warning: the
// parser's "File to import not found" and "Could not import" with no
// prefix, a directive's own behind the HTTP adapter's "parsing
// caddyfile tokens for '…':". Were a Caddy bump to change the first,
// an import through a variable would still be refused, in the words
// for neither; were it to change the second, `protocols
// {$TLS_MIN:tls1.2}` would be refused again, as before #394 — so both
// are held here, on the adapter's own stderr, where Make's verdict
// alone could not tell.
func TestIntegrationTheAdapterRefusesAnImportAndADirectiveAsInspectReadsThem(t *testing.T) {
	realAdapter(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	if err := os.MkdirAll(filepath.Join(dir, "sites", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join("sites", "prod.caddy"), filepath.Join("sites", "prod", "a.caddy")} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("# what the default imports\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	said := func(caddyfile string, env ...string) string {
		t.Helper()
		if err := os.WriteFile(file, []byte(caddyfile), 0o644); err != nil {
			t.Fatal(err)
		}
		adapt := exec.Command(motel, "adapt", "--adapter", "caddyfile", "--config", file)
		adapt.Env = append([]string{}, env...) // never nil: nil means "inherit"
		var stderr strings.Builder
		adapt.Stderr = &stderr
		_ = adapt.Run()
		return stderr.String()
	}
	const site = "example.com {\n\ttls {\n\t\tprotocols {$TLS_MIN:tls1.2}\n\t}\n\trespond ok\n}\n"
	for name, tc := range map[string]struct {
		said                  string
		atImport, atDirective bool
	}{
		"a directive's check":     {said(site, "TLS_MIN=1"), false, true},
		"a file to import":        {said("import sites/{$ENV:prod}.caddy\n"+site, "ENV=1"), true, false},
		"a directory to import":   {said("import {$F:sites/prod.caddy}\n"+site, "F=sites"), true, false},
		"a glob, and a directive": {said("import sites/{$ENV:prod}/*.caddy\nexample.com {\n\ttls {\n\t\tprotocols {$ENV:tls1.2}\n\t}\n}\n", "ENV=1"), true, true},
	} {
		if failedAtImport(tc.said) != tc.atImport || failedAtDirective(tc.said) != tc.atDirective {
			t.Errorf("%s: import %v, directive %v, want %v, %v; the adapter said:\n%s", name, failedAtImport(tc.said), failedAtDirective(tc.said), tc.atImport, tc.atDirective, tc.said)
		}
	}

	// And so Make: the one let through, the other refused by name.
	if err := os.WriteFile(file, []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("Make of a value the tls directive checks by form: %v", err)
	}
	if err := os.WriteFile(file, []byte("import sites/{$ENV:prod}.caddy\n"+site), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "imports through the environment variable(s) ENV") {
		t.Fatalf("Make of an import through a variable: %v", err)
	}

	// A snippet's argument reaches its {args[0]}, here the root: no
	// trial adapts (the tls check refuses each), and each occurrence
	// tried on its own finds the root moved — however the lexer chains
	// the occurrence onto its line.
	snippet := "(where) {\n\troot /srv/{args[0]}\n}\n{\n\tadmin off\n\tliveswap {\n\t\timport where {$X:tls1.2}\n\t\tartifact_allowlist example.com/\n\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository your-org/app\n\t\t}\n\t\tapp blog {\n\t\t\tcommand ./server\n\t\t\tbackup {\n\t\t\t\tsqlite app.db\n\t\t\t}\n\t\t}\n\t}\n}\n" + strings.Replace(site, "TLS_MIN", "X", 1)
	if out := said(snippet); strings.Contains(out, "\"level\":\"error\"") {
		t.Fatalf("fixture: the snippet Caddyfile does not adapt with its defaults: %s", out)
	}
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "depends on the environment variable(s) X") {
		t.Fatalf("Make of a root reached through a snippet's argument: %v", err)
	}
	for name, form := range map[string]string{
		"chained by a backslash": "import where \\\n\t\t\t{$X:tls1.2}",
		"quoted":                 "import where \"{$X:tls1.2}\"",
	} {
		if out := said(strings.Replace(snippet, "import where {$X:tls1.2}", form, 1)); strings.Contains(out, "\"level\":\"error\"") {
			t.Fatalf("fixture: the Caddyfile %s does not adapt with its defaults: %s", name, out)
		}
		if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "depends on the environment variable(s) X") {
			t.Fatalf("Make of a root reached through a snippet argument %s: %v", name, err)
		}
	}

	// A snippet's argument used twice — for the root and for the
	// protocols check, one import — is refused by the check quoting the
	// value from the snippet's line, not the import's (the adapter names
	// the import's line after it, in the import chain): where else it
	// went is not told.
	twice := "(site) {\n\t{\n\t\tadmin off\n\t\tliveswap {\n\t\t\troot /srv/{args[0]}\n\t\t\tartifact_allowlist example.com/\n\t\t\tdeploy_trust github {\n\t\t\t\taudience motel\n\t\t\t\tclaim repository your-org/app\n\t\t\t}\n\t\t\tapp blog {\n\t\t\t\tcommand ./server\n\t\t\t\tbackup {\n\t\t\t\t\tsqlite app.db\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t}\n\texample.com {\n\t\ttls {\n\t\t\tprotocols {args[0]}\n\t\t}\n\t}\n}\nimport site {$X:tls1.2}\n"
	if out := said(twice); strings.Contains(out, "\"level\":\"error\"") {
		t.Fatalf("fixture: the Caddyfile with the argument used twice does not adapt with its defaults: %s", out)
	}
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "could not be told") {
		t.Fatalf("Make with a snippet argument used twice: %v", err)
	}

	// An occurrence the copy does not hold — in a file imported by its
	// absolute path, which the adapter reads where it is — is seen, and
	// the variable refused as not knowable here.
	if err := os.WriteFile(filepath.Join(dir, "sites", "hdr.caddy"), []byte("header_up X-Env {$X:tls1.2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("example.com {\n\ttls {\n\t\tprotocols {$X:tls1.2}\n\t}\n\treverse_proxy a:1 {\n\t\timport "+filepath.Join(dir, "sites", "hdr.caddy")+"\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "could not be told") {
		t.Fatalf("Make with an occurrence past the copy: %v", err)
	}
	// A chained occurrence, and an inert one after it: each the
	// directive's own refusal, wherever the adapter places it.
	if err := os.WriteFile(file, []byte("example.com {\n\ttls {\n\t\tprotocols \\\n\t\t\t{$X:tls1.2}\n\t}\n\treverse_proxy a:1 {\n\t\theader_up X-Env {$X:tls1.2}\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("Make with a chained occurrence and one after it: %v", err)
	}
	// Imported by a relative path, the same file is in the copy: let through.
	if err := os.WriteFile(file, []byte("example.com {\n\ttls {\n\t\tprotocols {$X:tls1.2}\n\t}\n\treverse_proxy a:1 {\n\t\timport sites/hdr.caddy\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("Make with the occurrence in the copy: %v", err)
	}
	// But a value the directive refuses in an imported file is refused
	// through the import — the adapter's words end in the import chain
	// — and the variable as not knowable: the import may be expanded
	// again (an accepted residual, the README's table).
	if err := os.WriteFile(filepath.Join(dir, "sites", "tls.caddy"), []byte("tls {\n\tprotocols {$X:tls1.2}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("example.com {\n\timport sites/tls.caddy\n\trespond ok\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "could not be told") {
		t.Fatalf("Make with the directive's check in an imported file: %v", err)
	}

	const liveswap = "{\n\tadmin off\n\tliveswap {\n\t\troot /srv\n\t\tartifact_allowlist example.com/\n\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository your-org/app\n\t\t}\n\t\tapp blog {\n\t\t\tcommand ./server\n\t\t\tbackup {\n\t\t\t\t%s\n\t\t\t}\n\t\t}\n\t}\n}\n"
	adapts := func(name, caddyfile string, env ...string) {
		t.Helper()
		if out := said(caddyfile, env...); strings.Contains(out, "\"level\":\"error\"") {
			t.Fatalf("fixture: the Caddyfile %s does not adapt with %v: %s", name, env, out)
		}
	}

	// One textual occurrence a snippet carries to two places — a backup
	// path and a policy the adapter checks by form: the adapter refuses
	// the first expansion it meets, quoting the value from the snippet's
	// line with the import chain after it, and the other, where a plan
	// reads, is never seen. Refused as not knowable.
	shared := "(value) {\n\t{args[0]} {$X:first}\n}\n" + fmt.Sprintf(liveswap, "import value sqlite") + "example.com {\n\treverse_proxy a:1 {\n\t\timport value lb_policy\n\t}\n}\n"
	adapts("with a shared snippet occurrence", shared)
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "could not be told") {
		t.Fatalf("Make with one occurrence a snippet carries to a backup path and a policy: %v", err)
	}

	// A variable standing where a subdirective's name is read: every
	// value but a kind's is refused as an unknown subdirective, quoted at
	// its own line, as a check of form refuses an argument — and with
	// KIND=sqlite the server declares a database where the plan has a
	// file. Not tried, refused as not knowable; so when quoted, the
	// quotes being the lexer's; indented with a space of Unicode's; after
	// a comment ending in a backslash, which chains the line to the
	// comment's number and to no directive; and after a token, a
	// backslash and a space, where the lexer returns the token and the
	// escape goes with it, so nothing chains.
	for name, line := range map[string]string{
		"first on its line":                        "{$KIND:files} app.db",
		"quoted":                                   "\"{$KIND:files}\" app.db",
		"indented with a no-break space":           "\u00a0{$KIND:files} app.db",
		"after a backslash, a space and a newline": "files uploads\\ \n\t\t\t\t{$KIND:files} app.db",
		"after a comment ending in a backslash":    "# the kind \\\n\t\t\t\t{$KIND:files} app.db",
	} {
		kind := fmt.Sprintf(liveswap, line)
		adapts("with a kind "+name, kind)
		adapts("with a kind "+name, kind, "KIND=sqlite")
		if out := said(kind, "KIND=1"); !failedAtDirective(out) {
			t.Fatalf("fixture: a kind %s is not refused in a directive's words, so the probe is not reached: %s", name, out)
		}
		if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "could not be told") {
			t.Fatalf("Make with a kind %s: %v", name, err)
		}
	}
	// A line ending in two backslashes chains nothing — the first escapes
	// the second — so the token first on the next line is a
	// subdirective's name, where a chain would have made it an argument.
	escaped := "example.com {\n\treverse_proxy a:1 {\n\t\theader_up X-Env \\\\\n\t\t{$D:header_up} X-B v\n\t}\n}\n"
	adapts("with an escaped backslash", escaped)
	if out := said(escaped, "D=1"); !failedAtDirective(out) {
		t.Fatalf("fixture: the subdirective after the escaped backslash is not refused in a directive's words: %s", out)
	}
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "could not be told") {
		t.Fatalf("Make with a subdirective's name after an escaped backslash: %v", err)
	}

	// A backup path whose default is the value the probe would try
	// there, had it one value: the probe takes one the Caddyfile does not
	// hold, and the path is found moved.
	for _, def := range []string{"hsb-probe-x-1.invalid", "hsb-probe-x.invalid"} {
		marker := fmt.Sprintf(liveswap, "sqlite {$X:"+def+"}") + strings.Replace(site, "TLS_MIN", "X", 1)
		adapts("with the probe's marker "+def+" as a default", marker)
		if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "depends on the environment variable(s) X") {
			t.Fatalf("Make with the probe's marker %s as a backup path's default: %v", def, err)
		}
	}
}

// The adapter, motel built from this repository: once, by the first
// test that asks, and removed after the last (TestMain).
var (
	adapterOnce sync.Once
	adapterDir  string
	adapterErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if adapterDir != "" {
		_ = os.RemoveAll(adapterDir)
	}
	os.Exit(code)
}

// realAdapter has Make adapt with the real adapter for the rest of the
// test.
func realAdapter(t *testing.T) {
	t.Helper()
	adapterOnce.Do(func() {
		root, err := filepath.Abs("../..") // the workspace: the motel module is its root
		if err != nil {
			adapterErr = err
			return
		}
		if adapterDir, adapterErr = os.MkdirTemp("", "plan-adapter-"); adapterErr != nil {
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(adapterDir, "motel"), "./cmd/motel")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			adapterErr = fmt.Errorf("building motel: %w\n%s", err, out)
		}
	})
	if adapterErr != nil {
		t.Fatal(adapterErr)
	}
	old := motel
	motel = filepath.Join(adapterDir, "motel")
	t.Cleanup(func() { motel = old })
}

// A run adapts the Caddyfile without the server's environment, and what
// it reads of {$NAME} rests on how Caddy substitutes it: in the text,
// before any module sees a token; unset, with no default, as nothing —
// which makes `email {$ACME_EMAIL}` no Caddyfile at all; and
// {$NAME:default} as the default. So Make gives a variable with no
// default a made-up value of the kind its place takes, and refuses one
// whose value moves the root or a backup path, naming it (Inspect).
// Held to the real adapter, as the empty-glob warning is.
func TestIntegrationTheAdapterSubstitutesVariablesAsMakeReadsThem(t *testing.T) {
	realAdapter(t)
	const apps = `
	liveswap {
		root %s
		artifact_allowlist example.com/
		deploy_trust github {
			audience motel
			claim repository your-org/app
		}
		app blog {
			command ./server
			backup {
				sqlite app.db
				files  uploads
			}
		}
	}
`
	write := func(global, root, sites string) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "Caddyfile")
		if err := os.WriteFile(file, []byte("{\n\tadmin off\n"+global+strings.Replace(apps, "%s", root, 1)+"}\n\n"+sites), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}

	// An ordinary production Caddyfile: variables with no default.
	file := write("\temail {$ACME_EMAIL}\n", "/var/lib/liveswap", ":{$METRICS_PORT} {\n\trespond metrics\n}\n\n{$SITE_DOMAIN} {\n\trespond ok\n}\n")
	adapt := exec.Command(motel, "adapt", "--adapter", "caddyfile", "--config", file)
	adapt.Env = []string{}
	if out, err := adapt.CombinedOutput(); err == nil {
		t.Fatalf("fixture: with an empty environment this Caddyfile adapts, so the test proves nothing: %s", out)
	}
	p, err := Make(context.Background(), file)
	if err != nil || p.Root != "/var/lib/liveswap" || p.Apps["blog"] == nil {
		t.Fatalf("Make of a Caddyfile whose variables have no default: %+v, %v", p, err)
	}

	// A root the server and a run would read differently.
	file = write("", "{$LIVESWAP_ROOT:/var/lib/liveswap}", ":8080 {\n\trespond ok\n}\n")
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "depends on the environment variable(s) LIVESWAP_ROOT") {
		t.Fatalf("Make of a root written {$LIVESWAP_ROOT:…}: %v", err)
	}
}
