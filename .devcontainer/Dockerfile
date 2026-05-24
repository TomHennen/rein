# Dockerfile for rein development
#
# Built once by the Dev Containers extension. Iterating on this happens
# rarely — the agent should NOT modify this file as part of normal work.
# If the agent needs additional tooling, it should propose changes via
# a note in PLAN.md under "tooling requests", not edit this directly.
#
# Uses the official Go image rather than the Microsoft devcontainers base
# to avoid the MCR image lagging behind current Go releases. This matters
# for a security-focused project — we want current, supported Go.

FROM golang:1.26-bookworm

# Create a non-root user matching the VS Code Dev Containers convention.
# UID 1000 is the standard; the username 'vscode' is what the Dev Containers
# extension expects by default.
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID

RUN groupadd --gid $USER_GID $USERNAME \
    && useradd --uid $USER_UID --gid $USER_GID -m $USERNAME \
    && apt-get update \
    && apt-get install -y sudo \
    && echo "$USERNAME ALL=(root) NOPASSWD:ALL" > /etc/sudoers.d/$USERNAME \
    && chmod 0440 /etc/sudoers.d/$USERNAME

# System packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    # gh CLI for testing GitHub API interactions
    gh \
    # jq for inspecting JSON responses
    jq \
    # libsecret — for optional secret-service keyring backend on Linux;
    # not used in Phase 0 (file backend runs in the container) but avoids
    # needing a container rebuild if we want to test it later
    libsecret-1-0 \
    # Build tools for cgo dependencies
    build-essential \
    pkg-config \
    # Git (base image may already have it; explicit is better)
    git \
    # Shell utilities
    curl \
    && rm -rf /var/lib/apt/lists/*

# Install golangci-lint
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b /usr/local/bin v1.64.0

# Install Claude Code CLI
# Adjust if Anthropic changes the install path. The || true means the
# container builds even if this fails (e.g. network issue during build);
# Claude Code can be installed manually inside the container if needed.
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- --yes || true

# Make sure the vscode user owns its home directory and config paths
RUN mkdir -p /home/$USERNAME/.config/rein \
    && chown -R $USERNAME:$USERNAME /home/$USERNAME

# Set the default user and working directory
USER $USERNAME
WORKDIR /workspaces/rein

# Verify versions at build time
RUN go version

SHELL ["/bin/bash", "-c"]
