# HTTP API

This is the current backend-facing API surface implemented in the repo.

## Authentication

Authenticated API routes use:

- `X-API-Key: <key>`
- or `Authorization: Bearer <key>`

Public routes:

- `GET /healthz`
- `GET /metrics`
- `POST /v1/triggers/webhook/{token}`
- `ANY /f/{token}`
- `GET /w/{versionId}`
- `GET /w/{versionId}/*`

## Project APIs

- `POST /v1/projects`
- `GET /v1/projects`
- `GET /v1/projects/{projectId}`
- `GET /v1/projects/{projectId}/overview`

Use `POST /v1/projects` to create a new project before attaching functions to it. The platform is project-scoped; deploys should not silently collapse into one shared personal project.

## Deploy APIs

- `POST /v1/projects/{projectId}/functions`
- `GET /v1/functions/{versionId}`
- `GET /v1/build-jobs/{jobId}`
- `GET /v1/build-jobs/{jobId}/logs`

Deploy sources:

- `bundle`
- `git`

Important behavior:

- every successful build creates a default public HTTP trigger automatically
- ready function versions therefore get a Function URL without an extra setup step

## Execution APIs

- `POST /v1/functions/{versionId}/invoke`
- `GET /v1/jobs/{jobId}`
- `GET /v1/jobs/{jobId}/logs`
- `GET /v1/jobs/{jobId}/output`
- `GET /v1/jobs/{jobId}/attempts`
- `GET /v1/jobs/{jobId}/costs`

The standard invoke path is async and returns a job that must be polled.

## Trigger APIs

### Webhook triggers

- `POST /v1/functions/{versionId}/triggers/webhook`
- `GET /v1/functions/{versionId}/triggers/webhook`
- public delivery endpoint: `POST /v1/triggers/webhook/{token}`

### HTTP triggers / Function URLs

- `POST /v1/functions/{versionId}/triggers/http`
- `GET /v1/functions/{versionId}/triggers/http`
- public delivery endpoint: `ANY /f/{token}`

Auth modes:

- `none`
- `api_key`

## Function URL Behavior

Function URL requests take this path:

1. resolve trigger token
2. authorize if `authMode=api_key`
3. build HTTP payload from the incoming request
4. try direct execution against live execution capacity
5. if direct execution is unavailable, fall back to queued execution

Possible outcomes:

- successful buffered HTTP response
- successful streamed HTTP response
- `timeout`
- `overloaded`
- queued fallback response path

Latency reporting:

- buffered responses: `X-Lecrev-Latency-Ms` header
- streamed responses: `Trailer: X-Lecrev-Latency-Ms`

Other useful headers:

- `X-Lecrev-Job-Id`
- `X-Lecrev-Function-Version-Id`
- `X-Lecrev-Function-Url-Token`

## Website APIs

- `GET /v1/functions/{versionId}/site`
- preview route: `GET /w/{versionId}`
- preview subpaths: `GET /w/{versionId}/*`

The site payload includes:

- framework
- dynamic entrypoint
- static prefix
- preview URL
- function URL

## Region And Host APIs

- `GET /v1/regions`
- `GET /v1/regions/{region}/hosts`
- `GET /v1/regions/{region}/warm-pools`
- `POST /v1/regions/{region}/hosts/{hostId}/drain`

`GET /v1/regions` is safe for tenant-facing dashboards. Host and drain operations are operational/admin surfaces.

## Deployment Summary APIs

These power the dashboard and deployment lists:

- `GET /v1/deployments`
- `GET /v1/deployments/{deploymentId}`
- `GET /v1/deployments/{deploymentId}/logs`
- `GET /v1/deployments/{deploymentId}/output`
- `GET /v1/projects/{projectId}/deployments`
- `GET /v1/projects/{projectId}/functions`
- `GET /v1/projects/{projectId}/build-jobs`
- `GET /v1/projects/{projectId}/jobs`

## Request Fields Worth Knowing

Deploy request fields:

- `name`
- `runtime`
- `entrypoint`
- `memoryMb`
- `timeoutSec`
- `networkPolicy`
- `regions`
- `envVars`
- `envRefs`
- `maxRetries`
- `idempotencyKey`
- `source`

Git deploy source fields:

- `gitUrl`
- `gitRef`
- `subPath`
- `metadata`

Website-specific metadata:

- `framework=nextjs`
- `deliveryKind=website`

## Current Limits

The API currently enforces:

- runtime: `node22`
- memory: `64` to `4096` MB
- timeout: `1` to `900` seconds
- retries: `0` to `10`
- env vars: `128`
- env refs: `128`
- artifact size: `64 MiB`
- execution logs: `8 MiB`
- execution output: `32 MiB`
- list endpoints: `limit <= 200`
- Function URL response timeout cap: `180s`
