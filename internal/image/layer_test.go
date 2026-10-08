package image

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// copies is a single unnamed layer that copies src to dst.
func copies(src, dst string) []LayerSpec {
	return []LayerSpec{{Items: []Item{{Kind: Copy, Src: src, Dst: dst}}}}
}

func ptr[T any](v T) *T { return &v }

// layerTar returns the uncompressed tar stream for items.
func layerTar(items []Item, mtime time.Time) ([]byte, error) {
	entries, err := collectEntries(items)
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
	name     string
	typ      byte
	mode     int64
	uid, gid int
	link     string
	body     string
}

// readTar returns the entries of r, checking that every mtime is 42.
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
		if check && !hdr.ModTime.Equal(time.Unix(42, 0)) {
			t.Errorf("%s: mtime %v, want 42", hdr.Name, hdr.ModTime)
		}
		if check && (hdr.Uname != "" || hdr.Gname != "") {
			t.Errorf("%s: owner names %q:%q, want none", hdr.Name, hdr.Uname, hdr.Gname)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tarEntry{
			name: hdr.Name, typ: hdr.Typeflag, mode: hdr.Mode,
			uid: hdr.Uid, gid: hdr.Gid, link: hdr.Linkname, body: string(body),
		})
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
	// WriteFile is subject to the umask, and chmod is needed for setuid.
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func TestParseItems(t *testing.T) {
	got, err := ParseAdd("bin/app:/usr/local/bin/app")
	if err != nil || !reflect.DeepEqual(got, Item{Kind: Copy, Src: "bin/app", Dst: "/usr/local/bin/app"}) {
		t.Errorf("ParseAdd = %+v, %v", got, err)
	}
	got, err = ParseAdd("web:/srv/www:mode=0644,dirmode=0750,owner=101:102")
	want := Item{Kind: Copy, Src: "web", Dst: "/srv/www", Mode: ptr[int64](0o644), DirMode: ptr[int64](0o750), Owner: &Owner{101, 102}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAdd with options = %+v, %v", got, err)
	}
	got, err = ParseMkdir("/data:mode=1777,owner=65532")
	want = Item{Kind: Dir, Dst: "/data", Mode: ptr[int64](0o1777), Owner: &Owner{65532, 65532}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseMkdir = %+v, %v", got, err)
	}
	got, err = ParseSymlink("/app:/usr/local/bin/app")
	if err != nil || !reflect.DeepEqual(got, Item{Kind: Symlink, Dst: "/app", Target: "/usr/local/bin/app"}) {
		t.Errorf("ParseSymlink = %+v, %v", got, err)
	}

	for _, bad := range []string{"", "app", ":/app", "app:", "app:relative", "app:/x:mode=9", "app:/x:mode=17777", "app:/x:owner=me", "app:/x:owner=1:x", "app:/x:color=red", "app:/x:mode"} {
		if _, err := ParseAdd(bad); err == nil {
			t.Errorf("ParseAdd(%q) succeeded", bad)
		}
	}
	for _, bad := range []string{"", "data", "/data:dirmode=0755", "/data:owner=-1"} {
		if _, err := ParseMkdir(bad); err == nil {
			t.Errorf("ParseMkdir(%q) succeeded", bad)
		}
	}
	for _, bad := range []string{"/app", "/app:", "app:/x", "/:/x", "/dir/:/x"} {
		if _, err := ParseSymlink(bad); err == nil {
			t.Errorf("ParseSymlink(%q) succeeded", bad)
		}
	}
}

