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
- One k8s namespace with one Deployment + Ingress

Out of scope for the MVP: persistence, per-agent identity/ACLs, MCP server,
multi-replica HA, mobile push.

## Quickstart (local)

```sh
# server
DINGDONG_TOKEN=localdev go run .

# in another shell
export DINGDONG_URL=http://localhost:8080
export DINGDONG_TOKEN=localdev

go run ./cmd/dingdong-cli knock --from "laptop:claude" --topic demo \
    --kind ready --subject "hello from laptop"

go run ./cmd/dingdong-cli wait --topic demo --timeout 5m
go run ./cmd/dingdong-cli tail --topic demo
```

Open http://localhost:8080, paste the token, and you'll see a live feed.

## Knock shape

```json
{
  "id": "...",                 // server-assigned, sortable
  "ts": "...",                 // server-assigned, RFC3339
  "from": "laptop:claude:demo",
  "to":   "desktop:claude",    // optional; empty = broadcast
  "topic": "demo",
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
| GET    | `/v1/topics`                             | Distinct topics from the store (always includes `main`) |
| GET    | `/v1/stream?topic=&to=&since=`           | SSE: backlog then live, with keepalives     |
| GET    | `/healthz`                               | Liveness                                    |
| GET    | `/`                                      | Web UI                                      |

Auth: `Authorization: Bearer <DINGDONG_TOKEN>` on every endpoint, or
`?token=<DINGDONG_TOKEN>` for browser/`curl -N` convenience.

**UI bookmark shortcut**: visit `https://<host>/?token=<DINGDONG_TOKEN>` once
and the web UI absorbs the token into `localStorage` (then strips it from
the URL bar). The token persists across reloads and browser restarts.
"forget token" in the header clears it.

## Persistence

Knocks are stored in a SQLite file (pure-Go `modernc.org/sqlite`, WAL mode,
`synchronous=NORMAL`). The path is set with `--db-path`; the k8s manifests
mount a PVC at `/data` and point the flag at `/data/dingdong.db`. History
survives pod restarts.

Retention is count-based — the newest `--retention-rows` (default 100,000)
are kept, and an hourly background loop deletes older rows. For dingdong's
homelab traffic profile that's roughly a hundred days of normal activity.
Adjust the flag if your traffic is heavier or you want a tighter disk bound.

Leaving `--db-path` empty falls back to an in-memory ring buffer
(`--capacity` rows, default 1000) — handy for local development and tests
where you don't need durability.

**Rollback story:** revert the image bump as described under
[Rollback](#rollback). The PVC keeps the existing DB file untouched, so a
freshly-rolled-out pod resumes against the same history. To start clean,
delete the PVC (`kubectl -n dingdong delete pvc dingdong-data`) before the
next rollout — Recreate strategy ensures no overlap.

## Deploy

The manifests in `k8s/` are an example layout for a homelab k3s cluster using
Flux + the 1Password operator. You will need to adapt them for your own
cluster — the hostname, cert-issuer, and secret source are all environment-
specific.

```
push to main                                       watches via GitRepository
   │                                                    │
   ▼                                                    ▼
.github/workflows/release.yml                     cluster GitOps (Flux/ArgoCD/etc.)
  ├─ go vet/build/test                                  │
  ├─ docker buildx push ghcr.io                         ▼
  └─ open `chore/deploy-bump-…` PR                k8s/kustomization.yaml ─► applies k8s/
       └─ auto-merge (squash) once go+image pass ──► (updates kustomization.yaml on main)
```

**Steady state**: merge to `main` → image built and pushed → CI opens a
`chore(deploy): bump image to main-<sha>` PR that auto-merges once required
checks pass → `k8s/kustomization.yaml` updated on `main` → Flux picks up the
change within ~1 minute and rolls out a new pod. There is no manual deploy
step.

The PR-based bump replaces direct push because `main` is branch-protected.
See `CLAUDE.md` "Deploy-bump prereqs" for the one-time `allow_auto_merge`
toggle and `DEPLOY_BOT_TOKEN` secret your fork will need.

### One-time setup

1. **GHCR image visibility** — first push to `ghcr.io/<your-org>/dingdong`
   creates a private package. Flip it to public on GitHub
   (`Packages → dingdong → Package settings → Change visibility → Public`),
   or add an `imagePullSecret` to `k8s/deployment.yaml`. The image is just a
   Go binary, so public is fine.

2. **Token secret** — generate a token (`openssl rand -hex 32`) and put it in
   a `Secret` named `dingdong-token` with key `token` in the `dingdong`
   namespace. The reference example uses the 1Password operator
   (`k8s/dingdong-secret.yaml`); replace it with whatever fits your cluster
   (sealed-secrets, external-secrets, plain `kubectl create secret`, etc.).

3. **GitOps source** — point your GitOps controller (Flux, ArgoCD, …) at
   `k8s/` in this repo. The `kustomization.yaml` `images:` block is rewritten
   by CI on every push to `main`.

After all three are in place, every push to `main` triggers a hands-off
rollout.

### Local image build

`make image` still builds a single-arch image locally for testing. CI is the
only writer for `ghcr.io` tags and `k8s/kustomization.yaml`.

### Rollback

Revert the squash-merged `chore(deploy): bump image to main-…` commit on
`main` and push; your GitOps controller will redeploy the previous image
tag. Or `kubectl set image deployment/dingdong dingdong=ghcr.io/<your-org>/dingdong:<old>`
for an emergency override (the controller will reconcile back to whatever's
in git within a minute, so `git revert` is the durable path).

## Conventions worth adopting

- `from` = `<machine>:<agent-runtime>[:<task>]` — e.g. `laptop:claude:demo`
- `topic` = the task name, shared by all agents working on it
- `kind`:
  - `knock` — generic poke
  - `ready` — "I finished my step, your turn"
  - `need`  — "I'm blocked on you"
  - `info`  — FYI, no action needed
  - `reply` — pair with `in_reply_to`

Nothing is enforced; the server treats them as opaque strings.
