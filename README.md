# lecrev

`lecrev` is a Firecracker-first serverless compute platform built around a split control plane, build plane, and execution plane. The repo currently supports:

- async deploy, build, invoke, status, retry, and idempotency flows
- split runtimes for `control-plane`, `build-worker`, `node-agent`, and one-box `devstack`
- Firecracker-backed execution hosts with warm snapshots and jailed execution on dedicated workers
- default public Function URLs for every successful function build
- direct Function URL execution with HTTP streaming and latency trailers
- website deployment support for Next.js standalone output, including preview URLs and static asset serving
- PostgreSQL metadata, NATS-based dispatch, S3 or MinIO artifacts, and a scoped secrets proxy
- APAC-only execution region support (`ap-south-1`, `ap-south-2`, `ap-southeast-1`)

## Current Implementation Shape

### Control plane

- public HTTP API
- API key auth and project isolation
- metadata and status in Postgres
- global scheduling and region selection
- direct Function URL dispatch with queued fallback
- website preview routing

### Build plane

- dedicated `build-worker` runtime
- git or inline bundle deploy sources
- npm, pnpm, and yarn workspace preparation on the build worker host
- immutable bundle plus startup artifact generation
- optional artifact replication into per-region buckets
- default public Function URL creation when a version becomes ready

### Execution plane

- regional coordinators
- node agents with long-lived outbound mTLS gRPC control streams
- Firecracker microVM execution on Linux hosts
- warm blank/function snapshot paths
- execution leases, retries, archived logs, archived output, and cost records

## Quick Start

Optional local infrastructure:

```bash
docker compose -f deploy/local/docker-compose.yml up -d
```

Run the one-box local stack:

```bash
go run ./cmd/lecrev devstack
```

Run the embedded end-to-end smoke test:

```bash
go run ./cmd/lecrev smoke
```

Useful split-runtime commands:

```bash
go run ./cmd/lecrev control-plane
go run ./cmd/lecrev build-worker
go run ./cmd/lecrev node-agent
```

## Current Defaults And Limits

Deploy admission:

- runtime: `node22`
- network policy: `none` and `full`
- memory: `64` to `4096` MB
- timeout: `1` to `900` seconds
- retries: `0` to `10`
- env vars: up to `128`
- env refs: up to `128`
- artifact bundle size: up to `64 MiB`
- active build jobs per project: up to `20`
- active execution jobs per project: up to `200`

Execution archival:

- execution logs: up to `8 MiB`
- execution output payload: up to `32 MiB`

Website normalization:

- framework: `nextjs`
- delivery kind: `website`
- default memory floor: `2048 MB`
- default timeout floor: `180s`
- default network policy: `full`

HTTP surfaces:

- Function URL timeout is capped at `180s`
- streamed Function URL responses expose `Trailer: X-Lecrev-Latency-Ms`

Local dev defaults:

- build workers per region: `4`
- execution slots per host: `4`
- full-network slots per host: `1` unless a tap pool is configured

## Repository Layout

Top-level paths that matter:

- `cmd/lecrev`: CLI entrypoints for local, split, and smoke runtimes
- `cmd/lecrev-guest-runner`: guest PID 1 for Firecracker rootfs images
- `internal/build`: deploy normalization, git builds, bundle creation, Next.js website packaging
- `internal/httpapi`: public API, Function URLs, website previews, metrics, and admin surfaces
- `internal/scheduler`: region selection, direct execution, queued execution, and overload handling
- `internal/coordinator`: regional assignment, lease updates, direct host dispatch
- `internal/nodeagent`: execution-host worker, archival, heartbeats, slot accounting
- `internal/firecracker`: driver interfaces plus local-node and microVM implementations
- `deploy/ec2`: split-host deployment helpers, env templates, nginx, TLS, and runbooks
- `docs`: current architecture, API, website deployment, and benchmark documentation

The nested package layout under `internal/`, `proto/`, and generated `lecrev/` is intentional. Those directories follow Go package boundaries and protobuf namespace structure; the cleanup in this pass is focused on the documentation tree.

## Website And Function Delivery

Successful builds now create a default public Function URL automatically. The platform supports two delivery modes:

- **Function URLs**: `/f/{token}` for JSON or structured HTTP responses, with direct execution first and queued fallback when direct capacity is unavailable.
- **Website previews**: `/w/{functionVersionId}` for Next.js standalone websites, with static assets served from object storage and dynamic requests executed through Firecracker.

Large HTTP bodies stream end to end, and streamed responses expose final latency through the `X-Lecrev-Latency-Ms` trailer.

## Docs

- [Documentation Index](docs/README.md)
- [Architecture](docs/architecture.md)
- [HTTP API](docs/http-api.md)
- [Website And Next.js Deployments](docs/websites.md)
- [Function Lifecycle](FUNCTION_LIFECYCLE.md)
- [EC2 Deployment](deploy/ec2/README.md)
- [EC2 Runbook](deploy/ec2/RUNBOOK.md)
- [Benchmarks](docs/benchmarks/README.md)

## Current Gaps

This repo is substantially implemented, but it is not feature-complete relative to the original target architecture.

Still intentionally incomplete:

- build isolation is host-level, not Firecracker-isolated yet
- `allowlist` network policy is reserved but not enforced
- local and current split deployments do not provide true multi-region active-active worker fleets
- autoscaling is manual/config-driven rather than closed-loop
- ALB/WAF/domain automation is not part of the repo

## Notes

- The platform is intentionally APAC-scoped today. Unsupported regions are rejected at deploy time.
- The local developer default is still `local-node`; Firecracker execution is intended for Linux build/execution hosts.
- The repo currently contains deployment helpers for a split EC2 topology and a one-box demo path. Use the split topology for any serious Firecracker testing.
