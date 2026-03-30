# lecrev

`lecrev` is a Firecracker-first serverless compute platform scaffold. This repo implements:

- a control-plane HTTP API for deploy and invoke flows
- a regional coordinator to host control stream over gRPC
- a node-agent that executes assignments through a pluggable driver
- lease expiry recovery and retry orchestration in the control plane
- a PostgreSQL-backed metadata store with embedded migrations
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

Poll the build job or function version until it becomes ready:

```bash
curl -sS http://localhost:8080/v1/build-jobs/<build-job-id> \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/functions/<version-id> \
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

Inspect the live region and host inventory:

```bash
curl -sS http://localhost:8080/v1/regions \
  -H 'X-API-Key: dev-root-key'

curl -sS http://localhost:8080/v1/regions/ap-south-1/hosts \
  -H 'X-API-Key: dev-root-key'
```

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

This repo intentionally keeps the Firecracker boundary thin. The local `node` driver is only for developer machines; Linux execution hosts should swap in a real Firecracker-backed driver behind the same interface.

The default execution topology is APAC-only. Unsupported regions such as `us-east-1` are rejected at deploy time to keep the platform aligned with an `ap-south-1` primary and nearby failover regions.

The current production-oriented pieces in the repo are the metadata model, async build-job flow, retry and lease-recovery flow, idempotent deploy and invoke APIs, webhook triggers, per-job attempt inspection, cost-record generation, warm-pool inventory, host drain control, a scoped secrets proxy in front of the backend secrets provider, Postgres migrations, optional NATS and S3 or MinIO adapters, and mTLS for the coordinator-to-node-agent stream in local mode. The remaining major gap to full production is replacing the local Node execution driver with a Linux Firecracker host driver and adding the real AWS integrations around artifact replication, autoscaling, and host automation.
