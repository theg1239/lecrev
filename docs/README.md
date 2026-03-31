# Documentation

This directory is now the documentation entry point for the repo. The top-level `README.md` stays short and points here; deeper material is split by concern instead of being packed into one file.

## Core Docs

- [Architecture](architecture.md)
  - current implementation shape
  - runtime boundaries
  - what is implemented today versus still planned
- [HTTP API](http-api.md)
  - public API surfaces
  - auth model
  - Function URL and preview URL behavior
- [Website And Next.js Deployments](websites.md)
  - how website deploys are recognized
  - current defaults for Next.js sites
  - preview and static asset behavior
- [Function Lifecycle](../FUNCTION_LIFECYCLE.md)
  - step-by-step flow from deploy to build to invoke to completion

## Operations

- [EC2 Deployment Guide](../deploy/ec2/README.md)
- [EC2 Runbook](../deploy/ec2/RUNBOOK.md)

## Benchmarks

- [Benchmarks Overview](benchmarks/README.md)

## Documentation Layout

The docs tree is intentionally flat at the top level:

- `docs/*.md` for stable documentation
- `docs/benchmarks/*.md` for curated benchmark summaries
- `docs/benchmarks/generated/*` for raw generated benchmark outputs

That keeps the repo from having a `docs/` directory that only contains one nested folder and no clear starting point.
