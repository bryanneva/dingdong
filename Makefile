IMAGE ?= ghcr.io/bryanneva/dingdong
TAG   ?= dev

.PHONY: build cli test vet lint run image push deploy clean verify-fresh-clone install-hooks

build:
	go build -o bin/dingdong .

cli:
	go build -o bin/dingdong-cli ./cmd/dingdong-cli

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

run: build
	DINGDONG_TOKEN=$${DINGDONG_TOKEN:-localdev} ./bin/dingdong --addr :8080

image:
	docker build -t $(IMAGE):$(TAG) .

# Production deploys go through .github/workflows/release.yml + Flux. This
# target is for emergency manual pushes only — CI is the canonical writer.
push-emergency: image
	docker push $(IMAGE):$(TAG)

# Render manifests as Flux would — useful when iterating on k8s/
render:
	kubectl kustomize k8s

# Validate that no source directory is silently excluded by .gitignore.
# Run before declaring repo-shipped, after any .gitignore change, or after
# adding a new cmd/ subdirectory. Catches the PR #6 class of bug where an
# unanchored pattern (e.g. `dingdong-cli` without leading slash) silently
# filters an entire source directory on fresh checkout.
verify-fresh-clone:
	@echo "--- Verifying fresh clone integrity ---"
	@tmpdir=$$(mktemp -d); \
	git clone --quiet . "$$tmpdir/clone" 2>&1 >/dev/null; \
	missing=""; \
	for dir in cmd cmd/dingdong-cli internal internal/server internal/ui internal/ui/static; do \
	  if [ ! -d "$$tmpdir/clone/$$dir" ]; then \
	    missing="$$missing $$dir/"; \
	  fi; \
	done; \
	go_files=$$(find "$$tmpdir/clone" -name "*.go" 2>/dev/null | wc -l | tr -d ' '); \
	rm -rf "$$tmpdir"; \
	if [ -n "$$missing" ]; then \
	  echo "FAIL: missing in fresh clone:$$missing" >&2; \
	  echo "      check .gitignore for unanchored patterns; binary patterns should be anchored with '/' (e.g. /dingdong-cli not dingdong-cli)" >&2; \
	  exit 1; \
	fi; \
	echo "OK: fresh clone has all source dirs and $$go_files .go files."

clean:
	rm -rf bin/

# Wire the public-safety scanner and Closes-#N trailer as real git hooks.
# Sets core.hooksPath to the per-checkout hooks dir (resolved via
# `git rev-parse --absolute-git-dir` so it works in both regular clones and
# git worktrees, where `.git` is a gitfile rather than a directory) so our
# hooks run on direct-CLI commits without touching the global
# ~/.git-hooks/ setup. The global prepare-commit-msg delegation step is
# replaced by an inlined per-repo script (.claude/hooks/prepare-commit-msg)
# to avoid infinite recursion (see CLAUDE.md § Git Hooks).
#
# `set -e` is required: a failed ln (e.g. when the hooks dir doesn't exist
# or a target is occupied by a non-symlink) must abort the recipe instead
# of being swallowed and leaving core.hooksPath pointing at an empty dir.
install-hooks:
	@set -e; \
	echo "--- Installing git hooks ---"; \
	hooks_dir="$$(git rev-parse --absolute-git-dir)/hooks"; \
	src_dir="$$(pwd)/.claude/hooks"; \
	mkdir -p "$$hooks_dir"; \
	git config core.hooksPath "$$hooks_dir"; \
	for hook in pre-commit pre-push prepare-commit-msg; do \
	  ln -sf "$$src_dir/$$hook" "$$hooks_dir/$$hook"; \
	  echo "  $$hooks_dir/$$hook -> $$src_dir/$$hook"; \
	done; \
	echo "OK: hooks installed (core.hooksPath = $$hooks_dir)."
