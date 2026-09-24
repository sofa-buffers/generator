# SofabGenerate.cmake — run sofabgen at build time and wire its output
# straight into a CMake target.
#
# See cmake/CMakeLists.txt for how to fetch this bundle. Included directly
# (rather than through that bootstrap) it still works standalone: the
# include() below resolves SOFABGEN_EXECUTABLE the same way.
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
# source on the target and OUT is added to its include directories, so
# `#include "message.h"`-style generated headers resolve without the caller
# repeating the path.
#
# OUTPUTS is required and not discovered automatically: add_custom_command()
# needs every OUTPUT named up front for correct incremental-build tracking,
# and what sofabgen writes for a given schema depends on the language (the
# message name for most targets, a fixed `message.py`-style name for some —
# see docs/generator/<lang>.md in this repo) in a way this function has no
# reason to hardcode per language. If you don't already know the names, run
# sofabgen by hand once and use what it reports:
#
#   sofabgen -lang <lang> -in <schema> -out <dir>
include_guard(GLOBAL)

include("${CMAKE_CURRENT_LIST_DIR}/FetchSofabgen.cmake")
sofab_fetch_binary()

function(sofab_generate TARGET)
  set(_sofab_keywords LANG IN OUT CONFIG OUTPUTS)
  cmake_parse_arguments(SOFAB "" "LANG;IN;OUT;CONFIG" "OUTPUTS" ${ARGN})

  # cmake_parse_arguments clears SOFAB_<KEYWORD> for every keyword this call
  # omitted, and clearing a normal variable is what lets a CACHE variable of
  # the same name show through it silently — a project with, say, a stray
  # cache entry named SOFAB_LANG would otherwise have it leak into a call
  # that never passed LANG at all. Re-clearing every keyword the call did not
  # actually name (per the ARGN walk below, which is how the parser itself
  # reads "passed") closes that.
  set(_sofab_named "")
  set(_sofab_pending "")
  foreach(_sofab_arg IN LISTS ARGN)
    list(FIND _sofab_keywords "${_sofab_arg}" _sofab_at)
    if(NOT _sofab_at EQUAL -1)
      set(_sofab_pending "${_sofab_arg}")
    elseif(NOT _sofab_pending STREQUAL "")
      list(APPEND _sofab_named "${_sofab_pending}")
      set(_sofab_pending "")
    endif()
  endforeach()
  foreach(_sofab_keyword IN LISTS _sofab_keywords)
    list(FIND _sofab_named "${_sofab_keyword}" _sofab_at)
    if(_sofab_at EQUAL -1)
      set("SOFAB_${_sofab_keyword}" "")
    endif()
  endforeach()

  if(NOT TARGET "${TARGET}")
    message(FATAL_ERROR "sofab_generate: '${TARGET}' is not a target — "
        "create it with add_executable()/add_library() first.")
  endif()
  get_target_property(_sofab_alias "${TARGET}" ALIASED_TARGET)
  if(_sofab_alias)
    message(WARNING "sofab_generate ignored for ALIAS target ${TARGET}")
    return()
  endif()
  get_target_property(_sofab_type "${TARGET}" TYPE)
  if(_sofab_type STREQUAL "INTERFACE_LIBRARY")
    message(WARNING "sofab_generate ignored for INTERFACE library ${TARGET} — "
        "target_sources() has no PRIVATE scope to attach generated files to on one")
    return()
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
