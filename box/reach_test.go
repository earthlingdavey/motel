package box

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	// The `timeouts` directive, so the adapter here writes the handler
	// as moteld's binary (Caddy's standard modules) does: its keys
	// and the shape of each value are then the real adapter's to prove.
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/timeouts"
)

// reachFile assembles a Caddyfile around the design's example box site
// (DESIGN-box.md, "The shape"): the fixture's global block with `order`
// lines added, the example app site, then before, the box site with
// inside added to it, and after. Position inside a site decides
// nothing: Caddy orders directives by its list, so `inside` may name a
// matcher and use it wherever it stands.
func reachFile(order []string, before, inside, after string) string {
	global := good[:strings.Index(good, "\n}\n")+3]
	if len(order) > 0 {
		var b strings.Builder
		for _, o := range order {
			b.WriteString("\torder " + o + "\n")
		}
		global = strings.Replace(global, "{\n", "{\n"+b.String(), 1)
	}
	site := "deploy.example.com {\n" + inside + "\tliveswap_webhook\n\tbox_webhook\n}\n"
	return global + "\nexample.com {\n\treverse_proxy {\n\t\tdynamic liveswap example\n\t}\n}\n\n" + before + site + after
}

// reachOf adapts a Caddyfile and asks the question of its adaptation.
func reachOf(t *testing.T, in string) error {
	t.Helper()
	cfg, _, err := adapt(t, in)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, in)
	}
	return reach(cfg, "deploy.example.com", nil)
}

// wantReach checks a verdict: "" is reachable; otherwise the exact
// refusal text, as the catalogue has it.
func wantReach(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		return
	}
	var r *refusal
	if !errors.As(err, &r) {
		t.Fatalf("got %v, want the refusal %q", err, want)
	}
	if r.msg != msgUnreachable(want) {
		t.Fatalf("got  %q\nwant %q", r.msg, msgUnreachable(want))
	}
}

