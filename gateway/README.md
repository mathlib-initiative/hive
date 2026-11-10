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
| `request_log` | Optional. Uploads a record of every forwarded request to an S3 bucket. See [Request log](#request-log). |

Before forwarding, the gateway removes any credentials the client sent
(`Authorization`, `X-Api-Key`, `Proxy-Authorization`, `Cookie`), so the
gateway token never reaches the provider. The configured headers are added
after that. `GET /healthz` always returns 200 and needs no token.

### Cloudflare AI Gateway

[example.json](example.json) routes both providers through a
[Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/). The
gateway needs Unified Billing or stored provider keys (BYOK), because only the
`cf-aig-authorization` header is sent.

### Request log

With `request_log`, the gateway uploads a record of every forwarded request
to a bucket in Amazon S3 or in any S3-compatible store, such as Garage.

```json
"request_log": {
  "url": "https://s3.us-east-1.amazonaws.com/my-bucket/gateway",
  "region": "us-east-1",
  "access_key_id": "${S3_ACCESS_KEY_ID}",
  "secret_access_key": "${S3_SECRET_ACCESS_KEY}",
  "gzip": true
}
```

| Field | Meaning |
|---|---|
| `url` | The bucket's URL, optionally followed by a key prefix, in path style (`https://ENDPOINT/BUCKET/PREFIX`) or virtual-hosted style (`https://BUCKET.ENDPOINT/PREFIX`). |
| `region` | The region that uploads are signed for: the bucket's region, or the one the store expects, such as `auto` for Cloudflare R2. |
| `access_key_id`, `secret_access_key` | A key that may put objects into the bucket. |
| `gzip` | Optional. If true, each record is compressed with gzip. |

Each record is one object, named `PREFIX/CLIENT/YYYY-MM-DD/HHMMSS.mmm-ID.json`
after the time the request arrived, in UTC, with `.gz` appended when
compressed. Without `clients`, requests are not authenticated, and the
`CLIENT/` part is left out. A record looks like this:

```json
{
  "id": "1f3a9c2b7d4e5f60",
  "time": "2026-11-10T18:01:53.123Z",
  "client": "alice",
  "method": "POST",
  "url": "/anthropic/v1/messages?beta=true",
  "status": 200,
  "duration_ms": 3214,
  "request": { "header": { "Anthropic-Version": ["2023-06-01"] }, "body": "{\"model\":…}" },
  "response": { "header": { "Content-Type": ["text/event-stream"] }, "body": "event: message_start…" }
}
```

`request` holds the header as the client sent it, without the credentials
that the gateway removes, and the body as far as the upstream read it: all
of it, unless the request failed or the upstream answered early.
`response` holds the header and body as the client received them; a
streamed response is recorded whole. A body that is not valid UTF-8 is in
`body_base64` instead of `body`. If the request ended early, `note` says so
as the log line does: `client went away` or `aborted`. Only forwarded
requests are recorded, so not `/healthz`, not requests refused with 400, 401
or 404, and not upgraded connections such as WebSockets.

A record is uploaded after its response is complete, so the bucket never
delays a client. Until then, and while it is uploaded, the gateway holds the
record in memory. Each upload attempt may take up to a minute. A failed
upload is tried again after 1 and after 5 seconds, then the record is
dropped and the failure reported in the log. So if the bucket stops
answering, the gateway holds up to about three minutes' worth of records.
On shutdown, the gateway waits up to 20 seconds in all for requests to
finish and records to be uploaded. Docker kills a container after 10
seconds, so add `--stop-timeout 25` or more.

So that records hold readable bodies, the gateway does not pass the client's
`Accept-Encoding` on. It asks the upstream for gzip itself, except for HEAD
and range requests, and passes responses on uncompressed, without
`Content-Length` if the upstream compressed them.

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
off or the gateway failed. Upstream errors get a line of their own. The log
never shows headers, bodies or upstream URLs; only the
[request log](#request-log) records headers and bodies.

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
- A request log holds every prompt and response, code included. Give the
  gateway a key that can only put objects, and limit who can read the
  bucket.
- The gateway serves plain HTTP. Put a TLS proxy in front if traffic crosses a
  network you do not trust.
- Request paths containing a `..` segment are rejected, so a request cannot
  escape the base path of its route.
