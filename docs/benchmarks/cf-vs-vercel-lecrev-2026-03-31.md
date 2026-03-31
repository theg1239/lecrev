# Cloudflare vs Vercel vs Lecrev

- Date: 2026-03-31
- Upstream reference: `/tmp/cf-vs-vercel-bench/results/results-2025-10-12T23-31-26-819Z.json`
- Lecrev raw run: [/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T06-19-23-873Z.md](/Users/ishaan/eeeverc/docs/benchmarks/generated/lecrev-function-url-benchmark-2026-03-31T06-19-23-873Z.md)
- Methodology: same wall-clock measurement style as the upstream runner, using `100` iterations and concurrency `10`
- Lecrev workloads ported directly from the upstream `vanilla-bench/vercel-edition` handlers:
  - `realistic-math-bench`
  - `vanilla-slower`

## Comparison

| Test | Cloudflare mean | Vercel mean | Lecrev mean | Winner |
|------|------------------|-------------|-------------|--------|
| next-js | 1.015s | 0.493s | n/a | Vercel |
| react-ssr-bench | 0.158s | 0.107s | n/a | Vercel |
| sveltekit | 0.101s | 0.146s | n/a | Cloudflare |
| realistic-math-bench | 0.673s | 0.667s | 8.979s | Vercel |
| vanilla-slower | 0.156s | 0.164s | failed (100.00%) | Cloudflare |

## Lecrev Results

### realistic-math-bench

- Successful requests: `11/100`
- Failed requests: `89/100`
- Failure rate: `89.00%`
- Mean wall-clock: `8.979s`
- Mean platform latency header: `6951ms`
- P50 wall-clock: `6.079s`
- P95 wall-clock: `15.476s`

### vanilla-slower

- Successful requests: `0/100`
- Failed requests: `100/100`
- Failure rate: `100.00%`

## What This Means

- The recent warm-readiness work materially improved the first-hit path for normal functions. On prod, a fresh inline function now reached `startMode=function-warm` on the immediate first invoke after build, with `396ms` platform latency.
- These benchmark workloads are a different regime. They are CPU-heavy server-rendering handlers run at concurrency `10`, while the live Lecrev deployment is still one small execution host. The benchmark is exposing execution saturation and queueing, not build latency or cold-start behavior.
- The current bottleneck is capacity, not the scheduler. To materially improve these benchmark numbers, the next concrete moves are:
  - more execution CPU across additional hosts
  - stricter overload handling for synchronous Function URLs
  - routing CPU-heavy workloads away from the same tiny worker pool used for demo traffic
