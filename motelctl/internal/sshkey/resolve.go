package sshkey

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/smallhoursorg/motel/motelctl/internal/local"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// Key is the key motelctl signs with, and why it is that one.
type Key struct {
	Public PublicKey
	// Path is what ssh-keygen -f is given: the private key, or its
	// .pub when the agent holds the private half. Empty for a key the
	// agent alone holds, which is handed to ssh-keygen on a pipe.
	Path string
	// Shown names the key to the person: the path with the home
	// directory as ~, or "the agent's key".
	Shown string
	// Source is how it was chosen, in the words `motelctl key` says.
	Source string
}

// The sources, in the order Resolve tries them.
const (
	SourceFlag    = "--key"
	SourceEnv     = "MOTEL_KEY"
	SourceGit     = "git's user.signingkey"
	SourceDefault = "the default file"
	SourceAgent   = "the agent's only key"
)

// defaultFiles are ssh's own default keys a box can take, in ssh's
// order. id_rsa is not among them: no box takes it.
var defaultFiles = []string{"id_ed25519", "id_ecdsa", "id_ed25519_sk", "id_ecdsa_sk"}

// ErrNoKey is Resolve's answer when nothing names a key.
var ErrNoKey = errors.New("name your key: --key ~/.ssh/<key>.pub or MOTEL_KEY")

// Lookup is what Resolve reads.
type Lookup struct {
	// Flag is --key's value.
	Flag   string
	Getenv func(string) string
	// Home is the home directory, for ~ and the default files.
	Home string
	// Dir is the working directory, for git's config.
	Dir string
	Run local.Runner
}

// Resolve finds the one key a command signs with: --key, then
// MOTEL_KEY, then git's user.signingkey inside a box repository when
// git signs with ssh, then ssh's default files, then the agent's key
// when it holds exactly one. It never tries several keys: each refused
// key would be a 401 against the box's budget for logging them, and a
// touch of a hardware key. An RSA key, wherever it came from, is
// refused here (*RSAError).
func Resolve(ctx context.Context, l Lookup) (Key, error) {
	if l.Flag != "" {
		return l.load(l.Flag, SourceFlag)
	}
	if v := l.Getenv("MOTEL_KEY"); v != "" {
		return l.load(v, SourceEnv)
	}
	if _, ok := local.BoxRepo(ctx, l.Run, l.Dir); ok &&
		local.GitConfig(ctx, l.Run, l.Dir, "gpg.format") == "ssh" {
		if v := local.GitConfig(ctx, l.Run, l.Dir, "user.signingkey"); v != "" {
			// git also takes the key itself, as key::<line>: then the
			// agent holds it, as for a key with no file.
			if line, ok := strings.CutPrefix(v, "key::"); ok {
				return fromLine(line, "", "git's user.signingkey key", SourceGit)
			}
			return l.load(v, SourceGit)
		}
	}
	for _, name := range defaultFiles {
		path := filepath.Join(l.Home, ".ssh", name)
		if _, err := os.Stat(path + ".pub"); err == nil {
			return l.load(path, SourceDefault)
		}
	}
	out, err := l.Run.Output(ctx, local.Cmd{Name: "ssh-add", Args: []string{"-L"}})
	if err == nil {
		var lines []string
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 1 {
			return fromLine(lines[0], "", "the agent's key", SourceAgent)
		}
	}
	return Key{}, ErrNoKey
}

// load reads the key at path: its .pub, beside it or itself.
func (l Lookup) load(path, source string) (Key, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		path = filepath.Join(l.Home, rest)
	}
	shown := l.shown(path)
	pub := path
	if !strings.HasSuffix(path, ".pub") {
		pub = path + ".pub"
		if _, err := os.Stat(pub); errors.Is(err, fs.ErrNotExist) {
			// A key file named with no .pub beside it may itself be
			// the public line.
			pub = path
		}
	}
	b, err := os.ReadFile(pub) //nolint:gosec // G304: the key the person named
	if err != nil {
		return Key{}, fmt.Errorf("%s (%s): %w", shown, source, err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	if pub == path && !strings.HasSuffix(path, ".pub") && !looksPublic(line) {
		return Key{}, fmt.Errorf("%s (%s): no public key beside it: name its .pub", shown, source)
	}
	return fromLine(line, path, l.shown(pub), source)
}

func fromLine(line, path, shown, source string) (Key, error) {
	k, err := ParsePublic(line)
	if err != nil {
		return Key{}, fmt.Errorf("%s (%s): %w", shown, source, err)
	}
	if k.RSA() {
		return Key{}, &RSAError{Shown: shown}
	}
	return Key{Public: k, Path: path, Shown: shown, Source: source}, nil
}

// shown is path as the person would write it, made safe to print: a
// path from the environment or git's config may hold anything.
func (l Lookup) shown(path string) string {
	if l.Home != "" {
		if rest, ok := strings.CutPrefix(path, l.Home+string(filepath.Separator)); ok {
			path = "~/" + rest
		}
	}
	return termtext.From(path).String()
}

// looksPublic says whether a file's first line could be a public key
// rather than a private one.
func looksPublic(line string) bool {
	return !strings.HasPrefix(line, "-----BEGIN") && len(strings.Fields(line)) >= 2
}
