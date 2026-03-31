# Split EC2 Runbook

This runbook is for the split production-shaped deployment:

- one EC2 for the control plane
- one EC2 for the dedicated build worker
- one EC2 for the Firecracker execution host

The control plane is public. The execution host should be private-only once provisioning is complete.

## 1. Define local shell variables

Set these before running any commands from your laptop:

```bash
export AWS_REGION='ap-south-1'
export CONTROL_PLANE_PUBLIC_HOST='<control-plane-public-ip-or-dns>'
export CONTROL_PLANE_PRIVATE_HOST='<control-plane-private-ip>'
export BUILD_HOST_PRIVATE='<build-host-private-ip>'
export EXECUTION_HOST_PRIVATE='<execution-host-private-ip>'
export FRONTEND_HOST='lecrev.example.com'
export API_HOST='api.lecrev.example.com'
export FUNCTIONS_HOST='functions.lecrev.example.com'
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
- nginx renderer: `/usr/local/bin/lecrev-render-control-plane-nginx`
- TLS provisioner: `/usr/local/bin/lecrev-provision-control-plane-tls`

The control plane should keep only its local `LECREV_S3_BUCKET`. Do not set `LECREV_S3_REGION_BUCKETS` there; that mapping belongs on the build worker.

## 3. Build-worker host layout

Main services:

- `lecrev-build-worker`

Important files:

- env: `/etc/lecrev/build-worker.env`
- binary: `/opt/lecrev/bin/lecrev`

The build worker is also where real cross-region artifact replication is configured. `LECREV_S3_REGION_BUCKETS` should map each execution region to the bucket that will hold that region's bundle and startup objects.

## 4. Execution host layout

Main services:

- `lecrev-firecracker-network`
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

`/etc/lecrev/node-agent.env` should set `LECREV_S3_BUCKET` to the bucket assigned to `LECREV_NODE_AGENT_REGION` by the build worker's `LECREV_S3_REGION_BUCKETS` mapping.

## 5. Redeploy commands

Redeploy the control plane and frontend:

```bash
cd /Users/ishaan/eeeverc
bash deploy/ec2/deploy-control-plane.sh "${CONTROL_PLANE_PUBLIC_HOST}" "${SSH_KEY_PATH}" "${FRONTEND_DIR}"
```

Issue or renew the Let's Encrypt certificate set after the domains resolve:

```bash
ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo /usr/local/bin/lecrev-provision-control-plane-tls'
```

Redeploy the execution host over the private address:

```bash
cd /Users/ishaan/eeeverc
bash deploy/ec2/deploy-execution-host.sh "${EXECUTION_HOST_PRIVATE}" "${SSH_KEY_PATH}"
```

Redeploy the build worker over the private address:

```bash
cd /Users/ishaan/eeeverc
bash deploy/ec2/deploy-build-worker.sh "${BUILD_HOST_PRIVATE}" "${SSH_KEY_PATH}"
```

If `LECREV_SSH_PROXY_JUMP` is set, the deploy scripts automatically use a proxy command that reuses the same PEM for the jump host. If the execution host still has a temporary public IP during bootstrap, you can use that first and remove it later.

## 6. Service start, stop, restart

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

Build-worker service:

```bash
ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${BUILD_HOST_PRIVATE}" \
  'sudo systemctl status lecrev-build-worker --no-pager'

ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${BUILD_HOST_PRIVATE}" \
  'sudo systemctl restart lecrev-build-worker'

ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${BUILD_HOST_PRIVATE}" \
  'sudo systemctl stop lecrev-build-worker'

ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${BUILD_HOST_PRIVATE}" \
  'sudo systemctl start lecrev-build-worker'
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

## 7. Log inspection

Control-plane logs:

```bash
ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo journalctl -u lecrev-control-plane -n 200 --no-pager'

ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo journalctl -u nginx -n 100 --no-pager'

ssh -i "${SSH_KEY_PATH}" ec2-user@"${CONTROL_PLANE_PUBLIC_HOST}" \
  'sudo /usr/local/bin/lecrev-render-control-plane-nginx && sudo nginx -t'
```

Build-worker logs:

```bash
ssh -i "${SSH_KEY_PATH}" \
  -o "ProxyCommand=ssh -i ${SSH_KEY_PATH} -o StrictHostKeyChecking=no ${LECREV_SSH_PROXY_JUMP} -W %h:%p" \
  ec2-user@"${BUILD_HOST_PRIVATE}" \
  'sudo journalctl -u lecrev-build-worker -n 200 --no-pager'
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

## 8. Health and inventory checks

Public health:

```bash
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/healthz"
curl -sS "http://${CONTROL_PLANE_PUBLIC_HOST}/metrics"
curl -I "https://${FRONTEND_HOST}"
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

## 9. End-to-end smoke

Run the public smoke test from the repo:

```bash
cd /Users/ishaan/eeeverc
go run ./cmd/lecrev smoke \
  --api "http://${CONTROL_PLANE_PUBLIC_HOST}" \
  --api-key '<bootstrap-api-key>' \
  --project demo \
  --regions ap-south-1
```

## 10. Function URL operations

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

## 11. Stop instances to control spend

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
  --instance-ids <control-plane-instance-id> <build-host-instance-id> <execution-host-instance-id>
```

Start them again:

```bash
aws ec2 start-instances \
  --region "${AWS_REGION}" \
  --instance-ids <control-plane-instance-id> <build-host-instance-id> <execution-host-instance-id>
```

Wait for state changes:

```bash
aws ec2 wait instance-stopped --region "${AWS_REGION}" --instance-ids <instance-id>
aws ec2 wait instance-running --region "${AWS_REGION}" --instance-ids <instance-id>
```

## 12. Remove execution-host public exposure

Recommended target state:

- control plane keeps the public ingress
- execution host has no public IP
- execution host SSH allows access only from the control-plane security group or SSM
- execution host reaches S3 privately through a gateway endpoint or NAT

If the execution host subnet does not have NAT, create an S3 gateway endpoint before removing the worker public IP:

```bash
aws ec2 create-vpc-endpoint \
  --region "${AWS_REGION}" \
  --vpc-id <vpc-id> \
  --service-name "com.amazonaws.${AWS_REGION}.s3" \
  --vpc-endpoint-type Gateway \
  --route-table-ids <route-table-id>
```

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

## 13. Current platform constraints

- `networkPolicy=full` now scales up to the configured tap-pool size on each execution host. Keep `LECREV_EXECUTION_HOST_FULL_NETWORK_SLOTS` less than or equal to both `LECREV_FIRECRACKER_TAP_COUNT` and the host vCPU count.
- Full-network functions now create tap-scoped blank and function-warm snapshots, so first-hit deploys can warm on the build path and later invokes can restore a tap-matched prepared snapshot safely.
- The control plane is the only public entrypoint.
- Function URLs are synchronous on the control plane and should be kept within the configured function timeout envelope.
