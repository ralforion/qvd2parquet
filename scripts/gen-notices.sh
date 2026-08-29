#!/usr/bin/env bash
#
# Generate THIRD-PARTY-NOTICES.md from the modules that end up inside the
# released binary.
#
#   ./scripts/gen-notices.sh            # rewrite THIRD-PARTY-NOTICES.md
#   ./scripts/gen-notices.sh --check    # exit 1 if the file is out of date
#
# The binary is statically linked (CGO_ENABLED=0), so every dependency's
# compiled code ships inside it. The MIT, BSD and Apache-2.0 licences involved
# all attach attribution duties to redistribution in binary form, and
# Apache-2.0 4(d) additionally requires carrying forward each dependency's own
# NOTICE file. Shipping an archive without those texts is what this fixes.
#
# Only the dependencies of ./cmd/qvd2parquet are listed. Test-only and tooling
# modules appear in go.sum but are not linked into anything shipped, so they
# carry no redistribution duty.

set -euo pipefail

cd "$(dirname "$0")/.."

PKG=./cmd/qvd2parquet
OUT=THIRD-PARTY-NOTICES.md

check=false
if [ "${1:-}" = "--check" ]; then
    check=true
elif [ $# -gt 0 ]; then
    echo "usage: $0 [--check]" >&2
    exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# LICENCE is the British spelling a few modules use. PATENTS is the additional
# patent grant the golang.org/x modules license under alongside their BSD text.
license_files() {
    find "$1" -maxdepth 1 -type f \
        \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' \
           -o -iname 'NOTICE*' -o -iname 'PATENTS' \) | sort
}

# .Module.Dir is empty until the module is in the local cache.
go mod download

main_module=$(go list -m)

# Inside {{with .Module}} the dot is the module and $ is the package, so this
# prints one line per linked package: module path, version, module dir, and the
# package's own directory. Standard library packages have no .Module and print
# a blank line.
go list -deps -f '{{with .Module}}{{printf "%s\t%s\t%s\t%s" .Path .Version .Dir $.Dir}}{{end}}' "$PKG" \
    | grep -v '^[[:space:]]*$' \
    | grep -v "^${main_module}	" \
    | sort -u > "$work/packages"

if [ ! -s "$work/packages" ]; then
    echo "no third-party packages found for $PKG" >&2
    exit 1
fi

# Several dependencies carry code under a licence other than the one at their
# module root: github.com/klauspost/compress vendors s2, snappy and xxhash,
# each under its own LICENSE, and all three are linked in. Resolving per
# package rather than per module is what catches them. Nested licences covering
# code that is not linked in, such as the Dart and Swift ports inside
# flatbuffers, are correctly left out.
: > "$work/licdirs"
: > "$work/modules"

while IFS=$'\t' read -r path version moddir pkgdir; do
    printf '%s\t%s\t%s\n' "$path" "$version" "$moddir" >> "$work/modules"

    # The module root always applies: it holds the NOTICE, and it is the
    # licence of record for the module as a whole.
    printf '%s\t.\n' "$path" >> "$work/licdirs"

    # Then the nearest licence at or above the package itself.
    dir=$pkgdir
    while [ "$dir" != "$moddir" ]; do
        if [ -n "$(license_files "$dir")" ]; then
            printf '%s\t%s\n' "$path" "${dir#"$moddir"/}" >> "$work/licdirs"
            break
        fi
        dir=$(dirname "$dir")
    done
done < "$work/packages"

sort -u -o "$work/modules" "$work/modules"
sort -u -o "$work/licdirs" "$work/licdirs"

# Group modules by the exact bytes of their licence texts. The Apache-2.0
# dependencies would otherwise repeat the same 200 lines apiece, and the
# golang.org/x modules share one text between them. Grouping keeps the file
# readable while still naming every module a text applies to.
: > "$work/order"

while IFS=$'\t' read -r path version moddir; do
    blob="$work/blob"
    : > "$blob"
    found=false

    # "." sorts before every real subdirectory, so the root licence leads.
    while IFS=$'\t' read -r _ reldir; do
        [ "$reldir" = . ] && dir=$moddir || dir=$moddir/$reldir
        while IFS= read -r file; do
            [ "$reldir" = . ] && label=$(basename "$file") || label=$reldir/$(basename "$file")
            {
                printf -- '--- %s ---\n\n' "$label"
                cat "$file"
                printf '\n'
            } >> "$blob"
            found=true
        done < <(license_files "$dir")
    done < <(grep "^${path}	" "$work/licdirs")

    if [ "$found" = false ]; then
        echo "no licence file in $moddir for $path $version" >&2
        echo "a dependency with no licence text cannot be redistributed" >&2
        exit 1
    fi

    hash=$(shasum -a 256 "$blob" | cut -d' ' -f1)
    if [ ! -f "$work/$hash.text" ]; then
        cp "$blob" "$work/$hash.text"
        echo "$hash" >> "$work/order"
    fi
    printf '%s\t%s\n' "$path" "$version" >> "$work/$hash.mods"
done < "$work/modules"

generated="$work/notices.md"

{
    cat <<'HEADER'
# Third-party notices

The `qvd2parquet` binary is statically linked, so the compiled code of every
module below is embedded in the executable you downloaded. Their licences are
reproduced here in full, which is what those licences ask for when the software
is redistributed in binary form.

None of them is copyleft, so nothing here places any condition on your own use
of the converted data, or on software you build alongside the converter.

This file is generated. Run `./scripts/gen-notices.sh` after changing a
dependency; CI fails when it is out of date.

## Modules

| Module | Version |
| --- | --- |
HEADER

    while IFS=$'\t' read -r path version _; do
        printf '| `%s` | %s |\n' "$path" "$version"
    done < "$work/modules"

    while IFS= read -r hash; do
        first_path=$(head -1 "$work/$hash.mods" | cut -f1)
        count=$(grep -c '' "$work/$hash.mods")

        printf '\n## %s' "$first_path"
        [ "$count" -gt 1 ] && printf ' and %d more' "$((count - 1))"
        printf '\n\n'

        [ "$count" -gt 1 ] && printf 'The following modules share this licence text.\n\n'
        while IFS=$'\t' read -r path version; do
            printf -- '- `%s` %s\n' "$path" "$version"
        done < "$work/$hash.mods"

        printf '\n```text\n'
        cat "$work/$hash.text"
        printf '```\n'
    done < "$work/order"
} > "$generated"

if [ "$check" = true ]; then
    if ! diff -u "$OUT" "$generated"; then
        echo >&2
        echo "$OUT is out of date; run ./scripts/gen-notices.sh and commit the result" >&2
        exit 1
    fi
    echo "$OUT is up to date ($(grep -c '' "$work/modules") modules)"
    exit 0
fi

mv "$generated" "$OUT"
echo "wrote $OUT ($(grep -c '' "$OUT") lines, $(grep -c '' "$work/modules") modules)"
