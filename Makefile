# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

CONTAINER_ENGINE ?= podman
PLATFORM ?= linux/amd64
GOOGLE_CLOUD_PROJECT ?= dberkov-gke-dev3
ATE_ANT_IMAGE ?= gcr.io/$(GOOGLE_CLOUD_PROJECT)/ate-ant
ATE_IMAGES_REPO ?= gcr.io/$(GOOGLE_CLOUD_PROJECT)/ate-images
SUBSTRATE_REPO ?= $(HOME)/substrate
SUBSTRATE_DIR ?=

.PHONY: build install test vet clean image push images push-worker-image push-ateom-image verify-boilerplate

build:
	go build ./...

# Install CLI tool to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test -v ./...

vet:
	go vet ./...

verify-boilerplate:
	hack/verify/boilerplate.sh

image:
	@echo "Building worker image with $(CONTAINER_ENGINE) for $(PLATFORM)..."
	$(CONTAINER_ENGINE) build --platform $(PLATFORM) -t $(ATE_ANT_IMAGE):latest .

# Build and push the ate-ant and ateworker-gvisor images, then print export
# commands for their digest-pinned references.
images: push-worker-image push-ateom-image
	@echo
	@echo "Images pushed:"
	@echo "export WORKER_IMAGE=$$(cat bin/worker-image)"
	@echo "export ATEOM_IMAGE=$$(cat bin/ateom-image)"

# Kept for benchmarks/build-images.sh and benchmarks/install.sh.
push: push-worker-image

push-worker-image: image
	@echo "Authenticating $(CONTAINER_ENGINE) with gcr.io..."
	@(gcloud auth application-default print-access-token 2>/dev/null || gcloud auth print-access-token) | \
		$(CONTAINER_ENGINE) login -u oauth2accesstoken --password-stdin https://gcr.io >/dev/null
	@echo "Pushing worker image to $(ATE_ANT_IMAGE)..."
	@digest_file=$$(mktemp); trap 'rm -f "$$digest_file"' EXIT; \
	if $(CONTAINER_ENGINE) push --digestfile "$$digest_file" $(ATE_ANT_IMAGE):latest; then \
		digest=$$(cat "$$digest_file" 2>/dev/null); \
	elif $(CONTAINER_ENGINE) push $(ATE_ANT_IMAGE):latest; then \
		digest=$$($(CONTAINER_ENGINE) inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' $(ATE_ANT_IMAGE):latest 2>/dev/null | \
			grep '^$(ATE_ANT_IMAGE)@' | head -n1 | sed 's/.*@//'); \
	else \
		echo "Error: Failed to push image to $(ATE_ANT_IMAGE)" >&2; \
		exit 1; \
	fi; \
	if [ -z "$$digest" ]; then \
		echo "Error: Could not determine the digest of $(ATE_ANT_IMAGE):latest" >&2; \
		exit 1; \
	fi; \
	mkdir -p bin; \
	echo "$(ATE_ANT_IMAGE)@$$digest" > bin/worker-image; \
	echo "export WORKER_IMAGE=$(ATE_ANT_IMAGE)@$$digest"

# Build the Agent Substrate gVisor worker image from a Substrate checkout
# matching go.mod; see hack/push-ateom-image.sh.
push-ateom-image:
	@mkdir -p bin
	@GOOGLE_CLOUD_PROJECT=$(GOOGLE_CLOUD_PROJECT) PLATFORM=$(PLATFORM) ATE_IMAGES_REPO=$(ATE_IMAGES_REPO) \
		SUBSTRATE_REPO=$(SUBSTRATE_REPO) SUBSTRATE_DIR=$(SUBSTRATE_DIR) \
		hack/push-ateom-image.sh > bin/ateom-image.tmp
	@mv bin/ateom-image.tmp bin/ateom-image
	@echo "export ATEOM_IMAGE=$$(cat bin/ateom-image)"

clean:
	rm -rf bin
