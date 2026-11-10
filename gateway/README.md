# hive-gateway

A reverse proxy for inference APIs that holds the provider credentials.
Clients such as `claude` and `codex` send requests to the gateway without
real credentials. The gateway adds them and forwards each request to the
provider, streaming the response back.

The point is that an agent never sees the provider key. If an agent leaks its
environment, the leak contains at most its gateway token. That token works
only against the gateway, and you can replace it without touching the
provider key or the other clients' tokens.

## Configuration

The gateway reads one JSON file, by default `/etc/hive-gateway/config.json`
(change it with `-config`). In any string value, `${NAME}` is replaced with
the environment variable `NAME`. If a referenced variable is unset or empty,
the gateway refuses to start. This keeps secrets out of the file.

```json
{
  "listen": ":8080",
  "clients": {
    "alice": "${ALICE_TOKEN}",
    "bob": "${BOB_TOKEN}"
  },
  "routes": {
    "/anthropic": {
      "upstream": "https://api.anthropic.com",
      "headers": { "x-api-key": "${ANTHROPIC_API_KEY}" }
    },
    "/openai": {
      "upstream": "https://api.openai.com/v1",
      "headers": { "authorization": "Bearer ${OPENAI_API_KEY}" }
    }
  }
}
```

| Field | Meaning |
|---|---|
| `listen` | Address to serve on. Default `:8080`. |
| `clients` | Optional. Maps each client's name to its own token. If not empty, every client must send its token, either as `Authorization: Bearer <token>` or as `X-Api-Key: <token>`, and the log names the client. A name starts with an ASCII letter or digit and contains only ASCII letters, digits, `.`, `_` and `-`. No two clients may share a token. |
| `routes` | Maps a path prefix to an upstream. A request for `/openai/responses` goes to `https://api.openai.com/v1/responses`. If prefixes overlap, the longest matching prefix wins. |
| `routes.*.upstream` | Base URL of the provider. |
| `routes.*.headers` | Set on every forwarded request, replacing any value the client sent. Provider credentials go here. |

Before forwarding, the gateway removes any credentials the client sent
(`Authorization`, `X-Api-Key`, `Proxy-Authorization`, `Cookie`), so the
gateway token never reaches the provider. The configured headers are added
after that. `GET /healthz` always returns 200 and needs no token.

### Cloudflare AI Gateway

[example.json](example.json) routes both providers through a
[Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/). The
gateway needs Unified Billing or stored provider keys (BYOK), because only the
`cf-aig-authorization` header is sent.

## Running

Run the published image with your config and secrets:

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/gateway/example.json:/etc/hive-gateway/config.json:ro" \
  -e GATEWAY_TOKEN -e CF_ACCOUNT_ID -e CF_GATEWAY_ID -e CF_AIG_TOKEN \
  ghcr.io/mathlib-initiative/hive-gateway
```

Podman takes the same arguments. To build the image yourself, run
`docker build -f gateway/Dockerfile -t hive-gateway .` in the repository root.
In Kubernetes, mount the config from a ConfigMap and set the variables from a
Secret. Without a container, run
`go build -o hive-gateway ./gateway && ./hive-gateway -config config.json`.

The gateway logs one line per request: client (`-` if the request was not
authenticated), method, path, status and duration. A request that ends
early is marked "(client went away)", or "(aborted)" if the upstream broke
off or the gateway failed. Upstream errors get a line of their own. The
gateway never logs headers, bodies or upstream URLs.

## Clients

In these examples, `GATEWAY_TOKEN` holds the client's token.

Claude Code:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080/anthropic
export ANTHROPIC_AUTH_TOKEN="$GATEWAY_TOKEN"
claude -p "hello"
```

Codex, in `~/.codex/config.toml`:

```toml
model_provider = "hive-gateway"

[model_providers.hive-gateway]
name = "hive-gateway"
base_url = "http://localhost:8080/openai"
env_key = "GATEWAY_TOKEN"
wire_api = "responses"
```

## Security notes

- Anyone who can reach the gateway can use its credentials. Set `clients`,
  keep the gateway on a private network, or do both.
- The gateway serves plain HTTP. Put a TLS proxy in front if traffic crosses a
  network you do not trust.
- Request paths containing a `..` segment are rejected, so a request cannot
  escape the base path of its route.
