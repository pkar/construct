package image

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Scratch names the empty base image.
const Scratch = "scratch"

// Spec describes one image build.
type Spec struct {
	// Base is a registry reference, or Scratch for an empty image.
	Base string
	// Platform selects the base image from a multi-platform index and is
	// recorded in the config of scratch images.
	Platform v1.Platform
	// Layers are added on top of the base in order. {os}, {arch}, and
	// {variant} in a Copy item's Src are replaced with the platform's
	// values, so one spec can pick per-platform binaries.
	Layers []LayerSpec

	// Nil Entrypoint or Cmd inherit the base value; an empty non-nil slice
	// clears it.
	Entrypoint []string
	Cmd        []string
	// Env entries are KEY=VALUE and replace base entries with the same key.
	Env    []string
	Labels map[string]string
	// Empty WorkDir and User inherit the base value.
	WorkDir string
	User    string

	// Created stamps the image config, history, and file times.
	Created time.Time
}

// Options holds registry settings shared by pulls and pushes.
type Options struct {
	Keychain authn.Keychain
	// Insecure allows plain HTTP and unverified TLS registries.
	Insecure bool
}

func (o Options) nameOptions() []name.Option {
	if o.Insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

func (o Options) remoteOptions(ctx context.Context) []remote.Option {
	kc := o.Keychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(kc)}
}

// Build assembles the image described by spec.
func Build(ctx context.Context, spec Spec, opts Options) (v1.Image, error) {
	img, err := baseImage(ctx, spec, opts)
	if err != nil {
		return nil, err
	}

	mt, err := img.MediaType()
	if err != nil {
		return nil, err
	}
	lo := LayerOptions{Created: spec.Created, MediaType: types.OCILayer}
	if mt == types.DockerManifestSchema2 {
		lo.MediaType = types.DockerLayer
	}
	for i, ls := range spec.Layers {
		name := ls.Name
		if name == "" {
			name = fmt.Sprintf("layer %d", i+1)
		}
		if len(ls.Items) == 0 {
			return nil, fmt.Errorf("layer %s is empty", name)
		}
		items := expandItems(ls.Items, spec.Platform)
		layer, err := Layer(items, lo)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", name, err)
		}
		descs := make([]string, len(items))
		for i, it := range items {
			descs[i] = it.describe()
		}
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:     layer,
			MediaType: lo.MediaType,
			History: v1.History{
				Created:   v1.Time{Time: spec.Created},
				CreatedBy: "construct: layer " + name + ": " + strings.Join(descs, "; "),
			},
		})
		if err != nil {
			return nil, err
		}
	}

	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	if err := applyConfig(cf, spec); err != nil {
		return nil, err
	}
	return mutate.ConfigFile(img, cf)
}

func baseImage(ctx context.Context, spec Spec, opts Options) (v1.Image, error) {
	if spec.Base == "" || spec.Base == Scratch {
		img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
		img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
		cf, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		cf = cf.DeepCopy()
		cf.OS = spec.Platform.OS
		cf.Architecture = spec.Platform.Architecture
		cf.Variant = spec.Platform.Variant
		cf.OSVersion = spec.Platform.OSVersion
		return mutate.ConfigFile(img, cf)
	}

	ref, err := name.ParseReference(spec.Base, opts.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("base %q: %w", spec.Base, err)
	}
	ro := append(opts.remoteOptions(ctx), remote.WithPlatform(spec.Platform))
	img, err := remote.Image(ref, ro...)
	if err != nil {
		return nil, fmt.Errorf("pull base %s: %w", ref, err)
	}
	// A single-platform base is returned whatever platform was asked for,
	// so check it rather than silently mixing architectures.
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("pull base %s: %w", ref, err)
	}
	if !platformMatches(cf, spec.Platform) {
		return nil, fmt.Errorf("base %s is %s, not %s", ref, cf.Platform(), spec.Platform)
	}
	return img, nil
}

func platformMatches(cf *v1.ConfigFile, want v1.Platform) bool {
	if want.OS == "" || (cf.OS == "" && cf.Architecture == "") {
		return true
	}
	if cf.OS != want.OS || cf.Architecture != want.Architecture {
		return false
	}
	return cf.Variant == "" || want.Variant == "" || cf.Variant == want.Variant
}

// BuildIndex builds spec once per platform and returns a multi-platform
// image index. The index is an OCI index unless every image uses Docker
// manifests, in which case it is a Docker manifest list.
func BuildIndex(ctx context.Context, spec Spec, platforms []v1.Platform, opts Options) (v1.ImageIndex, error) {
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no platforms")
	}
	seen := map[string]bool{}
	adds := make([]mutate.IndexAddendum, 0, len(platforms))
	allDocker := true
	for _, p := range platforms {
		if seen[p.String()] {
			return nil, fmt.Errorf("platform %s listed twice", p)
		}
		seen[p.String()] = true

		s := spec
		s.Platform = p
		img, err := Build(ctx, s, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		mt, err := img.MediaType()
		if err != nil {
			return nil, err
		}
		if mt != types.DockerManifestSchema2 {
			allDocker = false
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		plat := cf.Platform()
		if plat == nil {
			plat = &p
		}
		adds = append(adds, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: plat},
		})
	}
	indexType := types.OCIImageIndex
	if allDocker {
		indexType = types.DockerManifestList
	}
	return mutate.AppendManifests(mutate.IndexMediaType(empty.Index, indexType), adds...), nil
}

// ParsePlatforms parses a comma-separated list of os/arch[/variant].
func ParsePlatforms(s string) ([]v1.Platform, error) {
	var out []v1.Platform
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := v1.ParsePlatform(f)
		if err != nil {
			return nil, fmt.Errorf("platform %q: %w", f, err)
		}
		if p.OS == "" || p.Architecture == "" {
			return nil, fmt.Errorf("platform %q: want os/arch[/variant]", f)
		}
		out = append(out, *p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no platform given")
	}
	return out, nil
}

func expandItems(items []Item, p v1.Platform) []Item {
	r := strings.NewReplacer("{os}", p.OS, "{arch}", p.Architecture, "{variant}", p.Variant)
	out := make([]Item, len(items))
	for i, it := range items {
		if it.Kind == Copy {
			it.Src = r.Replace(it.Src)
		}
		out[i] = it
	}
	return out
}

func applyConfig(cf *v1.ConfigFile, spec Spec) error {
	cf.Created = v1.Time{Time: spec.Created}
	c := &cf.Config
	if spec.Entrypoint != nil {
		c.Entrypoint = spec.Entrypoint
		// As in a Dockerfile, a new entrypoint drops the inherited command.
		if spec.Cmd == nil {
			c.Cmd = nil
		}
	}
	if spec.Cmd != nil {
		c.Cmd = spec.Cmd
	}
	if spec.WorkDir != "" {
		c.WorkingDir = spec.WorkDir
	}
	if spec.User != "" {
		c.User = spec.User
	}
	for _, kv := range spec.Env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return fmt.Errorf("env %q: want KEY=VALUE", kv)
		}
		c.Env = setEnv(c.Env, key, kv)
	}
	if len(spec.Labels) > 0 {
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		for k, v := range spec.Labels {
			c.Labels[k] = v
		}
	}
	return nil
}

func setEnv(env []string, key, kv string) []string {
	for i, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == key {
			env[i] = kv
			return env
		}
	}
	return append(env, kv)
}
