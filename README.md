# construct

`construct` builds OCI container images from local files and pushes them to a
registry. It does not need Docker, a daemon, or root: it takes a base image
(or `scratch`), adds your files as one layer, sets the image config, and writes
the result.

Builds are reproducible. Files are added in sorted order, owned by root, and
stamped with `SOURCE_DATE_EPOCH` (the Unix epoch when unset), so the same
inputs give the same digest.

## Install

```sh
make install            # builds bin/construct and installs to ~/.local/bin
make install BINDIR=/usr/local/bin
```

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
| `-platform os/arch[/variant]` | Platform to pull from a multi-platform base; default `linux/<host arch>`. |
| `-add SRC:DST` | Copy a host file or directory to an absolute path in the image. A file with `DST` ending in `/` keeps its name. Repeatable. |
| `-entrypoint`, `-cmd` | JSON array or space-separated words. Unset inherits from the base; `'[]'` clears. A new entrypoint drops the inherited cmd, as in a Dockerfile. |
| `-env KEY=VALUE`, `-label KEY=VALUE` | Repeatable; replace base values with the same key. |
| `-workdir`, `-user` | Override the base values. |
| `-tag REF` | Image reference used by `-push` and `-tarball`, and recorded in `-oci-layout`. |
| `-push`, `-oci-layout DIR`, `-tarball FILE` | Outputs; at least one is required. `-oci-layout` replaces the layout's index with this image. |
| `-insecure` | Allow plain HTTP or unverified TLS registries. `localhost` and `127.0.0.1` already use HTTP. |

## Registry credentials

`construct` reads the Docker config (`~/.docker/config.json`, or
`$DOCKER_CONFIG`), including credential helpers, so log in with
`docker login`, `podman login --authfile ~/.docker/config.json`, or
`crane auth login`.

## Development

```sh
make check   # gofmt, go vet, go test -race
```

Tests run against an in-process registry and need no network.

## Limitations

- One platform per build; there is no multi-platform index output yet.
- Each build adds one layer, held in memory while it is written.
- Hard links are stored as separate files; devices, sockets, and FIFOs are rejected.
