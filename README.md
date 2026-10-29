# Claude Managed Agents on Agent Substrate

Run [Claude Managed Agents self-hosted sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes) work (tool calls such as bash, file operations, and skills) inside isolated [Agent Substrate](https://github.com/agent-substrate/substrate) gVisor/microVM actors.

> [!WARNING]
> We are actively hardening this integration. Expect improvements and some changes.

## Overview

```
┌───────────────────────┐  poll & ack  ┌──────────────────────┐
│ Claude Managed Agents │ ◄─────────── │ Controller           │
└───────────────────────┘              │ (ate-ant controller) │
            ▲                          └───┬────────────────▲─┘
            │                              │                │
            │          resume, POST /work, │                │ enqueue/consume
            │            suspend when idle │                ▼
            │                              │         ┌──────────────┐
            │                              │         │ Valkey queue │
            │                              │         └──────────────┘
            │                              ▼
            │                   ┌─────────────────────┐
            └────────────────── │ Actor (ate-ant run) │
              tool results,     └─────────────────────┘
              heartbeats
```

See [DESIGN.md](DESIGN.md) for the detailed architecture, event queuing, and actor lifecycle.

## Quickstart

### Prerequisites

1. An [Agent Substrate](https://github.com/agent-substrate/substrate) cluster installed with the Kubernetes Secrets credential provider and with the controller's `router-client` ServiceAccount allowed through the ingress router:

   ```bash
   ate-setup deploy ate-system \
     --credential-provider='{"name":"k8s.io"}' \
     --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-ant/sa/router-client \
     --snapshot-transfer-gsa-name=ate-snapshot-transfer \
     --preview=PerTemplateSnapshotLocation
   ```

   The `PerTemplateSnapshotLocation` preview gate lets the ActorTemplate carry its own snapshot bucket (step 5).

   On GKE, the snapshot transfer service account must exist with access to the snapshot bucket (`tools/setup-gcp create iam` in the Substrate repository, or [`benchmarks/install.sh`](benchmarks/install.sh), creates it).
2. Anthropic credentials: `ANTHROPIC_API_KEY`, `ANTHROPIC_ENVIRONMENT_ID`, and `ANTHROPIC_ENVIRONMENT_KEY` (see [Creating a Self-Hosted Environment](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes#create-an-environment)). To use a custom Anthropic API URL, also set `ANTHROPIC_BASE_URL` (or pass `--anthropic-base-url`).
3. A Google Cloud project (`GOOGLE_CLOUD_PROJECT`) and a GCS bucket for snapshots (`gs://$GOOGLE_CLOUD_PROJECT/ate-ant/`).
4. Podman (or Docker) to build the `ate-ant` image, and a clone of [Agent Substrate](https://github.com/agent-substrate/substrate) at `~/substrate` (or set `SUBSTRATE_REPO`) to build the gVisor worker image.

### 1. Build and Push the Images

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push-images
```

`make push-images` builds the `ate-ant` image and the Agent Substrate gVisor worker image, pushes them to `gcr.io/$GOOGLE_CLOUD_PROJECT`, and ends by printing `export` commands for `WORKER_IMAGE` and `ATEOM_IMAGE`. Run them before continuing.

### 2. Apply the Kubernetes Manifests

`--worker-image` is the `ate-ant` image, used by the controller and the actors. `--ateom-image` is the Agent Substrate gVisor worker that the WorkerPool runs.

```bash
go run ./cmd/ate-ant manifest \
  --worker-image "$WORKER_IMAGE" \
  --ateom-image  "$ATEOM_IMAGE" | kubectl apply -f -
```

### 3. Restart the Credential Provider

The manifests include the Kubernetes Secrets credential provider's policy, which grants the `ate-ant` atespace access to the `ate-ant-creds` Secret only. The provider reads its policy at startup:

```bash
kubectl -n ate-system rollout restart deployment/k8s-credential-provider
```

### 4. Grant the Controller Access to Substrate

Agent Substrate authorizes every API call. Create the atespace and give the controller's `router-client` ServiceAccount the `editor` role, which lets it create, resume, and suspend actors:

```bash
kubectl-ate create atespace ate-ant
kubectl-ate create access-policy -a ate-ant -f - <<EOF
bindings:
- role: editor
  members:
  - user:system:serviceaccount:ate-ant:router-client
EOF
```

### 5. Register the ActorTemplate

ActorTemplates live in the Substrate control plane, not in Kubernetes:

```bash
go run ./cmd/ate-ant manifest template \
  --worker-image     "$WORKER_IMAGE" \
  --snapshots-bucket gs://$GOOGLE_CLOUD_PROJECT/ate-ant/ | kubectl-ate create actor-template --preview -f -
```

### 6. Verify

```bash
kubectl get pods -n ate-ant
kubectl logs -n ate-ant -l app=ate-ant-controller -f
```

## Example Agent

```bash
export ANTHROPIC_API_KEY="sk-ant-api..."
export ANTHROPIC_ENVIRONMENT_ID="env_..."

go run ./examples/agent --prompt "Run 'uname -a' in bash and show the output."
```
