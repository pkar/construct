package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Engines are the container engines Load knows, in the order tried when
// none is named.
var Engines = []string{"docker", "podman"}

// FindEngine returns the path of the named engine, or of the first engine
// in Engines on PATH when name is empty.
func FindEngine(name string) (string, error) {
	if name != "" {
		p, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("container engine %s: %w", name, err)
		}
		return p, nil
	}
	for _, e := range Engines {
		if p, err := exec.LookPath(e); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no container engine found; install %s or set -engine", strings.Join(Engines, " or "))
}

// SelectPlatform returns a itself if it is an image, or the image in the
// index that matches p.
func SelectPlatform(a Artifact, p v1.Platform) (v1.Image, error) {
	switch a := a.(type) {
	case v1.Image:
		return a, nil
	case v1.ImageIndex:
		im, err := a.IndexManifest()
		if err != nil {
			return nil, err
		}
		var have []string
		for _, d := range im.Manifests {
			if d.Platform == nil {
				continue
			}
			if d.Platform.Satisfies(p) {
				return a.Image(d.Digest)
			}
			have = append(have, d.Platform.String())
		}
		return nil, fmt.Errorf("no %s image in the index (it has %s)", p, strings.Join(have, ", "))
	default:
		return nil, fmt.Errorf("unsupported artifact %T", a)
	}
}

// errEngineExited fails the tarball writer when the engine quits before
// reading all of its input.
var errEngineExited = errors.New("engine exited")

// Load streams img, tagged with every tag, to `ENGINE load` and copies
// the engine's output to out. engine is a path or a name on PATH.
func Load(ctx context.Context, engine string, img v1.Image, tags []string, opts Options, out io.Writer) error {
	if len(tags) == 0 {
		return fmt.Errorf("load: needs a tag")
	}
	// Check the tags before starting the engine.
	if _, err := repoTags(tags, opts); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, engine, "load")
	var stderr bytes.Buffer
	cmd.Stdin = pr
	cmd.Stdout = out
	cmd.Stderr = &stderr
	writeErr := make(chan error, 1)
	go func() {
		err := writeTarball(pw, img, tags, opts)
		pw.CloseWithError(err)
		writeErr <- err
	}()
	runErr := cmd.Run()
	// Unblock the writer if the engine stopped reading early.
	pr.CloseWithError(errEngineExited)
	werr := <-writeErr
	if werr != nil && runErr == nil {
		return fmt.Errorf("load: %w", werr)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		// A write that failed because the engine quit says nothing the
		// engine's own exit status and message don't.
		if werr != nil && !errors.Is(werr, io.ErrClosedPipe) && !errors.Is(werr, errEngineExited) {
			return fmt.Errorf("load: %w", werr)
		}
		if msg != "" {
			return fmt.Errorf("%s load: %w: %s", engine, runErr, msg)
		}
		return fmt.Errorf("%s load: %w", engine, runErr)
	}
	return nil
}
