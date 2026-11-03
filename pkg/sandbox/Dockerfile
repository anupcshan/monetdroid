# The sandbox image contains only the container prerequisites. Project tools
# come from the repository's devShell, and Nix is not installed here.
FROM docker.io/library/golang:1.27-bookworm AS init

WORKDIR /src
COPY main.go main.go
RUN CGO_ENABLED=0 go build -o /sandbox-init main.go

FROM docker.io/library/debian:bookworm-slim

ARG USER_ID
ARG GROUP_ID
ARG USER_NAME
ARG HOME_PATH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client \
    && rm -rf /var/lib/apt/lists/* \
    && if ! getent group "$GROUP_ID" >/dev/null; then groupadd --gid "$GROUP_ID" "$USER_NAME"; fi \
    && group_name="$(getent group "$GROUP_ID" | cut -d: -f1)" \
    && useradd --uid "$USER_ID" --gid "$group_name" --home-dir "$HOME_PATH" --create-home --shell /bin/bash "$USER_NAME" \
    && chown "$USER_ID:$group_name" "$HOME_PATH"

COPY --from=init /sandbox-init /usr/local/bin/sandbox-init

USER $USER_ID:$GROUP_ID
