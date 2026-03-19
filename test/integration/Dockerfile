FROM debian:bookworm-slim

ARG CLAUDE_VERSION=2.1.72
ARG GCS_BUCKET=https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fSL "$GCS_BUCKET/$CLAUDE_VERSION/linux-x64/claude" -o /usr/local/bin/claude \
    && chmod +x /usr/local/bin/claude
