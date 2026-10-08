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

	"github.com/pkar/construct/internal/image"
)

// version is stamped by the release build via -ldflags.
var version = "dev"

const usage = `construct builds OCI container images and pushes them to registries.

Usage:
  construct build [flags]           build an image from a base and local files
  construct push [flags] DIR REF    push the image or index in an OCI layout to REF
  construct lock [flags] [IMAGE...] pin base images to digests in construct.lock
  construct version                 print the version

Run 'construct COMMAND -h' for flags.

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
	case "lock":
		err = runLock(ctx, args[1:], stdout, stderr)
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
