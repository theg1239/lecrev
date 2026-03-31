#!/usr/bin/env node

import fs from "node:fs/promises";
import path from "node:path";
import { performance } from "node:perf_hooks";

const DEFAULT_ITERATIONS = 100;
const DEFAULT_CONCURRENCY = 10;
const DEFAULT_OUTPUT_DIR = path.resolve(process.cwd(), "docs/benchmarks/generated");
const DEFAULT_PUBLISHED_RESULTS = "/tmp/cf-vs-vercel-bench/results/results-2025-10-12T23-31-26-819Z.json";

function usage() {
  console.error(`Usage:
  node scripts/benchmarks/function_url_bench.mjs \\
    --test name=url [--test name=url ...] \\
    [--iterations 100] \\
    [--concurrency 10] \\
    [--output-dir docs/benchmarks/generated] \\
    [--published-results /path/to/results.json]`);
}

function parseArgs(argv) {
  const options = {
    iterations: DEFAULT_ITERATIONS,
    concurrency: DEFAULT_CONCURRENCY,
    outputDir: DEFAULT_OUTPUT_DIR,
    publishedResultsPath: DEFAULT_PUBLISHED_RESULTS,
    tests: [],
  };

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "--iterations") {
      options.iterations = Number(argv[++i]);
      continue;
    }
    if (arg === "--concurrency") {
      options.concurrency = Number(argv[++i]);
      continue;
    }
    if (arg === "--output-dir") {
      options.outputDir = path.resolve(argv[++i]);
      continue;
    }
    if (arg === "--published-results") {
      options.publishedResultsPath = path.resolve(argv[++i]);
      continue;
    }
    if (arg === "--test") {
      const raw = argv[++i];
      const separator = raw.indexOf("=");
      if (separator <= 0) {
        throw new Error(`invalid --test value ${JSON.stringify(raw)}; expected name=url`);
      }
      options.tests.push({
        name: raw.slice(0, separator),
        url: raw.slice(separator + 1),
      });
      continue;
    }
    throw new Error(`unknown argument: ${arg}`);
  }

  if (options.tests.length === 0) {
    throw new Error("at least one --test name=url is required");
  }
  if (!Number.isFinite(options.iterations) || options.iterations <= 0) {
    throw new Error(`invalid iterations value: ${options.iterations}`);
  }
  if (!Number.isFinite(options.concurrency) || options.concurrency <= 0) {
    throw new Error(`invalid concurrency value: ${options.concurrency}`);
  }

  return options;
}

function round(value) {
  return Math.round(value * 1000) / 1000;
}

function formatSeconds(ms) {
  return `${(ms / 1000).toFixed(3)}s`;
}

function redactURL(rawURL) {
  try {
    const url = new URL(rawURL);
    if (url.pathname.startsWith("/f/")) {
      return `${url.origin}/f/<token>`;
    }
    url.search = "";
    url.hash = "";
    return url.toString();
  } catch {
    return rawURL;
  }
}

async function measureResponseTime(url) {
  const startedAt = performance.now();
  try {
    const response = await fetch(url);
    const body = await response.text();
    const finishedAt = performance.now();
    const platformLatencyHeader = response.headers.get("x-lecrev-latency-ms");
    return {
      time: finishedAt - startedAt,
      status: response.status,
      success: response.ok,
      contentLength: body.length,
      platformLatencyMs: platformLatencyHeader ? Number(platformLatencyHeader) : null,
    };
  } catch (error) {
    return {
      time: null,
      status: null,
      success: false,
      error: error instanceof Error ? error.message : String(error),
      contentLength: 0,
      platformLatencyMs: null,
    };
  }
}

function summarizeResults(results) {
  const successful = results.filter((result) => result.success);
  const failed = results.filter((result) => !result.success);
  const times = successful.map((result) => result.time);
  const platformLatencies = successful
    .map((result) => result.platformLatencyMs)
    .filter((value) => Number.isFinite(value));
  const contentLengths = successful.map((result) => result.contentLength);

  const statusCodes = {};
  for (const result of results) {
    if (result.status !== null) {
      statusCodes[result.status] = (statusCodes[result.status] || 0) + 1;
    }
  }

  const errors = {};
  for (const result of failed) {
    if (result.error) {
      errors[result.error] = (errors[result.error] || 0) + 1;
    }
  }

  const average = (values) => values.reduce((sum, value) => sum + value, 0) / values.length;

  return {
    successful: successful.length,
    failed: failed.length,
    failureRate: results.length === 0 ? 0 : (failed.length / results.length) * 100,
    statusCodes,
    errors,
    wallClock: times.length > 0
      ? {
          min: Math.min(...times),
          max: Math.max(...times),
          mean: average(times),
          variability: Math.max(...times) - Math.min(...times),
          p50: percentile(times, 0.5),
          p95: percentile(times, 0.95),
        }
      : null,
    platformLatency: platformLatencies.length > 0
      ? {
          min: Math.min(...platformLatencies),
          max: Math.max(...platformLatencies),
          mean: average(platformLatencies),
          variability: Math.max(...platformLatencies) - Math.min(...platformLatencies),
          p50: percentile(platformLatencies, 0.5),
          p95: percentile(platformLatencies, 0.95),
        }
      : null,
    contentLength: contentLengths.length > 0
      ? {
          min: Math.min(...contentLengths),
          max: Math.max(...contentLengths),
          mean: average(contentLengths),
        }
      : null,
  };
}

