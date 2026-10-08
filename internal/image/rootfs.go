package image

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// System asks Rootfs to take CA certificates or time zone data from the
// build machine.
const System = "system"

// Paths where the CA bundle and time zone data are written in the image.
// Go, OpenSSL on Debian, and glibc look in these places.
const (
	CACertsPath = "/etc/ssl/certs/ca-certificates.crt"
	ZoneinfoDir = "/usr/share/zoneinfo"
)

// Host locations probed for System, in order.
var (
	systemCACerts = []string{
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine, Arch
		"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
		"/etc/ssl/ca-bundle.pem",             // openSUSE
		"/etc/ssl/cert.pem",                  // macOS, Alpine
	}
	systemZoneinfo = []string{"/usr/share/zoneinfo", "/usr/lib/zoneinfo", "/usr/share/lib/zoneinfo"}
)

// Rootfs describes the files a minimal image needs that construct can
// generate: the usual directory layout, user and group databases, CA
// certificates, and time zone data. It becomes one layer below the
// image's own layers.
type Rootfs struct {
	// Skeleton adds /etc, /home, /root, /tmp, /var, and /var/tmp with the
	// usual modes, /etc/nsswitch.conf, and a nonroot user (65532).
	Skeleton bool
	// Users are NAME:UID[:GID[:HOME]] entries for /etc/passwd and
	// /etc/group, each with its own group unless GID names an existing
	// one. Homes default to /home/NAME and are created owned by the user.
	Users []string
	// CACerts is a PEM bundle on the host, or System.
	CACerts string
	// Tzdata is a zoneinfo directory on the host, or System.
	Tzdata string
}

// Empty reports whether r adds nothing.
func (r Rootfs) Empty() bool {
	return !r.Skeleton && len(r.Users) == 0 && r.CACerts == "" && r.Tzdata == ""
}

// WritesUsers reports whether r generates /etc/passwd and /etc/group,
// which replace any the base image has.
func (r Rootfs) WritesUsers() bool { return r.Skeleton || len(r.Users) > 0 }

type account struct {
	name     string
	uid, gid int
	home     string
	makeHome bool // create home, owned by the user
}

var accountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ParseUser parses NAME:UID[:GID[:HOME]].
func ParseUser(s string) (name string, uid, gid int, home string, err error) {
	parts := strings.Split(s, ":")
	bad := fmt.Errorf("user %q: want NAME:UID[:GID[:HOME]] with a lowercase NAME and numeric IDs", s)
	if len(parts) < 2 || len(parts) > 4 || !accountName.MatchString(parts[0]) {
		return "", 0, 0, "", bad
	}
	name = parts[0]
	if uid, err = strconv.Atoi(parts[1]); err != nil || uid < 0 {
		return "", 0, 0, "", bad
	}
	gid = uid
	if len(parts) > 2 && parts[2] != "" {
		if gid, err = strconv.Atoi(parts[2]); err != nil || gid < 0 {
			return "", 0, 0, "", bad
		}
	}
	home = "/home/" + name
	if len(parts) > 3 {
		if !path.IsAbs(parts[3]) {
			return "", 0, 0, "", fmt.Errorf("user %q: home must be an absolute path", s)
		}
		home = path.Clean(parts[3])
	}
	return name, uid, gid, home, nil
}

