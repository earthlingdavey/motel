package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap"
)

// stubHost is the box_webhook host the stub tests validate for.
const stubHost = "deploy.example.com"

// reachableAdaptation is what the stub `motel adapt` prints unless a
// test says otherwise: one server, the box site routing / to
// box_webhook.
const reachableAdaptation = `{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`

// stubValidator is the real child runner over stub programs: motel
// is the body of a /bin/sh script standing for `motel validate`,
// backup, unless "", for `motel-backup`; `motel adapt` prints
// reachableAdaptation. Each script records its arguments, one line per
// run, and the staged file beside itself.
func stubValidator(t *testing.T, motel, backup string, env ...string) (*childValidator, string) {
	t.Helper()
	return stubChildren(t, motel, "printf '%s' '"+reachableAdaptation+"'; exit 0", backup, env...)
}

// stubAdapt is stubValidator with `motel validate` passing and adapt
// the body of the script standing for `motel adapt`.
func stubAdapt(t *testing.T, adapt string, env ...string) (*childValidator, string) {
	t.Helper()
	return stubChildren(t, "exit 0", adapt, "", env...)
}

func stubChildren(t *testing.T, validate, adapt, backup string, env ...string) (*childValidator, string) {
	t.Helper()
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	must(t, os.Mkdir(stage, 0o700))
	// One program answers both subcommands, as the real one does.
	motel := "case \"$1\" in adapt)\n" + adapt + "\n;; esac\n" + validate
	// The box's own deadline: a child a test means to finish must not
	// race the machine's load (#289). Tests of the deadline set their
	// own.
	c := &childValidator{
		stage: stage, motel: script(t, dir, "motel", motel), backup: filepath.Join(dir, "absent"),
		env: env, timeout: validateTimeout, redactor: liveswap.NewRedactor(secretCandidates(env), nil),
	}
	if backup != "" {
		c.backup = script(t, dir, "motel-backup", backup)
	}
	return c, dir
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	rec := filepath.Join(dir, name+".args")
	// The last argument is the staged file: copy what it held while
	// the child ran.
	text := "#!/bin/sh\necho \"$@\" >> " + rec + "\nfor a; do last=$a; done\ncat \"$last\" > " + rec + ".file 2>/dev/null\n" + body + "\n"
	must(t, os.WriteFile(path, []byte(text), 0o755))
	return path
}

