package image

import (
	"context"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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
		Adds:       []Add{{Src: filepath.Join(src, "app"), Dst: "/app"}},
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
		Adds:       []Add{{Src: filepath.Join(src, "one"), Dst: "/one"}},
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
		Adds:     []Add{{Src: filepath.Join(src, "two"), Dst: "/two"}},
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

func TestWriteTarball(t *testing.T) {
	img, err := Build(context.Background(), Spec{Base: Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "img.tar")
	if err := WriteTarball(file, img, "example.com/app:v1", anonymous); err != nil {
		t.Fatal(err)
	}
	if err := WriteTarball(file, img, "example.com/app@sha256:abc", anonymous); err == nil {
		t.Error("want error for a digest reference")
	}
}
