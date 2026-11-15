package liveswap

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The app-name and version alphabets, and the two helpers over them.
// Every layer of the package shares this vocabulary — the unit name a
// runner builds, the directory a release lands in, the name the
// webhook echoes back — so it lives on its own rather than in the
// config file that happens to validate against it.

// appNameMaxLen bounds an app name; it is also how much of a
// request-supplied name the webhook is willing to log.
const appNameMaxLen = 63

var appNameRe = regexp.MustCompile(`^[a-z0-9-]{1,` + strconv.Itoa(appNameMaxLen) + `}$`)

// versionRe is the tag alphabet: the Nomad-era webhook's set, minus a
// leading dot. The alphabet has no path separator, so no tag can
// escape releases/; the leading dot is refused because the releases
// dir uses it for its own bookkeeping — ".extract-<version>" is the
// staging dir a deploy renames into place, and release GC removes any
// ".extract-*" it finds as a crash orphan and skips every other
// dot-entry as a stray. A version named ".extract-1" would therefore
// be deleted by the GC of the deploy that installed it, and ".v1"
// would never be listed or pruned. "." and ".." fall out of the same
// rule (both would resolve releases/<version> onto the releases dir
// itself or the app root — where shared/ lives).
var versionRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

// sha256Re is a pull's artifact pin as sha256sum prints it, in either
// case (parseDeployPayload lowercases what it accepts). Nothing else:
// no `sha256:` prefix, no other algorithm.
var sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// versionPathComponent renders a version tag safe to use as a single
// path component, mechanically: rooting the string at "/" and cleaning
// resolves any "..", and the leading separator is then stripped. For
// every tag validVersion accepts this is the identity function (the
// tag alphabet contains no separators), so it changes no behavior —
// it is belt-and-braces beneath validVersion, which remains the real
// gate. It also uses the exact idiom static analysis models as a
// path-traversal sanitizer (CodeQL's FilepathCleanSanitizer), so the
// version->filesystem flows stop lighting up go/path-injection at
// every join downstream. (Tested for both properties.)
func versionPathComponent(v string) string {
	return strings.TrimPrefix(filepath.Clean("/"+v), "/")
}

// validVersion reports whether v is an acceptable version tag: the
// gate every version passes before it reaches a filesystem path (see
// versionRe for what the alphabet refuses and why). FuzzDeployRequest
// pins the filesystem property behind it.
func validVersion(v string) bool {
	return versionRe.MatchString(v)
}

// ValidVersion is validVersion for another package: notify's
// `motel monitor silence add --release` takes a release by this grammar.
func ValidVersion(v string) bool { return validVersion(v) }