func TestLayerContents(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "static", "index.html"), "<h1>hi</h1>", 0o644)
	if err := os.Symlink("index.html", filepath.Join(src, "static", "home.html")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "app"), "binary", 0o755)
	writeFile(t, filepath.Join(src, "helper"), "suid", 0o755|os.ModeSetuid)
	writeFile(t, filepath.Join(src, "conf"), "k=v", 0o600)

	items := []Item{
		{Kind: Copy, Src: filepath.Join(src, "static"), Dst: "/srv/www", DirMode: ptr[int64](0o750), Owner: &Owner{101, 101}},
		{Kind: Copy, Src: filepath.Join(src, "app"), Dst: "/usr/local/bin/"},
		{Kind: Copy, Src: filepath.Join(src, "helper"), Dst: "/usr/local/bin/helper"},
		{Kind: Copy, Src: filepath.Join(src, "conf"), Dst: "/etc/app.conf", Mode: ptr[int64](0o640)},
		{Kind: Dir, Dst: "/data", Mode: ptr[int64](0o700), Owner: &Owner{65532, 65532}},
		{Kind: Dir, Dst: "/tmp", Mode: ptr[int64](0o1777)},
		{Kind: Symlink, Dst: "/app", Target: "/usr/local/bin/app"},
		{Kind: File, Dst: "/etc/motd", Content: []byte("hello\n")},
		// A later item replaces an earlier one at the same path.
		{Kind: File, Dst: "/etc/motd", Content: []byte("bye\n"), Mode: ptr[int64](0o444)},
	}
	data, err := layerTar(items, time.Unix(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := readTar(t, bytes.NewReader(data))
	want := []tarEntry{
		{name: "app", typ: tar.TypeSymlink, mode: 0o777, link: "/usr/local/bin/app"},
		{name: "data/", typ: tar.TypeDir, mode: 0o700, uid: 65532, gid: 65532},
		{name: "etc/app.conf", typ: tar.TypeReg, mode: 0o640, body: "k=v"},
		{name: "etc/motd", typ: tar.TypeReg, mode: 0o444, body: "bye\n"},
		{name: "srv/www/", typ: tar.TypeDir, mode: 0o750, uid: 101, gid: 101},
		{name: "srv/www/home.html", typ: tar.TypeSymlink, mode: 0o777, uid: 101, gid: 101, link: "index.html"},
		{name: "srv/www/index.html", typ: tar.TypeReg, mode: 0o644, uid: 101, gid: 101, body: "<h1>hi</h1>"},
		{name: "tmp/", typ: tar.TypeDir, mode: 0o1777},
		{name: "usr/local/bin/app", typ: tar.TypeReg, mode: 0o755, body: "binary"},
		{name: "usr/local/bin/helper", typ: tar.TypeReg, mode: 0o4755, body: "suid"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("layer entries:\n got %+v\nwant %+v", got, want)
	}
}

func TestLayerDeterministic(t *testing.T) {
	src := t.TempDir()
	for _, n := range []string{"b", "a", "c/d", "c/a"} {
		writeFile(t, filepath.Join(src, n), n, 0o644)
	}
	items := []Item{{Kind: Copy, Src: src, Dst: "/data"}}
	first, err := Layer(items, LayerOptions{Created: time.Unix(42, 0)})
	if err != nil {
		t.Fatal(err)
	}
	// Touch every file: content and mode are unchanged, so the digest
	// must be too.
	later := time.Now().Add(time.Hour)
	for _, n := range []string{"b", "a", "c/d", "c/a"} {
		if err := os.Chtimes(filepath.Join(src, n), later, later); err != nil {
			t.Fatal(err)
		}
	}
	second, err := Layer(items, LayerOptions{Created: time.Unix(42, 0)})
	if err != nil {
		t.Fatal(err)
	}
	d1, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("digest changed: %s != %s", d1, d2)
	}

	data, err := layerTar(items, time.Unix(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range readTar(t, bytes.NewReader(data)) {
		names = append(names, e.name)
	}
	// Sorted, and no implied parents above the destination.
	want := []string{"data/", "data/a", "data/b", "data/c/", "data/c/a", "data/c/d"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %q, want %q", names, want)
	}
}

// Layers are streamed from disk on every read rather than buffered, so a
// source that shrinks after Layer is called must fail the read loudly.
func TestLayerStreamsFromDisk(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big")
	writeFile(t, src, string(bytes.Repeat([]byte("x"), 1<<20)), 0o644)
	l, err := Layer([]Item{{Kind: Copy, Src: src, Dst: "/big"}}, LayerOptions{Created: time.Unix(42, 0), MediaType: types.OCILayer})
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

func TestExpandItems(t *testing.T) {
	got := expandItems([]Item{
		{Kind: Copy, Src: "dist/app-{os}-{arch}{variant}", Dst: "/{arch}"},
		{Kind: Symlink, Dst: "/x", Target: "{arch}"},
	}, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"})
	if got[0].Src != "dist/app-linux-armv7" || got[0].Dst != "/{arch}" || got[1].Target != "{arch}" {
		t.Errorf("expandItems = %+v", got)
	}
}

func TestLayerRejectsBadItems(t *testing.T) {
	for _, it := range []Item{
		{Kind: Copy, Src: filepath.Join(t.TempDir(), "nope"), Dst: "/x"},
		{Kind: Copy, Src: "x", Dst: "relative"},
		{Kind: Symlink, Dst: "/x"},
		{Kind: File, Dst: "/etc/"},
		{Kind: Kind(99), Dst: "/x"},
	} {
		if _, err := layerTar([]Item{it}, time.Unix(0, 0)); err == nil {
			t.Errorf("item %+v accepted", it)
		}
	}
}
