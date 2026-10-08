package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Lock pins base image references to digests, so a build keeps using the
// same base until the lock is refreshed on purpose.
type Lock struct {
	Version int `json:"version"`
	// Bases maps each base reference, as written in the build, to the
	// digest of its manifest or image index.
	Bases map[string]string `json:"bases"`
}

const lockVersion = 1

// ReadLock reads a lock file. A missing file returns an error wrapping
// fs.ErrNotExist.
func ReadLock(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if l.Version != lockVersion {
		return nil, fmt.Errorf("lock %s: version %d, want %d", path, l.Version, lockVersion)
	}
	for ref, d := range l.Bases {
		if _, err := name.NewDigest("example.com/x@" + d); err != nil {
			return nil, fmt.Errorf("lock %s: %s: bad digest %q", path, ref, d)
		}
	}
	return &l, nil
}

// Write writes the lock as indented JSON with sorted keys.
func (l *Lock) Write(path string) error {
	l.Version = lockVersion
	if l.Bases == nil {
		l.Bases = map[string]string{}
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ErrNotLocked is returned by Pin for a base missing from the lock.
var ErrNotLocked = errors.New("not in the lock file")

// Pin returns base as a digest reference taken from the lock. Scratch and
// references that already name a digest are returned unchanged. A nil
// lock, or a base missing from it, returns ErrNotLocked.
func (l *Lock) Pin(base string, opts Options) (string, error) {
	if base == "" || base == Scratch {
		return base, nil
	}
	ref, err := name.ParseReference(base, opts.nameOptions()...)
	if err != nil {
		return "", fmt.Errorf("base %q: %w", base, err)
	}
	if _, ok := ref.(name.Digest); ok {
		return base, nil
	}
	if l == nil || l.Bases[base] == "" {
		return "", fmt.Errorf("base %s: %w", base, ErrNotLocked)
	}
	return ref.Context().Digest(l.Bases[base]).String(), nil
}

// LockBases resolves each base to the digest a registry serves for it now.
// Scratch and digest references are skipped.
func LockBases(ctx context.Context, bases []string, opts Options) (*Lock, error) {
	l := &Lock{Version: lockVersion, Bases: map[string]string{}}
	sorted := append([]string(nil), bases...)
	sort.Strings(sorted)
	for _, base := range sorted {
		if base == "" || base == Scratch || l.Bases[base] != "" {
			continue
		}
		ref, err := name.ParseReference(base, opts.nameOptions()...)
		if err != nil {
			return nil, fmt.Errorf("base %q: %w", base, err)
		}
		if _, ok := ref.(name.Digest); ok {
			continue
		}
		desc, err := remote.Head(ref, opts.remoteOptions(ctx)...)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", base, err)
		}
		l.Bases[base] = desc.Digest.String()
	}
	return l, nil
}
