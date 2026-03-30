# lecrev

`lecrev` is a Firecracker-first serverless compute platform scaffold. This repo implements:

- a control-plane HTTP API for deploy and invoke flows
- a regional coordinator to host control stream over gRPC
- a node-agent that executes assignments through a pluggable driver
- lease expiry recovery and retry orchestration in the control plane
- a PostgreSQL-backed metadata store with embedded migrations
- a Linux Firecracker execution driver that boots microVMs and talks to a guest runner over vsock
- a Linux guest-runner binary for Firecracker rootfs images
- a local Node.js execution driver that simulates the Firecracker guest path on macOS
- an in-memory metadata backend for fast local iteration

## Architecture Diagram

```text
Users / CI / SDK
        |
        v
Public API (Deploy / Invoke / Status / Webhook)
        |
        v
+-------------------------------------------------------------+
| Control Plane - Primary Region (active-passive writes)      |
|  - Auth + API Keys                                           |
|  - Global Scheduler -----------------------------------+     |
|  - Build Controller                                    |     |
|  - Secrets Proxy ---------------------------+          |     |
|  - Postgres Writer <------------------------+----------+     |
+-------------------------------------------------------------+
                 |
                 +--> cross-region replica --> Postgres Read Replica (Secondary)
                 |
                 +--> JetStream Region A -------------------------------+
                 |                                                      |
                 |                                                      v
                 |                                      +------------------------------+
                 |                                      | Build Plane - Region A       |
                 |                                      |  Build Workers (Firecracker) |
                 |                                      |        |                     |
                 |                                      |        v                     |
                 |                                      |  S3 Artifacts A             |
                 |                                      +------------------------------+
                 |
                 +--> JetStream Region B -------------------------------+
                                                                        |
                                                                        v
                                                      +------------------------------+
                                                      | Build Plane - Region B       |
                                                      |  Build Workers (Firecracker) |
                                                      |        |                     |
                                                      |        v                     |
                                                      |  S3 Artifacts B             |
                                                      +------------------------------+

S3 Artifacts A <---------------- artifact replication ----------------> S3 Artifacts B

+-----------------------------------+     +-----------------------------------+
| Execution Plane - Region A        |     | Execution Plane - Region B        |
| JetStream A -> Coordinator A      |     | JetStream B -> Coordinator B      |
| Coordinator <-> Node Agents (mTLS)|     | Coordinator <-> Node Agents (mTLS)|
| Node Agents -> Firecracker Hosts  |     | Node Agents -> Firecracker Hosts  |
| Hosts -> MicroVMs / Snapshots     |     | Hosts -> MicroVMs / Snapshots     |
+-----------------------------------+     +-----------------------------------+

Secrets Proxy -> Coordinators (A/B)
S3 Artifacts (A/B) -> Node Agents (A/B)
MicroVMs -> Results / Logs in S3
Node Agents -> Prometheus + Loki
Coordinators -> Attempt Leases + Heartbeats + TTL -> Postgres Writer

Platform semantics:
- At-least-once delivery via JetStream, idempotency enforced at API layer
- No SSH to hosts; node-agents keep long-lived outbound control channels
- Dedicated host pools for build and execution
```

If your markdown renderer supports Mermaid, the source diagram is in [PLANNING.md](PLANNING.md).

For a clear step-by-step lifecycle walkthrough (deploy -> build -> invoke -> execution -> retries), see [FUNCTION_LIFECYCLE.md](FUNCTION_LIFECYCLE.md).

## Local Development

Optional local dependencies:

```bash
docker compose -f deploy/local/docker-compose.yml up -d
```

Start the whole stack:

```bash
go run ./cmd/lecrev devstack
```

Run an end-to-end smoke check without binding any local ports:

```bash
go run ./cmd/lecrev smoke
```

This starts an embedded in-process control plane, coordinators, and node-agents, then runs a real deploy -> invoke -> poll -> inspect flow and prints the result as JSON.

Deploys are asynchronous. The create-function API returns a function version with a `buildJobId`, and callers should wait for that build job or function version to become ready before invoking it.

To run the control plane against Postgres instead of the in-memory store:

```bash
export LECREV_POSTGRES_DSN='postgres://lecrev:lecrev@localhost:5432/lecrev?sslmode=disable'
go run ./cmd/lecrev devstack
```

To run the split production-shaped topology instead of the one-box `devstack`:

```bash
# control-plane host
export LECREV_API_ADDR=':8080'
export LECREV_PUBLIC_BASE_URL='https://functions.example.com'
export LECREV_COORDINATOR_BIND_HOST='0.0.0.0'
go run ./cmd/lecrev control-plane

# execution host
export LECREV_NODE_AGENT_REGION='ap-south-1'
export LECREV_NODE_AGENT_HOST_ID='host-ap-south-1-a'
export LECREV_COORDINATOR_ADDR='10.0.0.10:9091'
export LECREV_CONTROL_PLANE_BASE_URL='http://10.0.0.10:8080'
go run ./cmd/lecrev node-agent
```

