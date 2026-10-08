package image

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Scratch names the empty base image.
const Scratch = "scratch"

// Spec describes one image build.
type Spec struct {
	// Base is a registry reference, or Scratch for an empty image.
	Base string
	// BaseName is the human-readable base reference recorded in the
	// org.opencontainers.image.base.name annotation, when Base has been
	// pinned to a digest. It defaults to Base.
	BaseName string
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
	// ExposedPorts are PORT[/PROTO] (tcp when omitted) and Volumes are
	// absolute paths; both add to the base image's values.
	ExposedPorts []string
	Volumes      []string
	// StopSignal, such as SIGTERM, replaces the base value when set.
	StopSignal string
	// Annotations are set on the image manifest, and on the index for
	// multi-platform builds.
	Annotations map[string]string

	// Created stamps the image config, history, and file times.
	Created time.Time

	// Compression and CompressionLevel apply to the new layers; see
	// LayerOptions.
	Compression      string
	CompressionLevel int

	// Runner executes run layers. It is needed only when a layer has Run.
	Runner Runner
}

// Runner runs the run layers of one platform's build in a container and
// returns, for each step, an uncompressed layer tar file holding the
// changes it made. The files must stay in place until the image has been
// written.
type Runner interface {
	Run(ctx context.Context, req RunRequest) ([]string, error)
}

// RunRequest describes the run layers of one platform.
type RunRequest struct {
	// Image is the base image, pinned to its platform manifest digest, as
	// a reference a container engine can pull.
	Image    string
	Platform v1.Platform
	// Names are the layer names, one per step.
	Names []string
	Steps []RunStep
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
	base, err := baseImage(ctx, spec, opts)
	if err != nil {
		return nil, err
	}
	img := base

	mt, err := img.MediaType()
	if err != nil {
		return nil, err
	}
	lo := LayerOptions{
		Created:          spec.Created,
		MediaType:        types.OCILayer,
		Compression:      spec.Compression,
		CompressionLevel: spec.CompressionLevel,
	}
	if mt == types.DockerManifestSchema2 {
		lo.MediaType = types.DockerLayer
	}
	nruns, err := countRuns(spec.Layers)
	if err != nil {
		return nil, err
	}
	if nruns > 0 {
		if img, err = appendRuns(ctx, img, spec, spec.Layers[:nruns], lo, opts); err != nil {
			return nil, err
		}
	}
	for i, ls := range spec.Layers[nruns:] {
		i += nruns
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
		lmt, err := layer.MediaType()
		if err != nil {
			return nil, err
		}
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:     layer,
			MediaType: lmt,
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
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		return nil, err
	}
	anns, err := baseAnnotations(spec, base)
	if err != nil {
		return nil, err
	}
	for k, v := range spec.Annotations {
		anns[k] = v
	}
	if len(anns) > 0 {
		img = mutate.Annotations(img, anns).(v1.Image)
	}
	return img, nil
}

// layerName is the name a layer goes by in messages.
func layerName(ls LayerSpec, i int) string {
	if ls.Name != "" {
		return ls.Name
	}
	return fmt.Sprintf("layer %d", i+1)
}

// countRuns returns how many run layers lead the list, and fails if a run
// layer follows a file layer.
func countRuns(layers []LayerSpec) (int, error) {
	n := 0
	for i, ls := range layers {
		if ls.Run == nil {
			continue
		}
		if len(ls.Items) > 0 {
			return 0, fmt.Errorf("layer %s has both a script and files", layerName(ls, i))
		}
		if strings.TrimSpace(ls.Run.Script) == "" {
			return 0, fmt.Errorf("layer %s: empty run script", layerName(ls, i))
		}
		if i != n {
			return 0, fmt.Errorf("run layer %s comes after a file layer; run layers run on the base image, so list them first", layerName(ls, i))
		}
		n++
	}
	return n, nil
}

