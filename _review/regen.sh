#!/bin/sh
# TEMPORARY (see README.md) — regenerate the review outputs from example.yaml.
# Run from the repository root: _review/regen.sh
set -e
cd "$(dirname "$0")/.."
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

gen() { # gen <name> <lang> <schema> <config-yaml>
    rm -rf "$TMP/in"
    mkdir -p "$TMP/in"
    cp "_review/$3" "$TMP/in/"
    printf '%s' "$4" > "$TMP/config.yaml"
    mkdir -p "_review/generated/$1"
    go run ./cmd/sofabgen -config "$TMP/config.yaml" -lang "$2" \
        -in "$TMP/in" -out "_review/generated/$1"
    echo "==> _review/generated/$1"
}

# maxspeed C++ (corelib-cpp)
gen cpp cpp example.yaml 'targets:
  cpp:
    namespace: sofabuffers
'

# footprint C++ (corelib-c-cpp) with heap storage: the same bounded schema,
# std::string / std::vector instead of inline containers
gen c-cpp-dynamic cpp example-bounded.yaml 'targets:
  cpp:
    namespace: sofabuffers
    corelib: c-cpp
    allow_dynamic: true
'

# footprint C++ (corelib-c-cpp), default: every field inline, no allocation
gen c-cpp-static cpp example-bounded.yaml 'targets:
  cpp:
    namespace: sofabuffers
    corelib: c-cpp
    allow_dynamic: false
'

# the C target on the same corelib
gen c c example-bounded.yaml 'targets:
  c:
    symbol_prefix: sofab
'
