package client

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/smallhoursorg/motel/client/internal/local"
	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Box is the box a command asks: its name, which is its identity (the
// box_webhook host, the aud of a person's token), and the port when an
// app URL named one.
type Box struct {
	Host string
	Port string // empty is 443
}

// Addr is the box as a URL's host: the name, and the port when it is
// not 443.
func (b Box) Addr() string {
	if b.Port == "" {
		return b.Host
	}
	return net.JoinHostPort(b.Host, b.Port)
}

// PortOr443 is the port the box is asked on.
func (b Box) PortOr443() string {
	if b.Port == "" {
		return "443"
	}
	return b.Port
}

// appRe and versionRe are the box's own alphabets (liveswap/names.go):
// a name or version outside them could only be refused, or worse, be
// read as another (`#` would cut the query, `%31` arrive as `1`).
var (
	appRe     = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
)

// App is an app named on the command line: its name, the path its
// webhook answers on, and the box when an app URL named one.
type App struct {
	Name string
	Path string
	Box  *Box
}

// ParseApp reads <app>: a bare name, or an app's URL as MOTEL_URL
// holds it (https://deploy.example.com/blog), which names the box too.
// The URL rule is deploy.sh's: https only, and the app is the last
// path segment once trailing slashes are dropped; the request goes to
// the URL's own path, as deploy.sh's does.
func ParseApp(arg string) (App, error) {
	if !strings.Contains(arg, "/") {
		if !appRe.MatchString(arg) {
			return App{}, usagef("%q is not an app name (a-z, 0-9 and -, at most 63) or an app's https:// URL", arg)
		}
		return App{Name: arg, Path: "/" + arg}, nil
	}
	box, u, err := parseHTTPS(arg, "the app's URL")
	if err != nil {
		return App{}, err
	}
	p := strings.TrimRight(u.Path, "/")
	name := p[strings.LastIndexByte(p, '/')+1:]
	if !appRe.MatchString(name) {
		return App{}, usagef("%q does not end in an app name (a-z, 0-9 and -, at most 63)", arg)
	}
	return App{Name: name, Path: p, Box: &box}, nil
}

// CheckVersion holds a version to the box's alphabet before it is
// sent; its length is left to the box, whose 422 says so.
func CheckVersion(v string) error {
	if !versionRe.MatchString(v) {
		return usagef("%q is not a version (letters, digits, . _ -; not starting with .)", v)
	}
	return nil
}

// BoxLookup is what ResolveBox reads.
type BoxLookup struct {
	// FromApp is the box an app URL named, if one did.
	FromApp *Box
	// Flag is --box's value.
	Flag   string
	Getenv func(string) string
	Dir    string
	Run    local.Runner
}

// ResolveBox finds the box a command asks: an app URL as <app>, then
// --box, then MOTEL_BOX, then the host of MOTEL_URL, then the box
// repository's directory: the box whose directory the shell is in, or
// the only box of the repository at its top. No file of named boxes
// exists: a box's name is short, and a file once written is one-way.
func ResolveBox(ctx context.Context, l BoxLookup) (Box, error) {
	if l.FromApp != nil {
		return *l.FromApp, nil
	}
	if l.Flag != "" {
		return bareBox(l.Flag, "--box")
	}
	if v := l.Getenv("MOTEL_BOX"); v != "" {
		return bareBox(v, "MOTEL_BOX")
	}
	if v := l.Getenv("MOTEL_URL"); v != "" {
		box, _, err := parseHTTPS(v, "MOTEL_URL")
		return box, err
	}
	if repo, ok := local.BoxRepo(ctx, l.Run, l.Dir); ok {
		switch {
		case repo.Here != "":
			return Box{Host: repo.Here}, nil
		case len(repo.Boxes) == 1 && repo.AtTop(l.Dir):
			return Box{Host: repo.Boxes[0]}, nil
		case len(repo.Boxes) > 1 && repo.AtTop(l.Dir):
			return Box{}, usagef("this repository has %d boxes (%s): name one with --box, or run inside its directory", len(repo.Boxes), strings.Join(repo.Boxes, ", "))
		}
	}
	return Box{}, usagef("name the box: --box deploy.example.com, MOTEL_BOX, or run inside the box repository")
}

// bareBox is a box named by --box or MOTEL_BOX: its name alone.
func bareBox(v, from string) (Box, error) {
	h := strings.ToLower(v)
	if !local.IsHostname(h) {
		return Box{}, usagef("%s %q is not a box's name: give its host name, deploy.example.com", from, v)
	}
	return Box{Host: h}, nil
}

// parseHTTPS reads an https URL's box. Only https: the request carries
// a token, which over http would cross the wire in the clear.
func parseHTTPS(raw, what string) (Box, *url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.Opaque != "" {
		return Box{}, nil, usagef("%s %q is not an https:// URL, e.g. https://deploy.example.com/blog", what, raw)
	}
	box := Box{Host: strings.ToLower(u.Hostname()), Port: u.Port()}
	if !local.IsHostname(box.Host) {
		return Box{}, nil, usagef("%s %q does not name a box by its host name", what, raw)
	}
	if box.Port != "" {
		if n, err := strconv.Atoi(box.Port); err != nil || n < 1 || n > 65535 {
			return Box{}, nil, usagef("%s %q has no usable port", what, raw)
		}
		if box.Port == "443" {
			box.Port = ""
		}
	}
	return box, u, nil
}

// usagef is a usage refusal: nothing was sent. What it quotes of the
// person's own arguments is made safe to print.
func usagef(format string, args ...any) *Failure {
	for i, a := range args {
		if s, ok := a.(string); ok {
			args[i] = termtext.From(s).String()
		}
	}
	return &Failure{Exit: ExitUsage, Message: fmt.Sprintf(format, args...)}
}
