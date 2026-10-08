package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/pkar/construct/internal/check"
	"github.com/pkar/construct/internal/config"
	"github.com/pkar/construct/internal/image"
	"github.com/pkar/construct/internal/vcs"
)

const buildUsage = `Usage:
  construct build [flags]
  construct build -f construct.yaml [flags] [IMAGE...]

Build an image from a base and local files. Every image needs at least one
output: -push, -oci-layout, or -tarball.

With -f, images come from a build file and IMAGE names select some of them
(all by default). Flags then apply to every selected image: settings such
as -base or -tag replace the file's values, while -add, -env, -label,
-expose, and the like add to them.

Flags:
`

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("construct build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		file       = fs.String("f", "", "read images from build `file`, e.g. construct.yaml")
		base       = fs.String("base", "", "base image reference, or scratch (default scratch)")
		platform   = fs.String("platform", "", "target `platforms` as os/arch[/variant], comma-separated; several build an image index\n(default linux/"+runtime.GOARCH+")")
		push       = fs.Bool("push", false, "push the image to every -tag")
		layoutDir  = fs.String("oci-layout", "", "write the image to an OCI layout `directory`")
		tarFile    = fs.String("tarball", "", "write a docker/podman-loadable tarball to `file` (needs -tag)")
		load       = fs.Bool("load", false, "load the image into the local container engine (needs -tag); for several platforms,\nthe linux/"+runtime.GOARCH+" image is loaded")
		engine     = fs.String("engine", "", "container `engine` for -load: docker or podman (default: whichever is installed)")
		workdir    = fs.String("workdir", "", "working `directory`")
		user       = fs.String("user", "", "`user[:group]` to run as")
		insecure   = fs.Bool("insecure", false, "allow plain HTTP and unverified TLS registries")
		stopSignal = fs.String("stop-signal", "", "`SIGNAL` that stops the container, e.g. SIGINT")
		compress   = fs.String("compression", "", "layer `compression`: gzip (default), or zstd (smaller and faster; needs an OCI base\nand a recent runtime)")
		level      = fs.Int("compression-level", 0, "compression `level`: 1-9 for gzip, 1-22 for zstd; 0 for the default")
		useVCS     = fs.Bool("vcs", true, "annotate the image with the Git commit and source URL")
		skeleton   = fs.Bool("skeleton", false, "add a minimal root filesystem: /etc, /home, /root, /tmp, /var, passwd and group\nwith root, nobody, and nonroot (65532)")
		caCerts    = fs.String("ca-certs", "", "add CA certificates from PEM `file` at "+image.CACertsPath+"; system uses\nthe build machine's bundle")
		tzdata     = fs.String("tzdata", "", "add time zone data from zoneinfo `directory` at "+image.ZoneinfoDir+"; system uses\nthe build machine's")
		users      repeated
		runTests   = fs.Bool("test", true, "run the build file's tests before writing outputs")
		lockPath   = fs.String("lock", "", "pin base images to the digests in lock `file`, adding missing ones (default\nconstruct.lock next to -f, when it exists)")
		locked     = fs.Bool("locked", false, "fail if a base image is missing from the lock file; never update it")
		entrypoint optionalList
		cmd        optionalList
		layers     layerFlags
		tags       repeated
		env        repeated
		labels     repeated
		annots     repeated
		expose     repeated
		volumes    repeated
	)
	fs.Var(layerFlag{&layers}, "layer", "start a new layer `NAME`; later -add, -mkdir, and -symlink go into it")
	fs.Var(itemFlag{&layers, image.ParseAdd}, "add", "copy host `SRC:DST[:OPTIONS]` into the image (repeatable); {os}, {arch}, {variant} in SRC\nexpand per platform. OPTIONS: mode=OCTAL,dirmode=OCTAL,owner=UID[:GID]")
	fs.Var(itemFlag{&layers, image.ParseMkdir}, "mkdir", "create directory `DST[:OPTIONS]` (repeatable). OPTIONS: mode=OCTAL,owner=UID[:GID]")
	fs.Var(itemFlag{&layers, image.ParseSymlink}, "symlink", "create symlink `DST:TARGET` (repeatable)")
	fs.Var(&users, "add-user", "add user `NAME:UID[:GID[:HOME]]` to /etc/passwd and /etc/group, with a home directory\n(repeatable; scratch base only)")
	fs.Var(&tags, "tag", "image `reference`, e.g. registry.example.com/team/app:v1 (repeatable); {git.commit}, {git.short},\n{git.branch}, {git.tag}, and {env.NAME} expand here and in label and annotation values")
	fs.Var(&entrypoint, "entrypoint", `entrypoint as a JSON array or space-separated words`)
	fs.Var(&cmd, "cmd", `default arguments as a JSON array or space-separated words`)
	fs.Var(&env, "env", "set `KEY=VALUE` in the environment (repeatable)")
	fs.Var(&labels, "label", "set label `KEY=VALUE` (repeatable)")
	fs.Var(&annots, "annotation", "set manifest annotation `KEY=VALUE` (repeatable)")
	fs.Var(&expose, "expose", "expose `PORT[/PROTO]` (repeatable)")
	fs.Var(&volumes, "volume", "declare volume `PATH` (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), buildUsage)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}

	over := config.Image{Layers: layers.layers}
	rootfs := func() *config.Rootfs {
		if over.Rootfs == nil {
			over.Rootfs = &config.Rootfs{}
		}
		return over.Rootfs
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "skeleton":
			rootfs().Skeleton = skeleton
		case "ca-certs":
			rootfs().CACerts = caCerts
		case "tzdata":
			rootfs().Tzdata = tzdata
		case "add-user":
			rootfs().Users = users
		case "base":
			over.Base = base
		case "platform":
			over.Platforms = config.StringList{*platform}
		case "push":
			over.Push = push
		case "oci-layout":
			over.OCILayout = layoutDir
		case "tarball":
			over.Tarball = tarFile
		case "load":
			over.Load = load
		case "engine":
			over.Engine = engine
		case "workdir":
			over.WorkDir = workdir
		case "user":
			over.User = user
		case "insecure":
			over.Insecure = insecure
		case "stop-signal":
			over.StopSignal = stopSignal
		case "compression":
			over.Compression = compress
		case "compression-level":
			over.CompressionLevel = level
		case "vcs":
			over.VCS = useVCS
		case "tag":
			over.Tags = config.StringList(tags)
		case "entrypoint":
			over.Entrypoint = (*config.Command)(&entrypoint.list)
		case "cmd":
			over.Cmd = (*config.Command)(&cmd.list)
		case "env":
			over.Env = config.EnvList(env)
		case "expose":
			over.Expose = config.StringList(expose)
		case "volume":
			over.Volumes = config.StringList(volumes)
		case "label":
			if over.Labels, err = keyValues("label", labels); err != nil {
				err = usageErr(fs, "%v", err)
			}
		case "annotation":
			if over.Annotations, err = keyValues("annotation", annots); err != nil {
				err = usageErr(fs, "%v", err)
			}
		}
	})
	if err != nil {
		return err
	}

	var (
		images  []config.Image
		dir     = "."
		fileDir string
	)
	if *file == "" {
		if fs.NArg() > 0 {
			return usageErr(fs, "unexpected arguments: %s (image names need -f)", strings.Join(fs.Args(), " "))
		}
		images = []config.Image{over}
	} else {
		f, err := config.Load(*file)
		if err != nil {
			return err
		}
		sel, err := f.Select(fs.Args())
		if err != nil {
			return err
		}
		for _, im := range sel {
			images = append(images, config.Merge(im, over))
		}
		dir, fileDir = f.Dir, f.Dir
	}
	lock, err := openLock(*lockPath, fileDir, *locked)
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			return usageErr(fs, "%v", err)
		}
		return err
	}

	plans := make([]*plan, 0, len(images))
	outputs := map[string]string{}
	for _, im := range images {
		p, err := newPlan(im, dir, false, stderr)
		if p != nil {
			p.skipTests = !*runTests
		}
		var ue usageError
		switch {
		case errors.As(err, &ue) && *file == "":
			return usageErr(fs, "%v", err)
		case err != nil:
			return p.errorf("%w", err)
		}
		for _, out := range []string{p.layout, p.tarball} {
			if out == "" {
				continue
			}
			abs, _ := filepath.Abs(out)
			if other, ok := outputs[abs]; ok {
				return fmt.Errorf("images %s and %s both write %s", other, p.label(), out)
			}
			outputs[abs] = p.label()
		}
		if err := lock.pin(ctx, p); err != nil {
			return p.errorf("%w", err)
		}
		plans = append(plans, p)
	}
	if err := lock.save(stderr); err != nil {
		return err
	}
	for _, p := range plans {
		if err := p.run(ctx, stdout, stderr); err != nil {
			return p.errorf("%w", err)
		}
	}
	return nil
}

