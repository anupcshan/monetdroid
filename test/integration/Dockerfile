FROM debian:bookworm-slim

# Upgrade: update CLAUDE_VERSION and CLAUDE_CHECKSUM together.
# Latest version: curl -fsSL https://downloads.claude.ai/claude-code-releases/latest
# Checksum is platforms["linux-x64"].checksum in this manifest:
#   curl -fsSL https://downloads.claude.ai/claude-code-releases/<version>/manifest.json
# Use linux-x64 (not -musl) because the base image is glibc, debian bookworm.
# These URLs mirror the canonical installer at https://claude.ai/install.sh.
# If an upgrade breaks tests, check the per-version changelog for what changed:
#   https://code.claude.com/docs/en/changelog.md
ARG CLAUDE_VERSION=2.1.246
ARG DOWNLOAD_BASE_URL=https://downloads.claude.ai/claude-code-releases
ARG CLAUDE_CHECKSUM=1a0a662dc1bb938eaec38545abce9a4a69113d7d7f7c5e1a553ea276617b906a

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates git python3 \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fSL "$DOWNLOAD_BASE_URL/$CLAUDE_VERSION/linux-x64/claude" -o /usr/local/bin/claude \
    && echo "$CLAUDE_CHECKSUM  /usr/local/bin/claude" | sha256sum -c - \
    && chmod +x /usr/local/bin/claude

RUN printf '#!/bin/sh\nexec env KB_CLI_MODE=kb /test "$@"\n' > /usr/local/bin/kb \
    && printf '#!/bin/sh\nexec env KB_CLI_MODE=kbadmin /test "$@"\n' > /usr/local/bin/kbadmin \
    && printf '#!/bin/sh\nexec env BS_CLI_MODE=bashstreamer /test "$@"\n' > /usr/local/bin/bashstreamer \
    && chmod +x /usr/local/bin/kb /usr/local/bin/kbadmin /usr/local/bin/bashstreamer