// TestReach is the Reachability tables (DESIGN-box.md) on adapted
// Caddyfiles: what the default order lets stand before box_webhook,
// what an `order` option or a second site for the host puts in front,
// and how each matcher is decided.
func TestReach(t *testing.T) {
	const (
		respondFirst = "static_response answers first"
		mayHeader    = "static_response may answer first (its header matcher is decided per request)"
		cutStall     = "timeouts's read_timeout cuts an upload that stalls under 10s first"
		cutRate      = "timeouts's read_min_rate cuts an upload under 64 KiB/s first"
	)
	for name, c := range map[string]struct {
		order                 []string
		before, inside, after string
		want                  string
	}{
		// Reachable.
		"the design's example":                  {},
		"the directives a box site might carry": {inside: "\trespond /x ok\n\thandle /h {\n\t\trespond h\n\t}\n\troute /r {\n\t\trespond r\n\t}\n\tredir /old /new\n\treverse_proxy /p localhost:9\n"},
		"a response header":                     {inside: "\theader X-Frame-Options DENY\n"},
		"another path on the host":              {before: "deploy.example.com/api/* {\n\trespond api\n}\n"},
		"a wildcard host":                       {after: "*.example.com {\n\trespond wild\n}\n"},
		"a longer host":                         {before: "xdeploy.example.com {\n\trespond other\n}\n"},
		"an ordered responder on another path":  {order: []string{"respond first"}, inside: "\trespond /x ok\n"},
		"not path /":                            {order: []string{"respond first"}, inside: "\t@m not path /\n\trespond @m ok\n"},
		"protocol http":                         {order: []string{"respond first"}, inside: "\t@m protocol http\n\trespond @m ok\n"},
		"method PUT":                            {order: []string{"respond first"}, inside: "\t@m method PUT\n\trespond @m ok\n"},
		"path and method that both decide no":   {order: []string{"respond first"}, inside: "\t@m path /x\n\trespond @m ok\n\t@n method DELETE\n\trespond @n ok\n"},
		"a handle group, the first member matched and passes": {order: []string{"handle first"},
			inside: "\thandle /* {\n\t\theader X-A b\n\t}\n\thandle {\n\t\trespond no\n\t}\n"},
		// Member 1 may match, member 2 matches for sure: either way the
		// group is satisfied before member 3, which Caddy never runs.
		"a handle group satisfied by its second member": {order: []string{"handle first"},
			inside: "\t@m header X-A b\n\thandle @m {\n\t\theader X-B c\n\t}\n\thandle /* {\n\t\theader X-C d\n\t}\n\thandle {\n\t\trespond no\n\t}\n"},
		"an ordered handle on another path": {order: []string{"handle first"}, inside: "\thandle /x {\n\t\trespond no\n\t}\n"},
		"a header matcher on a handler that passes": {order: []string{"header first"},
			inside: "\t@m header X-A b\n\theader @m X-B c\n"},

		// Refused: a second site for the host, sorted ahead whatever
		// the file order.
		"a site for host/* before":                 {before: "deploy.example.com/* {\n\trespond x\n}\n", want: respondFirst},
		"a site for host/* after":                  {after: "deploy.example.com/* {\n\trespond x\n}\n", want: respondFirst},
		"an empty site for host/":                  {before: "deploy.example.com/ {\n}\n", want: "an earlier site for deploy.example.com answers first"},
		"a site for host/ that only sets a header": {before: "deploy.example.com/ {\n\theader X-A b\n}\n", want: "an earlier site for deploy.example.com answers first"},

		// Refused: an `order` option moved a responder ahead.
		"an ordered responder": {order: []string{"respond first"}, inside: "\trespond ok\n", want: respondFirst},
		"method GET":           {order: []string{"respond first"}, inside: "\t@m method GET\n\trespond @m ok\n", want: respondFirst},
		"method POST":          {order: []string{"respond first"}, inside: "\t@m method POST\n\trespond @m ok\n", want: respondFirst},
		"protocol http/2":      {order: []string{"respond first"}, inside: "\t@m protocol http/2\n\trespond @m ok\n", want: "static_response may answer first (its protocol matcher is decided per request)"},
		"protocol http/1.1":    {order: []string{"respond first"}, inside: "\t@m protocol http/1.1\n\trespond @m ok\n", want: "static_response may answer first (its protocol matcher is decided per request)"},
		// A second block naming the bare host is "ambiguous site
		// definition" to the adapter (validate refuses it first); one
		// naming it with the port is another address string, the same
		// host matcher, and sorts by file order.
		"a site naming the host with its port before": {before: "deploy.example.com:443 {\n\trespond x\n}\n", want: respondFirst},
		"a site naming the host with its port after":  {after: "deploy.example.com:443 {\n\trespond x\n}\n"},
		"protocol https":        {order: []string{"respond first"}, inside: "\t@m protocol https\n\trespond @m ok\n", want: respondFirst},
		"path /*":               {order: []string{"respond first"}, inside: "\t@m path /*\n\trespond @m ok\n", want: respondFirst},
		"not path /x":           {order: []string{"respond first"}, inside: "\t@m not path /x\n\trespond @m ok\n", want: respondFirst},
		"a reverse_proxy":       {order: []string{"reverse_proxy first"}, inside: "\treverse_proxy localhost:9\n", want: "reverse_proxy answers first"},
		"liveswap_webhook":      {order: []string{"liveswap_webhook first"}, want: "liveswap_webhook answers first"},
		"a handle that answers": {order: []string{"handle first"}, inside: "\thandle {\n\t\trespond no\n\t}\n", want: respondFirst},
		"a route that answers":  {order: []string{"route first"}, inside: "\troute {\n\t\trespond no\n\t}\n", want: respondFirst},
		// Two handle blocks with the same (no) matcher adapt to ONE
		// grouped route holding both subroutes, so the second runs
		// after the first passes: Caddy's consolidation, mirrored.
		"two matcherless handles": {order: []string{"handle first"}, inside: "\thandle {\n\t\theader X-A b\n\t}\n\thandle {\n\t\trespond no\n\t}\n", want: respondFirst},
		"a handle group whose first member may have matched": {order: []string{"handle first"},
			inside: "\t@m header X-A b\n\thandle @m {\n\t\theader X-B c\n\t}\n\thandle {\n\t\trespond no\n\t}\n", want: mayHeader},

		// Refused: the request changed before box_webhook sees it.
		"a rewrite":                        {order: []string{"rewrite first"}, inside: "\trewrite * /x\n", want: "rewrite changes the request first"},
		"a uri rewrite":                    {order: []string{"uri first"}, inside: "\turi strip_prefix /x\n", want: "rewrite changes the request first"},
		"a request header":                 {order: []string{"request_header first"}, inside: "\trequest_header -Authorization\n", want: "headers changes the request first"},
		"a rewrite under a header matcher": {order: []string{"rewrite first"}, inside: "\t@m header X-A b\n\trewrite @m /x\n", want: "rewrite may change the request first (its header matcher is decided per request)"},

		// Refused: a matcher decided per request.
		"a header matcher":      {order: []string{"respond first"}, inside: "\t@m header X-A b\n\trespond @m ok\n", want: mayHeader},
		"an expression matcher": {order: []string{"respond first"}, inside: "\t@m expression {method} == \"GET\"\n\trespond @m ok\n", want: "the expression matcher ahead of it may fail (decided per request)"},
		// A matcher that may fail refuses whatever stands behind it:
		// Caddy answers its error and routes no further.
		"an expression matcher on a handler that passes": {order: []string{"header first"}, inside: "\t@m expression {method} == \"GET\"\n\theader @m X-B c\n", want: "the expression matcher ahead of it may fail (decided per request)"},
		"not over an expression":                         {order: []string{"respond first"}, inside: "\t@m not expression {method} == \"GET\"\n\trespond @m ok\n", want: "the not matcher ahead of it may fail (decided per request)"},
		"a query matcher":                                {order: []string{"respond first"}, inside: "\t@m query x=1\n\trespond @m ok\n", want: "static_response may answer first (its query matcher is decided per request)"},
		"a placeholder in a path":                        {order: []string{"respond first"}, inside: "\t@m path /{http.request.uri.query.x}\n\trespond @m ok\n", want: "static_response may answer first (its path matcher is decided per request)"},
		"not over a header":                              {order: []string{"respond first"}, inside: "\t@m not header X-A b\n\trespond @m ok\n", want: "static_response may answer first (its not matcher is decided per request)"},
		"a decided path beside an undecided header": {order: []string{"respond first"},
			inside: "\t@m path /x\n\t@n header X-A b\n\trespond @m ok\n\trespond @n ok\n", want: mayHeader},

		// The timeouts directive, which Caddy's default order puts ahead
		// of box_webhook: its read_timeout is an idle deadline that
		// counts only when positive, its rate only under one; the
		// floors are the server's for read_body_idle; the write settings
		// get none.
		"timeouts with the usual hardening":                    {inside: "\ttimeouts {\n\t\tread_timeout 30s 1024\n\t\twrite_timeout 30s 1024\n\t\tmax_write_chunk 64KB\n\t}\n"},
		"timeouts with nothing set":                            {inside: "\ttimeouts\n"},
		"timeouts read_timeout at the floor":                   {inside: "\ttimeouts {\n\t\tread_timeout 10s\n\t}\n"},
		"timeouts read_timeout under the floor":                {inside: "\ttimeouts {\n\t\tread_timeout 9s\n\t}\n", want: cutStall},
		"timeouts read_timeout 0s, no reader":                  {inside: "\ttimeouts {\n\t\tread_timeout 0s\n\t}\n"},
		"timeouts read_timeout negative, no reader":            {inside: "\ttimeouts {\n\t\tread_timeout -1s\n\t}\n"},
		"timeouts rate at the ceiling":                         {inside: "\ttimeouts {\n\t\tread_timeout 10s 65536\n\t}\n"},
		"timeouts rate over the ceiling":                       {inside: "\ttimeouts {\n\t\tread_timeout 10s 65537\n\t}\n", want: cutRate},
		"timeouts rate over the ceiling under no deadline":     {inside: "\ttimeouts {\n\t\tread_timeout 0s 65537\n\t}\n"},
		"timeouts read_timeout under and rate over":            {inside: "\ttimeouts {\n\t\tread_timeout 1s 65537\n\t}\n", want: cutStall},
		"timeouts write_timeout and max_write_chunk, no floor": {inside: "\ttimeouts {\n\t\twrite_timeout 1ms 1000000000\n\t\tmax_write_chunk 1\n\t}\n"},
		"timeouts on another path":                             {inside: "\ttimeouts /x {\n\t\tread_timeout 1s\n\t}\n"},
		"timeouts under a header matcher":                      {inside: "\t@m header X-A b\n\ttimeouts @m {\n\t\tread_timeout 1s\n\t}\n", want: "timeouts's read_timeout may cut an upload that stalls under 10s first (its header matcher is decided per request)"},
		"timeouts for the POST alone":                          {inside: "\t@m method POST\n\ttimeouts @m {\n\t\tread_timeout 1s\n\t}\n", want: cutStall},
		// Refused though the GET reads no body: the method is not weighed
		// in the walk (Accepted residuals).
		"timeouts for the GET alone": {inside: "\t@m method GET\n\ttimeouts @m {\n\t\tread_timeout 1s\n\t}\n", want: cutStall},
	} {
		t.Run(name, func(t *testing.T) {
			wantReach(t, reachOf(t, reachFile(c.order, c.before, c.inside, c.after)), c.want)
		})
	}
}

