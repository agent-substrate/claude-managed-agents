# Stage 1: Build Go binaries
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -o /out/ate-antworker ./cmd/ate-antworker

# Stage 2: Runtime image (Ubuntu 24.04 LTS)
FROM ubuntu:noble

ENV DEBIAN_FRONTEND=noninteractive

# Install system utilities and developer tools needed by agent tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    coreutils \
    curl \
    git \
    jq \
    procps \
    ripgrep \
    tar \
    gzip \
    && rm -rf /var/lib/apt/lists/*

# Install Anthropic's official prebuilt ant CLI binary
ARG ANT_VERSION=1.22.1
RUN set -eux; \
    ARCH="$(dpkg --print-architecture)"; \
    case "${ARCH}" in \
        amd64) ANTHROPIC_ARCH="amd64" ;; \
        arm64) ANTHROPIC_ARCH="arm64" ;; \
        *) echo "Unsupported architecture: ${ARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/anthropics/anthropic-cli/releases/download/v${ANT_VERSION}/ant_${ANT_VERSION}_linux_${ANTHROPIC_ARCH}.tar.gz" \
    | tar -xz -C /usr/local/bin ant; \
    chmod +x /usr/local/bin/ant; \
    ant --version

# Copy compiled binaries from builder
COPY --from=builder /out/ate-antworker /usr/local/bin/ate-antworker
RUN ln -s /usr/local/bin/ate-antworker /usr/local/bin/ate-antworker-dispatch

# Set default working directory
WORKDIR /workspace

# Default entrypoint
ENTRYPOINT ["ate-antworker", "worker"]
