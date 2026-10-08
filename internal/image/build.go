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
	// Adds become one new layer on top of the base. No layer is added when
	// Adds is empty.
	Adds []Add

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

	if len(spec.Adds) > 0 {
		mt, err := img.MediaType()
		if err != nil {
			return nil, err
		}
		layerType := types.OCILayer
		if mt == types.DockerManifestSchema2 {
			layerType = types.DockerLayer
		}
		layer, err := Layer(spec.Adds, spec.Created, layerType)
		if err != nil {
			return nil, fmt.Errorf("build layer: %w", err)
		}
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:     layer,
			MediaType: layerType,
			History: v1.History{
				Created:   v1.Time{Time: spec.Created},
				CreatedBy: "construct: add " + describeAdds(spec.Adds),
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
	return img, nil
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

func describeAdds(adds []Add) string {
	parts := make([]string, len(adds))
	for i, a := range adds {
		parts[i] = a.Src + ":" + a.Dst
	}
	return strings.Join(parts, " ")
}
