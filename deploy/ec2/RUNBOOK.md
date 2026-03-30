# Split EC2 Runbook

This runbook is for the split production-shaped deployment:

- one EC2 for the control plane
- one EC2 for the Firecracker execution host

The control plane is public. The execution host should be private-only once provisioning is complete.

## 1. Define local shell variables

Set these before running any commands from your laptop:

```bash
export AWS_REGION='ap-south-1'
export CONTROL_PLANE_PUBLIC_HOST='<control-plane-public-ip-or-dns>'
export CONTROL_PLANE_PRIVATE_HOST='<control-plane-private-ip>'
export EXECUTION_HOST_PRIVATE='<execution-host-private-ip>'
export SSH_KEY_PATH='/absolute/path/to/key.pem'
export FRONTEND_DIR='/absolute/path/to/frontend-repo'
export LECREV_SSH_PROXY_JUMP="ec2-user@${CONTROL_PLANE_PUBLIC_HOST}"
```

Example access pattern once the worker is private-only:

```bash
ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}"
```

## 2. Control-plane host layout

Main services:

- `lecrev-control-plane`
- `nginx`
- `nats-server`
- `postgresql`

Important files:

- env: `/etc/lecrev/control-plane.env`
- gRPC certs: `/etc/lecrev/grpc/*.pem`
- binary: `/opt/lecrev/bin/lecrev`
- static frontend: `/var/www/lecrev`

## 3. Execution host layout

Main services:

- `lecrev-node-agent`

Important files:

- env: `/etc/lecrev/node-agent.env`
- gRPC certs: `/etc/lecrev/grpc/*.pem`
- binaries:
  - `/opt/lecrev/bin/lecrev`
  - `/opt/lecrev/bin/lecrev-guest-runner`
- Firecracker assets:
  - `/var/lib/lecrev/vmlinux`
  - `/var/lib/lecrev/rootfs.ext4`
  - `/var/lib/lecrev/runtime`
  - `/var/lib/lecrev/jailer`

## 4. Redeploy commands

Redeploy the control plane and frontend:

```bash
cd /Users/ishaan/eeeverc
bash deploy/ec2/deploy-control-plane.sh "${CONTROL_PLANE_PUBLIC_HOST}" "${SSH_KEY_PATH}" "${FRONTEND_DIR}"
```

Redeploy the execution host over the private address:

```bash
cd /Users/ishaan/eeeverc
bash deploy/ec2/deploy-execution-host.sh "${EXECUTION_HOST_PRIVATE}" "${SSH_KEY_PATH}"
```

If `LECREV_SSH_PROXY_JUMP` is set, the deploy scripts automatically use a proxy command that reuses the same PEM for the jump host. If the execution host still has a temporary public IP during bootstrap, you can use that first and remove it later.

## 5. Service start, stop, restart

Control-plane services:

```bash
ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo systemctl status lecrev-control-plane nginx nats-server postgresql --no-pager'

ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo systemctl restart lecrev-control-plane nginx nats-server postgresql'

ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo systemctl stop lecrev-control-plane nginx nats-server postgresql'

ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo systemctl start lecrev-control-plane nginx nats-server postgresql'
```

Execution-host service:

```bash
ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}" \
  'sudo systemctl status lecrev-node-agent --no-pager'

ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}" \
  'sudo systemctl restart lecrev-node-agent'

ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}" \
  'sudo systemctl stop lecrev-node-agent'

ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}" \
  'sudo systemctl start lecrev-node-agent'
```

## 6. Log inspection

Control-plane logs:

```bash
ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo journalctl -u lecrev-control-plane -n 200 --no-pager'

ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo journalctl -u nginx -n 100 --no-pager'
```

Execution-host logs:

```bash
ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}" \
  'sudo journalctl -u lecrev-node-agent -n 200 --no-pager'
```

Firecracker host check:

```bash
ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${EXECUTION_HOST_PRIVATE}" \
  'sudo APP_USER=lecrev /usr/local/bin/lecrev-check-firecracker-host'
```

## 7. Health and inventory checks

Public health:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/healthz"
```

Region and host inventory:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/v1/regions" \
  -H 'X-API-Key: <bootstrap-api-key>'

curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/v1/regions/ap-south-1/hosts" \
  -H 'X-API-Key: <bootstrap-api-key>'

curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/v1/regions/ap-south-1/warm-pools" \
  -H 'X-API-Key: <bootstrap-api-key>'
```

