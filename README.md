# hive

Small, self-contained components for running coding agents. Each component
is a single Go program with its own container image. Each one works by
itself: under Docker or Podman, in Kubernetes, or as a plain process.

| Component | What it does |
|---|---|
| [hive-gateway](gateway/) | Reverse proxy for inference APIs. It holds the provider credentials and adds them to requests. |
| [hive-agent](agent/) | Runs `claude -p`, `codex exec` or `pi -p` on a timer, on HTTP triggers, or both. |

## Images

Each push to main publishes both images for linux/amd64 and linux/arm64:
`ghcr.io/mathlib-initiative/hive-gateway` and
`ghcr.io/mathlib-initiative/hive-agent`. The tag `latest` follows main, and
`sha-` plus the first seven characters of a commit hash names the build of
that commit. A version tag such as `v1.2.3` publishes `1.2.3`.

## Development

```sh
go vet ./...
go test ./...
docker build -f gateway/Dockerfile -t hive-gateway .
docker build -f agent/Dockerfile -t hive-agent .
```

The code depends only on the Go standard library.

## License

[Apache-2.0](LICENSE)
