# hive-agent

Runs a coding agent CLI, `claude`, `codex` or `pi`, again and again: on a
timer, when triggered over HTTP, or both. Each run is one non-interactive
invocation, `claude -p`, `codex exec` or `pi -p`. Only one run happens at a
time.

## Usage

```
hive-agent -cli claude|codex|pi -session resume|fresh [flags] [-- extra CLI arguments]
```

| Flag | Meaning |
|---|---|
| `-cli` | Required. `claude`, `codex` or `pi`. |
| `-session` | Required. `resume`: every run continues the most recent session in the working directory, and the first run starts one. `fresh`: every run starts a new session. |
| `-prompt`, `-prompt-file` | The prompt for every run. The file is read again before each run, so it can change while the agent is running. If it cannot be read, the run is skipped and its trigger messages are kept for the next run. |
| `-every` | Run at start, then this long after each run ends (`30m`, `1h`). Needs a prompt. |
| `-listen` | Serve the trigger API on this address (`:8080`). |
| `-timeout` | Stop a run that takes longer than this. By default there is no limit. |

At least one of `-every` and `-listen` is needed. Everything after `--` is
passed to the CLI, which runs as:

| | `fresh` | `resume` |
|---|---|---|
| claude | `claude -p ARGS` | `claude -p --continue ARGS` |
| codex | `codex exec ARGS -` | `codex exec resume --last ARGS -` |
| pi | `pi -p ARGS` | `pi -p --continue ARGS` |

The prompt goes to the CLI's standard input. With codex in resume mode, ARGS
may only contain options that `codex exec resume` also accepts, such as `-m`,
`-c` and `--skip-git-repo-check`.

The CLI inherits hive-agent's working directory and environment, so it is
configured as usual: with its own settings files or with variables such as
`ANTHROPIC_BASE_URL`. Its output goes to hive-agent's output.

The CLI runs in a process group of its own. On timeout or shutdown, the group
gets SIGTERM, then SIGKILL 10 seconds later. When a run ends, whatever is left
in the group gets SIGKILL. A process that leaves the group, for example with
`setsid`, is not stopped. pi runs each command of its bash tool in a session
of its own, so whatever those commands leave running is not stopped either.

## Triggers

`POST /trigger` asks for a run. The run starts at once, or as soon as the
current run ends. A non-empty request body is added to the end of the
prompt, after a blank line. All triggers that arrive while a run is in
progress are merged into one next run. Queued bodies may total at most 1 MiB;
past that, `POST /trigger` returns 429, but still asks for a run so that the
queue drains. A single body larger than 1 MiB gets 400. Queued bodies are
kept in memory only, so they are lost when hive-agent stops.

```sh
curl -X POST -H "Authorization: Bearer $HIVE_AGENT_TOKEN" \
  --data 'Issue 12 was opened.' http://localhost:8080/trigger
```

If the environment variable `HIVE_AGENT_TOKEN` is set, `POST /trigger`
requires `Authorization: Bearer <token>`. The variable is removed from the
CLI's environment, so that it does not turn up in the agent's output, but the
CLI runs as the same user and could still find it. The token keeps other
clients out, not the agent itself. `GET /healthz` returns 200.

## Container

The image, `ghcr.io/mathlib-initiative/hive-agent`, contains the three CLIs
plus git, curl and tini. It runs as user `node` (uid 1000) in `/work`.
Claude Code refuses `--dangerously-skip-permissions` as root, so keep it that
way. Sessions and CLI settings live under `/home/node`, so mount a volume
there to keep them across restarts.

```sh
mkdir -p work prompt  # and write prompt/PROMPT.md
docker run -d --name agent \
  -v agent-home:/home/node -v "$PWD/work:/work" -v "$PWD/prompt:/prompt:ro" \
  -e ANTHROPIC_BASE_URL=http://gateway:8080/anthropic -e ANTHROPIC_AUTH_TOKEN \
  ghcr.io/mathlib-initiative/hive-agent \
  -cli claude -session resume -every 30m -prompt-file /prompt/PROMPT.md \
  -- --dangerously-skip-permissions
```

To build the image yourself, run this in the repository root:
`docker build -f agent/Dockerfile -t hive-agent .`

The bind-mounted `/work` must be writable for uid 1000. Create it before
starting, since Docker would otherwise create it owned by root. With rootless
Podman, also add `--userns=keep-id:uid=1000,gid=1000`. The prompt is mounted
read-only, outside `/work`, so that the agent cannot change its own
instructions.

By default, `docker stop` waits 10 seconds before it kills the container,
the same time that hive-agent gives a run after SIGTERM. Add
`--stop-timeout 15` or more so that hive-agent can stop the run and exit by
itself.

For codex, use `-cli codex` and pass
`--dangerously-bypass-approvals-and-sandbox --skip-git-repo-check`. To route
codex through [hive-gateway](../gateway/), put the provider settings from the
gateway README into `/home/node/.codex/config.toml`, or pass them with `-c`.
Point `base_url` at the gateway, such as `http://gateway:8080/openai`, and set
the variable named by `env_key`, such as `-e GATEWAY_TOKEN`.

For pi, use `-cli pi` and choose a model, such as
`-- --model anthropic/claude-haiku-4-5`. pi does not ask for permission
before tool calls, so it needs no extra flag. By default it ignores
project-local files such as `.pi/settings.json` and project skills;
`--approve` loads them. pi ignores `ANTHROPIC_BASE_URL`. To route it through
hive-gateway, set the provider's `baseUrl` in
`/home/node/.pi/agent/models.json` and pass the gateway token as the
provider's credential, such as `-e ANTHROPIC_AUTH_TOKEN`:

```json
{ "providers": { "anthropic": { "baseUrl": "http://gateway:8080/anthropic" } } }
```

## Security notes

- The examples turn off the CLI's permission prompts, which a
  non-interactive run cannot answer anyway. The container is the sandbox, so
  give it only what the agent may touch. Pair it with hive-gateway so the
  container holds no provider credentials.
- A trigger body becomes part of the prompt. Set `HIVE_AGENT_TOKEN`, keep the
  trigger API on a private network, or do both.
- In `resume` mode the CLI continues the most recent session for its working
  directory. Give each agent its own working directory and home.
