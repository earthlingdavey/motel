package plan

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// What validate says beyond the plan: the apps that declare no backup —
// a new app's block with the two lines forgotten is otherwise silent.
func TestInspectNamesTheAppsThatDeclareNoBackup(t *testing.T) {
	fakeMotel(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app blog\napp shop\n")
	got, err := Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan == nil || len(got.Plan.Apps) != 1 || got.Plan.Apps["blog"] == nil {
		t.Fatalf("the plan: %+v", got.Plan)
	}
	if !slices.Equal(got.Undeclared, []string{"cart", "shop"}) {
		t.Fatalf("undeclared: %q, want cart and shop", got.Undeclared)
	}
	made, err := Make(context.Background(), file)
	if err != nil || len(made.Apps) != 1 {
		t.Fatalf("Make no longer agrees: %+v, %v", made, err)
	}
}

func TestInspectWithEveryAppDeclared(t *testing.T) {
	fakeMotel(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app blog\n")
	got, err := Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil || len(got.Undeclared) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

// An import glob that matches nothing is a warning to the adapter and a
// refusal here: inside a run's view a glob reaching outside it — or
// through a directory the run may not list — matches nothing, and the
// run would plan without what is declared there. The adapter's warning
// comes after its own snippets, heredocs and {$NAME} [measured with the
// real one; the stand-in here warns for the file's own lines].
func TestAnImportGlobThatMatchesNothingIsRefused(t *testing.T) {
	fakeMotel(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	for _, line := range []string{"import /srv/apps/*.caddy", "import sites/*.caddy"} {
		write(t, file, line+"\napp blog\n")
		for what, err := range map[string]error{"Inspect": second(Inspect(context.Background(), file, filepath.Dir(file))), "Make": second(Make(context.Background(), file))} {
			pattern := strings.TrimPrefix(line, "import ")
			if err == nil || !strings.Contains(err.Error(), pattern+", which matches no file") || !strings.Contains(err.Error(), "its own directory") {
				t.Errorf("%s of %q: %v", what, line, err)
			}
		}
	}
	// One that matches is no business of this.
	write(t, filepath.Join(dir, "sites", "blog.caddy"), "# nothing\n")
	write(t, file, "import sites/*.caddy\napp blog\n")
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("a glob that matches: %v", err)
	}
}

// An empty glob with no variable in it is not the variables' doing: it
// is said as it is, and no made-up value is tried in its place.
func TestAnEmptyGlobIsNotBlamedOnAVariable(t *testing.T) {
	fakeMotel(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "email {$ACME_EMAIL}\nimport /srv/none/*.caddy\napp blog\n")
	_, err := Make(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), "/srv/none/*.caddy, which matches no file") || strings.Contains(err.Error(), "made-up values") {
		t.Fatalf("%v", err)
	}
}

// Nor is an import that fails with no made-up value in the adapter's
// words — a literal path that is not there — whatever variables the
// file has besides: the hint about imports is for a path made of one.
func TestALiteralImportThatIsNotThereIsNotBlamedOnAVariable(t *testing.T) {
	fakeMotel(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "import sites/missing.caddy\nemail {$ACME_EMAIL}\n")
	_, err := Make(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), "File to import not found: sites/missing.caddy") || strings.Contains(err.Error(), "made-up values") {
		t.Fatalf("%v", err)
	}
	// Nor a literal glob that matches nothing, warned of beside the error
	// of a directive that took no made-up value: the directive's hint,
	// not the import's.
	write(t, file, "import nowhere/*.caddy\nthing {$MAGIC}\n")
	_, err = Make(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), "unrecognized value") || !strings.Contains(err.Error(), "settles it") || strings.Contains(err.Error(), "import literally") {
		t.Fatalf("%v", err)
	}
}

// The adapter's warning is read as the adapter writes it, a JSON line
// among others on stderr, and never taken from anything else there.
func TestEmptyGlobsAreReadFromTheAdaptersOwnWarning(t *testing.T) {
	stderr := []byte(`{"level":"info","msg":"using config from file","file":"/etc/motel/Caddyfile"}
{"level":"warn","ts":1790355132.8,"msg":"No files matching import glob pattern","pattern":"/srv/apps/*.caddy"}
No files matching import glob pattern: not JSON, not the adapter's
{"level":"warn","msg":"Caddyfile input is not formatted"}
{"level":"warn","msg":"No files matching import glob pattern","pattern":"sites/*.caddy"}
`)
	if got := emptyGlobs(stderr); !slices.Equal(got, []string{"/srv/apps/*.caddy", "sites/*.caddy"}) {
		t.Fatalf("%q", got)
	}
}

