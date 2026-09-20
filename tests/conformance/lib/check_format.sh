#!/usr/bin/env sh
# Shared canonical-formatter check over generated code (ARCHITECTURE §12).
#
# Users run their language's formatter in CHECK mode over their whole tree in
# CI -- `gofmt -l`, `zig fmt --check`, `cargo fmt --check` -- generated files
# included. A generated file that fails it forces them to exclude it, or to
# reformat after every regeneration. So the generated code is held to that same
# check, with the formatter's DEFAULT settings (no config of ours is in reach of
# the generated tree), at the version the lang-<x> CI job pins with its
# toolchain.
#
# Only languages with ONE canonical formatter are held to one. A language without
# (C, C++, Java, Kotlin, C#) would first need a house style chosen, which is a
# separate decision; this driver refuses an unknown language rather than
# passing it.
#
# What the gate checks is a tree of its OWN: `format_gen` below generates it,
# and the suite's other legs -- compile, lint, typecheck, round-trip -- stay on
# the emitters' own bytes, which is what `sofabgen` writes by default and
# therefore what a user actually builds. See format_gen for why.
#
# The formatter itself may be missing: `sofabgen` never requires the target's
# toolchain (--format defaults to off, ARCHITECTURE §12 gate 10), and neither
# does this repository's own test suite -- rustfmt is a separate rustup
# component, and neither ruff nor prettier is part of its language's toolchain
# at all. So `format_gen` generates nothing and this check SKIPS, loudly and
# unmistakably, when the tool is not there. SOFAB_FORMAT_STRICT=1 turns every
# such skip into a failure; the lang-<x> CI jobs install the formatters and set
# it, so what is optional locally is mandatory there.
#
# check_format <lang> <dir>...
#   Runs <lang>'s formatter in check mode over every source file of that
#   language under the given directories -- the whole gate-10 tree, never one
#   file -- and fails listing each offending file with an excerpt of the change
#   the formatter wants. Build output inside a project (zig's
#   .zig-cache/zig-out, cargo's target/, dart's .dart_tool, npm's node_modules
#   and dist) is not generated code and is not looked at, and neither is a
#   `corelib` directory. A directory set holding no file of the language fails
#   too: a check over nothing proves nothing.

# The canonical formatter of <lang>: the binary to look for in $_cf_bin and the
# name to print in $_cf_toolname. An unknown language is refused here rather
# than reported as "not installed" -- a typo must not become a silent skip.
_cf_formatter() {
    case "$1" in
        go)     _cf_bin=gofmt;   _cf_toolname="gofmt" ;;
        zig)    _cf_bin=zig;     _cf_toolname="zig fmt" ;;
        rust)   _cf_bin=rustfmt; _cf_toolname="rustfmt" ;;
        dart)   _cf_bin=dart;    _cf_toolname="dart format" ;;
        python) _cf_bin="${RUFF:-${SOFAB_RUFF:-ruff}}"; _cf_toolname="ruff format" ;;
        typescript) _cf_bin="${PRETTIER:-${SOFAB_PRETTIER:-prettier}}"; _cf_toolname="prettier" ;;
        *)
            echo "FAIL: check_format: no canonical formatter is defined for '$1'"
            exit 1
            ;;
    esac
}

# formatter_present <lang> -- true when this RUN can use that formatter.
formatter_present() {
    _cf_formatter "$1"
    case " $CF_UNAVAILABLE " in *" $1 "*) return 1 ;; esac
    command -v "$_cf_bin" >/dev/null 2>&1
}

# format_unavailable <lang> [why] -- declare that this run cannot use <lang>'s
# formatter although a binary of that name may be on PATH.
#
# The one case today is a version the suite does not pin. ruff and prettier
# change their layout between releases, so a check against another version
# answers another question and must not be reported as a pass -- but refusing to
# RUN would leave a developer who happens to have some ruff or some prettier
# worse off than one who has none, who still gets the whole suite minus the
# formatter gate. So an unpinned formatter is treated exactly like an absent
# one: nothing is generated for gate 10, and the gate skips, loudly, naming the
# version it wanted. SOFAB_FORMAT_STRICT=1 makes it a failure again, and the
# lang-<x> jobs, which install the pin, set it.
CF_UNAVAILABLE=""
CF_UNAVAILABLE_WHY=""
format_unavailable() {
    CF_UNAVAILABLE="$CF_UNAVAILABLE $1"
    if [ -n "${2:-}" ]; then
        CF_UNAVAILABLE_WHY="$2"
    fi
}

