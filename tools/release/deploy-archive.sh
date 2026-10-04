#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Writes the deploy archive of a release: the deploy/ tree of this checkout
# with both images pinned to the release by digest, so an installation from
# the archive edits nothing to run the version it downloaded.
#
#	REGISTRY=ghcr.io OWNER=<owner> \
#	LECTIOD_DIGEST=sha256:... CONVERT_DIGEST=sha256:... \
#	tools/release/deploy-archive.sh v0.1.0 [DIST]
#
# The server's image is pinned in the base, which every overlay builds on.
# The sidecar's is pinned in its component, since the component's objects
# join an overlay beside the base and a pin in the base would not reach
# them. Each pin is an `images` entry appended to the kustomization, which
# is why it edits the files of the checkout it runs in: run it in a
# checkout made for the release.
#
# Every overlay is then rendered, and the archive is written only when each
# names the published images and no placeholder is left. kubectl renders
# files here and reaches no cluster.
set -euo pipefail

tag=${1:?usage: deploy-archive.sh TAG [DIST]}
dist=${2:-dist}
: "${REGISTRY:?the registry the images were pushed to}"
: "${OWNER:?the namespace the images were pushed under}"
: "${LECTIOD_DIGEST:?the digest of the lectiod image}"
: "${CONVERT_DIGEST:?the digest of the lectio-convert image}"

# pin appends the images entry that points a placeholder at its digest.
pin() {
  printf 'images:\n  - name: %s\n    newName: %s/%s/%s\n    digest: %s\n' \
    "$2" "$REGISTRY" "$OWNER" "$2" "$3" >> "$1"
}
pin deploy/base/kustomization.yaml lectiod "$LECTIOD_DIGEST"
pin deploy/components/converter/kustomization.yaml lectio-convert "$CONVERT_DIGEST"

server="image: $REGISTRY/$OWNER/lectiod@$LECTIOD_DIGEST"
sidecar="image: $REGISTRY/$OWNER/lectio-convert@$CONVERT_DIGEST"
rendered=$(mktemp)
trap 'rm -f "$rendered"' EXIT
for overlay in deploy/base deploy/examples/generic deploy/examples/with-converter; do
  kubectl kustomize "$overlay" > "$rendered"
  grep -qF "$server" "$rendered" \
    || { echo "$overlay does not render the published lectiod image" >&2; exit 1; }
  # A placeholder that survived is an image no node can pull.
  if grep -qE '^ +image: (lectiod|lectio-convert)$' "$rendered"; then
    echo "$overlay still renders an image placeholder" >&2; exit 1
  fi
done
grep -qF "$sidecar" "$rendered" \
  || { echo "deploy/examples/with-converter does not render the published lectio-convert image" >&2; exit 1; }

mkdir -p "$dist"
tar -czf "$dist/deploy-$tag.tar.gz" deploy
echo "deploy-archive: $dist/deploy-$tag.tar.gz"