// Layer returns the rootfs layer. Host files are located now, so a
// missing CA bundle or zoneinfo directory is reported before building.
func (r Rootfs) Layer() (LayerSpec, error) {
	ls := LayerSpec{Name: "rootfs"}
	dirs := map[string]Item{}
	dir := func(p string, mode int64, owner *Owner) {
		dirs[p] = Item{Kind: Dir, Dst: p, Mode: &mode, Owner: owner}
	}
	var files []Item

	if r.Skeleton {
		for p, m := range map[string]int64{"/etc": 0o755, "/home": 0o755, "/root": 0o700, "/tmp": 0o1777, "/var": 0o755, "/var/tmp": 0o1777} {
			dir(p, m, nil)
		}
		files = append(files, Item{Kind: File, Dst: "/etc/nsswitch.conf", Content: []byte("hosts: files dns\n")})
	}

	if r.WritesUsers() {
		accounts := []account{{"root", 0, 0, "/root", false}, {"nobody", 65534, 65534, "/nonexistent", false}}
		if r.Skeleton {
			accounts = append(accounts, account{"nonroot", 65532, 65532, "/home/nonroot", true})
		}
		for _, u := range r.Users {
			name, uid, gid, home, err := ParseUser(u)
			if err != nil {
				return ls, err
			}
			for _, a := range accounts {
				if a.name == name || a.uid == uid {
					return ls, fmt.Errorf("user %q: clashes with existing user %s (%d)", u, a.name, a.uid)
				}
			}
			accounts = append(accounts, account{name, uid, gid, home, home != "/nonexistent"})
		}
		var passwd, group strings.Builder
		groups := map[int]bool{}
		for _, a := range accounts {
			fmt.Fprintf(&passwd, "%s:x:%d:%d:%s:%s:/sbin/nologin\n", a.name, a.uid, a.gid, a.name, a.home)
			if !groups[a.gid] {
				groups[a.gid] = true
				fmt.Fprintf(&group, "%s:x:%d:\n", a.name, a.gid)
			}
			if a.makeHome {
				for p := path.Dir(a.home); p != "/"; p = path.Dir(p) {
					if _, ok := dirs[p]; !ok {
						dir(p, 0o755, nil)
					}
				}
				dir(a.home, 0o700, &Owner{a.uid, a.gid})
			}
		}
		if _, ok := dirs["/root"]; !ok {
			dir("/root", 0o700, nil)
		}
		files = append(files,
			Item{Kind: File, Dst: "/etc/passwd", Content: []byte(passwd.String())},
			Item{Kind: File, Dst: "/etc/group", Content: []byte(group.String())},
		)
	}

	if r.CACerts != "" {
		src, err := hostPath(r.CACerts, systemCACerts, false)
		if err != nil {
			return ls, fmt.Errorf("ca-certs: %w", err)
		}
		mode := int64(0o644)
		files = append(files, Item{Kind: Copy, Src: src, Dst: CACertsPath, Mode: &mode})
	}
	if r.Tzdata != "" {
		src, err := hostPath(r.Tzdata, systemZoneinfo, true)
		if err != nil {
			return ls, fmt.Errorf("tzdata: %w", err)
		}
		mode, dirMode := int64(0o644), int64(0o755)
		files = append(files, Item{Kind: Copy, Src: src, Dst: ZoneinfoDir, Mode: &mode, DirMode: &dirMode})
	}

	// A scratch image has no directories at all, so every parent of a
	// generated file is created too.
	for _, f := range files {
		for p := path.Dir(f.Dst); p != "/"; p = path.Dir(p) {
			if _, ok := dirs[p]; !ok {
				dir(p, 0o755, nil)
			}
		}
	}
	names := make([]string, 0, len(dirs))
	for p := range dirs {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		ls.Items = append(ls.Items, dirs[p])
	}
	ls.Items = append(ls.Items, files...)
	return ls, nil
}

// hostPath resolves p, or the first existing candidate for System, to a
// real path of the wanted type. Symlinks are followed because macOS
// keeps zoneinfo behind one.
func hostPath(p string, candidates []string, wantDir bool) (string, error) {
	if p == System {
		for _, c := range candidates {
			if real, err := hostPath(c, nil, wantDir); err == nil {
				return real, nil
			}
		}
		return "", fmt.Errorf("none found on this machine (looked in %s); give a path", strings.Join(candidates, ", "))
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if info.IsDir() != wantDir {
		if wantDir {
			return "", fmt.Errorf("%s is not a directory", p)
		}
		return "", fmt.Errorf("%s is a directory", p)
	}
	return real, nil
}
