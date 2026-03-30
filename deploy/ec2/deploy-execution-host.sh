#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <ec2-host> <ssh-key-path>" >&2
  exit 1
fi

HOST="$1"
KEY_PATH="$2"
PROXY_JUMP="${LECREV_SSH_PROXY_JUMP:-}"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP_DIR="$(mktemp -d)"
REMOTE_TMP="/tmp/lecrev-execution-host-deploy"
trap 'rm -rf "${TMP_DIR}"' EXIT

SSH_ARGS=(-i "${KEY_PATH}" -o StrictHostKeyChecking=no)
if [[ -n "${PROXY_JUMP}" ]]; then
  SSH_ARGS+=(-J "${PROXY_JUMP}")
fi

cd "${ROOT_DIR}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${TMP_DIR}/lecrev" ./cmd/lecrev
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${TMP_DIR}/lecrev-guest-runner" ./cmd/lecrev-guest-runner

cp deploy/ec2/lecrev-node-agent.service "${TMP_DIR}/"
cp deploy/ec2/build-firecracker-rootfs.sh "${TMP_DIR}/"
cp deploy/ec2/check-firecracker-host.sh "${TMP_DIR}/"

ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
tar -C "${TMP_DIR}" -cf - . | ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "tar -C '${REMOTE_TMP}' -xf -"

ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "\
  test -f /etc/lecrev/node-agent.env && \
  sudo install -m 0755 '${REMOTE_TMP}/lecrev' /opt/lecrev/bin/lecrev && \
  sudo install -m 0755 '${REMOTE_TMP}/lecrev-guest-runner' /opt/lecrev/bin/lecrev-guest-runner && \
  sudo install -m 0644 '${REMOTE_TMP}/lecrev-node-agent.service' /etc/systemd/system/lecrev-node-agent.service && \
  sudo install -m 0755 '${REMOTE_TMP}/build-firecracker-rootfs.sh' /usr/local/bin/lecrev-build-firecracker-rootfs && \
  sudo install -m 0755 '${REMOTE_TMP}/check-firecracker-host.sh' /usr/local/bin/lecrev-check-firecracker-host && \
  sudo APP_USER=lecrev bash /usr/local/bin/lecrev-build-firecracker-rootfs && \
  sudo APP_USER=lecrev bash /usr/local/bin/lecrev-check-firecracker-host && \
  sudo systemctl daemon-reload && \
  sudo systemctl enable lecrev-node-agent && \
  sudo systemctl restart lecrev-node-agent"

echo "deployed lecrev execution host to ${HOST}"
