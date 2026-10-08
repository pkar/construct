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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

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
