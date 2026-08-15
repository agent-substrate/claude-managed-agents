CONTAINER_ENGINE ?= podman
PLATFORM ?= linux/amd64
GOOGLE_CLOUD_PROJECT ?= dberkov-gke-dev3
ATE_ANTWORKER_IMAGE ?= gcr.io/$(GOOGLE_CLOUD_PROJECT)/ate-antworker

.PHONY: build install test vet clean image push

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
	@echo "Building worker image with $(CONTAINER_ENGINE) for $(PLATFORM)..."
	$(CONTAINER_ENGINE) build --platform $(PLATFORM) -t $(ATE_ANTWORKER_IMAGE):latest .

push: image
	@echo "Pushing worker image to $(ATE_ANTWORKER_IMAGE)..."
	@digest_file=$$(mktemp); \
	if $(CONTAINER_ENGINE) push --digestfile "$$digest_file" $(ATE_ANTWORKER_IMAGE):latest; then \
		digest=$$(cat "$$digest_file" 2>/dev/null); \
		rm -f "$$digest_file"; \
		if [ -n "$$digest" ]; then \
			echo "Image pushed:"; \
			echo "  $(ATE_ANTWORKER_IMAGE)@$$digest"; \
		else \
			echo "Image pushed: $(ATE_ANTWORKER_IMAGE):latest"; \
		fi; \
	elif $(CONTAINER_ENGINE) push $(ATE_ANTWORKER_IMAGE):latest; then \
		rm -f "$$digest_file"; \
		digest=$$($(CONTAINER_ENGINE) inspect --format '{{.Digest}}' $(ATE_ANTWORKER_IMAGE):latest 2>/dev/null); \
		if [ -n "$$digest" ]; then \
			echo "Image pushed:"; \
			echo "  $(ATE_ANTWORKER_IMAGE)@$$digest"; \
		else \
			echo "Image pushed: $(ATE_ANTWORKER_IMAGE):latest"; \
		fi; \
	else \
		rm -f "$$digest_file"; \
		echo "Error: Failed to push image to $(ATE_ANTWORKER_IMAGE)"; \
		exit 1; \
	fi

clean:
	rm -rf bin
