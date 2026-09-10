# Same Go version as go.mod and the release Dockerfile.
FROM golang:1.26-bookworm AS golang

FROM mcr.microsoft.com/devcontainers/base:bookworm

COPY --from=golang /usr/local/go /usr/local/go
ENV GOPATH=/home/vscode/go
ENV PATH=/usr/local/go/bin:/home/vscode/go/bin:$PATH

# `ENV PATH` alone is not enough: /etc/profile reassigns PATH outright, so any
# login shell (VS Code terminals, and anything Claude Code spawns) would lose
# the Go toolchain. GOPATH survives, only PATH needs replaying here.
RUN printf 'export PATH=/usr/local/go/bin:${GOPATH:-$HOME/go}/bin:$PATH\n' \
        > /etc/profile.d/go.sh

# ffmpeg and mpv are shelled out to at runtime (transcode/, jukebox/), and
# transcode_test.go skips without ffmpeg. shared-mime-info matches the release
# image. build-essential is only needed for `-tags nowasm`, which swaps the
# wasm sqlite driver for the cgo one.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential \
        ffmpeg \
        mpv \
        shared-mime-info \
        sqlite3 \
    && rm -rf /var/lib/apt/lists/*

# Same golangci-lint the CI workflow pins.
ARG GOLANGCI_LINT_VERSION=v2.12.2
RUN GOPATH=/tmp/go GOBIN=/usr/local/bin \
        go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} \
    && rm -rf /tmp/go
