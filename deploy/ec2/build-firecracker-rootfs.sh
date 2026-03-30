#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  exec sudo "$0" "$@"
fi

APP_USER="${APP_USER:-lecrev}"
STAGE_DIR="${STAGE_DIR:-/var/lib/lecrev/rootfs-stage}"
ROOTFS_PATH="${ROOTFS_PATH:-/var/lib/lecrev/rootfs.ext4}"
ROOTFS_SIZE_MB="${ROOTFS_SIZE_MB:-1536}"
NODE_SOURCE="${NODE_SOURCE:-/usr/local/bin/node}"
GUEST_RUNNER_SOURCE="${GUEST_RUNNER_SOURCE:-/opt/lecrev/bin/lecrev-guest-runner}"
GUEST_INIT_PATH="${GUEST_INIT_PATH:-/usr/local/bin/lecrev-guest-runner}"

if [[ ! -x "${NODE_SOURCE}" ]]; then
  echo "node binary not found: ${NODE_SOURCE}" >&2
  exit 1
fi

if [[ ! -x "${GUEST_RUNNER_SOURCE}" ]]; then
  echo "guest runner not found: ${GUEST_RUNNER_SOURCE}" >&2
  exit 1
fi

if ! command -v mkfs.ext4 >/dev/null 2>&1; then
  echo "mkfs.ext4 is required; install e2fsprogs first" >&2
  exit 1
fi

node_real="$(readlink -f "${NODE_SOURCE}")"

rm -rf "${STAGE_DIR}"
install -d -m 0755 \
  "${STAGE_DIR}/dev" \
  "${STAGE_DIR}/dev/pts" \
  "${STAGE_DIR}/dev/shm" \
  "${STAGE_DIR}/etc" \
  "${STAGE_DIR}/proc" \
  "${STAGE_DIR}/sys" \
  "${STAGE_DIR}/tmp" \
  "${STAGE_DIR}/usr/bin" \
  "${STAGE_DIR}/usr/local/bin" \
  "${STAGE_DIR}/var/lib/lecrev/functions"

install -m 0755 "${node_real}" "${STAGE_DIR}/usr/local/bin/node"
ln -sfn /usr/local/bin/node "${STAGE_DIR}/usr/bin/node"
install -D -m 0755 "${GUEST_RUNNER_SOURCE}" "${STAGE_DIR}${GUEST_INIT_PATH}"

write_text_file() {
  local path="$1"
  shift
  mkdir -p "$(dirname "${path}")"
  printf '%s\n' "$1" >"${path}"
}

copy_with_parents() {
  local source_path="$1"
  if [[ ! -e "${source_path}" && ! -L "${source_path}" ]]; then
    return 0
  fi
  local dest_path="${STAGE_DIR}${source_path}"
  mkdir -p "$(dirname "${dest_path}")"
  if [[ -L "${source_path}" ]]; then
    cp -a "${source_path}" "${dest_path}"
    local resolved
    resolved="$(readlink -f "${source_path}")"
    if [[ "${resolved}" != "${source_path}" ]]; then
      copy_with_parents "${resolved}"
    fi
    return 0
  fi
  cp -a "${source_path}" "${dest_path}"
}

mapfile -t deps < <(ldd "${node_real}" | awk '/=> \// { print $3 } /^\// { print $1 }' | sort -u)
for dep in "${deps[@]}"; do
  copy_with_parents "${dep}"
done

for candidate in \
  /etc/hosts \
  /etc/nsswitch.conf \
  /etc/resolv.conf \
  /etc/localtime \
  /etc/pki/tls/certs/ca-bundle.crt \
  /etc/ssl/certs/ca-bundle.crt \
  /etc/ssl/certs/ca-certificates.crt
do
  if [[ -e "${candidate}" ]]; then
    copy_with_parents "${candidate}"
  fi
done

write_text_file "${STAGE_DIR}/etc/passwd" "root:x:0:0:root:/root:/sbin/nologin
lecrev:x:1000:1000:Lecrev:/var/lib/lecrev:/sbin/nologin"
write_text_file "${STAGE_DIR}/etc/group" "root:x:0:
lecrev:x:1000:"

truncate -s "${ROOTFS_SIZE_MB}M" "${ROOTFS_PATH}"
mkfs.ext4 -q -F -d "${STAGE_DIR}" "${ROOTFS_PATH}"
chmod 0644 "${ROOTFS_PATH}"
chown "${APP_USER}:${APP_USER}" "${ROOTFS_PATH}"

echo "built Firecracker rootfs at ${ROOTFS_PATH}"
