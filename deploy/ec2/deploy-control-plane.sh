#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <ec2-host> <ssh-key-path> <frontend-dir>" >&2
  exit 1
fi

HOST="$1"
KEY_PATH="$2"
FRONTEND_DIR="$3"
PROXY_JUMP="${LECREV_SSH_PROXY_JUMP:-}"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP_DIR="$(mktemp -d)"
REMOTE_TMP="/tmp/lecrev-control-plane-deploy"
trap 'rm -rf "${TMP_DIR}"' EXIT

export COPYFILE_DISABLE=1
export COPY_EXTENDED_ATTRIBUTES_DISABLE=1

SSH_ARGS=(-i "${KEY_PATH}" -o StrictHostKeyChecking=no)
if [[ -n "${PROXY_JUMP}" ]]; then
  SSH_ARGS+=(-o "ProxyCommand=ssh -i ${KEY_PATH} -o StrictHostKeyChecking=no ${PROXY_JUMP} -W %h:%p")
fi

cd "${ROOT_DIR}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${TMP_DIR}/lecrev" ./cmd/lecrev

(
  cd "${FRONTEND_DIR}"
  npm run build >/dev/null
)

cp deploy/ec2/lecrev-control-plane.service "${TMP_DIR}/"
cp deploy/ec2/nats-server.service "${TMP_DIR}/"
cp deploy/ec2/nginx.lecrev.http.conf.tmpl "${TMP_DIR}/"
cp deploy/ec2/nginx.lecrev.https.conf.tmpl "${TMP_DIR}/"
cp deploy/ec2/render-control-plane-nginx.sh "${TMP_DIR}/"
cp deploy/ec2/provision-control-plane-tls.sh "${TMP_DIR}/"
mkdir -p "${TMP_DIR}/frontend-dist"
cp -R "${FRONTEND_DIR}/dist/." "${TMP_DIR}/frontend-dist/"

ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
tar -C "${TMP_DIR}" -cf - . | ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "tar -C '${REMOTE_TMP}' -xf -"

ssh "${SSH_ARGS[@]}" ec2-user@"${HOST}" "\
  test -f /etc/lecrev/control-plane.env && \
  sudo chown root:lecrev /etc/lecrev/control-plane.env && \
  sudo chmod 0640 /etc/lecrev/control-plane.env && \
  sudo bash -lc 'shopt -s nullglob; for cert in /etc/lecrev/grpc/*.pem; do chown root:lecrev \"\$cert\"; chmod 0640 \"\$cert\"; done' && \
  sudo install -o lecrev -g lecrev -m 0755 '${REMOTE_TMP}/lecrev' /opt/lecrev/bin/lecrev && \
  sudo install -m 0644 '${REMOTE_TMP}/lecrev-control-plane.service' /etc/systemd/system/lecrev-control-plane.service && \
  sudo install -m 0644 '${REMOTE_TMP}/nats-server.service' /etc/systemd/system/nats-server.service && \
  sudo install -o root -g root -m 0644 '${REMOTE_TMP}/nginx.lecrev.http.conf.tmpl' /etc/lecrev/nginx.lecrev.http.conf.tmpl && \
  sudo install -o root -g root -m 0644 '${REMOTE_TMP}/nginx.lecrev.https.conf.tmpl' /etc/lecrev/nginx.lecrev.https.conf.tmpl && \
  sudo install -o root -g root -m 0755 '${REMOTE_TMP}/render-control-plane-nginx.sh' /usr/local/bin/lecrev-render-control-plane-nginx && \
  sudo install -o root -g root -m 0755 '${REMOTE_TMP}/provision-control-plane-tls.sh' /usr/local/bin/lecrev-provision-control-plane-tls && \
  sudo rm -rf /var/www/lecrev/* && \
  sudo mkdir -p /var/www/certbot && \
  sudo cp -R '${REMOTE_TMP}/frontend-dist/.' /var/www/lecrev/ && \
  sudo chown -R nginx:nginx /var/www/lecrev && \
  sudo /usr/local/bin/lecrev-render-control-plane-nginx && \
  sudo nginx -t && \
  sudo systemctl daemon-reload && \
  sudo systemctl enable nats-server nginx lecrev-control-plane && \
  sudo systemctl restart nats-server nginx lecrev-control-plane"

echo "deployed lecrev control-plane to ${HOST}"
