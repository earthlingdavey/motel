package liveswap

import (
	"math"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"

	"github.com/smallhoursorg/motel/liveswap/internal/dispenser"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("liveswap", parseGlobalOption)
	httpcaddyfile.RegisterHandlerDirective("liveswap_webhook", parseWebhookDirective)
	// The webhook is terminal, but give it a sane default position for
	// site blocks that mix it with other directives.
	httpcaddyfile.RegisterDirectiveOrder("liveswap_webhook", httpcaddyfile.Before, "reverse_proxy")
}

func parseGlobalOption(d *caddyfile.Dispenser, _ any) (any, error) {
	a := new(App)
	if err := a.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  "liveswap",
		Value: caddyconfig.JSON(a, nil),
	}, nil
}

func parseWebhookDirective(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}

// UnmarshalCaddyfile parses the global options block. No defaults are
// applied here — Provision owns every default — so an empty block is
// valid. Provision reads a zero field as unset, so an explicit value
// below an option's minimum, 0 included, is refused here, naming both
// (parseDurationArg, parseCountArg); soak and deploy_log_lines, whose 0
// is a setting, are pointers.
//
//	liveswap {
//	    root                   <path>
//	    deploy_trust <preset>  { ... }   # who may deploy (repeatable)
//	    allow_insecure_http
//	    allow_tcp_admin                   # test rigs: the admin API on TCP is meant
//	    artifact_allowlist     <host[:port][/path/][?param&param...]...>
//	    app <name> {
//	        command           <cmd> [args...]
//	        pre_start         <cmd> [args...]
//	        env               <KEY> <value>
//	        env_file          <path>
//	        deploy_trust <preset> { ... }  # overrides the global default
//	        artifact_allowlist <host[:port][/path/][?param&param...]...>
//	        health_path       <path|off>
//	        health_interval   <duration>
//	        health_timeout    <duration>
//	        soak              <duration>
//	        deadline          <duration>
//	        drain             <duration>
//	        grace             <duration>
//	        watchdog          <on|off>
//	        watchdog_failures <n>
//	        watchdog_grace    <duration>
//	        watchdog_restarts <n>
//	        watchdog_window   <duration>
//	        keep              <n>
//	        max_artifact_size <size>
//	        max_artifact_entries <n>
//	        deploy_log_lines  <n>
//	        backup {
//	            sqlite <path>...             # relative to the shared dir
//	            files  <path>...
//	        }
//	        memory_max        <size>
//	        tasks_max         <n>
//	        cpu_quota         <n>%
//	    }
//	}
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume option name
	if d.NextArg() {
		return d.ArgErr() // no positional args; everything is in the block
	}
	var found []BackupSource
	rootFile := ""
	seen := map[string]bool{}
	for d.NextBlock(0) {
		if err := dispenser.RefuseRepeat(d, seen, "deploy_trust", "artifact_allowlist", "app"); err != nil {
			return err
		}
		switch d.Val() {
		case "root":
			rootFile = d.File()
			if !d.NextArg() {
				return d.ArgErr()
			}
			a.Root = d.Val()
		case "deploy_trust":
			tc, err := deploytrust.Parse(d)
			if err != nil {
				return err
			}
			a.DeployTrust = append(a.DeployTrust, tc)
			continue // the block consumed its own trailing tokens
		case "allow_insecure_http":
			a.AllowInsecureHTTP = true
		case "allow_tcp_admin":
			a.AllowTCPAdmin = true
		case "artifact_allowlist":
			entries := d.RemainingArgs()
			if len(entries) == 0 {
				return d.ArgErr()
			}
			a.ArtifactAllowlist = append(a.ArtifactAllowlist, entries...)
		case "allowed_artifact_hosts":
			return d.Errf("allowed_artifact_hosts was replaced by artifact_allowlist; entries may now pin a path prefix (github.com/your-org/), which is the recommended form on multi-tenant hosts")
		case "app":
			if !d.NextArg() {
				return d.ArgErr()
			}
			name, appFile := d.Val(), d.File()
			if a.Apps == nil {
				a.Apps = make(map[string]*AppConfig)
			}
			if _, dup := a.Apps[name]; dup {
				return d.Errf("duplicate app %q", name)
			}
			cfg := new(AppConfig)
			var backupFiles []string
			if err := cfg.unmarshalBlock(d, &backupFiles); err != nil {
				return err
			}
			a.Apps[name] = cfg
			if cfg.Backup != nil {
				for _, f := range append([]string{appFile}, backupFiles...) {
					found = append(found, BackupSource{App: name, File: f})
				}
			}
			continue // the app block consumed its own trailing tokens
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
		if d.NextArg() {
			return d.ArgErr() // no simple subdirective takes more args
		}
	}
	if len(found) > 0 && rootFile != "" {
		found = append([]BackupSource{{File: rootFile}}, found...)
	}
	setBackupSources(found)
	return nil
}

