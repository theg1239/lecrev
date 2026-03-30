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

## 3. Provision the Firecracker host assets

If you want the execution plane to use real Firecracker microVMs on this box, run:

```bash
scp -i /path/to/key.pem deploy/ec2/install-firecracker-host.sh ec2-user@<host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<host> 'bash /tmp/install-firecracker-host.sh'
```

This installs:

- `firecracker`
- `jailer`
- the matching upstream Firecracker CI guest kernel at `/var/lib/lecrev/vmlinux`
- runtime directories under `/var/lib/lecrev/runtime`

It does **not** build the guest rootfs yet. The deploy step below rebuilds that image from the current host `node` binary plus the freshly uploaded `lecrev-guest-runner`, so the guest stays aligned with the backend release you just deployed.

If you also want `networkPolicy=full` to work on this one-box host, configure the static tap device used by the current Firecracker driver:

```bash
scp -i /path/to/key.pem deploy/ec2/configure-firecracker-network.sh ec2-user@<host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<host> 'bash /tmp/configure-firecracker-network.sh'
```

This helper creates `tap0`, assigns `172.16.0.1/30`, and installs outbound NAT rules. Keep `LECREV_EXECUTION_HOST_SLOTS=1` when using this static tap layout.

## 4. Install the Lecrev environment file

Copy [lecrev.env.example](./lecrev.env.example) to `/etc/lecrev/lecrev.env` on the host and replace the placeholder secrets.

At minimum:

- `LECREV_BOOTSTRAP_ADMIN_API_KEY`
- `LECREV_SECRETS_PROXY_TOKEN`
- `LECREV_POSTGRES_DSN`

To switch the execution plane over from the local fallback to Firecracker, set:

- `LECREV_EXECUTION_DRIVER=firecracker`
- `LECREV_EXECUTION_HOST_SLOTS=1`
- `LECREV_FIRECRACKER_BINARY=/usr/local/bin/firecracker`
- `LECREV_JAILER_BINARY=/usr/local/bin/jailer`
- `LECREV_FIRECRACKER_USE_JAILER=false`
- `LECREV_FIRECRACKER_KERNEL_IMAGE=/var/lib/lecrev/vmlinux`
- `LECREV_FIRECRACKER_ROOTFS=/var/lib/lecrev/rootfs.ext4`
- `LECREV_FIRECRACKER_WORKSPACE_DIR=/var/lib/lecrev/runtime`
- `LECREV_FIRECRACKER_SNAPSHOT_DIR=/var/lib/lecrev/runtime/snapshots`
- `LECREV_FIRECRACKER_GUEST_INIT=/usr/local/bin/lecrev-guest-runner`
- `LECREV_FIRECRACKER_TAP_DEVICE=tap0`
- `LECREV_FIRECRACKER_GUEST_MAC=06:00:ac:10:00:02`
- `LECREV_FIRECRACKER_GUEST_IP=172.16.0.2`
- `LECREV_FIRECRACKER_GATEWAY_IP=172.16.0.1`
- `LECREV_FIRECRACKER_NETMASK=255.255.255.252`

`LECREV_FIRECRACKER_USE_JAILER=false` is intentional for this single-box deployment. The shipped `lecrev.service` runs the whole stack as the unprivileged `lecrev` user; using the jailer cleanly requires splitting privileged execution-host duties out of the monolithic service.

## 5. Deploy backend + frontend

From this repo root:

```bash
bash deploy/ec2/deploy.sh <host> /path/to/key.pem /absolute/path/to/frontend-repo
```

This script:

- cross-compiles `lecrev` for Linux amd64
- cross-compiles `lecrev-guest-runner` for Linux amd64
- builds the frontend static bundle
- uploads binaries, systemd units, nginx config, and frontend assets
- rebuilds `/var/lib/lecrev/rootfs.ext4` when Firecracker assets are present on the host
- restarts `nats-server`, `nginx`, and `lecrev`

## 6. Verify

```bash
curl -sS http://<host>/healthz
curl -sS http://<host>/v1/regions -H 'X-API-Key: <bootstrap-api-key>'
```

For Firecracker hosts, you can also run:

```bash
ssh -i /path/to/key.pem ec2-user@<host> 'sudo APP_USER=lecrev /usr/local/bin/lecrev-check-firecracker-host'
```

For the frontend demo, either:

- enter the API key manually in the dashboard settings, or
- bake `VITE_LECREV_API_KEY` into the frontend build before running `deploy.sh`

## Firecracker

The default deployment env uses `LECREV_EXECUTION_DRIVER=local-node` so the one-box demo can come up immediately. The new Firecracker host scripts close the operational gap:

- `install-firecracker-host.sh` installs Firecracker binaries and the guest kernel
- `deploy.sh` rebuilds the guest rootfs from the deployed guest runner and the host Node runtime
- `check-firecracker-host.sh` validates that `/dev/kvm`, binaries, kernel, rootfs, and snapshot directories are all ready before you flip the service over
