package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rootfsEntries(t *testing.T, r Rootfs) map[string]tarEntry {
	t.Helper()
	ls, err := r.Layer()
	if err != nil {
		t.Fatal(err)
	}
	data, err := layerTar(ls.Items, time.Unix(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]tarEntry{}
	for _, e := range readTar(t, bytes.NewReader(data)) {
		out[e.name] = e
	}
	return out
}

func TestRootfsSkeletonAndUsers(t *testing.T) {
	got := rootfsEntries(t, Rootfs{Skeleton: true, Users: []string{"app:1000", "svc:2000:1000:/srv/svc"}})
	for name, want := range map[string]struct {
		typ      byte
		mode     int64
		uid, gid int
	}{
		"etc/":          {tar.TypeDir, 0o755, 0, 0},
		"root/":         {tar.TypeDir, 0o700, 0, 0},
		"tmp/":          {tar.TypeDir, 0o1777, 0, 0},
		"var/tmp/":      {tar.TypeDir, 0o1777, 0, 0},
		"home/nonroot/": {tar.TypeDir, 0o700, 65532, 65532},
		"home/app/":     {tar.TypeDir, 0o700, 1000, 1000},
		"srv/":          {tar.TypeDir, 0o755, 0, 0},
		"srv/svc/":      {tar.TypeDir, 0o700, 2000, 1000},
		"etc/passwd":    {tar.TypeReg, 0o644, 0, 0},
		"etc/group":     {tar.TypeReg, 0o644, 0, 0},
	} {
		e, ok := got[name]
		if !ok || e.typ != want.typ || e.mode != want.mode || e.uid != want.uid || e.gid != want.gid {
			t.Errorf("%s = %+v (present %v), want %+v", name, e, ok, want)
		}
	}
	passwd := "root:x:0:0:root:/root:/sbin/nologin\n" +
		"nobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin\n" +
		"nonroot:x:65532:65532:nonroot:/home/nonroot:/sbin/nologin\n" +
		"app:x:1000:1000:app:/home/app:/sbin/nologin\n" +
		"svc:x:2000:1000:svc:/srv/svc:/sbin/nologin\n"
	if got["etc/passwd"].body != passwd {
		t.Errorf("passwd:\n%s", got["etc/passwd"].body)
	}
	// svc shares app's group, so it gets no group line of its own.
	group := "root:x:0:\nnobody:x:65534:\nnonroot:x:65532:\napp:x:1000:\n"
	if got["etc/group"].body != group {
		t.Errorf("group:\n%s", got["etc/group"].body)
	}
	if _, ok := got["home/nobody/"]; ok {
		t.Error("nobody got a home directory")
	}
}

func TestRootfsUsersOnly(t *testing.T) {
	got := rootfsEntries(t, Rootfs{Users: []string{"app:1000"}})
	for _, name := range []string{"etc/", "etc/passwd", "etc/group", "home/", "home/app/", "root/"} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	if _, ok := got["tmp/"]; ok {
		t.Error("users without skeleton added /tmp")
	}
	if strings.Contains(got["etc/passwd"].body, "nonroot") {
		t.Error("users without skeleton added nonroot")
	}
}

func TestRootfsHostFiles(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "ca.pem"), "PEM", 0o600)
	zone := filepath.Join(src, "zoneinfo")
	writeFile(t, filepath.Join(zone, "UTC"), "TZif-utc", 0o600)
	writeFile(t, filepath.Join(zone, "Europe", "Paris"), "TZif-paris", 0o600)
	// macOS reaches zoneinfo through a symlink; it must be followed.
	link := filepath.Join(src, "zonelink")
	if err := os.Symlink(zone, link); err != nil {
		t.Fatal(err)
	}

	got := rootfsEntries(t, Rootfs{CACerts: filepath.Join(src, "ca.pem"), Tzdata: link})
	for name, want := range map[string]struct {
		mode int64
		body string
	}{
		"etc/ssl/certs/ca-certificates.crt": {0o644, "PEM"},
		"usr/share/zoneinfo/UTC":            {0o644, "TZif-utc"},
		"usr/share/zoneinfo/Europe/Paris":   {0o644, "TZif-paris"},
		"usr/share/zoneinfo/Europe/":        {0o755, ""},
		"etc/ssl/certs/":                    {0o755, ""},
		"usr/":                              {0o755, ""},
	} {
		if e, ok := got[name]; !ok || e.mode != want.mode || e.body != want.body {
			t.Errorf("%s = %+v (present %v), want %+v", name, e, ok, want)
		}
	}
	if _, ok := got["etc/passwd"]; ok {
		t.Error("ca-certs alone wrote /etc/passwd")
	}
}

func TestRootfsSystem(t *testing.T) {
	ls, err := Rootfs{CACerts: System, Tzdata: System}.Layer()
	if err != nil {
		t.Skipf("no system CA bundle or zoneinfo here: %v", err)
	}
	data, err := layerTar(ls.Items, time.Unix(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range readTar(t, bytes.NewReader(data)) {
		got[e.name] = true
	}
	if !got["etc/ssl/certs/ca-certificates.crt"] || !got["usr/share/zoneinfo/UTC"] {
		t.Errorf("system rootfs lacks the CA bundle or UTC zone (%d entries)", len(got))
	}
}

func TestRootfsErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "file"), "x", 0o644)
	for name, r := range map[string]Rootfs{
		"bad user":          {Users: []string{"App:1"}},
		"no uid":            {Users: []string{"app"}},
		"bad uid":           {Users: []string{"app:x"}},
		"relative home":     {Users: []string{"app:1:1:home"}},
		"root clash":        {Users: []string{"admin:0"}},
		"nonroot clash":     {Skeleton: true, Users: []string{"nonroot:5"}},
		"duplicate":         {Users: []string{"a:5", "b:5"}},
		"missing ca":        {CACerts: filepath.Join(dir, "missing")},
		"ca is a directory": {CACerts: dir},
		"tz is a file":      {Tzdata: filepath.Join(dir, "file")},
	} {
		if _, err := r.Layer(); err == nil {
			t.Errorf("%s: Layer succeeded", name)
		}
	}
	if (Rootfs{}).Empty() != true || (Rootfs{Tzdata: System}).Empty() || (Rootfs{CACerts: System}).WritesUsers() {
		t.Error("Empty/WritesUsers wrong")
	}
}
