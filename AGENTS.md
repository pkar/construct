# construct

Go CLI that builds OCI images from a base image plus local files, without a
container daemon, and writes them to a registry, an OCI layout, or a
`docker load` tarball. Registry and image handling use
`github.com/google/go-containerregistry`.

- `cmd/construct/`: CLI entry point, flag parsing, `version` stamped via
  `-X main.version`.
- `internal/image/`: layer packing (`layer.go`), image assembly (`build.go`),
  and outputs (`output.go`).

Builds must stay reproducible: sorted tar entries, root ownership, and
timestamps from `SOURCE_DATE_EPOCH` (default Unix epoch). Tests use an
in-process registry, so they need no network or Docker.

Checks: `make check` (gofmt, `go vet ./...`, `go test -race ./...`).
Never add a `Co-Authored-By:` footer to commits.
