# Lecrev Function URL Benchmark

- Run timestamp: 2026-03-31T02:02:57.599Z
- Methodology: wall-clock request timing around `fetch(url)` + full response body read, matching the upstream cf-vs-vercel benchmark runner.
- Iterations: 20
- Concurrency: 1
- Important context: the live execution host currently advertises `availableSlots=1`, so this load test includes real queueing under concurrency.

## public-http-demo

- URL: `https://functions.lecrev.everything-assistant.com/f/<token>`
- Successful requests: 20/20

| Metric | Wall clock | Platform latency header |
|--------|------------|-------------------------|
| Mean | 0.661s | 512ms |
| Min | 0.532s | 481ms |
| Max | 1.308s | 539ms |
| P50 | 0.608s | 514ms |
| P95 | 0.708s | 539ms |
| Variability | 0.776s | 58ms |

## lecrev-demo-function

- URL: `https://functions.lecrev.everything-assistant.com/f/<token>`
- Successful requests: 20/20

| Metric | Wall clock | Platform latency header |
|--------|------------|-------------------------|
| Mean | 0.634s | 559ms |
| Min | 0.607s | 535ms |
| Max | 0.710s | 604ms |
| P50 | 0.633s | 559ms |
| P95 | 0.708s | 580ms |
| Variability | 0.103s | 69ms |

## Final Results Summary

| Test | Mean | Min | Max | Variability | Platform mean |
|------|------|-----|-----|-------------|---------------|
| public-http-demo | 0.661s | 0.532s | 1.308s | 0.776s | 512ms |
| lecrev-demo-function | 0.634s | 0.607s | 0.710s | 0.103s | 559ms |

## Published Upstream Reference

These rows come from the checked-in upstream results file at `results-2025-10-12T23-31-26-819Z.json`. They use 100 iterations and concurrency 10. This is reference context only, not an apples-to-apples workload match.

| Test | Cloudflare mean | Vercel mean | Winner |
|------|------------------|-------------|--------|
| next-js | 1.015s | 0.493s | Vercel |
| react-ssr-bench | 0.158s | 0.107s | Vercel |
| sveltekit | 0.101s | 0.146s | Cloudflare |
| realistic-math-bench | 0.673s | 0.667s | Vercel |
| vanilla-slower | 0.156s | 0.164s | Cloudflare |

