FROM ubuntu:26.04

ARG GO_VERSION=1.24.4
# Keep in lockstep with the mlugg/setup-zig version pinned by the lang-zig job
# in .github/workflows/ci.yml and the zig bench row in .github/workflows/bench.yml.
ARG ZIG_VERSION=0.16.0

ENV DEBIAN_FRONTEND=noninteractive
ENV GOPATH=/root/go
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH

# Base tooling
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        wget \
        git \
        build-essential \
        gnupg \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

# Zig toolchain — the Zig backend (generators/zig) has its own conformance
# harness (tests/conformance/zig/run.sh) and bench row, both of which shell out
# to `zig build`. Installed from the official tarball because apt carries no
# Zig package. Note the archive name puts the arch first
# (zig-x86_64-linux-<ver>), which is the post-0.15 naming convention.
RUN ZIG_ARCH="$(dpkg --print-architecture)" \
    && case "$ZIG_ARCH" in \
         amd64) ZIG_ARCH=x86_64 ;; \
         arm64) ZIG_ARCH=aarch64 ;; \
         *) echo "unsupported arch for zig: $ZIG_ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-${ZIG_ARCH}-linux-${ZIG_VERSION}.tar.xz" \
       -o /tmp/zig.tar.xz \
    && mkdir -p /usr/local/zig \
    && tar -C /usr/local/zig --strip-components=1 -xJf /tmp/zig.tar.xz \
    && rm /tmp/zig.tar.xz \
    && ln -sf /usr/local/zig/zig /usr/local/bin/zig

# Language toolchains for the per-language conformance harnesses
# (tests/conformance/<lang>/run.sh). C/C++ (gcc/g++/make) come from
# build-essential above; Go and Zig are installed above; Node (TypeScript) is
# installed below. This adds Python and Java + Maven; Rust is installed via
# rustup below (the footprint bench needs bare-metal targets apt rustc cannot
# add); the .NET SDK (C#) is installed separately below because Ubuntu 24.04's
# apt only carries .NET 8 while the generated C# project targets net9.0.
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 \
        default-jdk \
        maven \
    && rm -rf /var/lib/apt/lists/*
ENV JAVA_HOME=/usr/lib/jvm/default-java

# Rust via rustup (not apt cargo/rustc). The conformance harnesses only need
# host cargo/rustc, but the footprint bench (tests/bench/lang/rust.sh) links the
# generated crate for thumbv6m-none-eabi and sizes it with llvm-size, which
# require `rustup target add thumbv6m-none-eabi` and the llvm-tools-preview
# component — apt's rustc has no way to add either. One rustup toolchain serves
# both harnesses.
ENV RUSTUP_HOME=/root/.rustup CARGO_HOME=/root/.cargo
ENV PATH=/root/.cargo/bin:$PATH
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
       | sh -s -- -y --default-toolchain stable --profile minimal \
    && rustup component add llvm-tools-preview \
    && rustup target add thumbv6m-none-eabi

# Bench toolchain (tests/bench/run.sh, ARCHITECTURE §15). Not needed by the
# conformance harnesses, but the bench measures two axes that need extra tools:
#   * Ir/op via Callgrind for every row            -> valgrind
#   * footprint (.text/.data/.bss) cross-compiled to the embedded targets the
#     footprint profiles ship to (rows c, cpp-c-cpp) -> arm-none-eabi and
#     riscv64-unknown-elf GCC + binutils (`size`); picolibc supplies the RISC-V
#     freestanding headers. (The rust-rs-no-std footprint uses the rustup
#     thumbv6m target added above.)
# Without these a bench run records `!` for the affected cells and overwrites the
# committed results.txt (tests/bench/lib/format.py does not preserve prior values
# on a failed measurement).
RUN apt-get update && apt-get install -y --no-install-recommends \
        valgrind \
        gcc-arm-none-eabi \
        gcc-riscv64-unknown-elf \
        picolibc-riscv64-unknown-elf \
    && rm -rf /var/lib/apt/lists/*

# .NET SDK 9 — the generated C# project and corelib-cs target net9.0, which
# Ubuntu 24.04's apt does not provide, so install it from Microsoft's official
# script (self-contained under /usr/share/dotnet).
RUN curl -fsSL https://dot.net/v1/dotnet-install.sh -o /tmp/dotnet-install.sh \
    && bash /tmp/dotnet-install.sh --channel 9.0 --install-dir /usr/share/dotnet \
    && rm /tmp/dotnet-install.sh \
    && ln -sf /usr/share/dotnet/dotnet /usr/local/bin/dotnet
ENV DOTNET_ROOT=/usr/share/dotnet
ENV PATH=/usr/share/dotnet:$PATH
# .NET needs this where ICU/locale data is absent (the C# csproj also sets
# InvariantGlobalization, but the env var keeps ad-hoc `dotnet` runs working).
ENV DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1

# Node.js (LTS) for Claude Code
RUN curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Claude Code
RUN npm install -g @anthropic-ai/claude-code

# GitHub CLI (gh) — installed from GitHub's official apt repository
RUN mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

CMD ["/bin/bash"]
