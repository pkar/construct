package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/pkar/construct/internal/image"
	"github.com/pkar/construct/internal/run/runtest"
)

func TestMain(m *testing.M) {
	runtest.Serve()
	os.Exit(m.Run())
}

func baseDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"etc/os-release": "ID=fake\n",
		"etc/old":        "remove me\n",
		"bin/keep":       "keep\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testRequest() image.RunRequest {
	return image.RunRequest{
		Image:    "example.com/base@sha256:" + strings.Repeat("a", 64),
		Platform: v1.Platform{OS: "linux", Architecture: "arm64"},
		Names:    []string{"deps", "tweak"},
		Steps: []image.RunStep{
			{Script: `mkdir -p usr/local/bin && printf "$GREETING" > usr/local/bin/tool && chmod 755 usr/local/bin/tool`, Env: []string{"GREETING=hello"}},
			{Script: "rm etc/old"},
		},
	}
}

func layerNames(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	names, got := readLayer(t, data, time.Unix(0, 0).UTC())
	if e, ok := got["usr/local/bin/tool"]; ok && (e.body != "hello" || e.mode != 0o755) {
		t.Errorf("usr/local/bin/tool = %+v", e)
	}
	return names
}

func newRunner(t *testing.T, engines ...runtest.Engine) (*Runner, *bytes.Buffer) {
	var log bytes.Buffer
	r := &Runner{CacheDir: t.TempDir(), Created: time.Unix(0, 0).UTC(), Log: &log, Prefix: "app"}
	for _, e := range engines {
		r.Engines = append(r.Engines, Engine{Name: e.Name, Path: e.Path})
	}
	return r, &log
}

func TestRunner(t *testing.T) {
	eng := runtest.Install(t, "podman", baseDir(t), false)
	r, log := newRunner(t, eng)
	ctx := context.Background()
	files, err := r.Run(ctx, testRequest())
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if len(files) != 2 {
		t.Fatalf("got %d layers", len(files))
	}
	if got := strings.Join(layerNames(t, files[0]), " "); got != "usr/ usr/local/ usr/local/bin/ usr/local/bin/tool" {
		t.Errorf("deps layer: %s", got)
	}
	if got := strings.Join(layerNames(t, files[1]), " "); got != "etc/.wh.old" {
		t.Errorf("tweak layer: %s", got)
	}
	calls := eng.Calls(t)
	var verbs []string
	for _, c := range calls {
		verbs = append(verbs, strings.Fields(c)[0])
	}
	if got := strings.Join(verbs, " "); got != "run exec export exec export exec export rm" {
		t.Errorf("engine calls: %s", got)
	}
	if !strings.Contains(calls[0], "--platform linux/arm64") || !strings.Contains(calls[0], "--entrypoint /bin/sh "+testRequest().Image+" -c ") {
		t.Errorf("run call: %s", calls[0])
	}
	if !strings.Contains(calls[3], "exec -u 0 -e GREETING=hello") {
		t.Errorf("exec call: %s", calls[3])
	}
	used := r.Used()
	if len(used) != 2 || used["app/deps/linux/arm64"].Layer == "" || used["app/tweak/linux/arm64"].Inputs == "" {
		t.Errorf("used: %v", used)
	}

	// A second run comes from the cache without touching the engine.
	again, err := r.Run(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != files[0] || len(eng.Calls(t)) != len(calls) {
		t.Errorf("second run was not cached: %d engine calls", len(eng.Calls(t)))
	}

	// Changing the second script reruns, and keeps the first layer's key.
	req := testRequest()
	req.Steps[1].Script = "rm etc/old && echo x > etc/new"
	changed, err := r.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if changed[0] != files[0] || changed[1] == files[1] {
		t.Errorf("keys: %v vs %v", changed, files)
	}

	// Refresh ignores the cache.
	r.Refresh = true
	n := len(eng.Calls(t))
	if _, err := r.Run(ctx, testRequest()); err != nil {
		t.Fatal(err)
	}
	if len(eng.Calls(t)) == n {
		t.Error("refresh did not run the engine")
	}
}

func TestRunnerLocked(t *testing.T) {
	eng := runtest.Install(t, "docker", baseDir(t), false)
	r, _ := newRunner(t, eng)
	ctx := context.Background()
	if _, err := r.Run(ctx, testRequest()); err != nil {
		t.Fatal(err)
	}
	pins := r.Used()

	r.Locked, r.Pins = true, pins
	if _, err := r.Run(ctx, testRequest()); err != nil {
		t.Errorf("locked run with matching pins: %v", err)
	}

	r.Pins = map[string]image.RunPin{"app/deps/linux/arm64": pins["app/deps/linux/arm64"]}
	if _, err := r.Run(ctx, testRequest()); err == nil || !strings.Contains(err.Error(), "tweak (linux/arm64) is not in the lock file") {
		t.Errorf("missing pin: %v", err)
	}

	req := testRequest()
	req.Steps[0].Script += " && true"
	r.Pins = pins
	if _, err := r.Run(ctx, req); err == nil || !strings.Contains(err.Error(), "scripts changed") {
		t.Errorf("changed inputs: %v", err)
	}

	bad := map[string]image.RunPin{}
	for k, v := range pins {
		v.Layer = "sha256:" + strings.Repeat("0", 64)
		bad[k] = v
	}
	r.Pins = bad
	if _, err := r.Run(ctx, testRequest()); err == nil || !strings.Contains(err.Error(), "not reproducible") {
		t.Errorf("different layer: %v", err)
	}
}

func TestRunnerEngineFallback(t *testing.T) {
	base := baseDir(t)
	down := runtest.Install(t, "container", base, true)
	up := runtest.Install(t, "docker", base, false)
	r, log := newRunner(t, down, up)
	if _, err := r.Run(context.Background(), testRequest()); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "in docker") {
		t.Errorf("log:\n%s", log)
	}
	if calls := down.Calls(t); len(calls) != 2 || !strings.HasPrefix(calls[1], "rm -f construct-run-") {
		t.Errorf("failed engine calls: %v", calls)
	}
}

