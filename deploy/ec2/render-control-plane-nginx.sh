#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${1:-/etc/lecrev/control-plane.env}"
HTTP_TEMPLATE="${2:-/etc/lecrev/nginx.lecrev.http.conf.tmpl}"
HTTPS_TEMPLATE="${3:-/etc/lecrev/nginx.lecrev.https.conf.tmpl}"
OUTPUT_PATH="${4:-/etc/nginx/conf.d/lecrev.conf}"

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

TLS_CERT_NAME="${LECREV_TLS_CERT_NAME:-${LECREV_FRONTEND_HOST}}"
WEBROOT_DIR="${LECREV_CERTBOT_WEBROOT:-/var/www/certbot}"
mkdir -p "${WEBROOT_DIR}"

template="${HTTP_TEMPLATE}"
if [[ -f "/etc/letsencrypt/live/${TLS_CERT_NAME}/fullchain.pem" && -f "/etc/letsencrypt/live/${TLS_CERT_NAME}/privkey.pem" ]]; then
  template="${HTTPS_TEMPLATE}"
fi

sed \
  -e "s#__FRONTEND_HOST__#${LECREV_FRONTEND_HOST}#g" \
  -e "s#__API_HOST__#${LECREV_API_HOST}#g" \
  -e "s#__FUNCTIONS_HOST__#${LECREV_FUNCTIONS_HOST}#g" \
  -e "s#__TLS_CERT_NAME__#${TLS_CERT_NAME}#g" \
  -e "s#__CERTBOT_WEBROOT__#${WEBROOT_DIR}#g" \
  "${template}" >"${OUTPUT_PATH}"