// appendRuns runs the run layers through spec.Runner and appends the
// resulting layers to base.
func appendRuns(ctx context.Context, base v1.Image, spec Spec, runs []LayerSpec, lo LayerOptions, opts Options) (v1.Image, error) {
	if spec.Runner == nil {
		return nil, fmt.Errorf("run layers need a container engine")
	}
	if spec.Base == "" || spec.Base == Scratch {
		return nil, fmt.Errorf("run layer %s needs a base image with /bin/sh, not scratch", layerName(runs[0], 0))
	}
	ref, err := name.ParseReference(spec.Base, opts.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("base %q: %w", spec.Base, err)
	}
	d, err := base.Digest()
	if err != nil {
		return nil, err
	}
	req := RunRequest{Image: engineRef(ref.Context(), d), Platform: spec.Platform}
	for i, ls := range runs {
		req.Names = append(req.Names, layerName(ls, i))
		req.Steps = append(req.Steps, *ls.Run)
	}
	files, err := spec.Runner.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(files) != len(runs) {
		return nil, fmt.Errorf("runner returned %d layers for %d run steps", len(files), len(runs))
	}
	topts, err := lo.tarballOptions()
	if err != nil {
		return nil, err
	}
	img := base
	for i, f := range files {
		layer, err := tarball.LayerFromFile(f, topts...)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", req.Names[i], err)
		}
		lmt, err := layer.MediaType()
		if err != nil {
			return nil, err
		}
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:     layer,
			MediaType: lmt,
			History: v1.History{
				Created:   v1.Time{Time: spec.Created},
				CreatedBy: "construct: layer " + req.Names[i] + ": run: " + runs[i].Run.Script,
			},
		})
		if err != nil {
			return nil, err
		}
	}
	return img, nil
}

// engineRef names the image with digest d in repo the way container
// engines expect: Docker Hub images as docker.io/... rather than
// index.docker.io/....
func engineRef(repo name.Repository, d v1.Hash) string {
	reg := repo.RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	return reg + "/" + repo.RepositoryStr() + "@" + d.String()
}

// baseAnnotations records which base image was used, as the OCI image
// spec suggests, so scanners can tell when a rebuild is due.
func baseAnnotations(spec Spec, base v1.Image) (map[string]string, error) {
	anns := map[string]string{}
	if spec.Base == "" || spec.Base == Scratch {
		return anns, nil
	}
	d, err := base.Digest()
	if err != nil {
		return nil, err
	}
	anns["org.opencontainers.image.base.name"] = cmp.Or(spec.BaseName, spec.Base)
	anns["org.opencontainers.image.base.digest"] = d.String()
	return anns, nil
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
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, indexType), adds...)
	if len(spec.Annotations) > 0 {
		idx = mutate.Annotations(idx, spec.Annotations).(v1.ImageIndex)
	}
	return idx, nil
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
	for _, p := range spec.ExposedPorts {
		port, err := ParsePort(p)
		if err != nil {
			return err
		}
		if c.ExposedPorts == nil {
			c.ExposedPorts = map[string]struct{}{}
		}
		c.ExposedPorts[port] = struct{}{}
	}
	for _, v := range spec.Volumes {
		if !path.IsAbs(v) {
			return fmt.Errorf("volume %q: want an absolute path", v)
		}
		if c.Volumes == nil {
			c.Volumes = map[string]struct{}{}
		}
		c.Volumes[path.Clean(v)] = struct{}{}
	}
	if spec.StopSignal != "" {
		c.StopSignal = spec.StopSignal
	}
	return nil
}

// ParsePort normalises PORT[/PROTO] to the PORT/PROTO form used in image
// configs. PROTO is tcp, udp, or sctp and defaults to tcp.
func ParsePort(s string) (string, error) {
	port, proto, hasProto := strings.Cut(s, "/")
	if !hasProto {
		proto = "tcp"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("port %q: want PORT[/tcp|udp|sctp] with PORT 1-65535", s)
	}
	switch proto {
	case "tcp", "udp", "sctp":
	default:
		return "", fmt.Errorf("port %q: want PORT[/tcp|udp|sctp] with PORT 1-65535", s)
	}
	return strconv.Itoa(n) + "/" + proto, nil
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
