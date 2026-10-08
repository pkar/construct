package image

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// refNameAnnotation is the OCI annotation that names an image in a layout.
const refNameAnnotation = "org.opencontainers.image.ref.name"

// Artifact is a build result: a v1.Image for one platform or a
// v1.ImageIndex for several.
type Artifact interface {
	Digest() (v1.Hash, error)
	MediaType() (types.MediaType, error)
}

// Push uploads a to the registry reference ref and returns the digest
// reference that was written.
func Push(ctx context.Context, a Artifact, ref string, opts Options) (name.Digest, error) {
	ds, err := PushAll(ctx, a, []string{ref}, opts)
	if err != nil {
		return name.Digest{}, err
	}
	return ds[0], nil
}

// PushAll uploads a under every reference in refs and returns one digest
// reference per repository, in the order the repositories first appear.
// Blobs and the manifest are written once per repository; further tags in
// the same repository only point at the manifest that is already there.
func PushAll(ctx context.Context, a Artifact, refs []string, opts Options) ([]name.Digest, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("push: no references")
	}
	d, err := a.Digest()
	if err != nil {
		return nil, err
	}
	parsed, err := ParseRefs(refs, opts)
	if err != nil {
		return nil, err
	}
	ro := opts.remoteOptions(ctx)
	var out []name.Digest
	written := map[string]bool{}
	for _, r := range parsed {
		repo := r.Context().String()
		if dr, ok := r.(name.Digest); ok && dr.DigestStr() != d.String() {
			return out, fmt.Errorf("push %s: image digest is %s", r, d)
		}
		switch {
		case !written[repo]:
			err = write(r, a, ro)
			written[repo] = true
			out = append(out, r.Context().Digest(d.String()))
		case isTag(r):
			err = remote.Tag(r.(name.Tag), a.(remote.Taggable), ro...)
		}
		if err != nil {
			return out, fmt.Errorf("push %s: %w", r, err)
		}
	}
	return out, nil
}

func isTag(r name.Reference) bool {
	_, ok := r.(name.Tag)
	return ok
}

func write(r name.Reference, a Artifact, ro []remote.Option) error {
	switch a := a.(type) {
	case v1.ImageIndex:
		return remote.WriteIndex(r, a, ro...)
	case v1.Image:
		return remote.Write(r, a, ro...)
	default:
		return fmt.Errorf("unsupported artifact %T", a)
	}
}

// ParseRefs parses image references, rejecting duplicates.
func ParseRefs(refs []string, opts Options) ([]name.Reference, error) {
	seen := map[string]bool{}
	out := make([]name.Reference, 0, len(refs))
	for _, s := range refs {
		r, err := name.ParseReference(s, opts.nameOptions()...)
		if err != nil {
			return nil, fmt.Errorf("reference %q: %w", s, err)
		}
		if seen[r.Name()] {
			return nil, fmt.Errorf("reference %s given twice", r.Name())
		}
		seen[r.Name()] = true
		out = append(out, r)
	}
	return out, nil
}

// WriteLayout writes a as the only entry in the OCI layout at dir,
// replacing any existing index. refName, when set, is recorded as the
// ref.name annotation.
func WriteLayout(dir string, a Artifact, refName string) error {
	p, err := layout.Write(dir, empty.Index)
	if err != nil {
		return fmt.Errorf("oci layout %s: %w", dir, err)
	}
	var lo []layout.Option
	if refName != "" {
		lo = append(lo, layout.WithAnnotations(map[string]string{refNameAnnotation: refName}))
	}
	switch a := a.(type) {
	case v1.ImageIndex:
		err = p.AppendIndex(a, lo...)
	case v1.Image:
		err = p.AppendImage(a, lo...)
	default:
		err = fmt.Errorf("unsupported artifact %T", a)
	}
	if err != nil {
		return fmt.Errorf("oci layout %s: %w", dir, err)
	}
	return nil
}

// ReadLayout returns the single image or image index stored in the OCI
// layout at dir.
func ReadLayout(dir string) (Artifact, error) {
	p, err := layout.FromPath(dir)
	if err != nil {
		return nil, fmt.Errorf("oci layout %s: %w", dir, err)
	}
	idx, err := p.ImageIndex()
	if err != nil {
		return nil, err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	if len(im.Manifests) != 1 {
		return nil, fmt.Errorf("oci layout %s: want exactly one image, found %d", dir, len(im.Manifests))
	}
	desc := im.Manifests[0]
	switch {
	case desc.MediaType.IsIndex():
		return idx.ImageIndex(desc.Digest)
	case desc.MediaType.IsImage():
		return idx.Image(desc.Digest)
	default:
		return nil, fmt.Errorf("oci layout %s: unsupported media type %s", dir, desc.MediaType)
	}
}

// WriteTarball writes a single-platform image to file in the format
// accepted by `docker load` and `podman load`, tagged with every tag.
func WriteTarball(file string, a Artifact, tags []string, opts Options) error {
	f, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("tarball %s: %w", file, err)
	}
	if err := writeTarball(f, a, tags, opts); err != nil {
		f.Close()
		os.Remove(file)
		return fmt.Errorf("tarball %s: %w", file, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("tarball %s: %w", file, err)
	}
	return nil
}

func writeTarball(w io.Writer, a Artifact, tags []string, opts Options) error {
	img, ok := a.(v1.Image)
	if !ok {
		return fmt.Errorf("a tarball holds one platform; build with a single -platform")
	}
	if len(tags) == 0 {
		return fmt.Errorf("a tarball needs a tag")
	}
	refs := map[name.Reference]v1.Image{}
	for _, tag := range tags {
		t, err := name.NewTag(tag, opts.nameOptions()...)
		if err != nil {
			return fmt.Errorf("tag %q: %w", tag, err)
		}
		refs[t] = img
	}
	return tarball.MultiRefWrite(refs, w)
}
