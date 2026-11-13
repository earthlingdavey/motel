package box

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	"golang.org/x/crypto/ssh"
)

// keys are two real ed25519 public keys for signer lines.
var keys = func() [2]string {
	var out [2]string
	for i := range out {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		k, err := ssh.NewPublicKey(pub)
		if err != nil {
			panic(err)
		}
		out[i] = base64.StdEncoding.EncodeToString(k.Marshal())
	}
	return out
}()

// file is a Caddyfile from a template in which K1 and K2 are the keys.
func file(s string) []byte {
	return []byte(strings.NewReplacer("K1", keys[0], "K2", keys[1]).Replace(s))
}

const good = `{
	admin unix//run/motel/admin.sock
	email {$ACME_EMAIL:ops@example.com}

	box {
		deploy_trust github {
			audience motel
			claim repository your-org/boxes
		}
		# signers
		signer alice@example.com ssh-ed25519 K1
		signer bob ssh-ed25519 K2
	}

	liveswap {
		app example {
			command node server.js
		}
	}
}

example.com {
	reverse_proxy {
		dynamic liveswap example
	}
}

deploy.example.com {
	liveswap_webhook
	box_webhook
}
`

// Apps are the global liveswap block's `app` lines in liveswap's
// name grammar, in file order; nothing else named `app` counts.
func TestWalkApps(t *testing.T) {
	f := strings.Replace(good, "\t\tapp example {", "\t\tapp Bad_Name {\n\t\t}\n\t\tapp second {\n\t\t}\n\t\tapp example {", 1)
	f = strings.Replace(f, "deploy.example.com {", "deploy.example.com {\n\trespond /app 200", 1)
	s, err := Walk(file(f))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Apps, ",") != "second,example" {
		t.Errorf("apps %q", s.Apps)
	}
}

func TestWalkGood(t *testing.T) {
	s, err := Walk(file(good))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "deploy.example.com" || len(s.Signers) != 2 || s.Signers[0].Principal != "alice@example.com" || s.Signers[1].Principal != "bob" || s.Signers[0].B64 != keys[0] {
		t.Fatalf("%+v", s)
	}
}

