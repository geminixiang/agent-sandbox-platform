FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download golang.org/x/sys
COPY apps/command-supervisor ./apps/command-supervisor
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/agent-sandbox-supervisor ./apps/command-supervisor/cmd/supervisor \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/agent-sandbox-ctl ./apps/command-supervisor/cmd/ctl

FROM python:3.13.5-slim-bookworm
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl git jq openssh-client tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 10001 --shell /bin/bash sandbox \
 && mkdir -p /workspace \
 && chown sandbox:sandbox /workspace
COPY --from=builder /out/agent-sandbox-supervisor /usr/local/bin/agent-sandbox-supervisor
COPY --from=builder /out/agent-sandbox-ctl /usr/local/bin/agent-sandbox-ctl
ENV HOME=/home/sandbox
USER 0:0
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/agent-sandbox-supervisor"]
