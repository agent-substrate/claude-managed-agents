# Redeploying Claude Managed Agents on Agent Substrate

Redeploying takes a long time, only start doing this if user explicitly
asks for you to redeploy.
Follow these steps to build, update, and redeploy the worker stack:

## 1. Delete Existing Namespace

Tear down the existing deployment and resources:

```bash
kubectl delete ns ate-antworker
```

Wait for the namespace to finish terminating before proceeding.

## 2. Build and Push Container Image

Build the container image with the updated binaries and push to Google Container Registry (GCR):

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

Note the image digest emitted by `make push` (e.g. `gcr.io/$GOOGLE_CLOUD_PROJECT/ate-antworker@sha256:...`).

## 3. Update README

Update the `--worker-image` flag in [`README.md`](file:///Users/jbd/substrate-anthropic-worker/README.md) with the new digest-pinned image tag from step 2.

## 4. Generate and Apply Manifests

Generate the Kubernetes and Substrate manifests using `ate-antworker manifest` and apply them to the cluster:

```bash
go run ./cmd/ate-antworker manifest \
  --worker-image      gcr.io/$GOOGLE_CLOUD_PROJECT/ate-antworker@sha256:... \
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket  gs://$GOOGLE_CLOUD_PROJECT/ate-antworker/ \
  --anthropic-api-key "$ANTHROPIC_API_KEY" \
  --anthropic-env-id  "$ANTHROPIC_ENVIRONMENT_ID" \
  --anthropic-env-key "$ANTHROPIC_ENVIRONMENT_KEY" | kubectl apply -f -
```

## 5. Verify Pods

Check that all worker pool and poller pods are running:

```bash
kubectl get pods -n ate-antworker
```

See the logs for more:

```bash
kubectl logs -n ate-antworker -l app=ate-antworker --all-containers --tail=50
```