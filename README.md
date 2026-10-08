# construct

`construct` builds OCI container images from local files and pushes them to a
registry. It does not need Docker, a daemon, or root: it takes a base image
(or `scratch`), adds your files as one layer, sets the image config, and writes
the result.

Builds are reproducible. Files are added in sorted order, owned by root, and
stamped with `SOURCE_DATE_EPOCH` (the Unix epoch when unset), so the same
inputs give the same digest.

## Install

With the installer (needs `gh` logged in to an account that can read this
private repository, or plain `curl` once the repository is public):

```sh
gh api repos/pkar/construct/contents/install.sh -H 'Accept: application/vnd.github.raw' > install.sh
sh install.sh
```

Or run `sh install.sh` from a clone. It resolves the latest release once and
fetches everything from that tag. On Linux amd64/arm64 and macOS arm64 it
downloads the binary and checks it against the release's `checksums.txt`; on
other Linux/macOS targets it builds that tag with Go. It installs to
`~/.local/bin`; set `CONSTRUCT_INSTALL_DIR` to choose another directory.
Checksums catch corrupted downloads; they are not independent signatures.

From source:

```sh
make install            # builds bin/construct and installs to ~/.local/bin
make install BINDIR=/usr/local/bin
make install PREFIX=/opt/construct   # installs to /opt/construct/bin
```

Tagged releases publish `construct-linux-amd64`, `construct-linux-arm64`,
`construct-darwin-arm64`, and `checksums.txt` on the GitHub releases page,
if you would rather download by hand.

## Examples

Push a static Go binary on an empty base:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o app ./cmd/app
construct build \
  -platform linux/amd64 \
  -add app:/usr/local/bin/app \
  -entrypoint /usr/local/bin/app \
  -tag registry.example.com/team/app:v1 \
  -push
```

Layer files onto a base image from a registry:

```sh
construct build \
  -base gcr.io/distroless/static-debian12:nonroot \
  -add ./public:/srv/www \
  -add ./config.json:/etc/app/ \
  -env PORT=8080 \
  -label org.opencontainers.image.source=https://github.com/pkar/app \
  -entrypoint '["/usr/local/bin/app", "-config", "/etc/app/config.json"]' \
  -tag registry.example.com/team/app:v1 \
  -push
```

Build one image for several platforms. `{os}`, `{arch}`, and `{variant}` in
an `-add` source are filled in per platform, so each image gets its own
binary. The result is an OCI image index, and a multi-platform base image
contributes the matching platform to each image:

```sh
for arch in amd64 arm64; do
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o dist/app-linux-$arch ./cmd/app
done
construct build \
  -base gcr.io/distroless/static-debian12:nonroot \
  -platform linux/amd64,linux/arm64 \
  -add 'dist/app-{os}-{arch}:/usr/local/bin/app' \
  -entrypoint /usr/local/bin/app \
  -tag registry.example.com/team/app:v1 \
  -push
```

Build now, push later:

```sh
construct build -add app:/app -entrypoint /app -oci-layout ./out
construct push ./out registry.example.com/team/app:v1
```

Load into Docker or Podman without a registry:

```sh
construct build -add app:/app -entrypoint /app -tag app:dev -tarball app.tar
docker load -i app.tar
```

`build` prints the image digest; with `-push` it prints the full
`repo@sha256:...` reference instead.

## Flags

| Flag | Meaning |
| --- | --- |
| `-base REF` | Base image, or `scratch` (default). |
| `-platform os/arch[/variant],...` | Target platforms; default `linux/<host arch>`. One platform builds an image, several build an image index. The base image must provide each platform. |
| `-add SRC:DST` | Copy a host file or directory to an absolute path in the image. A file with `DST` ending in `/` keeps its name. `{os}`, `{arch}`, `{variant}` in `SRC` expand per platform. Repeatable. |
| `-entrypoint`, `-cmd` | JSON array or space-separated words. Unset inherits from the base; `'[]'` clears. A new entrypoint drops the inherited cmd, as in a Dockerfile. |
| `-env KEY=VALUE`, `-label KEY=VALUE` | Repeatable; replace base values with the same key. |
| `-workdir`, `-user` | Override the base values. |
| `-tag REF` | Image reference used by `-push` and `-tarball`, and recorded in `-oci-layout`. |
| `-push`, `-oci-layout DIR`, `-tarball FILE` | Outputs; at least one is required. `-oci-layout` replaces the layout's index with this image or index. `-tarball` takes a single platform only. |
| `-insecure` | Allow plain HTTP or unverified TLS registries. `localhost` and `127.0.0.1` already use HTTP. |

## Registry credentials

`construct` reads the Docker config (`~/.docker/config.json`, or
`$DOCKER_CONFIG`), including credential helpers, so log in with
`docker login`, `podman login --authfile ~/.docker/config.json`, or
`crane auth login`.

## Development

```sh
make check   # gofmt, go vet, go test -race
make dist    # cross-build release binaries into dist/
```

Tests run against an in-process registry and need no network. CI
(`.github/workflows/ci.yml`) runs the same checks on Linux and macOS for
pushes to `main` and for pull requests.

## Releasing

Push a `v*` tag from a clean, tested `main`:

```sh
git tag -a v0.1.0 -m v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` reruns the tests, runs `make dist` with the
tag (minus the `v`) as the version, and creates the GitHub release with the
binaries and `checksums.txt`. It fails rather than overwrite a release
that already exists for the tag.

## Notes

- Each build adds one layer on top of the base. Its contents are streamed
  from disk whenever the layer is hashed or uploaded, so large trees are not
  held in memory, but sources must not change while `construct` runs.
- Hard links are stored as separate files; devices, sockets, and FIFOs are rejected.
