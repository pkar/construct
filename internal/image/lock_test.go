package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestLock(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	base, err := Build(ctx, Spec{Base: Scratch, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/base:v1"
	pushed, err := Push(ctx, base, ref, anonymous)
	if err != nil {
		t.Fatal(err)
	}

	l, err := LockBases(ctx, []string{ref, Scratch, "", ref, pushed.String()}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Bases) != 1 || l.Bases[ref] != pushed.DigestStr() {
		t.Fatalf("lock = %+v, want only %s -> %s", l.Bases, ref, pushed.DigestStr())
	}
	path := filepath.Join(t.TempDir(), "construct.lock")
	if err := l.Write(path); err != nil {
		t.Fatal(err)
	}
	read, err := ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := read.Pin(ref, anonymous)
	if err != nil || got != pushed.String() {
		t.Errorf("Pin = %q, %v; want %s", got, err, pushed)
	}
	for _, same := range []string{Scratch, "", pushed.String()} {
		if got, err := read.Pin(same, anonymous); err != nil || got != same {
			t.Errorf("Pin(%q) = %q, %v", same, got, err)
		}
	}
	if _, err := read.Pin(host+"/other:v1", anonymous); !errors.Is(err, ErrNotLocked) {
		t.Errorf("Pin(unlocked) err = %v", err)
	}
	var none *Lock
	if _, err := none.Pin(ref, anonymous); !errors.Is(err, ErrNotLocked) {
		t.Errorf("nil Pin err = %v", err)
	}

	if _, err := LockBases(ctx, []string{host + "/missing:v1"}, anonymous); err == nil {
		t.Error("LockBases(missing) succeeded")
	}
	for name, body := range map[string]string{
		"json":    "{",
		"version": `{"version": 2, "bases": {}}`,
		"digest":  `{"version": 1, "bases": {"a:b": "sha256:nope"}}`,
	} {
		bad := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLock(bad); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%s: ReadLock err = %v", name, err)
		}
	}
}

func TestBuildBaseAnnotations(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	p := v1.Platform{OS: "linux", Architecture: "amd64"}
	base, err := Build(ctx, Spec{Base: Scratch, Platform: p}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := Push(ctx, base, host+"/base:v1", anonymous)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Build(ctx, Spec{Base: pushed.String(), BaseName: host + "/base:v1", Platform: p}, anonymous)
	if err != nil {
		t.Fatal(err)
	}
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Annotations["org.opencontainers.image.base.name"] != host+"/base:v1" || m.Annotations["org.opencontainers.image.base.digest"] != pushed.DigestStr() {
		t.Errorf("annotations = %v", m.Annotations)
	}
	if m, _ := base.Manifest(); len(m.Annotations) != 0 {
		t.Errorf("scratch image annotations = %v", m.Annotations)
	}
}
