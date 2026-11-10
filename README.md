# hive

Small, self-contained components for running coding agents. Each component
is a single Go program with its own container image. Each one works by
itself: under Docker or Podman, in Kubernetes, or as a plain process.

| Component | What it does |
|---|---|
| [hive-gateway](gateway/) | Reverse proxy for inference APIs. It holds the provider credentials and adds them to requests. |
| [hive-agent](agent/) | Runs `claude -p` or `codex exec` on a timer, on HTTP triggers, or both. |

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
