# Lecrev Function URL Benchmark Summary

## Scope

This report benchmarks Lecrev's public Function URL path using the same client-side timing model as [`cf-vs-vercel-bench`](https://github.com/t3dotgg/cf-vs-vercel-bench): wall-clock time around `fetch(url)` plus full response body read.

Two live production Function URLs were tested:

- `public-http-demo`: minimal bundle-backed public Function URL
- `lecrev-demo-function`: git-backed demo repo Function URL

Important deployment context for this run:

- Region: `ap-south-1`
- Execution host driver: `firecracker`
- Current live host capacity: `availableSlots=1`
- Function URLs are synchronous and wait for job completion before responding

That means concurrency-heavy results below include real queueing on the single live worker slot.

## Run Artifacts

- Published-shape load run:
  - [/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-08-870Z.md](/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-08-870Z.md)
  - [/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-08-870Z.json](/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-08-870Z.json)
- Single-flight control run:
  - [/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-57-599Z.md](/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-57-599Z.md)
  - [/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-57-599Z.json](/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T02-02-57-599Z.json)

## Published-Shape Run

This run matches the checked-in upstream sample result shape:

- Iterations: `100`
- Concurrency: `10`

### Results

| Test | Success | Failure rate | Mean wall clock | P95 wall clock | Platform mean |
|------|---------|--------------|-----------------|----------------|---------------|
| public-http-demo | 27/100 | 73.00% | 5.832s | 8.442s | 521ms |
| lecrev-demo-function | 0/100 | 100.00% | n/a | n/a | n/a |

### What This Means

- The execution path itself is not taking 5.8 seconds.
- Successful `public-http-demo` responses still reported a platform latency header around `521ms`.
- The gap between `521ms` server work and `5.832s` client wall time is queue wait on the single live slot.
- At concurrency `10`, the public Function URL path is currently overload-prone and starts returning `502` before queued requests can complete.

This is the current production bottleneck more than raw Firecracker invoke speed.

## Single-Flight Control Run

This run isolates execution speed by removing queue pressure:

- Iterations: `20`
- Concurrency: `1`

### Results

| Test | Success | Mean wall clock | Min | Max | P95 | Platform mean |
|------|---------|-----------------|-----|-----|-----|---------------|
| public-http-demo | 20/20 | 0.661s | 0.532s | 1.308s | 0.708s | 512ms |
| lecrev-demo-function | 20/20 | 0.634s | 0.607s | 0.710s | 0.708s | 559ms |

### What This Means

- Current steady-state Function URL execution is about `0.63s` to `0.66s` wall-clock from this client.
- The platform-reported server-side latency is about `512ms` to `559ms`.
- Network + ingress + response overhead is therefore roughly `75ms` to `150ms` on top of platform execution for these runs.

## Upstream Reference Context

The upstream benchmark repo's checked-in sample results used:

- Iterations: `100`
- Concurrency: `10`

Reference means from the upstream sample:

| Test | Cloudflare mean | Vercel mean |
|------|------------------|-------------|
| next-js | 1.015s | 0.493s |
| react-ssr-bench | 0.158s | 0.107s |
| sveltekit | 0.101s | 0.146s |
| realistic-math-bench | 0.673s | 0.667s |
| vanilla-slower | 0.156s | 0.164s |

This is not an apples-to-apples workload comparison. Those are different apps and platforms. The useful comparison is:

- Lecrev single-flight public Function URL today: about `0.63s` to `0.66s`
- Lecrev concurrency-10 public Function URL today: queue collapse with `73%` to `100%` failure on a single live slot

## Bottom Line

The current platform has two separate latency stories:

- **Core execution speed**: reasonable but still slower than mature warm-path serverless platforms, at roughly `0.5s` platform latency and `0.63s` to `0.66s` client-observed wall clock.
- **Loaded HTTP function URL behavior**: not production-ready, because the live deployment only exposes one execution slot and synchronous function URLs queue behind it until callers hit response limits.

## Next Fixes

1. Increase real execution concurrency by adding more worker slots or more execution hosts.
2. Replace the static single-slot full-network constraint with per-VM network device allocation.
3. Decouple Function URL caller deadlines from queue wait, or reject earlier with explicit overload signals.
4. Continue reducing the remaining `~500ms` platform execution path after the queueing issue is addressed.
