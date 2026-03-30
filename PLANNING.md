# Firecracker-First Multi-Region Async Compute Platform

## Summary
- Split the platform into three planes: control plane (API, auth, metadata, scheduler), build plane (repo clone, package, bundle), and execution plane (regional coordinators plus Firecracker hosts). Keep control-plane writes active-passive in one primary AWS region; keep build and execution active-active across two AWS regions.
- Run Firecracker only on KVM-capable Linux hosts. Default the real build and execution pools to EC2 bare-metal Auto Scaling Groups; treat AWS nested-virtualization-enabled C8i, M8i, and R8i instances as a secondary dev or cost option, because AWS supports KVM nested virtualization there but recommends bare metal for latency-sensitive virtualization workloads, and Firecracker itself is KVM-based with a host API and jailer model. ([docs.aws.amazon.com](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html))
- Optimize v1 for async jobs, not open HTTP requests. “Infinitely scalable” here means the build and execution hot path scales horizontally by region and host, while the metadata plane stays strongly consistent and can shard later by tenant.

## Public Interfaces and Data Model
- External deploy API: `POST /v1/projects/{project}/functions` with `source.type=bundle|git`, `runtime=node22`, `entrypoint`, `memoryMb`, `timeoutSec`, `networkPolicy=none|allowlist|full`, `regions[]`, `envRefs[]`, `maxRetries`, and `idempotencyKey`.
- External invoke and status APIs: `POST /v1/functions/{versionId}/invoke`, `GET /v1/jobs/{jobId}`, `GET /v1/functions/{versionId}`, and `POST /v1/triggers/webhook/{token}`.
- Internal tables and entities: `tenants`, `projects`, `api_keys`, `secret_refs`, `functions`, `function_versions`, `artifacts`, `build_jobs`, `execution_jobs`, `attempts`, `leases`, `regions`, `hosts`, `warm_pools`, and `cost_records`.
- Artifact contract: every deploy becomes an immutable `{bundle.tgz, function.json, sha256, startup.json}` object; execution only ever runs by digest, never by mutable tag or branch.

