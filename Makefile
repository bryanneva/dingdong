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

push: image
	docker push $(IMAGE):$(TAG)

deploy:
	kubectl apply -f deploy/k8s/namespace.yaml
	kubectl apply -f deploy/k8s/

clean:
	rm -rf bin/
