# FetchSofabgen.cmake — resolve a working `sofabgen`, the same way
# install.sh does it: a prebuilt binary from this repo's GitHub releases,
# checksum-verified, retried against a flaky network, never left half-written.
#
# Most consumers want SofabGenerate.cmake instead (same directory): it
# include()s this file and adds sofab_generate(), a function that wires
# sofabgen straight into a CMake target. Use this file directly only if you
# want the resolved SOFABGEN_EXECUTABLE and nothing else.
#
# --- version -----------------------------------------------------------------
#
# SOFABGEN_VERSION names the release to fetch, e.g. "v0.24.0", or "latest"
# (resolved via GitHub's /releases/latest/download/ redirect).
#
# Its default is a placeholder, @SOFABGEN_VERSION_DEFAULT@: the release
# pipeline substitutes it with the exact tag when it packages cmake/ as the
# sofabgen-cmake.tar.gz release asset (see .github/workflows/release.yml), so
# a consumer who fetches THAT asset gets the matching binary version for
# free, with nothing to pin by hand. A copy taken straight from this repo's
# source tree still carries the unsubstituted placeholder — there is no
# release to match — so it falls back to "latest" instead of failing; unlike
# a version mismatch, "latest" has always been a safe, documented default
# here. Override either way with -DSOFABGEN_VERSION=v0.24.0.
if(NOT DEFINED SOFABGEN_VERSION)
  set(_sofabgen_version_default "@SOFABGEN_VERSION_DEFAULT@")
  if(_sofabgen_version_default MATCHES "^@.*@$")
    set(_sofabgen_version_default "latest")
  endif()
  set(SOFABGEN_VERSION "${_sofabgen_version_default}" CACHE STRING
      "sofabgen release tag to fetch (e.g. v0.24.0), or \"latest\"")
  unset(_sofabgen_version_default)
endif()

set(SOFABGEN_EXECUTABLE "sofabgen" CACHE FILEPATH "Path to the sofabgen executable")
set(SOFABGEN_FETCH_REPOSITORY "sofa-buffers/generator" CACHE STRING
    "Repository the release is fetched from")

# --- TLS -----------------------------------------------------------------
# The peer is always checked by default. Behind a TLS-intercepting proxy that
# check fails against the proxy's certificate rather than GitHub's; naming the
# CA that signed it is the fix, not turning verification off — SHA256SUMS-
# style verification arrives over the same connection as the binary it
# vouches for, so an unverified connection proves the two merely agree with
# each other, not who sent them.
set(SOFABGEN_FETCH_TLS_CAINFO "" CACHE FILEPATH
    "CA bundle used to verify the download, e.g. the root a TLS-intercepting proxy re-signs with")
set(SOFABGEN_FETCH_TLS_VERIFY "" CACHE STRING
    "Verify the TLS peer for sofabgen's downloads; empty falls back to CMAKE_TLS_VERIFY, then ON")

# Pin the exact digest published in the release's own <asset>.sha256, from
# somewhere other than this download — the release notes, a colleague, a
# prior audit. Unpinned, the download is still checked against that same
# <asset>.sha256, fetched alongside it; that catches a truncated or corrupted
# transfer (trust-on-first-use), but not a network that could substitute both.
set(SOFABGEN_FETCH_SHA256 "" CACHE STRING
    "Expected SHA-256 of the host's sofabgen binary; when set, the published .sha256 sidecar is not fetched")

function(_sofabgen_tls_verify out_value out_origin)
  if(NOT "${SOFABGEN_FETCH_TLS_VERIFY}" STREQUAL "")
    set(_raw "${SOFABGEN_FETCH_TLS_VERIFY}")
    set(_origin "SOFABGEN_FETCH_TLS_VERIFY")
  elseif(DEFINED CMAKE_TLS_VERIFY AND NOT "${CMAKE_TLS_VERIFY}" STREQUAL "")
    set(_raw "${CMAKE_TLS_VERIFY}")
    set(_origin "CMAKE_TLS_VERIFY")
  else()
    set(${out_value} ON PARENT_SCOPE)
    set(${out_origin} "the default" PARENT_SCOPE)
    return()
  endif()
  # An allow-list, not if(${_raw}): a bare string in if() is taken as a
  # variable name and expanded, so a typo would quietly become "off".
  string(TOUPPER "${_raw}" _upper)
  if(_upper MATCHES "^(1|ON|YES|TRUE|Y)$")
    set(${out_value} ON PARENT_SCOPE)
  elseif(_upper MATCHES "^(0|OFF|NO|FALSE|N)$")
    set(${out_value} OFF PARENT_SCOPE)
  else()
    message(FATAL_ERROR "sofabgen: ${_origin} is not a boolean: got '${_raw}'. Use ON or OFF.")
  endif()
  set(${out_origin} "${_origin}" PARENT_SCOPE)