In the split deployment, the execution host no longer needs direct Postgres access. The coordinator now sends a self-contained execution assignment that includes the artifact bundle key and runtime sizing data, and the node-agent pulls bundles from object storage plus scoped secrets from the control-plane secrets proxy.

Optional local infrastructure adapters:

```bash
export LECREV_NATS_URL='nats://localhost:4222'
export LECREV_S3_REGION='ap-south-1'
export LECREV_S3_ENDPOINT='http://localhost:9000'
export LECREV_S3_ACCESS_KEY='minioadmin'
export LECREV_S3_SECRET_KEY='minioadmin'
export LECREV_S3_BUCKET='lecrev-artifacts'
export LECREV_SECRETS_BACKEND='memory'
export LECREV_SECRETS_PROXY_TOKEN='dev-secrets-token'
export LECREV_ENABLE_MTLS='true'
go run ./cmd/lecrev devstack
```

Run the real Firecracker driver on a Linux execution host:

```bash
go build -o ./dist/lecrev-guest-runner ./cmd/lecrev-guest-runner

export LECREV_EXECUTION_DRIVER='firecracker'
export LECREV_EXECUTION_HOST_SLOTS='1'
export LECREV_FIRECRACKER_BINARY='/usr/local/bin/firecracker'
export LECREV_JAILER_BINARY='/usr/local/bin/jailer'
export LECREV_FIRECRACKER_USE_JAILER='true'
export LECREV_FIRECRACKER_JAILER_USER='lecrev'
export LECREV_FIRECRACKER_KERNEL_IMAGE='/var/lib/lecrev/vmlinux'
export LECREV_FIRECRACKER_ROOTFS='/var/lib/lecrev/rootfs.ext4'
export LECREV_FIRECRACKER_WORKSPACE_DIR='/var/lib/lecrev/runtime'
export LECREV_FIRECRACKER_SNAPSHOT_DIR='/var/lib/lecrev/runtime/snapshots'
export LECREV_FIRECRACKER_GUEST_INIT='/usr/local/bin/lecrev-guest-runner'
go run ./cmd/lecrev node-agent
```

On EC2, use the deployment helpers instead of hand-building the guest image:

```bash
bash deploy/ec2/install-firecracker-host.sh
bash deploy/ec2/build-firecracker-rootfs.sh
bash deploy/ec2/check-firecracker-host.sh
```

The Linux Firecracker path now keeps a host-local snapshot cache under `LECREV_FIRECRACKER_SNAPSHOT_DIR`. It boots a clean guest runner as PID 1, creates a blank host-local snapshot for prep work, proactively builds per-function snapshots for snapshot-safe workloads once versions become ready, and later invocations restore those snapshots directly when warm capacity exists.

For `networkPolicy=full`, configure a host tap device and guest network tuple as well:

```bash
bash deploy/ec2/configure-firecracker-network.sh

export LECREV_FIRECRACKER_TAP_DEVICE='tap0'
export LECREV_FIRECRACKER_GUEST_MAC='06:00:ac:10:00:02'
export LECREV_FIRECRACKER_GUEST_IP='172.16.0.2'
export LECREV_FIRECRACKER_GATEWAY_IP='172.16.0.1'
export LECREV_FIRECRACKER_NETMASK='255.255.255.252'
```

The current EC2 helper uses a single static tap device, so keep `LECREV_EXECUTION_HOST_SLOTS=1` when you need outbound guest networking.

If you already have `devstack` running and want to smoke the live HTTP API instead of the embedded stack:

```bash
go run ./cmd/lecrev smoke --api http://localhost:8080
```

This boots:

- an HTTP API on `:8080`
- an `ap-south-1` coordinator on `:9091`
- an `ap-south-2` coordinator on `:9092`
- an `ap-southeast-1` coordinator on `:9093`
- one node-agent per region

Deploy an inline bundle:

```bash
curl -sS http://localhost:8080/v1/projects/demo/functions \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d @- <<'JSON'
{
  "name": "echo",
  "environment": "staging",
  "runtime": "node22",
  "entrypoint": "index.mjs",
  "memoryMb": 128,
  "timeoutSec": 10,
  "networkPolicy": "full",
  "regions": ["ap-south-1", "ap-south-2", "ap-southeast-1"],
  "envRefs": ["DEMO_SECRET"],
  "maxRetries": 2,
  "source": {
    "type": "bundle",
    "inlineFiles": {
      "index.mjs": "export async function handler(event, context) { return { ok: true, echo: event, region: context.region, hostId: context.hostId, secret: process.env.DEMO_SECRET ?? null }; }"
    }
  }
}
JSON
```

