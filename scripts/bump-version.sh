#!/usr/bin/env bash
#
# Prepare a release: set the version everywhere it is written down, and open
# the CHANGELOG section for it.
#
#   ./scripts/bump-version.sh 2.8.0
#   ./scripts/bump-version.sh v2.8.0        # the v is optional
#   DATE=2026-09-11 ./scripts/bump-version.sh 2.8.0
#
# It edits three files, which is what every release before this one touched:
#
#   cmd/qvd2parquet/main.go   defaultVersion, what the binary reports
#   README.md                 the two banner lines, which a test pins to it
#   CHANGELOG.md              a heading for the version, and the compare links
#
# What it does not do is commit, push, tag or release. The version has to be
# chosen by a person -- the compatibility promise at the top of CHANGELOG.md
# decides between a patch, a minor and a major, and no script can read a
# changelog and tell which one the entries add up to. The steps it leaves are
# printed at the end.

set -euo pipefail

cd "$(dirname "$0")/.."

version=${1:?usage: bump-version.sh <version>   e.g. 2.8.0}
version=${version#v}
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "bump-version: $version is not a MAJOR.MINOR.PATCH version" >&2
	exit 1
fi
date=${DATE:-$(date -u +%F)}

main=cmd/qvd2parquet/main.go
readme=README.md
changelog=CHANGELOG.md

current=$(sed -n 's/^const defaultVersion = "\(.*\)"$/\1/p' "$main")
if [[ -z $current ]]; then
	echo "bump-version: no defaultVersion in $main" >&2
	exit 1
fi
if [[ $current == "$version" ]]; then
	echo "bump-version: already at $version" >&2
	exit 1
fi
if grep -q "^## \[$version\]" "$changelog"; then
	echo "bump-version: $changelog already has a $version section" >&2
	exit 1
fi

# An empty Unreleased section means there is nothing to release, and a release
# whose notes are empty is worse than no release: the tag exists, the archives
# build, and the changelog says the version changed nothing.
if ! awk '/^## \[Unreleased\]/{u=1; next} /^## \[/{exit} u && NF {found=1} END{exit !found}' "$changelog"; then
	echo "bump-version: the Unreleased section of $changelog is empty" >&2
	exit 1
fi

# Written through a temporary and moved, so a failure partway leaves the file
# as it was rather than half edited. sed -i is deliberately not used: it takes
# an argument on macOS that it rejects on Linux.
edit() {
	local file=$1
	shift
	local tmp
	tmp=$(mktemp "$file.XXXXXX")
	"$@" <"$file" >"$tmp"
	mv "$tmp" "$file"
}

edit "$main" sed "s/^const defaultVersion = \"$current\"$/const defaultVersion = \"$version\"/"
edit "$readme" sed "s/^qvd2parquet $current  /qvd2parquet $version  /"

# The heading goes directly under Unreleased, so what was unreleased becomes
# this version and Unreleased is left empty for the next change.
edit "$changelog" awk -v v="$version" -v d="$date" '
	/^## \[Unreleased\]$/ && !done {
		print; print ""; print "## [" v "] - " d; print ""
		done = 1
		getline            # the blank line that followed Unreleased
		next
	}
	{ print }
'
# The compare links at the foot: Unreleased now starts from this version, and
# this version spans from the one before it.
edit "$changelog" awk -v v="$version" -v c="$current" '
	/^\[Unreleased\]: / {
		base = $2
		sub(/v[0-9.]+\.\.\.HEAD$/, "", base)
		print "[Unreleased]: " base "v" v "...HEAD"
		print "[" v "]: " base "v" c "...v" v
		next
	}
	{ print }
'

echo "bumped $current -> $version ($date)"
git --no-pager diff --stat
cat <<NEXT

Verify and finish:

  go test ./...                          # the banner and changelog tests pin this
  git switch -c release-$version
  git commit -am "Release $version"      # say what kind of bump, and why
  git push -u origin release-$version
  gh pr create --base main

Once it is merged and CI is green on main:

  git switch main && git pull
  git tag -a v$version -m "Release $version"
  git push origin v$version              # this is what builds and publishes

NEXT
