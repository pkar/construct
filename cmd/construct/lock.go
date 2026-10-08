package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pkar/construct/internal/config"
	"github.com/pkar/construct/internal/image"
)

const lockFileName = "construct.lock"

const lockUsage = `Usage:
  construct lock -f construct.yaml [flags] [IMAGE...]
  construct lock -lock FILE -base REF [-base REF...]

Resolve base images to the digests their registries serve now and write
them to a lock file (default construct.lock next to the build file).
'construct build' then uses those digests instead of the moving tags, and
'construct build -locked' fails if a base is missing from the lock. Run
lock again to move to newer bases.

Flags:
`

func runLock(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fls := flag.NewFlagSet("construct lock", flag.ContinueOnError)
	fls.SetOutput(stderr)
	var (
		file     = fls.String("f", "", "read base images from build `file`")
		lockPath = fls.String("lock", "", "lock `file` to write (default construct.lock next to -f)")
		insecure = fls.Bool("insecure", false, "allow plain HTTP and unverified TLS registries")
		bases    repeated
	)
	fls.Var(&bases, "base", "base image `reference` to lock (repeatable)")
	fls.Usage = func() {
		fmt.Fprint(fls.Output(), lockUsage)
		fls.PrintDefaults()
	}
	if err := parse(fls, args); err != nil {
		return err
	}
	if *file == "" && len(bases) == 0 {
		return usageErr(fls, "nothing to lock: set -f or -base")
	}
	if *file == "" && fls.NArg() > 0 {
		return usageErr(fls, "unexpected arguments: %s (image names need -f)", strings.Join(fls.Args(), " "))
	}
	if *file != "" {
		f, err := config.Load(*file)
		if err != nil {
			return err
		}
		sel, err := f.Select(fls.Args())
		if err != nil {
			return err
		}
		for _, im := range sel {
			bases = append(bases, deref(im.Base))
			if im.Insecure != nil && *im.Insecure {
				*insecure = true
			}
		}
		if *lockPath == "" {
			*lockPath = filepath.Join(f.Dir, lockFileName)
		}
	}
	if *lockPath == "" {
		return usageErr(fls, "-base needs -lock FILE")
	}

	fresh, err := image.LockBases(ctx, bases, image.Options{Insecure: *insecure})
	if err != nil {
		return err
	}
	// Keep entries for images that were not selected this time.
	if old, err := image.ReadLock(*lockPath); err == nil && fls.NArg() > 0 {
		for ref, d := range old.Bases {
			if _, ok := fresh.Bases[ref]; !ok {
				fresh.Bases[ref] = d
			}
		}
	}
	if err := fresh.Write(*lockPath); err != nil {
		return err
	}
	for _, ref := range sortedKeys(fresh.Bases) {
		fmt.Fprintf(stdout, "%s %s\n", ref, fresh.Bases[ref])
	}
	fmt.Fprintf(stderr, "wrote %s\n", *lockPath)
	return nil
}

// lockState is the lock file used by one build.
type lockState struct {
	path    string
	locked  bool // every base must already be in the file
	lock    *image.Lock
	changed bool
}

// openLock picks the lock file for a build: lockPath when set, otherwise
// construct.lock beside the build file if one exists. It returns nil when
// the build uses no lock.
func openLock(lockPath, buildFileDir string, locked bool) (*lockState, error) {
	if lockPath == "" && buildFileDir != "" {
		cand := filepath.Join(buildFileDir, lockFileName)
		if _, err := os.Stat(cand); err == nil {
			lockPath = cand
		}
	}
	if lockPath == "" {
		if locked {
			return nil, usagef("-locked needs -lock FILE, or a %s next to the build file", lockFileName)
		}
		return nil, nil
	}
	l, err := image.ReadLock(lockPath)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !locked:
		l = &image.Lock{}
	case err != nil:
		return nil, err
	}
	return &lockState{path: lockPath, locked: locked, lock: l}, nil
}

// pin replaces p's base with its locked digest. Without -locked, a base
// missing from the lock is resolved now and added to it.
func (s *lockState) pin(ctx context.Context, p *plan) error {
	if s == nil {
		return nil
	}
	base := p.spec.Base
	pinned, err := s.lock.Pin(base, p.opts)
	if errors.Is(err, image.ErrNotLocked) && !s.locked {
		var add *image.Lock
		if add, err = image.LockBases(ctx, []string{base}, p.opts); err != nil {
			return err
		}
		if s.lock.Bases == nil {
			s.lock.Bases = map[string]string{}
		}
		s.lock.Bases[base] = add.Bases[base]
		s.changed = true
		pinned, err = s.lock.Pin(base, p.opts)
	}
	if err != nil {
		if errors.Is(err, image.ErrNotLocked) {
			return fmt.Errorf("%w (%s); run construct lock", err, s.path)
		}
		return err
	}
	if pinned != base {
		p.spec.Base, p.spec.BaseName = pinned, base
	}
	return nil
}

func (s *lockState) save(stderr io.Writer) error {
	if s == nil || !s.changed {
		return nil
	}
	if err := s.lock.Write(s.path); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "updated %s\n", s.path)
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