Deploy from a git repository:

```bash
curl -sS http://localhost:8080/v1/projects/demo/functions \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d @- <<'JSON'
{
  "name": "git-echo",
  "environment": "production",
  "runtime": "node22",
  "entrypoint": "dist/index.mjs",
  "memoryMb": 128,
  "timeoutSec": 10,
  "networkPolicy": "full",
  "regions": ["ap-south-1", "ap-south-2", "ap-southeast-1"],
  "maxRetries": 2,
  "source": {
    "type": "git",
    "gitUrl": "file:///absolute/path/to/repo",
    "gitRef": "main",
    "subPath": "."
  }
}
JSON
```

The current git build path is npm-based: the build worker clones the repository, installs dependencies, runs `npm run build` when a build script is present, prunes dev dependencies, verifies the declared entrypoint exists, then packages the resulting workspace into the immutable execution artifact.

Current deploy admission caps in the implementation:

- runtime: `node22` only
- network policy: `none` and `full` only (`allowlist` is reserved but rejected in v1)
- memory: `64` to `1024` MB
- timeout: `1` to `300` seconds
- retries: `0` to `5`
- env refs: up to `64`
- archived execution artifact size: up to `10 MiB`
- execution logs: up to `1 MiB`
- execution output payload: up to `1 MiB`
- active build jobs per project: up to `5`
- active execution jobs per project: up to `50`

Poll the build job or function version until it becomes ready:

```bash
curl -sS http://localhost:8080/v1/build-jobs/<build-job-id> \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/functions/<version-id> \
  -H 'X-API-Key: dev-root-key'
```

Fetch archived build logs:

```bash
curl -sS http://localhost:8080/v1/build-jobs/<build-job-id>/logs \
  -H 'X-API-Key: dev-root-key'
```

Invoke the returned function version:

```bash
curl -sS http://localhost:8080/v1/functions/<version-id>/invoke \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d '{"payload":{"hello":"world"}}'
```

The invoke path also accepts API-level idempotency through either the `Idempotency-Key` header or `idempotencyKey` field in the JSON body.

Fetch job status:

```bash
curl -sS http://localhost:8080/v1/jobs/<job-id> \
  -H 'X-API-Key: dev-root-key'
```

Terminal job results include inline `logs` and `output` for quick inspection plus deterministic `logsKey` and `outputKey` fields pointing at the archived raw execution artifacts in object storage.

Fetch the archived execution artifacts through the public API:

```bash
curl -sS http://localhost:8080/v1/jobs/<job-id>/logs \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/jobs/<job-id>/output \
  -H 'X-API-Key: dev-root-key'
```

Inspect the live region and host inventory:

```bash
curl -sS http://localhost:8080/v1/regions \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/regions/ap-south-1/hosts \
  -H 'X-API-Key: dev-root-key'
```

`GET /v1/regions` is tenant-accessible so browser dashboards can discover supported execution regions. Host, warm-pool, and drain endpoints remain admin-only.

Inspect project-scoped dashboard views:

```bash
curl -sS 'http://localhost:8080/v1/projects?limit=20' \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/projects/demo \
  -H 'X-API-Key: dev-root-key'

curl -sS 'http://localhost:8080/v1/projects/demo/overview?limit=8' \
  -H 'X-API-Key: dev-root-key'

curl -sS 'http://localhost:8080/v1/deployments?limit=20' \
  -H 'X-API-Key: dev-root-key'

curl -sS 'http://localhost:8080/v1/projects/demo/deployments?status=ready&environment=staging' \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/deployments/<deployment-id> \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/deployments/<deployment-id>/logs \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/deployments/<deployment-id>/output \
  -H 'X-API-Key: dev-root-key'

curl -sS 'http://localhost:8080/v1/projects/demo/functions?limit=20' \
  -H 'X-API-Key: dev-root-key'

curl -sS 'http://localhost:8080/v1/projects/demo/build-jobs?limit=20' \
  -H 'X-API-Key: dev-root-key'

curl -sS 'http://localhost:8080/v1/projects/demo/jobs?limit=20' \
  -H 'X-API-Key: dev-root-key'
```

These endpoints are intended for a frontend dashboard. They return compact project, deployment, function, build-job, and execution-job summaries rather than the full archived artifacts. For git-backed builds, deployment summaries now include persisted `branch`, `gitUrl`, and resolved `commitSha` metadata captured during the build.

Inspect per-job attempts and cost records:

```bash
curl -sS http://localhost:8080/v1/jobs/<job-id>/attempts \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/jobs/<job-id>/costs \
  -H 'X-API-Key: dev-root-key'
```

