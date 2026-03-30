#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${1:-/etc/lecrev/control-plane.env}"
RENDER_SCRIPT="${2:-/usr/local/bin/lecrev-render-control-plane-nginx}"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "missing env file: ${ENV_FILE}" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

: "${LECREV_FRONTEND_HOST:?LECREV_FRONTEND_HOST is required}"
: "${LECREV_API_HOST:?LECREV_API_HOST is required}"
: "${LECREV_FUNCTIONS_HOST:?LECREV_FUNCTIONS_HOST is required}"
: "${LECREV_ACME_EMAIL:?LECREV_ACME_EMAIL is required}"

WEBROOT_DIR="${LECREV_CERTBOT_WEBROOT:-/var/www/certbot}"

sudo dnf install -y certbot
sudo mkdir -p "${WEBROOT_DIR}"

sudo "${RENDER_SCRIPT}"
sudo nginx -t
sudo systemctl reload nginx

sudo certbot certonly \
  --non-interactive \
  --agree-tos \
  --email "${LECREV_ACME_EMAIL}" \
  --webroot \
  -w "${WEBROOT_DIR}" \
  -d "${LECREV_FRONTEND_HOST}" \
  -d "${LECREV_API_HOST}" \
  -d "${LECREV_FUNCTIONS_HOST}" \
  --keep-until-expiring

sudo "${RENDER_SCRIPT}"
sudo nginx -t
sudo systemctl reload nginx