## 8. End-to-end smoke

Run the public smoke test from the repo:

```bash
cd /Users/ishaan/eeeverc
go run ./cmd/lecrev smoke \
  --api "http://${CONTROL_PLANE_PUBLIC_HOST}" \
  --api-key '<bootstrap-api-key>' \
  --project demo \
  --regions ap-south-1
```

## 9. Function URL operations

The control plane should set `LECREV_PUBLIC_BASE_URL` in `/etc/lecrev/control-plane.env` so returned function URLs use the real public origin.

Create a public Function URL:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/v1/functions/<version-id>/triggers/http" \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <bootstrap-api-key>' \
  -d '{"description":"public endpoint","authMode":"none"}'
```

Create an API-key-protected Function URL:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/v1/functions/<version-id>/triggers/http" \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <bootstrap-api-key>' \
  -d '{"description":"private endpoint","authMode":"api_key"}'
```

List HTTP triggers for a function version:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/v1/functions/<version-id>/triggers/http" \
  -H 'X-API-Key: <bootstrap-api-key>'
```

Invoke a public Function URL:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/f/<token>" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: request-123' \
  -d '{"hello":"world"}'
```

Invoke a private Function URL:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/f/<token>" \
  -H 'Authorization: Bearer <project-api-key>' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: request-456' \
  -d '{"hello":"world"}'
```

`HEAD` requests return headers and status without a body:

```bash
curl -I "http://${CONTROL_PLANE_PUBLIC_HOST}/f/<token>" \
  -H 'Authorization: Bearer <project-api-key>'
```

## 10. Stop instances to control spend

Stop only the execution host when you do not need Firecracker validation:

```bash
aws ec2 stop-instances \
  --region "${AWS_REGION}" \
  --instance-ids <execution-host-instance-id>
```

Stop both hosts when the demo window is over:

```bash
aws ec2 stop-instances \
  --region "${AWS_REGION}" \
  --instance-ids <control-plane-instance-id> <execution-host-instance-id>
```

Start them again:

```bash
aws ec2 start-instances \
  --region "${AWS_REGION}" \
  --instance-ids <control-plane-instance-id> <execution-host-instance-id>
```

Wait for state changes:

```bash
aws ec2 wait instance-stopped --region "${AWS_REGION}" --instance-ids <instance-id>
aws ec2 wait instance-running --region "${AWS_REGION}" --instance-ids <instance-id>
```

## 11. Remove execution-host public exposure

Recommended target state:

- control plane keeps the public ingress
- execution host has no public IP
- execution host SSH allows access only from the control-plane security group or SSM

Allow SSH from the control-plane security group to the execution-host security group:

```bash
aws ec2 authorize-security-group-ingress \
  --region "${AWS_REGION}" \
  --group-id <execution-host-sg-id> \
  --ip-permissions '[{"IpProtocol":"tcp","FromPort":22,"ToPort":22,"UserIdGroupPairs":[{"GroupId":"<control-plane-sg-id>"}]}]'
```

Disable auto-association of a public IP on the execution host primary ENI:

```bash
aws ec2 modify-network-interface-attribute \
  --region "${AWS_REGION}" \
  --network-interface-id <execution-host-eni-id> \
  --no-associate-public-ip-address
```

Then stop and start the execution host so the temporary public IP is released:

```bash
aws ec2 stop-instances --region "${AWS_REGION}" --instance-ids <execution-host-instance-id>
aws ec2 wait instance-stopped --region "${AWS_REGION}" --instance-ids <execution-host-instance-id>
aws ec2 start-instances --region "${AWS_REGION}" --instance-ids <execution-host-instance-id>
aws ec2 wait instance-running --region "${AWS_REGION}" --instance-ids <execution-host-instance-id>
```

Verify there is no public IP left:

```bash
aws ec2 describe-instances \
  --region "${AWS_REGION}" \
  --instance-ids <execution-host-instance-id> \
  --query 'Reservations[0].Instances[0].PublicIpAddress' \
  --output text
```

## 12. Current platform constraints

- `networkPolicy=full` still uses a single static `tap0`, so keep `LECREV_EXECUTION_HOST_SLOTS=1`.
- The control plane is the only public entrypoint.
- Function URLs are synchronous on the control plane and should be kept within the configured function timeout envelope.