endfunction()

function(_sofabgen_cainfo out_file out_origin)
  if(SOFABGEN_FETCH_TLS_CAINFO)
    set(_file "${SOFABGEN_FETCH_TLS_CAINFO}")
    set(_origin "SOFABGEN_FETCH_TLS_CAINFO")
  elseif(CMAKE_TLS_CAINFO)
    set(_file "${CMAKE_TLS_CAINFO}")
    set(_origin "CMAKE_TLS_CAINFO")
  elseif(NOT "$ENV{SSL_CERT_FILE}" STREQUAL "")
    set(_file "$ENV{SSL_CERT_FILE}")
    set(_origin "the SSL_CERT_FILE environment variable")
  elseif(NOT "$ENV{CURL_CA_BUNDLE}" STREQUAL "")
    set(_file "$ENV{CURL_CA_BUNDLE}")
    set(_origin "the CURL_CA_BUNDLE environment variable")
  else()
    set(${out_file} "" PARENT_SCOPE)
    set(${out_origin} "" PARENT_SCOPE)
    return()
  endif()
  if(NOT EXISTS "${_file}")
    message(FATAL_ERROR
        "sofabgen: ${_origin} names ${_file}, which does not exist; no CA bundle, no verified download.")
  endif()
  set(${out_file} "${_file}" PARENT_SCOPE)
  set(${out_origin} "${_origin}" PARENT_SCOPE)
endfunction()

# _sofabgen_download fetches one URL and reports what actually went wrong.
# Three attempts: a single refused request is not an answer about anything —
# a few CI jobs pulling one release at the same moment is enough for one of
# them to be throttled, and that configure would have worked a second later.
function(_sofabgen_download url out_file what out_error)
  _sofabgen_cainfo(_ca _ca_origin)
  _sofabgen_tls_verify(_verify _verify_origin)
  set(_tls TLS_VERIFY ${_verify})
  if(_ca)
    list(APPEND _tls TLS_CAINFO "${_ca}")
  endif()

  set(_attempts 3)
  foreach(_try RANGE 1 ${_attempts})
    file(DOWNLOAD "${url}" "${out_file}" STATUS _status INACTIVITY_TIMEOUT 60 ${_tls})
    list(GET _status 0 _code)
    if(_code EQUAL 0)
      set(${out_error} "" PARENT_SCOPE)
      return()
    endif()
    list(GET _status 1 _message)
    if(_try LESS _attempts)
      message(STATUS "sofabgen: ${what} failed (${_message}); retrying")
      math(EXPR _pause "${_try} * 2")
      execute_process(COMMAND "${CMAKE_COMMAND}" -E sleep ${_pause})
    endif()
  endforeach()

  if(_message MATCHES "[Cc]ertificate|SSL|TLS")
    if(_ca)
      set(_message "${_message} (verified against ${_ca}, named by ${_ca_origin})")
    else()
      set(_message
          "${_message} -- if this network intercepts TLS, name the CA it re-signs with: -DSOFABGEN_FETCH_TLS_CAINFO=<file>")
    endif()
  endif()
  set(${out_error} "${_message}" PARENT_SCOPE)
endfunction()

