#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  exec sudo "$0" "$@"
fi

APP_USER="${APP_USER:-lecrev}"
NODE_BASE_URL="${NODE_BASE_URL:-https://nodejs.org/dist/latest-v22.x}"
NATS_RELEASE_API_URL="${NATS_RELEASE_API_URL:-https://api.github.com/repos/nats-io/nats-server/releases/latest}"

dnf install -y git nginx postgresql15 postgresql15-server tar gzip xz jq

install -d -m 0755 /opt /usr/local/bin /etc/lecrev /var/www/lecrev /var/lib/lecrev /var/lib/nats

if ! id -u "${APP_USER}" >/dev/null 2>&1; then
  useradd --system --home-dir /opt/lecrev --create-home --shell /sbin/nologin "${APP_USER}"
fi

install -d -o "${APP_USER}" -g "${APP_USER}" -m 0755 /opt/lecrev/bin /opt/lecrev/releases /var/lib/lecrev /var/lib/nats
chown -R "${APP_USER}:${APP_USER}" /opt/lecrev /var/lib/lecrev /var/lib/nats

if [[ ! -x /usr/local/bin/node ]]; then
  node_shasums="$(curl -fsSL "${NODE_BASE_URL}/SHASUMS256.txt")"
  node_tarball="$(python3 -c 'import re,sys
for line in sys.stdin.read().splitlines():
    if re.search(r"linux-x64\\.tar\\.xz$", line):
        parts = line.split()
        if len(parts) >= 2:
            print(parts[1])
            break
' <<<"${node_shasums}")"
  if [[ -z "${node_tarball}" ]]; then
    echo "failed to discover a Node.js v22 Linux tarball from ${NODE_BASE_URL}" >&2
    exit 1
  fi
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir}"' EXIT
  curl -fsSL "${NODE_BASE_URL}/${node_tarball}" -o "${tmpdir}/node.tar.xz"
  tar -xJf "${tmpdir}/node.tar.xz" -C /opt
  node_dir="/opt/${node_tarball%.tar.xz}"
  ln -sfn "${node_dir}" /opt/node22
  ln -sfn /opt/node22/bin/node /usr/local/bin/node
  ln -sfn /opt/node22/bin/npm /usr/local/bin/npm
  ln -sfn /opt/node22/bin/npx /usr/local/bin/npx
  trap - EXIT
  rm -rf "${tmpdir}"
fi

if [[ ! -x /usr/local/bin/nats-server ]]; then
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir}"' EXIT
  nats_release_json="$(curl -fsSL "${NATS_RELEASE_API_URL}")"
  nats_url="$(python3 -c 'import json,sys
data=json.load(sys.stdin)
for asset in data.get("assets", []):
    url=asset.get("browser_download_url", "")
    if "linux-amd64.tar.gz" in url:
        print(url)
        break
' <<<"${nats_release_json}")"
  if [[ -z "${nats_url}" ]]; then
    echo "failed to discover latest nats-server Linux amd64 tarball" >&2
    exit 1
  fi
  curl -fsSL "${nats_url}" -o "${tmpdir}/nats-server.tgz"
  tar -xzf "${tmpdir}/nats-server.tgz" -C "${tmpdir}"
  nats_bin="$(find "${tmpdir}" -type f -name nats-server -print -quit)"
  install -m 0755 "${nats_bin}" /usr/local/bin/nats-server
  trap - EXIT
  rm -rf "${tmpdir}"
fi

echo "runtime dependencies installed"
