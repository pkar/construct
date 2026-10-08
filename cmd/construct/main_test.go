package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/pkar/construct/internal/image"
)

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageErrors(t *testing.T) {
	// Keep credential lookups away from the developer's Docker config.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown", []string{"frobnicate"}, 2},
		{"no output", []string{"build"}, 2},
		{"push without tag", []string{"build", "-push"}, 2},
		{"tarball without tag", []string{"build", "-tarball", "x.tar"}, 2},
		{"stray arg", []string{"build", "-oci-layout", "x", "extra"}, 2},
		{"bad flag", []string{"build", "-nope"}, 2},
		{"push args", []string{"push", "only-one"}, 2},
		{"bad add", []string{"build", "-oci-layout", t.TempDir(), "-add", "app:relative"}, 2},
		{"bad add option", []string{"build", "-oci-layout", t.TempDir(), "-add", "app:/app:mode=999"}, 2},
		{"duplicate layer", []string{"build", "-oci-layout", t.TempDir(), "-layer", "a", "-layer", "a"}, 2},
		{"empty layer", []string{"build", "-oci-layout", t.TempDir(), "-layer", "a"}, 1},
		{"bad compression", []string{"build", "-oci-layout", t.TempDir(), "-compression", "lz4"}, 2},
		{"bad level", []string{"build", "-oci-layout", t.TempDir(), "-compression-level", "10"}, 2},
		{"missing source", []string{"build", "-oci-layout", t.TempDir(), "-add", "/does/not/exist:/x"}, 1},
		{"bad platform", []string{"build", "-oci-layout", t.TempDir(), "-platform", "a/b/c/d"}, 1},
		{"multi-platform tarball", []string{"build", "-tag", "app:v1", "-tarball", "x.tar", "-platform", "linux/amd64,linux/arm64"}, 2},
		{"help", []string{"build", "-h"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, stderr := runCLI(t, tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, tc.want, stderr)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := runCLI(t, "version")
	if code != 0 || out != "construct dev\n" {
		t.Errorf("version = %d %q", code, out)
	}
}

func TestOptionalList(t *testing.T) {
	for in, want := range map[string][]string{
		`/app --port 80`:           {"/app", "--port", "80"},
		`["/bin/sh", "-c", "a b"]`: {"/bin/sh", "-c", "a b"},
		`[]`:                       {},
	} {
		var o optionalList
		if err := o.Set(in); err != nil {
			t.Fatalf("Set(%q): %v", in, err)
		}
		if o.list == nil || !slices.Equal(o.list, want) {
			t.Errorf("Set(%q) = %q, want %q", in, o.list, want)
		}
	}
	var o optionalList
	if err := o.Set(`["unterminated`); err == nil {
		t.Error("want error for bad JSON")
	}
}

func TestBuildLayers(t *testing.T) {
	src := t.TempDir()
	for _, n := range []string{"dep.so", "app"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "layout")
	code, _, stderr := runCLI(t, "build",
		"-layer", "deps", "-add", filepath.Join(src, "dep.so")+":/usr/lib/",
		"-layer", "app", "-add", filepath.Join(src, "app")+":/app:mode=0555,owner=65532",
		"-mkdir", "/data:mode=0700,owner=65532:65532",
		"-symlink", "/usr/local/bin/app:/app",
		"-oci-layout", dir,
	)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	a, err := image.ReadLayout(dir)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := a.(v1.Image).Layers()
	if err != nil || len(layers) != 2 {
		t.Fatalf("layers = %d, %v; want 2", len(layers), err)
	}
	rc, err := layers[1].Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var got []string
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %o %d:%d %s", hdr.Name, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Linkname))
	}
	want := []string{
		"app 555 65532:65532 ",
		"data/ 700 65532:65532 ",
		"usr/local/bin/app 777 0:0 /app",
	}
	if !slices.Equal(got, want) {
		t.Errorf("app layer:\n got %q\nwant %q", got, want)
	}
}

func gitInit(t *testing.T, dir, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"remote", "add", "origin", remote},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "one"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestBuildConfigAndAnnotations(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	repo := t.TempDir()
	gitInit(t, repo, "git@example.com:org/app.git")
	t.Chdir(repo)

	manifest := func(extra ...string) (*v1.Manifest, *v1.ConfigFile) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "layout")
		args := append([]string{"build", "-oci-layout", dir, "-platform", "linux/amd64"}, extra...)
		if code, _, stderr := runCLI(t, args...); code != 0 {
			t.Fatalf("build exit %d: %s", code, stderr)
		}
		a, err := image.ReadLayout(dir)
		if err != nil {
			t.Fatal(err)
		}
		m, err := a.(v1.Image).Manifest()
		if err != nil {
			t.Fatal(err)
		}
		cf, err := a.(v1.Image).ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		return m, cf
	}

	m, cf := manifest("-expose", "8080", "-expose", "53/udp", "-volume", "/data", "-stop-signal", "SIGINT",
		"-annotation", "org.opencontainers.image.source=https://override.example.com/app")
	if len(m.Annotations["org.opencontainers.image.revision"]) != 40 {
		t.Errorf("revision annotation = %q", m.Annotations["org.opencontainers.image.revision"])
	}
	if got := m.Annotations["org.opencontainers.image.source"]; got != "https://override.example.com/app" {
		t.Errorf("source annotation = %q, want the -annotation override", got)
	}
	c := cf.Config
	if _, ok := c.ExposedPorts["53/udp"]; !ok || len(c.ExposedPorts) != 2 || len(c.Volumes) != 1 || c.StopSignal != "SIGINT" {
		t.Errorf("config = %+v", c)
	}

	m, _ = manifest()
	if got := m.Annotations["org.opencontainers.image.source"]; got != "https://example.com/org/app" {
		t.Errorf("source annotation = %q", got)
	}
	m, _ = manifest("-vcs=false")
	if len(m.Annotations) != 0 {
		t.Errorf("-vcs=false annotations = %v", m.Annotations)
	}

	if code, _, _ := runCLI(t, "build", "-oci-layout", t.TempDir(), "-expose", "http"); code != 2 {
		t.Errorf("bad -expose exit %d, want 2", code)
	}
}

