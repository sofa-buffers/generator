# Shared corelib checkout helper for the conformance runners.
#
# Sourced, not executed. Every runner needs the same thing: a corelib checkout,
# taken from a caller-supplied path if there is one, else cloned. The ref to
# clone defaults to "main" and is overridden per repo by an environment variable
# derived from the repo name:
#
#   corelib-cpp        ->  SOFAB_CORELIB_CPP_REF
#   corelib-c-cpp      ->  SOFAB_CORELIB_C_CPP_REF
#   corelib-rs-no-std  ->  SOFAB_CORELIB_RS_NO_STD_REF
#
# That is how a generator change which needs an unreleased corelib is tested
# before that corelib merges (see docs/CI.md). A ref that does not exist is a
# hard failure and never a silent fall back to main: a run that is green against
# the wrong library is worse than a run that is red.

# clone_corelib <repo> <dest>
clone_corelib() {
    _cl_repo=$1
    _cl_dest=$2
    _cl_var="SOFAB_$(printf '%s' "$_cl_repo" | tr 'a-z-' 'A-Z_')_REF"
    eval "_cl_ref=\${$_cl_var:-main}"
    if ! git clone --depth 1 --branch "$_cl_ref" \
         "https://github.com/sofa-buffers/$_cl_repo.git" "$_cl_dest" >/dev/null 2>&1; then
        echo "FAIL: cannot clone $_cl_repo at ref '$_cl_ref' (\$$_cl_var)"
        exit 1
    fi
    echo "==> $_cl_repo @ $_cl_ref ($(git -C "$_cl_dest" rev-parse HEAD))"
}

# log_corelib <repo> <dir>
# Names the commit of a corelib checkout the suite is about to use, whether it
# was cloned above or handed in by the caller, so a log ties a green run to the
# exact corelib it tested. A directory that is not a git checkout says so.
log_corelib() {
    if _lc_sha=$(git -C "$2" rev-parse HEAD 2>/dev/null); then
        echo "==> $1: $2 @ $_lc_sha"
    else
        echo "==> $1: $2 (not a git checkout)"
    fi
}

# log_vectors <repo> <dir>
# Prints the git blob SHA of the assets/test_vectors.json the suite reads from
# <dir>. The file is copied into every corelib, so this is what shows which copy
# a run used (tests/conformance/lib/check_vector_copies.sh compares them).
log_vectors() {
    _lv_file="$2/assets/test_vectors.json"
    if [ -f "$_lv_file" ]; then
        echo "==> $1: assets/test_vectors.json blob $(git hash-object "$_lv_file")"
    else
        echo "FAIL: $1 has no assets/test_vectors.json ($_lv_file)"
        exit 1
    fi
}
