# 50-Node / 5,000-Actor Benchmark Suite

This directory contains the cluster provisioning scripts, deployment installer, and multi-mode Go load generator to benchmark **Claude Managed Agents on Agent Substrate** at a scale of **50 GKE worker nodes** in `us-west1-a` serving **5,000 agent session actors**.

## Architecture & Sizing

| Component | Default Sizing | Description |
| --- | --- | --- |
| **System Node Pool** (`default-pool`) | `3 x e2-standard-4` | Hosts `ate-system` control plane, `valkey`, `ate-ant-controller` replicas, and the in-cluster load generator pod |
| **gVisor Worker Pool** (`gvisor-pool`) | `50 x e2-standard-2` | Hosts the pre-warmed Agent Substrate sandbox runners in `us-west1-a` |
| **`WorkerPool` CRD** (`ate-ant-workerpool`) | `50` warm replicas | Pre-warmed gVisor sandboxes (1 per worker node) multiplexed across 5,000 total actors via GCS snapshot suspend/resume |
| **Controller Deployment** (`ate-ant-controller`) | `20` replicas | Consumes lifecycle events from `stream:events` in parallel and dispatches `/work` notifications |
| **Total Actor Sessions** | `5,000` actors | Multiplexed over the warm `WorkerPool` via checkpoint/restore to `gs://<project>-ate-ant-snapshots/` |

---

## 1. Create the 50-Node Cluster

Run [`cluster-create.sh`](cluster-create.sh) to provision the GKE cluster in `us-west1-a` with `/14` pod CIDR, `/18` service CIDR, Workload Identity, GCS snapshot bucket, and the 50-node gVisor node pool:

```bash
export GOOGLE_CLOUD_PROJECT="kubecon-na-26-keynote-demo"
export CLUSTER_NAME="ate-ant-bench"
export REGION="us-west1"
export ZONE="us-west1-a"

./benchmarks/cluster-create.sh
```

---

## 2. Rebuild Images & Install Stack

To rebuild both the `ate-ant` image and the Agent Substrate gVisor worker image (`ateworker-gvisor`, built with `ko` from the `go.ate.dev/substrate` version pinned in `go.mod`) and automatically update [`install.sh`](install.sh) and [`../README.md`](../README.md) with the new `@sha256:...` digests, run [`build-images.sh`](build-images.sh):

```bash
export GOOGLE_CLOUD_PROJECT="kubecon-na-26-keynote-demo"

# Rebuild ate-ant + Agent Substrate images and update install.sh with the new sha256 digests
./benchmarks/build-images.sh

# Or skip rebuilding the Agent Substrate worker image and only rebuild ate-ant
BUILD_SUBSTRATE_IMAGES=false ./benchmarks/build-images.sh
```

Then run [`install.sh`](install.sh) to install Agent Substrate, deploy `valkey`, `ate-ant-workerpool` (`50` warm pods), and `ate-ant-controller` (`20` replicas), and register the `ActorTemplate`:

```bash
export GOOGLE_CLOUD_PROJECT="kubecon-na-26-keynote-demo"
export ANTHROPIC_ENVIRONMENT_ID="env_..."
export ANTHROPIC_ENVIRONMENT_KEY="ek_..."
export ANTHROPIC_API_KEY="sk-ant-..."

./benchmarks/install.sh
```

---

## 3. Run the Load Generator

The Go load generator ([`benchmarks/loadgen`](loadgen/main.go)) supports four modes:

1. **`valkey`**: Enqueues `resume` (and optional `suspend`) events for 5,000 sessions directly into Valkey `stream:events` and waits for the `ate-ant-controller` consumer group to drain the stream.
2. **`direct`**: Connects directly to Substrate `ateapi` and `atenet-router` to benchmark cold actor creation (`Turn 1`), `POST /work` notification, actor suspension (`SuspendActor`), and warm snapshot restoration (`Turn 2+`) across 5,000 actors multiplexed over the warm `WorkerPool`.
3. **`anthropic`**: End-to-end benchmark using the Anthropic SDK (`Beta.Agents.New`, `Beta.Sessions.New`, `Beta.Sessions.Events.Send`) across 5,000 sessions (supports custom `ANTHROPIC_BASE_URL`).
4. **`both`**: Runs `valkey` queue injection followed by `anthropic` API session load.

### Run In-Cluster (Recommended for `valkey` and `direct` modes)

[`run-loadgen.sh`](run-loadgen.sh) cross-compiles `loadgen`, launches a temporary runner pod inside the `ate-ant` namespace with the required `ateapi-token` service account projection and credentials secret, and streams the benchmark report:

```bash
# 1. Direct Substrate lifecycle benchmark: 5k sessions x 2 turns (cold create + suspend + warm restore)
MODE=direct SESSIONS=5000 CONCURRENCY=100 TURNS_PER_SESSION=2 JSON_OUT=direct-5k.json \
  ./benchmarks/run-loadgen.sh

# 2. Valkey queue + controller pipeline benchmark: 5k sessions with full stream drain check
MODE=valkey SESSIONS=5000 CONCURRENCY=100 WAIT_DRAIN=true JSON_OUT=valkey-5k.json \
  ./benchmarks/run-loadgen.sh

# 3. End-to-end Anthropic Sessions API benchmark
MODE=anthropic SESSIONS=5000 CONCURRENCY=100 TURNS_PER_SESSION=1 JSON_OUT=anthropic-5k.json \
  ./benchmarks/run-loadgen.sh
```

### Run Locally

You can also invoke `loadgen` directly with `go run`:

```bash
go run ./benchmarks/loadgen \
  --mode=anthropic \
  --sessions=5000 \
  --concurrency=100 \
  --turns-per-session=2 \
  --turn-interval=35s \
  --json-out=report.json
```

---

## 4. Live In-Cluster Benchmark Dashboard

[`run-dashboard.sh`](run-dashboard.sh) builds and deploys the live web dashboard ([`benchmarks/dashboard`](dashboard/main.go)) inside the `ate-ant` namespace and exposes it via a `LoadBalancer` Service (`ate-ant-dashboard`):

```bash
./benchmarks/run-dashboard.sh
```

The dashboard polls the cluster in real time and displays:
- **Actively Running Actors**: Live breakdown of `RUNNING`, `RESUMING`, `SUSPENDING`, `SUSPENDED` (snapshot in GCS), total actors, and multiplexing ratio (`total actors / warm workers`).
- **Worker Pool & Controllers**: `ate-ant-workerpool` desired/ready/claimed/idle gVisor sandboxes, worker utilization %, and `ate-ant-controller` replica status.
- **Valkey Event Queue (`stream:events`)**: Stream length (`XLEN`), pending PEL entries (`XPENDING`), consumer group lag, total entries read, and active consumers.
- **Real-Time Time-Series Chart & Actor Table**: Rolling 60-sample canvas chart (`Running`, `Resuming+Suspending`, `Queue Lag`) and a filterable table of session actors with state badges, assigned worker pod/IP, snapshot version, and age.

---

## 5. Teardown

To delete the 50-node GKE cluster (and optionally purge the GCS snapshot bucket):

```bash
export GOOGLE_CLOUD_PROJECT="kubecon-na-26-keynote-demo"
PURGE_SNAPSHOTS=true ./benchmarks/cluster-delete.sh
```

