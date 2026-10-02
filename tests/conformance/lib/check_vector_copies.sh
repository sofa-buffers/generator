#!/usr/bin/env bash
# Every corelib carries its own copy of assets/test_vectors.json, and every
# conformance suite reads the copy of the corelib it tests. The canonical file is
# corelib-c-cpp's (CORELIB_PLAN); a copy that has drifted makes a suite check
# different vectors than the others without any run failing. This compares the
# git blob SHA of main's copy in each corelib against the canonical one and
# fails, naming each corelib that differs.
#
#   check_vector_copies.sh                       # GitHub contents API, branch main
#   check_vector_copies.sh corelib-go=/path/to/checkout ...
#
# A name=path argument hashes that checkout's file instead of asking the API
# (a local checkout, or a fork under test). The API being unreachable is a
# failure: a check that skips itself says nothing.
set -euo pipefail

ORG=sofa-buffers
CANON=corelib-c-cpp
REPOS="corelib-c-cpp corelib-cpp corelib-cs corelib-dart corelib-go corelib-java corelib-kotlin-mp corelib-py corelib-rs corelib-rs-no-std corelib-ts corelib-zig"
FILE=assets/test_vectors.json

declare -A LOCAL=()
for a in "$@"; do
    case "$a" in
        corelib-*=*) LOCAL["${a%%=*}"]="${a#*=}" ;;
        *) echo "FAIL: unknown argument '$a' (expected corelib-<x>=<dir>)"; exit 2 ;;
    esac
done

# blob_sha <repo>: the blob SHA of the repo's copy, or a hard failure.
blob_sha() {
    local repo=$1 dir sha
    if [ -n "${LOCAL[$repo]:-}" ]; then
        dir=${LOCAL[$repo]}
        [ -f "$dir/$FILE" ] || { echo "FAIL: $repo: no $dir/$FILE" >&2; return 1; }
        git hash-object "$dir/$FILE"
        return
    fi
    command -v gh >/dev/null || { echo "FAIL: $repo: gh is not installed, cannot query the GitHub API" >&2; return 1; }
    sha=$(gh api "repos/$ORG/$repo/contents/$FILE?ref=main" --jq .sha 2>&1) \
        || { echo "FAIL: $repo: GitHub API request failed: $sha" >&2; return 1; }
    printf '%s' "$sha" | grep -Eq '^[0-9a-f]{40}$' \
        || { echo "FAIL: $repo: API did not return a blob SHA: $sha" >&2; return 1; }
    echo "$sha"
}

canon=$(blob_sha "$CANON") || exit 1
status=0
for repo in $REPOS; do
    sha=$(blob_sha "$repo") || exit 1
    if [ "$sha" = "$canon" ]; then
        printf '%-20s %s\n' "$repo" "$sha"
    else
        printf '%-20s %s  DIFFERS from %s (%s)\n' "$repo" "$sha" "$CANON" "$canon"
        echo "FAIL: $repo: $FILE is not the canonical copy" >&2
        status=1
    fi
done
[ "$status" -eq 0 ] && echo "==> all $(echo $REPOS | wc -w) copies of $FILE are identical ($canon)"
exit "$status"
