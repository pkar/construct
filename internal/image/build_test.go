package image

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// testRegistry starts an in-memory OCI registry and returns its host:port.
func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

var anonymous = Options{Keychain: authn.NewMultiKeychain()}

func TestBuildScratch(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "app"), "binary", 0o755)

	created := time.Unix(1700000000, 0).UTC()
	spec := Spec{
		Base:       Scratch,
		Platform:   v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		Layers:     copies(filepath.Join(src, "app"), "/app"),
		Entrypoint: []string{"/app"},
		Cmd:        []string{"--port", "8080"},
		Env:        []string{"A=1", "B=2", "A=3"},
		Labels:     map[string]string{"org.opencontainers.image.source": "https://example.com/app"},
		WorkDir:    "/",
		User:       "65532:65532",
		Created:    created,
	}
	img, err := Build(context.Background(), spec, anonymous)
	if err != nil {
		t.Fatal(err)
	}

	mt, err := img.MediaType()
	if err != nil || mt != types.OCIManifestSchema1 {
		t.Errorf("manifest media type = %s, %v", mt, err)
	}
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Config.MediaType != types.OCIConfigJSON {
		t.Errorf("config media type = %s", m.Config.MediaType)
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != types.OCILayer {
		t.Fatalf("layers = %+v, want one OCI layer", m.Layers)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.OS != "linux" || cf.Architecture != "arm64" || cf.Variant != "v8" {
		t.Errorf("platform = %s/%s/%s", cf.OS, cf.Architecture, cf.Variant)
	}
	if !cf.Created.Time.Equal(created) {
		t.Errorf("created = %v, want %v", cf.Created.Time, created)
	}
	c := cf.Config
	if !slices.Equal(c.Entrypoint, spec.Entrypoint) || !slices.Equal(c.Cmd, spec.Cmd) {
		t.Errorf("entrypoint/cmd = %q %q", c.Entrypoint, c.Cmd)
	}
	if !slices.Equal(c.Env, []string{"A=3", "B=2"}) {
		t.Errorf("env = %q", c.Env)
	}
	if c.WorkingDir != "/" || c.User != "65532:65532" || c.Labels["org.opencontainers.image.source"] != "https://example.com/app" {
		t.Errorf("config = %+v", c)
	}
	if len(cf.History) != 1 || len(cf.RootFS.DiffIDs) != 1 {
		t.Errorf("history %d, diff ids %d; want 1 each", len(cf.History), len(cf.RootFS.DiffIDs))
	}

	// The same spec must produce the same digest.
	again, err := Build(context.Background(), spec, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := img.Digest()
	d2, _ := again.Digest()
	if d1 != d2 {
		t.Errorf("rebuild digest %s != %s", d2, d1)
	}
}

func TestBuildLayers(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "lib", "dep.so"), "dependency", 0o644)
	writeFile(t, filepath.Join(src, "app"), "v1", 0o755)
	spec := Spec{
		Base:     Scratch,
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		Layers: []LayerSpec{
			{Name: "deps", Items: []Item{{Kind: Copy, Src: filepath.Join(src, "lib"), Dst: "/usr/lib/app"}}},
			{Name: "app", Items: []Item{
				{Kind: Copy, Src: filepath.Join(src, "app"), Dst: "/usr/local/bin/app"},
				{Kind: Dir, Dst: "/data", Owner: &Owner{65532, 65532}},
			}},
		},
	}
	build := func() []v1.Hash {
		t.Helper()
		img, err := Build(context.Background(), spec, anonymous)
		if err != nil {
			t.Fatal(err)
		}
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"construct: layer deps: add /usr/lib/app",
			"construct: layer app: add /usr/local/bin/app; mkdir /data",
		}
		var got []string
		for _, h := range cf.History {
			got = append(got, h.CreatedBy)
		}
		if !slices.Equal(got, want) {
			t.Errorf("history = %q, want %q", got, want)
		}
		m, err := img.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		var digests []v1.Hash
		for _, l := range m.Layers {
			digests = append(digests, l.Digest)
		}
		return digests
	}
	before := build()
	if len(before) != 2 {
		t.Fatalf("layers = %d, want 2", len(before))
	}
	// Changing the app leaves the dependency layer, and its upload, alone.
	writeFile(t, filepath.Join(src, "app"), "v2", 0o755)
	after := build()
	if before[0] != after[0] || before[1] == after[1] {
		t.Errorf("layer digests before %v after %v; want only the app layer to change", before, after)
	}

	spec.Layers = append(spec.Layers, LayerSpec{Name: "empty"})
	if _, err := Build(context.Background(), spec, anonymous); err == nil || !strings.Contains(err.Error(), "layer empty is empty") {
		t.Errorf("empty layer: err = %v", err)
	}
}

