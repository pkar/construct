package image

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// recordingRegistry is an in-process registry that records every request
// as "METHOD PATH".
type recordingRegistry struct {
	host string
	mu   sync.Mutex
	reqs []string
}

func newRecordingRegistry(t *testing.T) *recordingRegistry {
	t.Helper()
	r := &recordingRegistry{}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reqs = append(r.reqs, req.Method+" "+req.URL.Path)
		r.mu.Unlock()
		reg.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.host = u.Host
	return r
}

func (r *recordingRegistry) reset() {
	r.mu.Lock()
	r.reqs = nil
	r.mu.Unlock()
}

// blobGets returns the GETs of the given blobs.
func (r *recordingRegistry) blobGets(digests []v1.Hash) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, req := range r.reqs {
		for _, d := range digests {
			if strings.HasPrefix(req, "GET ") && strings.HasSuffix(req, "/blobs/"+d.String()) {
				out = append(out, req)
			}
		}
	}
	return out
}

// A build on a registry base only needs the base's manifest and config.
// Pushing to the same registry mounts the base layers into the target
// repository instead of downloading and re-uploading them, so a large
// base costs nothing to build on.
func TestShallowBase(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "big"), strings.Repeat("base layer ", 100000), 0o644)
	writeFile(t, filepath.Join(src, "app"), "app", 0o755)
	platforms := []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}

	reg := newRecordingRegistry(t)
	base, err := BuildIndex(ctx, Spec{Base: Scratch, Layers: copies(filepath.Join(src, "big"), "/big")}, platforms, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	baseRef := reg.host + "/base:v1"
	if _, err := Push(ctx, base, baseRef, anonymous); err != nil {
		t.Fatal(err)
	}
	var baseLayers []v1.Hash
	im, err := base.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range im.Manifests {
		img, err := base.Image(d.Digest)
		if err != nil {
			t.Fatal(err)
		}
		m, err := img.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range m.Layers {
			baseLayers = append(baseLayers, l.Digest)
		}
	}

	reg.reset()
	app, err := BuildIndex(ctx, Spec{Base: baseRef, Layers: copies(filepath.Join(src, "app"), "/app")}, platforms, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PushAll(ctx, app, []string{reg.host + "/app:v1", reg.host + "/app:latest", reg.host + "/mirror:v1"}, anonymous); err != nil {
		t.Fatal(err)
	}
	if gets := reg.blobGets(baseLayers); len(gets) != 0 {
		t.Errorf("same-registry push downloaded base layers:\n%s", strings.Join(gets, "\n"))
	}

	// A different registry has to receive the bytes, so they are read
	// from the source registry once per layer and target repository.
	other := newRecordingRegistry(t)
	reg.reset()
	if _, err := Push(ctx, app, other.host+"/app:v1", anonymous); err != nil {
		t.Fatal(err)
	}
	if gets := reg.blobGets(baseLayers); len(gets) == 0 {
		t.Error("cross-registry push read no base layers; the test is not measuring what it claims")
	}
}
