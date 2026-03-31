# Function Lifecycle and Data Flow

This document explains what happens in the platform from the moment a user uploads a function to final job completion.

It is written against the current implementation in this repo, not the original target architecture.

## 1) What the platform is split into

The system has three planes that work together:

- Control plane: API, auth, metadata, scheduling, and status tracking.
- Build plane: turns user source into an immutable artifact.
- Execution plane: runs async jobs in Firecracker microVMs in each region.

Design intent:

- Metadata writes are strongly consistent in one primary region.
- Build and execution are active-active across regions.
- Internal delivery is at-least-once, so API-level idempotency is required.

## 2) Main components involved

- Public API service.
- Postgres (system of record).
- NATS JetStream in each region (durable work queues).
- Global scheduler and regional coordinators.
- Dedicated build worker runtime.
- Node agents on Firecracker hosts.
- Artifact storage in S3 (replicated per target region).
- Secrets proxy + AWS Secrets Manager.

## 3) Deploy flow (user uploads a function)

Request:

- `POST /v1/projects/{project}/functions`
- Source can be `bundle` or `git`.
- Includes runtime, entrypoint, memory, timeout, network policy, target regions, env refs, retries, and idempotency key.

Step-by-step:

1. API authenticates key, resolves tenant/project, validates request.
2. API checks idempotency key. If duplicate, returns existing deploy result.
3. Admission checks run: quotas, limits, allowed runtime/policy.
4. Control-plane metadata is written in Postgres:
- function record
- function version record (initially pending/building)
- build job record (if build required)
5. Scheduler determines target build region.
6. Build work item is published to that region's JetStream stream.

## 4) Build flow (two paths)

### A) Git source deploy

1. Builder clones repo.
2. Detects the package manager (`npm`, `pnpm`, or `yarn`).
3. Installs dependencies on the build worker host.
4. Runs the package `build` script when present.
5. Emits immutable artifact set:
- `bundle.tgz`
- `function.json`
- `sha256`
- `startup.json`
6. Uploads artifact to S3.
7. Replicates artifact to target execution regions.
8. If the build produced a Next.js standalone website, writes a site manifest plus static assets into object storage.
9. Marks function version as ready.
10. Ensures a default public HTTP trigger exists for the ready version.
11. Queues best-effort function snapshot preparation in each target execution region when the network policy allows safe snapshot reuse.
12. Archives build logs for later inspection through `GET /v1/build-jobs/{buildJobId}/logs`.

### B) Bundle source deploy

1. Validates manifest and payload.
2. Runs unpack/smoke checks.
3. Computes digest.
4. Uploads to S3.
5. Replicates to target regions.
6. Marks function version as ready.
7. Ensures a default public HTTP trigger exists for the ready version.
8. Queues best-effort function snapshot preparation in each target execution region when the network policy allows safe snapshot reuse.
9. Archives build logs for later inspection through `GET /v1/build-jobs/{buildJobId}/logs`.

Build isolation notes:

- Build workers are separate from execution hosts.
- Git builds currently run package-manager commands directly on the build worker host.
- Firecracker-isolated build workers are still a planned improvement, not the current implementation.
- Only safe content-addressed caches should be reused.

## 5) Artifact contract (why this matters)

Each deploy becomes immutable and digest-addressed. Execution always uses digest, never mutable tags/branches.

Effects:

- Reproducible runs.
- Safe retries and regional failover.
- Better cache hit behavior on hosts.

## 6) Invoke flow (user runs function)

Request:

- `POST /v1/functions/{versionId}/invoke`
- `POST /v1/functions/{versionId}/triggers/http`
- `GET /v1/functions/{versionId}/triggers/http`
- Public Function URL: `/f/{token}`
- Website preview URL: `/w/{functionVersionId}`

Step-by-step:

1. API authenticates and validates invoke payload.
2. API applies idempotency and tenant quotas.
3. Creates `execution_job` row in Postgres (queued). 
4. Global scheduler picks best region using policy + health + capacity + queue age + cost + artifact locality.
5. Job is published to selected region's JetStream stream.

### Function URL flow

This is the Lambda Function URL equivalent in this platform.

1. Authenticated API caller creates an HTTP trigger for a ready function version.
2. Control plane generates a stable token and returns a public URL in the form `/f/{token}`.
3. Trigger auth mode decides whether the URL is public or requires a platform API key:
- `none`: public function URL
- `api_key`: caller must send `X-API-Key` or `Authorization: Bearer <api-key>`
4. External caller sends an HTTP request to that URL.
5. Control plane wraps the inbound request into an execution payload with method, path, query, headers, and body.
6. Control plane tries direct synchronous execution against live region capacity first.
7. If direct capacity is unavailable, control plane falls back to the queued execution-job path.
8. The Function URL handler maps the function output back to the caller:
- raw JSON output returns `200 application/json`
- structured output in the shape `{ statusCode, headers, body }` is emitted as an HTTP response directly
- large bodies can stream chunk-by-chunk from the function runtime to the caller
- `HEAD` requests suppress the response body while preserving headers and status
- final streamed-response latency is emitted as the `X-Lecrev-Latency-Ms` trailer

