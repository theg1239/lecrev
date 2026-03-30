#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  exec sudo bash "$0" "$@"
fi

APP_USER="${APP_USER:-lecrev}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
STATE_DIR="${STATE_DIR:-/var/lib/lecrev}"
WORKSPACE_DIR="${WORKSPACE_DIR:-${STATE_DIR}/runtime}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-${WORKSPACE_DIR}/snapshots}"
CHROOT_BASE_DIR="${CHROOT_BASE_DIR:-/srv/jailer}"
FIRECRACKER_RELEASE_URL="${FIRECRACKER_RELEASE_URL:-https://github.com/firecracker-microvm/firecracker/releases}"

case "$(uname -m)" in
  x86_64|amd64)
    FIRECRACKER_ARCH="x86_64"
    ;;
  aarch64|arm64)
    FIRECRACKER_ARCH="aarch64"
    ;;
  *)
    echo "unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

dnf install -y jq tar gzip xz e2fsprogs rsync coreutils findutils

install -d -m 0755 "${INSTALL_DIR}" /etc/lecrev "${STATE_DIR}" "${WORKSPACE_DIR}" "${SNAPSHOT_DIR}" "${CHROOT_BASE_DIR}"

if ! id -u "${APP_USER}" >/dev/null 2>&1; then
  useradd --system --home-dir /opt/lecrev --create-home --shell /sbin/nologin "${APP_USER}"
fi

chown -R "${APP_USER}:${APP_USER}" "${STATE_DIR}"

if getent group kvm >/dev/null 2>&1; then
  usermod -aG kvm "${APP_USER}" || true
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

version="${FIRECRACKER_VERSION:-}"
if [[ -z "${version}" ]]; then
  version="$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${FIRECRACKER_RELEASE_URL}/latest")")"
fi
ci_version="${FIRECRACKER_CI_VERSION:-${version%.*}}"

release_tgz="${tmpdir}/firecracker.tgz"
curl -fsSL "${FIRECRACKER_RELEASE_URL}/download/${version}/firecracker-${version}-${FIRECRACKER_ARCH}.tgz" -o "${release_tgz}"
tar -xzf "${release_tgz}" -C "${tmpdir}"

firecracker_bin="$(find "${tmpdir}" -type f -name "firecracker-*-${FIRECRACKER_ARCH}" ! -name '*.sig' -print -quit)"
jailer_bin="$(find "${tmpdir}" -type f -name "jailer-*-${FIRECRACKER_ARCH}" ! -name '*.sig' -print -quit)"
snapshot_editor_bin="$(find "${tmpdir}" -type f -name "snapshot-editor-*-${FIRECRACKER_ARCH}" ! -name '*.sig' -print -quit)"

if [[ -z "${firecracker_bin}" || -z "${jailer_bin}" ]]; then
  echo "failed to locate firecracker and jailer binaries in release ${version}" >&2
  exit 1
fi

install -m 0755 "${firecracker_bin}" "${INSTALL_DIR}/firecracker"
install -m 0755 "${jailer_bin}" "${INSTALL_DIR}/jailer"
if [[ -n "${snapshot_editor_bin}" ]]; then
  install -m 0755 "${snapshot_editor_bin}" "${INSTALL_DIR}/snapshot-editor"
fi

kernel_listing="$(curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min?prefix=firecracker-ci/${ci_version}/${FIRECRACKER_ARCH}/vmlinux-&list-type=2")"
kernel_key="$(grep -oE "firecracker-ci/${ci_version}/${FIRECRACKER_ARCH}/vmlinux-[0-9]+\\.[0-9]+\\.[0-9]+" <<<"${kernel_listing}" | sort -V | tail -n 1)"
if [[ -z "${kernel_key}" ]]; then
  echo "failed to discover Firecracker CI kernel for ${ci_version}/${FIRECRACKER_ARCH}" >&2
  exit 1
fi

curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/${kernel_key}" -o "${STATE_DIR}/vmlinux"
chmod 0644 "${STATE_DIR}/vmlinux"
chown "${APP_USER}:${APP_USER}" "${STATE_DIR}/vmlinux"

cat >"${STATE_DIR}/firecracker-release.txt" <<EOF
version=${version}
ci_version=${ci_version}
kernel_key=${kernel_key}
installed_at=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
EOF
chown "${APP_USER}:${APP_USER}" "${STATE_DIR}/firecracker-release.txt"

echo "installed Firecracker ${version} for ${FIRECRACKER_ARCH}"
echo "kernel: ${kernel_key}"
echo "workspace: ${WORKSPACE_DIR}"
