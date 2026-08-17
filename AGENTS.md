# Redeploying Claude Managed Agents on Agent Substrate

Redeploying takes a long time, only start doing this if user explicitly
asks for you to redeploy.
Follow these steps to build, update, and redeploy the worker stack:

## 1. Delete Existing Namespace

First delete everything but not the running actors.

## 2. Build and Push Container Image

Build the container image with the updated binaries and push to Google Container Registry (GCR):

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

Note the image digest emitted by `make push` (e.g. `gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant@sha256:...`).

## 3. Update README

Update the `--worker-image` flag in [`README.md`](file:///Users/jbd/substrate-anthropic-worker/README.md) with the new digest-pinned image tag from step 2.

## 4. Generate and Apply Manifests

Generate the Kubernetes and Substrate manifests using `ate-ant manifest` and apply them to the cluster:

```bash
go run ./cmd/ate-ant manifest \
  --worker-image      gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant@sha256:... \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/ate-ant/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

## 5. Verify Pods

Check that all worker pool and poller pods are running:

```bash
kubectl get pods -n ate-ant
```

See the logs for more:

```bash
kubectl logs -n ate-ant -l app=ate-ant --all-containers --tail=50
```