#!/usr/bin/env bash
#
# Print the CHANGELOG.md section for one version, without its heading.
#
#   ./scripts/changelog-section.sh v0.3.1
#
# Used by the release workflow to build the GitHub release body, so the release
# notes and the changelog can never drift apart.

set -euo pipefail

cd "$(dirname "$0")/.."

version=${1:?usage: changelog-section.sh <version>}
version=${version#v}

awk -v want="$version" '
    # Section headings look like "## [0.3.1] - 2026-08-21".
    /^## \[/ {
        if (found) exit                       # next section: stop
        line = $0
        sub(/^## \[/, "", line)
        sub(/\].*$/, "", line)
        if (line == want) { found = 1; next }
        next
    }
    found { print }
' CHANGELOG.md | awk '
    # Trim leading and trailing blank lines.
    NF { blank = 0; for (i = 0; i < pending; i++) print ""; pending = 0; print; next }
    { if (NR > 1) pending++ }
'
