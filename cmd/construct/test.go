package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"gopkg.in/yaml.v3"

	"github.com/pkar/construct/internal/check"
	"github.com/pkar/construct/internal/config"
	"github.com/pkar/construct/internal/image"
)

const testUsage = `Usage:
  construct test -f construct.yaml [IMAGE...]
  construct test -checks FILE [flags] SOURCE

Run structure tests: checks on an image's files and config that need no
container runtime. With -f, each selected image is built in memory and
checked against its tests: (construct build also runs them before writing
outputs). With -checks, the tests: list in FILE is run against SOURCE,
an OCI layout directory or a registry reference. Every platform of a
multi-platform image is checked.

Flags:
`

func runTest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("construct test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		file     = fs.String("f", "", "build images from build `file` and run their tests")
		checks   = fs.String("checks", "", "run the tests: in `file` against SOURCE")
		insecure = fs.Bool("insecure", false, "allow plain HTTP and unverified TLS registries")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), testUsage)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case (*file == "") == (*checks == ""):
		return usageErr(fs, "set one of -f or -checks")
	case *checks != "" && fs.NArg() != 1:
		return usageErr(fs, "-checks needs one SOURCE: an OCI layout directory or a registry reference")
	case *checks != "":
		return testSource(ctx, *checks, fs.Arg(0), image.Options{Insecure: *insecure}, stdout, stderr)
	}

	f, err := config.Load(*file)
	if err != nil {
		return err
	}
	sel, err := f.Select(fs.Args())
	if err != nil {
		return err
	}
	lock, err := openLock("", f.Dir, false)
	if err != nil {
		return err
	}
	var failed []string
	for _, im := range sel {
		if *insecure {
			im.Insecure = insecure
		}
		p, err := newPlan(im, f.Dir, true, stderr)
		if err != nil {
			return p.errorf("%w", err)
		}
		if len(p.checks) == 0 {
			fmt.Fprintf(stderr, "%sno tests\n", p.prefix())
			continue
		}
		// Test against the locked base, but leave the lock file alone.
		if err := lock.pin(ctx, p); err != nil {
			return p.errorf("%w", err)
		}
		art, err := p.build(ctx)
		if err != nil {
			return p.errorf("%w", err)
		}
		if err := runChecks(art, p.checks, p.prefix(), stderr); err != nil {
			if !errors.Is(err, errChecksFailed) {
				return p.errorf("%w", err)
			}
			failed = append(failed, p.label())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("tests failed for %s", strings.Join(failed, ", "))
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}

// testSource runs the checks in file against a layout or registry image.
func testSource(ctx context.Context, file, source string, opts image.Options, stdout, stderr io.Writer) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc struct {
		Tests []check.Check `yaml:"tests"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if len(doc.Tests) == 0 {
		return fmt.Errorf("%s: no tests", file)
	}
	var art image.Artifact
	if info, err := os.Stat(source); err == nil && info.IsDir() {
		art, err = image.ReadLayout(source)
		if err != nil {
			return err
		}
	} else if art, err = image.Fetch(ctx, source, opts); err != nil {
		return err
	}
	if err := runChecks(art, doc.Tests, "", stderr); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}

var errChecksFailed = errors.New("tests failed")

// runChecks runs checks against every image in art, prints failures, and
// returns errChecksFailed if any check failed.
func runChecks(art image.Artifact, checks []check.Check, prefix string, stderr io.Writer) error {
	type target struct {
		label string
		img   v1.Image
	}
	var targets []target
	switch a := art.(type) {
	case v1.Image:
		targets = append(targets, target{"", a})
	case v1.ImageIndex:
		im, err := a.IndexManifest()
		if err != nil {
			return err
		}
		for _, d := range im.Manifests {
			img, err := a.Image(d.Digest)
			if err != nil {
				return err
			}
			label := d.Digest.String()
			if d.Platform != nil {
				label = d.Platform.String()
			}
			targets = append(targets, target{label + ": ", img})
		}
	default:
		return fmt.Errorf("unsupported artifact %T", art)
	}
	failures := 0
	for _, t := range targets {
		msgs, err := check.Run(t.img, checks)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			fmt.Fprintf(stderr, "%sFAIL %s%s\n", prefix, t.label, m)
		}
		failures += len(msgs)
	}
	total := len(checks) * len(targets)
	if failures > 0 {
		fmt.Fprintf(stderr, "%s%d of %d tests failed\n", prefix, failures, total)
		return errChecksFailed
	}
	fmt.Fprintf(stderr, "%s%d tests passed\n", prefix, total)
	return nil
}
