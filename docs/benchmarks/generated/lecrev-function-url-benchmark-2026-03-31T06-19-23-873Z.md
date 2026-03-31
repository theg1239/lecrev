# Lecrev Function URL Benchmark

- Run timestamp: 2026-03-31T06:19:23.873Z
- Methodology: wall-clock request timing around `fetch(url)` + full response body read, matching the upstream cf-vs-vercel benchmark runner.
- Iterations: 100
- Concurrency: 10
- Important context: run each test in isolation after prior queue backlog drains; timed-out synchronous requests can still leave background work in flight and contaminate later benchmark cases.

## realistic-math-bench

- URL: `https://functions.lecrev.everything-assistant.com/f/<token>`
- Successful requests: 11/100
- Failed requests: 89/100
- Failure rate: 89.00%

| Metric | Wall clock | Platform latency header |
|--------|------------|-------------------------|
| Mean | 8.979s | 6951ms |
| Min | 6.049s | 5513ms |
| Max | 15.476s | 9375ms |
| P50 | 6.079s | 5578ms |
| P95 | 15.476s | 9375ms |
| Variability | 9.428s | 3862ms |

## vanilla-slower

- URL: `https://functions.lecrev.everything-assistant.com/f/<token>`
- Successful requests: 0/100
- Failed requests: 100/100
- Failure rate: 100.00%

| Metric | Wall clock | Platform latency header |
|--------|------------|-------------------------|

## Final Results Summary

| Test | Mean | Min | Max | Variability | Platform mean |
|------|------|-----|-----|-------------|---------------|
| realistic-math-bench | 8.979s | 6.049s | 15.476s | 9.428s | 6951ms |

## Published Upstream Reference

These rows come from the checked-in upstream results file at `results-2025-10-12T23-31-26-819Z.json`. They use 100 iterations and concurrency 10. This is reference context only, not an apples-to-apples workload match.

| Test | Cloudflare mean | Vercel mean | Lecrev mean | Winner |
|------|------------------|-------------|-------------|--------|
| next-js | 1.015s | 0.493s | n/a | Vercel |
| react-ssr-bench | 0.158s | 0.107s | n/a | Vercel |
| sveltekit | 0.101s | 0.146s | n/a | Cloudflare |
| realistic-math-bench | 0.673s | 0.667s | 8.979s | Vercel |
| vanilla-slower | 0.156s | 0.164s | n/a | Cloudflare |