function(_sofabgen_host_asset out_asset)
  if(CMAKE_HOST_SYSTEM_NAME STREQUAL "Linux")
    set(_os "linux")
  elseif(CMAKE_HOST_SYSTEM_NAME STREQUAL "Darwin")
    set(_os "darwin")
  elseif(CMAKE_HOST_SYSTEM_NAME STREQUAL "Windows")
    set(_os "windows")
  else()
    set(${out_asset} "" PARENT_SCOPE)
    return()
  endif()

  set(_arch_raw "${CMAKE_HOST_SYSTEM_PROCESSOR}")
  if(_arch_raw MATCHES "^(x86_64|AMD64|amd64)$")
    set(_arch "amd64")
  elseif(_arch_raw MATCHES "^(aarch64|arm64|ARM64)$")
    set(_arch "arm64")
  elseif(_arch_raw MATCHES "^(i386|i686|x86)$")
    set(_arch "386")
  elseif(_arch_raw MATCHES "^(armv7l|armv6l|arm)$")
    set(_arch "arm")
  else()
    set(${out_asset} "" PARENT_SCOPE)
    return()
  endif()

  # Guard against combinations release.yml does not build (matches install.sh).
  set(_supported linux-amd64 linux-386 linux-arm64 linux-arm darwin-amd64 darwin-arm64
      windows-amd64 windows-386 windows-arm64)
  if(NOT "${_os}-${_arch}" IN_LIST _supported)
    set(${out_asset} "" PARENT_SCOPE)
    return()
  endif()

  set(_ext "")
  if(_os STREQUAL "windows")
    set(_ext ".exe")
  endif()
  set(${out_asset} "sofabgen-${_os}-${_arch}${_ext}" PARENT_SCOPE)
endfunction()

# _sofabgen_cached_ok says whether a binary already fetched into the build
# tree still matches its expected digest — used by sofab_fetch_binary() the
# next time its fetch path actually runs (a fresh build tree pointed at an
# existing _sofabgen/<version>/ directory, or SOFABGEN_EXECUTABLE reset by
# hand): once SOFABGEN_EXECUTABLE is cached to a resolved path, that alone
# is what later configures short-circuit on, same as the local-PATH case
# above — this only re-hashes when the resolution itself runs again.
function(_sofabgen_cached_ok binary sidecar out_ok)
  set(${out_ok} FALSE PARENT_SCOPE)
  if(SOFABGEN_FETCH_SHA256)
    set(_expected "${SOFABGEN_FETCH_SHA256}")
  elseif(EXISTS "${sidecar}")
    file(STRINGS "${sidecar}" _line LIMIT_COUNT 1)
    separate_arguments(_parts UNIX_COMMAND "${_line}")
    list(GET _parts 0 _expected)
  else()
    return()
  endif()
  file(SHA256 "${binary}" _actual)
  if(_actual STREQUAL _expected)
    set(${out_ok} TRUE PARENT_SCOPE)
  endif()
endfunction()

