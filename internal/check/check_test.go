package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"gopkg.in/yaml.v3"

	"github.com/pkar/construct/internal/image"
)

func testImage(t *testing.T) v1.Image {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app"), []byte("version 1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mode, owner := int64(0o750), &image.Owner{UID: 1000, GID: 1000}
	layer := func(items ...image.Item) image.LayerSpec { return image.LayerSpec{Items: items} }
	img, err := image.Build(context.Background(), image.Spec{
		Base:         image.Scratch,
		Platform:     v1.Platform{OS: "linux", Architecture: "amd64"},
		Entrypoint:   []string{"/app"},
		Env:          []string{"PORT=8080", "MODE=prod"},
		User:         "1000",
		WorkDir:      "/data",
		Labels:       map[string]string{"team": "core"},
		ExposedPorts: []string{"8080/tcp"},
		Layers: []image.LayerSpec{
			layer(
				image.Item{Kind: image.Copy, Src: filepath.Join(src, "app"), Dst: "/app"},
				image.Item{Kind: image.Dir, Dst: "/data", Mode: &mode, Owner: owner},
				image.Item{Kind: image.Symlink, Dst: "/bin/app", Target: "/app"},
				image.Item{Kind: image.File, Dst: "/tmp/scratch", Content: []byte("x")},
			),
			// A later layer deleting a file must hide it.
			layer(image.Item{Kind: image.File, Dst: "/tmp/.wh.scratch"}),
		},
	}, image.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func parseChecks(t *testing.T, doc string) []Check {
	t.Helper()
	var checks []Check
	if err := yaml.Unmarshal([]byte(doc), &checks); err != nil {
		t.Fatal(err)
	}
	return checks
}

func TestRunPasses(t *testing.T) {
	checks := parseChecks(t, `
- file: /app
  type: file
  mode: 0755
  owner: 0:0
  contains: 'version \d+\.\d+'
- {file: /data, type: dir, mode: "0750", owner: "1000"}
- {file: /data/, type: dir}
- {file: /bin/app, target: /app}
- {absent: /tmp/scratch}
- {absent: /bin/sh}
- name: runtime config
  config:
    user: "1000"
    workdir: /data
    entrypoint: [/app]
    cmd: []
    env: {PORT: "8080"}
    labels: {team: core}
    expose: [8080]
`)
	failures, err := Run(testImage(t), checks)
	if err != nil || len(failures) != 0 {
		t.Errorf("Run = %q, %v", failures, err)
	}
}

func TestRunFailures(t *testing.T) {
	checks := parseChecks(t, `
- {file: /missing}
- {file: /app, type: dir}
- {file: /app, mode: "0644", owner: "5"}
- {file: /app, contains: '^nope'}
- {file: /data, contains: x}
- {file: /bin/app, target: /elsewhere}
- {absent: /app}
- name: cfg
  config:
    user: root
    workdir: /
    entrypoint: [/other]
    cmd: [x]
    stop-signal: SIGINT
    env: {PORT: "80", MISSING: "1"}
    labels: {team: edge, other: x}
    expose: [9090/udp]
`)
	failures, err := Run(testImage(t), checks)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(failures, "\n")
	for _, want := range []string{
		"file /missing: does not exist",
		"file /app: is a file, want dir",
		"file /app: mode 0755, want 0644",
		"file /app: owner 0:0, want 5:5",
		"file /app: content does not match",
		"file /data: is a dir, so it has no content",
		`file /bin/app: points to "/app", want "/elsewhere"`,
		"absent /app: exists (file)",
		`cfg: user "1000", want "root"`,
		`cfg: workdir "/data", want "/"`,
		"cfg: entrypoint",
		"cfg: cmd",
		"cfg: stop-signal",
		`cfg: env PORT="8080", want "80"`,
		"cfg: env MISSING is not set",
		`cfg: label team="core", want "edge"`,
		"cfg: label other is not set",
		"cfg: port 9090/udp is not exposed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing failure %q in:\n%s", want, got)
		}
	}
	if len(failures) != 18 {
		t.Errorf("%d failures, want 18:\n%s", len(failures), got)
	}
}

func TestCheckValidation(t *testing.T) {
	for name, doc := range map[string]string{
		"none":             "- {name: x}",
		"two":              "- {file: /a, absent: /b}",
		"relative":         "- {file: a}",
		"absent with mode": "- {absent: /a, mode: '0755'}",
		"config with type": "- {config: {}, type: file}",
		"bad type":         "- {file: /a, type: socket}",
		"target on dir":    "- {file: /a, type: dir, target: /b}",
		"contains on dir":  "- {file: /a, type: dir, contains: x}",
		"bad mode":         "- {file: /a, mode: rw}",
		"bad owner":        "- {file: /a, owner: me}",
		"bad regexp":       "- {file: /a, contains: '('}",
		"bad port":         "- {config: {expose: [http]}}",
	} {
		var checks []Check
		if err := yaml.Unmarshal([]byte(doc), &checks); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}