func TestBuildConfigExtras(t *testing.T) {
	spec := Spec{
		Base:         Scratch,
		Platform:     v1.Platform{OS: "linux", Architecture: "amd64"},
		ExposedPorts: []string{"8080", "53/udp", "08080/tcp"},
		Volumes:      []string{"/data/", "/cache"},
		StopSignal:   "SIGINT",
		Annotations:  map[string]string{"org.opencontainers.image.revision": "abc"},
	}
	img, err := Build(context.Background(), spec, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cf.Config
	if len(c.ExposedPorts) != 2 || len(c.Volumes) != 2 || c.StopSignal != "SIGINT" {
		t.Errorf("config = %+v", c)
	}
	for _, k := range []string{"8080/tcp", "53/udp"} {
		if _, ok := c.ExposedPorts[k]; !ok {
			t.Errorf("missing port %s in %v", k, c.ExposedPorts)
		}
	}
	if _, ok := c.Volumes["/data"]; !ok {
		t.Errorf("volumes = %v", c.Volumes)
	}
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Annotations["org.opencontainers.image.revision"] != "abc" {
		t.Errorf("manifest annotations = %v", m.Annotations)
	}

	idx, err := BuildIndex(context.Background(), spec, []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if im.Annotations["org.opencontainers.image.revision"] != "abc" {
		t.Errorf("index annotations = %v", im.Annotations)
	}

	for _, bad := range []Spec{
		{ExposedPorts: []string{"http"}},
		{ExposedPorts: []string{"70000"}},
		{ExposedPorts: []string{"80/icmp"}},
		{Volumes: []string{"data"}},
	} {
		bad.Base = Scratch
		if _, err := Build(context.Background(), bad, anonymous); err == nil {
			t.Errorf("spec %+v accepted", bad)
		}
	}
}

func TestBuildZstdPush(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "app"), "binary", 0o755)
	img, err := Build(ctx, Spec{
		Base:        Scratch,
		Platform:    v1.Platform{OS: "linux", Architecture: "amd64"},
		Layers:      copies(filepath.Join(src, "app"), "/app"),
		Compression: "zstd",
	}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Push(ctx, img, host+"/zstd:v1", anonymous)
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	m, err := got.Manifest()
	if err != nil || len(m.Layers) != 1 || m.Layers[0].MediaType != types.OCILayerZStd {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	layers, err := got.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if body := fileInLayer(t, layers[0], "app"); body != "binary" {
		t.Errorf("app = %q", body)
	}
}

func TestBuildRejectsBadEnv(t *testing.T) {
	_, err := Build(context.Background(), Spec{Base: Scratch, Env: []string{"NOEQUALS"}}, anonymous)
	if err == nil {
		t.Fatal("want error for env without =")
	}
}

func TestBuildFromRegistryBaseAndPush(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "one"), "1", 0o644)
	writeFile(t, filepath.Join(src, "two"), "2", 0o644)
	plat := v1.Platform{OS: "linux", Architecture: "amd64"}

	// Publish a base image with an entrypoint and env.
	base, err := Build(ctx, Spec{
		Base:       Scratch,
		Platform:   plat,
		Layers:     copies(filepath.Join(src, "one"), "/one"),
		Entrypoint: []string{"/bin/base"},
		Cmd:        []string{"serve"},
		Env:        []string{"PATH=/bin", "KEEP=yes"},
	}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	baseRef := host + "/base:v1"
	if _, err := Push(ctx, base, baseRef, anonymous); err != nil {
		t.Fatal(err)
	}

	// Build on top of it and push the result.
	img, err := Build(ctx, Spec{
		Base:     baseRef,
		Platform: plat,
		Layers:   copies(filepath.Join(src, "two"), "/two"),
		Env:      []string{"PATH=/usr/bin:/bin"},
	}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	appRef := host + "/team/app:v2"
	pushed, err := Push(ctx, img, appRef, anonymous)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := name.ParseReference(appRef)
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, _ := got.Digest()
	if gotDigest.String() != pushed.DigestStr() {
		t.Errorf("registry digest %s, Push returned %s", gotDigest, pushed.DigestStr())
	}
	layers, _ := got.Layers()
	if len(layers) != 2 {
		t.Errorf("layers = %d, want base + 1", len(layers))
	}
	cf, _ := got.ConfigFile()
	if !slices.Equal(cf.Config.Entrypoint, []string{"/bin/base"}) || !slices.Equal(cf.Config.Cmd, []string{"serve"}) {
		t.Errorf("inherited entrypoint/cmd = %q %q", cf.Config.Entrypoint, cf.Config.Cmd)
	}
	if !slices.Equal(cf.Config.Env, []string{"PATH=/usr/bin:/bin", "KEEP=yes"}) {
		t.Errorf("env = %q", cf.Config.Env)
	}
}

func TestNewEntrypointDropsInheritedCmd(t *testing.T) {
	cf := &v1.ConfigFile{Config: v1.Config{Entrypoint: []string{"/old"}, Cmd: []string{"old"}}}
	if err := applyConfig(cf, Spec{Entrypoint: []string{"/new"}}); err != nil {
		t.Fatal(err)
	}
	if cf.Config.Cmd != nil {
		t.Errorf("cmd = %q, want cleared", cf.Config.Cmd)
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	img, err := Build(ctx, Spec{Base: Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Cmd: []string{"x"}}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "layout")
	if err := WriteLayout(dir, img, "example/app:v1"); err != nil {
		t.Fatal(err)
	}
	// Writing again replaces the index rather than appending a second image.
	if err := WriteLayout(dir, img, "example/app:v1"); err != nil {
		t.Fatal(err)
	}
	read, err := ReadLayout(dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	if got, _ := read.Digest(); got != want {
		t.Fatalf("layout digest %s, want %s", got, want)
	}
	pushed, err := Push(ctx, read, host+"/layout/app:v1", anonymous)
	if err != nil {
		t.Fatal(err)
	}
	if pushed.DigestStr() != want.String() {
		t.Errorf("pushed %s, want %s", pushed.DigestStr(), want)
	}
}

// fileInLayer returns the contents of name (no leading slash) in layer.
func fileInLayer(t *testing.T, layer v1.Layer, name string) string {
	t.Helper()
	rc, err := layer.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	for _, e := range readTarNoChecks(t, rc) {
		if e.name == name {
			return e.body
		}
	}
	t.Fatalf("%s not in layer", name)
	return ""
}

func TestBuildIndex(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	src := t.TempDir()
	platforms, err := ParsePlatforms("linux/amd64, linux/arm64/v8")
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		writeFile(t, filepath.Join(src, "base-linux-"+arch), "base "+arch, 0o644)
		writeFile(t, filepath.Join(src, "app-linux-"+arch), "app "+arch, 0o755)
	}

	// A multi-platform base, then an app index built on top of it.
	base, err := BuildIndex(ctx, Spec{
		Base:   Scratch,
		Layers: copies(filepath.Join(src, "base-{os}-{arch}"), "/base"),
	}, platforms, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	baseRef := host + "/base:multi"
	if _, err := Push(ctx, base, baseRef, anonymous); err != nil {
		t.Fatal(err)
	}
	idx, err := BuildIndex(ctx, Spec{
		Base:       baseRef,
		Layers:     copies(filepath.Join(src, "app-{os}-{arch}"), "/app"),
		Entrypoint: []string{"/app"},
	}, platforms, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := Push(ctx, idx, host+"/app:multi", anonymous)
	if err != nil {
		t.Fatal(err)
	}

	got, err := remote.Index(pushed)
	if err != nil {
		t.Fatal(err)
	}
	if mt, _ := got.MediaType(); mt != types.OCIImageIndex {
		t.Errorf("index media type = %s", mt)
	}
	im, err := got.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Manifests) != 2 {
		t.Fatalf("index has %d manifests, want 2", len(im.Manifests))
	}
	for i, desc := range im.Manifests {
		want := platforms[i]
		if desc.Platform == nil || desc.Platform.OS != want.OS || desc.Platform.Architecture != want.Architecture || desc.Platform.Variant != want.Variant {
			t.Errorf("manifest %d platform = %+v, want %+v", i, desc.Platform, want)
		}
		img, err := got.Image(desc.Digest)
		if err != nil {
			t.Fatal(err)
		}
		layers, err := img.Layers()
		if err != nil || len(layers) != 2 {
			t.Fatalf("%s: layers = %d, %v; want base + app", want.Architecture, len(layers), err)
		}
		if body := fileInLayer(t, layers[0], "base"); body != "base "+want.Architecture {
			t.Errorf("%s: base layer has %q", want.Architecture, body)
		}
		if body := fileInLayer(t, layers[1], "app"); body != "app "+want.Architecture {
			t.Errorf("%s: app layer has %q", want.Architecture, body)
		}
	}

	// The index survives an OCI layout round trip.
	dir := filepath.Join(t.TempDir(), "layout")
	if err := WriteLayout(dir, idx, "app:multi"); err != nil {
		t.Fatal(err)
	}
	read, err := ReadLayout(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := read.(v1.ImageIndex); !ok {
		t.Fatalf("ReadLayout returned %T, want an index", read)
	}
	if d, _ := read.Digest(); d.String() != pushed.DigestStr() {
		t.Errorf("layout digest %s, want %s", d, pushed.DigestStr())
	}

	if err := WriteTarball(filepath.Join(t.TempDir(), "x.tar"), idx, []string{"app:multi"}, anonymous); err == nil {
		t.Error("want error writing an index as a tarball")
	}
}

func TestBuildIndexRejectsDuplicatePlatform(t *testing.T) {
	platforms, err := ParsePlatforms("linux/amd64,linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildIndex(context.Background(), Spec{Base: Scratch}, platforms, anonymous); err == nil {
		t.Fatal("want error for duplicate platform")
	}
}

func TestParsePlatforms(t *testing.T) {
	for _, bad := range []string{"", ",", "linux", "a/b/c/d"} {
		if _, err := ParsePlatforms(bad); err == nil {
			t.Errorf("ParsePlatforms(%q) succeeded", bad)
		}
	}
}

func TestBaseWrongPlatform(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	base, err := Build(ctx, Spec{Base: Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/amd64-only:v1"
	if _, err := Push(ctx, base, ref, anonymous); err != nil {
		t.Fatal(err)
	}
	_, err = Build(ctx, Spec{Base: ref, Platform: v1.Platform{OS: "linux", Architecture: "arm64"}}, anonymous)
	if err == nil {
		t.Fatal("want error building arm64 on an amd64-only base")
	}
}

func mustDigest(t *testing.T, a Artifact) v1.Hash {
	t.Helper()
	d, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWriteTarball(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "app"), "binary", 0o755)
	img, err := Build(context.Background(), Spec{
		Base:     Scratch,
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		Layers:   append(copies(filepath.Join(src, "app"), "/app"), copies(filepath.Join(src, "app"), "/app")...),
	}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "img.tar")
	tags := []string{"example.com/app:v1", "example.com/app:latest"}
	if err := WriteTarball(file, img, tags, anonymous); err != nil {
		t.Fatal(err)
	}
	m, err := tarball.LoadManifest(func() (io.ReadCloser, error) { return os.Open(file) })
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || !slices.Equal(m[0].RepoTags, tags) {
		t.Errorf("tarball manifest = %+v, want one image tagged %q", m, tags)
	}
	// The tarball loads back with the same config and layers. (Docker
	// tarballs hold no manifest, so the reader makes up its own.)
	loaded, err := tarball.ImageFromPath(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := img.ConfigName()
	c2, err := loaded.ConfigName()
	if err != nil || c1 != c2 {
		t.Errorf("loaded config %s, %v; want %s", c2, err, c1)
	}
	m1, _ := img.Manifest()
	m2, err := loaded.Manifest()
	if err != nil || len(m2.Layers) != 2 || m2.Layers[0].Digest != m1.Layers[0].Digest || m2.Layers[1].Digest != m1.Layers[1].Digest {
		t.Errorf("loaded layers %+v, %v; want %+v", m2, err, m1.Layers)
	}
	first, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := WriteTarball(file, img, tags, anonymous); err != nil {
			t.Fatal(err)
		}
		again, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("rewriting the tarball changed its bytes")
		}
	}
	if err := WriteTarball(file, img, []string{"example.com/app@sha256:abc"}, anonymous); err == nil {
		t.Error("want error for a digest reference")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("failed write left %s behind: %v", file, err)
	}
}

func TestPushAll(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	img, err := Build(ctx, Spec{Base: Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	refs := []string{host + "/app:v1", host + "/app:latest", host + "/mirror/app:v1", host + "/app@" + d.String()}
	got, err := PushAll(ctx, img, refs, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Context().RepositoryStr() != "app" || got[1].Context().RepositoryStr() != "mirror/app" {
		t.Errorf("digests = %v, want one per repository", got)
	}
	for _, r := range refs[:3] {
		ref, err := name.ParseReference(r)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := remote.Head(ref)
		if err != nil || desc.Digest != d {
			t.Errorf("%s: %v, %v; want %s", r, desc, err, d)
		}
	}

	for _, bad := range [][]string{
		nil,
		{host + "/app:v1", host + "/app:v1"},
		{host + "/app@sha256:" + strings.Repeat("0", 64)},
		{"UPPER/app:v1"},
	} {
		if _, err := PushAll(ctx, img, bad, anonymous); err == nil {
			t.Errorf("PushAll(%q) succeeded", bad)
		}
	}
}
