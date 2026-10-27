# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# ---
# relationships:
#   implements: menagerie
# ---
# The Menagerie image: the workspace service (Go, with the web UI embedded)
# and the template service (Rust) under the s6-overlay process supervisor.
# Every build stage runs on the build platform and cross-compiles for the
# target, so only the final stage's package install runs as the target.

ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

# The web UI. The workspace service embeds its build, so it builds first.
FROM --platform=$BUILDPLATFORM node:24.22.0-trixie-slim@sha256:a6155384446670e9719bdc4fd0ebe4d4b29fb32a8d6aa523da3547a6cfbcf22c AS web
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build && test -f dist/index.html

# The workspace service, the start-up helper that prepares its folders, and
# the workspace-commit client the workspace sockets give each devcontainer,
# static binaries.
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS workspace-service
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
COPY web/embed.go ./web/
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/ ./backend/cmd/menagerie ./backend/cmd/menagerie-folders ./backend/cmd/workspace-commit

# The template service. curl-sys finds no system libcurl here, so it builds
# curl with rustls into the binary; the binary needs only the C runtime.
FROM --platform=$BUILDPLATFORM rust:1.97.1-slim-trixie@sha256:8e8cf8f7fd54a2d23d5a743b3a03f56e26b6c774276c33fa0595111704ebb15c AS template-service
ARG TARGETARCH
ARG BUILDARCH
ENV CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=x86_64-linux-gnu-gcc \
    CC_x86_64_unknown_linux_gnu=x86_64-linux-gnu-gcc \
    AR_x86_64_unknown_linux_gnu=x86_64-linux-gnu-ar \
    CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc \
    CC_aarch64_unknown_linux_gnu=aarch64-linux-gnu-gcc \
    AR_aarch64_unknown_linux_gnu=aarch64-linux-gnu-ar
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) target=x86_64-unknown-linux-gnu; cross="gcc-x86-64-linux-gnu libc6-dev-amd64-cross" ;; \
      arm64) target=aarch64-unknown-linux-gnu; cross="gcc-aarch64-linux-gnu libc6-dev-arm64-cross" ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    if [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      apt-get update; \
      apt-get install -y --no-install-recommends $cross; \
      rm -rf /var/lib/apt/lists/*; \
    fi; \
    rustup target add "$target"; \
    echo "$target" > /rust-target
WORKDIR /src/template-service
COPY template-service/ ./
# Only the registry is cached: a cached target folder could keep a binary
# built against other system libraries.
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    set -eu; \
    target=$(cat /rust-target); \
    cargo build --locked --release --target "$target"; \
    mkdir -p /out; \
    cp "target/$target/release/menagerie-template-service" /out/; \
    prefix=$(echo "$target" | sed 's/-unknown-/-/'); \
    needed=$("$prefix-readelf" -d /out/menagerie-template-service | sed -n 's/.*Shared library: \[\(.*\)\]/\1/p' | sort | tr '\n' ' '); \
    for library in $needed; do \
      case "$library" in \
        libc.so.6|libm.so.6|libgcc_s.so.1|ld-linux-*) ;; \
        *) echo "the template service links $library; it may link only the C runtime" >&2; exit 1 ;; \
      esac; \
    done; \
    echo "template service links: $needed"

# The binaries alone, for checking a cross-compiled target without
# emulation: docker build --target binaries --output type=local,dest=out .
FROM scratch AS binaries
COPY --from=workspace-service /out/menagerie /out/menagerie-folders /out/workspace-commit /
COPY --from=template-service /out/menagerie-template-service /

# s6-overlay, downloaded and checked against its release checksums.
FROM --platform=$BUILDPLATFORM $DEBIAN_IMAGE AS s6-overlay
ARG TARGETARCH
# renovate: datasource=github-releases depName=just-containers/s6-overlay versioning=loose extractVersion=^v(?<version>.*)$
ARG S6_OVERLAY_VERSION=3.2.3.2
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64 ;; \
      arm64) arch=aarch64 ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl xz-utils; \
    rm -rf /var/lib/apt/lists/*; \
    url="https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}"; \
    mkdir -p /download /s6-overlay; \
    cd /download; \
    for archive in s6-overlay-noarch.tar.xz "s6-overlay-$arch.tar.xz"; do \
      curl -fsSL -o "$archive" "$url/$archive"; \
      curl -fsSL -o "$archive.sha256" "$url/$archive.sha256"; \
      sha256sum -c "$archive.sha256"; \
      tar -xJpf "$archive" -C /s6-overlay; \
    done; \
    test -x /s6-overlay/init

FROM $DEBIAN_IMAGE
ARG VERSION=dev
# git for Toha's file transport, template hooks and initializeCommand; the
# ssh client for Toha's SSH transport through SSH_AUTH_SOCK; curl for the
# health check.
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl git openssh-client; \
    rm -rf /var/lib/apt/lists/*
COPY --from=s6-overlay /s6-overlay/ /
COPY image/rootfs/ /
# workspace-commit sits beside menagerie, which copies it into each
# workspace's socket folder.
COPY --from=workspace-service /out/menagerie /out/menagerie-folders /out/workspace-commit /usr/local/bin/
COPY --from=template-service /out/menagerie-template-service /usr/local/bin/menagerie-template-service
# A service that cannot start stops the container. Services report no
# readiness, so s6 starts them in order and never waits on a deadline.
ENV S6_BEHAVIOUR_IF_STAGE2_FAILS=2 \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0
LABEL org.opencontainers.image.title="Menagerie" \
      org.opencontainers.image.description="Manages workspaces and fleets of development containers through a web UI." \
      org.opencontainers.image.source="https://github.com/wyrd-company/menagerie" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION"
EXPOSE 8080
HEALTHCHECK CMD ["/usr/local/bin/menagerie-healthcheck"]
ENTRYPOINT ["/init"]