### Website preview flow

Website previews are distinct from Function URLs.

1. A website build writes a site manifest plus static assets into object storage.
2. The control plane exposes a preview route at `/w/{functionVersionId}`.
3. Static assets are served directly from object storage when the path matches a known asset.
4. Dynamic requests route into the function runtime through the same direct-or-queued execution model used by Function URLs.
5. Website previews return `503` while the site manifest or warm readiness is not yet available.

## 7) Regional assignment flow

1. Regional coordinator pulls job from JetStream.
2. Coordinator selects host based on:
- snapshot availability
- free CPU/RAM
- recent host failures
- artifact cache locality
3. Coordinator sends a self-contained assignment to the node agent over long-lived outbound mTLS gRPC stream.

Control model:

- Hosts dial coordinator; control plane does not SSH into hosts.
- Commands include assignment, snapshot prep, drain, terminate, and ack.
- Assignment includes the artifact bundle key plus runtime sizing data, so the execution host does not need direct metadata-database access just to run a job.

## 8) Lease and retry model

For every execution attempt:

1. Attempt row is created.
2. Lease with TTL is created.
3. Node agent heartbeats renew lease.
4. If heartbeat stops, lease expires.
5. Job becomes eligible for retry on another host/region.

This is the core recovery model for host/process failures.

## 9) On-host execution in Firecracker

Node agent manages full microVM lifecycle:

1. Picks startup mode:
- cold boot from base image
- warm restore from blank snapshot
- hot restore from function snapshot
2. Function snapshots can be prepared proactively when a version becomes ready and the runtime/network policy allows safe reuse, so first invoke in a region can land on already-prepared warm state.
3. Configures Firecracker + jailer + cgroups + networking + vsock.
4. Ensures required artifact digest is local (fetch if missing).
5. Retrieves scoped secrets via secrets proxy.
6. Injects env and secrets to guest over controlled channel.
7. Guest runner executes handler subprocess.
8. For HTTP-triggered functions, guest can stream HTTP response events back before the handler fully finishes.
9. Guest streams structured logs and final result back.
10. Node agent finalizes attempt and either tears down VM or preserves safe reusable snapshot.

Isolation rule:

- Do not run unrelated tenant jobs concurrently in the same dirty VM state.

## 10) Completion and status

On success:

1. Attempt is marked successful.
2. Execution job is marked completed.
3. Result metadata is persisted.
4. Logs/outputs are stored and indexed.
5. Cost records are updated.
6. Client polls `GET /v1/jobs/{jobId}` to terminal status.
7. Client can fetch archived artifacts via `GET /v1/jobs/{jobId}/logs` and `GET /v1/jobs/{jobId}/output`.

## 11) Failure scenarios and outcomes

- Build failure: function version stays not-ready/failed; deploy reports build error.
- Host crash during run: lease expires, attempt retried.
- Region outage: new work routed to healthy region; in-flight work retries after lease timeout.
- Runtime timeout/resource exceed: attempt fails and retries up to configured limit.
- Duplicate submits/invokes: idempotency key returns stable prior result.

## 12) Security and policy flow

- Secrets stay in AWS Secrets Manager.
- Guest never receives long-lived AWS credentials.
- Network policy is function-scoped (`none` and `full` in v1; `allowlist` later).
- Hard limits enforced at admission and runtime (artifact size, memory, timeout, log size, concurrency, build minutes).

Current v1 implementation caps:

- runtime: `node22`
- memory: `64` to `4096` MB
- timeout: `1` to `900` seconds
- retries: `0` to `10`
- env vars: up to `128`
- env refs: up to `128`
- artifact size: `64 MiB`
- execution logs: `8 MiB`
- execution output payload: `32 MiB`
- active build jobs per project: `20`
- active execution jobs per project: `200`
- Function URL timeout cap: `180` seconds
- website defaults (`framework=nextjs` or `deliveryKind=website`):
  - memory floor `2048 MB`
  - timeout floor `180` seconds
  - network policy `full`

## 13) Observability flow

- `/metrics` exposes Prometheus-format control-plane metrics.
- Raw logs/results are archived to object storage and exposed through the API.
- Execution traces now include latency and warm-start mode data.
- Postgres remains source of truth for object and status state.

## 14) End-to-end timeline (happy path)

1. User deploys function.
2. Control plane validates and records metadata.
3. Build job queued and processed.
4. Immutable artifact generated, uploaded, replicated.
5. Default public Function URL is created.
6. Function version marked ready.
7. User invokes function, Function URL, or website preview.
8. Execution is dispatched directly or queued in the chosen region.
9. Coordinator assigns host.
10. Node agent runs microVM and executes handler.
11. Result/logs persisted; status becomes completed.

## 15) Mental model to remember

- Control plane decides and records.
- Build plane packages immutable bytes.
- Execution plane runs those exact bytes safely and repeatedly.
- Leases + retries handle failure.
- Idempotency preserves correctness under at-least-once delivery.
