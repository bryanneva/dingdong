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

## Tests

```sh
make test                       # Go unit tests (matches CI's `go` job)
go test -race ./...             # what CI actually runs
npm install && npm test         # vitest + happy-dom UI tests for app.js
```

The JS suite covers the channel sidebar, SSE knock handling, and the
auth-401 path in `internal/ui/static/app.js`. See `docs/js-tests.md` for
the rationale and harness design.

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
| POST   | `/v1/webhooks`                           | Register a webhook subscriber               |
| GET    | `/v1/webhooks`                           | List registered subscribers (secrets redacted) |
| DELETE | `/v1/webhooks/{id}`                      | Remove a webhook subscriber                 |
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

## Webhooks

A webhook subscriber registers an HTTP(S) URL that dingdong will POST every
matching knock to as soon as it lands. The delivery shape is intentionally
compatible with [GitHub's webhook signing scheme](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
so consumers (including Hermes' webhook validator) can reuse standard
verification code.

### Register

```sh
curl -X POST "$DINGDONG_URL/v1/webhooks" \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://your-listener.example/hook","topic":"ops"}'
```

Response (`201 Created`):

```json
{
  "id": "0123456789abcdef...",
  "url": "https://your-listener.example/hook",
  "topic": "ops",
  "secret": "<64 hex chars>",
  "created_at": "2026-05-16T10:00:00Z"
}
```

- `topic` is optional. Omit for a wildcard subscription that receives every
  knock regardless of channel.
- `secret` is optional on register. If you don't supply one, the server
  generates a 32-byte random secret and returns it ONCE in the response —
  store it on your side, you can't retrieve it later.
- The body cap is 8 KiB; registrations are tiny.

### List

```sh
curl -H "Authorization: Bearer <token>" "$DINGDONG_URL/v1/webhooks"
```

Returns the array of subscribers with `secret` redacted. The dashboard reads
this every 15s and surfaces it in the sidebar.

### Delete

```sh
curl -X DELETE \
  -H "Authorization: Bearer <token>" \
  "$DINGDONG_URL/v1/webhooks/<id>"
```

Returns `204 No Content`. Deletions take effect immediately — knocks posted
after the delete will not fan out to the removed subscriber.

### Delivery shape

For each matching knock, dingdong POSTs the knock JSON to the subscriber URL
with these headers:

| Header | Value |
|--------|-------|
| `Content-Type` | `application/json` |
| `User-Agent` | `dingdong-webhook/1` |
| `X-Hub-Signature-256` | `sha256=<hex>` — HMAC-SHA256 over the raw body keyed by the subscriber's secret |
| `X-Dingdong-Webhook-Id` | The subscriber id, for log correlation |
| `X-Dingdong-Knock-Id` | The knock id, for log correlation + de-dup |
| `X-Dingdong-Topic` | The knock's topic |
| `X-Dingdong-Delivery-Attempt` | 1-based attempt counter for retries |

Verifying the signature on the receiving side, in Go:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(body)
want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
ok := hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want))
```

### Retry / backoff

Dingdong retries with exponential backoff (1s, 2s, 4s) for up to 4 attempts
total. Retry is triggered on transport errors, HTTP 5xx, and HTTP 429.
Other 4xx statuses (400-499 except 429) are treated as subscriber config
bugs and not retried — the delivery is dropped.

### Persistence and retry durability

When the server runs with `--db-path` (SQLite mode), webhook subscribers and
the retry queue are fully durable:

- Subscriber registrations survive pod restarts — no client re-registration
  needed.
- In-flight retries are written to the `webhook_deliveries` table before the
  delivery goroutine exits. On the next startup, the background delivery
  worker picks them up at the next scheduled backoff slot.
- Deleting a subscriber (`DELETE /v1/webhooks/{id}`) immediately cancels all
  queued retries for that subscriber in the DB — no zombie dispatches.

When running without `--db-path` (in-memory mode, the default for local dev
and tests), the existing behaviour applies: subscribers and in-flight retries
live in memory and are lost on restart.

### Remaining caveats

- **No dead-letter / inspection.** A failed delivery is silently dropped after
  all retry attempts. There is no API to list failed deliveries.
- **No per-subscriber concurrency limit.** A slow subscriber will not block
  others, but a very large number of failing subscribers can accumulate pending
  rows in `webhook_deliveries` until backoff clears.
- **Single-replica only.** The fan-out is in-process; running multiple replicas
  would deliver each knock once per replica.

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
