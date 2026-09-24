# The entry point FetchContent uses.
#
# This is not a project. It is the module bundle a consumer pulls in so that
# sofab_generate() and a matching sofabgen binary are both there:
#
#   include(FetchContent)
#   FetchContent_Declare(sofabuffers_cmake
#     URL https://github.com/sofa-buffers/generator/releases/download/<tag>/sofabgen-cmake.tar.gz)
#   FetchContent_MakeAvailable(sofabuffers_cmake)
#
#   add_executable(app src/main.c)
#   sofab_generate(app LANG c IN schema.yaml OUT generated OUTPUTS message.c message.h)
#
# CMake functions are global once defined, so sofab_generate is available in
# the consumer's own lists after MakeAvailable adds this directory.
#
# A copy taken directly from this repo's source tree (add_subdirectory, or
# FetchContent against the whole repo rather than the packaged tarball) works
# the same way — nothing here depends on being inside the tarball, only
# FetchSofabgen.cmake's default SOFABGEN_VERSION differs (see its header).
cmake_minimum_required(VERSION 3.19) # file(CHMOD) needs 3.19

# SofabGenerate.cmake include()s FetchSofabgen.cmake and calls
# sofab_fetch_binary() itself, at include() time — before any target the
# consumer's own CMakeLists.txt defines — so a configure either has the tool
# or has failed there, never later, deep inside a build, at the custom
# command. Nothing left to do here.
include("${CMAKE_CURRENT_LIST_DIR}/SofabGenerate.cmake")
