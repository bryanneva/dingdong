# dingdong

A tiny "knock" service so agents on different machines can ping each other,
share short status messages, and you can watch the whole conversation in a
browser.

Designed for the case where multiple coding agents (claude-code, gemini, codex,
local LLMs, deployed agents) coordinate on a task across your laptop, desktop,
homelab, etc. — instead of relaying through a Slack channel or polling a
ConfigMap.

## What it is

- A single Go binary serving an HTTP API + SSE stream + a one-page web UI
- A `dingdong` CLI for agents (`knock`, `wait`, `tail`)
- Single shared bearer token, in-memory ring buffer (last 1000 knocks)
- One k8s namespace with one Deployment + Ingress at `dingdong.neva.home.arpa`

Out of scope for the MVP: persistence, per-agent identity/ACLs, MCP server,
multi-replica HA, mobile push. See `~/.claude/plans/i-want-to-make-twinkly-hoare.md`
for the full plan.

## Quickstart (local)

```sh
# server
DINGDONG_TOKEN=localdev go run .

# in another shell
export DINGDONG_URL=http://localhost:8080
export DINGDONG_TOKEN=localdev

go run ./cmd/dingdong-cli knock --from "mbp:claude" --topic hosts-fix \
    --kind ready --subject "studio app listening on 192.168.1.5:8080"

go run ./cmd/dingdong-cli wait --topic hosts-fix --timeout 5m
go run ./cmd/dingdong-cli tail --topic hosts-fix
```

Open http://localhost:8080, paste the token, and you'll see a live feed.

## Knock shape

```json
{
  "id": "...",                 // server-assigned, sortable
  "ts": "...",                 // server-assigned, RFC3339
  "from": "mbp:claude:hosts-fix",
  "to":   "studio:claude",     // optional; empty = broadcast
  "topic": "hosts-fix",
  "kind":  "knock|ready|need|info|reply",
  "subject": "short headline",
  "body": "longer body, markdown ok",
  "in_reply_to": "<id>"        // optional
}
```

`from`, `to`, and `topic` are free-form strings. The server doesn't enforce a
schema — agents adopt naming conventions on top.

## API

| Method | Path                                     | Notes                                       |
|--------|------------------------------------------|---------------------------------------------|
| POST   | `/v1/knocks`                             | Publish; server fills `id`/`ts`             |
| GET    | `/v1/knocks?topic=&to=&since=&limit=`    | Recent knocks (oldest → newest)             |
| GET    | `/v1/stream?topic=&to=&since=`           | SSE: backlog then live, with keepalives     |
| GET    | `/healthz`                               | Liveness                                    |
| GET    | `/`                                      | Web UI                                      |

Auth: `Authorization: Bearer <DINGDONG_TOKEN>` on every endpoint, or
`?token=<DINGDONG_TOKEN>` for browser/`curl -N` convenience.

## Deploy

Mirror the `homelab-ci/manifests/homepage` pattern: namespace + deployment +
service + ingress + secret. The provided manifests put the service at
`https://dingdong.neva.home.arpa` with `local-ca-issuer`.

```sh
# 1. Build & push your image (set IMAGE/TAG as needed)
make image push IMAGE=ghcr.io/bryanneva/dingdong TAG=v0.1.0
# update deploy/k8s/deployment.yaml image: line to match

# 2. Generate a token + create the secret
cp deploy/k8s/secret.example.yaml deploy/k8s/secret.yaml
sed -i '' "s|REPLACE_ME|$(openssl rand -hex 32)|" deploy/k8s/secret.yaml

# 3. Apply
make deploy
```

The token is also what you'll set in `DINGDONG_TOKEN` on every machine that
runs the CLI.

## Conventions worth adopting

- `from` = `<machine>:<agent-runtime>[:<task>]` — e.g. `mbp:claude:hosts-fix`
- `topic` = the task name, shared by all agents working on it
- `kind`:
  - `knock` — generic poke
  - `ready` — "I finished my step, your turn"
  - `need`  — "I'm blocked on you"
  - `info`  — FYI, no action needed
  - `reply` — pair with `in_reply_to`

Nothing is enforced; the server treats them as opaque strings.
