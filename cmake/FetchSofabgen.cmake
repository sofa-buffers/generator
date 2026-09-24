# FetchSofabgen.cmake — get a working `sofabgen` without a separate install
# step, the same way install.sh does it: a prebuilt binary from this repo's
# GitHub releases, checksum-verified.
#
# Most consumers want SofabGenerate.cmake instead (same directory): it
# include()s this file for you and adds sofab_generate(), a function that
# wires sofabgen straight into a CMake target. Use this file directly only
# if you want the resolved SOFABGEN_EXECUTABLE and nothing else — e.g. to
# drive it yourself outside of add_custom_command. Both files are meant to
# be reached together via one FetchContent of this repo (see
# SofabGenerate.cmake's own header for the exact snippet); this repo is a
# CMake-less Go project, so FetchContent populates the source without also
# trying to add_subdirectory() it.
#
# Resolution order once included:
#   1. A `sofabgen` already on PATH (or -DSOFABGEN_EXECUTABLE=... by hand) —
#      if you already have it installed, nothing here runs at all.
#   2. Otherwise, FetchContent pulls the matching prebuilt binary for the
#      host OS/arch straight from the release this repo's own release.yml
#      publishes, and this file verifies it against the published .sha256
#      before anything runs it — same trust model as install.sh, just
#      expressed as CMake instead of shell.
#
# Override the version with -DSOFABGEN_VERSION=v0.24.0 (default: "latest",
# resolved via GitHub's /releases/latest/download/ redirect — see
# install.sh's own SOFABGEN_VERSION for the exact same convention).

find_program(SOFABGEN_EXECUTABLE sofabgen)

if(SOFABGEN_EXECUTABLE)
    message(STATUS "sofabgen: using local install at ${SOFABGEN_EXECUTABLE}")
else()
    set(SOFABGEN_VERSION "latest" CACHE STRING
        "sofabgen release tag to fetch (e.g. v0.24.0), or \"latest\"")

    # --- map the host to a release asset name, exactly as install.sh does ---
    if(CMAKE_HOST_SYSTEM_NAME STREQUAL "Linux")
        set(_sofabgen_os "linux")
    elseif(CMAKE_HOST_SYSTEM_NAME STREQUAL "Darwin")
        set(_sofabgen_os "darwin")
    elseif(CMAKE_HOST_SYSTEM_NAME STREQUAL "Windows")
        set(_sofabgen_os "windows")
    else()
        message(FATAL_ERROR
            "sofabgen: no prebuilt binary for host OS '${CMAKE_HOST_SYSTEM_NAME}' — "
            "install sofabgen yourself (see this repo's README) and re-run "
            "cmake, or set -DSOFABGEN_EXECUTABLE=/path/to/sofabgen.")
    endif()

    set(_sofabgen_arch_raw "${CMAKE_HOST_SYSTEM_PROCESSOR}")
    if(_sofabgen_arch_raw MATCHES "^(x86_64|AMD64|amd64)$")
        set(_sofabgen_arch "amd64")
    elseif(_sofabgen_arch_raw MATCHES "^(aarch64|arm64|ARM64)$")
        set(_sofabgen_arch "arm64")
    elseif(_sofabgen_arch_raw MATCHES "^(i386|i686|x86)$")
        set(_sofabgen_arch "386")
    elseif(_sofabgen_arch_raw MATCHES "^(armv7l|armv6l|arm)$")
        set(_sofabgen_arch "arm")
    else()
        message(FATAL_ERROR
            "sofabgen: no prebuilt binary for host arch '${_sofabgen_arch_raw}' — "
            "install sofabgen yourself and re-run cmake, or set "
            "-DSOFABGEN_EXECUTABLE=/path/to/sofabgen.")
    endif()

    set(_sofabgen_ext "")
    if(_sofabgen_os STREQUAL "windows")
        set(_sofabgen_ext ".exe")
    endif()

    set(_sofabgen_asset "sofabgen-${_sofabgen_os}-${_sofabgen_arch}${_sofabgen_ext}")

    if(SOFABGEN_VERSION STREQUAL "latest")
        set(_sofabgen_base "https://github.com/sofa-buffers/generator/releases/latest/download")
    else()
        set(_sofabgen_base "https://github.com/sofa-buffers/generator/releases/download/${SOFABGEN_VERSION}")
    endif()

    message(STATUS "sofabgen: no local install found, fetching ${_sofabgen_asset} (${SOFABGEN_VERSION}) via FetchContent")

    include(FetchContent)
    FetchContent_Declare(sofabgen_binary
        URL "${_sofabgen_base}/${_sofabgen_asset}"
        DOWNLOAD_NO_EXTRACT TRUE
        DOWNLOAD_NAME "${_sofabgen_asset}"
    )
    FetchContent_MakeAvailable(sofabgen_binary)

    set(_sofabgen_downloaded "${sofabgen_binary_SOURCE_DIR}/${_sofabgen_asset}")

    # --- verify against the published checksum, same as install.sh -----------
    # The hash itself is NOT hardcoded here (it changes every release): it is
    # downloaded alongside the binary and compared, not baked into this file.
    if(NOT EXISTS "${_sofabgen_downloaded}.sha256")
        file(DOWNLOAD "${_sofabgen_base}/${_sofabgen_asset}.sha256" "${_sofabgen_downloaded}.sha256")
    endif()
    file(STRINGS "${_sofabgen_downloaded}.sha256" _sofabgen_sha256_line LIMIT_COUNT 1)
    separate_arguments(_sofabgen_sha256_parts UNIX_COMMAND "${_sofabgen_sha256_line}")
    list(GET _sofabgen_sha256_parts 0 _sofabgen_expected_sha256)
    file(SHA256 "${_sofabgen_downloaded}" _sofabgen_actual_sha256)
    if(NOT _sofabgen_actual_sha256 STREQUAL _sofabgen_expected_sha256)
        message(FATAL_ERROR
            "sofabgen: checksum mismatch for ${_sofabgen_asset}\n"
            "  expected: ${_sofabgen_expected_sha256}\n"
            "  actual:   ${_sofabgen_actual_sha256}")
    endif()

    if(NOT _sofabgen_os STREQUAL "windows")
        file(CHMOD "${_sofabgen_downloaded}"
            PERMISSIONS OWNER_READ OWNER_WRITE OWNER_EXECUTE
                        GROUP_READ GROUP_EXECUTE
                        WORLD_READ WORLD_EXECUTE)
    endif()

    set(SOFABGEN_EXECUTABLE "${_sofabgen_downloaded}" CACHE FILEPATH "Path to the sofabgen binary" FORCE)
    message(STATUS "sofabgen: fetched and checksum-verified at ${SOFABGEN_EXECUTABLE}")
endif()
