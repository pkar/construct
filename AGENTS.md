# construct

Go CLI that builds OCI images from a base image plus local files, without a
container daemon, and writes them to a registry, an OCI layout, a
`docker load` tarball, or straight into docker/podman. Registry and image
handling use `github.com/google/go-containerregistry`; build files use
`gopkg.in/yaml.v3`.

- `cmd/construct/`: CLI. `main.go` dispatches subcommands; `build.go` turns
  flags and build files into a `plan` (`newPlan`) and runs it; `flags.go`
  holds the layer/item flag types; `lock.go`, `load.go`, `test.go` are the
  `lock`, `load`, and `test` commands. `version` is stamped via
  `-X main.version`.
- `internal/config/`: `construct.yaml` parsing and the merge rules (settings
  replace, lists append, maps merge). Flags reuse `config.Image` and
  `config.Merge`, so a new build option is added there and in `build.go`.
- `internal/image/`: layer items (`item.go`), layer packing (`layer.go`,
  streamed from disk through a pipe), image and index assembly (`build.go`),
  outputs (`output.go`; the docker tarball writer is our own so tag order
  stays deterministic), base locks (`lock.go`), engine loading (`load.go`),
  and the generated rootfs layer (`rootfs.go`).
- `internal/vcs/`: Git info for annotations and `{git.*}`/`{env.*}` stamping.
- `internal/check/`: structure tests run against the flattened filesystem
  and config.
- `examples/construct.yaml`: documented example; `TestExampleFile` keeps it
  parsing. Keep it and README.md in step with the flags.
- `.github/workflows/`: `ci.yml` (Linux/macOS checks) and `release.yml`
  (tests, then `make dist` and `gh release create` on `v*` tags). Release
  targets live in `DIST_TARGETS` in the Makefile.
- `install.sh`: installs the latest release, verifying `checksums.txt`, or
  builds the tag with Go on other targets. Uses a logged-in `gh` (the repo
  is private) and falls back to anonymous `curl`. `install_test.go` runs it
  against fake `gh`/`curl`/`go`; keep asset names in step with `make dist`.

GitHub remote: `git@github.com:pkar/construct.git`. Do not push tags or
publish releases unless asked.

Builds must stay reproducible: sorted tar entries, root ownership, and
timestamps from `SOURCE_DATE_EPOCH` (default Unix epoch). Tests use an
in-process registry, so they need no network or Docker; `-load` tests use a
fake engine script on `PATH`. In the macOS sandbox, `go test` needs loopback
networking for the `httptest` registry.

Checks: `make check` (gofmt, `go vet ./...`, `go test -race ./...`).
Never add a `Co-Authored-By:` footer to commits.