function percentile(values, ratio) {
  if (values.length === 0) {
    return null;
  }
  const sorted = [...values].sort((a, b) => a - b);
  const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil(sorted.length * ratio) - 1));
  return sorted[index];
}

async function runBenchmark(test, iterations, concurrency) {
  const results = [];
  let completed = 0;
  let nextIndex = 0;

  async function worker() {
    while (true) {
      const index = nextIndex;
      nextIndex += 1;
      if (index >= iterations) {
        return;
      }
      const result = await measureResponseTime(test.url);
      results.push(result);
      completed += 1;
      process.stdout.write(`  ${test.name}: ${completed}/${iterations}\r`);
    }
  }

  const workerCount = Math.min(iterations, concurrency);
  await Promise.all(Array.from({ length: workerCount }, () => worker()));
  process.stdout.write("\n");

  return summarizeResults(results);
}

async function loadPublishedResults(filePath) {
  try {
    const raw = await fs.readFile(filePath, "utf8");
    const parsed = JSON.parse(raw);
    return {
      timestamp: parsed.timestamp,
      iterations: parsed.iterations,
      concurrency: parsed.concurrency,
      tests: Array.isArray(parsed.tests)
        ? parsed.tests.map((test) => ({
            name: test.name,
            results: {
              cloudflare: {
                min: round(test.results.cloudflare.min),
                max: round(test.results.cloudflare.max),
                mean: round(test.results.cloudflare.mean),
              },
              vercel: {
                min: round(test.results.vercel.min),
                max: round(test.results.vercel.max),
                mean: round(test.results.vercel.mean),
              },
            },
          }))
        : [],
    };
  } catch {
    return null;
  }
}

