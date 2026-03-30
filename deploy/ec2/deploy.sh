#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <ec2-host> <ssh-key-path> <frontend-dir>" >&2
  exit 1
fi

HOST="$1"
KEY_PATH="$2"
FRONTEND_DIR="$3"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP_DIR="$(mktemp -d)"
REMOTE_TMP="/tmp/lecrev-deploy"
trap 'rm -rf "${TMP_DIR}"' EXIT

cd "${ROOT_DIR}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${TMP_DIR}/lecrev" ./cmd/lecrev
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${TMP_DIR}/lecrev-guest-runner" ./cmd/lecrev-guest-runner

(
  cd "${FRONTEND_DIR}"
  npm run build >/dev/null
)

cp deploy/ec2/lecrev.service "${TMP_DIR}/"
cp deploy/ec2/nats-server.service "${TMP_DIR}/"
cp deploy/ec2/nginx.lecrev.conf "${TMP_DIR}/"
mkdir -p "${TMP_DIR}/frontend-dist"
cp -R "${FRONTEND_DIR}/dist/." "${TMP_DIR}/frontend-dist/"

ssh -i "${KEY_PATH}" -o StrictHostKeyChecking=no ec2-user@"${HOST}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
tar -C "${TMP_DIR}" -cf - . | ssh -i "${KEY_PATH}" -o StrictHostKeyChecking=no ec2-user@"${HOST}" "tar -C '${REMOTE_TMP}' -xf -"

ssh -i "${KEY_PATH}" -o StrictHostKeyChecking=no ec2-user@"${HOST}" "\
  test -f /etc/lecrev/lecrev.env && \
  sudo install -o lecrev -g lecrev -m 0755 '${REMOTE_TMP}/lecrev' /opt/lecrev/bin/lecrev && \
  sudo install -o lecrev -g lecrev -m 0755 '${REMOTE_TMP}/lecrev-guest-runner' /opt/lecrev/bin/lecrev-guest-runner && \
  sudo install -m 0644 '${REMOTE_TMP}/lecrev.service' /etc/systemd/system/lecrev.service && \
  sudo install -m 0644 '${REMOTE_TMP}/nats-server.service' /etc/systemd/system/nats-server.service && \
  sudo install -m 0644 '${REMOTE_TMP}/nginx.lecrev.conf' /etc/nginx/conf.d/lecrev.conf && \
  sudo rm -rf /var/www/lecrev/* && \
  sudo cp -R '${REMOTE_TMP}/frontend-dist/.' /var/www/lecrev/ && \
  sudo chown -R nginx:nginx /var/www/lecrev && \
  sudo nginx -t && \
  sudo systemctl daemon-reload && \
  sudo systemctl enable --now nats-server nginx lecrev"

echo "deployed lecrev to ${HOST}"