## Control Plane and Host Control
- Run API, auth, scheduler, build controller, secrets proxy, and dashboard backend as Go services on regular EC2 VMs under `systemd`, fronted by ALB for public HTTP.
- Use managed Postgres as the system of record; use one writer in the primary region and a cross-region replica in the secondary. Keep scheduler replicas stateless and elect one active leader with Postgres advisory locks.
- Use one 3-node NATS JetStream cluster per region, not one cross-region supercluster. The global scheduler writes build and execution work into the target region’s JetStream stream; regional services consume locally.
- Coordinator-to-host control is long-lived outbound mTLS gRPC: each `node-agent` dials its regional coordinator, registers capacity, heartbeats every few seconds, and receives `AssignExecution`, `PrepareSnapshot`, `DrainHost`, `TerminateVm`, and `Ack` messages over that stream. The control plane never SSHs into hosts.
- Use JetStream durable pull consumers with explicit `Ack` and `InProgress` handling for build and execution work; keep platform semantics at-least-once and require idempotency at the API layer instead of pretending to offer exactly-once end to end. ([docs.nats.io](https://docs.nats.io/nats-concepts/jetstream))

## Build Plane
- Repo deploys go through a separate build pipeline: clone repo -> resolve lockfile -> install dependencies -> run bundler -> emit `bundle.tgz + function.json + sha256` -> upload to S3 -> replicate to target regions -> mark version `ready`.
- Bundle deploys skip clone and install and only do manifest validation, unpack smoke test, digest computation, replication, and optional precompute of a function snapshot recipe.
- Run builders inside Firecracker too, but on separate build-host Auto Scaling Groups, not execution hosts. Builders need broader egress, larger scratch volumes, and different cache behavior; mixing them with execution hosts will damage both latency and isolation.
- Cache only build inputs that are safe and content-addressed: git mirror cache, npm tarball cache, and build layer cache. Never share writable build workspace state across tenants.

## Execution Plane
- Each region runs two small coordinator replicas behind an internal load balancer and a fleet of dedicated Firecracker hosts. Each host runs `node-agent`, Firecracker, jailer, and local snapshot and artifact caches on NVMe or fast EBS.
- The `node-agent` owns the full VM lifecycle locally: create API socket, start jailer, configure kernel, rootfs, network, vsock, and cgroups, restore a snapshot or boot a new microVM, attach a job lease, stream logs and results back, then recycle or destroy the VM.
- Keep three reusable assets per runtime and size class: base guest image, blank warm snapshot (OS plus runner plus Node already booted), and function snapshot (artifact unpacked and entry module preloaded before any user job runs).
- Do not run unrelated jobs concurrently inside one live microVM in the first serious multi-tenant version. Reuse should mean restoring clean function snapshots or keeping a ready queue of identical function workers, not multiplexing dirty shared state between tenants or invocations.
- Guest runner contract: receive assignment over vsock, fetch or validate local artifact by digest, mount env and secrets provided by the `node-agent`, execute the handler in a subprocess, send structured logs, exit code, and result over vsock, then shut down or snapshot if the VM is still clean.

## Scheduling, Scaling, and Failure Handling
- Placement is two-stage. Global scheduler picks region by tenant policy, artifact locality, region health, warm function capacity, blank warm capacity, queue age, and cost. Regional coordinator then picks host by matching function snapshot availability, free RAM and CPU, recent failures, and local artifact-cache hit rate.
- Keep four autoscaling loops separate: blank warm snapshot pool, hot function snapshot pool, build-host ASGs, and execution-host ASGs. Build pressure and execution pressure must not scale the same nodes.
- Use explicit attempt leases with TTL. Assignment creates a lease; `node-agent` heartbeats renew it through the coordinator; missing heartbeats expire the lease and the job returns to JetStream and Postgres for retry on another host or region.
- Host drain is first-class: coordinator marks host `draining`, `node-agent` stops new assignments, completes or times out leased jobs, preserves only safe reusable snapshots, then terminates.
- Region failover is for new work immediately and for running work only after lease expiry. Do not try cross-region live migration of executing microVMs in the first version.

## Security, Secrets, and Observability
- Secrets live in AWS Secrets Manager; the guest never receives AWS credentials. At run time the `node-agent` requests scoped secret values from a secrets proxy, injects them over vsock, and scrubs them after completion.
- Network policy is per function, but implement it in phases: ship `none` and `full` first; add `allowlist` later via an egress proxy or `nftables` or `ipset` control. That keeps the manifest stable without overpromising v1 networking.
- Enforce hard caps at admission and at run time: artifact size, memory, CPU shares, runtime timeout, log size, output size, concurrent jobs per tenant, and build minutes per tenant.
- Pipe structured logs from `node-agent`s to Loki and archive raw execution logs and results in S3; export Prometheus metrics for queue depth, cold, warm, and hot starts, active VMs, host saturation, retry rate, build backlog, and per-tenant cost.
- Firecracker exposes the host-side API needed to configure the microVM, supports vsock and snapshot and restore, and is designed around a jailer plus seccomp plus minimal-device model; the `node-agent` should be a thin orchestrator around those primitives, not an extra container scheduler. ([github.com](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md))

## Test Plan and Defaults
- Test both deploy paths, then load-test three execution classes separately: cold boot from base image, warm restore from blank snapshot, and hot restore from function snapshot.
- Kill one microVM, then one execution host, then one whole region; verify lease expiry, retry, region reroute, and correct final job state in Postgres.
- Validate isolation with filesystem wipe checks, secret non-reuse across jobs, network-policy enforcement, and per-tenant quota exhaustion.
- Defaults chosen: Node.js 22 only, async jobs only, multi-tenant external users, platform-native repo builds, all control services on EC2 VMs, Firecracker build and execution on dedicated host pools, active-active execution with active-passive control-plane writes.
- Honest scaling boundary: the execution plane is horizontally scalable from day one; the first real bottleneck is control-plane metadata throughput, so the forward path is sharding by tenant or region rather than pretending the initial writer database is unlimited.
