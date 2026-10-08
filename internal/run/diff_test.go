package run

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name    string
	typ     byte
	mode    int64
	body    string
	link    string
	uid     int
	mtime   int64
	xattr   string
	devMino int64
}

func writeTestTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     e.mode,
			Uid:      e.uid,
			Linkname: e.link,
			ModTime:  time.Unix(e.mtime, 0),
			Devminor: e.devMino,
			Format:   tar.FormatPAX,
		}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if e.xattr != "" {
			h.PAXRecords = map[string]string{"SCHILY.xattr.security.capability": e.xattr}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fs.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type gotEntry struct {
	typ   byte
	mode  int64
	body  string
	link  string
	xattr string
}

func readLayer(t *testing.T, data []byte, mtime time.Time) ([]string, map[string]gotEntry) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	got := map[string]gotEntry{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !h.ModTime.Equal(mtime) {
			t.Errorf("%s: mtime %v, want %v", h.Name, h.ModTime, mtime)
		}
		if h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: owner names %q %q, want numeric IDs only", h.Name, h.Uname, h.Gname)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		got[h.Name] = gotEntry{h.Typeflag, h.Mode, string(body), h.Linkname, h.PAXRecords["SCHILY.xattr.security.capability"]}
	}
	return names, got
}

func TestDiff(t *testing.T) {
	d := func(n string) tarEntry { return tarEntry{name: n, typ: tar.TypeDir, mode: 0o755} }
	f := func(n, body string) tarEntry { return tarEntry{name: n, typ: tar.TypeReg, mode: 0o644, body: body} }
	before := writeTestTar(t, []tarEntry{
		d("./"), d("./etc/"), d("./usr/"), d("./usr/bin/"), d("./var/"), d("./var/cache/"), d("./var/cache/apt/"),
		d("./proc/"), d("./opt/"),
		f("./etc/changed", "old"),
		{name: "./etc/touched", typ: tar.TypeReg, mode: 0o644, body: "same", mtime: 100},
		f("./etc/chmod", "x"),
		f("./usr/bin/gone", "bye"),
		f("./var/cache/apt/pkgcache.bin", "cache"),
		d("./var/cache/apt/archives/"),
		f("./var/cache/apt/archives/a.deb", "deb"),
		{name: "./etc/alt", typ: tar.TypeSymlink, link: "/usr/bin/a"},
		f("./usr/bin/ping", "ping"),
		f("./opt/tool", "file"),
	})
	after := writeTestTar(t, []tarEntry{
		d("./"), d("./etc/"), d("./usr/"), d("./usr/bin/"), d("./var/"), d("./var/cache/"),
		d("./proc/"), d("./proc/1/"), f("./proc/1/status", "running"),
		f("./etc/changed", "new"),
		{name: "./etc/touched", typ: tar.TypeReg, mode: 0o644, body: "same", mtime: 200},
		{name: "./etc/chmod", typ: tar.TypeReg, mode: 0o600, body: "x"},
		{name: "./etc/alt", typ: tar.TypeSymlink, link: "/usr/bin/b"},
		{name: "./usr/bin/ping", typ: tar.TypeReg, mode: 0o755, body: "ping"}, // mode change
		{name: "./usr/bin/cap", typ: tar.TypeReg, mode: 0o755, body: "c", xattr: "\x01\x00"},
		d("./usr/lib/"), d("./usr/lib/git-core/"),
		{name: "./usr/lib/git-core/git", typ: tar.TypeReg, mode: 0o755, body: "git binary"},
		{name: "./usr/bin/git", typ: tar.TypeLink, link: "./usr/lib/git-core/git"},
		d("./opt/"), d("./opt/tool/"), f("./opt/tool/run", "dir now"),
		{name: "./dev/", typ: tar.TypeDir, mode: 0o755},
	})
	mtime := time.Unix(1700000000, 0).UTC()
	var out bytes.Buffer
	st, err := Diff(before, after, &out, mtime)
	if err != nil {
		t.Fatal(err)
	}
	names, got := readLayer(t, out.Bytes(), mtime)
	want := []string{
		"dev/",
		"etc/",
		"etc/alt",
		"etc/changed",
		"etc/chmod",
		"opt/",
		"opt/tool/",
		"opt/tool/run",
		"usr/",
		"usr/bin/",
		"usr/bin/.wh.gone",
		"usr/bin/cap",
		"usr/bin/git",
		"usr/bin/ping",
		"usr/lib/",
		"usr/lib/git-core/",
		"usr/lib/git-core/git",
		"var/cache/.wh.apt",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("layer entries:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if st.Deleted != 2 || st.Changed != len(want)-2 {
		t.Errorf("stats %+v", st)
	}
	if e := got["etc/changed"]; e.body != "new" {
		t.Errorf("etc/changed = %q", e.body)
	}
	if e := got["etc/chmod"]; e.mode != 0o600 {
		t.Errorf("etc/chmod mode %o", e.mode)
	}
	if e := got["etc/alt"]; e.typ != tar.TypeSymlink || e.link != "/usr/bin/b" {
		t.Errorf("etc/alt = %+v", e)
	}
	if e := got["usr/bin/cap"]; e.xattr != "\x01\x00" {
		t.Errorf("usr/bin/cap lost its capability xattr: %+v", e)
	}
	// The hard link group keeps one copy; the first name holds it.
	if e := got["usr/bin/git"]; e.typ != tar.TypeReg || e.body != "git binary" || e.mode != 0o755 {
		t.Errorf("usr/bin/git = %+v", e)
	}
	if e := got["usr/lib/git-core/git"]; e.typ != tar.TypeLink || e.link != "usr/bin/git" {
		t.Errorf("usr/lib/git-core/git = %+v", e)
	}

	var again bytes.Buffer
	if _, err := Diff(before, after, &again, mtime); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Error("Diff is not deterministic")
	}
}

func TestDiffNoChanges(t *testing.T) {
	fsTar := writeTestTar(t, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./a", typ: tar.TypeReg, mode: 0o644, body: "a"},
	})
	var out bytes.Buffer
	st, err := Diff(fsTar, fsTar, &out, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	names, _ := readLayer(t, out.Bytes(), time.Unix(0, 0))
	if len(names) != 0 || st != (Stats{}) {
		t.Errorf("entries %v, stats %+v", names, st)
	}
}

func TestDiffBadHardLink(t *testing.T) {
	after := writeTestTar(t, []tarEntry{{name: "a", typ: tar.TypeLink, link: "missing"}})
	before := writeTestTar(t, nil)
	_, err := Diff(before, after, io.Discard, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "hard link a") {
		t.Errorf("err = %v", err)
	}
}