// A variable every made-up value fails at a directive's check of is
// tried one occurrence at a time, in a copy of the Caddyfile's
// directory (probe). An occurrence in a file the Caddyfile imports is
// in the copy, substituted, and found; one the adapter reaches past the
// copy — an import by absolute path — is seen by the value the copy's
// environment gives the variable, and refused as not knowable; and a
// copy checked away from its directory, read alone, is refused the same
// way where a file beside it holds the variable.
func TestAProbeReachesWhatTheCaddyfileImports(t *testing.T) {
	fakeMotel(t)
	for name, tc := range map[string]struct {
		imports, site, refused string // the import line; sites/x.caddy; what the refusal names, "" for a plan made
		away                   bool   // checked as a copy away from its directory
	}{
		"a root in an imported file":                {"import sites/x.caddy", "root /srv/{$X:tls1.2}\n", "depends on the environment variable(s) X", false},
		"nothing a plan reads, in an imported file": {"import sites/x.caddy", "header_up X-Env {$X:tls1.2}\n", "", false},
		"an import by absolute path":                {"import ABS/sites/x.caddy", "header_up X-Env {$X:tls1.2}\n", "could not be told", false},
		"a copy read alone, the file beside it":     {"import sites/x.caddy", "header_up X-Env {$X:tls1.2}\n", "could not be told", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "Caddyfile")
			write(t, file, strings.Replace(tc.imports, "ABS", dir, 1)+"\ntls {\n\tprotocols {$X:tls1.2}\n}\n")
			write(t, filepath.Join(dir, "sites", "x.caddy"), tc.site)
			configDir := dir
			if tc.away {
				configDir = t.TempDir()
			}
			_, err := Inspect(context.Background(), file, configDir)
			if tc.refused == "" && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Fatalf("want a refusal naming %q, got %v", tc.refused, err)
			}
		})
	}
}

// A probe's refusal is the directive's own unless it came through an
// import: a token a snippet or an imported file brought in has the
// import chain after the line [measured, Caddy 2.11.7], and is refused
// once where the import may expand it again.
func TestARefusalThroughAnImportIsNotTheDirectivesOwn(t *testing.T) {
	plain := `{"level":"error","msg":"parsing caddyfile tokens for 'tls': wrong protocol name or protocol not supported: 'hsb-probe-x-1.invalid', at /tmp/hsb-probe-1/Caddyfile:3"}`
	chain := `{"level":"error","msg":"parsing caddyfile tokens for 'tls': wrong protocol name or protocol not supported: 'hsb-probe-x-1.invalid', at /tmp/hsb-probe-1/Caddyfile:21 import chain ['/tmp/hsb-probe-1/Caddyfile:25 (import site)']"}`
	if throughAnImport(plain) || !throughAnImport(chain) || throughAnImport("") {
		t.Fatal("throughAnImport")
	}
}

// An import's words end before the parser's location: its line number
// is a digit, and so is a value a variable may be tried at ("1", "2").
func TestAnImportsWordsEndBeforeTheParsersLocation(t *testing.T) {
	words, atImport := importWords(errors.New("exit status 1"), `{"level":"error","ts":1,"msg":"File to import not found: sites/1.caddy, at /etc/motel/Caddyfile:1"}`+"\n")
	if !atImport || !strings.HasSuffix(words, "sites/1.caddy") {
		t.Fatalf("importWords = %q, %v", words, atImport)
	}
	if words, atImport := importWords(&emptyGlobError{[]string{"sites/1/*.caddy"}}, ""); !atImport || words != "sites/1/*.caddy" {
		t.Fatalf("importWords of an empty glob = %q, %v", words, atImport)
	}
}

