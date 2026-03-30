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
  'sudo mkdir -p /etc/lecrev/grpc && sudo mv /tmp/control-plane.env /etc/lecrev/control-plane.env'
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/ca.pem ec2-user@<control-plane-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/server.pem ec2-user@<control-plane-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/server-key.pem ec2-user@<control-plane-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<control-plane-host> \
  'sudo install -m 0600 /tmp/ca.pem /etc/lecrev/grpc/ca.pem && \
   sudo install -m 0600 /tmp/server.pem /etc/lecrev/grpc/server.pem && \
   sudo install -m 0600 /tmp/server-key.pem /etc/lecrev/grpc/server-key.pem'
```

Deploy the control plane and frontend:

```bash
bash deploy/ec2/deploy-control-plane.sh <control-plane-host> /path/to/key.pem /absolute/path/to/frontend-repo
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
  'sudo mkdir -p /etc/lecrev/grpc && sudo mv /tmp/node-agent.env /etc/lecrev/node-agent.env'
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/ca.pem ec2-user@<execution-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/client.pem ec2-user@<execution-host>:/tmp/
scp -i /path/to/key.pem /tmp/lecrev-grpc-certs/client-key.pem ec2-user@<execution-host>:/tmp/
ssh -i /path/to/key.pem ec2-user@<execution-host> \
  'sudo install -m 0600 /tmp/ca.pem /etc/lecrev/grpc/ca.pem && \
   sudo install -m 0600 /tmp/client.pem /etc/lecrev/grpc/client.pem && \
   sudo install -m 0600 /tmp/client-key.pem /etc/lecrev/grpc/client-key.pem'
```

Deploy the execution host:

```bash
bash deploy/ec2/deploy-execution-host.sh <execution-host> /path/to/key.pem
```

That deploys:

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

## One-box demo deployment

The older one-box path is still available for staging and demos:

```bash
bash deploy/ec2/deploy.sh <host> /path/to/key.pem /absolute/path/to/frontend-repo
```

That path intentionally keeps `LECREV_FIRECRACKER_USE_JAILER=false`, because it runs the full stack as one unprivileged `lecrev` service.
