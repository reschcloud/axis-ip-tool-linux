#!/bin/sh
# Fill the commit placeholder in the Flathub manifest after tagging a release.
# Usage: packaging/flathub/prepare.sh v0.5.0
set -eu
tag="${1:-v0.5.0}"
manifest="packaging/flathub/io.github.reschcloud.iptoolforaxis.yml"
commit=$(git rev-parse "$tag^{commit}")
sed -i "s/REPLACE_WITH_TAG_COMMIT/$commit/" "$manifest"
printf 'Pinned %s to %s\n' "$manifest" "$commit"
