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
REMOTE_TMP="/tmp/lecrev-build-worker-deploy"
trap 'rm -rf "${TMP_DIR}"' EXIT

export COPYFILE_DISABLE=1
export COPY_EXTENDED_ATTRIBUTES_DISABLE=1

SSH_ARGS=(-i "${KEY_PATH}" -o StrictHostKeyChecking=no)
if [[ -n "${PROXY_JUMP}" ]]; then
  SSH_ARGS+=(-o "ProxyCommand=ssh -i ${KEY_PATH} -o StrictHostKeyChecking=no ${PROXY_JUMP} -W %h:%p")
fi

cd "${ROOT_DIR}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${TMP_DIR}/lecrev" ./cmd/lecrev

cp deploy/ec2/lecrev-build-worker.service "${TMP_DIR}/"

ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
tar -C "${TMP_DIR}" -cf - . | ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "tar -C '${REMOTE_TMP}' -xf -"

ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "\
  test -f /etc/lecrev/build-worker.env && \
  sudo chown root:lecrev /etc/lecrev/build-worker.env && \
  sudo chmod 0640 /etc/lecrev/build-worker.env && \
  sudo install -o lecrev -g lecrev -m 0755 '${REMOTE_TMP}/lecrev' /opt/lecrev/bin/lecrev && \
  sudo install -m 0644 '${REMOTE_TMP}/lecrev-build-worker.service' /etc/systemd/system/lecrev-build-worker.service && \
  sudo systemctl daemon-reload && \
  sudo systemctl enable lecrev-build-worker && \
  sudo systemctl restart lecrev-build-worker"

echo "deployed lecrev build-worker to ${HOST}"
