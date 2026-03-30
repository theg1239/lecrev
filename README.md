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

## Local Development

Optional local dependencies:

```bash
docker compose -f deploy/local/docker-compose.yml up -d
```

Start the whole stack:

```bash
go run ./cmd/lecrev devstack
```

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
export LECREV_ENABLE_MTLS='true'
go run ./cmd/lecrev devstack
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

The current production-oriented pieces in the repo are the metadata model, retry and lease-recovery flow, idempotent deploy and invoke APIs, webhook triggers, per-job attempt inspection, cost-record generation, warm-pool inventory, host drain control, Postgres migrations, optional NATS and S3 or MinIO adapters, and mTLS for the coordinator-to-node-agent stream in local mode. The remaining major gap to full production is replacing the local Node execution driver with a Linux Firecracker host driver and adding the real AWS integrations around Secrets Manager, object replication, autoscaling, and host automation.
