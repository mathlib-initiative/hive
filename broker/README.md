# hive-broker

Hands agents short-lived credentials, so that no agent holds a long-lived
GitHub credential. For now it issues GitHub App installation tokens, for use
with `gh` and `git`.

The broker holds a GitHub App's private key. An agent asks the broker for a
token with its own broker token. The broker checks what that agent may
reach, creates an installation token limited to it, and hands it out. The
token expires within an hour. If an agent leaks it, the leak is limited to
the agent's repositories and permissions, and to that hour.

## Configuration

The broker reads one JSON file, by default `/etc/hive-broker/config.json`
(change it with `-config`). In any string value, `${NAME}` is replaced with
the environment variable `NAME`. If a referenced variable is unset or empty,
the broker refuses to start.

```json
{
  "listen": ":8080",
  "clients": {
    "alice": "${ALICE_TOKEN}",
    "bob": "${BOB_TOKEN}"
  },
  "github": {
    "app_id": "Iv23liExampleClientId",
    "private_key_file": "/run/secrets/github-app.pem",
    "access": {
      "alice": {
        "installation_id": 12345678,
        "repositories": ["hive", "docs"],
        "permissions": { "contents": "write", "pull_requests": "write" }
      },
      "bob": { "installation_id": 12345678 }
    }
  }
}
```

| Field | Meaning |
|---|---|
| `listen` | Address to serve on. Default `:8080`. |
| `clients` | Required. Maps each client's name to its own token. Every request must send one, either as `Authorization: Bearer <token>` or as `X-Api-Key: <token>`, and the log names the client. A name starts with an ASCII letter or digit and contains only ASCII letters, digits, `.`, `_` and `-`. No two clients may share a token. This is the same as `clients` in [hive-gateway](../gateway/), so an agent can use one token for both. |
| `github.app_id` | The App's client ID, which GitHub recommends, or its numeric app ID. |
| `github.private_key_file` | Path of one of the App's private keys, in PEM. |
| `github.api_url` | Optional. Base URL of the REST API. Default `https://api.github.com`; for GitHub Enterprise Server, `https://HOST/api/v3`. |
| `github.access` | Maps a client's name to the most its tokens may reach. A client without an entry gets no GitHub tokens. |
| `github.access.*.installation_id` | The App's installation on the account that owns the repositories. |
| `github.access.*.repositories` | Optional. Limits tokens to these repositories of the installation, by name, at most 500. Without it, tokens reach every repository of the installation. |
| `github.access.*.permissions` | Optional. Limits tokens to these permissions, each `read`, `write` or `admin`, such as `{"contents": "write"}`. Without it, tokens have every permission of the installation. |

### Setting up a GitHub App

1. In the settings of the account that owns the repositories, open
   Developer settings, GitHub Apps, and create a new App. It needs no
   webhook, so clear "Active" under Webhook.
2. Give it the repository permissions that the agents need, such as
   Contents and Pull requests, both read and write.
3. Install the App on the account, for all repositories or for some. The
   installation's settings page ends in the installation ID, such as
   `.../settings/installations/12345678`.
4. On the App's settings page, note the client ID, and generate a private
   key. GitHub downloads it as a `.pem` file.

A token never gets more than the App was granted and installed for, whatever
`access` says.

## Getting a token

`POST /github/token` returns a token for the client's access:

```sh
curl -X POST -H "Authorization: Bearer $HIVE_BROKER_TOKEN" http://localhost:8080/github/token
```

```json
{"token":"ghs_…","expires_at":"2026-11-11T13:04:05Z","permissions":{"contents":"write","pull_requests":"write"},"repositories":["hive","docs"]}
```

A JSON body asks for less than the client's access, such as one repository
or a lower level: `{"repositories": ["hive"], "permissions": {"contents":
"read"}}`. Its `repositories` and `permissions` each replace the access's,
so this token has no `pull_requests` permission. Asking for more than the
access allows gets 403. When the token is limited to some repositories, the
answer lists them.

The broker reuses a token for the same client and request while it has more
than 10 minutes left, so a token it hands out lasts between 10 and 60
minutes. Fetch one right before use rather than keeping it. If GitHub refuses
to create a token, the broker answers 502 with GitHub's message.
`GET /healthz` returns 200 and needs no token.

The broker logs one line per request: client (`-` if the request was not
authenticated), method, path, status and duration. GitHub errors get a line
of their own. The broker never logs tokens.

## Using a token with gh and git

`gh` takes a token from `GH_TOKEN`:

```sh
token() {
  curl -sf -X POST -H "Authorization: Bearer $HIVE_BROKER_TOKEN" \
    http://broker:8080/github/token | jq -r .token
}
GH_TOKEN=$(token) gh pr list -R OWNER/REPO
```

With GitHub Enterprise Server, set `GH_HOST` to the server and pass the
token as `GH_ENTERPRISE_TOKEN` instead.

`git` can ask the broker each time it needs a password for github.com
(with GitHub Enterprise Server, use the server's URL instead):

```sh
git config --global credential.https://github.com.helper \
  '!f() { test "$1" = get && printf "username=x-access-token\npassword=%s\n" "$(curl -sf -X POST -H "Authorization: Bearer $HIVE_BROKER_TOKEN" http://broker:8080/github/token | jq -r .token)"; }; f'
```

## Running

The image runs as uid 65532, which must be able to read the private key.
With Podman, keep the key as a secret:

```sh
podman secret create github-app-key app.pem
podman run --rm -p 8080:8080 \
  -v "$PWD/config.json:/etc/hive-broker/config.json:ro" \
  --secret github-app-key,target=/run/secrets/github-app.pem,uid=65532,mode=0400 \
  -e ALICE_TOKEN -e BOB_TOKEN \
  ghcr.io/mathlib-initiative/hive-broker
```

With Docker, mount the key read-only. If only you can read the file, add
`--user "$(id -u)"` so that the broker runs as you. In Kubernetes, mount the
key from a Secret. To build the image yourself, run
`docker build -f broker/Dockerfile -t hive-broker .` in the repository root.
Without a container, run
`go build -o hive-broker ./broker && ./hive-broker -config config.json`.

## Security notes

- The private key can create tokens for every repository the App is
  installed on. Keep it in the broker only, readable by the broker only.
- A client's broker token lets anyone who has it get that client's GitHub
  tokens. Replace it in `clients` if it leaks; tokens already handed out
  stay valid until they expire.
- The broker serves plain HTTP. Keep it on a private network, and put a TLS
  proxy in front if traffic crosses a network you do not trust.
