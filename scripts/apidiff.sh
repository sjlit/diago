#!/usr/bin/env bash
# Compare the public API surface with a baseline git ref using apidiff.
#
# Usage:
#   scripts/apidiff.sh [baseline-git-ref]
#
# Defaults to the merge-base with the main branch. The baseline is exported to
# a temporary directory; the working tree is compared against it. Exit code 0
# means the API is unchanged or only backward-compatible; incompatible changes
# are reported and exit non-zero (apidiff's exit code is passed through).
#
# APIDIFF_VERSION pins the tool for reproducible CI runs; override it to try
# another x/exp version.
set -euo pipefail

base_ref=${1:-$(git merge-base HEAD origin/main 2>/dev/null || git merge-base HEAD main)}
root=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

apidiff_version=${APIDIFF_VERSION:-v0.0.0-20260908205506-85c1c2202aba}
apidiff() { go run "golang.org/x/exp/cmd/apidiff@${apidiff_version}" "$@"; }

mkdir -p "$tmp/base"
git -C "$root" archive "$base_ref" | tar -x -C "$tmp/base"

echo "Comparing public API of $base_ref with the working tree (apidiff ${apidiff_version})"

(
	cd "$tmp/base"
	apidiff -w "$tmp/base.api" github.com/sjlit/diago/v2
)
(
	cd "$root"
	apidiff -w "$tmp/head.api" github.com/sjlit/diago/v2
)

# apidiff prints differences but does not set a non-zero exit status, so treat
# any output as a mismatch.
if ! output=$(apidiff "$tmp/base.api" "$tmp/head.api" 2>&1); then
	printf '%s\n' "$output"
	exit 1
fi
if [ -n "$output" ]; then
	printf '%s\n' "$output"
	exit 1
fi
