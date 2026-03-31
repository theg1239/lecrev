# Lecrev Function URL Benchmark

- Run timestamp: 2026-03-31T02:02:08.870Z
- Methodology: wall-clock request timing around `fetch(url)` + full response body read, matching the upstream cf-vs-vercel benchmark runner.
- Iterations: 100
- Concurrency: 10
- Important context: the live execution host currently advertises `availableSlots=1`, so this load test includes real queueing under concurrency.

## public-http-demo

- URL: `https://functions.lecrev.everything-assistant.com/f/<token>`
- Successful requests: 27/100
- Failed requests: 73/100
- Failure rate: 73.00%

| Metric | Wall clock | Platform latency header |
|--------|------------|-------------------------|
| Mean | 5.832s | 521ms |
| Min | 0.698s | 486ms |
| Max | 8.443s | 556ms |
| P50 | 6.321s | 519ms |
| P95 | 8.442s | 545ms |
| Variability | 7.745s | 70ms |

## lecrev-demo-function

- URL: `https://functions.lecrev.everything-assistant.com/f/<token>`
- Successful requests: 0/100
- Failed requests: 100/100
- Failure rate: 100.00%

| Metric | Wall clock | Platform latency header |
|--------|------------|-------------------------|

## Final Results Summary

| Test | Mean | Min | Max | Variability | Platform mean |
|------|------|-----|-----|-------------|---------------|
| public-http-demo | 5.832s | 0.698s | 8.443s | 7.745s | 521ms |

## Published Upstream Reference

These rows come from the checked-in upstream results file at `results-2025-10-12T23-31-26-819Z.json`. They use 100 iterations and concurrency 10. This is reference context only, not an apples-to-apples workload match.

| Test | Cloudflare mean | Vercel mean | Winner |
|------|------------------|-------------|--------|
| next-js | 1.015s | 0.493s | Vercel |
| react-ssr-bench | 0.158s | 0.107s | Vercel |
| sveltekit | 0.101s | 0.146s | Cloudflare |
| realistic-math-bench | 0.673s | 0.667s | Vercel |
| vanilla-slower | 0.156s | 0.164s | Cloudflare |

