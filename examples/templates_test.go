package examples

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// .github/scripts/publish-template.sh, release.yml's templates job,
// against local repositories: git's insteadOf turns the script's
// https://github.com/ into file://, so nothing leaves the machine and gh
// is never asked for a credential. Each release below is a commit and a
// tag in a stand-in motel; the mirror is a bare repository.
func TestPublishTemplate(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("this test needs git on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", ".github", "scripts", "publish-template.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// sh opens the script, not this process: the stat is what puts it in
	// Go's test cache, so an edit to it re-runs this test.
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.file://"+root+"/.insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/",
		"GIT_CONFIG_KEY_1=init.defaultBranch", "GIT_CONFIG_VALUE_1=main",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	src := filepath.Join(root, "src")
	mirror := filepath.Join(root, "org", "tpl.git")
	hand := filepath.Join(root, "hand")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(tag string) (string, error) {
		cmd := exec.Command("sh", script, "node", tag, "org/tpl")
		cmd.Dir, cmd.Env = src, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	release := func(tag string) { // a commit that changes the example, tagged
		t.Helper()
		write(filepath.Join(src, "examples", "node", "README.md"), "# example at "+tag+"\n", 0o644)
		run(src, "git", "add", "-A")
		run(src, "git", "commit", "-q", "-m", tag)
		run(src, "git", "tag", tag)
	}
	mirrorMain := func() string { return run(root, "git", "--git-dir", mirror, "rev-parse", "main") }
	holds := func(tag string) {
		t.Helper()
		if got, want := run(root, "git", "--git-dir", mirror, "ls-tree", "-r", "main"), run(src, "git", "ls-tree", "-r", tag+":examples/node"); got != want {
			t.Fatalf("mirror main is not examples/node at %s:\n%s\nwant:\n%s", tag, got, want)
		}
		if got, want := run(root, "git", "--git-dir", mirror, "rev-parse", tag+"^{commit}"), mirrorMain(); got != want {
			t.Fatalf("mirror tag %s is %s, main is %s", tag, got, want)
		}
	}
	handEdit := func(msg string) string { // someone pushes to the mirror's main by hand
		t.Helper()
		if _, err := os.Stat(hand); err != nil {
			run(root, "git", "clone", "-q", mirror, hand)
		}
		run(hand, "git", "pull", "-q", "--ff-only")
		write(filepath.Join(hand, "HAND.md"), msg+"\n", 0o644)
		run(hand, "git", "add", "-A")
		run(hand, "git", "commit", "-q", "-m", msg)
		run(hand, "git", "push", "-q", "origin", "main")
		return mirrorMain()
	}

	run(root, "git", "init", "-q", "--bare", mirror)
	run(root, "git", "init", "-q", src)
	// An executable, and a file the example's own .gitignore names: both
	// ship as they are.
	write(filepath.Join(src, "examples", "node", "scripts", "run.sh"), "#!/bin/sh\n", 0o755)
	write(filepath.Join(src, "examples", "node", ".gitignore"), "data/\n", 0o644)
	write(filepath.Join(src, "examples", "node", "data", "seed.txt"), "seed\n", 0o644)
	write(filepath.Join(src, "other.txt"), "not the example\n", 0o644)
	run(src, "git", "add", "-A", "-f")
	release("v1.0.0")

	if out, err := publish("v1.0.0"); err != nil {
		t.Fatalf("first release into an empty mirror: %v\n%s", err, out)
	}
	holds("v1.0.0")
	if mode := run(root, "git", "--git-dir", mirror, "ls-tree", "main", "scripts/run.sh"); !strings.HasPrefix(mode, "100755") {
		t.Fatalf("scripts/run.sh lost its executable bit: %s", mode)
	}

	before := mirrorMain()
	if out, err := publish("v1.0.0"); err != nil || mirrorMain() != before {
		t.Fatalf("a re-run must push nothing: %v\n%s", err, out)
	}

	run(src, "git", "tag", "v1.1.0-rc1")
	if out, err := publish("v1.1.0-rc1"); err != nil || mirrorMain() != before || !strings.Contains(out, "prerelease") {
		t.Fatalf("a prerelease must leave the mirror alone: %v\n%s", err, out)
	}

	// A newer tag whose release never went out holds nothing back.
	run(src, "git", "tag", "v2.0.0")
	release("v1.0.1")
	if out, err := publish("v1.0.1"); err != nil {
		t.Fatalf("v1.0.1 with an unpublished v2.0.0 tag: %v\n%s", err, out)
	}
	holds("v1.0.1")

	// A hand edit is overwritten by the next release, on top of it.
	edited := handEdit("hand edit")
	release("v1.1.0")
	if out, err := publish("v1.1.0"); err != nil {
		t.Fatalf("v1.1.0 after a hand edit: %v\n%s", err, out)
	}
	holds("v1.1.0")
	if parent := run(root, "git", "--git-dir", mirror, "rev-parse", "main~1"); parent != edited {
		t.Fatalf("v1.1.0 is not a fast-forward from the hand edit: parent %s, want %s", parent, edited)
	}

	// A fix to an older line, or an older run arriving late, is skipped.
	before = mirrorMain()
	release("v1.0.2")
	if out, err := publish("v1.0.2"); err != nil || mirrorMain() != before || !strings.Contains(out, "already has v1.1.0") {
		t.Fatalf("v1.0.2 after v1.1.0 must leave the mirror alone: %v\n%s", err, out)
	}

	// The same tag again after a hand edit would need the tag moved:
	// refused, and --atomic leaves main where the hand edit put it.
	edited = handEdit("another hand edit")
	if out, err := publish("v1.1.0"); err == nil || mirrorMain() != edited {
		t.Fatalf("moving a published tag must be refused with main untouched: %v\n%s", err, out)
	}
}

// A markdown link's or image's target — `](target` — and a reference
// definition's — `[name]: target`.
var (
	inlineLink = regexp.MustCompile(`\]\(([^)\s]+)`)
	refLink    = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*(\S+)`)
)

// Each release publishes examples/node, examples/deno and examples/box
// as template repositories of their own (release.yml's templates job), and every
// repository made from one is its own root too: a relative link that
// leaves the example's directory works here and is a 404 there. So a
// link in an example's docs is absolute, or stays inside the example
// and names something that exists.
func TestTemplateLinksStayInside(t *testing.T) {
	t.Parallel()
	for _, ex := range []string{"node", "deno", "box"} {
		err := filepath.WalkDir(ex, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var targets []string
			for _, re := range []*regexp.Regexp{inlineLink, refLink} {
				for _, m := range re.FindAllStringSubmatch(string(b), -1) {
					targets = append(targets, m[1])
				}
			}
			for _, target := range targets {
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
					continue
				}
				file, _, _ := strings.Cut(target, "#")
				resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(file))
				rel, err := filepath.Rel(ex, resolved)
				// A leading / is the repository's root, motel's here
				// and the example's own in the template: it cannot name
				// the same file in both.
				if strings.HasPrefix(file, "/") || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Errorf("%s links to %s, outside examples/%s: in its template repository that is a 404; link to https://github.com/smallhoursorg/motel/… instead", path, target, ex)
					continue
				}
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s links to %s, which does not exist", path, target)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
