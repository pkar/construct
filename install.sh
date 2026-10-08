#!/bin/sh
# Install construct from the latest GitHub release. On linux/amd64,
# linux/arm64, and darwin/arm64 it downloads the prebuilt binary and checks
# it against the release's checksums.txt; on other Linux/macOS targets it
# builds the same tag with Go. Downloads go through a logged-in gh CLI when
# one is available, which avoids anonymous rate limits, otherwise curl.
# Installs to $CONSTRUCT_INSTALL_DIR (default ~/.local/bin).
set -eu
REPO=pkar/construct
fail() { echo "construct install: $*" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' 0
trap 'exit 1' INT TERM

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in linux|darwin) ;; *) fail "unsupported OS: $os" ;; esac
case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac

if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
 via=gh
elif command -v curl >/dev/null 2>&1; then
 via=curl
else
 fail "needs a logged-in gh CLI or curl"
fi

# Resolve one tag and fetch everything from that same tag.
case "$via" in
gh)
 tag=$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>/dev/null) ||
  fail "could not resolve the latest release of $REPO"
 ;;
curl)
 release=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null) ||
  fail "could not resolve the latest release of $REPO (check the network; if GitHub is rate-limiting you, install gh and run 'gh auth login')"
 prefix="https://github.com/$REPO/releases/tag/"
 case "$release" in "$prefix"*) tag=${release#"$prefix"} ;; *) fail "invalid release URL" ;; esac
 ;;
esac
case "$tag" in v[0-9]*) ;; *) fail "invalid release tag" ;; esac
case "$tag" in *[!a-zA-Z0-9.-]*) fail "invalid release tag" ;; esac

# fetch_asset NAME DEST downloads release asset NAME of $tag.
fetch_asset() {
 case "$via" in
 gh) gh release download "$tag" --repo "$REPO" --pattern "$1" --output "$2" >/dev/null 2>&1 ;;
 curl) curl -fsSL -o "$2" "https://github.com/$REPO/releases/download/$tag/$1" ;;
 esac
}

# fetch_source DEST downloads the source tarball of $tag.
fetch_source() {
 case "$via" in
 gh) gh api "repos/$REPO/tarball/$tag" > "$1" ;;
 curl) curl -fsSL -o "$1" "https://github.com/$REPO/archive/refs/tags/$tag.tar.gz" ;;
 esac
}

asset="construct-$os-$arch"
case "$os/$arch" in
linux/amd64|linux/arm64|darwin/arm64)
 fetch_asset "$asset" "$tmp/construct" || fail "could not download $asset"
 fetch_asset checksums.txt "$tmp/checksums.txt" || fail "could not download checksums"
 expected=""
 while read -r digest name extra; do
  if [ "$name" = "$asset" ]; then
   [ -z "$expected" ] && [ -z "$extra" ] || fail "ambiguous checksum entry"
   [ "${#digest}" -eq 64 ] || fail "invalid checksum"
   case "$digest" in *[!0-9a-f]*) fail "invalid checksum" ;; esac
   expected=$digest
  fi
 done < "$tmp/checksums.txt"
 [ -n "$expected" ] || fail "missing checksum for $asset"
 if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/construct")
 elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/construct")
 else
  fail "checksum verification requires sha256sum or shasum"
 fi
 [ "${actual%% *}" = "$expected" ] || fail "checksum mismatch for $asset"
 ;;
*)
 command -v go >/dev/null 2>&1 || fail "no prebuilt binary for $os/$arch; install Go to build from source"
 fetch_source "$tmp/source.tar.gz" || fail "could not download source for $tag"
 mkdir "$tmp/source"
 tar -xzf "$tmp/source.tar.gz" -C "$tmp/source" --strip-components=1
 (cd "$tmp/source" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${tag#v}" -o "$tmp/construct" ./cmd/construct) ||
  fail "build from source failed"
 ;;
esac

BINDIR=${CONSTRUCT_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$BINDIR"
install -m 0755 "$tmp/construct" "$BINDIR/construct"
echo "installed $BINDIR/construct ($tag)"
case ":$PATH:" in *:"$BINDIR":*) ;; *) echo "note: $BINDIR is not on your PATH" >&2 ;; esac
