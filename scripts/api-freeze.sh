#!/usr/bin/env bash
# Regenerate the frozen public API baseline at api/v2.api.
#
# Use this only after an intentional API change, then commit the updated
# baseline together with the code. CI runs scripts/api-check.sh against it.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

apidiff_version=${APIDIFF_VERSION:-v0.0.0-20260908205506-85c1c2202aba}
apidiff() { go run "golang.org/x/exp/cmd/apidiff@${apidiff_version}" "$@"; }

cd "$root"

mkdir -p api
apidiff -m -w api/v2.api github.com/sjlit/diago/v2
echo "Wrote $root/api/v2.api"
