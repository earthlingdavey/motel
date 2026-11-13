package notify

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
)

// motelServiceGroups are the groups motel.service adds to the motel
// user's own (packaging/motel.service, SupplementaryGroups=): what the
// server may read through, beside them.
var motelServiceGroups = []string{"systemd-journal"}

// motelUser is the uid motel serves as, and the groups it serves with:
// its own and the unit's. ok is false on a host with no motel user (a
// test, a build host), where nothing is checked. A variable, so a test
// can stand one in.
var motelUser = func() (uid int, gids []int, ok bool) {
	u, err := user.Lookup("motel")
	if err != nil {
		return 0, nil, false
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, nil, false
	}
	ids, err := u.GroupIds()
	if err != nil {
		ids = []string{u.Gid}
	}
	for _, name := range motelServiceGroups {
		if g, err := user.LookupGroup(name); err == nil {
			ids = append(ids, g.Gid)
		}
	}
	for _, s := range ids {
		if g, err := strconv.Atoi(s); err == nil {
			gids = append(gids, g)
		}
	}
	return uid, gids, true
}

// motelCannotRead is why the motel user could not read the secrets
// file at path, or nil: the file read and each directory above it
// searched, by owner, group and mode, as the kernel judges an open by
// that uid and those groups. An ACL is not read (the box sets none on
// /etc/motel). A missing file is resolve's to name, key by key; a path
// validate cannot stat is left to the server's own read.
func motelCannotRead(path string) error {
	uid, gids, ok := motelUser()
	if !ok || os.Geteuid() == uid {
		return nil // motel's own read above has judged it
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	if !may(fi, uid, gids, 4) {
		return fmt.Errorf("the motel user cannot read the box's secrets file %s (%s): a reload would be refused and a restart would drop every channel; chown root:motel %s && chmod 0640 %s", path, modeText(fi), path, path)
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		di, err := os.Stat(dir)
		if err != nil {
			return nil
		}
		if !may(di, uid, gids, 1) {
			return fmt.Errorf("the motel user cannot reach the box's secrets file %s: it may not enter %s (%s): a reload would be refused and a restart would drop every channel; the box makes /etc/motel/secrets 2750 root:motel", path, dir, modeText(di))
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

// may reports whether uid, with gids, has the permission bit want (4
// read, 1 search) on fi by its owner, group and mode.
func may(fi fs.FileInfo, uid int, gids []int, want fs.FileMode) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	perm := fi.Mode().Perm()
	switch {
	case int(st.Uid) == uid:
		return perm&(want<<6) != 0
	case slices.Contains(gids, int(st.Gid)):
		return perm&(want<<3) != 0
	}
	return perm&want != 0
}

// modeText is fi's owner, group and mode, as `owner:group 0640`.
func modeText(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("%04o", fi.Mode().Perm())
	}
	owner, group := strconv.Itoa(int(st.Uid)), strconv.Itoa(int(st.Gid))
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return fmt.Sprintf("%s:%s %04o", owner, group, fi.Mode().Perm())
}
