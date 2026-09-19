#!/usr/bin/env sh
# Shared canonical-formatter check over generated code (ARCHITECTURE §12).
#
# Users run their language's formatter in CHECK mode over their whole tree in
# CI -- `gofmt -l`, `zig fmt --check` -- generated files included. A generated
# file that fails it forces them to exclude it, or to reformat after every
# regeneration. So the generated code is held to that same check, with the
# formatter's DEFAULT settings (no config of ours is in reach of the generated
# tree), at the version the lang-<x> CI job pins with its toolchain.
#
# Only languages with ONE canonical formatter are held to one. A language without
# (C, C++, Java, Kotlin, C#) would first need a house style chosen, which is a
# separate decision; this driver refuses an unknown language rather than
# passing it.
#
# check_format <lang> <dir>...
#   Runs <lang>'s formatter in check mode over every source file of that
#   language under the given directories -- the generated example project AND
#   every generated corpus project, never one file -- and fails listing each
#   offending file with an excerpt of the change the formatter wants. Build
#   output inside a project (zig's .zig-cache/zig-out) is not generated code and
#   is not looked at. A directory set holding no file of the language fails too:
#   a check over nothing proves nothing.
check_format() {
    _cf_lang=$1
    shift
    case "$_cf_lang" in
        go)
            _cf_version=$(go version)
            _cf_files=$(find "$@" -name '*.go' -type f | sort)
            ;;
        zig)
            _cf_version="zig $(zig version)"
            _cf_files=$(find "$@" \( -name .zig-cache -o -name zig-out \) -prune -o \
                \( -name '*.zig' -o -name '*.zon' \) -type f -print | sort)
            ;;
        *)
            echo "FAIL: check_format: no canonical formatter is defined for '$_cf_lang'"
            exit 1
            ;;
    esac
    _cf_n=$(printf '%s\n' "$_cf_files" | grep -c . || true)
    if [ "$_cf_n" -eq 0 ]; then
        echo "FAIL: check_format $_cf_lang: no $_cf_lang source under $*"
        exit 1
    fi

    # The formatter's own verdict, stdout and stderr together: a file it cannot
    # parse is a failure as much as a file it would reformat.
    _cf_rc=0
    case "$_cf_lang" in
        go) _cf_out=$(printf '%s\n' "$_cf_files" | xargs gofmt -l 2>&1) || _cf_rc=$? ;;
        zig) _cf_out=$(printf '%s\n' "$_cf_files" | xargs zig fmt --check 2>&1) || _cf_rc=$? ;;
    esac
    if [ "$_cf_rc" -eq 0 ] && [ -z "$_cf_out" ]; then
        echo "==> $_cf_lang: $_cf_n generated files are formatter-clean ($_cf_version)"
        return 0
    fi

    echo "FAIL: generated $_cf_lang is not formatter-clean ($_cf_version, exit $_cf_rc):"
    printf '%s\n' "$_cf_out" | sed 's/^/  /'
    _cf_tmp=$(mktemp -d)
    _cf_shown=0
    for _cf_f in $(printf '%s\n' "$_cf_out" | grep -E '\.(go|zig|zon)$' || true); do
        [ -f "$_cf_f" ] || continue
        [ "$_cf_shown" -lt 5 ] || break
        _cf_shown=$((_cf_shown + 1))
        echo "--- what the formatter wants in $_cf_f (excerpt):"
        case "$_cf_lang" in
            go) gofmt -d "$_cf_f" 2>&1 | head -40 ;;
            zig)
                cp "$_cf_f" "$_cf_tmp/$(basename "$_cf_f")"
                zig fmt "$_cf_tmp/$(basename "$_cf_f")" >/dev/null 2>&1 || true
                diff -u "$_cf_f" "$_cf_tmp/$(basename "$_cf_f")" | head -40 || true
                ;;
        esac
    done
    rm -rf "$_cf_tmp"
    exit 1
}
