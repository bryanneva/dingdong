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

The pipeline lives in this repo; the cluster registration lives in
[`bryanneva/homelab-ci`](https://github.com/bryanneva/homelab-ci).

```
push to main                          watches via GitRepository
   │                                       │
   ▼                                       ▼
.github/workflows/release.yml        homelab-ci/manifests/flux-sources/
  ├─ go vet/build/test                  dingdong.yaml
  ├─ docker buildx push ghcr.io           │
  └─ kustomize edit + commit ──┐          ▼
                               ▼     Flux Kustomization
                       k8s/kustomization.yaml ────► applies k8s/ to cluster
```

**Steady state**: merge to `main` → image built and pushed → CI commits the
new tag into `k8s/kustomization.yaml` → Flux picks up the change within ~1
minute and rolls out a new pod. There is no manual deploy step.

### One-time setup

1. **GHCR image visibility** — first push to `ghcr.io/bryanneva/dingdong`
   creates a private package. Flip it to public on GitHub
   (`Packages → dingdong → Package settings → Change visibility → Public`),
   or add an `imagePullSecret` to `k8s/deployment.yaml`. The image is just a
   Go binary, so public is fine.

2. **Token in 1Password** — create a vault item at
   `Homelab/dingdong` with a single field `token`:
   ```sh
   op item create --category=password --vault=Homelab --title=dingdong \
     "token[password]=$(openssl rand -hex 32)"
   ```
   The `OnePasswordItem` in `k8s/dingdong-secret.yaml` syncs it into the
   cluster as the `dingdong-token` Secret.

3. **Flux deploy key** — Flux clones this private repo over SSH:
   ```sh
   ssh-keygen -t ed25519 -N "" -f /tmp/flux-dingdong -C flux-dingdong
   gh repo deploy-key add /tmp/flux-dingdong.pub --repo bryanneva/dingdong --title flux-read
   kubectl create secret generic flux-dingdong \
     --namespace=flux-system \
     --from-file=identity=/tmp/flux-dingdong \
     --from-file=identity.pub=/tmp/flux-dingdong.pub \
     --from-literal=known_hosts="$(ssh-keyscan github.com 2>/dev/null)"
   shred -u /tmp/flux-dingdong /tmp/flux-dingdong.pub
   ```

4. **Register with homelab-ci** — add `manifests/namespaces/dingdong.yaml`
   and `manifests/flux-sources/dingdong.yaml` mirroring the open-brain
   pattern. (PR for this is opened separately when this repo's deploy
   pipeline lands.)

After all four are in place, every push to `main` triggers a hands-off rollout.

### Local image build

`make image` still builds a single-arch image locally for testing. CI is the
only writer for `ghcr.io` tags and `k8s/kustomization.yaml`.

### Rollback

Revert the bot's `chore(deploy): bump image to main-…` commit and push;
Flux will redeploy the previous image tag. Or `kubectl set image
deployment/dingdong dingdong=ghcr.io/bryanneva/dingdong:<old>` for an
emergency override (Flux will reconcile back to whatever's in git within
a minute, so `git revert` is the durable path).

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