func TestValidateChild(t *testing.T) {
	file := []byte("example.com {\n\trespond hi\n}\n")
	t.Run("passes", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "")
		msg, err := c.validate(context.Background(), "0123456789abcdef0123456789abcdef", file, stubHost, nil, nil)
		if msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "motel.args"))
		staged := filepath.Join(c.stage, stagedConfig("0123456789abcdef0123456789abcdef"))
		if strings.TrimSpace(string(args)) != "validate --adapter caddyfile --config "+staged+"\nadapt --adapter caddyfile --config "+staged {
			t.Fatalf("args %q", args)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "motel.args.file")); string(got) != string(file) {
			t.Fatalf("the child read %q", got)
		}
		if exists(staged) {
			t.Fatal("the staged copy was left behind")
		}
	})
	t.Run("names the push's box secrets to validate alone, over the service's own", func(t *testing.T) {
		// The stub records what validate was given (adapt exits before).
		c, dir := stubValidator(t, "echo \"v=$"+NotifyPushedSecretsEnv+"\" >> \"$(dirname \"$0\")/env\"; exit 0", "", NotifyPushedSecretsEnv+"=NOTIFY_FROM_THE_SERVICE")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, []string{"NOTIFY_OPS_URL", "NOTIFY_HC_URL"}); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "env"))
		if string(got) != "v=NOTIFY_OPS_URL,NOTIFY_HC_URL\nv=\n" {
			t.Fatalf("validate was given %q", got)
		}
		if strings.Contains(strings.Join(c.env, " "), "NOTIFY_OPS_URL") {
			t.Fatal("the names stayed in the validator's environment")
		}
	})
	t.Run("refuses with the error line", func(t *testing.T) {
		c, _ := stubValidator(t, `echo '{"level":"info","msg":"using config"}' >&2; echo 'Error: adapting config using caddyfile: Caddyfile:2: unrecognized directive: respnd' >&2; exit 1`, "")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || msg != "motel validate: adapting config using caddyfile: Caddyfile:2: unrecognized directive: respnd" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("refuses with the reason in Caddy's JSON error line", func(t *testing.T) {
		for name, line := range map[string]string{
			"provision": caddyErrorLine,
			"validate":  caddyLine(caddyChain + "loading module 'liveswap_webhook': http.handlers.liveswap_webhook: invalid configuration: " + caddyReason),
		} {
			out := filepath.Join(t.TempDir(), "out")
			must(t, os.WriteFile(out, []byte(`{"level":"info","msg":"using config from file"}`+"\n"+line+"\n"), 0o600))
			c, _ := stubValidator(t, "cat '"+out+"' >&2; exit 1", "")
			msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
			if err != nil || !strings.HasPrefix(msg, "motel validate: ...") || !strings.HasSuffix(msg, caddyReason) || len(msg) > len("motel validate: ...")+300 {
				t.Fatalf("%s: msg %q err %v", name, msg, err)
			}
		}
	})
	t.Run("keeps the head of an adapter error", func(t *testing.T) {
		// The adapter says why first and where after; an import chain of
		// staged paths can outrun the bound.
		staged := "/var/lib/motel-box/stage/." + strings.Repeat("a", 32) + ".Caddyfile"
		why := "adapting config using caddyfile: parsing caddyfile tokens for 'reverse_proxy': wrong argument count or unexpected line ending after 'to'"
		line := caddyLine(why + ", at " + staged + ":12 import chain ['" + staged + ":3 (import a)','" + staged + ":7 (import b)','" + staged + ":9 (import c)']")
		out := filepath.Join(t.TempDir(), "out")
		must(t, os.WriteFile(out, []byte(line+"\n"), 0o600))
		c, _ := stubValidator(t, "cat '"+out+"' >&2; exit 1", "")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || !strings.HasPrefix(msg, "motel validate: "+why) || !strings.HasSuffix(msg, "...") {
			t.Fatalf("msg %q err %v", msg, err)
		}
		// The end is chosen from the line as the child wrote it: a known
		// value redacted out of the prefix does not move the cut.
		c, _ = stubValidator(t, "cat '"+out+"' >&2; exit 1", "", "DB_PASSWORD=adapting")
		msg, err = c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || strings.Contains(msg, "adapting") || !strings.Contains(msg, "wrong argument count") {
			t.Fatalf("with a known value: msg %q err %v", msg, err)
		}
	})
	t.Run("redacts the environment's values in Caddy's JSON error line", func(t *testing.T) {
		secret := "pg-pass-" + strings.Repeat("q7", 10)
		out := filepath.Join(t.TempDir(), "out")
		must(t, os.WriteFile(out, []byte(caddyLine("provision liveswap: app x: listen "+secret+" is not a number")+"\n"), 0o600))
		c, _ := stubValidator(t, "cat '"+out+"'; exit 1", "", "DB_PASS="+secret)
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || strings.Contains(msg, secret) || !strings.HasPrefix(msg, "motel validate: provision liveswap: app x: listen ") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("redacts the environment's values", func(t *testing.T) {
		secret := "pg-pass-" + strings.Repeat("q7", 10)
		// Quoted bare, as an expanded placeholder is: only the layer
		// primed with the environment can know it.
		// The service's own words stay: USER is motel, and the staged
		// file's path names it.
		c, _ := stubValidator(t, `echo "Error: /var/lib/motel-box/stage/x.Caddyfile:2: parsing port: $DB_PASS is not a number"; exit 1`, "",
			"DB_PASS="+secret, "HOME=/var/lib/motel", "USER=motel", "LOGNAME=motel")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || strings.Contains(msg, secret) || !strings.HasPrefix(msg, "motel validate: /var/lib/motel-box/stage/x.Caddyfile:2: parsing port: ") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("bounds the line", func(t *testing.T) {
		c, _ := stubValidator(t, `printf 'Error: '; head -c 2000 /dev/zero | tr '\0' 'x'; echo; exit 1`, "")
		msg, _ := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if len(msg) > len("motel validate: ")+300+len("...") { // proof.Bound's cap
			t.Fatalf("%d bytes", len(msg))
		}
	})
	t.Run("keeps the tail of a long output", func(t *testing.T) {
		c, _ := stubValidator(t, `head -c 100000 /dev/zero | tr '\0' 'y'; echo; echo 'Error: the end'; exit 1`, "")
		msg, _ := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if msg != "motel validate: the end" {
			t.Fatalf("msg %q", msg)
		}
	})
	t.Run("no output", func(t *testing.T) {
		c, _ := stubValidator(t, "exit 3", "")
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || msg != "motel validate: exit status 3" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("an exit status decides over the deadline", func(t *testing.T) {
		// The child says why and exits; a grandchild holds the output past
		// the deadline. The answer is the child's line, not a timeout.
		c, _ := stubValidator(t, "sleep 60 & echo 'Error: said why'; exit 1", "")
		c.timeout = 800 * time.Millisecond // the shell's exit well inside it, WaitDelay (1 s) past it
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "motel validate: said why" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a child that does not finish", func(t *testing.T) {
		c, _ := stubValidator(t, "exec sleep 60", "")
		c.timeout = 200 * time.Millisecond
		start := time.Now()
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || msg != "motel validate: did not finish within 200ms" || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("a grandchild holding the output", func(t *testing.T) {
		// The child exits; a grandchild keeps the pipe. WaitDelay ends
		// the wait instead of the grandchild's sleep.
		c, _ := stubValidator(t, "sleep 60 & echo 'Error: gone'; exit 1", "")
		start := time.Now()
		msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if err != nil || msg != "motel validate: gone" || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("a grandchild holding the output after a pass", func(t *testing.T) {
		c, _ := stubValidator(t, "sleep 60 & exit 0", "")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a child a signal killed is the box's", func(t *testing.T) {
		c, _ := stubValidator(t, "kill -9 $$", "")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err == nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a program that cannot start is the box's", func(t *testing.T) {
		c, _ := stubValidator(t, "exit 0", "")
		c.motel = filepath.Join(t.TempDir(), "missing")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err == nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("the caller gone is not a verdict", func(t *testing.T) {
		c, _ := stubValidator(t, "exec sleep 60", "")
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		if msg, err := c.validate(ctx, randomID(t), file, stubHost, nil, nil); msg != "" || err == nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
}

func TestValidateBackup(t *testing.T) {
	file := []byte("example.com\n")
	t.Run("installed and refusing", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "echo 'Error: backup app is not declared' >&2; exit 2")
		id := randomID(t)
		msg, err := c.validate(context.Background(), id, file, stubHost, nil, nil)
		if err != nil || msg != "motel-backup validate: backup app is not declared" {
			t.Fatalf("msg %q err %v", msg, err)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "motel-backup.args"))
		if strings.TrimSpace(string(args)) != "validate "+filepath.Join(c.stage, stagedConfig(id)) {
			t.Fatalf("args %q", args)
		}
	})
	t.Run("keeps the head of its line", func(t *testing.T) {
		// Its errors quote the adapter, which says why first.
		c, _ := stubValidator(t, "exit 0", `printf 'Error: the why, at '; head -c 2000 /dev/zero | tr '\0' 'x'; echo; exit 1`)
		msg, _ := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil)
		if !strings.HasPrefix(msg, "motel-backup validate: the why, at x") || !strings.HasSuffix(msg, "...") {
			t.Fatalf("msg %q", msg)
		}
	})
	t.Run("installed and passing", func(t *testing.T) {
		c, _ := stubValidator(t, "exit 0", "exit 0")
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("not after motel refused", func(t *testing.T) {
		c, dir := stubValidator(t, "echo 'Error: no'; exit 1", "exit 0")
		if msg, _ := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "motel validate: no" {
			t.Fatalf("msg %q", msg)
		}
		if exists(filepath.Join(dir, "motel-backup.args")) {
			t.Fatal("backup validate ran after motel's refusal")
		}
	})
	t.Run("not installed, as test -x reads it", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "")
		c.backup = filepath.Join(dir, "not-executable")
		must(t, os.WriteFile(c.backup, []byte("#!/bin/sh\nexit 1\n"), 0o644))
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err != nil {
			t.Fatalf("EACCES: msg %q err %v", msg, err)
		}
		c.backup = filepath.Join(dir, "motel.args", "x") // a component that is not a directory
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err != nil {
			t.Fatalf("ENOTDIR: msg %q err %v", msg, err)
		}
	})
	t.Run("could not tell is the box's", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "")
		c.backup = filepath.Join(dir, "loop")
		must(t, os.Symlink(c.backup, c.backup))
		if msg, err := c.validate(context.Background(), randomID(t), file, stubHost, nil, nil); msg != "" || err == nil || !strings.HasPrefix(err.Error(), "could not ask whether backups are installed: ") {
			t.Fatalf("ELOOP: msg %q err %v", msg, err)
		}
	})
}

// caddyChain, caddyReason and caddyErrorLine are how Caddy (v2.11.6
// on, #163) reports a validate that failed when its output is not a
// terminal: one JSON log line from its root logger, the module chain
// first and the reason last. The chain is the head recorded on #324
// (box apply 3, a backup path into a sibling's shared/), cut there at
// 300 bytes; the reason is the one that run had, as liveswap words it.
const (
	caddyChain  = "loading http app module: provision http: server srv1: setting up route handlers: route 0: loading handler modules: position 0: "
	caddyReason = `app deno-example: backup files "../../deno-example/shared": the path reaches outside the app's shared dir`
	caddyMsg    = caddyChain + "loading module 'liveswap_webhook': provision http.handlers.liveswap_webhook: loading liveswap app module: provision liveswap: " + caddyReason
)

var caddyErrorLine = caddyLine(caddyMsg)

// caddyLine is msg as Caddy logs a command's error; the texts here
// need only `"` escaped.
func caddyLine(msg string) string {
	return `{"level":"error","ts":1792333313.1590059,"msg":"` + strings.ReplaceAll(msg, `"`, `\"`) + `"}`
}

func TestErrorLine(t *testing.T) {
	for out, want := range map[string]string{
		"":                            "",
		"\n\n":                        "",
		"one\ntwo\n":                  "two",
		"Error: first\nlater noise\n": "first",
		"Error: a\nError: b\n\n":      "b",
		"  indented last  \n":         "indented last",
		"x Error: not at the start\n": "x Error: not at the start",
		caddyErrorLine + "\n":         caddyMsg,
		// The last line that is an error, over noise after it.
		caddyErrorLine + "\n" + `{"level":"info","msg":"shutting down"}` + "\n":                                  caddyMsg,
		`{"level":"error","msg":"adapting config using caddyfile: Caddyfile:2: unrecognized directive: respnd"}`: "adapting config using caddyfile: Caddyfile:2: unrecognized directive: respnd",
		// A module's own error line is not the command's error.
		"Error: the reason\n" + `{"level":"error","logger":"tls","msg":"could not clean up","error":"x"}` + "\n": "the reason",
		// Not an error line, or not one whole: the last line as it stands.
		`{"level":"info","msg":"not an error"}` + "\n": `{"level":"info","msg":"not an error"}`,
		`{"level":"error","msg":"tail cut"` + "\n":     `{"level":"error","msg":"tail cut"`,
		`{"level":"error","msg":7}` + "\n":             `{"level":"error","msg":7}`,
		// The last line that is an error, in either form.
		"Error: earlier\n" + `{"level":"error","msg":"last"}` + "\n": "last",
		`{"level":"error","msg":"earlier"}` + "\nError: last\n":      "last",
	} {
		if got := errorLine([]byte(out)); got != want {
			t.Errorf("errorLine(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	big := strings.Repeat("a", maxValidateOutput) + "END"
	for _, chunk := range []string{"start ", big[:100], big[100:], "\nlast"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatal(n, err)
		}
	}
	if len(b.b) != maxValidateOutput || !strings.HasSuffix(string(b.b), "END\nlast") {
		t.Fatalf("%d bytes, ends %q", len(b.b), string(b.b[len(b.b)-10:]))
	}
	b = tailBuffer{}
	_, _ = b.Write([]byte(big))
	if len(b.b) != maxValidateOutput || !strings.HasSuffix(string(b.b), "END") {
		t.Fatal("one large write")
	}
}

func TestSecretCandidates(t *testing.T) {
	got := secretCandidates([]string{"PATH=/usr/bin:/bin", "HOME=/var/lib/motel", "USER=motel", "LOGNAME=motel",
		"INVOCATION_ID=0123456789abcdef", "LC_ALL=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/999", "TOKEN=abc", "SLASHED=/Zq3+secret", "EMPTY=", "NOEQUALS"})
	if strings.Join(got, ",") != "TOKEN=abc,SLASHED=/Zq3+secret,EMPTY=" {
		t.Fatalf("%v", got)
	}
}

// TestValidateTakesTheProcessGroup: whatever the child started goes
// with it — after a deadline, and after a child that exited and left a
// grandchild holding the output.
func TestValidateTakesTheProcessGroup(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/stat"); err != nil {
		t.Skip("needs /proc to see the grandchild: the dev container has it")
	}
	for name, tail := range map[string]string{"deadline": "exec sleep 60", "exited": "exit 1"} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			c, _ := stubValidator(t, "sleep 60 & echo $! > "+pidFile+"\n"+tail, "")
			c.timeout = time.Second
			if _, err := c.validate(context.Background(), randomID(t), []byte("x"), stubHost, nil, nil); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid := strings.TrimSpace(string(b))
			// Gone, or a zombie waiting for its new parent to reap it.
			deadline := time.Now().Add(5 * time.Second)
			for {
				stat, err := os.ReadFile("/proc/" + pid + "/stat")
				if err != nil || strings.Contains(string(stat), ") Z ") {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("grandchild %s still running: %s", pid, stat)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
