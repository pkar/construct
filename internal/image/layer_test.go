package image

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestParseAdd(t *testing.T) {
	good, err := ParseAdd("bin/app:/usr/local/bin/app")
	if err != nil || good != (Add{Src: "bin/app", Dst: "/usr/local/bin/app"}) {
		t.Fatalf("ParseAdd = %+v, %v", good, err)
	}
	for _, bad := range []string{"", "app", ":/app", "app:", "app:relative"} {
		if _, err := ParseAdd(bad); err == nil {
			t.Errorf("ParseAdd(%q) succeeded, want error", bad)
		}
	}
}

// layerTar returns the uncompressed tar stream for adds.
func layerTar(adds []Add, mtime time.Time) ([]byte, error) {
	entries, err := collectEntries(adds)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeTar(&buf, entries, mtime); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type tarEntry struct {
	name, link, body string
	typ              byte
	mode             int64
}

// readTar returns the entries of r, checking root ownership and mtime 42.
func readTar(t *testing.T, r io.Reader) []tarEntry {
	t.Helper()
	return readTarEntries(t, r, true)
}

func readTarNoChecks(t *testing.T, r io.Reader) []tarEntry {
	t.Helper()
	return readTarEntries(t, r, false)
}

func readTarEntries(t *testing.T, r io.Reader, check bool) []tarEntry {
	t.Helper()
	var out []tarEntry
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if check && (hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "") {
			t.Errorf("%s: owner %d:%d %q:%q, want root", hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
		}
		if check && !hdr.ModTime.Equal(time.Unix(42, 0)) {
			t.Errorf("%s: mtime %v, want 42", hdr.Name, hdr.ModTime)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tarEntry{name: hdr.Name, link: hdr.Linkname, body: string(body), typ: hdr.Typeflag, mode: hdr.Mode})
	}
}

func writeFile(t *testing.T, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; set the mode the test expects.
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLayerContents(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "app"), "binary", 0o755)
	writeFile(t, filepath.Join(src, "static", "index.html"), "<h1>hi</h1>", 0o644)
	if err := os.Symlink("index.html", filepath.Join(src, "static", "home.html")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "config.json"), "{}", 0o600)

	adds := []Add{
		{Src: filepath.Join(src, "app"), Dst: "/usr/local/bin/app"},
		{Src: filepath.Join(src, "static"), Dst: "/srv/www"},
		{Src: filepath.Join(src, "config.json"), Dst: "/etc/app/"},
	}
	data, err := layerTar(adds, time.Unix(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := readTar(t, bytes.NewReader(data))

	want := []tarEntry{
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/app/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/app/config.json", typ: tar.TypeReg, mode: 0o600, body: "{}"},
		{name: "srv/", typ: tar.TypeDir, mode: 0o755},
		{name: "srv/www/", typ: tar.TypeDir, mode: dirMode(t, filepath.Join(src, "static"))},
		{name: "srv/www/home.html", typ: tar.TypeSymlink, mode: linkMode(t, filepath.Join(src, "static", "home.html")), link: "index.html"},
		{name: "srv/www/index.html", typ: tar.TypeReg, mode: 0o644, body: "<h1>hi</h1>"},
		{name: "usr/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/local/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/local/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/local/bin/app", typ: tar.TypeReg, mode: 0o755, body: "binary"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("layer entries:\n got %+v\nwant %+v", got, want)
	}
}

func dirMode(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int64(fi.Mode().Perm())
}

func linkMode(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int64(fi.Mode().Perm())
}

func TestLayerDeterministic(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "a", 0o644)
	writeFile(t, filepath.Join(src, "b", "c"), "c", 0o644)
	adds := []Add{{Src: src, Dst: "/data"}}

	digest := func() string {
		l, err := Layer(adds, time.Unix(42, 0), types.OCILayer)
		if err != nil {
			t.Fatal(err)
		}
		d, err := l.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return d.String()
	}
	first := digest()
	// Touch the files; content-identical inputs must not change the digest.
	later := time.Now().Add(time.Hour)
	for _, p := range []string{filepath.Join(src, "a"), filepath.Join(src, "b", "c")} {
		if err := os.Chtimes(p, later, later); err != nil {
			t.Fatal(err)
		}
	}
	if second := digest(); first != second {
		t.Errorf("digest changed between builds: %s != %s", first, second)
	}
}

// Layers are streamed from disk on every read rather than buffered, so a
// source that shrinks after Layer is called must fail the read loudly.
func TestLayerStreamsFromDisk(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big")
	writeFile(t, src, string(bytes.Repeat([]byte("x"), 1<<20)), 0o644)
	l, err := Layer([]Add{{Src: src, Dst: "/big"}}, time.Unix(42, 0), types.OCILayer)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	entries := readTar(t, rc)
	rc.Close()
	if len(entries) != 1 || len(entries[0].body) != 1<<20 {
		t.Fatalf("entries = %d, want one 1 MiB file", len(entries))
	}

	if err := os.WriteFile(src, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err = l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err == nil {
		t.Fatal("want error reading a layer whose source shrank")
	}
}

func TestExpandAdds(t *testing.T) {
	got := expandAdds([]Add{{Src: "dist/app-{os}-{arch}{variant}", Dst: "/{arch}"}}, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"})
	if want := (Add{Src: "dist/app-linux-armv7", Dst: "/{arch}"}); got[0] != want {
		t.Errorf("expandAdds = %+v, want %+v", got[0], want)
	}
}

func TestLayerMissingSource(t *testing.T) {
	if _, err := layerTar([]Add{{Src: filepath.Join(t.TempDir(), "nope"), Dst: "/x"}}, time.Unix(0, 0)); err == nil {
		t.Fatal("want error for missing source")
	}
}