function renderMarkdownReport(run) {
  const lines = [];
  const lecrevByName = new Map(run.tests.map((test) => [test.name, test]));
  lines.push("# Lecrev Function URL Benchmark");
  lines.push("");
  lines.push(`- Run timestamp: ${run.timestamp}`);
  lines.push(`- Methodology: wall-clock request timing around \`fetch(url)\` + full response body read, matching the upstream cf-vs-vercel benchmark runner.`);
  lines.push(`- Iterations: ${run.iterations}`);
  lines.push(`- Concurrency: ${run.concurrency}`);
  lines.push(`- Important context: run each test in isolation after prior queue backlog drains; timed-out synchronous requests can still leave background work in flight and contaminate later benchmark cases.`);
  lines.push("");

  for (const test of run.tests) {
    lines.push(`## ${test.name}`);
    lines.push("");
    lines.push(`- URL: \`${test.redactedURL}\``);
    lines.push(`- Successful requests: ${test.results.successful}/${run.iterations}`);
    if (test.results.failed > 0) {
      lines.push(`- Failed requests: ${test.results.failed}/${run.iterations}`);
      lines.push(`- Failure rate: ${test.results.failureRate.toFixed(2)}%`);
    }
    lines.push("");
    lines.push("| Metric | Wall clock | Platform latency header |");
    lines.push("|--------|------------|-------------------------|");
    if (test.results.wallClock) {
      const wall = test.results.wallClock;
      const platform = test.results.platformLatency;
      lines.push(`| Mean | ${formatSeconds(wall.mean)} | ${platform ? `${Math.round(platform.mean)}ms` : "n/a"} |`);
      lines.push(`| Min | ${formatSeconds(wall.min)} | ${platform ? `${Math.round(platform.min)}ms` : "n/a"} |`);
      lines.push(`| Max | ${formatSeconds(wall.max)} | ${platform ? `${Math.round(platform.max)}ms` : "n/a"} |`);
      lines.push(`| P50 | ${formatSeconds(wall.p50)} | ${platform ? `${Math.round(platform.p50)}ms` : "n/a"} |`);
      lines.push(`| P95 | ${formatSeconds(wall.p95)} | ${platform ? `${Math.round(platform.p95)}ms` : "n/a"} |`);
      lines.push(`| Variability | ${formatSeconds(wall.variability)} | ${platform ? `${Math.round(platform.variability)}ms` : "n/a"} |`);
    }
    lines.push("");
  }

  lines.push("## Final Results Summary");
  lines.push("");
  lines.push("| Test | Mean | Min | Max | Variability | Platform mean |");
  lines.push("|------|------|-----|-----|-------------|---------------|");
  for (const test of run.tests) {
    const wall = test.results.wallClock;
    const platform = test.results.platformLatency;
    if (!wall) {
      lines.push(`| ${test.name} | failed (${test.results.failureRate.toFixed(2)}%) | n/a | n/a | n/a | n/a |`);
      continue;
    }
    lines.push(`| ${test.name} | ${formatSeconds(wall.mean)} | ${formatSeconds(wall.min)} | ${formatSeconds(wall.max)} | ${formatSeconds(wall.variability)} | ${platform ? `${Math.round(platform.mean)}ms` : "n/a"} |`);
  }
  lines.push("");

  if (run.publishedReference?.tests?.length) {
    lines.push("## Published Upstream Reference");
    lines.push("");
    lines.push(`These rows come from the checked-in upstream results file at \`${path.basename(run.publishedReferencePath)}\`. They use ${run.publishedReference.iterations} iterations and concurrency ${run.publishedReference.concurrency}. This is reference context only, not an apples-to-apples workload match.`);
    lines.push("");
    lines.push("| Test | Cloudflare mean | Vercel mean | Lecrev mean | Winner |");
    lines.push("|------|------------------|-------------|-------------|--------|");
    for (const test of run.publishedReference.tests) {
      const cf = test.results.cloudflare;
      const vercel = test.results.vercel;
      const lecrevResult = lecrevByName.get(test.name)?.results ?? null;
      const lecrev = lecrevResult?.wallClock?.mean ?? null;
      const candidates = [
        { name: "Cloudflare", mean: cf.mean },
        { name: "Vercel", mean: vercel.mean },
      ];
      if (typeof lecrev === "number") {
        candidates.push({ name: "Lecrev", mean: lecrev });
      }
      candidates.sort((left, right) => left.mean - right.mean);
      const lecrevCell = typeof lecrev === "number"
        ? formatSeconds(lecrev)
        : lecrevResult
          ? `failed (${lecrevResult.failureRate.toFixed(2)}%)`
          : "n/a";
      lines.push(`| ${test.name} | ${formatSeconds(cf.mean)} | ${formatSeconds(vercel.mean)} | ${lecrevCell} | ${candidates[0].name} |`);
    }
    lines.push("");
  }

  return lines.join("\n");
}

async function main() {
  let options;
  try {
    options = parseArgs(process.argv.slice(2));
  } catch (error) {
    usage();
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
    return;
  }

  const timestamp = new Date().toISOString();
  const safeStamp = timestamp.replace(/[:.]/g, "-");
  const publishedReference = await loadPublishedResults(options.publishedResultsPath);

  await fs.mkdir(options.outputDir, { recursive: true });

  const run = {
    timestamp,
    iterations: options.iterations,
    concurrency: options.concurrency,
    publishedReferencePath: options.publishedResultsPath,
    publishedReference,
    tests: [],
  };

  console.log("=".repeat(60));
  console.log("  Lecrev Function URL Benchmark");
  console.log("=".repeat(60));

  for (const test of options.tests) {
    console.log(`\nRunning ${test.name}`);
    console.log(`URL: ${test.url}`);
    const results = await runBenchmark(test, options.iterations, options.concurrency);
    run.tests.push({
      ...test,
      redactedURL: redactURL(test.url),
      results: JSON.parse(JSON.stringify(results, (_, value) => typeof value === "number" ? round(value) : value)),
    });
  }

  const jsonPath = path.join(options.outputDir, `lecrev-function-url-benchmark-${safeStamp}.json`);
  const mdPath = path.join(options.outputDir, `lecrev-function-url-benchmark-${safeStamp}.md`);
  await fs.writeFile(jsonPath, `${JSON.stringify(run, null, 2)}\n`, "utf8");
  await fs.writeFile(mdPath, `${renderMarkdownReport(run)}\n`, "utf8");

  console.log(`\nWrote ${jsonPath}`);
  console.log(`Wrote ${mdPath}`);
}

await main();