Inspect warm-pool availability and drain a host:

```bash
curl -sS http://localhost:8080/v1/regions/ap-south-1/warm-pools \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/regions/ap-south-1/hosts/host-ap-south-1-a/drain \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d '{"reason":"maintenance"}'
```

Create and use a webhook trigger:

```bash
curl -sS http://localhost:8080/v1/functions/<version-id>/triggers/webhook \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d '{"description":"github push"}'

curl -sS http://localhost:8080/v1/triggers/webhook/<token> \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: delivery-123' \
  -d '{"event":"push","repository":"lecrev"}'
```

Webhook deliveries are public token-authenticated endpoints. Management operations for creating and listing triggers stay behind the normal API key middleware.

Create and use a Lambda-style Function URL:

```bash
curl -sS http://localhost:8080/v1/functions/<version-id>/triggers/http \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d '{"description":"public http endpoint","authMode":"none"}'

curl -sS http://localhost:8080/f/<token> \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: request-123' \
  -d '{"hello":"world"}'

curl -sS http://localhost:8080/v1/functions/<version-id>/triggers/http \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: dev-root-key' \
  -d '{"description":"private http endpoint","authMode":"api_key"}'

curl -sS http://localhost:8080/f/<token> \
  -H 'Authorization: Bearer dev-root-key' \
  -H 'Idempotency-Key: request-456'
```

The Function URL path waits for terminal job completion and returns either:

- raw JSON output as `200 application/json`, or
- a structured HTTP response when the function returns `{ "statusCode": ..., "headers": {...}, "body": ... }`

`authMode` values:

- `none`: public function URL
- `api_key`: caller must send `X-API-Key` or `Authorization: Bearer <api-key>` and be authorized for the owning project

## Browser Clients

The HTTP API now emits permissive CORS headers for browser-based frontends. Preflight `OPTIONS` requests are handled on the API routes, and the control plane allows `Content-Type`, `X-API-Key`, `Idempotency-Key`, and `Authorization` headers so a separate Vite or Next.js frontend can call the backend directly during development.

## CLI

The repo currently exposes two commands:

```bash
go run ./cmd/lecrev devstack
go run ./cmd/lecrev smoke
```

`smoke` accepts:

```bash
go run ./cmd/lecrev smoke --api http://localhost:8080 --api-key dev-root-key --project demo --regions ap-south-1,ap-south-2,ap-southeast-1
```

The smoke output now includes both the build job and the execution job so you can verify the full deploy -> build -> execute lifecycle in one run.

## Secrets Proxy

Node-agents no longer resolve secrets directly from the backend provider. They call an internal secrets-proxy path at `/v1/internal/secrets/resolve` with a bearer token from `LECREV_SECRETS_PROXY_TOKEN`.

The proxy validates that:

- the host exists in the claimed region
- the function version is allowed in that region
- the requested secret refs are declared in `envRefs` for that function version

Only after that does the control plane resolve the values from the configured backend such as in-memory secrets or AWS Secrets Manager.

## Protobuf

Generated Go code is produced with:

```bash
PATH="$PWD/.tools/bin:$PATH" \
  GOBIN="$PWD/.tools/bin" \
  go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6

PATH="$PWD/.tools/bin:$PATH" \
  GOBIN="$PWD/.tools/bin" \
  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

PATH="$PWD/.tools/bin:$PATH" \
  go run github.com/bufbuild/buf/cmd/buf@v1.56.0 generate
```

## Scope

This repo intentionally keeps the driver boundary thin. The local `node` driver is still the default for developer machines, but Linux execution hosts can now swap in the Firecracker-backed driver behind the same interface.

The default execution topology is APAC-only. Unsupported regions such as `us-east-1` are rejected at deploy time to keep the platform aligned with an `ap-south-1` primary and nearby failover regions.

The current production-oriented pieces in the repo are the metadata model, async build-job flow, retry and lease-recovery flow, idempotent deploy and invoke APIs, webhook triggers, per-job attempt inspection, cost-record generation, warm-pool inventory, host drain control, a scoped secrets proxy in front of the backend secrets provider, Postgres migrations, optional NATS and S3 or MinIO adapters, mTLS for the coordinator-to-node-agent stream in local mode, a Linux Firecracker host driver with function snapshot restore, and a guest-runner binary for Firecracker rootfs images. The remaining major gaps to full production are policy-aware blank-warm scheduling, artifact replication automation, autoscaling, and deeper AWS host automation.

For EC2 deployment details, see [deploy/ec2/README.md](deploy/ec2/README.md). For the split-host operational commands, including stop/start, redeploy, private worker access, and Function URL examples, see [deploy/ec2/RUNBOOK.md](deploy/ec2/RUNBOOK.md).
