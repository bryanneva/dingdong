IMAGE ?= ghcr.io/bryanneva/dingdong
TAG   ?= dev

.PHONY: build cli test vet run image push deploy clean

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

clean:
	rm -rf bin/
