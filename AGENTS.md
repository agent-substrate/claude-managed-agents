# Redeploying Claude Managed Agents on Agent Substrate

Redeploying takes a long time, only start doing this if user explicitly
asks for you to redeploy.
Follow these steps to build, update, and redeploy the worker stack:

## 1. Cleanup

First, cleanup:

```bash
kubectl delete deployments,statefulsets,workerpools,services,secrets --all -n ate-ant
```

The `ActorTemplate` lives in the Substrate control plane, not in Kubernetes;
delete it with `kubectl-ate delete actor-template ate-ant-actor-template --atespace=ate-ant`.

## 2. Build and Push Container Images

Build the `ate-ant` image and the Agent Substrate gVisor worker image
(`ateworker-gvisor`, built with `ko` from the `go.ate.dev/substrate` version
pinned in `go.mod`), push them to Google Container Registry (GCR), and update
the digests in `benchmarks/install.sh`:

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
./benchmarks/build-images.sh
```

It ends by printing `WORKER_IMAGE` and `ATEOM_IMAGE`; export them before the
next step. `make push-images` builds and pushes both images without touching
`install.sh`, and prints the `export` commands. To rebuild only `ate-ant`, run
`make push-worker-image` (or `BUILD_SUBSTRATE_IMAGES=false ./benchmarks/build-images.sh`).

## 3. Generate and Apply Manifests

Generate the Kubernetes manifests using `ate-ant manifest` and apply them to the cluster:

```bash
go run ./cmd/ate-ant manifest \
  --worker-image "$WORKER_IMAGE" \
  --ateom-image  "$ATEOM_IMAGE" | kubectl apply -f -
```

Register the `ActorTemplate` in Substrate control plane using `ate-ant manifest template`.
Use a `kubectl-ate` built from the same Substrate version as the cluster
(`go install ./cmd/kubectl-ate` in the Substrate checkout); `--preview` is
required while the `PerTemplateSnapshotLocation` preview gate is enabled:

```bash
go run ./cmd/ate-ant manifest template \
  --worker-image     "$WORKER_IMAGE" \
  --snapshots-bucket gs://$GOOGLE_CLOUD_PROJECT/ate-ant/ | kubectl-ate create actor-template --preview -f -
```

For the benchmark cluster, `INSTALL_SUBSTRATE=false ./benchmarks/install.sh`
performs steps 3 and 4 using the digests pinned in the script.

## 4. Verify Pods

Check that all worker pool and controller pods are running:

```bash
kubectl get pods -n ate-ant
```

See the logs for more:

```bash
kubectl logs -n ate-ant -l app=ate-ant-controller --all-containers --tail=50
```

## 5. Explore Valkey

```bash
kubectl exec -n ate-ant valkey-0 -- valkey-cli xinfo groups stream:events
```

## Manifest Reference

| Resource | Kind | Description |
| --- | --- | --- |
| `ate-ant` | `Namespace` | Target Kubernetes namespace |
| `ate-ant-creds` | `Secret` | Stores Anthropic credentials and Valkey connection string |
| `k8s-credential-provider-namespace-policy` | `ConfigMap` (in `ate-system`) | Grants the `ate-ant` atespace the labeled `ate-ant-creds` Secret for egress environment key injection (restart `k8s-credential-provider` after applying) |
| `ate-ant-creds-reader` | `Role` / `RoleBinding` | RBAC allowing `ate-api-server` to resolve credentials for actor resumption |
| `valkey` | `Service` & `StatefulSet` | Single-replica Valkey instance backing the session lifecycle event queue |
| `ate-ant-workerpool` | `WorkerPool` (`ate.dev/v1alpha1`) | Pool of gVisor/microVM runners for sandbox actors (10 replicas) |
| `ate-ant-actor-template` | `ActorTemplate` (Substrate control-plane proto) | Sandbox container definition (`ate-ant run`) and GCS snapshot location |
| `ate-ant-controller` | `Deployment` | Unified daemon polling Anthropic for work and dispatching lifecycle events to Substrate actors |