// usageError marks a plan error caused by a missing or conflicting flag.
type usageError struct{ error }

func usagef(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }

// plan is one fully resolved image build.
type plan struct {
	name      string
	spec      image.Spec
	platforms []v1.Platform
	tags      []string
	push      bool
	layout    string
	tarball   string
	load      bool
	engine    string
	checks    []check.Check
	skipTests bool
	opts      image.Options
}

func (p *plan) label() string {
	if p == nil || p.name == "" {
		return "image"
	}
	return p.name
}

func (p *plan) errorf(format string, a ...any) error {
	if p == nil || p.name == "" {
		return fmt.Errorf(format, a...)
	}
	return fmt.Errorf("%s: "+format, append([]any{p.name}, a...)...)
}

// newPlan checks im and resolves defaults, stamps, and Git annotations.
// dir is the directory whose Git state stamps the image. The returned plan
// is non-nil even on error, so errors can name the image.
//
// With testOnly, outputs are ignored: the plan builds the image in memory
// so construct test can check it.
func newPlan(im config.Image, dir string, testOnly bool, stderr io.Writer) (*plan, error) {
	p := &plan{
		name:   im.Name,
		checks: im.Tests,
		opts:   image.Options{Insecure: deref(im.Insecure)},
	}
	if !testOnly {
		p.push = deref(im.Push)
		p.layout = deref(im.OCILayout)
		p.tarball = deref(im.Tarball)
		p.load = deref(im.Load)
		p.engine = deref(im.Engine)
	}
	if !testOnly && !p.push && p.layout == "" && p.tarball == "" && !p.load {
		return p, usagef("nothing to do: set -push, -oci-layout, -tarball, or -load")
	}
	if (p.push || p.tarball != "" || p.load) && len(im.Tags) == 0 {
		return p, usagef("-push, -tarball, and -load need -tag")
	}
	if p.engine != "" && !p.load {
		return p, usagef("-engine only applies with -load")
	}
	if p.load {
		path, err := image.FindEngine(p.engine)
		if err != nil {
			return p, err
		}
		p.engine = path
	}
	compression, level := deref(im.Compression), deref(im.CompressionLevel)
	if err := image.CheckCompression(compression, level); err != nil {
		return p, usageError{err}
	}
	platforms := strings.Join(im.Platforms, ",")
	if platforms == "" {
		platforms = "linux/" + runtime.GOARCH
	}
	var err error
	if p.platforms, err = image.ParsePlatforms(platforms); err != nil {
		return p, err
	}
	if len(p.platforms) > 1 && p.tarball != "" {
		return p, usagef("-tarball holds one platform; use -push or -oci-layout for several")
	}

	stamper := &vcs.Stamper{Dir: dir}
	for _, t := range im.Tags {
		x, err := stamper.Expand(t, true)
		if err != nil {
			return p, fmt.Errorf("tag %w", err)
		}
		p.tags = append(p.tags, x)
	}
	if _, err := image.ParseRefs(p.tags, p.opts); err != nil {
		return p, err
	}

	p.spec = image.Spec{
		Base:             deref(im.Base),
		Env:              im.Env,
		Volumes:          im.Volumes,
		WorkDir:          deref(im.WorkDir),
		User:             deref(im.User),
		StopSignal:       deref(im.StopSignal),
		Compression:      compression,
		CompressionLevel: level,
	}
	if p.spec.Base == "" {
		p.spec.Base = image.Scratch
	}
	if im.Entrypoint != nil {
		p.spec.Entrypoint = append([]string{}, *im.Entrypoint...)
	}
	if im.Cmd != nil {
		p.spec.Cmd = append([]string{}, *im.Cmd...)
	}
	for _, port := range im.Expose {
		norm, err := image.ParsePort(port)
		if err != nil {
			return p, usageError{err}
		}
		p.spec.ExposedPorts = append(p.spec.ExposedPorts, norm)
	}
	if r := im.Rootfs.Image(); !r.Empty() {
		if r.WritesUsers() && p.spec.Base != image.Scratch {
			return p, usagef("-skeleton and -add-user write /etc/passwd, which would replace the base image's; use them with a scratch base")
		}
		ls, err := r.Layer()
		if err != nil {
			return p, err
		}
		p.spec.Layers = append(p.spec.Layers, ls)
	}
	for _, l := range im.Layers {
		ls := image.LayerSpec{Name: l.Name}
		for _, it := range l.Contents {
			ls.Items = append(ls.Items, it.Item)
		}
		p.spec.Layers = append(p.spec.Layers, ls)
	}
	if p.spec.Created, err = buildTime(); err != nil {
		return p, err
	}

	p.spec.Labels = copyMap(im.Labels)
	if err := stamper.ExpandAll(p.spec.Labels); err != nil {
		return p, fmt.Errorf("label %w", err)
	}
	if im.VCS == nil || *im.VCS {
		p.spec.Annotations = vcsAnnotations(dir, stderr)
	}
	for k, v := range im.Annotations {
		if p.spec.Annotations == nil {
			p.spec.Annotations = map[string]string{}
		}
		p.spec.Annotations[k] = v
	}
	if err := stamper.ExpandAll(p.spec.Annotations); err != nil {
		return p, fmt.Errorf("annotation %w", err)
	}
	return p, nil
}

