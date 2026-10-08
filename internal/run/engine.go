// Package run executes build scripts in a container engine and turns the
// files they change into an image layer.
//
// construct starts a container from the base image, exports its
// filesystem, runs each script with exec, exports again, and diffs the two
// exports (see Diff). Nothing in it knows about package managers: whatever
// a script changes, by apt, apk, dnf, curl, or sed, ends up in the layer.
package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Engines are the container engines tried, in order, when none is named:
// Apple's container on macOS, then Docker, then Podman. They share the
// run, exec, export, and rm commands construct uses.
var Engines = []string{"container", "docker", "podman"}

// Engine is a container engine CLI.
type Engine struct {
	Name string // container, docker, or podman
	Path string
}

// FindEngines returns the named engine, or every engine in Engines that
// is on PATH.
func FindEngines(name string) ([]Engine, error) {
	if name != "" {
		p, err := exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf("container engine %s: %w", name, err)
		}
		return []Engine{{Name: name, Path: p}}, nil
	}
	var out []Engine
	for _, n := range Engines {
		if p, err := exec.LookPath(n); err == nil {
			out = append(out, Engine{Name: n, Path: p})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("run layers need a container engine; install Apple container, Docker, or Podman")
	}
	return out, nil
}

// command runs the engine and returns its trimmed stderr in the error.
func (e Engine) command(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, e.Path, args...)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := lastLines(stderr.String(), 5); msg != "" {
			return fmt.Errorf("%s %s: %w: %s", e.Name, args[0], err, msg)
		}
		return fmt.Errorf("%s %s: %w", e.Name, args[0], err)
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// container is a running container that waits for exec commands.
type container struct {
	engine Engine
	name   string
}

// idle keeps a container running until it is removed. Apple's container
// does not hold stdin open for a detached container, so a shell reading
// stdin would exit at once; a sleep loop works in every engine.
const idle = `command -v sleep >/dev/null || { echo "construct: the base image has no sleep command" >&2; exit 1; }
trap 'exit 0' TERM INT
while :; do sleep 3600 & wait $!; done`

// start runs a container from image for platform, idling in a sleep
// loop. A probe exec then proves the engine can run the platform's
// binaries.
func (e Engine) start(ctx context.Context, image, platform string) (*container, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	c := &container{engine: e, name: "construct-run-" + hex.EncodeToString(b)}
	err := e.command(ctx, io.Discard, "run", "-d", "--platform", platform,
		"--name", c.name, "--entrypoint", "/bin/sh", image, "-c", idle)
	if err == nil {
		err = e.command(ctx, io.Discard, "exec", c.name, "/bin/sh", "-c", "exit 0")
	}
	if err != nil {
		c.remove()
		return nil, err
	}
	return c, nil
}

// exec runs script as root with /bin/sh -c, sending its output to out.
func (c *container) exec(ctx context.Context, script string, env []string, out io.Writer) error {
	args := []string{"exec", "-u", "0"}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, c.name, "/bin/sh", "-c", script)
	cmd := exec.CommandContext(ctx, c.engine.Path, args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("script failed: %w", err)
		}
		return fmt.Errorf("%s exec: %w", c.engine.Name, err)
	}
	return nil
}

// export writes the container's filesystem to file as a tar.
func (c *container) export(ctx context.Context, file string) error {
	return c.engine.command(ctx, io.Discard, "export", "-o", file, c.name)
}

// remove stops and deletes the container. It ignores cancellation, so an
// interrupted build still cleans up.
func (c *container) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = c.engine.command(ctx, io.Discard, "rm", "-f", c.name)
}
