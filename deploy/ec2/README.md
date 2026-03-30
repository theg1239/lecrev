# EC2 Deployment

There are now two deployment shapes in this repo:

1. **Split deployment**: one EC2 for the control plane, one EC2 for the execution host.
2. **One-box demo**: the older monolith that runs `lecrev devstack` on a single host.

Use the split deployment when you want `jailer` enabled and a production-shaped Firecracker execution host.

## Split deployment

### 1. Control-plane EC2

Install the base runtime on the control-plane host:

```bash
scp -i /path/to/key.pem deploy/ec2/install-runtime.sh ec2-user@<control-plane-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<control-plane-host> 'bash /tmp/install-runtime.sh'
```

Initialize PostgreSQL:

```bash
scp -i /path/to/key.pem deploy/ec2/init-postgres.sh ec2-user@<control-plane-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<control-plane-host> \
  'export LECREV_DB_PASSWORD=replace-me && bash /tmp/init-postgres.sh'
```

Generate gRPC mTLS credentials locally:

```bash
bash deploy/ec2/generate-grpc-mtls-certs.sh /tmp/lecrev-grpc-certs lecrev-coordinator.internal <control-plane-private-ip>
```

Install the control-plane env file and gRPC certs:

```bash
scp -i /path/to/key.pem deploy/ec2/control-plane.env.example ec2-user@<control-plane-host>:/tmp/control-plane.env
ssh -i /path/to/key.pem ec2-user@<control-plane-host> \
  'sudo install -o root -g lecrev -m 0640 /tmp/control-plane.env /etc/lecrev/control-plane.env && \
   sudo mkdir -p /etc/lecrev/grpc'
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/ca.pem ec2-user@<control-plane-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/server.pem ec2-user@<control-plane-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/server-key.pem ec2-user@<control-plane-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<control-plane-host> \
  'sudo install -o root -g lecrev -m 0640 /tmp/ca.pem /etc/lecrev/grpc/ca.pem && \
   sudo install -o root -g lecrev -m 0640 /tmp/server.pem /etc/lecrev/grpc/server.pem && \
   sudo install -o root -g lecrev -m 0640 /tmp/server-key.pem /etc/lecrev/grpc/server-key.pem'
```

Deploy the control plane and frontend:

```bash
bash deploy/ec2/deploy-control-plane.sh <control-plane-host> /path/to/key.pem /absolute/path/to/frontend-repo
```

If you need to hop through a bastion or the public control-plane host, set:

```bash
export LECREV_SSH_PROXY_JUMP='ec2-user@<jump-host>'
```

That deploys:

- `lecrev control-plane`
- `nats-server`
- `nginx`
- the frontend static bundle

### 2. Execution-host EC2

Install the base runtime plus Firecracker host assets:

```bash
scp -i /path/to/key.pem deploy/ec2/install-runtime.sh ec2-user@<execution-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<execution-host> 'bash /tmp/install-runtime.sh'

scp -i /path/to/key.pem deploy/ec2/install-firecracker-host.sh ec2-user@<execution-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<execution-host> 'bash /tmp/install-firecracker-host.sh'

scp -i /path/to/key.pem deploy/ec2/configure-firecracker-network.sh ec2-user@<execution-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<execution-host> 'bash /tmp/configure-firecracker-network.sh'
```

Install the node-agent env file and gRPC client certs:

```bash
scp -i /path/to/key.pem deploy/ec2/node-agent.env.example ec2-user@<execution-host>:/tmp/node-agent.env
ssh -i /path/to/key.pem ec2-user@<execution-host> \
  'sudo mkdir -p /etc/lecrev/grpc && sudo install -o root -g root -m 0640 /tmp/node-agent.env /etc/lecrev/node-agent.env'
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/ca.pem ec2-user@<execution-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/client.pem ec2-user@<execution-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/client-key.pem ec2-user@<execution-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<execution-host> \
  'sudo install -m 0600 /tmp/ca.pem /etc/lecrev/grpc/ca.pem && \
   sudo install -m 0600 /tmp/client.pem /etc/lecrev/grpc/client.pem && \
   sudo install -m 0600 /tmp/client-key.pem /etc/lecrev/grpc/client-key.pem'
```

`install-runtime.sh` prefers the official Node.js v22 tarball, but it now falls back to `dnf install nodejs22 nodejs22-npm` on Amazon Linux if the tarball metadata or download step fails.

Deploy the execution host:

```bash
bash deploy/ec2/deploy-execution-host.sh <execution-host> /path/to/key.pem
```

For a private-only execution host, pass its private IP or private DNS name and set `LECREV_SSH_PROXY_JUMP` to the control-plane host.

Before removing the execution host public IP, make sure the worker still has private access to artifact storage. In this AWS setup, that means either:

- an S3 gateway VPC endpoint attached to the subnet route table, or
- NAT egress for the worker subnet

Without one of those, the node-agent blocks on S3 bucket checks at startup and cannot fetch execution artifacts.

That deploys:

- `lecrev-firecracker-network`
- `lecrev node-agent`
- `lecrev-guest-runner`
- the Firecracker rootfs rebuild helper
- the Firecracker host check helper

### 3. Verify the split deployment

Control plane:

```bash
curl -sS http://<control-plane-host>/healthz
curl -sS http://<control-plane-host>/v1/regions -H 'X-API-Key: <bootstrap-api-key>'
```

Execution host:

```bash
ssh -i /path/to/key.pem ec2-user@<execution-host> 'sudo systemctl status lecrev-node-agent --no-pager'
ssh -i /path/to/key.pem ec2-user@<execution-host> 'sudo APP_USER=lecrev /usr/local/bin/lecrev-check-firecracker-host'
```

Important current constraint:

- `configure-firecracker-network.sh` still provisions a single static `tap0`, so keep `LECREV_EXECUTION_HOST_SLOTS=1` if you need `networkPolicy=full`.
- the execution-host deployment now installs `lecrev-firecracker-network.service` so `tap0` is recreated after reboots before the node-agent starts.
- Set `LECREV_PUBLIC_BASE_URL` on the control plane so created HTTP trigger URLs use the real external origin instead of the incoming request host header.

For an operational runbook with start, stop, restart, verification, and function URL commands, see [RUNBOOK.md](RUNBOOK.md).

## One-box demo deployment

The older one-box path is still available for staging and demos:

```bash
bash deploy/ec2/deploy.sh <host> /path/to/key.pem /absolute/path/to/frontend-repo
```

That path intentionally keeps `LECREV_FIRECRACKER_USE_JAILER=false`, because it runs the full stack as one unprivileged `lecrev` service.
