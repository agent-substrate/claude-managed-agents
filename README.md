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

- **Controller** (`ate-ant controller`): horizontally scalable. Each replica polls the Anthropic API for work and enqueues it in Valkey, then consumes lifecycle events from the queue (shared across replicas through a consumer group), creates, resumes, and suspends Substrate actors, and delivers work to actors through `atenet-router`.
- **Queue**: an in-cluster Valkey instance that buffers session lifecycle events, decoupling ingestion from execution.
- **Actors** (`ate-ant run`): one isolated sandbox per agent session. Each runs the Anthropic SDK's environment worker in-process to execute tools, and keeps session state across turns through snapshots.
- **Environment key injection**: actors never hold the Anthropic environment key. They send a placeholder bearer token, and the Substrate egress gateway replaces it with `ANTHROPIC_ENVIRONMENT_KEY` from the `ate-ant-creds` Secret on requests to the Anthropic API.

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
4. Podman (or Docker) to build the container image.

### 1. Build and Push the Image

```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project"
make push
```

`make push` prints the digest-pinned image. Use it as `--worker-image` in the steps below in place of the pinned digest.

### 2. Apply the Kubernetes Manifests

`--worker-image` is the `ate-ant` image from step 1, used by the controller and the actors. `--ateom-image` is the Agent Substrate gVisor worker that the WorkerPool runs.

```bash
go run ./cmd/ate-ant manifest \
  --worker-image gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant@sha256:2f5b931584f80ccbd72851058a597af39e706eeaa20dfd86a7bc2fcd7410b416 \
  --ateom-image  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-images/ateworker-gvisor@sha256:c97cf56ec19ef37895a0e7cc5c6dada677cb3824c5e93fcf7f142053cfe19a85 | kubectl apply -f -
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
  --worker-image     gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant@sha256:2f5b931584f80ccbd72851058a597af39e706eeaa20dfd86a7bc2fcd7410b416 \
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

## Running Locally

To shell into the container locally with your Anthropic environment variables:

```bash
podman run -it --rm \
  --entrypoint bash \
  -e ANTHROPIC_ENVIRONMENT_ID="$ANTHROPIC_ENVIRONMENT_ID" \
  -e ANTHROPIC_ENVIRONMENT_KEY="$ANTHROPIC_ENVIRONMENT_KEY" \
  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant:latest
```

Inside the container, run a worker manually with `ant beta:worker run --work-id "<work-id>" --session-id "<session-id>"`.
