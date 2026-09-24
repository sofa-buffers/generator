# SofabGenerate.cmake — run sofabgen at build time and wire its output
# straight into a CMake target.
#
# The whole point of fetching this file is that a consumer's CMakeLists.txt
# never hand-writes an add_custom_command() for sofabgen: one function call
# does it, and does it the same way every time (right DEPENDS, right include
# directory, right VERBATIM handling).
#
# Reach it with ONE FetchContent of this repo — this repo has no root
# CMakeLists.txt (it's a Go project), so FetchContent_MakeAvailable just
# populates the source tree without trying to add_subdirectory() it, which
# is exactly what lets this work as a "grab some files" fetch rather than a
# full subproject build:
#
#   include(FetchContent)
#   set(SOFABUFFERS_GENERATOR_REF "main" CACHE STRING
#       "generator ref to fetch cmake/ from")
#   FetchContent_Declare(sofabuffers_generator
#       GIT_REPOSITORY https://github.com/sofa-buffers/generator.git
#       GIT_TAG        ${SOFABUFFERS_GENERATOR_REF}
#       GIT_SHALLOW    TRUE
#   )
#   FetchContent_MakeAvailable(sofabuffers_generator)
#   include(${sofabuffers_generator_SOURCE_DIR}/cmake/SofabGenerate.cmake)
#
# That one include() also resolves SOFABGEN_EXECUTABLE for you (see
# FetchSofabgen.cmake, next to this file): a `sofabgen` already on PATH is
# used as-is, otherwise the matching release binary is fetched and
# checksum-verified. SOFABUFFERS_GENERATOR_REF (which version of *this
# tooling*) and SOFABGEN_VERSION (which version of the *binary*
# FetchSofabgen.cmake fetches) are independent knobs on purpose — pin one
# without the other.
#
# ---------------------------------------------------------------------------
#
#   sofab_generate(<target>
#       LANG <c|cpp|go|python|typescript|rust|csharp|java|kotlin|zig|dart>
#       IN <schema-file>
#       OUT <output-dir>
#       OUTPUTS <file> [<file>...]
#       [CONFIG <sofabgen-config.yaml>]
#   )
#
# <target> must already exist (add_executable/add_library) — this only adds
# to it: every path in OUTPUTS (resolved under OUT) becomes a generated
# source on the target via target_sources(), and OUT is added to its
# include directories, so `#include "message.h"`-style generated headers
# resolve without the caller repeating the path.
#
# OUTPUTS is required and not discovered automatically: add_custom_command()
# needs every OUTPUT named up front for correct incremental-build tracking,
# and what sofabgen writes for a given schema depends on the language (the
# message name for most targets, a fixed `message.py`-style name for some —
# see docs/generator/<lang>.md in this repo) in a way this function has no
# reason to hardcode per language. If you don't already know the names, run
# sofabgen by hand once:
#
#   sofabgen -lang <lang> -in <schema> -out <dir>
#
# and use what it reports.
include_guard(GLOBAL)

# Resolve SOFABGEN_EXECUTABLE once, when this file is included — not on
# every sofab_generate() call — so calling it for several targets in one
# project doesn't re-run find_program()/FetchContent for the binary each
# time.
include("${CMAKE_CURRENT_LIST_DIR}/FetchSofabgen.cmake")

function(sofab_generate TARGET)
    cmake_parse_arguments(SOFAB "" "LANG;IN;OUT;CONFIG" "OUTPUTS" ${ARGN})

    if(NOT TARGET ${TARGET})
        message(FATAL_ERROR "sofab_generate: '${TARGET}' is not a target — "
            "create it with add_executable()/add_library() first.")
    endif()
    if(NOT SOFAB_LANG)
        message(FATAL_ERROR "sofab_generate(${TARGET}): LANG is required")
    endif()
    if(NOT SOFAB_IN)
        message(FATAL_ERROR "sofab_generate(${TARGET}): IN is required")
    endif()
    if(NOT SOFAB_OUT)
        message(FATAL_ERROR "sofab_generate(${TARGET}): OUT is required")
    endif()
    if(NOT SOFAB_OUTPUTS)
        message(FATAL_ERROR "sofab_generate(${TARGET}): OUTPUTS is required — "
            "list every file sofabgen writes for this schema/language; run it "
            "by hand once if unsure (see this file's header).")
    endif()

    file(MAKE_DIRECTORY "${SOFAB_OUT}")

    list(TRANSFORM SOFAB_OUTPUTS PREPEND "${SOFAB_OUT}/"
        OUTPUT_VARIABLE _sofab_full_outputs)

    set(_sofab_cmd "${SOFABGEN_EXECUTABLE}" -lang "${SOFAB_LANG}" -in "${SOFAB_IN}" -out "${SOFAB_OUT}")
    set(_sofab_depends "${SOFAB_IN}")
    if(SOFAB_CONFIG)
        list(APPEND _sofab_cmd -config "${SOFAB_CONFIG}")
        list(APPEND _sofab_depends "${SOFAB_CONFIG}")
    endif()

    add_custom_command(
        OUTPUT ${_sofab_full_outputs}
        COMMAND ${_sofab_cmd}
        DEPENDS ${_sofab_depends}
        COMMENT "sofabgen: generating ${SOFAB_LANG} code from ${SOFAB_IN}"
        VERBATIM
    )

    target_sources(${TARGET} PRIVATE ${_sofab_full_outputs})
    target_include_directories(${TARGET} PRIVATE "${SOFAB_OUT}")
endfunction()