func TestWalkAccepts(t *testing.T) {
	for name, in := range map[string]string{
		"host upper-cased":         strings.Replace(good, "deploy.example.com {", "Deploy.Example.COM {", 1),
		"webhook twice":            strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook\n\tbox_webhook\n", 1),
		"matcher":                  strings.Replace(good, "\tbox_webhook\n", "\t@m {\n\t\tpath /x\n\t}\n\trespond @m ok\n\tbox_webhook\n", 1),
		"placeholder value":        strings.Replace(good, "\tbox_webhook\n", "\trespond {$MSG:hi}\n\tbox_webhook\n", 1),
		"heredoc value":            strings.Replace(good, "\tbox_webhook\n", "\trespond <<EOF\n\thello\n\timport nothing\n\tEOF 200\n\tbox_webhook\n", 1),
		"quoted multiline":         strings.Replace(good, "\tbox_webhook\n", "\trespond \"a\nimport b\n\"\n\tbox_webhook\n", 1),
		"nested close on the line": strings.Replace(good, "\tbox_webhook\n", "\troute {\n\t\trespond ok }\n\tbox_webhook\n", 1),
		"empty box_webhook":        strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook {\n\t}\n", 1),
		"comments":                 strings.Replace(good, "\tbox_webhook\n", "\t# import x\n\tbox_webhook # not import\n", 1),
		"single label host":        strings.Replace(good, "deploy.example.com {", "localhost {", 1),
		"ip host":                  strings.Replace(good, "deploy.example.com {", "192.0.2.1 {", 1),
		"braced empty site":        good + "\nother.example.com {\n}\n",
		"quoted braces are values": strings.Replace(good, "\tbox_webhook\n", "\trespond \"{\"\n\trespond \"}\" 200\n\theader X \"{}\"\n\tbox_webhook\n", 1),
		"field named box_webhook":  strings.Replace(good, "\tbox_webhook\n", "\theader {\n\t\tX-Hook box_webhook\n\t}\n\tbox_webhook\n", 1),
		"field in another site":    strings.Replace(good, "example.com {\n", "example.com {\n\theader {\n\t\tX-Hook box_webhook\n\t}\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Walk(file(in))
			if err != nil {
				t.Fatal(err)
			}
			if s.Host == "" || len(s.Signers) != 2 {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestWalkRefuses(t *testing.T) {
	noKey := "\t\tsigner alice ssh-ed25519 K1\n"
	for name, c := range map[string]struct{ in, want string }{
		"empty":                          {"", "is empty"},
		"whitespace":                     {"\n\t\n", "is empty"},
		"comment only":                   {"# nothing\n", "has no box block"},
		"no global block":                {"deploy.example.com {\n\tbox_webhook\n}\n", "has no box block"},
		"global without box":             {"{\n\tadmin off\n}\ndeploy.example.com {\n\tbox_webhook\n}\n", "has no box block"},
		"box in a site":                  {"{\n}\ndeploy.example.com {\n\tbox {\n" + noKey + "\t}\n\tbox_webhook\n}\n", "has no box block"},
		"two boxes":                      {strings.Replace(good, "\tbox {\n", "\tbox {\n\t}\n\tbox {\n", 1), "has more than one box block"},
		"two global blocks":              {"{\n}\n" + good, "has a global options block that is not the first block"},
		"no signer":                      {strings.Replace(strings.Replace(good, "\t\tsigner alice@example.com ssh-ed25519 K1\n", "", 1), "\t\tsigner bob ssh-ed25519 K2\n", "", 1), "has no signer"},
		"box empty":                      {strings.Replace(good, "\tbox {\n", "\tbox {\n\t}\n\tbox_ {\n", 1), "has no signer"},
		"box without a block":            {strings.Replace(good, "\tbox {\n", "\tbox\n\tbox_ {\n", 1), "has no signer"},
		"no deploy_trust":                {strings.Replace(good, "\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository your-org/boxes\n\t\t}\n", "", 1), "has no deploy_trust"},
		"empty deploy_trust":             {strings.Replace(good, "\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository your-org/boxes\n\t\t}\n", "\t\tdeploy_trust github {\n\t\t}\n", 1), "has no deploy_trust"},
		"signer two args":                {strings.Replace(good, "signer bob ssh-ed25519 K2", "signer bob K2", 1), "has a signer line that is not `signer <principal> <key-type> <base64>`"},
		"signer four args":               {strings.Replace(good, "signer bob ssh-ed25519 K2", "signer bob ssh-ed25519 K2 extra", 1), "has a signer line that is not"},
		"signer with a block":            {strings.Replace(good, "signer bob ssh-ed25519 K2", "signer bob ssh-ed25519 K2 {\n\t\t}", 1), "has a signer line that is not"},
		"signer bad principal":           {strings.Replace(good, "signer bob ", "signer b*b ", 1), "has a bad signer principal \"b*b\" may only contain"},
		"signer bad type":                {strings.Replace(good, "signer bob ssh-ed25519", "signer bob ssh-dss", 1), "has a bad signer bob: key type \"ssh-dss\" is not one of"},
		"signer bad key":                 {strings.Replace(good, "ssh-ed25519 K2", "ssh-ed25519 AAAA", 1), "has a bad signer bob: the key does not parse"},
		"signer same key twice":          {strings.Replace(good, "ssh-ed25519 K2", "ssh-ed25519 K1", 1), "has a bad signer alice@example.com and signer bob are the same key"},
		"no webhook":                     {strings.Replace(good, "\tbox_webhook\n", "", 1), "has no site with box_webhook"},
		"webhook with a path matcher":    {strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook /hook\n", 1), "has box_webhook with arguments (/hook); it takes none, and a matcher would leave / unserved"},
		"webhook with a named matcher":   {strings.Replace(good, "\tbox_webhook\n", "\t@m path /\n\tbox_webhook @m\n", 1), "has box_webhook with arguments (@m);"},
		"webhook with a wildcard":        {strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook *\n", 1), "has box_webhook with arguments (*);"},
		"webhook with an empty argument": {strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook \"\"\n", 1), "has box_webhook with arguments ();"},
		"webhook in snippet":             {strings.Replace(good, "\tbox_webhook\n", "", 1) + "(s) {\n\tbox_webhook\n}\n", "has box_webhook inside a snippet or named route ((s)); write it in the site"},
		"two webhook sites":              {good + "\nother.example.com {\n\tbox_webhook\n}\n", "has more than one site with box_webhook"},
		"two addresses":                  {strings.Replace(good, "deploy.example.com {", "deploy.example.com, deploy2.example.com {", 1), "has a box_webhook site whose address is not one bare hostname (deploy.example.com, deploy2.example.com)"},
		"continued addresses":            {strings.Replace(good, "deploy.example.com {", "deploy.example.com,\ndeploy2.example.com {", 1), "not one bare hostname (deploy.example.com, deploy2.example.com)"},
		"scheme":                         {strings.Replace(good, "deploy.example.com {", "https://deploy.example.com {", 1), "not one bare hostname (https://deploy.example.com)"},
		"port":                           {strings.Replace(good, "deploy.example.com {", "deploy.example.com:8443 {", 1), "not one bare hostname (deploy.example.com:8443)"},
		"path":                           {strings.Replace(good, "deploy.example.com {", "deploy.example.com/hook {", 1), "not one bare hostname"},
		"wildcard":                       {strings.Replace(good, "deploy.example.com {", "*.example.com {", 1), "not one bare hostname (*.example.com)"},
		"port only":                      {strings.Replace(good, "deploy.example.com {", ":443 {", 1), "not one bare hostname (:443)"},
		"trailing dot":                   {strings.Replace(good, "deploy.example.com {", "deploy.example.com. {", 1), "not one bare hostname"},
		"hyphen label":                   {strings.Replace(good, "deploy.example.com {", "-deploy.example.com {", 1), "not one bare hostname"},
		"underscore":                     {strings.Replace(good, "deploy.example.com {", "deploy_1.example.com {", 1), "not one bare hostname"},
		"placeholder address":            {strings.Replace(good, "deploy.example.com {", "{$DEPLOY_HOST} {", 1), "has a placeholder where a directive name, a site address or a box line goes ({$DEPLOY_HOST})"},
		"placeholder in address":         {strings.Replace(good, "deploy.example.com {", "deploy.{$DOMAIN}.com {", 1), "has a placeholder where"},
		"placeholder directive":          {strings.Replace(good, "\tbox_webhook\n", "\t{$DIRECTIVE:respond} ok\n\tbox_webhook\n", 1), "has a placeholder where a directive name, a site address or a box line goes ({$DIRECTIVE:respond})"},
		"placeholder option":             {strings.Replace(good, "\tadmin unix", "\t{$ADMIN:admin} unix", 1), "has a placeholder where"},
		"placeholder signer":             {strings.Replace(good, "ssh-ed25519 K2", "ssh-ed25519 {$BOB_KEY}", 1), "has a placeholder where a directive name, a site address or a box line goes ({$BOB_KEY})"},
		"placeholder in trust":           {strings.Replace(good, "audience motel", "audience {$AUD}", 1), "has a placeholder where"},
		"placeholder box line":           {strings.Replace(good, "\t\t# signers\n", "\t\t{$X:signer} a b c\n", 1), "has a placeholder where"},
		"import top":                     {"import snippets/*\n" + good, "imports snippets/*; inline the snippet"},
		"import in global":               {strings.Replace(good, "\tadmin unix", "\timport common\n\tadmin unix", 1), "imports common;"},
		"import in box":                  {strings.Replace(good, "\t\t# signers\n", "\t\timport signers.caddy\n", 1), "imports signers.caddy;"},
		"import in site":                 {strings.Replace(good, "\tbox_webhook\n", "\timport tls-snippet\n\tbox_webhook\n", 1), "imports tls-snippet;"},
		"import nested":                  {strings.Replace(good, "\tbox_webhook\n", "\troute {\n\t\timport inner arg\n\t}\n\tbox_webhook\n", 1), "imports inner;"},
		"import bare":                    {strings.Replace(good, "\tbox_webhook\n", "\timport\n\tbox_webhook\n", 1), "imports; inline"},
		"import quoted":                  {strings.Replace(good, "\tbox_webhook\n", "\t\"import\" x\n\tbox_webhook\n", 1), "imports x;"},
		"import after comma":             {strings.Replace(good, "deploy.example.com {", "deploy.example.com,\nimport x\n{", 1), "imports x;"},
		"import snippet body":            {"(s) {\n\timport x\n}\n" + good, "imports x;"},
		"unclosed":                       {strings.TrimSuffix(good, "}\n"), "does not parse: a block is not closed"},
		"stray close":                    {good + "}\n", "does not parse: a } with no block open"},
		"braceless site":                 {good + "\nother.example.com\n", "has a site without braces (other.example.com)"},
		"braceless site body":            {strings.Replace(good, "deploy.example.com {\n\tliveswap_webhook\n\tbox_webhook\n}\n", "deploy.example.com\nliveswap_webhook\nbox_webhook\n", 1), "has a site without braces (deploy.example.com)"},
		"braceless continued":            {good + "\na.com,\nb.com\n", "has a site without braces (a.com, b.com)"},
		"comma then brace":               {strings.Replace(good, "deploy.example.com {", "deploy.example.com,\n{", 1), "does not parse: a site address list ends with a comma"},
		"webhook in named route":         {strings.Replace(good, "\tbox_webhook\n", "\tinvoke hook\n", 1) + "\n&(hook) {\n\tbox_webhook\n}\n", "has box_webhook inside a snippet or named route (&(hook)); write it in the site"},
		"webhook in snippet named":       {strings.Replace(good, "\tbox_webhook\n", "\trespond ok\n", 1) + "\n(s) {\n\tbox_webhook\n}\n", "has box_webhook inside a snippet or named route ((s)); write it in the site"},
		"lexer error is bounded":         {strings.Replace(good, "\tbox_webhook\n", "\trespond <<"+strings.Repeat("\x1b[2J", 100_000)+"\n", 1), "does not tokenize: "},
		"webhook as a header field":      {strings.Replace(good, "\tbox_webhook\n", "\theader {\n\t\tbox_webhook enabled\n\t}\n", 1), "has box_webhook inside header; it must stand directly in the site block, where no matcher or nested route can leave / unserved"},
		"webhook in a matcher":           {strings.Replace(good, "\tbox_webhook\n", "\t@m {\n\t\tbox_webhook\n\t}\n", 1), "has box_webhook inside @m;"},
		"webhook in an option block":     {strings.Replace(good, "\tbox_webhook\n", "\treverse_proxy x {\n\t\tbox_webhook\n\t}\n", 1), "has box_webhook inside reverse_proxy;"},
		"webhook under a matched handle": {strings.Replace(good, "\tbox_webhook\n", "\thandle /x {\n\t\tbox_webhook\n\t}\n", 1), "has box_webhook inside handle;"},
		"webhook under a route":          {strings.Replace(good, "\tbox_webhook\n", "\troute {\n\t\tbox_webhook\n\t}\n", 1), "has box_webhook inside route;"},
		"webhook under handle_path":      {strings.Replace(good, "\tbox_webhook\n", "\thandle_path /x {\n\t\troute {\n\t\t\tbox_webhook\n\t\t}\n\t}\n", 1), "has box_webhook inside route;"},
		"webhook in reverse_proxy's handle_response":       {strings.Replace(good, "\tbox_webhook\n", "\treverse_proxy x {\n\t\thandle_response {\n\t\t\tbox_webhook\n\t\t}\n\t}\n", 1), "has box_webhook inside handle_response;"},
		"webhook in intercept's handle_response":           {strings.Replace(good, "\tbox_webhook\n", "\tintercept {\n\t\thandle_response {\n\t\t\tbox_webhook\n\t\t}\n\t}\n", 1), "has box_webhook inside handle_response;"},
		"webhook in handle_response outside reverse_proxy": {strings.Replace(good, "\tbox_webhook\n", "\theader {\n\t\thandle_response {\n\t\t\tbox_webhook\n\t\t}\n\t}\n", 1), "has box_webhook inside handle_response;"},
		"webhook in global block":                          {strings.Replace(good, "\tadmin unix", "\tbox_webhook\n\tadmin unix", 1), "has box_webhook inside the global options;"},
		"webhook deep in named route":                      {strings.Replace(good, "\tbox_webhook\n", "\tinvoke hook\n", 1) + "\n&(hook) {\n\troute {\n\t\thandle {\n\t\t\tbox_webhook\n\t\t}\n\t}\n}\n", "has box_webhook inside a snippet or named route (&(hook)); write it in the site"},
		"webhook deep in snippet":                          {strings.Replace(good, "\tbox_webhook\n", "\trespond ok\n", 1) + "\n(s) {\n\treverse_proxy x {\n\t\thandle_response {\n\t\t\tbox_webhook\n\t\t}\n\t}\n}\n", "has box_webhook inside a snippet or named route ((s)); write it in the site"},
		"two hosts via handle_response":                    {strings.Replace(good, "example.com {\n\treverse_proxy {\n\t\tdynamic liveswap example\n\t}\n}\n", "example.com {\n\treverse_proxy {\n\t\tdynamic liveswap example\n\t\thandle_response {\n\t\t\tbox_webhook\n\t\t}\n\t}\n}\n", 1), "has box_webhook inside handle_response;"},
		"global block not first":                           {"other.example.com {\n\trespond ok\n}\n" + good, "has a global options block that is not the first block"},
		"site closed on a line":                            {strings.Replace(good, "\tbox_webhook\n}\n", "\tbox_webhook }\n", 1), "does not parse: a } that closes a top-level block on a directive's line"},
		"global closed on a line":                          {strings.Replace(good, "\t}\n}\n\nexample.com", "\t} }\n\nexample.com", 1), "does not parse: a } that closes a top-level block"},
		"token after brace":                                {strings.Replace(good, "deploy.example.com {", "deploy.example.com { box_webhook", 1), "does not parse"},
		"comma in address":                                 {strings.Replace(good, "deploy.example.com {", "a.com,b.com {", 1), "does not parse: a comma inside a site address"},
		"trailing comma":                                   {good + "\na.com,\n", "does not parse: a site address list ends with a comma"},
		"bad lexing":                                       {strings.Replace(good, "\tbox_webhook\n", "\trespond <<EOF\n\tnever closed\n\tbox_webhook\n", 1), "does not tokenize"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Walk(file(c.in))
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("want a refusal containing %q, got %v", c.want, err)
			}
			if !strings.Contains(r.Reason, c.want) {
				t.Fatalf("got %q, want it to contain %q", r.Reason, c.want)
			}
			if !strings.HasPrefix(err.Error(), "the Caddyfile ") {
				t.Fatal(err)
			}
			if len(err.Error()) > 400 {
				t.Fatalf("a refusal of %d bytes is not bounded", len(err.Error()))
			}
		})
	}
	// A named route without the webhook is not a site either: the one
	// real site still carries it.
	s, err := Walk(file(good + "\n&(other) {\n\trespond ok\n}\n"))
	if err != nil || s.Host != "deploy.example.com" {
		t.Fatal(s, err)
	}
}

// TestExpandEmptyEnvIsLinear: a megabyte of placeholders expands in
// well under a second, where splicing in place would take minutes.
func TestExpandEmptyEnvIsLinear(t *testing.T) {
	in := bytes.Repeat([]byte("{$A}"), 262_144)
	start := time.Now()
	if out := expandEmptyEnv(in); len(out) != 0 {
		t.Fatalf("%d bytes left", len(out))
	}
	in = bytes.Repeat([]byte("{$A:x}"), 174_762)
	if out := expandEmptyEnv(in); len(out) != 174_762 || out[0] != 'x' {
		t.Fatalf("%d bytes", len(out))
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

// TestWalkSeesThroughPlaceholderQuotes is the case the second reading
// exists for: on the raw bytes, `import evil` sits inside a quoted
// value and nothing is at directive position; after Caddy expands the
// two defaults — each a lone quote — the quoting flips and `import
// evil` is a line of its own. Caddy's own parser confirms it would
// import.
func TestWalkSeesThroughPlaceholderQuotes(t *testing.T) {
	in := file(strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook\n\trespond {$MOTEL_BOX_TEST_UNSET_Q:\"} \"a\nimport evil\" {$MOTEL_BOX_TEST_UNSET_R:\"}\n", 1))
	// The raw reading alone is fooled.
	if _, err := walk(in); err != nil {
		t.Fatalf("the raw reading should pass: %v", err)
	}
	_, err := Walk(in)
	var r *Refusal
	if !errors.As(err, &r) || !strings.HasPrefix(r.Reason, "imports ") {
		t.Fatalf("got %v", err)
	}
	unsetForTest(t, "MOTEL_BOX_TEST_UNSET_Q", "MOTEL_BOX_TEST_UNSET_R")
	if _, err := caddyfile.Parse("Caddyfile", in); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("Caddy's parser should try the import: %v", err)
	}
}

// TestExpandEmptyEnvPinsCaddy holds expandEmptyEnv to caddyfile.Parse:
// for an input whose placeholder names are unset, the tokens Parse
// makes are the tokens of the input expanded here, braces aside.
func TestExpandEmptyEnvPinsCaddy(t *testing.T) {
	const a, b = "MOTEL_BOX_TEST_UNSET_A", "MOTEL_BOX_TEST_UNSET_B"
	unsetForTest(t, a, b)
	for _, in := range []string{
		"x {$" + a + ":dflt} y",
		"{$" + b + "} x",
		"{$} y {$:z}",
		"x {$" + a + ":a:b} {$" + a + ":}",
		"x {$" + a + ":{$" + b + "}} y",
		"x {$" + a,
		"x {$" + a + ":\"} \"a\nb\" {$" + b + ":\"}",
		"example.com {\n\trespond {$" + a + ":hello world} {$" + b + "}\n\theader {$" + a + ":X-A} {$" + b + ":B}\n}\n",
		"{\n\temail {$" + a + ":a@example.com}\n}\n",
	} {
		blocks, err := caddyfile.Parse("Caddyfile", []byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		var want []string
		for _, sb := range blocks {
			want = append(want, sb.GetKeysText()...)
			for _, seg := range sb.Segments {
				for _, tok := range seg {
					want = append(want, tok.Text)
				}
			}
		}
		toks, err := caddyfile.Tokenize(expandEmptyEnv([]byte(in)), "Caddyfile")
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		var got []string
		for _, tok := range toks {
			got = append(got, tok.Text)
		}
		if strings.Join(strip(want), "\x00") != strings.Join(strip(got), "\x00") {
			t.Fatalf("%q:\nParse:    %q\nexpanded: %q", in, want, got)
		}
		// And the expansion did something, where there was something.
		if strings.Contains(in, ":") {
			raw, _ := caddyfile.Tokenize([]byte(in), "Caddyfile")
			var rawTexts []string
			for _, tok := range raw {
				rawTexts = append(rawTexts, tok.Text)
			}
			if strings.Join(strip(rawTexts), "\x00") == strings.Join(strip(got), "\x00") {
				t.Fatalf("%q: nothing expanded", in)
			}
		}
	}
	// A placeholder's value may hold more placeholders, which are not
	// expanded again (Caddy's one-level chaining), and the input is
	// not mutated.
	in := []byte("{$" + a + ":{$" + b + ":x}}")
	if got := string(expandEmptyEnv(in)); got != "{$"+b+":x}" || string(in) != "{$"+a+":{$"+b+":x}}" {
		t.Fatal(got, string(in))
	}
}

func strip(texts []string) []string {
	var out []string
	for _, s := range texts {
		if s != "{" && s != "}" {
			out = append(out, s)
		}
	}
	return out
}

// unsetForTest fails if a name the test relies on being unset is set.
func unsetForTest(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, set := os.LookupEnv(n); set {
			t.Fatalf("%s is set in the environment", n)
		}
	}
}

func TestBareHost(t *testing.T) {
	for in, want := range map[string]string{
		"deploy.example.com":    "deploy.example.com",
		"Deploy.Example.COM":    "deploy.example.com",
		"localhost":             "localhost",
		"192.0.2.1":             "192.0.2.1",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"a-b.c-d":               "a-b.c-d",
	} {
		got, ok := BareHost([]string{in})
		if !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", ".", "a.", ".a", "a..b", "-a", "a-", "a_b", "a b", "a:1", "a/b", "*.a", "{$A}", "https://a", "é.com", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "com"} {
		if _, ok := BareHost([]string{bad}); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := BareHost([]string{"a.com", "b.com"}); ok {
		t.Error("two accepted")
	}
	if _, ok := BareHost(nil); ok {
		t.Error("none accepted")
	}
}

func FuzzWalk(f *testing.F) {
	f.Add(file(good))
	f.Add(file(strings.Replace(good, "\tbox_webhook\n", "\trespond {$X:\"} \"a\nimport evil\" {$Y:\"}\n\tbox_webhook\n", 1)))
	f.Add([]byte("import x\n"))
	f.Add([]byte("{\n\tbox {\n\t\tsigner a b c\n\t}\n}\n"))
	f.Add([]byte("a.com,\nb.com {\n\tbox_webhook\n}\n"))
	f.Add([]byte("respond <<EOF\nx\nEOF\n"))
	f.Add(file(strings.Replace(good, "example.com {\n", "example.com {\n\trespond \"{\"\n", 1)))
	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := Walk(in)
		if err != nil {
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("not a refusal: %v", err)
			}
			return
		}
		if _, ok := BareHost([]string{s.Host}); !ok || s.Host != strings.ToLower(s.Host) || len(s.Signers) == 0 {
			t.Fatalf("%+v", s)
		}
		if _, err := s.Signers.AllowedSigners(); err != nil {
			t.Fatal(err)
		}
	})
}

// The monitor block's secrets are the {secret.<NAME>} arguments of its
// channel and heartbeat lines, in file order; anything else is notify's
// to refuse.
func TestWalkNotify(t *testing.T) {
	f := strings.Replace(good, "\tliveswap {", "\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL} severity warning\n\t\tchannel raw json https://hooks.example.com/x\n\t\tchannel odd json {secret.OPS_URL}\n\t\toperator ops\n\t\theartbeat {secret.NOTIFY_HC_URL}\n\t\tsecrets_file /etc/motel/secrets/_box.env\n\t}\n\n\tliveswap {", 1)
	s, err := Walk(file(f))
	if err != nil {
		t.Fatal(err)
	}
	if s.Notify == nil || !s.Notify.Channels || s.Notify.SecretsFile != "/etc/motel/secrets/_box.env" || !slices.Equal(s.Notify.Secrets, []string{"NOTIFY_OPS_URL", "NOTIFY_HC_URL"}) {
		t.Fatalf("%+v", s.Notify)
	}
	// A metrics line alone is a monitor block with no channel.
	f = strings.Replace(good, "\tliveswap {", "\tmonitor {\n\t\tmetrics\n\t}\n\n\tliveswap {", 1)
	if s, err := Walk(file(f)); err != nil || s.Notify == nil || s.Notify.Channels || len(s.Notify.Secrets) != 0 {
		t.Fatalf("a metrics line alone: %+v, %v", s.Notify, err)
	}
	// The old blocks are refused by name, pointing at the upgrade's
	// order, before the slot's check could refuse them as no block.
	for old, want := range map[string]string{
		"\tnotify {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n": "the Caddyfile has a notify block, which is now `monitor { … }`, with the same lines but state_dir and alertmanager, whose paths are fixed; to move a box onto it, docs/upgrading.md",
		"\tmetrics_store\n": "the Caddyfile has a metrics_store block, which is now a `metrics [<socket path>]` line in the monitor block",
		"\tmetrics_store {\n\t\tstore /run/x/vm.sock\n\t}\n": "has a metrics_store block",
	} {
		f = strings.Replace(good, "\tliveswap {", old+"\n\tliveswap {", 1)
		if _, err := Walk(file(f)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", old, err, want)
		}
	}
	if s, err := Walk(file(good)); err != nil || s.Notify != nil {
		t.Fatalf("no monitor block: %+v, %v", s, err)
	}
}
