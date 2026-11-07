#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# The live check after a deploy: it asks the API from outside, through
# whatever routes requests to lectiod, and proves the release that answers.
# An operator runs it by hand after an upgrade. The release workflow runs it
# after the deploy job's rollout, where that job runs at all (the repository
# variable LECTIO_RELEASE_DEPLOY is set): with the cluster credential
# already dropped, again every few seconds until it passes or the stage's
# time runs out, and the release notes carry its Markdown evidence.
#
# Unauthenticated, read-only, and safe to run any number of times. It does
# not wait: it fails at once on the first failing check and leaves the retry
# to its caller.
#
# BASE_URL is the API origin plus LECTIO_BASE_PATH, such as
# https://parse.example.com/v1, and for production https://api.latere.ai/v1/parsing.
# It has no default.
#
# Checks, in order:
#   1. GET $BASE_URL/openapi.yaml answers 200: the route reaches lectiod.
#   2. GET $BASE_URL/readers answers 401: the API's own refusal of a request
#      with no bearer, which proves the request reached lectiod's routes.
#      /healthz is not checked: it is served outside the base path, which an
#      ingress that routes only the base path, as the production one does,
#      never reaches.
#   3. The Lectio-Version header of the answer equals TAG, when TAG is set.
#      Every answer names the release that wrote it in that header, which is
#      where the version is readable from outside: lectiod's /version is
#      served on its internal listener (LECTIO_INTERNAL_ADDR), which no
#      ingress routes.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL    the API origin and prefix to check; required
#   TAG         the release being proved; when set, the served version must equal it
#   COMMIT      the release commit, for the evidence
#   DEPLOY_URL  the run that deployed it, for the evidence
#   OUTPUT_MD   where to write the Markdown evidence; nowhere when unset

set -euo pipefail

BASE_URL="${BASE_URL:-}"
BASE_URL="${BASE_URL%/}"
TAG="${TAG:-}"
COMMIT="${COMMIT:-unknown}"
DEPLOY_URL="${DEPLOY_URL:-}"
OUTPUT_MD="${OUTPUT_MD:-}"

pass() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -n "$BASE_URL" ] || fail "BASE_URL is required; this smoke has no default target"
for cmd in curl grep; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# fetch LABEL PATH WANT: one request, no retry; the status must be WANT.
fetch() {
  local label="$1" path="$2" want="$3" code
  code=$(curl -sS -D "$tmp/$label.head" -o "$tmp/$label.body" -w '%{http_code}' \
    "${BASE_URL}${path}") || fail "GET $path: curl failed"
  [ "$code" = "$want" ] || fail "GET $path: expected $want, got $code (body: $(head -c 200 "$tmp/$label.body" 2>/dev/null || true))"
  pass "GET $path ($code)"
}

fetch spec /openapi.yaml 200
fetch readers /readers 401

served=$(grep -i '^lectio-version:' "$tmp/spec.head" | head -1 | cut -d: -f2- | tr -d '[:space:]' || true)
[ -n "$served" ] || fail "GET /openapi.yaml: no Lectio-Version header in the answer"
if [ -n "$TAG" ]; then
  [ "$served" = "$TAG" ] || fail "version mismatch: served $served, expected $TAG"
  pass "served version matches the tag ($served)"
else
  pass "served version recorded ($served)"
fi

if [ -n "$OUTPUT_MD" ]; then
  {
    echo "<!-- release-evidence -->"
    echo
    echo "## Release evidence"
    echo
    echo "- Tag: \`${TAG:-unknown}\`"
    echo "- Commit: \`${COMMIT}\`"
    [ -n "$DEPLOY_URL" ] && echo "- Deploy: ${DEPLOY_URL}"
    echo "- Origin: ${BASE_URL}"
    echo "- Served version: \`${served}\` (the Lectio-Version header)"
    echo "- Smoke: \`/openapi.yaml\` answered 200, \`/readers\` answered 401 with no bearer"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
