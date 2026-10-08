package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/pkar/construct/internal/image"
)

const loadUsage = `Usage:
  construct load [flags] DIR

Load the image in the OCI layout DIR into a local container engine with
'docker load' or 'podman load'. Tags default to the name the layout was
built with (its first -tag). For an image index, the image for -platform
is loaded.

Flags:
`

func runLoad(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("construct load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		engine   = fs.String("engine", "", "container `engine`: docker or podman (default: whichever is installed)")
		platform = fs.String("platform", "", "`platform` to load from an image index (default "+hostPlatform().String()+")")
		tags     repeated
	)
	fs.Var(&tags, "tag", "tag the loaded image as `reference` (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), loadUsage)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErr(fs, "want one OCI layout directory")
	}
	p := hostPlatform()
	if *platform != "" {
		ps, err := image.ParsePlatforms(*platform)
		if err != nil || len(ps) != 1 {
			return usageErr(fs, "-platform %q: want one os/arch[/variant]", *platform)
		}
		p = ps[0]
	}
	a, ref, err := image.ReadLayoutRef(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		if ref == "" {
			return fmt.Errorf("%s has no image name; set -tag", fs.Arg(0))
		}
		tags = repeated{ref}
	}
	img, err := image.SelectPlatform(a, p)
	if err != nil {
		return err
	}
	path, err := image.FindEngine(*engine)
	if err != nil {
		return err
	}
	if err := image.Load(ctx, path, img, tags, image.Options{}, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "loaded %s into %s\n", strings.Join(tags, ", "), filepath.Base(path))
	d, err := img.Digest()
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, d)
	return nil
}
