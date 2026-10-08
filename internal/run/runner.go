package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkar/construct/internal/image"
)

// Runner runs build scripts and caches the layers they produce. It
// implements image.Runner.
type Runner struct {
	// Engines are tried in order until one can run the platform.
	Engines []Engine
	// CacheDir holds finished layers, named by a hash of their inputs.
	CacheDir string
	// Refresh reruns scripts even when their layers are cached.
	Refresh bool
	// Created stamps every entry of the layers.
	Created time.Time
	// Log receives progress messages and script output.
	Log io.Writer
	// Prefix starts the IDs of this build's run layers; IDs are
	// PREFIX/LAYER/PLATFORM.
	Prefix string
	// Pins are the run layers recorded in the lock file, by ID.
	Pins map[string]image.RunPin
	// Locked requires every run layer to be pinned with matching inputs
	// and to produce the pinned layer.
	Locked bool

	mu   sync.Mutex
	used map[string]image.RunPin
}

// DefaultCacheDir is $CONSTRUCT_CACHE_DIR, or construct/ in the user cache
// directory.
func DefaultCacheDir() (string, error) {
	if d := os.Getenv("CONSTRUCT_CACHE_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache directory: %w; set CONSTRUCT_CACHE_DIR", err)
	}
	return filepath.Join(d, "construct"), nil
}

// Used returns the pins of every run layer this runner produced or took
// from the cache.
func (r *Runner) Used() map[string]image.RunPin {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]image.RunPin, len(r.used))
	for k, v := range r.used {
		out[k] = v
	}
	return out
}

// inputKeys returns one key per step. Each key covers the base image,
// the platform, the file timestamp, and every script up to and including
// its own, so a change to one step invalidates it and the steps after it.
func inputKeys(req image.RunRequest, created time.Time) []string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode([]string{"construct run v1", req.Image, req.Platform.String(), fmt.Sprint(created.Unix())})
	keys := make([]string, len(req.Steps))
	for i, s := range req.Steps {
		_ = enc.Encode(s)
		keys[i] = "sha256:" + hex.EncodeToString(h.Sum(nil))
	}
	return keys
}

func (r *Runner) cachePath(key string) string {
	return filepath.Join(r.CacheDir, "run", strings.TrimPrefix(key, "sha256:")+".tar")
}

func fileDigest(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// Run implements image.Runner.
func (r *Runner) Run(ctx context.Context, req image.RunRequest) ([]string, error) {
	plat := req.Platform.String()
	keys := inputKeys(req, r.Created)
	ids := make([]string, len(req.Steps))
	for i, n := range req.Names {
		ids[i] = strings.Join([]string{r.Prefix, n, plat}, "/")
	}
	if r.Locked {
		for i, id := range ids {
			pin, ok := r.Pins[id]
			switch {
			case !ok:
				return nil, fmt.Errorf("run layer %s (%s) is not in the lock file; build without -locked to add it", req.Names[i], plat)
			case pin.Inputs != keys[i]:
				return nil, fmt.Errorf("run layer %s (%s): the base image or scripts changed since the lock file was written; build without -locked to update it", req.Names[i], plat)
			}
		}
	}

	paths := make([]string, len(keys))
	cached := !r.Refresh
	for i, k := range keys {
		paths[i] = r.cachePath(k)
		if _, err := os.Stat(paths[i]); err != nil {
			cached = false
		}
	}
	if cached {
		fmt.Fprintf(r.Log, "construct: run layers for %s: cached\n", plat)
	} else if err := r.execute(ctx, req, paths); err != nil {
		return nil, err
	}

	for i, p := range paths {
		d, err := fileDigest(p)
		if err != nil {
			return nil, err
		}
		if pin, ok := r.Pins[ids[i]]; r.Locked && ok && pin.Layer != d {
			return nil, fmt.Errorf("run layer %s (%s) produced %s, but the lock file pins %s: the script's output is not reproducible; build without -locked to accept the new layer, or restore the cached one", req.Names[i], plat, d, pin.Layer)
		}
		r.mu.Lock()
		if r.used == nil {
			r.used = map[string]image.RunPin{}
		}
		r.used[ids[i]] = image.RunPin{Inputs: keys[i], Layer: d}
		r.mu.Unlock()
	}
	return paths, nil
}

// execute runs every step in one container and writes each step's layer
// to its path.
func (r *Runner) execute(ctx context.Context, req image.RunRequest, paths []string) error {
	plat := req.Platform.String()
	if err := os.MkdirAll(filepath.Join(r.CacheDir, "run"), 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Join(r.CacheDir, "run"), ".work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	c, err := r.startAny(ctx, req.Image, plat)
	if err != nil {
		return err
	}
	defer c.remove()

	prev := filepath.Join(work, "fs-0.tar")
	if err := c.export(ctx, prev); err != nil {
		return err
	}
	for i, step := range req.Steps {
		fmt.Fprintf(r.Log, "construct: run layer %s (%s) in %s\n", req.Names[i], plat, c.engine.Name)
		if err := c.exec(ctx, step.Script, step.Env, r.Log); err != nil {
			return fmt.Errorf("run layer %s (%s): %w", req.Names[i], plat, err)
		}
		next := filepath.Join(work, fmt.Sprintf("fs-%d.tar", i+1))
		if err := c.export(ctx, next); err != nil {
			return err
		}
		st, err := writeDiff(prev, next, paths[i], r.Created)
		if err != nil {
			return fmt.Errorf("run layer %s (%s): %w", req.Names[i], plat, err)
		}
		fmt.Fprintf(r.Log, "construct: run layer %s (%s): %d changed, %d deleted\n", req.Names[i], plat, st.Changed, st.Deleted)
		os.Remove(prev)
		prev = next
	}
	return nil
}

// startAny starts a container in the first engine that can run platform.
func (r *Runner) startAny(ctx context.Context, image, plat string) (*container, error) {
	if len(r.Engines) == 0 {
		return nil, fmt.Errorf("run layers need a container engine; install Apple container, Docker, or Podman")
	}
	var errs []string
	for _, e := range r.Engines {
		fmt.Fprintf(r.Log, "construct: starting %s in %s\n", image, e.Name)
		c, err := e.start(ctx, image, plat)
		if err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, err.Error())
	}
	return nil, fmt.Errorf("no container engine could run %s:\n  %s", plat, strings.Join(errs, "\n  "))
}

// writeDiff writes the diff layer to a temporary file and renames it into
// place, so the cache never holds a partial layer.
func writeDiff(before, after, dst string, mtime time.Time) (Stats, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".layer-")
	if err != nil {
		return Stats{}, err
	}
	defer os.Remove(tmp.Name())
	st, err := Diff(before, after, tmp, mtime)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return st, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil && !errors.Is(err, fs.ErrExist) {
		return st, err
	}
	return st, nil
}
