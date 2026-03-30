#!/usr/bin/env bash
set -euo pipefail

APP_USER="${APP_USER:-lecrev}"

checks=(
  "/dev/kvm"
  "/usr/local/bin/firecracker"
  "/usr/local/bin/jailer"
  "/usr/local/bin/node"
  "/opt/lecrev/bin/lecrev-guest-runner"
  "/var/lib/lecrev/vmlinux"
  "/var/lib/lecrev/rootfs.ext4"
  "/var/lib/lecrev/runtime"
  "/var/lib/lecrev/runtime/snapshots"
)

failures=0
for path in "${checks[@]}"; do
  if [[ -e "${path}" ]]; then
    echo "[ok] ${path}"
  else
    echo "[missing] ${path}" >&2
    failures=1
  fi
done

if id -u "${APP_USER}" >/dev/null 2>&1; then
  if sudo -u "${APP_USER}" test -r /dev/kvm && sudo -u "${APP_USER}" test -w /dev/kvm; then
    echo "[ok] ${APP_USER} can access /dev/kvm"
  else
    echo "[missing] ${APP_USER} lacks read/write access to /dev/kvm" >&2
    failures=1
  fi
else
  echo "[missing] app user ${APP_USER} does not exist" >&2
  failures=1
fi

if command -v firecracker >/dev/null 2>&1; then
  firecracker --version || true
fi

exit "${failures}"
