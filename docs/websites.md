# Website And Next.js Deployments

The platform also supports website-style deployments in addition to plain function handlers (experimental, significantly)

## Current Scope

Implemented today:

- detection of Next.js standalone output
- upload of website static assets to object storage
- website preview URLs at `/w/{functionVersionId}`
- default public Function URL for the same function version
- static asset serving plus dynamic request execution through the function runtime

Current framework support:

- `nextjs`

## How Website Deploys Are Identified

The build pipeline treats a deploy as a website when either of these metadata values are present:

- `framework=nextjs`
- `deliveryKind=website`

For git builds, Next.js standalone detection also happens automatically if the workspace contains `next` and produces `.next/standalone/server.js`.

## Default Normalization For Next.js

Website deploys are normalized more aggressively than plain functions:

- memory floor: `2048 MB`
- timeout floor: `180s`
- network policy: `full`

Those defaults are applied in the build service before the function version is created.

## Build Output Shape

When a Next.js standalone build is detected, the builder:

1. runs the normal package-manager install/build flow
2. stages `.next/static` into the bundle root
3. stages `public/` into the bundle root
4. writes a platform handler that proxies requests into the standalone `server.js`
5. uploads collected static assets into object storage
6. writes a site manifest alongside the normal function bundle metadata

The final bundle still remains a normal immutable function artifact. The website path is a delivery mode layered on top of the same deploy/build/execution model.

## Serving Model

### Preview URL

Each website function version gets a preview URL:

```text
/w/{functionVersionId}
```

That route:

- resolves the website manifest
- serves static assets directly from object storage when the path matches
- executes the dynamic site handler for non-static requests

### Function URL

Website functions also get the normal default public Function URL:

```text
/f/{token}
```

That is useful for:

- direct execution testing
- HTTP trigger semantics
- non-preview integration testing

## Environment Variables

Environment variables are supported through the normal deploy request:

- `envVars` for inline values
- `envRefs` for secret-backed values

For website deploys, those values are available to the dynamic handler path exactly like a normal function version.

## Current Limitations

- only Next.js standalone output is modeled as a website today
- the preview path is still backed by function execution, so large SSR pages still depend on execution host capacity
- the builder is not yet Firecracker-isolated
- there is no framework-specific optimization layer such as OpenNext packaging or Lambda-style route splitting yet

## Recommended Use

Today’s website path is good for:

- small to medium Next.js sites
- preview environments
- demonstrating website deployment on top of the serverless runtime

It is not yet equivalent to Vercel’s mature Next.js platform path. The current implementation is closer to “Next.js standalone wrapped inside the platform’s function model.”
