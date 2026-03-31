# Architecture

This document describes the current implementation, not the original aspirational plan.

## Runtime Planes

### Control plane

The control plane owns:

- public HTTP API
- API key authentication
- project and tenant metadata
- build/job/function state transitions
- region selection
- direct Function URL dispatch
- website preview routing
- metrics exposure

Key implementation points:

- Postgres is the metadata source of truth.
- Public routes live in `internal/httpapi`.
- Region and host assignment logic lives in `internal/scheduler`.
- The control plane can dispatch synchronous Function URL requests directly to a live coordinator and only falls back to the queued async path when direct capacity is unavailable.

### Build plane

The build plane currently runs as a dedicated `build-worker` runtime.

What it does today:

- clones git repositories
- resolves package manager (`npm`, `pnpm`, or `yarn`)
- installs dependencies
- runs `build` when present
- prunes production dependencies
- packages an immutable `bundle.tgz`
- writes `function.json` and `startup.json`
- uploads build artifacts to object storage
- optionally replicates artifacts to per-region buckets
- prewarms ready functions through the control plane

Important limitation:

- builds are isolated at the host/service level today
- they are **not** yet executed inside Firecracker build microVMs

That distinction matters. The execution plane is Firecracker-isolated; the build plane is not yet.

### Execution plane

The execution plane is where Firecracker matters.

It consists of:

- one coordinator per execution region
- one or more node agents connected by outbound mTLS gRPC
- Firecracker microVMs on Linux hosts
- host-local warm snapshot inventory
- artifact caching and secrets injection on the host

Execution path:

1. coordinator selects a host
2. node agent selects a start mode
3. Firecracker guest runs the handler
4. logs, output, and cost data are archived
5. lease state is finalized and slots are returned

Supported start modes:

- `cold`
- `blank-warm`
- `function-warm`

## Delivery Modes

### Async function execution

This is the base path:

- `POST /v1/functions/{versionId}/invoke`
- queued job
- poll for status later

### Function URLs

These are the Lambda Function URL equivalent:

- public route: `/f/{token}`
- default public Function URL created automatically after a successful build
- direct synchronous execution first
- queued fallback if direct capacity is unavailable
- supports streamed HTTP bodies
- exposes `X-Lecrev-Latency-Ms` as a header for buffered responses or a trailer for streamed responses

### Website previews

Website previews are separate from Function URLs:

- route: `/w/{functionVersionId}`
- intended for website builds, especially Next.js standalone output
- static assets are served from object storage
- dynamic requests route into the function runtime

## Current Production Shape

The repo supports two deployment shapes:

- one-box demo: `lecrev devstack`
- split deployment:
  - `control-plane`
  - `build-worker`
  - `node-agent`

The split deployment is the production-shaped one and is what the EC2 deployment helpers target.

## Region Model

The platform is intentionally APAC-only today:

- `ap-south-1`
- `ap-south-2`
- `ap-southeast-1`

That is enforced by region normalization and deploy validation in code.

## Honest Gaps

Still not implemented or not fully production-ready:

- Firecracker-isolated build workers
- `allowlist` network policy
- true multi-region active-active execution fleets in deployment tooling
- autoscaling loops
- production ingress automation beyond the shipped EC2/nginx path
