#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <output-dir> <server-name> [server-ip ...]" >&2
  exit 1
fi

OUT_DIR="$1"
SERVER_NAME="$2"
shift 2
SERVER_IPS=("$@")

mkdir -p "${OUT_DIR}"
umask 077

CA_KEY="${OUT_DIR}/ca-key.pem"
CA_CERT="${OUT_DIR}/ca.pem"
SERVER_KEY="${OUT_DIR}/server-key.pem"
SERVER_CSR="${OUT_DIR}/server.csr"
SERVER_CERT="${OUT_DIR}/server.pem"
CLIENT_KEY="${OUT_DIR}/client-key.pem"
CLIENT_CSR="${OUT_DIR}/client.csr"
CLIENT_CERT="${OUT_DIR}/client.pem"
SERVER_EXT="${OUT_DIR}/server-ext.cnf"
CLIENT_EXT="${OUT_DIR}/client-ext.cnf"

openssl genrsa -out "${CA_KEY}" 4096 >/dev/null 2>&1
openssl req -x509 -new -nodes -key "${CA_KEY}" -sha256 -days 365 \
  -subj "/CN=lecrev-grpc-ca" \
  -out "${CA_CERT}" >/dev/null 2>&1

openssl genrsa -out "${SERVER_KEY}" 4096 >/dev/null 2>&1
openssl req -new -key "${SERVER_KEY}" -subj "/CN=${SERVER_NAME}" -out "${SERVER_CSR}" >/dev/null 2>&1
{
  echo "basicConstraints=CA:FALSE"
  echo "keyUsage=digitalSignature,keyEncipherment"
  echo "extendedKeyUsage=serverAuth"
  echo "subjectAltName=@alt_names"
  echo "[alt_names]"
  echo "DNS.1=${SERVER_NAME}"
  idx=2
  for ip in "${SERVER_IPS[@]}"; do
    echo "IP.${idx}=${ip}"
    idx=$((idx + 1))
  done
} >"${SERVER_EXT}"
openssl x509 -req -in "${SERVER_CSR}" -CA "${CA_CERT}" -CAkey "${CA_KEY}" -CAcreateserial \
  -out "${SERVER_CERT}" -days 365 -sha256 -extfile "${SERVER_EXT}" >/dev/null 2>&1

openssl genrsa -out "${CLIENT_KEY}" 4096 >/dev/null 2>&1
openssl req -new -key "${CLIENT_KEY}" -subj "/CN=lecrev-node-agent" -out "${CLIENT_CSR}" >/dev/null 2>&1
{
  echo "basicConstraints=CA:FALSE"
  echo "keyUsage=digitalSignature,keyEncipherment"
  echo "extendedKeyUsage=clientAuth"
} >"${CLIENT_EXT}"
openssl x509 -req -in "${CLIENT_CSR}" -CA "${CA_CERT}" -CAkey "${CA_KEY}" -CAcreateserial \
  -out "${CLIENT_CERT}" -days 365 -sha256 -extfile "${CLIENT_EXT}" >/dev/null 2>&1

rm -f "${SERVER_CSR}" "${CLIENT_CSR}" "${SERVER_EXT}" "${CLIENT_EXT}" "${OUT_DIR}/ca.srl"

echo "generated grpc mTLS assets in ${OUT_DIR}"
echo "server SAN DNS: ${SERVER_NAME}"
if [[ ${#SERVER_IPS[@]} -gt 0 ]]; then
  printf 'server SAN IPs: %s\n' "${SERVER_IPS[*]}"
fi