func TestRunnerNoEngineWorks(t *testing.T) {
	down := runtest.Install(t, "podman", baseDir(t), true)
	r, _ := newRunner(t, down)
	_, err := r.Run(context.Background(), testRequest())
	if err == nil || !strings.Contains(err.Error(), "no container engine could run linux/arm64") || !strings.Contains(err.Error(), "cannot connect to the engine") {
		t.Errorf("err = %v", err)
	}
}

func TestRunnerScriptFails(t *testing.T) {
	eng := runtest.Install(t, "docker", baseDir(t), false)
	r, _ := newRunner(t, eng)
	req := testRequest()
	req.Steps[1].Script = "echo boom >&2; exit 3"
	_, err := r.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "run layer tweak (linux/arm64): script failed: exit status 3") {
		t.Errorf("err = %v", err)
	}
	calls := eng.Calls(t)
	if !strings.HasPrefix(calls[len(calls)-1], "rm -f ") {
		t.Errorf("container not removed: %v", calls)
	}
	// The failed step leaves nothing in the cache.
	entries, _ := os.ReadDir(filepath.Join(r.CacheDir, "run"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left over %s", e.Name())
		}
	}
}

func TestFindEngines(t *testing.T) {
	base := baseDir(t)
	runtest.Install(t, "podman", base, false)
	runtest.Install(t, "container", base, false)
	engines, err := FindEngines("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range engines {
		names = append(names, e.Name)
	}
	// docker may be installed on the machine running the tests.
	got := strings.Join(names, " ")
	if !strings.HasPrefix(got, "container ") || !strings.HasSuffix(got, " podman") {
		t.Errorf("engines: %s", got)
	}
	if _, err := FindEngines("no-such-engine"); err == nil {
		t.Error("no error for a missing engine")
	}
}