// TestReachDoor is the door (DESIGN-box.md, "The door"): the new file's
// box server must listen where the loaded server the push arrived
// through does — `https_port`, `bind` and `default_bind` all move it —
// compared as Caddy spells the addresses, in any order, each once; a
// nil door (no Caddy server received the push) is not compared, and the
// e2e box's own port is kept by a file that names it. A move is the
// catalogue's fixed text, which names no address.
func TestReachDoor(t *testing.T) {
	for name, c := range map[string]struct {
		door                  []string
		global, inside, after string
		moved                 bool
	}{
		"the same address":                {door: []string{":443"}},
		"no door":                         {global: "\thttps_port 8443\n"},
		"spelt with its network":          {door: []string{"tcp/:443"}},
		"twice":                           {door: []string{":443", ":443"}},
		"https_port":                      {door: []string{":443"}, global: "\thttps_port 8443\n", moved: true},
		"the e2e box's port, kept":        {door: []string{":8443"}, global: "\thttps_port 8443\n"},
		"a bind":                          {door: []string{":443"}, inside: "\tbind 127.0.0.1\n", moved: true},
		"the same bind":                   {door: []string{"127.0.0.1:443"}, inside: "\tbind 127.0.0.1\n"},
		"two binds, in the other order":   {door: []string{"[::1]:443", "127.0.0.1:443"}, inside: "\tbind 127.0.0.1 ::1\n"},
		"a bind added beside the address": {door: []string{":443"}, inside: "\tbind 0.0.0.0 ::\n", moved: true},
		"default_bind":                    {door: []string{":443"}, global: "\tdefault_bind 10.0.0.5\n", moved: true},
		"the box site alone moved":        {door: []string{":443"}, inside: "\tbind 10.0.0.5\n", moved: true},
		"another site moved":              {door: []string{":443"}, global: "\thttp_port 8080\n"},
		// A site bound to two addresses adapts into a server per set of
		// addresses it shares with other sites, each carrying its
		// routes: the push's own server is the one asked.
		"two servers carry the webhook, the push through the shared address": {door: []string{"127.0.0.1:443"},
			inside: "\tbind 127.0.0.1 127.0.0.2\n", after: "other.example.com {\n\tbind 127.0.0.1\n\trespond x\n}\n"},
		"two servers carry the webhook, the push through the other address": {door: []string{"127.0.0.2:443"},
			inside: "\tbind 127.0.0.1 127.0.0.2\n", after: "other.example.com {\n\tbind 127.0.0.1\n\trespond x\n}\n"},
		"two servers carry the webhook, neither at the door": {door: []string{":443"},
			inside: "\tbind 127.0.0.1 127.0.0.2\n", after: "other.example.com {\n\tbind 127.0.0.1\n\trespond x\n}\n", moved: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(reachFile(nil, "", c.inside, c.after), "{\n", "{\n"+c.global, 1)
			cfg, _, err := adapt(t, in)
			if err != nil {
				t.Fatalf("adapt: %v\n%s", err, in)
			}
			wantDoor(t, reach(cfg, "deploy.example.com", c.door), c.moved)
		})
	}
}

// wantDoor checks a door verdict: kept, or the catalogue's fixed text.
func wantDoor(t *testing.T, err error, moved bool) {
	t.Helper()
	var r *refusal
	switch {
	case !moved && err != nil:
		t.Fatalf("refused: %v", err)
	case moved && (!errors.As(err, &r) || r.msg != msgListenMoved):
		t.Fatalf("got  %v\nwant %q", err, msgListenMoved)
	}
}

// TestReachDoorJSON is the door on adaptations by hand: a placeholder
// in an address, replaced as Caddy's App.Provision replaces it before
// the loaded server keeps it; a listen list the box must not trust
// blindly; a port range and an address the grammar does not read,
// compared as written; and the text, which quotes nothing of any of
// them.
func TestReachDoorJSON(t *testing.T) {
	t.Setenv("BOX_TEST_ADDR", ":443")
	for name, c := range map[string]struct {
		listen string // the server's listen key with its comma, or ""
		door   []string
		want   string // "" kept; "moved" the fixed text; "error" the box's own
	}{
		"a placeholder resolving to the door": {listen: `"listen":["{env.BOX_TEST_ADDR}"],`, door: []string{":443"}},
		"a placeholder resolving elsewhere":   {listen: `"listen":["{env.BOX_TEST_ADDR}"],`, door: []string{":8443"}, want: "moved"},
		// Provision refuses an unresolved placeholder, and validate ran
		// it on the same bytes: here it is the box's own.
		"a placeholder that does not resolve":   {listen: `"listen":["{env.BOX_TEST_NOT_SET}"],`, door: []string{":443"}, want: "error"},
		"no listen key":                         {door: []string{":443"}, want: "error"},
		"listen null":                           {listen: `"listen":null,`, door: []string{":443"}, want: "error"},
		"listen empty":                          {listen: `"listen":[],`, door: []string{":443"}, want: "moved"},
		"an empty door":                         {listen: `"listen":[":443"],`, door: []string{}, want: "moved"},
		"no listen key and no door":             {},
		"an address that does not parse, twice": {listen: `"listen":["x:y","x:y"],`, door: []string{"x:y"}},
		"an address that does not parse, moved": {listen: `"listen":["x:y"],`, door: []string{":443"}, want: "moved"},
		"a port range, as written":              {listen: `"listen":[":443-444"],`, door: []string{":443", ":444"}, want: "moved"},
		"a listen that does not read":           {listen: `"listen":":443",`, door: []string{":443"}, want: "error"},
		"a listen whose address does not read":  {listen: `"listen":[443],`, door: []string{":443"}, want: "error"},
		"a listen that does not read, no door":  {listen: `"listen":":443",`},
		"a control byte in the address":         {listen: `"listen":[":443\n"],`, door: []string{":443"}, want: "moved"},
	} {
		t.Run(name, func(t *testing.T) {
			adapted := strings.Replace(string(connJSON("")), `"listen":[":443"],`, c.listen, 1)
			err := reach([]byte(adapted), "deploy.example.com", c.door)
			if c.want == "error" {
				var r *refusal
				if err == nil || errors.As(err, &r) {
					t.Fatalf("got %v, want the box's own error", err)
				}
				return
			}
			wantDoor(t, err, c.want == "moved")
		})
	}
}

