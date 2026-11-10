# Docker Compose example

[compose.yaml](compose.yaml) runs hive-agent with Claude Code once at start,
then 30 minutes after each run ends, and when triggered. Its requests go
through hive-gateway, which adds the Anthropic API key, so the agent never
sees the key.

```sh
cd examples/compose
touch .env && chmod 600 .env
echo "ANTHROPIC_API_KEY=..." > .env  # your key
echo "GATEWAY_TOKEN=$(openssl rand -hex 32)" >> .env
docker compose up -d
docker compose logs -f
```

Compose reads both variables from `.env`, which Git ignores, in every command
run in this directory. With Podman, use `podman compose`, which runs
docker-compose or podman-compose.

The agent reads [prompt/PROMPT.md](prompt/PROMPT.md) again before each run,
so edit it to change what the agent does. To ask for a run, with a message
added to the prompt:

```sh
curl -X POST --data 'Issue 12 was opened.' http://127.0.0.1:8080/trigger
```

The trigger API has no token here, so compose.yaml publishes it on this
machine only. To require one, set `HIVE_AGENT_TOKEN` in the agent's
`environment` and send it as the [hive-agent](../../agent/) README shows.

The agent works in the named volume `work` (`docker compose exec agent ls`
lists it) and keeps its sessions and settings in the named volume `home`.
`docker compose down` stops both services and keeps the volumes;
`docker compose down -v` also deletes them.

To use codex or pi instead, change `command` and configure the CLI as the
[hive-agent](../../agent/) README describes. codex also needs an `/openai`
route in `gateway.json`, as in the [hive-gateway](../../gateway/) README.