func TestBuildStampedTags(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("BUILD_NUMBER", "17")
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	repo := t.TempDir()
	gitInit(t, repo, "https://example.com/org/app.git")
	t.Chdir(repo)

	code, out, stderr := runCLI(t, "build", "-push",
		"-tag", u.Host+"/app:{git.short}",
		"-tag", u.Host+"/app:build-{env.BUILD_NUMBER}",
		"-tag", u.Host+"/app:{git.branch}",
		"-label", "build={env.BUILD_NUMBER}",
	)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	lines := strings.Fields(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "/app@sha256:") {
		t.Fatalf("stdout = %q, want one digest reference", out)
	}
	tags, err := remote.List(mustRepo(t, u.Host+"/app"))
	if err != nil {
		t.Fatal(err)
	}
	short := slices.IndexFunc(tags, func(s string) bool { return len(s) == 12 })
	if len(tags) != 3 || !slices.Contains(tags, "build-17") || !slices.Contains(tags, "main") || short < 0 {
		t.Errorf("tags = %q", tags)
	}
	img, err := remote.Image(mustRepo(t, u.Host+"/app").Tag("main"))
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil || cf.Config.Labels["build"] != "17" {
		t.Errorf("labels = %v, %v", cf.Config.Labels, err)
	}

	for _, args := range [][]string{
		{"build", "-push", "-tag", u.Host + "/app:{git.tag}"},
		{"build", "-push", "-tag", u.Host + "/app:{env.NOT_SET_ANYWHERE}"},
		{"build", "-push", "-tag", u.Host + "/app:x", "-tag", u.Host + "/app:x"},
	} {
		if code, _, stderr := runCLI(t, args...); code != 1 {
			t.Errorf("%q: exit %d, want 1: %s", args, code, stderr)
		}
	}
}

