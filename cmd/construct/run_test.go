package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/layout"

	"github.com/pkar/construct/internal/image"
	"github.com/pkar/construct/internal/run/runtest"
)

func TestMain(m *testing.M) {
	runtest.Serve()
	os.Exit(m.Run())
}

func TestBuildRunLayers(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("CONSTRUCT_CACHE_DIR", t.TempDir())
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	base := u.Host + "/base:v1"
	if code, _, stderr := runCLI(t, "build", "-vcs=false", "-platform", "linux/amd64,linux/arm64",
		"-mkdir", "/etc", "-push", "-tag", base); code != 0 {
		t.Fatalf("base build exit %d: %s", code, stderr)
	}

	// The fake engine's containers start from this directory.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng := runtest.Install(t, "docker", root, false)

	dir := t.TempDir()
	file := filepath.Join(dir, "construct.yaml")
	yaml := `base: ` + base + `
platforms: [linux/amd64, linux/arm64]
run-engine: docker
oci-layout: out
layers:
  - name: deps
    run: mkdir -p opt && echo "$WHO" > opt/who
    env: {WHO: builder}
  - name: app
    contents:
      - mkdir: /data
tests:
  - {file: /opt/who, contains: builder}
  - {file: /data, type: dir}
`
	if err := os.WriteFile(file, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	lockFile := filepath.Join(dir, "construct.lock")
	code, out, stderr := runCLI(t, "build", "-f", file, "-lock", lockFile)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr)
	}
	digest := strings.TrimSpace(out)
	if !strings.Contains(stderr, "4 tests passed") || !strings.Contains(stderr, "run layer deps (linux/arm64): 2 changed, 0 deleted") {
		t.Errorf("tests did not run: %s", stderr)
	}
	var runs []string
	for _, c := range eng.Calls(t) {
		if strings.HasPrefix(c, "run ") {
			runs = append(runs, c)
		}
	}
	if len(runs) != 2 || !strings.Contains(runs[0], "--platform linux/amd64") || !strings.Contains(runs[1], "--platform linux/arm64") ||
		!strings.Contains(runs[0], u.Host+"/base@sha256:") {
		t.Errorf("engine runs: %q", runs)
	}

	lock, err := image.ReadLock(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"image/deps/linux/amd64", "image/deps/linux/arm64"} {
		if pin := lock.Runs[id]; pin.Inputs == "" || pin.Layer == "" {
			t.Errorf("lock has no pin for %s: %+v", id, lock.Runs)
		}
	}

	// The run layer sits below the file layer.
	idx, err := layout.ImageIndexFromPath(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	im, _ := idx.IndexManifest()
	inner, _ := idx.ImageIndex(im.Manifests[0].Digest)
	im2, _ := inner.IndexManifest()
	img, _ := inner.Image(im2.Manifests[0].Digest)
	cf, _ := img.ConfigFile()
	var hist []string
	for _, h := range cf.History {
		hist = append(hist, h.CreatedBy)
	}
	if len(hist) != 3 || hist[1] != `construct: layer deps: run: mkdir -p opt && echo "$WHO" > opt/who` ||
		hist[2] != "construct: layer app: mkdir /data" {
		t.Errorf("history: %q", hist)
	}

	// -locked uses the cache and the engine is not started.
	n := len(eng.Calls(t))
	code, out, stderr = runCLI(t, "build", "-f", file, "-locked")
	if code != 0 || strings.TrimSpace(out) != digest {
		t.Errorf("locked rebuild exit %d, digest %q want %q: %s", code, out, digest, stderr)
	}
	if len(eng.Calls(t)) != n {
		t.Error("locked rebuild ran the engine")
	}

	// -refresh reruns the script; the same changes give the same image.
	code, out, stderr = runCLI(t, "build", "-f", file, "-refresh")
	if code != 0 || strings.TrimSpace(out) != digest {
		t.Errorf("refresh exit %d, digest %q want %q: %s", code, out, digest, stderr)
	}
	if len(eng.Calls(t)) == n {
		t.Error("refresh did not run the engine")
	}

	// A different script is not in the lock.
	edited := strings.Replace(yaml, `"$WHO"`, `"$WHO!"`, 1)
	if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCLI(t, "build", "-f", file, "-locked")
	if code != 1 || !strings.Contains(stderr, "scripts changed since the lock file was written") {
		t.Errorf("locked build with a changed script: exit %d: %s", code, stderr)
	}
}

func TestRunLayerUsageErrors(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("CONSTRUCT_CACHE_DIR", t.TempDir())
	out := t.TempDir()
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"scratch", []string{"-run", "true"}, "not scratch"},
		{"after files", []string{"-base", "example.com/base:v1", "-add", src + ":/f", "-run", "true"}, "comes after a file layer"},
		{"refresh and locked", []string{"-refresh", "-locked", "-run", "true"}, "-refresh and -locked conflict"},
		{"empty script", []string{"-run", " "}, "want a script"},
		{"missing engine", []string{"-base", "example.com/base:v1", "-run", "true", "-run-engine", "no-such-engine"}, "no-such-engine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"build", "-vcs=false", "-oci-layout", out}, tc.args...)
			code, _, stderr := runCLI(t, args...)
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d: %s", code, stderr)
			}
		})
	}
}

func TestRunFlagLayers(t *testing.T) {
	var l layerFlags
	set := func(f interface{ Set(string) error }, v string) {
		t.Helper()
		if err := f.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	set(layerFlag{&l}, "deps")
	set(runFlag{&l}, "apt-get update")
	set(runFlag{&l}, "apt-get install -y curl")
	set(itemFlag{&l, image.ParseMkdir}, "/data")
	if len(l.layers) != 3 || l.layers[0].Name != "deps" || l.layers[0].Run != "apt-get update" ||
		l.layers[1].Run != "apt-get install -y curl" || len(l.layers[2].Contents) != 1 || l.layers[2].Run != "" {
		t.Errorf("layers: %+v", l.layers)
	}
}
