SUBSTRATE_ANTHROPIC_IMAGE_REPO ?= us-docker.pkg.dev/agent-substrate/substrate-anthropic

.PHONY: build install test vet clean images

build:
	go build ./...

# Install CLI and worker binary to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test -v ./...

vet:
	go vet ./...

images:
	@echo "Building and pushing worker image to $(SUBSTRATE_ANTHROPIC_IMAGE_REPO)..."
	@worker_img=$$(KO_DOCKER_REPO=$(SUBSTRATE_ANTHROPIC_IMAGE_REPO)/substrate-anthropic-worker ko build --bare ./cmd/substrate-anthropic-worker); \
	echo "Worker image built:" && \
	echo "  --worker-image $$worker_img"

clean:
	rm -rf bin
