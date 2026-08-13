CONTAINER_ENGINE ?= podman
ANTHROPIC_WORKER_IMAGE ?= gcr.io/agent-substrate/anthropic-worker

.PHONY: build install test vet clean image push images

build:
	go build ./...

# Install CLI tool to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test -v ./...

vet:
	go vet ./...

image:
	@echo "Building worker image with $(CONTAINER_ENGINE)..."
	$(CONTAINER_ENGINE) build -t $(ANTHROPIC_WORKER_IMAGE):latest .

push: image
	@echo "Pushing worker image to $(ANTHROPIC_WORKER_IMAGE)..."
	$(CONTAINER_ENGINE) push $(ANTHROPIC_WORKER_IMAGE):latest

images: push
	@echo "Worker image published: $(ANTHROPIC_WORKER_IMAGE):latest"

clean:
	rm -rf bin
