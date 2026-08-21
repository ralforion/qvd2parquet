#!/usr/bin/env bash
#
# Cross-compile qvd2parquet for every supported platform and package the
# results into dist/ as .tar.gz (Unix) or .zip (Windows), plus SHA256SUMS.
#
#   ./scripts/build-release.sh              # version from git describe
#   VERSION=v1.2.3 ./scripts/build-release.sh
#   PLATFORMS="linux/amd64 windows/amd64" ./scripts/build-release.sh
#
# The binaries are pure Go (CGO_ENABLED=0), so they are statically linked and
# need no runtime dependencies.

set -euo pipefail

cd "$(dirname "$0")/.."

BINARY=qvd2parquet
DIST=${DIST:-dist}
# Prefer an exact tag, then a tag-derived description, then the baseline.
VERSION=${VERSION:-$(git describe --tags --dirty 2>/dev/null || echo v0.1.0)}

# Every platform verified to compile. All are pure Go.
DEFAULT_PLATFORMS="
darwin/amd64
darwin/arm64
linux/amd64
linux/arm64
linux/386
linux/arm
linux/ppc64le
linux/s390x
linux/riscv64
windows/amd64
windows/arm64
windows/386
freebsd/amd64
freebsd/arm64
netbsd/amd64
openbsd/amd64
"
PLATFORMS=${PLATFORMS:-$DEFAULT_PLATFORMS}

rm -rf "$DIST"
mkdir -p "$DIST"

echo "building $BINARY $VERSION"

for platform in $PLATFORMS; do
    GOOS=${platform%/*}
    GOARCH=${platform#*/}
    name="${BINARY}_${VERSION}_${GOOS}_${GOARCH}"
    stage="$DIST/$name"
    exe="$BINARY"
    [ "$GOOS" = windows ] && exe="$BINARY.exe"

    mkdir -p "$stage"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
        go build -trimpath \
            -ldflags "-s -w -X main.version=$VERSION" \
            -o "$stage/$exe" ./cmd/qvd2parquet

    cp README.md LICENSE "$stage/" 2>/dev/null || cp README.md "$stage/"

    if [ "$GOOS" = windows ]; then
        (cd "$DIST" && zip -qr "$name.zip" "$name")
    else
        tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
    fi
    rm -rf "$stage"

    printf '  %-28s %s\n' "$platform" "$(ls -lh "$DIST/$name".* | awk '{print $5}')"
done

# Collect the archives explicitly. Globbing both patterns directly would fail
# under `set -e` when only one matches, which is what happens whenever a single
# platform is built, e.g. PLATFORMS="linux/amd64".
(
    cd "$DIST"
    shopt -s nullglob
    archives=(*.tar.gz *.zip)
    if [ ${#archives[@]} -eq 0 ]; then
        echo "no archives were produced" >&2
        exit 1
    fi
    shasum -a 256 "${archives[@]}" > SHA256SUMS
)

echo
echo "artifacts in $DIST/:"
ls -1 "$DIST"
