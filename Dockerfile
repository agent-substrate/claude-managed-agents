FROM debian:bookworm-slim

# Install system utilities needed by agent tools (bash, git, curl, ripgrep, coreutils, procps)
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
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

# Set default working directory
WORKDIR /workspace

# Run Anthropic's prebuilt worker poller by default
ENTRYPOINT ["ant", "beta:worker", "poll"]
CMD ["--workdir", "/workspace"]