// build builds the image or index in memory.
func (p *plan) build(ctx context.Context) (image.Artifact, error) {
	if len(p.platforms) == 1 {
		spec := p.spec
		spec.Platform = p.platforms[0]
		return image.Build(ctx, spec, p.opts)
	}
	return image.BuildIndex(ctx, p.spec, p.platforms, p.opts)
}

func (p *plan) prefix() string {
	if p.name == "" {
		return ""
	}
	return p.name + ": "
}

// run builds the image, runs its tests, and writes every output.
func (p *plan) run(ctx context.Context, stdout, stderr io.Writer) error {
	art, err := p.build(ctx)
	if err != nil {
		return err
	}
	digest, err := art.Digest()
	if err != nil {
		return err
	}
	prefix := p.prefix()
	if len(p.checks) > 0 && !p.skipTests {
		if err := runChecks(art, p.checks, prefix, stderr); err != nil {
			return err
		}
	}

	if p.layout != "" {
		refName := ""
		if len(p.tags) > 0 {
			refName = p.tags[0]
		}
		if err := image.WriteLayout(p.layout, art, refName); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "%swrote OCI layout %s\n", prefix, p.layout)
	}
	if p.tarball != "" {
		if err := image.WriteTarball(p.tarball, art, p.tags, p.opts); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "%swrote tarball %s\n", prefix, p.tarball)
	}
	if p.load {
		img, err := image.SelectPlatform(art, hostPlatform())
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}
		if err := image.Load(ctx, p.engine, img, p.tags, p.opts, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "%sloaded %s into %s\n", prefix, strings.Join(p.tags, ", "), filepath.Base(p.engine))
	}
	if p.push {
		refs, err := image.PushAll(ctx, art, p.tags, p.opts)
		if err != nil {
			return err
		}
		for _, t := range p.tags {
			fmt.Fprintf(stderr, "%spushed %s\n", prefix, t)
		}
		for _, r := range refs {
			fmt.Fprintln(stdout, r)
		}
		return nil
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

// hostPlatform is the platform a local engine runs: Linux on this CPU,
// since Docker and Podman run Linux containers in a VM on macOS.
func hostPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// keyValues parses KEY=VALUE pairs; it returns nil for an empty list.
func keyValues(what string, list []string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q: want KEY=VALUE", what, kv)
		}
		m[k] = v
	}
	return m, nil
}

// vcsAnnotations returns the OCI revision and source annotations for the
// Git working tree containing dir, or nil outside a repository.
func vcsAnnotations(dir string, stderr io.Writer) map[string]string {
	info, err := vcs.Read(dir)
	if err != nil {
		return nil
	}
	if info.Dirty {
		fmt.Fprintf(stderr, "construct: warning: %s has uncommitted changes; the revision annotation names commit %s\n", dir, info.Short)
	}
	m := map[string]string{"org.opencontainers.image.revision": info.Commit}
	if info.Remote != "" {
		m["org.opencontainers.image.source"] = info.Remote
	}
	return m
}

// buildTime honours SOURCE_DATE_EPOCH (reproducible-builds.org) and
// otherwise uses the Unix epoch, so rebuilding the same inputs yields the
// same digest.
func buildTime() (time.Time, error) {
	s := os.Getenv("SOURCE_DATE_EPOCH")
	if s == "" {
		return time.Unix(0, 0).UTC(), nil
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q: %w", s, err)
	}
	return time.Unix(sec, 0).UTC(), nil
}