func TestBuildFromFile(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	dir := t.TempDir()
	for _, n := range []string{"api-amd64", "api-arm64", "worker"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	doc := `
vcs: false
labels: {team: core}
images:
  - name: api
    platforms: [linux/amd64, linux/arm64]
    tags: [REG/api:v1]
    push: true
    entrypoint: [/api]
    layers:
      - name: app
        contents: ["api-{arch}:/api"]
  - name: worker
    platforms: linux/amd64
    oci-layout: out/worker
    layers:
      - contents:
          - {src: worker, dst: /worker, mode: "0500"}
`
	if err := os.WriteFile(filepath.Join(dir, "construct.yaml"), []byte(strings.ReplaceAll(doc, "REG", u.Host)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Run from elsewhere: paths resolve against the file's directory.
	t.Chdir(t.TempDir())
	file := filepath.Join(dir, "construct.yaml")

	code, out, stderr := runCLI(t, "build", "-f", file)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	if lines := strings.Fields(out); len(lines) != 2 || !strings.Contains(lines[0], "/api@sha256:") || !strings.HasPrefix(lines[1], "sha256:") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(stderr, "api: pushed ") || !strings.Contains(stderr, "worker: wrote OCI layout ") {
		t.Errorf("stderr = %q", stderr)
	}
	idx, err := remote.Index(mustRepo(t, u.Host+"/api").Tag("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if im, err := idx.IndexManifest(); err != nil || len(im.Manifests) != 2 {
		t.Fatalf("api index = %+v, %v", im, err)
	}

	// Flags override the file for the selected image only.
	code, _, stderr = runCLI(t, "build", "-f", file, "-label", "team=edge", "-env", "DEBUG=1", "worker")
	if code != 0 {
		t.Fatalf("build worker exit %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "api") {
		t.Errorf("building worker touched api: %s", stderr)
	}
	a, err := image.ReadLayout(filepath.Join(dir, "out", "worker"))
	if err != nil {
		t.Fatal(err)
	}
	cf, err := a.(v1.Image).ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Config.Labels["team"] != "edge" || !slices.Contains(cf.Config.Env, "DEBUG=1") {
		t.Errorf("worker config = %+v", cf.Config)
	}

	for _, args := range [][]string{
		{"build", "-f", file, "nope"},
		{"build", "-f", filepath.Join(dir, "missing.yaml")},
		// Both images would write the same layout.
		{"build", "-f", file, "-oci-layout", filepath.Join(dir, "same")},
		// The api image has two platforms.
		{"build", "-f", file, "-tarball", "x.tar", "api"},
	} {
		if code, _, stderr := runCLI(t, args...); code != 1 {
			t.Errorf("%q: exit %d, want 1: %s", args, code, stderr)
		}
	}
	if code, _, _ := runCLI(t, "build", "-oci-layout", t.TempDir(), "api"); code != 2 {
		t.Errorf("image name without -f: exit %d, want 2", code)
	}
}

func TestLockAndLockedBuild(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	ctx := context.Background()
	baseRef := u.Host + "/base:stable"

	// pushBase pushes a new image to base:stable and returns its digest.
	pushBase := func(env string) string {
		t.Helper()
		img, err := image.Build(ctx, image.Spec{Base: image.Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Env: []string{env}}, image.Options{})
		if err != nil {
			t.Fatal(err)
		}
		d, err := image.Push(ctx, img, baseRef, image.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return d.DigestStr()
	}
	first := pushBase("V=1")

	dir := t.TempDir()
	file := filepath.Join(dir, "construct.yaml")
	doc := "base: " + baseRef + "\nplatforms: linux/amd64\nvcs: false\noci-layout: out\n"
	if err := os.WriteFile(file, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	// -locked with no lock file is refused.
	if code, _, stderr := runCLI(t, "build", "-f", file, "-locked"); code != 2 {
		t.Fatalf("-locked without lock: exit %d: %s", code, stderr)
	}
	code, out, stderr := runCLI(t, "lock", "-f", file)
	if code != 0 || !strings.Contains(out, baseRef+" "+first) {
		t.Fatalf("lock exit %d out %q: %s", code, out, stderr)
	}

	baseDigest := func() string {
		t.Helper()
		a, err := image.ReadLayout(filepath.Join(dir, "out"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := a.(v1.Image).Manifest()
		if err != nil {
			t.Fatal(err)
		}
		if m.Annotations["org.opencontainers.image.base.name"] != baseRef {
			t.Errorf("base.name = %q", m.Annotations["org.opencontainers.image.base.name"])
		}
		return m.Annotations["org.opencontainers.image.base.digest"]
	}

	// The tag moves, but the locked build keeps the old base.
	second := pushBase("V=2")
	if code, _, stderr := runCLI(t, "build", "-f", file, "-locked"); code != 0 {
		t.Fatalf("locked build exit %d: %s", code, stderr)
	}
	if got := baseDigest(); got != first {
		t.Errorf("locked build used base %s, want %s", got, first)
	}
	// Relocking picks up the new base.
	if code, _, stderr := runCLI(t, "lock", "-f", file); code != 0 {
		t.Fatalf("relock exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "build", "-f", file); code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	if got := baseDigest(); got != second {
		t.Errorf("relocked build used base %s, want %s", got, second)
	}

	// A base missing from the lock fails -locked and is added otherwise.
	other := u.Host + "/base:other"
	if _, err := image.Push(ctx, mustBuildScratch(t), other, image.Options{}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "build", "-f", file, "-base", other, "-locked"); code != 1 || !strings.Contains(stderr, "construct lock") {
		t.Errorf("-locked with unlocked base: exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "build", "-f", file, "-base", other); code != 0 || !strings.Contains(stderr, "updated ") {
		t.Errorf("build adding base: exit %d: %s", code, stderr)
	}
	l, err := image.ReadLock(filepath.Join(dir, "construct.lock"))
	if err != nil || len(l.Bases) != 2 {
		t.Errorf("lock = %+v, %v; want two bases", l, err)
	}

	// Without a build file, lock needs explicit bases and a path.
	lockFile := filepath.Join(t.TempDir(), "x.lock")
	if code, _, stderr := runCLI(t, "lock", "-lock", lockFile, "-base", baseRef); code != 0 {
		t.Errorf("lock -base exit %d: %s", code, stderr)
	}
	for _, args := range [][]string{{"lock"}, {"lock", "-base", baseRef}, {"lock", "-base", baseRef, "-lock", lockFile, "api"}} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

func mustBuildScratch(t *testing.T) v1.Image {
	t.Helper()
	img, err := image.Build(context.Background(), image.Spec{Base: image.Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}, image.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// fakeEngine puts a docker script on PATH that saves what `docker load`
// receives to dir/in.tar, and returns dir.
func fakeEngine(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\n" + strings.ReplaceAll(script, "DIR", dir) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

const saveLoad = `[ "$1" = load ] || exit 3; cat > DIR/in.tar; echo "Loaded image: fake"`

func loadedTar(t *testing.T, dir string) (tags []string, arch string) {
	t.Helper()
	file := filepath.Join(dir, "in.tar")
	m, err := tarball.LoadManifest(func() (io.ReadCloser, error) { return os.Open(file) })
	if err != nil || len(m) != 1 {
		t.Fatalf("loaded tar manifest %+v, %v", m, err)
	}
	img, err := tarball.ImageFromPath(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	return m[0].RepoTags, cf.Architecture
}

func TestBuildLoad(t *testing.T) {
	dir := fakeEngine(t, "podman", saveLoad)
	layout := filepath.Join(t.TempDir(), "layout")
	code, _, stderr := runCLI(t, "build", "-vcs=false", "-load", "-engine", "podman",
		"-platform", "linux/amd64,linux/arm64,linux/riscv64", "-tag", "example.com/app:v1", "-tag", "app:dev", "-oci-layout", layout)
	if code != 0 {
		t.Fatalf("build -load exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "Loaded image: fake") || !strings.Contains(stderr, "into podman") {
		t.Errorf("stderr = %q", stderr)
	}
	tags, arch := loadedTar(t, dir)
	if !slices.Equal(tags, []string{"example.com/app:v1", "app:dev"}) || arch != runtime.GOARCH {
		t.Errorf("loaded tags %q arch %s", tags, arch)
	}

	// construct load reads the layout and uses its ref.name by default.
	os.Remove(filepath.Join(dir, "in.tar"))
	code, out, stderr := runCLI(t, "load", "-engine", "podman", "-platform", "linux/riscv64", layout)
	if code != 0 || !strings.HasPrefix(out, "sha256:") {
		t.Fatalf("load exit %d out %q: %s", code, out, stderr)
	}
	if tags, arch := loadedTar(t, dir); !slices.Equal(tags, []string{"example.com/app:v1"}) || arch != "riscv64" {
		t.Errorf("construct load: tags %q arch %s", tags, arch)
	}
	if code, _, stderr := runCLI(t, "load", "-engine", "podman", "-tag", "other:v2", "-platform", "linux/s390x", layout); code != 1 || !strings.Contains(stderr, "no linux/s390x image") {
		t.Errorf("load missing platform: exit %d: %s", code, stderr)
	}

	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"build", "-load"}, 2},
		{[]string{"build", "-load", "-tag", "a:b", "-engine", "no-such-engine"}, 1},
		{[]string{"build", "-oci-layout", t.TempDir(), "-engine", "podman"}, 2},
		{[]string{"load"}, 2},
		{[]string{"load", t.TempDir()}, 1},
	} {
		if code, _, stderr := runCLI(t, c.args...); code != c.code {
			t.Errorf("%q: exit %d, want %d: %s", c.args, code, c.code, stderr)
		}
	}
}

func TestBuildLoadEngineFails(t *testing.T) {
	// The engine exits without reading its input; the build must report
	// the engine's message rather than hang.
	fakeEngine(t, "docker", `echo "Cannot connect to the Docker daemon" >&2; exit 1`)
	code, _, stderr := runCLI(t, "build", "-vcs=false", "-load", "-tag", "app:v1")
	if code != 1 || !strings.Contains(stderr, "Cannot connect to the Docker daemon") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

func TestBuildRootfs(t *testing.T) {
	src := t.TempDir()
	ca := filepath.Join(src, "ca.pem")
	if err := os.WriteFile(ca, []byte("PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "out")
	code, _, stderr := runCLI(t, "build", "-vcs=false", "-platform", "linux/amd64", "-skeleton", "-add-user", "app:1000",
		"-ca-certs", ca, "-user", "app", "-add", ca+":/app/ca.pem", "-oci-layout", layout)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	a, err := image.ReadLayout(layout)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := a.(v1.Image).Layers()
	if err != nil || len(layers) != 2 {
		t.Fatalf("layers = %d, %v; want rootfs + app", len(layers), err)
	}
	cf, _ := a.(v1.Image).ConfigFile()
	if h := cf.History[0].CreatedBy; !strings.Contains(h, "layer rootfs") {
		t.Errorf("first layer history %q", h)
	}

	// A base image's /etc/passwd must not be replaced.
	for _, args := range [][]string{
		{"build", "-base", "example.com/base:v1", "-skeleton", "-oci-layout", layout},
		{"build", "-add-user", "Bad", "-oci-layout", layout},
		{"build", "-tzdata", filepath.Join(src, "missing"), "-oci-layout", layout},
	} {
		code, _, stderr := runCLI(t, args...)
		want := 1
		if args[1] == "-base" {
			want = 2
		}
		if code != want {
			t.Errorf("%q: exit %d, want %d: %s", args, code, want, stderr)
		}
	}
}

func TestStructureTests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("hello v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `
vcs: false
platforms: [linux/amd64, linux/arm64]
oci-layout: out
layers:
  - contents: ["app:/app"]
tests:
  - {file: /app, mode: "0755", contains: v2}
  - {absent: /bin/sh}
  - config: {entrypoint: [/app]}
entrypoint: [/app]
`
	file := filepath.Join(dir, "construct.yaml")
	if err := os.WriteFile(file, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCLI(t, "test", "-f", file)
	if code != 0 || out != "ok\n" || !strings.Contains(stderr, "6 tests passed") {
		t.Fatalf("test exit %d out %q: %s", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Error("construct test wrote the oci-layout output")
	}

	// Build runs the tests too; a failing test stops the outputs.
	code, _, stderr = runCLI(t, "build", "-f", file, "-entrypoint", "/other")
	if code != 1 || !strings.Contains(stderr, "FAIL linux/amd64: config: entrypoint") || !strings.Contains(stderr, "2 of 6 tests failed") {
		t.Errorf("failing build exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Error("failed tests still wrote the output")
	}
	if code, _, stderr := runCLI(t, "build", "-f", file, "-entrypoint", "/other", "-test=false"); code != 0 {
		t.Errorf("-test=false exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "build", "-f", file); code != 0 || !strings.Contains(stderr, "6 tests passed") {
		t.Fatalf("build exit %d: %s", code, stderr)
	}

	// -checks runs a test file against an existing layout.
	checks := filepath.Join(dir, "checks.yaml")
	if err := os.WriteFile(checks, []byte("tests:\n  - {file: /app, contains: v3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCLI(t, "test", "-checks", checks, filepath.Join(dir, "out"))
	if code != 1 || strings.Count(stderr, "FAIL ") != 2 {
		t.Errorf("-checks exit %d: %s", code, stderr)
	}

	// The build file itself works as a -checks file.
	if code, _, stderr := runCLI(t, "test", "-checks", file, filepath.Join(dir, "out")); code != 0 || !strings.Contains(stderr, "6 tests passed") {
		t.Errorf("-checks build file: exit %d: %s", code, stderr)
	}
	multi := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(multi, []byte("images:\n  - {name: a, tests: [{absent: /x}]}\n  - {name: b, tests: [{file: /x}]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "test", "-checks", multi, filepath.Join(dir, "out")); code != 1 || !strings.Contains(stderr, "-image") {
		t.Errorf("multi without -image: exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "test", "-checks", multi, "-image", "a", filepath.Join(dir, "out")); code != 0 {
		t.Errorf("multi -image a: exit %d: %s", code, stderr)
	}

	for _, args := range [][]string{
		{"test"},
		{"test", "-f", file, "-checks", checks},
		{"test", "-checks", checks},
		{"test", "-f", file, "-image", "a"},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	if err := os.WriteFile(checks, []byte("tests:\n  - {file: relative}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "test", "-checks", checks, filepath.Join(dir, "out")); code != 1 || !strings.Contains(stderr, "absolute") {
		t.Errorf("bad check file: exit %d: %s", code, stderr)
	}
}

func TestStructureTestsRegistrySource(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	ref := u.Host + "/app:v1"
	if code, _, stderr := runCLI(t, "build", "-vcs=false", "-platform", "linux/amd64", "-mkdir", "/data", "-push", "-tag", ref); code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	checks := filepath.Join(t.TempDir(), "checks.yaml")
	if err := os.WriteFile(checks, []byte("tests:\n  - {file: /data, type: dir}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := runCLI(t, "test", "-checks", checks, ref); code != 0 || out != "ok\n" {
		t.Errorf("test registry exit %d out %q: %s", code, out, stderr)
	}
	if code, _, _ := runCLI(t, "test", "-checks", checks, u.Host+"/missing:v1"); code != 1 {
		t.Errorf("missing image: exit %d, want 1", code)
	}
}

func TestBuildLayoutThenPush(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	src := filepath.Join(t.TempDir(), "app")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "layout")
	tag := u.Host + "/demo/app:v1"

	code, out, stderr := runCLI(t, "build",
		"-platform", "linux/amd64",
		"-add", src+":/usr/local/bin/app",
		"-entrypoint", "/usr/local/bin/app",
		"-env", "MODE=prod",
		"-label", "team=infra",
		"-tag", tag,
		"-oci-layout", dir,
	)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	digest := strings.TrimSpace(out)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("build printed %q, want a digest", out)
	}

	code, out, stderr = runCLI(t, "push", dir, tag)
	if code != 0 {
		t.Fatalf("push exit %d: %s", code, stderr)
	}
	if want := u.Host + "/demo/app@" + digest; strings.TrimSpace(out) != want {
		t.Errorf("push printed %q, want %q", out, want)
	}

	ref, _ := name.ParseReference(tag)
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	if cf.Created.Unix() != 1700000000 || cf.Config.Labels["team"] != "infra" || !slices.Contains(cf.Config.Env, "MODE=prod") {
		t.Errorf("config = %+v", cf)
	}

	// Building straight to the registry yields the same digest.
	code, out, stderr = runCLI(t, "build",
		"-platform", "linux/amd64",
		"-add", src+":/usr/local/bin/app",
		"-entrypoint", "/usr/local/bin/app",
		"-env", "MODE=prod",
		"-label", "team=infra",
		"-tag", u.Host+"/demo/app:direct",
		"-push",
	)
	if code != 0 {
		t.Fatalf("build -push exit %d: %s", code, stderr)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "@"+digest) {
		t.Errorf("build -push printed %q, want digest %s", out, digest)
	}
}

func TestBuildMultiPlatform(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	src := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(src, "app-linux-"+arch), []byte(arch), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tag := u.Host + "/demo/multi:v1"
	code, out, stderr := runCLI(t, "build",
		"-platform", "linux/amd64,linux/arm64",
		"-add", filepath.Join(src, "app-{os}-{arch}")+":/app",
		"-entrypoint", "/app",
		"-tag", tag,
		"-push",
	)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	ref, err := name.ParseReference(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("build printed %q: %v", out, err)
	}
	idx, err := remote.Index(ref)
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range im.Manifests {
		got = append(got, d.Platform.String())
	}
	if want := []string{"linux/amd64", "linux/arm64"}; !slices.Equal(got, want) {
		t.Errorf("platforms = %q, want %q", got, want)
	}
}

func mustRepo(t *testing.T, s string) name.Repository {
	t.Helper()
	r, err := name.NewRepository(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