# format_mode <lang> -- the --format value gate 10 generates ITS OWN tree with.
#
#   go, zig     off      Their output is formatter-clean as it leaves Generate:
#                        go formats with the go/format LIBRARY, and zig's layout
#                        is emitted (generators/zig/layout.go). The gate proves
#                        the EMITTERS, so it must look at what a user gets at the
#                        DEFAULT switch value, which is off.
#   the rest    require  Their formatter is an external program the CLI runs
#                        only when asked, so what the gate proves is that the
#                        PASS reached every generated file and that its result
#                        is clean. require, not auto: a pass that silently
#                        stopped running must fail the generation, not go
#                        unnoticed.
#
# `off` is named explicitly rather than left out, so a `generic.run_formatter`
# in a config a suite writes or inherits cannot turn the pass on behind its back.
format_mode() {
    case "$1" in
        go | zig) echo "--format=off" ;;
        *) echo "--format=require" ;;
    esac
}

# format_gen <lang> <outdir> <sofabgen args...>
#   Generates ONE tree for gate 10 to check, under <outdir>, with the --format
#   value format_mode gives that language. Uses $ROOT (the repository) and needs
#   nothing else from the caller.
#
# Gate 10 generates its own trees rather than checking the ones a suite builds,
# and the reason is what the switch means. The default is --format=off, so the
# code a user receives from `sofabgen` is the EMITTERS' own bytes; that is what
# every other leg of a suite must compile, lint, typecheck and round-trip, or
# the guarantees would hold for a variant nobody gets. The formatted tree is a
# different artifact -- a convenience the user can ask for -- and it is checked
# here, once, on its own.
#
# Being its own tree is also what makes the check honest: it holds nothing but
# generated files, so a hand-written fixture a suite copies into a generated
# project (stream_check.ts, ownership_check.dart) can never be counted as
# generated code, and no filter is needed to keep it out.
#
# A missing formatter makes this a no-op; check_format then skips, loudly, for
# the same reason (or fails under SOFAB_FORMAT_STRICT=1).
format_gen() {
    _fg_lang=$1
    _fg_out=$2
    shift 2
    formatter_present "$_fg_lang" || return 0
    ( cd "$ROOT" && go run ./cmd/sofabgen "$(format_mode "$_fg_lang")" \
        --lang "$_fg_lang" --out "$_fg_out" "$@" ) >/dev/null
}