// Where a token stands is read as the lexer reads lines [Caddy 2.11.7,
// caddyconfig/caddyfile/lexer.go, parse.go; the comment's backslash and
// the escaped one held by
// TestIntegrationTheAdapterRefusesAnImportAndADirectiveAsInspectReadsThem]:
// a `\` escapes the newline after it, through the spaces before any
// token and through the rest of a comment; a space after a token
// returns the token and drops the escape; any other character uses it
// up; spaces are unicode.IsSpace. The first token of a logical line, or the
// first after a `}`, is where a directive's or a subdirective's name is
// read.
func TestATokenLeadingItsLineStandsWhereANameIsRead(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		leads bool
	}{
		{"{$X} a\n", true},
		{"  {$X} a\n", true},
		{"root {$X}\n", false},
		{"a\nroot {$X}\n", false},
		{"root \\\n\t{$X}\n", false},
		{"a\r\nroot \\\r\n\t{$X}\r\n", false},
		{"root \\ \t\n{$X}\n", false},  // the escape holds through spaces
		{"root \\\\\n{$X}\n", true},    // an escaped backslash chains nothing
		{"root \\\\\\\n{$X}\n", false}, // three: one escaped, one chaining
		{"root a\\b\n{$X}\n", true},    // used up by the b
		{"\\\n{$X}\n", true},
		{"} {$X} a\n", true},
		{"}\t\\\n{$X}\n", true},
		{"# c \\\n{$X} a\n", true},             // the comment ends at the newline the backslash escapes
		{"# C:\\certs\n{$X} a\n", true},        // a backslash anywhere in a comment holds to its end
		{"root /x # C:\\certs\n{$X}\n", false}, // and chains the line onto the directive before the comment
		{"foo # c \\\n{$X}\n", false},
		{"foo#bar\\\n{$X}\n", false}, // no comment: the # is not at a token's start
		{"# {$X}\n", false},          // a comment's own line
		{"a\nb \\\n\t{$X}\nc\n", false},
		{"\t\"{$X}\" a\n", true}, // a quoted name is a name
		{"`{$X}` a\n", true},
		{"root \"{$X}\"\n", false}, // a quoted argument
		{"\v{$X}\n", true},         // the lexer's other spaces
		{"\u00a0{$X} a\n", true},   // and its Unicode ones
		{"root\u00a0{$X}\n", false},
		{"files uploads\\ \n{$X} a\n", true}, // the space returns the token, and the escape goes with it
		{"files uploads\\\n{$X} a\n", false},
		{"root \\\u00a0\n{$X}\n", false}, // no token begun: the escape holds through the space
	} {
		off := strings.Index(tc.raw, "{$X}")
		if got := leadsItsLine([]byte(tc.raw), off); got != tc.leads {
			t.Errorf("leadsItsLine(%q) = %v", tc.raw, got)
		}
	}
}

// What a failed trial failed at is read off the adapter's words as the
// real adapter writes them [measured, Caddy 2.11.7; held there by
// TestIntegrationTheAdapterRefusesAnImportAndADirectiveAsInspectReadsThem]:
// its parser's about an import, a directive's own with the HTTP
// adapter's prefix, or neither.
func TestTheAdaptersWordsAreReadAsItWritesThem(t *testing.T) {
	for _, tc := range []struct {
		said                  string
		atImport, atDirective bool
	}{
		{`{"level":"error","ts":1,"msg":"File to import not found: sites/hsb-placeholder-env.invalid.caddy, at /etc/motel/Caddyfile:1"}`, true, false},
		{`{"level":"error","ts":1,"msg":"Could not import /etc/motel/sites: is a directory, at /etc/motel/Caddyfile:1"}`, true, false},
		{`{"level":"warn","ts":1,"msg":"No files matching import glob pattern","pattern":"sites/hsb-placeholder-env.invalid/*.caddy"}`, true, false},
		// A glob matching nothing is a warning, and the directive after
		// it fails: the import's, since the parser met it first.
		{`{"level":"warn","ts":1,"msg":"No files matching import glob pattern","pattern":"sites/hsb-placeholder-env.invalid/*.caddy"}
{"level":"error","ts":1,"msg":"parsing caddyfile tokens for 'tls': wrong protocol name or protocol not supported: 'hsb-placeholder-env.invalid', at /etc/motel/Caddyfile:4"}`, true, true},
		{`{"level":"error","ts":1,"msg":"parsing caddyfile tokens for 'tls': wrong protocol name or protocol not supported: '1', at /etc/motel/Caddyfile:3"}`, false, true},
		{`{"level":"error","ts":1,"msg":"parsing caddyfile tokens for 'reverse_proxy': getting module named 'http.reverse_proxy.selection_policies.1': module not registered: http.reverse_proxy.selection_policies.1, at /etc/motel/Caddyfile:3"}`, false, true},
		{`{"level":"error","ts":1,"msg":"parsing caddyfile tokens for 'redir': Redir code not in the 3xx range or 401: '1', at /etc/motel/Caddyfile:2"}`, false, true},
		{`{"level":"error","ts":1,"msg":"parsing caddyfile tokens for 'email': wrong argument count or unexpected line ending after 'email', at /etc/motel/Caddyfile:2"}`, false, true},
		{`Error: unrecognized value 'hsb-placeholder-magic.invalid'`, false, false},
		{``, false, false},
	} {
		if got := failedAtImport(tc.said); got != tc.atImport {
			t.Errorf("failedAtImport(%q) = %v", tc.said, got)
		}
		if got := failedAtDirective(tc.said); got != tc.atDirective {
			t.Errorf("failedAtDirective(%q) = %v", tc.said, got)
		}
	}
}

