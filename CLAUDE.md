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
k8s/                          example manifests (deployment, service, ingress, secret)
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

## Deploy

GitOps pipeline owned in this repo (`.github/workflows/release.yml`); cluster
registration is your GitOps controller's responsibility (Flux, ArgoCD, …).

On every push to `main`:
1. CI builds a multi-arch image and pushes `ghcr.io/<your-org>/dingdong:main`
   plus `:main-<sha7>`.
2. CI runs `yq` to set `images[0].newTag` in `k8s/kustomization.yaml` to
   `main-<sha7>` and commits the bump back as `github-actions[bot]`.
3. Your GitOps controller detects the source-repo change and applies the new
   manifests; the rollout uses `Recreate` (in-memory state can't tolerate
   two-pod overlap).

The `[skip ci]` token in the bot's commit message + the `paths-ignore` block
on `release.yml` prevent re-triggering. Don't hand-edit
`k8s/kustomization.yaml`'s `images:` block — CI owns it.

The example secret manifest (`k8s/dingdong-secret.yaml`) uses the 1Password
operator, but you can swap it for any secret source that produces a
`dingdong-token` Secret with a `token` key. The example ingress targets
`dingdong.example.com`; replace it with your own hostname and cert-issuer.

For local iteration on the deployment manifests, use `make render` to see the
hydrated YAML your controller would apply.

## Machine-Network Safety

Cluster-internal IPs (VM bridge networks, k3s pod/service CIDR — typically
something in `10.x.x.x` or `192.168.x.x`) are not routable from clients on a
different host or subnet. User-facing docs and bootstrap instructions must
reference one of:

- A DNS hostname that resolves on the client's network
- The host's LAN IP (routable from the same subnet)
- A VPN-overlay IP (Tailscale, WireGuard, etc.) when crossing networks

Before documenting a URL or `/etc/hosts` entry for cross-machine bootstrap,
probe from a machine on a different subnet to confirm reachability.

## Shipping Gate

Before declaring any work "repo-shipped" (merged and ready for cross-machine use):

1. **`make verify-fresh-clone`** — temp-clones this repo into `/tmp` and verifies
   `cmd/`, `cmd/dingdong-cli/`, `internal/server/`, `internal/ui/static/` are all
   present. Required after any `.gitignore` change or new `cmd/` subdirectory.
   Catches the PR #6 class of bug where an unanchored `.gitignore` pattern
   silently filters source on fresh checkout.
2. **CI green on the merge commit** — `gh run list --branch main --limit 3`
   before pointing another machine at the repo.
3. **For UI changes** (`internal/ui/static/**`): open the deployed page in a
   real browser and exercise the user-visible flow. PR #5 (auth-overlay
   `display: flex` overriding UA `[hidden]`) shipped because curl-only validation
   doesn't catch CSS specificity bugs.

## Public-repo Safety

This repo is **public**. Before committing or pushing, the
`.claude/settings.json` PreToolUse hooks scan staged content for personal /
internal patterns and block the operation if any are found. Patterns currently
flagged:

- `*.home.arpa` hostnames (homelab-only DNS)
- Real personal email addresses other than the noreply git author
- Specific cluster-internal CIDRs that don't belong in example docs
- Apparent bearer tokens (long hex strings on their own line)
- 1Password vault paths (`vaults/<name>/items/<name>`)

If the hook blocks legitimately-public content (e.g. you're documenting the
`.home.arpa` reservation generically), you can override for a single
operation by exporting `DINGDONG_PUBLIC_OVERRIDE=1` in the shell that runs
the commit. Don't make this the default — the whole point is the catch.

To extend the pattern list, edit
`.claude/scripts/check-public-safe.sh`.

## What the MVP deliberately leaves out

Short list: SQLite persistence, per-agent identity, MCP server, ACLs, mobile
push, threading UI. Add them only when the bare protocol clearly needs them.