# format_gen_corpus <lang> <outroot> <sofabgen args...>
#   format_gen over every corpus definition and every realworld schema, into
#   <outroot>/<name>. The whole corpus, because these are the shapes a target's
#   emitters can produce at all: one schema proves the pass ran, the corpus
#   proves it ran over every construct.
format_gen_corpus() {
    _fc_lang=$1
    _fc_out=$2
    shift 2
    formatter_present "$_fc_lang" || return 0
    for _fc_def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/*.yaml; do
        # $FMT_SKIP_DEFS: definitions THIS config cannot express at all, named by
        # file name -- the rust no_std legs, whose profile refuses an unbounded
        # field, are the only ones today. It is the same list that leg's build
        # loop skips; a definition left out here is one the pass genuinely never
        # sees, not one the gate forgot.
        case " ${FMT_SKIP_DEFS:-} " in *" $(basename "$_fc_def") "*) continue ;; esac
        format_gen "$_fc_lang" "$_fc_out/$(basename "$_fc_def" .yaml)" --in "$_fc_def" "$@"
    done
}

# skip_without_tool <tool> <what it would have checked>
#   An absent tool is a SKIP, and a skip is never printed as a pass: this is a
#   banner no one scrolling a log can mistake for one. SOFAB_FORMAT_STRICT=1
#   turns it into a failure instead -- the lang-<x> CI jobs install the tools
#   and set it, so what is optional on a laptop is mandatory in CI.
skip_without_tool() {
    if [ -n "${SOFAB_FORMAT_STRICT:-}" ]; then
        echo "FAIL: $1 is not available and SOFAB_FORMAT_STRICT is set -- skipped: $2."
        exit 1
    fi
    echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
    echo "!!!! SKIPPED, NOT PASSED: $2"
    echo "!!!! $1 is not available on this box."
    echo "!!!! Install it, or set SOFAB_FORMAT_STRICT=1 to make this a failure"
    echo "!!!! (the lang-<x> CI jobs install it and set SOFAB_FORMAT_STRICT=1)."
    echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
}

check_format() {
    _cf_lang=$1
    shift
    # The formatter this run cannot use is the formatter this cannot check:
    # format_gen above then generated nothing, so there is no tree under $@ at
    # all. Skip loudly instead (or fail, under strict).
    if ! formatter_present "$_cf_lang"; then
        _cf_why=$_cf_toolname
        case " $CF_UNAVAILABLE " in
        *" $_cf_lang "*)
            if [ -n "$CF_UNAVAILABLE_WHY" ]; then
                _cf_why=$CF_UNAVAILABLE_WHY
            fi
            ;;
        esac
        skip_without_tool "$_cf_why" "generated $_cf_lang against $_cf_toolname (ARCHITECTURE §12 gate 10)"
        return 0
    fi
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
        rust)
            _cf_version=$(rustfmt --version)
            # rustfmt parses per edition, so a check under a different one is a
            # check of different code. Take it from the crates themselves rather
            # than hard-coding it here: the generated Cargo.toml is what `cargo
            # fmt` in that tree would use, and two editions in one tree means
            # the answer is ambiguous, not that one of them is right.
            _cf_ed=$(find "$@" -name target -prune -o -name Cargo.toml -type f -print \
                | xargs -r sed -n 's/^edition *= *"\([0-9]*\)".*/\1/p' | sort -u)
            if [ "$(printf '%s\n' "$_cf_ed" | grep -c .)" -ne 1 ]; then
                echo "FAIL: check_format rust: expected exactly one Cargo.toml edition under $*, got: $_cf_ed"
                exit 1
            fi
            _cf_files=$(find "$@" -name target -prune -o -name '*.rs' -type f -print | sort)
            _cf_version="$_cf_version, edition $_cf_ed"
            ;;
        dart)
            _cf_version="dart $(dart --version 2>&1 | sed 's/^Dart SDK version: //;s/ .*//')"
            # `dart format` picks its STYLE from the language version of the
            # package a file belongs to (short below 3.7, tall from 3.7 on), so
            # the version decides what "clean" means. Take it from the generated
            # pubspec rather than hard-coding it here -- that is the constraint
            # the user's own `dart format` in that package resolves -- and refuse
            # a tree that declares more than one, which would make the answer
            # ambiguous rather than make one of them right. Passing it also keeps
            # the check independent of whether `dart pub get` has written a
            # package config yet.
            _cf_lv=$(find "$@" \( -name .dart_tool -o -name corelib \) -prune -o \
                -name pubspec.yaml -type f -print \
                | xargs -r sed -n 's/^ *sdk: *[^0-9]*\([0-9]*\.[0-9]*\)\..*/\1/p' | sort -u)
            if [ "$(printf '%s\n' "$_cf_lv" | grep -c .)" -ne 1 ]; then
                echo "FAIL: check_format dart: expected exactly one pubspec sdk constraint under $*, got: $_cf_lv"
                exit 1
            fi
            _cf_files=$(find "$@" \( -name .dart_tool -o -name build -o -name corelib \) -prune -o \
                -name '*.dart' -type f -print | sort)
            _cf_version="$_cf_version, language version $_cf_lv"
            ;;
        python)
            # The suite pins the ruff version and refuses to start under any
            # other one (tests/conformance/python/run.sh), because ruff's
            # formatting changes between releases; $RUFF is that binary.
            _cf_ruff="${RUFF:-${SOFAB_RUFF:-ruff}}"
            _cf_version=$("$_cf_ruff" --version)
            _cf_files=$(find "$@" -name corelib -prune -o -name '*.py' -type f -print | sort)
            ;;
        typescript)
            _cf_prettier="${PRETTIER:-${SOFAB_PRETTIER:-prettier}}"
            _cf_version="prettier $("$_cf_prettier" --version)"
            # .ts only, the way every other arm checks that language's SOURCE.
            # prettier also owns the generated package.json, tsconfig.json and
            # README.md, but those are fixed emitter constants rather than
            # schema-derived output, so a corpus-wide sweep would say the same
            # thing 27 times over; generators/typescript/format_test.go holds
            # them instead, and holds them at --format=off, where no formatter
            # runs. node_modules is a dependency tree, not generated code, and
            # dist/ is build output; neither exists in the gate-10 tree, and
            # both are pruned so that a directory passed by hand behaves too.
            _cf_files=$(find "$@" \( -name node_modules -o -name dist -o -name corelib \) -prune -o \
                -name '*.ts' -type f -print | sort)
            ;;
        # No default arm: _cf_formatter above already refused a language with
        # no canonical formatter, before anything was looked up.
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
        # rustfmt --check prints the change it wants, file by file, so its own
        # output IS the excerpt; there is nothing to re-derive below.
        rust) _cf_out=$(printf '%s\n' "$_cf_files" | xargs rustfmt --check --edition "$_cf_ed" 2>&1) || _cf_rc=$? ;;
        dart) _cf_out=$(printf '%s\n' "$_cf_files" | xargs dart format --output=none --summary=none \
            --language-version="$_cf_lv" --set-exit-if-changed 2>&1) || _cf_rc=$? ;;
        # No --isolated: sofabgen formats the generated tree with the user's own
        # ruff settings (generators/python/format.go), so a verdict that ignored
        # them would be a verdict about a tree nobody receives. ruff resolves
        # its config per file, so both sides resolve the same one. --quiet drops
        # the "N files already formatted" summary a CLEAN run prints, and only
        # that: the per-file diff of a dirty one still comes through.
        python) _cf_out=$(printf '%s\n' "$_cf_files" | xargs "$_cf_ruff" format --no-cache --quiet --check 2>&1) || _cf_rc=$? ;;
        typescript)
            # --ignore-path /dev/null, because an IGNORED file passes prettier
            # silently and would make this gate a pass over nothing. Prettier's
            # default ignore path list is [.gitignore, .prettierignore] resolved
            # from the CURRENT DIRECTORY, which here is this repository, not the
            # generated tree: its .gitignore holds `.claude*`, so a work dir
            # under a path with that name in it would be skipped without a word.
            # The generated tree carries no ignore file of its own, so answering
            # "none" is the whole truth about it.
            #
            # Then ask prettier what it makes of one of the files anyway: not
            # ignored, and parsed AS TypeScript, or this is not a verdict at all.
            _cf_one=$(printf '%s\n' "$_cf_files" | head -1)
            _cf_info=$("$_cf_prettier" --no-color --ignore-path /dev/null --file-info "$_cf_one" 2>&1)
            case "$_cf_info" in
                *'"ignored": false'*'"inferredParser": "typescript"'*) ;;
                *)
                    echo "FAIL: check_format typescript: prettier would not have checked $_cf_one -- $_cf_info"
                    exit 1
                    ;;
            esac
            # --list-different, not --check: it is silent on success and prints
            # one path per differing file, which is what the excerpt loop below
            # reads. --check would print its "All matched files ..." banner on a
            # clean run, which this driver reads as output, i.e. as a failure.
            # No --no-config: sofabgen formats the generated tree with the user's
            # own prettier settings (generators/typescript/format.go), so a
            # verdict that ignored them would be a verdict about a tree nobody
            # receives. In this suite's work dir there is no config to find, so
            # what it actually checks against is prettier's defaults.
            _cf_out=$(printf '%s\n' "$_cf_files" | xargs "$_cf_prettier" --no-color \
                --ignore-path /dev/null -l 2>&1) || _cf_rc=$?
            ;;
    esac
    if [ "$_cf_rc" -eq 0 ] && [ -z "$_cf_out" ]; then
        echo "==> $_cf_lang: $_cf_n generated files are formatter-clean ($_cf_version)"
        return 0
    fi

    echo "FAIL: generated $_cf_lang is not formatter-clean ($_cf_version, exit $_cf_rc):"
    # A whole-corpus diff runs to tens of thousands of lines; the first 200 say
    # what is wrong, and the command above reproduces the rest.
    printf '%s\n' "$_cf_out" | sed 's/^/  /' | head -200
    if [ "$(printf '%s\n' "$_cf_out" | wc -l)" -gt 200 ]; then
        echo "  ... (truncated; rerun the formatter over the generated tree for the rest)"
    fi
    # The files to show a diff for. rustfmt --check and `ruff format --check`
    # already print the change they want, so their own output above IS the
    # excerpt and there is nothing to re-derive for them; gofmt -l, zig fmt
    # --check and `dart format --set-exit-if-changed` only name files.
    case "$_cf_lang" in
        go | zig) _cf_bad=$(printf '%s\n' "$_cf_out" | grep -E '\.(go|zig|zon)$' || true) ;;
        dart) _cf_bad=$(printf '%s\n' "$_cf_out" | sed -n 's/^Changed //p') ;;
        typescript) _cf_bad=$(printf '%s\n' "$_cf_out" | grep -E '^[^[].*\.ts$' || true) ;;
        *) _cf_bad="" ;;
    esac
    _cf_tmp=$(mktemp -d)
    _cf_shown=0
    for _cf_f in $_cf_bad; do
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
            dart)
                dart format --output=show --summary=none --language-version="$_cf_lv" \
                    "$_cf_f" 2>/dev/null > "$_cf_tmp/want" || true
                diff -u "$_cf_f" "$_cf_tmp/want" | head -40 || true
                ;;
            typescript)
                "$_cf_prettier" --no-color --ignore-path /dev/null "$_cf_f" \
                    2>/dev/null > "$_cf_tmp/want" || true
                diff -u "$_cf_f" "$_cf_tmp/want" | head -40 || true
                ;;
        esac
    done
    rm -rf "$_cf_tmp"
    exit 1
}
