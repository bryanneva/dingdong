IMAGE ?= ghcr.io/bryanneva/dingdong
TAG   ?= dev

.PHONY: build cli test vet run image push deploy clean verify-fresh-clone

build:
	go build -o bin/dingdong .

cli:
	go build -o bin/dingdong-cli ./cmd/dingdong-cli

test:
	go test ./...

vet:
	go vet ./...

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
