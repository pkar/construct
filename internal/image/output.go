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
)

// refNameAnnotation is the OCI annotation that names an image in a layout.
const refNameAnnotation = "org.opencontainers.image.ref.name"

// Push uploads img to the registry reference ref and returns the digest
// reference that was written.
func Push(ctx context.Context, img v1.Image, ref string, opts Options) (name.Digest, error) {
	r, err := name.ParseReference(ref, opts.nameOptions()...)
	if err != nil {
		return name.Digest{}, fmt.Errorf("reference %q: %w", ref, err)
	}
	if err := remote.Write(r, img, opts.remoteOptions(ctx)...); err != nil {
		return name.Digest{}, fmt.Errorf("push %s: %w", r, err)
	}
	d, err := img.Digest()
	if err != nil {
		return name.Digest{}, err
	}
	return r.Context().Digest(d.String()), nil
}

// WriteLayout writes img as the only image in the OCI layout at dir,
// replacing any existing index. refName, when set, is recorded as the
// image's ref.name annotation.
func WriteLayout(dir string, img v1.Image, refName string) error {
	p, err := layout.Write(dir, empty.Index)
	if err != nil {
		return fmt.Errorf("oci layout %s: %w", dir, err)
	}
	var lo []layout.Option
	if refName != "" {
		lo = append(lo, layout.WithAnnotations(map[string]string{refNameAnnotation: refName}))
	}
	if err := p.AppendImage(img, lo...); err != nil {
		return fmt.Errorf("oci layout %s: %w", dir, err)
	}
	return nil
}

// ReadLayout returns the single image stored in the OCI layout at dir.
func ReadLayout(dir string) (v1.Image, error) {
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
	if !desc.MediaType.IsImage() {
		return nil, fmt.Errorf("oci layout %s: %s is not an image manifest", dir, desc.MediaType)
	}
	return idx.Image(desc.Digest)
}

// WriteTarball writes img to file in the format accepted by `docker load`
// and `podman load`, tagged as tag.
func WriteTarball(file string, img v1.Image, tag string, opts Options) error {
	t, err := name.NewTag(tag, opts.nameOptions()...)
	if err != nil {
		return fmt.Errorf("tag %q: %w", tag, err)
	}
	if err := tarball.WriteToFile(file, t, img); err != nil {
		return fmt.Errorf("tarball %s: %w", file, err)
	}
	return nil
}