// TestReachTemplate: the template's own box file (examples/box) reaches
// box_webhook, its connection included.
func TestReachTemplate(t *testing.T) {
	in, err := os.ReadFile("../examples/box/deploy.example.com/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	wantReach(t, reachOf(t, string(in)), "")
}

// TestReachConnection is the connection's tables (DESIGN-box.md,
// "Reachability") on adapted Caddyfiles: the protocols the box
// server's listeners serve, its listener wrappers, the TLS
// connection policy for the host, and the floors on its request
// limits — Caddy's defaults and the usual hardening pass, a value the
// workflow's request cannot work with is refused by name. global goes
// in the global options.
func TestReachConnection(t *testing.T) {
	const (
		servers    = "\tservers {\n%s\t}\n"
		timeouts   = "\tservers {\n\t\ttimeouts {\n%s\t\t}\n\t}\n"
		clientAuth = "\ttls {\n\t\tclient_auth {\n\t\t\tmode require\n\t\t}\n\t}\n"
		noH1H2     = "a listener's protocols include neither h1 nor h2"
		requires   = "a TLS connection policy for deploy.example.com requires a client certificate"
		readHeader = "the server's read_header_timeout is under 5s, the floor for a push's handshake and headers"
		readBody   = "the server's read_timeout is under 30s, the floor for a push's upload"
		readIdle   = "the server's read_idle_timeout is under 10s, the floor for a push's upload"
		minRate    = "the server's read_min_rate is over 64 KiB/s, the ceiling for a push's upload"
		maxHeader  = "the server's max_header_bytes is under 8 KB, the floor for a push's headers and token"
		write      = "the server's write_timeout is under 5s, the floor for a push's handshake"
	)
	for name, c := range map[string]struct {
		global, inside, after string
		want                  string
	}{
		"the design's example":                                    {},
		"the usual hardening":                                     {global: fmt.Sprintf(servers, "\t\tmax_header_size 16KB\n\t\ttimeouts {\n\t\t\tread_header 10s\n\t\t\tread_body 30s\n\t\t\tread_body_idle 30s 1024\n\t\t\twrite 30s\n\t\t\tidle 1m\n\t\t}\n")},
		"read_header at the floor":                                {global: fmt.Sprintf(timeouts, "\t\t\tread_header 5s\n")},
		"read_header under the floor":                             {global: fmt.Sprintf(timeouts, "\t\t\tread_header 1s\n"), want: readHeader},
		"read_header 0s, Caddy's default":                         {global: fmt.Sprintf(timeouts, "\t\t\tread_header 0s\n")},
		"read_body at the floor":                                  {global: fmt.Sprintf(timeouts, "\t\t\tread_body 30s\n")},
		"read_body under the floor":                               {global: fmt.Sprintf(timeouts, "\t\t\tread_body 29s\n"), want: readBody},
		"read_body_idle at the floor, the rate at its ceiling":    {global: fmt.Sprintf(timeouts, "\t\t\tread_body_idle 10s 65536\n")},
		"read_body_idle under the floor":                          {global: fmt.Sprintf(timeouts, "\t\t\tread_body_idle 9s\n"), want: readIdle},
		"a rate over the ceiling":                                 {global: fmt.Sprintf(timeouts, "\t\t\tread_body_idle 30s 65537\n"), want: minRate},
		"write at the floor":                                      {global: fmt.Sprintf(timeouts, "\t\t\twrite 5s\n")},
		"write under the floor":                                   {global: fmt.Sprintf(timeouts, "\t\t\twrite 1s\n"), want: write},
		"write_idle, its rate, idle and the keepalives, no floor": {global: fmt.Sprintf(servers, "\t\ttimeouts {\n\t\t\twrite_idle 1s 1000000\n\t\t\tidle 1s\n\t\t}\n\t\tkeepalive_interval 1s\n\t\tkeepalive_idle 1s\n\t\tkeepalive_count 1\n")},
		"max_header_size 8KiB":                                    {global: fmt.Sprintf(servers, "\t\tmax_header_size 8KiB\n")},
		"max_header_size under the floor":                         {global: fmt.Sprintf(servers, "\t\tmax_header_size 4KB\n"), want: maxHeader},
		// Caddy reads KB as 1000 bytes: the floor is 8000, so the usual
		// spelling is at it, and 7KB under.
		"max_header_size 8KB, at the floor":                 {global: fmt.Sprintf(servers, "\t\tmax_header_size 8KB\n")},
		"max_header_size 7KB":                               {global: fmt.Sprintf(servers, "\t\tmax_header_size 7KB\n"), want: maxHeader},
		"limits on another port's server":                   {global: "\tservers :8443 {\n\t\tmax_header_size 1KB\n\t\ttimeouts {\n\t\t\tread_header 1s\n\t\t}\n\t}\n"},
		"a short read_header ahead of a responder in front": {global: fmt.Sprintf(timeouts, "\t\t\tread_header 1s\n") + "\torder respond first\n", inside: "\trespond ok\n", want: readHeader},
		"protocols h1":                                      {global: fmt.Sprintf(servers, "\t\tprotocols h1\n")},
		"protocols h2 and h3":                               {global: fmt.Sprintf(servers, "\t\tprotocols h2 h3\n")},
		"protocols h3":                                      {global: fmt.Sprintf(servers, "\t\tprotocols h3\n"), want: noH1H2},
		"protocols h3 for another port's server":            {global: "\tservers :8443 {\n\t\tprotocols h3\n\t}\n"},
		"a bind serving h3 alone":                           {inside: "\tbind 0.0.0.0 {\n\t\tprotocols h3\n\t}\n", want: noH1H2},
		"a bind serving h1":                                 {inside: "\tbind 0.0.0.0 {\n\t\tprotocols h1\n\t}\n"},
		"http_redirect ahead of tls":                        {global: fmt.Sprintf(servers, "\t\tlistener_wrappers {\n\t\t\thttp_redirect\n\t\t\ttls\n\t\t}\n")},
		"http_redirect with no tls listed":                  {global: fmt.Sprintf(servers, "\t\tlistener_wrappers {\n\t\t\thttp_redirect\n\t\t}\n"), want: "the http_redirect listener wrapper may refuse the connection"},
		"a client certificate required":                     {inside: clientAuth, want: requires},
		"a trust pool and no mode":                          {inside: "\ttls {\n\t\tclient_auth {\n\t\t\ttrust_pool file /etc/ca.pem\n\t\t}\n\t}\n", want: requires},
		"a client certificate if given":                     {inside: "\ttls {\n\t\tclient_auth {\n\t\t\tmode verify_if_given\n\t\t\ttrust_pool file /etc/ca.pem\n\t\t}\n\t}\n"},
		"a client certificate requested":                    {inside: "\ttls {\n\t\tclient_auth {\n\t\t\tmode request\n\t\t}\n\t}\n"},
		"a client certificate on another site":              {after: "other.example.com {\n" + clientAuth + "\trespond x\n}\n"},
		"a client certificate on a wildcard site":           {after: "*.example.com {\n" + clientAuth + "\trespond x\n}\n"},
		"a client certificate on a wildcard site, the host's own policy too": {inside: "\ttls {\n\t\tprotocols tls1.3\n\t}\n",
			after: "*.example.com {\n" + clientAuth + "\trespond x\n}\n"},
		"alpn h2":       {inside: "\ttls {\n\t\talpn h2\n\t}\n"},
		"alpn http/1.1": {inside: "\ttls {\n\t\talpn http/1.1\n\t}\n"},
		"alpn h3":       {inside: "\ttls {\n\t\talpn h3\n\t}\n", want: "a TLS connection policy for deploy.example.com offers neither h2 nor http/1.1 by ALPN"},
		"alpn h2 where only h1 is served": {global: fmt.Sprintf(servers, "\t\tprotocols h1\n"), inside: "\ttls {\n\t\talpn h2\n\t}\n",
			want: "a TLS connection policy for deploy.example.com offers neither h2 nor http/1.1 by ALPN"},
		"TLS 1.3 only":                  {inside: "\ttls {\n\t\tprotocols tls1.3\n\t}\n"},
		"a certificate from files":      {inside: "\ttls /etc/cert.pem /etc/key.pem\n"},
		"the internal issuer":           {inside: "\ttls internal\n"},
		"ciphers":                       {inside: "\ttls {\n\t\tciphers TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256\n\t}\n", want: "a TLS connection policy for deploy.example.com sets cipher_suites, which may refuse the handshake"},
		"curves":                        {inside: "\ttls {\n\t\tcurves x25519\n\t}\n", want: "a TLS connection policy for deploy.example.com sets curves, which may refuse the handshake"},
		"ahead of a responder in front": {global: fmt.Sprintf(servers, "\t\tprotocols h3\n") + "\torder respond first\n", inside: "\trespond ok\n", want: noH1H2},
	} {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(reachFile(nil, "", c.inside, c.after), "{\n", "{\n"+c.global, 1)
			wantReach(t, reachOf(t, in), c.want)
		})
	}
}

// connJSON is an adaptation by hand: one server with settings (each
// followed by a comma) and the box site's route.
func connJSON(settings string) []byte {
	return []byte(`{"apps":{"http":{"servers":{"srv0":{"listen":[":443"],` + settings +
		`"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`)
}