// unmarshalBlock parses an app's block; backupFiles gets the file each
// token of its backup block was read from.
func (cfg *AppConfig) unmarshalBlock(d *caddyfile.Dispenser, backupFiles *[]string) error {
	seen := map[string]bool{}
	for d.NextBlock(1) {
		// backup refuses its own repeat, in its own words (case "backup").
		if err := dispenser.RefuseRepeat(d, seen, "env", "deploy_trust", "artifact_allowlist", "backup"); err != nil {
			return err
		}
		switch d.Val() {
		case "command":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			cfg.Command = args
			continue
		case "pre_start":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			cfg.PreStart = args
			continue
		case "env":
			if !d.NextArg() {
				return d.ArgErr()
			}
			key := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			// Checked here as well as in Validate, because Caddy strips
			// every "@id" key from the JSON before decoding it
			// (caddy.RemoveMetaFields): Validate never sees that one, and
			// the app would start without it. A config written as JSON
			// loses the key before any motel code runs, which is why
			// a JSON config is unsupported (README.md, "What motel is
			// (and isn't)"; #251).
			if !validEnvKey(key) {
				return d.Errf("env key %q is not a valid environment variable name (must match %s)", key, envKeyRe)
			}
			if shellOwnedEnvKey(key) {
				return d.Errf("env key %q %s", key, shellOwnedEnvKeyMsg)
			}
			if cfg.Env == nil {
				cfg.Env = make(map[string]string)
			}
			if _, dup := cfg.Env[key]; dup {
				return d.Errf("duplicate env %q", key)
			}
			cfg.Env[key] = d.Val()
		case "env_file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.EnvFile = d.Val()
		case "deploy_trust":
			tc, err := deploytrust.Parse(d)
			if err != nil {
				return err
			}
			cfg.DeployTrust = append(cfg.DeployTrust, tc)
			continue
		case "artifact_allowlist":
			entries := d.RemainingArgs()
			if len(entries) == 0 {
				return d.ArgErr()
			}
			cfg.ArtifactAllowlist = append(cfg.ArtifactAllowlist, entries...)
			continue
		case "health_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.HealthPath = d.Val()
		case "health_interval":
			if err := parseDurationArg(d, &cfg.HealthInterval, false); err != nil {
				return err
			}
		case "health_timeout":
			if err := parseDurationArg(d, &cfg.HealthTimeout, false); err != nil {
				return err
			}
		case "soak":
			var soak caddy.Duration
			if err := parseDurationArg(d, &soak, true); err != nil {
				return err
			}
			cfg.Soak = &soak
		case "deadline":
			if err := parseDurationArg(d, &cfg.Deadline, false); err != nil {
				return err
			}
		case "drain":
			if err := parseDurationArg(d, &cfg.Drain, false); err != nil {
				return err
			}
		case "grace":
			if err := parseDurationArg(d, &cfg.Grace, false); err != nil {
				return err
			}
		case "watchdog":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.Watchdog = d.Val()
		case "watchdog_failures":
			if err := parseCountArg(d, &cfg.WatchdogFailures, 1); err != nil {
				return err
			}
		case "watchdog_grace":
			if err := parseDurationArg(d, &cfg.WatchdogGrace, false); err != nil {
				return err
			}
		case "watchdog_restarts":
			if err := parseCountArg(d, &cfg.WatchdogRestarts, 1); err != nil {
				return err
			}
		case "watchdog_window":
			if err := parseDurationArg(d, &cfg.WatchdogWindow, false); err != nil {
				return err
			}
		case "deploy_log_lines":
			var n int
			if err := parseCountArg(d, &n, 0); err != nil {
				return err
			}
			cfg.DeployLogLines = &n
		case "keep":
			if err := parseCountArg(d, &cfg.Keep, 1); err != nil {
				return err
			}
		case "max_artifact_size":
			if !d.NextArg() {
				return d.ArgErr()
			}
			size, err := humanize.ParseBytes(d.Val())
			if err != nil {
				return d.Errf("invalid max_artifact_size %q: %v", d.Val(), err)
			}
			if size > math.MaxInt64 {
				return d.Errf("max_artifact_size %q overflows", d.Val())
			}
			if size == 0 {
				return d.Errf("max_artifact_size must be at least 1 byte, got %s; leave it out for the default", d.Val())
			}
			cfg.MaxArtifactSize = int64(size)
		case "max_artifact_entries":
			if err := parseCountArg(d, &cfg.MaxArtifactEntries, 1); err != nil {
				return err
			}
		case "backup":
			if cfg.Backup != nil {
				return d.Err("duplicate backup block")
			}
			*backupFiles = append(*backupFiles, d.File())
			b, err := parseBackup(d, backupFiles)
			if err != nil {
				return err
			}
			cfg.Backup = b
			continue // the block consumed its own trailing tokens
		case "memory_max":
			if !d.NextArg() {
				return d.ArgErr()
			}
			size, err := humanize.ParseBytes(d.Val())
			if err != nil {
				return d.Errf("invalid memory_max %q: %v", d.Val(), err)
			}
			if size < minMemoryMax {
				return d.Errf("memory_max must be at least %s, got %s; leave it out for no cap", humanize.Bytes(minMemoryMax), d.Val())
			}
			cfg.MemoryMax = size
		case "tasks_max":
			// 2, not 1: the sibling every unit carries (withSibling) is
			// one task, so a cap of 1 refuses the fork of the sibling
			// and the shell dies before the app runs.
			if err := parseCountArg(d, &cfg.TasksMax, 2); err != nil {
				return err
			}
		case "cpu_quota":
			if err := parsePercentArg(d, &cfg.CPUQuota); err != nil {
				return err
			}
		default:
			return d.Errf("unknown app subdirective %q", d.Val())
		}
		if d.NextArg() {
			return d.ArgErr() // no subdirective takes more args than consumed
		}
	}
	return nil
}

