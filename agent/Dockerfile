# Build from the repository root: docker build -f agent/Dockerfile .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY agent ./agent
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /hive-agent ./agent

FROM node:24-slim
ARG CLAUDE_CODE_VERSION=2.1.322
ARG CODEX_VERSION=0.167.0
ARG PI_VERSION=1.5.2
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git tini \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} @openai/codex@${CODEX_VERSION} \
    @earendil-works/pi-coding-agent@${PI_VERSION} \
 && npm cache clean --force \
 && mkdir /work && chown node:node /work
COPY --from=build /hive-agent /usr/local/bin/hive-agent
# Sessions and CLI settings live under /home/node; mount a volume there to
# keep them across restarts. The user is node, given by number so that
# Kubernetes can check runAsNonRoot.
USER 1000:1000
WORKDIR /work
# tini reaps the processes the CLIs leave behind.
ENTRYPOINT ["tini", "--", "hive-agent"]