// TestReachConnectionJSON is the connection's rows the adapter cannot
// be made to produce in this module's tests: proxy_protocol (not
// registered here), wrappers and policy settings no directive adapts
// to, matchers on a policy, and shapes the box must not trust blindly.
func TestReachConnectionJSON(t *testing.T) {
	const (
		requires = "a TLS connection policy for deploy.example.com requires a client certificate"
		wrapper  = "the %s listener wrapper may refuse the connection"
	)
	const ip = "203.0.113.5"
	for name, c := range map[string]struct {
		settings string
		host     string // deploy.example.com when empty
		want     string // "" reachable; "error" a plain error; else the refusal
	}{
		"proxy_protocol":                                   {settings: `"listener_wrappers":[{"wrapper":"proxy_protocol"},{"wrapper":"tls"}],`},
		"proxy_protocol after tls":                         {settings: `"listener_wrappers":[{"wrapper":"proxy_protocol"}],`},
		"proxy_protocol falling back to USE":               {settings: `"listener_wrappers":[{"wrapper":"proxy_protocol","fallback_policy":"USE"},{"wrapper":"tls"}],`},
		"proxy_protocol requiring a header":                {settings: `"listener_wrappers":[{"wrapper":"proxy_protocol","allow":["10.0.0.0/8"],"fallback_policy":"REQUIRE"},{"wrapper":"tls"}],`, want: fmt.Sprintf(wrapper, "proxy_protocol")},
		"proxy_protocol requiring a header, in lower case": {settings: `"listener_wrappers":[{"wrapper":"proxy_protocol","fallback_policy":"require"},{"wrapper":"tls"}],`, want: fmt.Sprintf(wrapper, "proxy_protocol")},
		"http_redirect behind tls":                         {settings: `"listener_wrappers":[{"wrapper":"proxy_protocol"},{"wrapper":"tls"},{"wrapper":"http_redirect"}],`, want: fmt.Sprintf(wrapper, "http_redirect")},
		"a wrapper the table does not name":                {settings: `"listener_wrappers":[{"wrapper":"layer4"},{"wrapper":"tls"}],`, want: fmt.Sprintf(wrapper, "layer4")},
		"a nameless wrapper":                               {settings: `"listener_wrappers":[{},{"wrapper":"tls"}],`, want: fmt.Sprintf(wrapper, "nameless")},
		"h2c alone":                                        {settings: `"protocols":["h2c"],`, want: "a listener's protocols include neither h1 nor h2"},
		"one listener of two serving h3 alone":             {settings: `"protocols":["h1","h2"],"listen_protocols":[["h3"],null],`, want: "a listener's protocols include neither h1 nor h2"},
		"a listener adding h3 to the server's":             {settings: `"protocols":["h1"],"listen_protocols":[["","h3"]],`},
		"a listener's empty list, the server's":            {settings: `"protocols":["h1"],"listen_protocols":[[]],`},
		"drop":                                             {settings: `"tls_connection_policies":[{"drop":true}],`, want: "a TLS connection policy for deploy.example.com drops the connection"},
		"drop false":                                       {settings: `"tls_connection_policies":[{"drop":false}],`},
		"an empty client_authentication":                   {settings: `"tls_connection_policies":[{"client_authentication":{}}],`},
		"a verifier, mode request":                         {settings: `"tls_connection_policies":[{"client_authentication":{"mode":"request","verifiers":[{"verifier":"leaf"}]}}],`},
		"trusted leaf certs, mode verify_if_given":         {settings: `"tls_connection_policies":[{"client_authentication":{"mode":"verify_if_given","trusted_leaf_certs":["AA=="]}}],`},
		"a verifier and no mode":                           {settings: `"tls_connection_policies":[{"client_authentication":{"verifiers":[{"verifier":"leaf"}]}}],`, want: requires},
		"a setting the table does not name":                {settings: `"tls_connection_policies":[{"handshake_context":{"module":"x"}}],`, want: "a TLS connection policy for deploy.example.com sets handshake_context, which may refuse the handshake"},
		// The first policy whose matchers all match is Caddy's: one
		// for the host shields the ones after it; one decided per
		// connection shields nothing.
		"the host's policy ahead of a catch-all requiring": {settings: `"tls_connection_policies":[{"match":{"sni":["deploy.example.com"]}},{"client_authentication":{"mode":"require"}}],`},
		"another host's policy requiring":                  {settings: `"tls_connection_policies":[{"match":{"sni":["other.example.com"]},"client_authentication":{"mode":"require"}},{}],`},
		"a wildcard policy requiring":                      {settings: `"tls_connection_policies":[{"match":{"sni":["*.example.com"]},"client_authentication":{"mode":"require"}},{}],`, want: requires},
		"a remote_ip policy requiring": {settings: `"tls_connection_policies":[{"match":{"remote_ip":{"ranges":["10.0.0.0/8"]}},"client_authentication":{"mode":"require"}},{}],`,
			want: "a TLS connection policy that may apply to deploy.example.com requires a client certificate (its remote_ip matcher is decided per connection)"},
		"a remote_ip policy ahead of a catch-all requiring": {settings: `"tls_connection_policies":[{"match":{"remote_ip":{"ranges":["10.0.0.0/8"]}}},{"client_authentication":{"mode":"require"}}],`, want: requires},
		"an sni placeholder requiring": {settings: `"tls_connection_policies":[{"match":{"sni":["{env.HOST}"]},"client_authentication":{"mode":"require"}},{}],`,
			want: "a TLS connection policy that may apply to deploy.example.com requires a client certificate (its sni matcher is decided per connection)"},
		"another server's settings": {settings: `"protocols":["h1"]},"other":{"listen":[":8443"],"protocols":["h3"],"listener_wrappers":[{"wrapper":"layer4"}],"tls_connection_policies":[{"drop":true}],"routes":[]},"srv1":{"listen":[":444"],`},
		// What Caddy reads as off, or as its default, is off or the
		// default here: Caddy's own type for client authentication,
		// and a setting written as null is absent.
		"client_authentication with an empty mode":       {settings: `"tls_connection_policies":[{"client_authentication":{"mode":""}}],`},
		"client_authentication with an empty trust list": {settings: `"tls_connection_policies":[{"client_authentication":{"trusted_ca_certs":[]}}],`},
		"mode request with an empty verifier list":       {settings: `"tls_connection_policies":[{"client_authentication":{"mode":"request","verifiers":[]}}],`},
		"alpn null, the default":                         {settings: `"tls_connection_policies":[{"alpn":null}],`},
		"alpn empty, no protocol negotiated, h1 served":  {settings: `"tls_connection_policies":[{"alpn":[]}],`},
		"alpn empty where only h2 is served":             {settings: `"protocols":["h2"],"tls_connection_policies":[{"alpn":[]}],`, want: "a TLS connection policy for deploy.example.com offers neither h2 nor http/1.1 by ALPN"},
		"cipher_suites null":                             {settings: `"tls_connection_policies":[{"cipher_suites":null}],`},
		// curl sends no SNI to an IP address: a policy naming it does
		// not apply, and the strict SNI check refuses the request.
		"an IP host's own policy ahead of a catch-all requiring": {host: ip, settings: `"strict_sni_host":false,"tls_connection_policies":[{"match":{"sni":["203.0.113.5"]}},{"client_authentication":{"mode":"require"}}],`,
			want: "a TLS connection policy for 203.0.113.5 requires a client certificate"},
		"an IP host's own policy requiring, strict SNI off": {host: ip, settings: `"strict_sni_host":false,"tls_connection_policies":[{"match":{"sni":["203.0.113.5"]},"client_authentication":{"mode":"require"}},{}],`},
		"an IP host, strict SNI on":                         {host: ip, settings: `"strict_sni_host":true,`, want: "the server's strict_sni_host refuses a request to an IP address, which carries no SNI"},
		"an IP host, strict SNI on by another site's client authentication": {host: ip, settings: `"tls_connection_policies":[{"match":{"sni":["example.com"]},"client_authentication":{"mode":"require"}},{}],`,
			want: "the server's strict_sni_host refuses a request to an IP address, which carries no SNI"},
		"a nameless setting":        {settings: `"tls_connection_policies":[{"":1}],`, want: "a TLS connection policy for deploy.example.com sets nameless, which may refuse the handshake"},
		"a nameless matcher":        {settings: `"tls_connection_policies":[{"match":{"":1},"drop":true},{}],`, want: "a TLS connection policy that may apply to deploy.example.com drops the connection (its nameless matcher is decided per connection)"},
		"a hostname, strict SNI on": {settings: `"strict_sni_host":true,`},
		// The request limits, in Caddy's shapes: a duration as a number
		// of nanoseconds or as a string, bytes as an integer. Zero or
		// null is Caddy's default, a negative value no limit; the
		// boundary is the floor itself.
		"read_header_timeout as a string, under":     {settings: `"read_header_timeout":"4s",`, want: "the server's read_header_timeout is under 5s, the floor for a push's handshake and headers"},
		"read_header_timeout as nanoseconds, under":  {settings: `"read_header_timeout":4999999999,`, want: "the server's read_header_timeout is under 5s, the floor for a push's handshake and headers"},
		"read_header_timeout at the floor":           {settings: `"read_header_timeout":5000000000,`},
		"read_header_timeout zero":                   {settings: `"read_header_timeout":0,`},
		"read_header_timeout null":                   {settings: `"read_header_timeout":null,`},
		"read_header_timeout negative, no timeout":   {settings: `"read_header_timeout":-1,`},
		"read_timeout under":                         {settings: `"read_timeout":"29s",`, want: "the server's read_timeout is under 30s, the floor for a push's upload"},
		"read_idle_timeout under":                    {settings: `"read_idle_timeout":"9s",`, want: "the server's read_idle_timeout is under 10s, the floor for a push's upload"},
		"write_timeout under":                        {settings: `"write_timeout":"4s",`, want: "the server's write_timeout is under 5s, the floor for a push's handshake"},
		"max_header_bytes under":                     {settings: `"max_header_bytes":7999,`, want: "the server's max_header_bytes is under 8 KB, the floor for a push's headers and token"},
		"max_header_bytes at the floor":              {settings: `"max_header_bytes":8000,`},
		"max_header_bytes negative, Caddy's default": {settings: `"max_header_bytes":-5,`},
		"read_min_rate at the ceiling":               {settings: `"read_min_rate":65536,`},
		"read_min_rate over":                         {settings: `"read_min_rate":65537,`, want: "the server's read_min_rate is over 64 KiB/s, the ceiling for a push's upload"},
		"read_min_rate negative, no rate":            {settings: `"read_min_rate":-1,`},
		"read_min_rate over, under a negative read idle, which Caddy never applies": {settings: `"read_idle_timeout":-1,"read_min_rate":65537,`},
		"read_min_rate over, read idle zero, the default":                           {settings: `"read_idle_timeout":0,"read_min_rate":65537,`, want: "the server's read_min_rate is over 64 KiB/s, the ceiling for a push's upload"},
		"write_idle_timeout, write_min_rate and idle_timeout, no floor":             {settings: `"write_idle_timeout":1,"write_min_rate":1000000,"idle_timeout":1,`},
		"two limits under: the table's first is named":                              {settings: `"read_header_timeout":"1s","max_header_bytes":1,`, want: "the server's max_header_bytes is under 8 KB, the floor for a push's headers and token"},
		"a limit under, behind a strict SNI refusal":                                {host: ip, settings: `"strict_sni_host":true,"max_header_bytes":1,`, want: "the server's strict_sni_host refuses a request to an IP address, which carries no SNI"},
		// Integers are kept as written: one at int64's edge reads as
		// Caddy reads it, where a float64 would round it out of range.
		"read_min_rate at int64's edge, under a negative read idle": {settings: `"read_idle_timeout":-1,"read_min_rate":9223372036854775807,`},
		"read_min_rate at int64's edge":                             {settings: `"read_min_rate":9223372036854775807,`, want: "the server's read_min_rate is over 64 KiB/s, the ceiling for a push's upload"},
		"read_header_timeout at int64's edge":                       {settings: `"read_header_timeout":9223372036854775807,`},
		"max_header_bytes past int64":                               {settings: `"max_header_bytes":9223372036854775808,`, want: "error"},
		"a duration that does not read":                             {settings: `"read_header_timeout":"soon",`, want: "error"},
		"a duration with a fraction":                                {settings: `"read_timeout":1.5,`, want: "error"},
		"a byte count that does not read":                           {settings: `"max_header_bytes":"8KB",`, want: "error"},
		"a rate that does not read":                                 {settings: `"read_min_rate":"fast",`, want: "error"},
		"protocols that do not read":                                {settings: `"protocols":"h1",`, want: "error"},
		"listener wrappers that do not read":                        {settings: `"listener_wrappers":{"wrapper":"tls"},`, want: "error"},
		"policies that do not read":                                 {settings: `"tls_connection_policies":[1],`, want: "error"},
	} {
		t.Run(name, func(t *testing.T) {
			host := c.host
			if host == "" {
				host = "deploy.example.com"
			}
			err := reach([]byte(strings.ReplaceAll(string(connJSON(c.settings)), "deploy.example.com", host)), host, nil)
			if c.want == "error" {
				var r *refusal
				if err == nil || errors.As(err, &r) {
					t.Fatalf("got %v, want the box's own error", err)
				}
				return
			}
			wantReach(t, err, c.want)
		})
	}
}