// parseCountArg reads one integer and refuses one below least, naming
// the option and its minimum. least is 1 wherever Provision reads 0 as
// unset, since an explicit 0 there would become the default without a
// word; it is 0 for deploy_log_lines, whose 0 is a setting.
func parseCountArg(d *caddyfile.Dispenser, out *int, least int) error {
	name := d.Val()
	if !d.NextArg() {
		return d.ArgErr()
	}
	n, err := strconv.Atoi(d.Val())
	if err != nil {
		return d.Errf("invalid %s %q: %v", name, d.Val(), err)
	}
	if n < least {
		return d.Errf("%s must be at least %d, got %d; leave it out for the default", name, least, n)
	}
	*out = n
	return nil
}

// The bounds of the two caps whose value is a size or a percentage.
// A memory_max under a megabyte is a unit nothing can exec in (the
// kernel rounds memory.max to pages, and 1 byte is 0 of them), which
// would be OOM-killed at every launch with nothing naming the cause; a
// cpu_quota over a thousand CPUs' worth is more than any box, so it is
// a mistyped line, refused by name rather than accepted as no cap at
// all (the arithmetic is nowhere near overflow: 100000% is 1e9 µs).
const (
	minMemoryMax = 1_000_000 // 1MB, in the SI units humanize parses
	maxCPUQuota  = 100_000   // percent: 1000 CPUs' worth
)

