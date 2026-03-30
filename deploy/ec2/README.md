# EC2 Deployment

This deployment bundle targets a single Amazon Linux 2023 EC2 host for demo or staging use.

## 1. Install runtime dependencies on the host

From your local machine:

```bash
scp -i /path/to/key.pem deploy/ec2/install-runtime.sh ec2-user@<host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<host> 'bash /tmp/install-runtime.sh'
```

This installs:

- `git`
- `nginx`
- `postgresql15`
- `nats-server` with JetStream
- Node.js 22 from the official Linux tarball

## 2. Initialize PostgreSQL

```bash
scp -i /path/to/key.pem deploy/ec2/init-postgres.sh ec2-user@<host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<host> \
  'export LECREV_DB_PASSWORD=replace-me && bash /tmp/init-postgres.sh'
```

## 3. Install the Lecrev environment file

Copy [lecrev.env.example](./lecrev.env.example) to `/etc/lecrev/lecrev.env` on the host and replace the placeholder secrets.

At minimum:

- `LECREV_BOOTSTRAP_ADMIN_API_KEY`
- `LECREV_SECRETS_PROXY_TOKEN`
- `LECREV_POSTGRES_DSN`

## 4. Deploy backend + frontend

From this repo root:

```bash
bash deploy/ec2/deploy.sh <host> /path/to/key.pem /absolute/path/to/frontend-repo
```

This script:

- cross-compiles `lecrev` for Linux amd64
- cross-compiles `lecrev-guest-runner` for Linux amd64
- builds the frontend static bundle
- uploads binaries, systemd units, nginx config, and frontend assets
- restarts `nats-server`, `nginx`, and `lecrev`

## 5. Verify

```bash
curl -sS http://<host>/healthz
curl -sS http://<host>/v1/regions -H 'X-API-Key: <bootstrap-api-key>'
```

For the frontend demo, either:

- enter the API key manually in the dashboard settings, or
- bake `VITE_LECREV_API_KEY` into the frontend build before running `deploy.sh`

## Firecracker

The default deployment env uses `LECREV_EXECUTION_DRIVER=local-node` so the one-box demo can come up immediately.

To switch the execution plane to Firecracker on this host, set:

- `LECREV_EXECUTION_DRIVER=firecracker`
- `LECREV_FIRECRACKER_BINARY`
- `LECREV_FIRECRACKER_KERNEL_IMAGE`
- `LECREV_FIRECRACKER_ROOTFS`
- `LECREV_FIRECRACKER_GUEST_INIT`

and provide a guest rootfs image that contains:

- `node`
- `lecrev-guest-runner`
- a writable root filesystem
