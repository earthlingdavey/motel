package plan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

// fromAdapted is what the plan step makes of `motel adapt`'s output:
// the plan, validated.
func fromAdapted(raw []byte) (*Plan, error) {
	p, _, err := extractAll(raw)
	if err != nil {
		return nil, err
	}
	return p, p.Validate()
}

func TestFromAdapted(t *testing.T) {
	p, err := fromAdapted([]byte(`{"apps":{"http":{},"liveswap":{"apps":{
		"blog":{"command":["./server"],"backup":{"sqlite":["app.db"],"files":["uploads"]}},
		"api":{"command":["./api"]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := &Plan{Root: "/var/lib/liveswap", Apps: map[string]*backupdecl.Config{"blog": {SQLite: []string{"app.db"}, Files: []string{"uploads"}}}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("plan = %+v, want the default root and only the app that declares a backup", p)
	}
	if p, err := fromAdapted([]byte(`{"apps":{"liveswap":{"root":"/srv/live swap"}}}`)); err != nil || p.Root != "/srv/live swap" {
		t.Fatalf("an explicit root: %+v, %v", p, err)
	}
	if p, err := fromAdapted([]byte(`{"apps":{"http":{}}}`)); err != nil || len(p.Apps) != 0 {
		t.Fatalf("a config with no liveswap app is an empty plan, got %+v, %v", p, err)
	}
}

// Nothing is trusted because `motel validate` would have refused
// it: the file may never have been validated.
func TestFromAdaptedRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"not JSON":                 {`Error: adapting config`, "reading the adapted config"},
		"a placeholder root":       {`{"apps":{"liveswap":{"root":"{env.ROOT}","apps":{"blog":{"backup":{"files":["."]}}}}}}`, "placeholder"},
		"a relative root":          {`{"apps":{"liveswap":{"root":"srv/liveswap"}}}`, "not an absolute path"},
		"an unclean root":          {`{"apps":{"liveswap":{"root":"/srv/../etc"}}}`, "not a clean path"},
		"a bad app name":           {`{"apps":{"liveswap":{"apps":{"../etc":{"backup":{"files":["."]}}}}}}`, "is not one liveswap accepts"},
		"a path outside shared":    {`{"apps":{"liveswap":{"apps":{"blog":{"backup":{"files":["../shop/shared"]}}}}}}`, "outside the app's shared dir"},
		"a declaration of nothing": {`{"apps":{"liveswap":{"apps":{"blog":{"backup":{}}}}}}`, "declares nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fromAdapted([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDecodeIsStrict(t *testing.T) {
	good := `{"root":"/var/lib/liveswap","apps":{"blog":{"sqlite":["app.db"]}}}`
	if _, err := Decode([]byte(good)); err != nil {
		t.Fatalf("a good plan: %v", err)
	}
	// A plan with no app has no root: what the plan unit prints for a
	// Caddyfile in which no app declares a backup.
	if _, err := Decode([]byte(`{"apps":{}}`)); err != nil {
		t.Fatalf("a plan of nothing: %v", err)
	}
	for name, tc := range map[string]struct{ raw, want string }{
		"an unknown field":           {`{"root":"/var/lib/liveswap","apps":{},"exec":"/bin/sh"}`, "unknown field"},
		"an unknown field in an app": {`{"root":"/var/lib/liveswap","apps":{"blog":{"sqlite":["a.db"],"user":"root"}}}`, "unknown field"},
		"something after the plan":   {good + `{"root":"/"}`, "something follows"},
		"a ] after the plan":         {good + `]`, "something follows"},
		"a } after the plan":         {good + ` }`, "something follows"},
		"no root, with an app":       {`{"apps":{"blog":{"sqlite":["a.db"]}}}`, "not an absolute path"},
		"a bare object":              {`{}`, "not an absolute path"}, // no apps key at all: the plan unit always prints one
		"a null declaration":         {`{"root":"/var/lib/liveswap","apps":{"blog":null}}`, "is null"},
		"an app name with a slash":   {`{"root":"/var/lib/liveswap","apps":{"a/b":{"files":["."]}}}`, "is not one liveswap accepts"},
		"a path that escapes":        {`{"root":"/var/lib/liveswap","apps":{"blog":{"files":["../x"]}}}`, "outside"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Every file beside the Caddyfile and below is read for {$NAME} —
// what a run's view holds, imported or not — with nothing of the
// Caddyfile's syntax read: an import by a snippet's argument, a heredoc,
// a variable, a file nothing imports; all of it is only text here.
func TestEnvNamesReadsEveryFileBesideTheCaddyfile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir+"/Caddyfile", "{\n\temail {$ACME_EMAIL}\n}\n(s) {\n\timport {args[0]}\n}\nimport <<P\nsites/*.caddy\nP\nimport {$CONF:byenv}/e.caddy\n")
	write(t, dir+"/quoted dir/q.caddy", "respond {$QUOTED}\n")
	write(t, dir+"/byenv/e.caddy", "respond {$BEHIND_A_DEFAULT}\n")
	write(t, dir+"/sites/a.caddy", "{$DOMAIN:example.com} {\n\timport deeper/*\n}\n")
	write(t, dir+"/sites/deeper/b", "root * {$WEBROOT}\n")
	write(t, dir+"/not-imported.caddy", "respond {$UNUSED:x}\n")
	write(t, dir+"/huge.caddy", "root {$HUGE_ROOT:/safe}\n"+strings.Repeat("#\n", 1<<20))
	got, bare, within, err := envNames(dir+"/Caddyfile", true)
	if err != nil {
		t.Fatal(err)
	}
	// On a line a plan reads: the two roots, from any depth, with or
	// without a default; not a site address or a respond.
	// And where each is written: the files a probe copies.
	if !slices.Equal(within["WEBROOT"], []string{dir + "/sites/deeper/b"}) || !slices.Equal(within["ACME_EMAIL"], []string{dir + "/Caddyfile"}) {
		t.Fatalf("within: %v", within)
	}
	// DOMAIN, CONF, HUGE_ROOT and UNUSED are only ever written with a default.
	if want := []string{"ACME_EMAIL", "BEHIND_A_DEFAULT", "QUOTED", "WEBROOT"}; !reflect.DeepEqual(bare, want) {
		t.Fatalf("with no default = %v, want %v", bare, want)
	}
	if want := []string{"ACME_EMAIL", "BEHIND_A_DEFAULT", "CONF", "DOMAIN", "HUGE_ROOT", "QUOTED", "UNUSED", "WEBROOT"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}

	// A copy checked away from the box — a checkout, a home directory —
	// is read alone: what is beside it there is not what a run's view
	// holds, and could be a whole tree.
	if got, _, _, err := envNames(dir+"/Caddyfile", false); err != nil || !reflect.DeepEqual(got, []string{"ACME_EMAIL", "CONF"}) {
		t.Fatalf("the Caddyfile alone: %v, %v", got, err)
	}
	// Nor is text that only looks like a name one: `{$` and a `}` lines
	// apart in a page's JavaScript.
	write(t, dir+"/page.js", "const a = {$\n  b: 1 }\n")
	if got, _, _, _ := envNames(dir+"/Caddyfile", true); slices.ContainsFunc(got, func(n string) bool { return strings.ContainsAny(n, " \t\n") }) {
		t.Fatalf("names = %q", got)
	}
}

// fakeMotel stands in for `motel adapt`, failing where and how
// the real one does [measured]: a variable with no default, unset or
// empty, is a parse error ({$ACME_EMAIL}, {$ROOT_NO_DEFAULT}, {$MAGIC});
// a port ({$METRICS_PORT}, {$ADMIN_PORT}, {$PORT:8080}) has to be a
// number, and the error quotes the value that is not one; {$ENV:prod}
// names a file to import, so nothing but "prod" adapts; {$MAGIC} takes
// one value nobody would guess. The root comes from
// {$LIVESWAP_ROOT:/var/lib/liveswap} or {$ROOT_NO_DEFAULT}, a backup
// path from {$DB:app.db}, an app's name from {$APP_NAME:blog}, and
// {$DOMAIN} changes nothing a plan holds. What a plan reads — the root,
// the app lines, the sqlite and files lines — and what `protocols`
// checks by form are read from the text, substituted as Caddy
// substitutes them, so a copy with a value written in is read as the
// real adapter reads it; a literal import of a file that is there is
// read too, one level down. A backup block's subdirective that is
// neither sqlite nor files is refused as parseBackup refuses it,
// quoted at its line; a file saying "# refusals carry an import chain"
// has every refusal end as one of a token an import brought in does.
// The other variables are read from the
// environment, with the prefix the real adapter's directive errors
// carry; MAGIC is refused in words that are neither a directive's nor
// an import's. It fails if anything of the caller's environment reaches
// it.
func fakeMotel(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "motel")
	write(t, script, `#!/bin/sh
[ -n "$CALLER_SECRET" ] && { echo "the caller's environment reached the adapter" >&2; exit 1; }
for last; do :; done
dir=$(dirname "$last")
# subst: {$NAME} and {$NAME:default} in $1, as Caddy substitutes them —
# a variable set, to anything, is its value; one unset is its default,
# or nothing.
subst() {
	s=$1
	for v in $(printf '%s' "$s" | grep -o '{\$[A-Za-z0-9_]*' | sed 's/{\$//' | sort -u); do
		if eval "[ -n \"\${$v+x}\" ]"; then eval "val=\$$v"; else val=$(printf '%s' "$s" | sed -n "s/.*{\\\$$v:\([^}]*\)}.*/\1/p" | head -1); fi
		s=$(printf '%s' "$s" | sed "s|{\\\$$v:[^}]*}|$val|g; s|{\\\$$v}|$val|g")
	done
	printf '%s' "$s"
}
# The text read: the file, and what a literal import of a file that is
# there brings in (one level; a glob's, a snippet's or a variable's
# import is not followed here). The parser meets an import before any
# directive sees an argument.
# A line ending in a backslash chains the next onto it, as the lexer
# does.
text=$(mktemp); trap 'rm -f "$text"' EXIT
read_files=$last
for f in $(grep -E '^[[:space:]]*import[[:space:]]+[^{* ]+[[:space:]]*$' "$last" | awk '{print $2}'); do
	grep -q "^($f)" "$last" && continue # a snippet's name, not a file's
	case "$f" in /*) p=$f ;; *) p=$dir/$f ;; esac
	p=$(printf '%s' "$p" | sed 's|/\./|/|g')
	[ -e "$p" ] || { echo "Error: File to import not found: $f, at $last:1" >&2; exit 1; }
	read_files="$read_files $p"
done
for f in $read_files; do awk '{ if (sub(/\\$/, "")) printf "%s", $0; else print }' "$f" >>"$text"; echo >>"$text"; done
uses() { grep -q "{\$$1[:}]" "$text"; }
args() { grep -E "^[[:space:]]*$1[[:space:]]" "$text" | awk '{for (i = 2; i <= NF; i++) print $i}'; }
first() { args "$1" | head -1; }
# where a directive's token is written, file:line, as the adapter's
# errors end: the line its chain starts on, the chain beginning with the
# directive. A file saying "# place refusals at N" has every refusal
# placed at its line N, for the tests of a refusal placed elsewhere.
where() {
	chain=""; grep -q '^# refusals carry an import chain$' "$last" && chain=" import chain ['$last:1 (import x)']"
	at=$(sed -n 's/^# place refusals at \([0-9]*\)$/\1/p' "$last" | head -1)
	[ -n "$at" ] && { printf '%s:%s%s' "$last" "$at" "$chain"; return; }
	for f in $read_files; do
		n=$(awk -v d="$1" -v tok="$2" '{ if (!cont) { start = NR; head = ($1 == d) } if (head && index($0, tok)) { print start; exit } cont = (substr($0, length($0)) == "\\") }' "$f")
		[ -n "$n" ] && { printf '%s:%s%s' "$f" "$n" "$chain"; return; }
	done
}
if uses ENV; then
	if grep -q '{\$ENV}' "$text"; then e=$ENV; else e=${ENV:-prod}; fi
	[ "$e" = prod ] || { echo "Error: File to import not found: sites/$e.caddy, at $last:1" >&2; exit 1; }
fi
# An import glob that matches nothing: a warning, and the config all the
# same.
grep -E '^[[:space:]]*import[[:space:]]' "$text" | while read -r _ pat _; do
	case "$pat" in *'*'*) ;; *) continue ;; esac
	pat=$(printf '%s' "$pat" | sed 's|{\$ENV:prod}|'"${ENV:-prod}"'|')
	case "$pat" in /*) p=$pat ;; *) p=$dir/$pat ;; esac
	set -- $p
	[ -e "$1" ] || printf '{"level":"warn","ts":1,"msg":"No files matching import glob pattern","pattern":"%s"}\n' "$pat" >&2
done
# Directives, with the prefix the real adapter's directive errors carry
# — but MAGIC, refused in words that are neither a directive's nor an
# import's.
if uses ACME_EMAIL && [ -z "$ACME_EMAIL" ]; then echo "Error: parsing caddyfile tokens for 'email': wrong argument count or unexpected line ending after 'email'" >&2; exit 1; fi
if uses MAGIC; then
	if grep -q '{\$MAGIC}' "$text"; then m=$MAGIC; else m=${MAGIC:-xyzzy}; fi
	[ -z "$m" ] && { echo "Error: parsing caddyfile tokens for 'thing': wrong argument count or unexpected line ending after 'MAGIC'" >&2; exit 1; }
	[ "$m" != xyzzy ] && { echo "Error: unrecognized value '$m'" >&2; exit 1; }
fi
for v in METRICS_PORT ADMIN_PORT; do
	if uses $v; then
		eval "p=\${$v}"
		case "$p" in ""|*[!0-9]*) echo "Error: parsing key: invalid port '$p': strconv.Atoi" >&2; exit 1;; esac
	fi
done
case "${PORT:-8080}" in *[!0-9]*) echo "Error: invalid port '$PORT'" >&2; exit 1;; esac
# Directives that check their argument's form, as the real adapter
# checks protocols and lb_policy: read from the text, literal or
# substituted.
for t in $(args protocols); do
	p=$(subst "$t")
	case "$p" in
	"") echo "Error: parsing caddyfile tokens for 'tls': protocols requires one or two arguments, at $(where protocols "$t")" >&2; exit 1;;
	tls1.[0-3]) ;;
	*) echo "Error: parsing caddyfile tokens for 'tls': wrong protocol name or protocol not supported: '$p', at $(where protocols "$t")" >&2; exit 1;;
	esac
done
for t in $(args lb_policy); do
	p=$(subst "$t")
	case "$p" in
	round_robin|first|random) ;;
	*) echo "Error: parsing caddyfile tokens for 'reverse_proxy': getting module named 'http.reverse_proxy.selection_policies.$p': module not registered: http.reverse_proxy.selection_policies.$p, at $(where lb_policy "$t")" >&2; exit 1;;
	esac
done
# A backup block's subdirective is sqlite or files, as parseBackup has
# it: any other word there is refused, quoted, at its line.
k=$(awk '/^[[:space:]]*backup[[:space:]]*\{/ { b = 1; next } b && /^[[:space:]]*\}/ { b = 0 } b && $1 != "" && $1 != "sqlite" && $1 != "files" && $1 !~ /^#/ { print $1; exit }' "$text")
if [ -n "$k" ]; then
	kk=$(subst "$k")
	case "$kk" in sqlite|files) ;; *) echo "Error: parsing caddyfile tokens for 'liveswap': unknown backup subdirective \"$kk\", at $(where "$k" "$k")" >&2; exit 1;; esac
fi
# A header's value goes into the config as any token's would.
headers=""; for h in $(args header_up); do headers="$headers $(subst "$h")"; done
# What a plan reads, from the text as the real adapter reads it. A
# {env.NAME} root is the server's to resolve: printed as written
# [measured]. The first app declares the backup, with the sqlite and
# files lines; the others declare none; "app shop" brings a cart with a
# backup block that says nothing. The two markers at the top make a box
# where no app declares a backup, or where there is no app.
root=/var/lib/liveswap
r=$(first root)
if [ -n "$r" ]; then
	case "$r" in
	'{env.'*) root=$r ;;
	*) root=$(subst "$r"); [ -z "$root" ] && { echo "Error: parsing caddyfile tokens for 'liveswap': wrong argument count or unexpected line ending after 'root'" >&2; exit 1; } ;;
	esac
fi
apps=""
for a in $(args app); do case "$a" in '{'*'}'|[a-z]*) apps="$apps $(subst "$a")" ;; esac; done
[ -z "$apps" ] && apps=blog
sqlite=app.db; s=$(first sqlite); [ -n "$s" ] && sqlite=$(subst "$s")
files=""; for f in $(args files); do files="$files,\"$(subst "$f")\""; done
json=""; i=0
for a in $apps; do
	if [ $i -eq 0 ] && ! grep -q "^# no app declares a backup" "$text"; then
		b="{\"sqlite\":[\"$sqlite\"]"; [ -n "$files" ] && b="$b,\"files\":[${files#,}]"
		json="$json,\"$a\":{\"backup\":$b}}"
	else
		json="$json,\"$a\":{}"
	fi
	i=$((i + 1))
done
grep -q "^app shop" "$text" && json="$json,\"cart\":{\"backup\":null}"
grep -q '^# no app$' "$text" && json=""
printf '{"apps":{"http":{"domain":"%s","headers":"%s"},"liveswap":{"root":"%s","apps":{%s}}}}' "${DOMAIN:-example.com}" "${headers# }" "$root" "${json#,}"
`)
	old := motel
	motel = script
	t.Cleanup(func() { motel = old })
}

func TestMakeRefusesAPlanThatDependsOnTheEnvironment(t *testing.T) {
	fakeMotel(t)
	t.Setenv("CALLER_SECRET", "must not reach the adapter")
	t.Setenv("LIVESWAP_ROOT", "/the/callers/own") // nor may this decide anything
	for name, tc := range map[string]struct {
		caddyfile string
		want      []string // in the error; nil means the plan is made
	}{
		"no variables":                         {"liveswap {\n}\n", nil},
		"a variable that is not the plan's":    {"{$DOMAIN} {\n}\n", nil},
		"a variable an arbitrary value breaks": {":{$PORT:8080} {\n}\n", nil}, // cleared by the trial value "1"
		// The server, with ENV=staging, imports another file, which may
		// say anything about the root: not knowable here, so refused — as
		// a variable no other value adapts with, since a file that is not
		// there is an error, and so, here, is a glob that matches nothing.
		"a variable that names a file to import": {"import sites/{$ENV:prod}.caddy\n", []string{"ENV", "imports through", "write the import literally"}},
		"a variable in an import glob":           {"import sites/{$ENV:prod}/*.caddy\n", []string{"ENV", "imports through"}},
		// The same, with no default: a default would not settle it, and
		// the hint says what would.
		"an import with no default": {"import sites/{$ENV}.caddy\n", []string{"File to import not found: sites/", "ENV", "write the import literally"}},
		// A directive that checks its argument's form takes no made-up
		// value, and a value it refused reached no import, root, app name
		// or backup path (#394): the plan is what it would be anyway.
		"a value a directive checks by form":                   {"tls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", nil},
		"a value a directive checks by form, with no default":  {"tls {\n\tprotocols {$TLS_MIN}\n}\n", []string{"wrong protocol name", "TLS_MIN", "`{$NAME:value}`, settles it"}},
		"one variable in an import and in a directive's check": {"import sites/{$ENV:prod}.caddy\ntls {\n\tprotocols {$ENV:tls1.2}\n}\n", []string{"ENV", "imports through"}},
		"one variable in a glob and in a directive's check":    {"import sites/{$ENV:prod}/*.caddy\ntls {\n\tprotocols {$ENV:tls1.2}\n}\n", []string{"ENV", "imports through"}},
		"one variable in two directives' checks":               {"tls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\ntls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", nil},
		// The same variable where a plan reads, which no trial reached:
		// each occurrence tried on its own finds it (probe), and it is
		// refused as a root or a backup path from the environment is.
		"one variable in a directive's check and the root":      {"liveswap {\n\troot /srv/{$X:tls1.2}\n}\ntls {\n\tprotocols {$X:tls1.2}\n}\n", []string{"X", "depends on"}},
		"one variable in a directive's check and an app line":   {"app {$X:first} {\n}\nlb_policy {$X:first}\n", []string{"X", "depends on"}},
		"one variable in a directive's check and a backup path": {"backup {\n\tsqlite {$X:tls1.2}\n\tfiles  {$Y:tls1.3}\n}\ntls {\n\tprotocols {$X:tls1.2} {$Y:tls1.3}\n}\n", []string{"X, Y", "depends on"}},
		// A backup path whose default is the value the probe would try
		// there: the probe takes one the Caddyfile does not hold, and
		// finds the path moved.
		"one variable in a directive's check and a backup path whose default is the probe's marker":      {"backup {\n\tsqlite {$X:hsb-probe-x-1.invalid}\n}\ntls {\n\tprotocols {$X:tls1.2}\n}\n", []string{"X", "depends on"}},
		"one variable in a directive's check and a backup path whose default is the marker's other form": {"backup {\n\tsqlite {$X:hsb-probe-x.invalid}\n}\ntls {\n\tprotocols {$X:tls1.2}\n}\n", []string{"X", "depends on"}},
		// The argument on the line after, chained by a backslash: the
		// adapter places its refusal where the chain starts, and so is
		// the occurrence.
		"a value a directive checks by form, chained onto the line": {"tls {\n\tprotocols \\\n\t\t{$TLS_MIN:tls1.2}\n}\n", nil},
		// The lines a chain spans are counted again after it: the second
		// occurrence is on its physical line.
		"the same, and another after the chain": {"tls {\n\tprotocols \\\n\t\t{$TLS_MIN:tls1.2}\n}\ntls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", nil},
		// And an earlier line holding the same text does not place it.
		"a header first, then the directive's check": {"header_up X-Env {$X:tls1.2}\ntls {\n\tprotocols {$X:tls1.2}\n}\n", nil},
		// A refusal placed on another line — a directive that checks at
		// its block's end — is the directive's own all the same: only an
		// import carries a token elsewhere, and that shows in its words.
		"a value refused from another line": {"# place refusals at 1\ntls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", nil},
		// A refusal through an import — the adapter's words end in the
		// import chain — is of the first expansion the parser met; an
		// import with arguments may expand the occurrence again, where a
		// plan reads: not knowable here.
		"a value refused through an import": {"# refusals carry an import chain\ntls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", []string{"TLS_MIN", "could not be told"}},
		// A variable standing where a subdirective's name is read is
		// checked by form as a name, and another value names another
		// subdirective: not tried, refused as not knowable.
		"a variable where a subdirective's name is read": {"backup {\n\t{$KIND:files} app.db\n}\n", []string{"KIND", "could not be told"}},
		// The same, written where nothing a plan reads is: let through.
		"one variable in a directive's check and a header": {"tls {\n\tprotocols {$X:tls1.2}\n}\nheader_up X-Env {$X:tls1.2}\n", nil},
		// With no default: a default first, and with one the root next
		// (an accepted residual: two rounds of advice).
		"a root with no default, and a directive's check": {"root /srv/{$X}\ntls {\n\tprotocols {$X}\n}\n", []string{"X", "settles it"}},
		// A snippet with no argument is a snippet, not a file.
		"a snippet with no argument, and a directive's check": {"(where) {\n\trespond ok\n}\nimport where\ntls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", nil},
		// Words that are neither an import's nor a directive's own are
		// refused as before: what they are about is not known here.
		"a value nothing made up will do for, with a default": {"thing {$MAGIC:xyzzy}\n", []string{"MAGIC", "does not adapt", "literally"}},
		"the root":           {"liveswap {\n\troot {$LIVESWAP_ROOT:/var/lib/liveswap}\n}\n", []string{"LIVESWAP_ROOT"}},
		"a backup path":      {"backup {\n\tsqlite {$DB:app.db}\n}\n", []string{"DB"}},
		"both, among others": {"{$DOMAIN:example.com} :{$PORT:80}\nroot {$LIVESWAP_ROOT:/var/lib/liveswap}\nsqlite {$DB:app.db}\n", []string{"DB, LIVESWAP_ROOT"}},
		// The ordinary production file: variables with no default, which
		// an empty environment makes a parse error of. Made-up values, of
		// the kind the adapter takes in each place, and the plan is what
		// it would be anyway.
		"an email with no default":             {"email {$ACME_EMAIL}\n", nil},
		"and a port, which has to be a number": {"email {$ACME_EMAIL}\n:{$METRICS_PORT} {\n}\n", nil},
		"and a second port":                    {"email {$ACME_EMAIL}\n:{$METRICS_PORT} {\n}\n:{$ADMIN_PORT} {\n}\n", nil},
		"an app's name":                        {"app {$APP_NAME:blog} {\n}\n", []string{"APP_NAME", "depends on"}},
		"a root with no default":               {"root {$ROOT_NO_DEFAULT}\n", []string{"ROOT_NO_DEFAULT", "depends on"}},
		"a value nothing made up will do for":  {"thing {$MAGIC}\n", []string{"unrecognized value", "MAGIC", "default"}},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "Caddyfile")
			write(t, file, tc.caddyfile)
			write(t, filepath.Join(filepath.Dir(file), "sites", "prod", "a.caddy"), "# what the default imports\n")
			p, err := Make(context.Background(), file)
			if tc.want == nil {
				if err != nil || p.Root != "/var/lib/liveswap" {
					t.Fatalf("want the plan, made with no environment at all, got %+v, %v", p, err)
				}
				return
			}
			for _, w := range tc.want {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Fatalf("want an error naming %q, got %v", w, err)
				}
			}
			if strings.Contains(err.Error(), "DOMAIN") || strings.Contains(err.Error(), "PORT") {
				t.Fatalf("the error blames a variable the plan does not depend on: %v", err)
			}
		})
	}
}