// An app that declares no backup and takes its name from the
// environment is named as the server names it only by luck: it is said
// as that, not under the name a default gives it here.
func TestAnUndeclaredAppNamedThroughTheEnvironment(t *testing.T) {
	fakeMotel(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app blog\napp {$SHOP_NAME:shop}\n")
	got, err := Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Undeclared, "shop") || !slices.Equal(got.UndeclaredByEnv, []string{"SHOP_NAME"}) {
		t.Fatalf("undeclared %q, by the environment %q", got.Undeclared, got.UndeclaredByEnv)
	}

	// A variable tried after it, that names nothing, is not blamed for it
	// — and one that also names an undeclared app is.
	write(t, file, "app blog\napp {$SHOP_NAME:shop}\nemail {$Z_EMAIL:a@example.com}\n")
	got, err = Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.UndeclaredByEnv, []string{"SHOP_NAME"}) || len(got.Undeclared) != 0 {
		t.Fatalf("with an unrelated variable after it: undeclared %q, by the environment %q", got.Undeclared, got.UndeclaredByEnv)
	}
}

func second[T any](_ T, err error) error { return err }

// A root that depends on the environment is refused only where an app
// declares a backup: with none declared a run backs nothing up, and the
// root is not its business — step 6 on a box that uses no backups
// would otherwise refuse a Caddyfile that works today (the owner, 4a:
// decide in 4c). Such a plan carries no root at all, so that nothing
// downstream reads a placeholder as a path. What hides an app — an
// import through a variable, a glob that matches nothing — is refused
// as before, backup or no backup.
func TestAPlaceholderRootIsRefusedOnlyWhereABackupIsDeclared(t *testing.T) {
	fakeMotel(t)
	for _, tc := range []struct {
		name, caddyfile string
		refused         string // in the error; empty means the plan is made
		root            string // of the plan made
	}{
		{"a defaulted root, a backup declared", "root {$LIVESWAP_ROOT:/var/lib/liveswap}\n", "LIVESWAP_ROOT", ""},
		{"a defaulted root, no backup declared", "# no app declares a backup\nroot {$LIVESWAP_ROOT:/var/lib/liveswap}\n", "", ""},
		{"a defaulted root, no app", "# no app\nroot {$LIVESWAP_ROOT:/var/lib/liveswap}\n", "", ""},
		{"a root with no default, a backup declared", "root {$ROOT_NO_DEFAULT}\n", "ROOT_NO_DEFAULT", ""},
		{"a root with no default, no backup declared", "# no app declares a backup\nroot {$ROOT_NO_DEFAULT}\n", "", ""},
		{"a literal root, no backup declared", "# no app declares a backup\nroot /var/lib/liveswap\n", "", ""},
		{"a literal root, a backup declared", "root /var/lib/liveswap\n", "", "/var/lib/liveswap"},
		// A {env.NAME} root reaches the plan as written, no variable to
		// try: refused for a backup's sake only.
		{"an env root, a backup declared", "root {env.LIVESWAP_ROOT}\n", "placeholder", ""},
		{"an env root, no backup declared", "# no app declares a backup\nroot {env.LIVESWAP_ROOT}\n", "", ""},
		{"an import through a variable, no backup declared", "# no app declares a backup\nimport sites/{$ENV:prod}.caddy\n", "ENV", ""},
		{"an import glob through a variable, no backup declared", "# no app declares a backup\nimport sites/{$ENV:prod}/*.caddy\n", "ENV", ""},
		{"an empty glob, no backup declared", "# no app declares a backup\nimport nowhere/*.caddy\n", "matches no file", ""},
		// A value a directive checks by form (#394): no made-up value
		// adapts, and none reached anything a plan reads.
		{"a value a directive checks by form, no backup declared", "# no app declares a backup\ntls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", "", ""},
		{"a value a directive checks by form, a backup declared", "tls {\n\tprotocols {$TLS_MIN:tls1.2}\n}\n", "", "/var/lib/liveswap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "Caddyfile")
			write(t, file, tc.caddyfile)
			write(t, filepath.Join(filepath.Dir(file), "sites", "prod", "a.caddy"), "# what the default imports\n")
			ins, err := Inspect(context.Background(), file, filepath.Dir(file))
			made, merr := Make(context.Background(), file)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) || merr == nil || !strings.Contains(merr.Error(), tc.refused) {
					t.Fatalf("want a refusal naming %q: validate %v, run %v", tc.refused, err, merr)
				}
				return
			}
			if err != nil || merr != nil {
				t.Fatalf("refused: validate %v, run %v", err, merr)
			}
			if ins.Plan.Root != tc.root || made.Root != tc.root {
				t.Fatalf("root: validate %q, run %q, want %q", ins.Plan.Root, made.Root, tc.root)
			}
			if p := ins.Plan; tc.root == "" && len(p.Apps) != 0 {
				t.Fatalf("a plan with no root has apps: %+v", p.Apps)
			}
		})
	}
}
