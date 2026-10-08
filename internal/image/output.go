package image

import (
	"context"
	"fmt"

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
	r, err := name.ParseReference(ref, opts.nameOptions()...)
	if err != nil {
		return name.Digest{}, fmt.Errorf("reference %q: %w", ref, err)
	}
	ro := opts.remoteOptions(ctx)
	switch a := a.(type) {
	case v1.ImageIndex:
		err = remote.WriteIndex(r, a, ro...)
	case v1.Image:
		err = remote.Write(r, a, ro...)
	default:
		return name.Digest{}, fmt.Errorf("push %s: unsupported artifact %T", r, a)
	}
	if err != nil {
		return name.Digest{}, fmt.Errorf("push %s: %w", r, err)
	}
	d, err := a.Digest()
	if err != nil {
		return name.Digest{}, err
	}
	return r.Context().Digest(d.String()), nil
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
// accepted by `docker load` and `podman load`, tagged as tag.
func WriteTarball(file string, a Artifact, tag string, opts Options) error {
	img, ok := a.(v1.Image)
	if !ok {
		return fmt.Errorf("tarball %s: holds one platform; build with a single -platform", file)
	}
	t, err := name.NewTag(tag, opts.nameOptions()...)
	if err != nil {
		return fmt.Errorf("tag %q: %w", tag, err)
	}
	if err := tarball.WriteToFile(file, t, img); err != nil {
		return fmt.Errorf("tarball %s: %w", file, err)
	}
	return nil
}
