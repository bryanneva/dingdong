---
description: Project instructions
alwaysApply: true
---

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

## TDD

`internal/server` has unit tests colocated as `*_test.go`. The suite uses
Go's standard `testing` + `net/http/httptest`. Run:

```sh
make test                                # go test ./... — fast local iteration
go test -race ./internal/server/...      # race detector — what CI runs;
                                         # catches the Add/cancel
                                         # send-on-closed-channel class of bug
go test -cover ./internal/server/...     # coverage summary
go test -coverprofile=/tmp/cov.out ./internal/server/... && \
  go tool cover -func=/tmp/cov.out       # per-function breakdown
```

CI runs `go test -race ./...` (in the `go` job) and `golangci-lint run`
(in the `lint` job, via `golangci-lint-action@v7` with linter `v2.11`).
`make test` is the fast local-iteration command; prefer `go test -race ./...`
before push for parity with what CI checks. See dingdong#16 for the wiring.

Test helpers in `internal/server/helpers_test.go` (`newTestServer`,
`bearerReq`). Synthetic IDs in tests must be lex-ordered to match the
`since` filter's strict-greater-than semantics — real `NewID()` output is
time-sorted, so prefer `"id001"`...`"id00N"` over arbitrary strings like
`"sentinel"` (which lex-sorts after `"live1"` and breaks de-dup).

## Deploy

GitOps pipeline owned in this repo (`.github/workflows/release.yml`); cluster
registration is your GitOps controller's responsibility (Flux, ArgoCD, …).

On every push to `main`:
1. CI builds a multi-arch image and pushes `ghcr.io/<your-org>/dingdong:main`
   plus `:main-<sha7>`.
2. CI runs `yq` to set `images[0].newTag` in `k8s/kustomization.yaml` to
   `main-<sha7>` and opens a `chore(deploy): bump image to main-<sha7>` PR
   from a `chore/deploy-bump-main-<sha7>` branch, then enables auto-merge
   (squash + delete-branch). Once the required `go` and `image` checks pass
   on the bump PR it merges itself.
3. Your GitOps controller detects the source-repo change and applies the new
   manifests; the rollout uses `Recreate` (in-memory state can't tolerate
   two-pod overlap).

The PR-based bump is required because `main` is branch-protected. Direct
pushes by `GITHUB_TOKEN` are rejected; opening a PR and auto-merging is the
sanctioned path.

The `[skip ci]` token in the bump commit message + the `paths-ignore` block
on `release.yml` (`k8s/kustomization.yaml`) prevent the bump-PR's own
squash-merge from re-triggering Release. Don't hand-edit
`k8s/kustomization.yaml`'s `images:` block — CI owns it.

### Deploy-bump prereqs (one-time, repo-level)

The PR-based flow needs two repo-level settings the workflow cannot bootstrap
itself:

1. `allow_auto_merge` enabled on the repo:
   ```sh
   gh api -X PATCH repos/<owner>/<repo> -F allow_auto_merge=true
   ```
2. A `DEPLOY_BOT_TOKEN` repo secret holding a fine-scoped PAT with
   `contents:write` and `pull-requests:write` on this repo. PRs created by
   the default `GITHUB_TOKEN` would not trigger the required `go` / `image`
   CI checks (GitHub's loop-prevention rule), so a PAT is required.

A `deploy` label is also expected; the workflow attaches it to every bump
PR. Create it once with:

```sh
gh label create deploy --color 1d76db \
  --description "Auto-generated deploy-bump PR (release.yml)"
```

### Known limitation: bump-PR CI may not auto-trigger

GitHub has anti-loop protection that suppresses `pull_request` events on
PRs created from inside a workflow run, even when the workflow uses a
fine-scoped PAT acting as a user. Symptoms: bump PR opens, auto-merge
arms (`enabledBy: bryanneva`, `mergeMethod: SQUASH`), but `go` and
`image` checks never register. Auto-merge sits forever.

The release.yml workflow attempts to work around this by pushing an
empty `ci: retrigger after fresh-branch CI miss` commit immediately
after `gh pr create`. Empirically this worked for the first bump PR
in the repo (#26), but did NOT work for subsequent bump PRs (#27, #29)
in the same hour — GitHub appears to deduplicate / rate-limit
workflow-context `pull_request` events.

**Workarounds when a bump PR sits stuck**:

1. **Push another empty commit from your own gh auth** (not the FGT) —
   sometimes triggers CI:
   ```sh
   git fetch origin chore/deploy-bump-main-<sha>
   git switch -c temp-bump origin/chore/deploy-bump-main-<sha>
   git commit --allow-empty -m "ci: retrigger"
   git push origin temp-bump:chore/deploy-bump-main-<sha>
   git switch main && git branch -D temp-bump
   ```
2. **Admin-merge** if the diff is mechanically-generated (yq on
   `kustomization.yaml`) and the image is already pushed to GHCR:
   ```sh
   gh pr merge <N> --admin --squash --delete-branch
   ```
   Safe because: (a) `enforce_admins: false` allows admin bypass,
   (b) the bump diff is one line, (c) the image was verified by
   the Release workflow's own `go vet/build/test` before opening
   the PR.
3. **Trigger a fresh Release run** via `gh workflow run release.yml
   --ref main` if the bump PR is stale (main moved past it). This
   builds a new image at the latest main SHA and opens a fresh
   bump PR.

A more durable fix would migrate from a PAT to a GitHub App
installation token (App-created PRs reliably trigger workflows under
a different identity model) or switch ci.yml from `pull_request` to
`pull_request_target`. Both are larger changes; deferred until the
admin-merge friction outgrows the alternative.

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

## Git Hooks

The Claude-side PreToolUse hooks above cover Claude-driven git ops. For
direct-CLI commits (rare but possible), run once after a fresh clone:

```sh
make install-hooks
```

This sets `core.hooksPath = .git/hooks` locally and symlinks three scripts
from `.claude/hooks/` into `.git/hooks/`:

| Hook | Purpose |
|------|---------|
| `pre-commit` | Runs the public-safety scanner on the staged diff |
| `pre-push` | Runs the public-safety scanner on commits about to leave the machine |
| `prepare-commit-msg` | Appends `Closes #N` when the branch is `<prefix>/<N>-<desc>` |

**Why `core.hooksPath` is set locally (not globally):** The global git config
uses `core.hooksPath = ~/.git-hooks/`, and the global `prepare-commit-msg`
delegates to a per-repo hook with `exec "$REPO_HOOK" "$@"`. Symlinking that
script back into `.git/hooks/` would cause infinite recursion. Instead, we
override `core.hooksPath` for this repo only, and the per-repo
`prepare-commit-msg` inlines the `Closes #N` logic directly. Other repos on
the machine are unaffected.

The hook source files live in `.claude/hooks/` (source-controlled); `.git/hooks/`
entries are symlinks and are not tracked by git.

## What the MVP deliberately leaves out

Short list: SQLite persistence, per-agent identity, MCP server, ACLs, mobile
push, threading UI. Add them only when the bare protocol clearly needs them.
