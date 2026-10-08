# construct

`construct` builds OCI container images from local files without Docker, a
daemon, or root. It takes a base image (or `scratch`), adds your files as
layers, sets the image config, and pushes the result to a registry, writes
an OCI layout or a `docker load` tarball, or loads it straight into Docker
or Podman.

Builds are reproducible. Tar entries are sorted, owned by root unless you
say otherwise, and stamped with `SOURCE_DATE_EPOCH` (the Unix epoch when
unset), so the same inputs give the same digest.

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

## Quick start

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

`build` prints the image digest. With `-push` it prints one
`repo@sha256:...` reference per repository instead.

## Build files

Anything past a one-liner belongs in a `construct.yaml`. One file can hold
several images that share defaults. [`examples/construct.yaml`](examples/construct.yaml)
uses every feature:

```yaml
base: gcr.io/distroless/static-debian12:nonroot
platforms: [linux/amd64, linux/arm64]
images:
  - name: api
    tags:
      - registry.example.com/team/api:{git.short}
      - registry.example.com/team/api:latest
    push: true
    entrypoint: [/usr/local/bin/api]
    layers:
      - name: app
        contents:
          - dist/api-linux-{arch}:/usr/local/bin/api
    tests:
      - {file: /usr/local/bin/api, mode: "0755"}
  - name: worker
    oci-layout: out/worker
    layers:
      - contents: ["dist/worker-linux-{arch}:/worker"]
```

```sh
construct build -f construct.yaml            # every image
construct build -f construct.yaml api        # just api
construct build -f construct.yaml -tag registry.example.com/team/api:rc1 api
```

Rules:

- Top-level fields are defaults for each entry in `images:`. A file with no
  `images:` describes one image.
- Settings (`base`, `user`, `tags`, outputs, ...) replace the defaults.
  `layers`, `env`, `expose`, `volumes`, `tests`, and `rootfs.users` add to
  them. `labels` and `annotations` merge key by key.
- Command-line flags apply on top, to every selected image, by the same
  rules. So `-add` adds a layer item, and `-tag` replaces the tags.
- Relative paths are relative to the build file, wherever you run from.
  Unknown fields are errors.
- A layer item is either the `-add` string `SRC:DST[:OPTIONS]` or a mapping
  with exactly one of `src` (plus `dst`), `mkdir`, `symlink` (plus
  `target`), or `file` (plus `content`). Each can also take `mode`,
  `dirmode`, and `owner`.
- `entrypoint` and `cmd` take a list or a space-separated string. `[]`
  clears the base image's value.
- YAML reads a bare `{` inside `[ ]` as the start of a mapping, so quote
  values with `{git.short}` and similar in flow lists:
  `tags: ["app:{git.short}"]`. Block lists (`- app:{git.short}`) need no
  quotes.

## Layers and files

Each `-layer NAME` starts a new layer, and the `-add`, `-mkdir`, and
`-symlink` flags after it go into that layer. Put things that change rarely
(dependencies, assets) in early layers and your binary last, so pushes
upload only what changed.

```sh
construct build \
  -layer deps   -add vendor/lib:/opt/app/lib \
  -layer app    -add 'dist/app:/opt/app/bin/app:mode=0755,owner=65532' \
                -mkdir /var/lib/app:mode=0700,owner=65532:65532 \
                -symlink /usr/local/bin/app:/opt/app/bin/app \
  -tag registry.example.com/team/app:v1 -push
```

- `-add SRC:DST[:OPTIONS]` copies a host file or directory. A file whose
  `DST` ends in `/` keeps its name. `{os}`, `{arch}`, and `{variant}` in `SRC`
  expand per platform.
- Options: `mode=OCTAL` (files), `dirmode=OCTAL` (directories under a copied
  tree), `owner=UID[:GID]`. Without them, host permission bits are kept
  (including setuid, setgid, and sticky) and everything is owned by 0:0.
- A later item for the same path replaces an earlier one.

### A root filesystem without a distro

`scratch` has nothing in it, which breaks programs that look up users, TLS
roots, or time zones. Instead of switching to a distroless base, generate
those files:

| Flag | Adds |
| --- | --- |
| `-skeleton` | `/etc`, `/home`, `/root` (0700), `/tmp` and `/var/tmp` (1777), `/etc/nsswitch.conf`, and `/etc/passwd` and `/etc/group` with `root`, `nobody`, and `nonroot` (65532). |
| `-add-user NAME:UID[:GID[:HOME]]` | A user and group, with a home directory it owns (default `/home/NAME`). Repeatable. |
| `-ca-certs FILE` | A PEM bundle at `/etc/ssl/certs/ca-certificates.crt`. `system` uses the build machine's bundle. |
| `-tzdata DIR` | A zoneinfo tree at `/usr/share/zoneinfo`. `system` uses the build machine's. |

These go into one `rootfs` layer below your own layers. `-skeleton` and
`-add-user` need a `scratch` base, because the generated `/etc/passwd` would
replace the base image's. With `system`, the bundle and zone data come from
whichever machine runs the build, so pin them with a path if two machines
must produce the same digest.

## Image config