// parsePercentArg reads one percentage written as systemd writes a CPU
// quota — `200%`, two CPUs' worth — and refuses anything else, the
// bare number included: a quota without its % reads as a count of
// something, and 2 would be two percent of one CPU.
func parsePercentArg(d *caddyfile.Dispenser, out *int) error {
	name := d.Val()
	if !d.NextArg() {
		return d.ArgErr()
	}
	v := d.Val()
	n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
	if !strings.HasSuffix(v, "%") || err != nil {
		return d.Errf("invalid %s %q: a percentage of one CPU, like 200%% for two CPUs' worth", name, v)
	}
	if n < 1 {
		return d.Errf("%s must be at least 1%%, got %s; leave it out for no cap", name, v)
	}
	if n > maxCPUQuota {
		return d.Errf("%s must be at most %d%% (a thousand CPUs' worth), got %s", name, maxCPUQuota, v)
	}
	*out = n
	return nil
}

// parseDurationArg reads one duration and refuses one below the
// option's minimum, naming both: more than 0, since Provision reads a
// zero duration as unset and an explicit 0 would become the default
// without a word, or 0 itself where 0 is a setting (zeroIsSetting:
// soak's "no soak").
func parseDurationArg(d *caddyfile.Dispenser, out *caddy.Duration, zeroIsSetting bool) error {
	name := d.Val()
	if !d.NextArg() {
		return d.ArgErr()
	}
	dur, err := caddy.ParseDuration(d.Val())
	if err != nil {
		return d.Errf("invalid %s %q: %v", name, d.Val(), err)
	}
	switch {
	case zeroIsSetting && dur < 0:
		return d.Errf("%s must be 0 or more, got %s", name, d.Val())
	case !zeroIsSetting && dur <= 0:
		return d.Errf("%s must be more than 0, got %s; leave it out for the default", name, d.Val())
	}
	*out = caddy.Duration(dur)
	return nil
}

// parseBackup parses an app's `backup` block. Each line names one or
// more paths and may repeat; what makes a declaration valid is
// backupdecl's to say (App.Validate), so a block that names nothing
// parses, and is refused there.
//
//	backup {
//	    sqlite app.db
//	    files  uploads
//	}
func parseBackup(d *caddyfile.Dispenser, files *[]string) (*backupdecl.Config, error) {
	if d.NextArg() {
		return nil, d.ArgErr() // no positional args; everything is in the block
	}
	b := new(backupdecl.Config)
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		kind := d.Val()
		*files = append(*files, d.File())
		var into *[]string
		switch kind {
		case "sqlite":
			into = &b.SQLite
		case "files":
			into = &b.Files
		default:
			return nil, d.Errf("unknown backup subdirective %q", kind)
		}
		paths := d.RemainingArgs()
		if len(paths) == 0 {
			return nil, d.ArgErr()
		}
		*into = append(*into, paths...)
	}
	return b, nil
}

// UnmarshalCaddyfile parses the webhook directive, which takes no
// arguments and no block:
//
//	liveswap_webhook
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Err("liveswap_webhook takes no block")
	}
	return nil
}

// UnmarshalCaddyfile parses the dynamic upstream source:
//
//	dynamic liveswap <app>
func (u *Upstreams) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume module name
	if !d.NextArg() {
		return d.ArgErr()
	}
	u.App = d.Val()
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Err("dynamic liveswap takes no block")
	}
	return nil
}

// Interface guards.
var (
	_ caddyfile.Unmarshaler = (*App)(nil)
	_ caddyfile.Unmarshaler = (*Handler)(nil)
	_ caddyfile.Unmarshaler = (*Upstreams)(nil)
)
