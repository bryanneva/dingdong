# dingdong — agent guide

Tiny coordination service for agents running across multiple machines. One Go
binary serves the HTTP API, the SSE stream, and a one-page UI. There is also a
small CLI under `cmd/dingdong-cli`.

## Layout

```
main.go                       server entry point (flags, env, graceful shutdown)
internal/server/
  server.go                   route table, mux
  store.go                    Knock type, Filter, ring buffer + subscriber hub, NewID
  knocks.go                   POST/GET /v1/knocks
  stream.go                   GET /v1/stream (SSE: backlog → live)
  auth.go                     bearer middleware (header or ?token=)
internal/ui/
  ui.go                       embed.FS for static assets
  static/index.html, app.js   one-page web UI (vanilla JS, EventSource)
cmd/dingdong-cli/main.go      knock | wait | tail subcommands
deploy/k8s/                   namespace, deployment, service, ingress, secret.example
Dockerfile, Makefile          multi-stage build, distroless runtime
```

## Mental model

- One namespace of free-form `topic` strings. Agents post `Knock` records,
  subscribers receive them filtered by `topic` and/or `to`.
- The server is stateless except for an in-memory ring buffer (last `--capacity`
  knocks, default 1000). Restart wipes it. That's intentional for the MVP.
- IDs are 28-char hex, lex-sortable by time. `since` filters use `id > since`.
- Auth is one shared bearer token from `DINGDONG_TOKEN`. The UI accepts it via
  `?token=` (sessionStorage) since `EventSource` can't set custom headers.

## Adding a feature

1. **New API surface**: add the handler under `internal/server/`, register it in
   `routes()`. Wrap with `s.requireAuth` unless it's `/healthz`.
2. **New CLI subcommand**: add a `runFoo` and case in `cmd/dingdong-cli/main.go`.
   Reuse `streamKnocks` for any SSE consumer.
3. **UI change**: edit `internal/ui/static/{index.html,app.js}`. The `embed.FS`
   captures them at build time — `go run .` after changes.

## Local dev

```sh
DINGDONG_TOKEN=localdev go run .
# in another shell:
export DINGDONG_URL=http://localhost:8080 DINGDONG_TOKEN=localdev
go run ./cmd/dingdong-cli knock --from test --topic demo --kind info --subject hi
```

## Deploy (k3s-home)

GitOps via Flux. The pipeline is owned in this repo
(`.github/workflows/release.yml`); registration happens in
[`homelab-ci/manifests/flux-sources/dingdong.yaml`](https://github.com/bryanneva/homelab-ci/tree/main/manifests/flux-sources).

On every push to `main`:
1. CI builds a multi-arch image and pushes `ghcr.io/bryanneva/dingdong:main`
   plus `:main-<sha7>`.
2. CI runs `yq` to set `images[0].newTag` in `k8s/kustomization.yaml` to
   `main-<sha7>` and commits the bump back as `github-actions[bot]`.
3. Flux detects the source-repo change within ~1 minute and applies the new
   manifests; the rollout uses `Recreate` (in-memory state can't tolerate
   two-pod overlap).

The `[skip ci]` token in the bot's commit message + the `paths-ignore` block
on `release.yml` prevent re-triggering. Don't hand-edit
`k8s/kustomization.yaml`'s `images:` block — CI owns it.

Secret is supplied by the `OnePasswordItem` in `k8s/dingdong-secret.yaml`
(vault item: `Homelab/dingdong`, field: `token`). Hostname is
`dingdong.neva.home.arpa`, TLS via `local-ca-issuer`.

For local iteration on the deployment manifests, use `make render` to see the
hydrated YAML Flux would apply.

## What the MVP deliberately leaves out

See the plan at `~/.claude/plans/i-want-to-make-twinkly-hoare.md`. Short list:
SQLite persistence, per-agent identity, MCP server, ACLs, mobile push,
threading UI. Add them only when the bare protocol clearly needs them.
