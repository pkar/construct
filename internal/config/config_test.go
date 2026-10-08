package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pkar/construct/internal/image"
)

const sample = `
# Defaults for every image.
base: gcr.io/distroless/static:nonroot
platforms: [linux/amd64, linux/arm64]
env: {TZ: UTC, LANG: C.UTF-8}
labels: {team: core}
compression: zstd
layers:
  - name: certs
    contents:
      - certs/ca.pem:/etc/ssl/certs/ca.pem:mode=0444

images:
  - name: api
    tags:
      - registry.example.com/api:{git.short}
      - registry.example.com/api:latest
    push: true
    entrypoint: /usr/local/bin/api --serve
    env: [PORT=8080]
    labels: {component: api}
    expose: [8080, 9090/udp]
    layers:
      - name: app
        contents:
          - src: dist/api-{arch}
            dst: /usr/local/bin/api
            mode: 0755
            owner: 65532
          - mkdir: /data
            mode: "0700"
            owner: 65532:65532
          - symlink: /api
            target: /usr/local/bin/api
          - file: /etc/motd
            content: |
              hello
  - name: worker
    base: scratch
    platforms: linux/amd64
    oci-layout: out/worker
    cmd: []
`

func TestParse(t *testing.T) {
	f, err := Parse([]byte(sample), "/src")
	if err != nil {
		t.Fatal(err)
	}
	images, err := f.Select(nil)
	if err != nil || len(images) != 2 {
		t.Fatalf("Select = %d images, %v", len(images), err)
	}
	api, worker := images[0], images[1]

	if *api.Base != "gcr.io/distroless/static:nonroot" || *worker.Base != "scratch" {
		t.Errorf("bases %q %q", *api.Base, *worker.Base)
	}
	if !reflect.DeepEqual(api.Platforms, StringList{"linux/amd64", "linux/arm64"}) || !reflect.DeepEqual(worker.Platforms, StringList{"linux/amd64"}) {
		t.Errorf("platforms %q %q", api.Platforms, worker.Platforms)
	}
	if !reflect.DeepEqual(api.Env, EnvList{"LANG=C.UTF-8", "TZ=UTC", "PORT=8080"}) {
		t.Errorf("env %q", api.Env)
	}
	if !reflect.DeepEqual(api.Labels, map[string]string{"team": "core", "component": "api"}) || !reflect.DeepEqual(worker.Labels, map[string]string{"team": "core"}) {
		t.Errorf("labels %v %v", api.Labels, worker.Labels)
	}
	if !reflect.DeepEqual(*api.Entrypoint, Command{"/usr/local/bin/api", "--serve"}) || api.Cmd != nil {
		t.Errorf("api entrypoint %q cmd %v", *api.Entrypoint, api.Cmd)
	}
	if worker.Cmd == nil || len(*worker.Cmd) != 0 {
		t.Errorf("worker cmd = %v, want set and empty", worker.Cmd)
	}
	if !reflect.DeepEqual(api.Expose, StringList{"8080", "9090/udp"}) {
		t.Errorf("expose %q", api.Expose)
	}
	if *worker.OCILayout != filepath.Join("/src", "out/worker") || *api.Compression != "zstd" {
		t.Errorf("worker layout %q, api compression %q", *worker.OCILayout, *api.Compression)
	}

	if len(worker.Layers) != 1 || len(api.Layers) != 2 || api.Layers[0].Name != "certs" || api.Layers[1].Name != "app" {
		t.Fatalf("layers: api %+v worker %+v", api.Layers, worker.Layers)
	}
	mode := func(m int64) *int64 { return &m }
	cert := api.Layers[0].Contents[0].Item
	if cert.Src != filepath.Join("/src", "certs/ca.pem") || *cert.Mode != 0o444 {
		t.Errorf("cert item %+v", cert)
	}
	var got []image.Item
	for _, it := range api.Layers[1].Contents {
		got = append(got, it.Item)
	}
	want := []image.Item{
		{Kind: image.Copy, Src: filepath.Join("/src", "dist/api-{arch}"), Dst: "/usr/local/bin/api", Mode: mode(0o755), Owner: &image.Owner{UID: 65532, GID: 65532}},
		{Kind: image.Dir, Dst: "/data", Mode: mode(0o700), Owner: &image.Owner{UID: 65532, GID: 65532}},
		{Kind: image.Symlink, Dst: "/api", Target: "/usr/local/bin/api"},
		{Kind: image.File, Dst: "/etc/motd", Content: []byte("hello\n")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("app items:\n got %+v\nwant %+v", got, want)
	}

	sel, err := f.Select([]string{"worker"})
	if err != nil || len(sel) != 1 || sel[0].Name != "worker" {
		t.Errorf("Select(worker) = %+v, %v", sel, err)
	}
	if _, err := f.Select([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "api, worker") {
		t.Errorf("Select(nope) err = %v", err)
	}
}

func TestParseSingleImage(t *testing.T) {
	f, err := Parse([]byte("base: alpine\ntags: app:v1\npush: true\n"), "/src")
	if err != nil {
		t.Fatal(err)
	}
	images, err := f.Select(nil)
	if err != nil || len(images) != 1 || *images[0].Base != "alpine" || images[0].Tags[0] != "app:v1" {
		t.Errorf("images = %+v, %v", images, err)
	}
}

func TestParseErrors(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":           "",
		"unknown field":   "bse: alpine\n",
		"unnamed images":  "images:\n  - base: a\n  - base: b\n",
		"duplicate names": "images:\n  - name: a\n  - name: a\n",
		"top-level name":  "name: x\nimages:\n  - name: a\n",
		"two kinds":       "layers:\n  - contents:\n      - {src: a, dst: /a, mkdir: /b}\n",
		"no kind":         "layers:\n  - contents:\n      - {mode: \"0755\"}\n",
		"unknown item":    "layers:\n  - contents:\n      - {src: a, dst: /a, colour: red}\n",
		"stray target":    "layers:\n  - contents:\n      - {src: a, dst: /a, target: /b}\n",
		"stray content":   "layers:\n  - contents:\n      - {mkdir: /a, content: x}\n",
		"bad mode":        "layers:\n  - contents:\n      - {mkdir: /a, mode: rwx}\n",
		"relative dst":    "layers:\n  - contents:\n      - {src: a, dst: a}\n",
		"bad shorthand":   "layers:\n  - contents:\n      - a\n",
		"bad port type":   "expose: {a: b}\n",
	} {
		if _, err := Parse([]byte(doc), "/src"); err == nil {
			t.Errorf("%s: Parse succeeded", name)
		}
	}
}

func TestMergeFlagsOverFile(t *testing.T) {
	str := func(s string) *string { return &s }
	file := Image{
		Base:   str("alpine"),
		Tags:   StringList{"a:1"},
		Env:    EnvList{"A=1"},
		Layers: []Layer{{Name: "app"}},
		Labels: map[string]string{"x": "1", "y": "1"},
	}
	flags := Image{
		Tags:   StringList{"b:2"},
		Env:    EnvList{"B=2"},
		Layers: []Layer{{Name: "extra"}},
		Labels: map[string]string{"y": "2"},
	}
	got := Merge(file, flags)
	if *got.Base != "alpine" || !reflect.DeepEqual(got.Tags, StringList{"b:2"}) || !reflect.DeepEqual(got.Env, EnvList{"A=1", "B=2"}) ||
		len(got.Layers) != 2 || !reflect.DeepEqual(got.Labels, map[string]string{"x": "1", "y": "2"}) {
		t.Errorf("Merge = %+v", got)
	}
	// Merging must not write through to the inputs.
	if len(file.Env) != 1 || file.Labels["y"] != "1" {
		t.Errorf("Merge modified its input: %+v", file)
	}
}

func TestExampleFile(t *testing.T) {
	f, err := Load(filepath.Join("..", "..", "examples", "construct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	images, err := f.Select(nil)
	if err != nil || len(images) != 2 {
		t.Fatalf("images = %d, %v", len(images), err)
	}
	api, migrate := images[0], images[1]
	if len(api.Tests) != 4 || len(api.Layers) != 1 || len(api.Layers[0].Contents) != 4 || api.Rootfs.Image().CACerts != image.System {
		t.Errorf("api = %+v", api)
	}
	if migrate.Tags[0] != "registry.example.com/team/migrate:{git.short}" || len(*migrate.Entrypoint) != 2 {
		t.Errorf("migrate = %+v", migrate)
	}
}

func TestParseRootfs(t *testing.T) {
	doc := "rootfs: {skeleton: true, users: [\"a:1\"], ca-certs: certs/ca.pem, tzdata: system}\n" +
		"images:\n  - name: x\n    rootfs: {users: [\"b:2\"]}\n"
	f, err := Parse([]byte(doc), "/src")
	if err != nil {
		t.Fatal(err)
	}
	images, err := f.Select(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := images[0].Rootfs.Image()
	want := image.Rootfs{Skeleton: true, Users: []string{"a:1", "b:2"}, CACerts: filepath.Join("/src", "certs/ca.pem"), Tzdata: image.System}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rootfs = %+v, want %+v", got, want)
	}
	if (*Rootfs)(nil).Image().Empty() != true {
		t.Error("nil rootfs not empty")
	}
}

func TestLoadResolvesAgainstFileDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "construct.yaml")
	if err := os.WriteFile(path, []byte("layers:\n  - contents: [bin/app:/app]\ntarball: out.tar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Dir != dir || f.Images[0].Layers[0].Contents[0].Src != filepath.Join(dir, "bin/app") || *f.Images[0].Tarball != filepath.Join(dir, "out.tar") {
		t.Errorf("file = %+v", f.Images[0])
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load(missing) succeeded")
	}
}