| Flag | Meaning |
| --- | --- |
| `-base REF` | Base image, or `scratch` (default). |
| `-platform os/arch[/variant],...` | Target platforms. The default is `linux/<host arch>`. Several build an OCI image index, and the base must provide each one. |
| `-entrypoint`, `-cmd` | A JSON array or space-separated words. Unset inherits from the base, and `'[]'` clears it. A new entrypoint drops the inherited cmd, as in a Dockerfile. |
| `-env KEY=VALUE`, `-label KEY=VALUE` | Repeatable. Each replaces a base value with the same key. |
| `-workdir`, `-user`, `-stop-signal` | Override the base values. |
| `-expose PORT[/PROTO]`, `-volume PATH` | Repeatable. Added to the base's ports and volumes. |
| `-annotation KEY=VALUE` | Manifest annotation, also set on the index for multi-platform builds. Repeatable. |
| `-vcs=false` | Skip the Git annotations described below. |
| `-compression gzip\|zstd`, `-compression-level N` | Compression for new layers. zstd is smaller and faster to unpack, but the runtime pulling the image must support zstd layers (containerd has since 1.5), and the base must use OCI rather than Docker manifests. Levels run 1-9 for gzip and 1-22 for zstd. |

Inside a Git repository, `construct` sets `org.opencontainers.image.revision`
to the commit and `org.opencontainers.image.source` to the `origin` URL, with
any credentials stripped and SSH URLs rewritten as https. If the tree has
uncommitted changes, it prints a warning, because the revision then names a
commit that does not match the image contents.

Images built on a registry base also record
`org.opencontainers.image.base.name` and `.base.digest`, so scanners can tell
when the base has moved.

## Tags and stamping

`-tag` is repeatable. `-push` uploads to each repository once and adds the
remaining tags without uploading again. Tags, label values, and annotation
values can include:

| Variable | Value |
| --- | --- |
| `{git.commit}`, `{git.short}` | The HEAD commit, in full or abbreviated to at least 12 characters. |
| `{git.branch}` | The current branch. Fails on a detached HEAD. |
| `{git.tag}` | A tag pointing at HEAD. Fails if there is none. |
| `{env.NAME}` | An environment variable. Fails if it is unset. |

In tags, characters a tag cannot hold become `-`, so the branch
`feature/login` becomes `feature-login`.

```sh
construct build -add app:/app \
  -tag 'registry.example.com/app:{git.short}' \
  -tag 'registry.example.com/app:{git.branch}' \
  -label 'build.id={env.CI_PIPELINE_ID}' -push
```

## Outputs

At least one output is required. Several can be combined.

| Flag | Writes |
| --- | --- |
| `-push` | The image to every `-tag`. |
| `-oci-layout DIR` | An OCI layout. This replaces the layout's index, and the first tag is recorded as the image name. |
| `-tarball FILE` | A `docker load`/`podman load` tarball holding every tag. Single platform only. |
| `-load [-engine docker\|podman]` | Streams the image into `docker load` or `podman load`, with no file in between. For a multi-platform build, it loads the `linux/<host arch>` image. |

Build now, push or load later:

```sh
construct build -f construct.yaml -oci-layout ./out api
construct push ./out registry.example.com/team/api:v1
construct load ./out                     # tagged with the layout's image name
construct load -platform linux/arm64 -tag api:arm ./out
```

## Pinning base images

A tag like `distroless/static:nonroot` moves. To control when your images
pick up a new base, lock it:

```sh
construct lock -f construct.yaml        # writes construct.lock next to it
git add construct.lock
construct build -f construct.yaml       # builds on the locked digests
construct build -f construct.yaml -locked   # in CI: fail if a base is not locked
```

`construct.lock` maps each base reference to the digest its registry served
when you ran `lock`. Run `lock` again to move to newer bases. When the build
file sits next to a `construct.lock`, builds use it automatically.
Otherwise, pass `-lock FILE`. Without `-locked`, a build adds any base the
lock is missing.

Without a build file:
`construct lock -lock bases.lock -base REF [-base REF...]`.

## Structure tests

Tests check a built image's files and config. They need no container
runtime. Put them under `tests:` in the build file:

```yaml
tests:
  - {file: /usr/local/bin/api, type: file, mode: "0755", owner: "0:0"}
  - {file: /etc/api/config.toml, contains: 'listen = ":\d+"'}
  - {file: /bin/api, target: /usr/local/bin/api}       # a symlink
  - {absent: /bin/sh}
  - name: runs as nonroot
    config:
      user: nonroot
      entrypoint: [/usr/local/bin/api]
      env: {TZ: UTC}           # env, labels, and expose must be present;
      labels: {team: core}     # other entries are allowed
      expose: [8080]
```

- `construct build` runs an image's tests before writing any output. If a
  test fails, nothing is pushed. Use `-test=false` to skip them.
- `construct test -f construct.yaml [IMAGE...]` builds in memory and runs
  the tests, writing nothing.
- `construct test -checks FILE SOURCE` runs the tests from FILE against an
  OCI layout directory or a registry image. FILE can be the build file
  itself (with `-image NAME` if it has several images) or a file holding
  only a `tests:` list.

Every platform of a multi-platform image is checked. Paths are checked as
stored and are not resolved through symlinks. `contains` is a Go regular
expression and reads at most 64 MiB of a file.

## Base images and pushing

A build on a registry base reads only the base's manifest and config. When
you push to the same registry, the base layers are mounted into the target
repository, so they are never downloaded. Pushing to a different registry
copies them across.

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

- Layer contents are streamed from disk whenever a layer is hashed or
  uploaded, so large trees are not held in memory. Sources must not change
  while `construct` runs.
- Hard links are stored as separate files. Devices, sockets, and FIFOs are
  rejected.
- Image signing and SBOMs are not built in. Use `cosign` or `syft` on the
  pushed digest.
