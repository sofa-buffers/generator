#!/usr/bin/env bash
# Kotlin Ir/op recipe — see tests/bench/README.md.
#
# Kotlin compiles to JVM bytecode and the JIT compiles the hot path at runtime,
# so there is no native symbol to --toggle-collect on: this is the `subtract`
# method, and the flags below are the java row's, for the reasons its header
# spells out. The two runs must differ in NOTHING but the rep count:
#
#   -XX:-TieredCompilation -XX:-BackgroundCompilation -XX:CompileThreshold=2000
#       one synchronous compile tier, reached during the harness's fixed WARMUP,
#       so no tier transition happens inside the measured loop.
#   -XX:+UseEpsilonGC -Xms4g -Xmx4g
#       no garbage collection at all and a fully-committed heap. 4g for the same
#       reason java uses it: EpsilonGC never frees, so the WHOLE run's allocation
#       has to fit, and the 32-field VehicleTelemetry allocates a lot per decode.
#   -XX:hashCode=2
#       a constant identity hashCode; the default seeds it from a per-run PRNG.
#
# TOOLCHAIN. Gradle comes from the corelib's own wrapper, so no system Gradle is
# needed -- but the Kotlin Gradle plugin refuses to run on a JDK newer than it
# knows, and a box whose default JDK is past that window cannot build this row at
# all. SOFAB_KOTLIN_JDK points at one that is (17..24; 21 is what CI uses), for
# exactly that case. It is deliberately PER-ROW rather than a plain JAVA_HOME
# export: pointing the whole run at another JVM would move the java and csharp
# rows for a reason no generator change caused. It falls back to JAVA_HOME and
# then to the JVM on PATH, so on a box that needs neither it stays unset --  and
# lib/format.py resolves the same knob into the toolchain table, so a row measured
# on a second runtime says so.
#
# No footprint row: corelib-kotlin-mp is a maxspeed target, and a JVM artifact has
# no .text/.data/.bss to cross-compile.

# _kotlin_jdk: the JDK this row builds and runs on; empty means "whatever is on
# PATH", which is right on a box whose default JDK the Kotlin plugin supports.
_kotlin_jdk() { echo "${SOFAB_KOTLIN_JDK:-${JAVA_HOME:-}}"; }

_kotlin_java() {
    local j
    j="$(_kotlin_jdk)"
    if [ -n "$j" ] && [ -x "$j/bin/java" ]; then echo "$j/bin/java"; else echo java; fi
}

# _kotlin_gradle <dir> <gradlew> <args...> — run gradle in <dir> on that JDK.
_kotlin_gradle() {
    local dir="$1" gradlew="$2" jdk
    shift 2
    jdk="$(_kotlin_jdk)"
    if [ -n "$jdk" ]; then
        ( cd "$dir" && JAVA_HOME="$jdk" "$gradlew" "$@" )
    else
        ( cd "$dir" && "$gradlew" "$@" )
    fi
}

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2" ver gradlew
    gradlew="$corelib/gradlew"
    [ -x "$gradlew" ] || return 1
    ver="$(sed -n 's/^version = "\(.*\)"$/\1/p' "$corelib/build.gradle.kts" | head -1)"
    # Only the JVM target and the multiplatform root module: the harness is a JVM
    # project, and publishing the native targets would pull the whole
    # Kotlin/Native toolchain for artifacts nothing here resolves.
    _kotlin_gradle "$corelib" "$gradlew" --console=plain -q \
        publishJvmPublicationToMavenLocal publishKotlinMultiplatformPublicationToMavenLocal \
        >/dev/null 2>&1 || return 1
    _kotlin_gradle "$proj" "$gradlew" --console=plain -q -Psofab.version="$ver" installDist \
        >/dev/null 2>&1 || return 1
    # Freeze the runtime classpath. The `installDist` start script is a shell
    # wrapper, and measuring it would put a shell inside the Callgrind run; a FILE
    # rather than a `lib/*` glob, because bench_cmd_ir's output is expanded by the
    # shell that runs it and a glob would arrive as several arguments.
    ls "$proj"/build/install/harness/lib/*.jar | paste -sd: > "$proj/.classpath" || return 1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
bench_cmd_ir() {
    echo "$(_kotlin_java) -XX:+UnlockExperimentalVMOptions -XX:+UseEpsilonGC -Xms4g -Xmx4g \
-XX:-TieredCompilation -XX:-BackgroundCompilation -XX:CompileThreshold=2000 \
-XX:hashCode=2 -cp $(cat "$1/.classpath") message.MainKt bench $2"
}
