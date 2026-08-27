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
  --ateom-image       gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateom-gvisor@sha256:9992059c51c9af88a3489104c1c792644f4c28d22073af7bdbed07c1f2c7d1f1 \
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

## 6. Explore Valkey

```bash
kubectl exec -n ate-ant valkey-0 -- valkey-cli xinfo groups stream:events
```