# sofab_fetch_binary resolves SOFABGEN_EXECUTABLE: a `sofabgen` already on
# PATH (or set explicitly) is used as-is; otherwise the matching release
# binary is fetched and checksum-verified.
function(sofab_fetch_binary)
  # Quoted on both sides on purpose: an undefined variable left bare in if()
  # compares as the literal string "SOFABGEN_EXECUTABLE", which would silently
  # treat "nobody set this" as "somebody did".
  if(NOT "${SOFABGEN_EXECUTABLE}" STREQUAL "sofabgen" AND NOT "${SOFABGEN_EXECUTABLE}" STREQUAL "")
    message(STATUS "sofabgen: using ${SOFABGEN_EXECUTABLE}, not fetching")
    return()
  endif()

  find_program(_sofabgen_on_path sofabgen)
  if(_sofabgen_on_path)
    set(SOFABGEN_EXECUTABLE "${_sofabgen_on_path}" CACHE FILEPATH "Path to the sofabgen executable" FORCE)
    message(STATUS "sofabgen: using local install at ${_sofabgen_on_path}")
    return()
  endif()

  _sofabgen_host_asset(_asset)
  if(NOT _asset)
    message(FATAL_ERROR
        "sofabgen: no prebuilt binary for ${CMAKE_HOST_SYSTEM_NAME}/${CMAKE_HOST_SYSTEM_PROCESSOR} — "
        "install sofabgen yourself (see the generator repo's README) and re-run cmake, "
        "or set -DSOFABGEN_EXECUTABLE=/path/to/sofabgen.")
  endif()

  if(SOFABGEN_VERSION STREQUAL "latest")
    set(_base "https://github.com/${SOFABGEN_FETCH_REPOSITORY}/releases/latest/download")
  else()
    set(_base "https://github.com/${SOFABGEN_FETCH_REPOSITORY}/releases/download/${SOFABGEN_VERSION}")
  endif()

  set(_dir "${CMAKE_BINARY_DIR}/_sofabgen/${SOFABGEN_VERSION}")
  set(_binary "${_dir}/${_asset}")
  set(_sidecar "${_dir}/${_asset}.sha256")

  if(EXISTS "${_binary}")
    _sofabgen_cached_ok("${_binary}" "${_sidecar}" _cached_ok)
    if(_cached_ok)
      message(STATUS "sofabgen: the cached ${_asset} still matches its digest")
      set(SOFABGEN_EXECUTABLE "${_binary}" CACHE FILEPATH "Path to the sofabgen executable" FORCE)
      return()
    endif()
    message(STATUS "sofabgen: discarding the cached ${_asset}, its digest no longer matches")
    file(REMOVE "${_binary}" "${_sidecar}")
  endif()

  file(MAKE_DIRECTORY "${_dir}")
  message(STATUS "sofabgen: fetching ${_asset} (${SOFABGEN_VERSION})")

  _sofabgen_tls_verify(_verify _verify_origin)
  if(NOT _verify AND NOT SOFABGEN_FETCH_SHA256)
    message(WARNING
        "sofabgen: TLS peer verification is off (${_verify_origin}). The .sha256 sidecar then "
        "arrives over the same unverified connection as the binary it vouches for, so the check "
        "shows only that the two agree, not who sent them. Pin -DSOFABGEN_FETCH_SHA256=<digest>.")
  endif()

  _sofabgen_download("${_base}/${_asset}" "${_binary}.part" "${_asset}" _error)
  if(_error)
    file(REMOVE "${_binary}.part")
    message(FATAL_ERROR "sofabgen: could not download ${_asset} (${SOFABGEN_VERSION}): ${_error}")
  endif()

  if(SOFABGEN_FETCH_SHA256)
    set(_expected "${SOFABGEN_FETCH_SHA256}")
    set(_expected_from "SOFABGEN_FETCH_SHA256")
  else()
    _sofabgen_download("${_base}/${_asset}.sha256" "${_sidecar}" "${_asset}.sha256" _error)
    if(_error)
      file(REMOVE "${_binary}.part")
      message(FATAL_ERROR
          "sofabgen: could not download ${_asset}.sha256 (${SOFABGEN_VERSION}), so the download "
          "cannot be verified: ${_error}")
    endif()
    file(STRINGS "${_sidecar}" _line LIMIT_COUNT 1)
    separate_arguments(_parts UNIX_COMMAND "${_line}")
    list(GET _parts 0 _expected)
    set(_expected_from "the published .sha256")
  endif()

  file(SHA256 "${_binary}.part" _actual)
  if(NOT _actual STREQUAL _expected)
    file(REMOVE "${_binary}.part")
    message(FATAL_ERROR
        "sofabgen: checksum mismatch for ${_asset} against ${_expected_from}: expected ${_expected}, got ${_actual}")
  endif()

  # Renamed only once verified, so an interrupted configure cannot leave a
  # half-written binary that the next run treats as cached.
  file(RENAME "${_binary}.part" "${_binary}")
  if(NOT CMAKE_HOST_SYSTEM_NAME STREQUAL "Windows")
    file(CHMOD "${_binary}" PERMISSIONS
        OWNER_READ OWNER_WRITE OWNER_EXECUTE
        GROUP_READ GROUP_EXECUTE
        WORLD_READ WORLD_EXECUTE)
  endif()

  set(SOFABGEN_EXECUTABLE "${_binary}" CACHE FILEPATH "Path to the sofabgen executable" FORCE)
  message(STATUS "sofabgen: fetched and checksum-verified at ${_binary}")
endfunction()