// reachJSON is an adaptation by hand: one server, the box site's route
// as Caddy emits it, with routes before box_webhook inside it.
func reachJSON(before string, siteBefore string) []byte {
	return []byte(`{"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[` + siteBefore +
		`{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[` + before +
		`{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`)
}

// TestReachJSON is the rows the adapter cannot be made to produce in
// this module's tests: a handler the table names that no registered
// directive adapts to here, one it does not name, and the shapes of an
// adaptation the box itself must not trust blindly.
func TestReachJSON(t *testing.T) {
	for name, c := range map[string]struct {
		adapted []byte
		want    string // "" reachable; "error" a plain error; else the refusal
	}{
		"the penaltybox hint ahead":        {adapted: reachJSON(`{"handle":[{"handler":"hint_penaltybox"}]},`, ""), want: ""},
		"vars, map, log and tracing ahead": {adapted: reachJSON(`{"handle":[{"handler":"vars"},{"handler":"map"},{"handler":"log_append"},{"handler":"tracing"},{"handler":"timeouts"},{"handler":"push"},{"handler":"encode"},{"handler":"request_body"}]},`, ""), want: ""},
		// request_body: a set replaces every body; a positive max_size
		// under the bundle's cap refuses a bundle the handler would
		// take; either under a per-request matcher is "may". Read as
		// Caddy reads them: "" and null are absent, a non-positive cap
		// is none, and set wins over the cap (it runs first).
		"request_body with a set":                                         {adapted: reachJSON(`{"handle":[{"handler":"request_body","set":"{}"}]},`, ""), want: "request_body's set changes the request first"},
		"request_body with a set under a header matcher":                  {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"request_body","set":"x"}]},`, ""), want: "request_body's set may change the request first (its header matcher is decided per request)"},
		"request_body with a set under a failing matcher":                 {adapted: reachJSON(`{"match":[{"expression":{"expr":"true"}}],"handle":[{"handler":"request_body","set":"x"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		"request_body with an empty set":                                  {adapted: reachJSON(`{"handle":[{"handler":"request_body","set":""}]},`, ""), want: ""},
		"request_body with set null":                                      {adapted: reachJSON(`{"handle":[{"handler":"request_body","set":null}]},`, ""), want: ""},
		"request_body with a max_size under the bundle cap":               {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":16777215}]},`, ""), want: "request_body's max_size caps the body under 16 MiB first"},
		"request_body with a max_size at the bundle cap":                  {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":16777216}]},`, ""), want: ""},
		"request_body with a max_size of 1 MiB under a remote_ip matcher": {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"request_body","max_size":1048576}]},`, ""), want: "request_body's max_size may cap the body under 16 MiB first (its remote_ip matcher is decided per request)"},
		"request_body with a max_size on another path":                    {adapted: reachJSON(`{"match":[{"path":["/x"]}],"handle":[{"handler":"request_body","max_size":1}]},`, ""), want: ""},
		"request_body with max_size zero":                                 {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":0}]},`, ""), want: ""},
		"request_body with a negative max_size":                           {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":-1}]},`, ""), want: ""},
		"request_body with max_size null":                                 {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":null}]},`, ""), want: ""},
		"request_body with a set and a max_size under":                    {adapted: reachJSON(`{"handle":[{"handler":"request_body","set":"x","max_size":1}]},`, ""), want: "request_body's set changes the request first"},
		"request_body with a set behind box_webhook":                      {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"},{"handler":"request_body","set":"x"}]}]}],"terminal":true}]}}}}}`), want: ""},
		"request_body whose set does not read":                            {adapted: reachJSON(`{"handle":[{"handler":"request_body","set":5}]},`, ""), want: "error"},
		"request_body whose max_size does not read":                       {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":"1MB"}]},`, ""), want: "error"},
		"request_body whose max_size has a fraction":                      {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":1.5}]},`, ""), want: "error"},
		"request_body with a max_size at int64's edge":                    {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":9223372036854775807}]},`, ""), want: ""},
		"request_body with a max_size past int64":                         {adapted: reachJSON(`{"handle":[{"handler":"request_body","max_size":9223372036854775808}]},`, ""), want: "error"},
		"a value after the adaptation":                                    {adapted: append(reachJSON("", ""), " 1"...), want: "error"},
		"whitespace after the adaptation":                                 {adapted: append(reachJSON("", ""), " \n"...), want: ""},
		"another handler's set, max_size and read_timeout keys":           {adapted: reachJSON(`{"handle":[{"handler":"vars","set":5,"max_size":"1MB","read_timeout":"1s"}]},`, ""), want: ""},
		// timeouts: read_timeout, an idle deadline, counts only when
		// positive, and read_min_rate only under one; the floors are
		// the server's for read_idle_timeout and read_min_rate. Read as
		// Caddy reads them: whole numbers (a time.Duration reads no
		// string), null absent; the deadline is named ahead of the rate.
		"timeouts with a read_timeout under the floor":                        {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":9999999999}]},`, ""), want: "timeouts's read_timeout cuts an upload that stalls under 10s first"},
		"timeouts with a read_timeout at the floor":                           {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":10000000000}]},`, ""), want: ""},
		"timeouts with read_timeout zero":                                     {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":0}]},`, ""), want: ""},
		"timeouts with a negative read_timeout":                               {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":-1}]},`, ""), want: ""},
		"timeouts with read_timeout null":                                     {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":null}]},`, ""), want: ""},
		"timeouts with a rate over the ceiling":                               {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":10000000000,"read_min_rate":65537}]},`, ""), want: "timeouts's read_min_rate cuts an upload under 64 KiB/s first"},
		"timeouts with a rate at the ceiling":                                 {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":10000000000,"read_min_rate":65536}]},`, ""), want: ""},
		"timeouts with a rate over the ceiling and no read_timeout":           {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_min_rate":65537}]},`, ""), want: ""},
		"timeouts with a rate over the ceiling under a negative read_timeout": {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":-1,"read_min_rate":65537}]},`, ""), want: ""},
		"timeouts with a read_timeout under and a rate over":                  {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":1,"read_min_rate":65537}]},`, ""), want: "timeouts's read_timeout cuts an upload that stalls under 10s first"},
		"timeouts with a rate at int64's edge":                                {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":10000000000,"read_min_rate":9223372036854775807}]},`, ""), want: "timeouts's read_min_rate cuts an upload under 64 KiB/s first"},
		"timeouts with a read_timeout at int64's edge":                        {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":9223372036854775807}]},`, ""), want: ""},
		"timeouts with the write settings alone":                              {adapted: reachJSON(`{"handle":[{"handler":"timeouts","write_timeout":1,"write_min_rate":1000000000,"max_write_chunk":1}]},`, ""), want: ""},
		"timeouts with a read_timeout under a remote_ip matcher":              {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"timeouts","read_timeout":1}]},`, ""), want: "timeouts's read_timeout may cut an upload that stalls under 10s first (its remote_ip matcher is decided per request)"},
		"timeouts with a rate over under a header matcher":                    {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"timeouts","read_timeout":10000000000,"read_min_rate":65537}]},`, ""), want: "timeouts's read_min_rate may cut an upload under 64 KiB/s first (its header matcher is decided per request)"},
		"timeouts with a read_timeout under a failing matcher":                {adapted: reachJSON(`{"match":[{"expression":{"expr":"true"}}],"handle":[{"handler":"timeouts","read_timeout":1}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		"timeouts with a read_timeout under, on another path":                 {adapted: reachJSON(`{"match":[{"path":["/x"]}],"handle":[{"handler":"timeouts","read_timeout":1}]},`, ""), want: ""},
		"timeouts with a read_timeout under, behind box_webhook":              {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"},{"handler":"timeouts","read_timeout":1}]}]}],"terminal":true}]}}}}}`), want: ""},
		"timeouts whose read_timeout is a duration string":                    {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":"10s"}]},`, ""), want: "error"},
		"timeouts whose read_timeout has a fraction":                          {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":1.5}]},`, ""), want: "error"},
		"timeouts whose read_timeout is past int64":                           {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_timeout":9223372036854775808}]},`, ""), want: "error"},
		"timeouts whose rate does not read, under no read_timeout":            {adapted: reachJSON(`{"handle":[{"handler":"timeouts","read_min_rate":"fast"}]},`, ""), want: "error"},
		"templates ahead":                               {adapted: reachJSON(`{"handle":[{"handler":"templates","mime_types":["application/json"]}]},`, ""), want: "templates answers first"},
		"templates ahead with its default types":        {adapted: reachJSON(`{"handle":[{"handler":"templates"}]},`, ""), want: "templates answers first"},
		"intercept ahead":                               {adapted: reachJSON(`{"handle":[{"handler":"intercept","handle_response":[{"match":{"status_code":[2]},"routes":[{"handle":[{"handler":"static_response","body":"maintenance"}]}]}]}]},`, ""), want: "intercept answers first"},
		"a response headers handler":                    {adapted: reachJSON(`{"handle":[{"handler":"headers","response":{"set":{"X-A":["b"]}}}]},`, ""), want: ""},
		"a request headers handler":                     {adapted: reachJSON(`{"handle":[{"handler":"headers","request":{"delete":["Authorization"]}}]},`, ""), want: "headers changes the request first"},
		"a handler the table does not name":             {adapted: reachJSON(`{"handle":[{"handler":"cache"}]},`, ""), want: "cache answers first"},
		"invoke":                                        {adapted: reachJSON(`{"handle":[{"handler":"invoke","name":"r"}]},`, ""), want: "invoke answers first"},
		"an undecided matcher on a handler that passes": {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"encode"}]},`, ""), want: ""},
		"an undecided matcher on a responder":           {adapted: reachJSON(`{"match":[{"remote_ip":{"ranges":["10.0.0.0/8"]}}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its remote_ip matcher is decided per request)"},
		"two sets, one decided no and one undecided":    {adapted: reachJSON(`{"match":[{"path":["/x"]},{"header":{"X-A":["b"]}}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its header matcher is decided per request)"},
		"two sets, one decided yes":                     {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}},{"path":["/"]}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response answers first"},
		// Sets run in order until one matches: a set that matches for
		// sure shields the sets after it, not the ones before.
		"a failing set after one that matches":                       {adapted: reachJSON(`{"match":[{"path":["/"]},{"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: ""},
		"a failing set before one that matches":                      {adapted: reachJSON(`{"match":[{"expression":{"expr":"true"}},{"path":["/"]}],"handle":[{"handler":"vars"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		"a failing matcher beside a decided no in one set":           {adapted: reachJSON(`{"match":[{"path":["/x"],"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		"a file matcher on a handler that passes":                    {adapted: reachJSON(`{"match":[{"file":{"try_files":["{path}"]}}],"handle":[{"handler":"encode"}]},`, ""), want: "the file matcher ahead of it may fail (decided per request)"},
		"a matcher the table does not know on a handler that passes": {adapted: reachJSON(`{"match":[{"geoip":{"country":["GB"]}}],"handle":[{"handler":"encode"}]},`, ""), want: "the geoip matcher ahead of it may fail (decided per request)"},
		"a failing matcher on a route a group already skips":         {adapted: reachJSON(`{"group":"g","handle":[{"handler":"vars"}]},{"group":"g","match":[{"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: ""},
		"a failing matcher on a route a group may have skipped":      {adapted: reachJSON(`{"group":"g","match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"vars"}]},{"group":"g","match":[{"expression":{"expr":"true"}}],"handle":[{"handler":"vars"}]},`, ""), want: "the expression matcher ahead of it may fail (decided per request)"},
		// Caddy's group map is one per request across nesting: a member
		// under a per-request outer route satisfies its group only for
		// the requests that outer route admits, so a sibling of the
		// group outside it still runs for the rest. (Caddy's adapter
		// never shares a group across levels; the contract is kept for
		// any bytes.)
		"a group member under an undecided outer route": {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"subroute","routes":[{"group":"g","handle":[{"handler":"vars"}]}]}]},{"group":"g","handle":[{"handler":"static_response"}]},`, ""), want: "static_response may answer first (its header matcher is decided per request)"},
		"a group member under a decided outer route":    {adapted: reachJSON(`{"match":[{"path":["/"]}],"handle":[{"handler":"subroute","routes":[{"group":"g","handle":[{"handler":"vars"}]}]}]},{"group":"g","handle":[{"handler":"static_response"}]},`, ""), want: ""},
		"a query matcher on a handler that passes":      {adapted: reachJSON(`{"match":[{"query":{"x":["1"]}}],"handle":[{"handler":"encode"}]},`, ""), want: ""},
		"an empty not": {adapted: reachJSON(`{"match":[{"not":[]}],"handle":[{"handler":"static_response"}]},`, ""), want: "static_response answers first"},
		"a terminal route inside the site that passes":            {adapted: reachJSON(`{"handle":[{"handler":"vars"}],"terminal":true},`, ""), want: "a terminal route answers first"},
		"a terminal route under an undecided matcher":             {adapted: reachJSON(`{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"vars"}],"terminal":true},`, ""), want: "a terminal route may answer first (its header matcher is decided per request)"},
		"an earlier site under an undecided matcher":              {adapted: reachJSON("", `{"match":[{"host":["deploy.example.com"],"header":{"X-A":["b"]}}],"handle":[],"terminal":true},`), want: "an earlier site for deploy.example.com may answer first (its header matcher is decided per request)"},
		"an earlier site on another host":                         {adapted: reachJSON("", `{"match":[{"host":["other.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true},`), want: ""},
		"an earlier site with a host placeholder":                 {adapted: reachJSON("", `{"match":[{"host":["{http.request.host}"]}],"handle":[{"handler":"static_response"}],"terminal":true},`), want: "static_response may answer first (its host matcher is decided per request)"},
		"a responder in a server that does not carry the webhook": {adapted: []byte(`{"apps":{"http":{"servers":{"other":{"listen":[":8443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true}]},"srv0":{"listen":[":443"],"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`), want: ""},
		"another handler's routes key, of another shape":          {adapted: reachJSON(`{"handle":[{"handler":"vars","routes":{"a":1}}]},`, ""), want: ""},
		"a subroute whose routes do not read":                     {adapted: reachJSON(`{"handle":[{"handler":"subroute","routes":{"a":1}}]},`, ""), want: "error"},
		// The webhook itself under a per-request matcher: reached for
		// some requests only, so not reached. Nothing the walk accepts
		// adapts to this; the contract is kept anyway.
		"the webhook behind a header matcher":               {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"box_webhook"}]},{"handle":[{"handler":"static_response"}]}]}],"terminal":true}]}}}}}`), want: "box_webhook stands behind a header matcher decided per request"},
		"the webhook in a subroute behind a header matcher": {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"subroute","routes":[{"match":[{"header":{"X-A":["b"]}}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}]}]}],"terminal":true}]}}}}}`), want: "box_webhook stands behind a header matcher decided per request"},
		"not JSON":                    {adapted: []byte("{"), want: "error"},
		"no box_webhook":              {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[{"handler":"static_response"}]}]}}}}}`), want: "error"},
		"the webhook on another host": {adapted: []byte(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["other.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"box_webhook"}]}]}],"terminal":true}]}}}}}`), want: "error"},
	} {
		t.Run(name, func(t *testing.T) {
			err := reach(c.adapted, "deploy.example.com", nil)
			if c.want == "error" {
				var r *refusal
				if err == nil || errors.As(err, &r) {
					t.Fatalf("got %v, want the box's own error", err)
				}
				return
			}
			wantReach(t, err, c.want)
		})
	}
}

// TestReachIsCaseInsensitive: the walk lower-cases the host; Caddy's
// host matcher does too, so a site written in capitals is still the
// box site, and still shadowed by one with a path.
func TestReachHostCase(t *testing.T) {
	in := strings.Replace(reachFile(nil, "Deploy.Example.com/* {\n\trespond x\n}\n", "", ""), "deploy.example.com {\n", "DEPLOY.example.com {\n", 1)
	wantReach(t, reachOf(t, in), "static_response answers first")
}

// TestReachDeepNesting: the adaptation is converted in one pass, so a
// chain of nested subroutes as deep as Go's JSON decoder allows is
// walked without re-reading the subtree at every level.
func TestReachDeepNesting(t *testing.T) {
	const depth = 2000 // four JSON levels a step; encoding/json stops at 10000
	var b strings.Builder
	b.WriteString(`{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["deploy.example.com"]}],"handle":[`)
	for range depth {
		b.WriteString(`{"handler":"subroute","routes":[{"handle":[`)
	}
	b.WriteString(`{"handler":"box_webhook"}`)
	for range depth {
		b.WriteString(`]}]}`)
	}
	b.WriteString(`],"terminal":true}]}}}}}`)
	start := time.Now()
	if err := reach([]byte(b.String()), "deploy.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}
