// Command construct builds OCI container images from local files without a
// container daemon and pushes them to a registry.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pkar/construct/internal/image"
	"github.com/pkar/construct/internal/vcs"
)

// version is stamped by the release build via -ldflags.
var version = "dev"

const usage = `construct builds OCI container images and pushes them to registries.

Usage:
  construct build [flags]          build an image from a base and local files
  construct push [flags] DIR REF   push the image or index in an OCI layout to REF
  construct version                print the version

Run 'construct build -h' or 'construct push -h' for flags.

Registry credentials come from the Docker config (~/.docker/config.json,
including credential helpers), so 'docker login' or 'crane auth login' work.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// errUsage marks errors already reported by the flag package.
var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "build":
		err = runBuild(ctx, args[1:], stdout, stderr)
	case "push":
		err = runPush(ctx, args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "construct", version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "construct: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintln(stderr, "construct:", err)
		return 1
	}
}

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("construct build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		base       = fs.String("base", image.Scratch, "base image reference, or scratch")
		platform   = fs.String("platform", "linux/"+runtime.GOARCH, "target `platforms` as os/arch[/variant], comma-separated; several build an image index")
		push       = fs.Bool("push", false, "push the image to every -tag")
		layoutDir  = fs.String("oci-layout", "", "write the image to an OCI layout directory")
		tarFile    = fs.String("tarball", "", "write a docker/podman-loadable tarball (needs -tag)")
		workdir    = fs.String("workdir", "", "working directory")
		user       = fs.String("user", "", "user[:group] to run as")
		insecure   = fs.Bool("insecure", false, "allow plain HTTP and unverified TLS registries")
		stopSignal = fs.String("stop-signal", "", "`SIGNAL` that stops the container, e.g. SIGINT")
		useVCS     = fs.Bool("vcs", true, "annotate the image with the Git commit and source URL of the current directory")
		expose     repeated
		volumes    repeated
		annots     repeated
		tags       repeated
		entrypoint optionalList
		cmd        optionalList
		layers     layerFlags
		env        repeated
		labels     repeated
	)
	fs.Var(layerFlag{&layers}, "layer", "start a new layer `NAME`; later -add, -mkdir, and -symlink go into it")
	fs.Var(itemFlag{&layers, image.ParseAdd}, "add", "copy host `SRC:DST[:OPTIONS]` into the image (repeatable); {os}, {arch}, {variant} in SRC\nexpand per platform. OPTIONS: mode=OCTAL,dirmode=OCTAL,owner=UID[:GID]")
	fs.Var(itemFlag{&layers, image.ParseMkdir}, "mkdir", "create directory `DST[:OPTIONS]` (repeatable). OPTIONS: mode=OCTAL,owner=UID[:GID]")
	fs.Var(itemFlag{&layers, image.ParseSymlink}, "symlink", "create symlink `DST:TARGET` (repeatable)")
	fs.Var(&entrypoint, "entrypoint", `entrypoint as a JSON array or space-separated words`)
	fs.Var(&cmd, "cmd", `default arguments as a JSON array or space-separated words`)
	fs.Var(&env, "env", "set `KEY=VALUE` in the environment (repeatable)")
	fs.Var(&labels, "label", "set label `KEY=VALUE` (repeatable)")
	fs.Var(&tags, "tag", "image `reference`, e.g. registry.example.com/team/app:v1 (repeatable); {git.commit}, {git.short},\n{git.branch}, {git.tag}, and {env.NAME} expand here and in -label and -annotation values")
	fs.Var(&annots, "annotation", "set manifest annotation `KEY=VALUE` (repeatable)")
	fs.Var(&expose, "expose", "expose `PORT[/PROTO]` (repeatable)")
	fs.Var(&volumes, "volume", "declare volume `PATH` (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: construct build [flags]\n\nAt least one of -push, -oci-layout, or -tarball is required.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr(fs, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !*push && *layoutDir == "" && *tarFile == "" {
		return usageErr(fs, "nothing to do: set -push, -oci-layout, or -tarball")
	}
	if (*push || *tarFile != "") && len(tags) == 0 {
		return usageErr(fs, "-push and -tarball need -tag")
	}
	stamper := &vcs.Stamper{Dir: "."}
	for i, t := range tags {
		x, err := stamper.Expand(t, true)
		if err != nil {
			return fmt.Errorf("tag %w", err)
		}
		tags[i] = x
	}
	if _, err := image.ParseRefs(tags, image.Options{Insecure: *insecure}); err != nil {
		return err
	}

	platforms, err := image.ParsePlatforms(*platform)
	if err != nil {
		return err
	}
	if len(platforms) > 1 && *tarFile != "" {
		return usageErr(fs, "-tarball holds one platform; use -push or -oci-layout for several")
	}
	spec := image.Spec{
		Base:       *base,
		Platform:   platforms[0],
		Layers:     layers.layers,
		Entrypoint: entrypoint.list,
		Cmd:        cmd.list,
		Env:        env,
		WorkDir:    *workdir,
		User:       *user,
		StopSignal: *stopSignal,
		Volumes:    volumes,
	}
	for _, p := range expose {
		port, err := image.ParsePort(p)
		if err != nil {
			return usageErr(fs, "%v", err)
		}
		spec.ExposedPorts = append(spec.ExposedPorts, port)
	}
	if spec.Created, err = buildTime(); err != nil {
		return err
	}
	if spec.Labels, err = keyValues("label", labels); err != nil {
		return err
	}
	if *useVCS {
		spec.Annotations = vcsAnnotations(".", stderr)
	}
	extra, err := keyValues("annotation", annots)
	if err != nil {
		return err
	}
	for k, v := range extra {
		if spec.Annotations == nil {
			spec.Annotations = map[string]string{}
		}
		spec.Annotations[k] = v
	}
	if err := stamper.ExpandAll(spec.Labels); err != nil {
		return fmt.Errorf("label %w", err)
	}
	if err := stamper.ExpandAll(spec.Annotations); err != nil {
		return fmt.Errorf("annotation %w", err)
	}

	opts := image.Options{Insecure: *insecure}
	var img image.Artifact
	if len(platforms) == 1 {
		img, err = image.Build(ctx, spec, opts)
	} else {
		img, err = image.BuildIndex(ctx, spec, platforms, opts)
	}
	if err != nil {
		return err
	}
	digest, err := img.Digest()
	if err != nil {
		return err
	}

	if *layoutDir != "" {
		refName := ""
		if len(tags) > 0 {
			refName = tags[0]
		}
		if err := image.WriteLayout(*layoutDir, img, refName); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote OCI layout %s\n", *layoutDir)
	}
	if *tarFile != "" {
		if err := image.WriteTarball(*tarFile, img, tags, opts); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote tarball %s\n", *tarFile)
	}
	if *push {
		refs, err := image.PushAll(ctx, img, tags, opts)
		if err != nil {
			return err
		}
		for _, t := range tags {
			fmt.Fprintf(stderr, "pushed %s\n", t)
		}
		for _, r := range refs {
			fmt.Fprintln(stdout, r)
		}
		return nil
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

func runPush(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("construct push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	insecure := fs.Bool("insecure", false, "allow plain HTTP and unverified TLS registries")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: construct push [flags] DIR REF\n\nPush the single image or image index in OCI layout DIR to registry reference REF.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usageErr(fs, "want DIR and REF")
	}
	img, err := image.ReadLayout(fs.Arg(0))
	if err != nil {
		return err
	}
	ref, err := image.Push(ctx, img, fs.Arg(1), image.Options{Insecure: *insecure})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "pushed %s\n", fs.Arg(1))
	fmt.Fprintln(stdout, ref)
	return nil
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

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	return nil
}

func usageErr(fs *flag.FlagSet, format string, a ...any) error {
	fmt.Fprintf(fs.Output(), "%s: %s\n", fs.Name(), fmt.Sprintf(format, a...))
	fs.Usage()
	return errUsage
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
