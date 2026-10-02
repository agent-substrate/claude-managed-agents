# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Stage 1: Build Go binaries
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -o /out/ate-ant ./cmd/ate-ant && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -o /out/loadgen ./benchmarks/loadgen && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -o /out/dashboard ./benchmarks/dashboard

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
COPY --from=builder /out/ate-ant /usr/local/bin/ate-ant
COPY --from=builder /out/loadgen /usr/local/bin/loadgen
COPY --from=builder /out/dashboard /usr/local/bin/dashboard

# Set default working directory
WORKDIR /workspace

# Default entrypoint
ENTRYPOINT ["ate-ant", "run"]
