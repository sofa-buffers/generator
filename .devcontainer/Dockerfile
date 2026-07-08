FROM ubuntu:26.04

ARG GO_VERSION=1.24.4

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
    && rm -rf /var/lib/apt/lists/*

# Go toolchain
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

# Language toolchains for the per-language conformance harnesses
# (tests/conformance/<lang>/run.sh). C/C++ (gcc/g++/make) come from
# build-essential above; Node (TypeScript) is installed below. This adds Python,
# Rust (cargo/rustc), and Java + Maven; the .NET SDK (C#) is installed separately
# below because Ubuntu 24.04's apt only carries .NET 8 while the generated C#
# project targets net9.0.
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 \
        cargo \
        rustc \
        default-jdk \
        maven \
    && rm -rf /var/lib/apt/lists/*
ENV JAVA_HOME=/usr/lib/jvm/default-java

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
