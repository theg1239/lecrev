#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  exec sudo bash "$0" "$@"
fi

APP_USER="${APP_USER:-lecrev}"
TAP_DEVICE_PREFIX="${LECREV_FIRECRACKER_TAP_DEVICE_PREFIX:-}"
TAP_COUNT="${LECREV_FIRECRACKER_TAP_COUNT:-}"
TAP_NETWORK_CIDR="${LECREV_FIRECRACKER_TAP_NETWORK_CIDR:-}"
TAP_SUBNET_PREFIX="${LECREV_FIRECRACKER_TAP_SUBNET_PREFIX:-30}"

LEGACY_TAP_DEVICE="${TAP_DEVICE:-${LECREV_FIRECRACKER_TAP_DEVICE:-tap0}}"
LEGACY_HOST_CIDR="${HOST_CIDR:-172.16.0.1/30}"
LEGACY_NETWORK_CIDR="${NETWORK_CIDR:-172.16.0.0/30}"

UPLINK_IFACE="${UPLINK_IFACE:-$(ip route show default | awk '/default/ { print $5; exit }')}"
SYSCTL_FILE="${SYSCTL_FILE:-/etc/sysctl.d/99-lecrev-firecracker.conf}"

if [[ -z "${UPLINK_IFACE}" ]]; then
  echo "failed to determine uplink interface; set UPLINK_IFACE explicitly" >&2
  exit 1
fi

dnf install -y iproute iptables python3

cat >"${SYSCTL_FILE}" <<'EOF'
net.ipv4.ip_forward=1
EOF
sysctl -w net.ipv4.ip_forward=1 >/dev/null

ensure_forwarding_rules() {
  local tap_device="$1"
  local network_cidr="$2"

  iptables -t nat -C POSTROUTING -s "${network_cidr}" -o "${UPLINK_IFACE}" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -s "${network_cidr}" -o "${UPLINK_IFACE}" -j MASQUERADE
  iptables -C FORWARD -i "${tap_device}" -o "${UPLINK_IFACE}" -j ACCEPT 2>/dev/null || \
    iptables -A FORWARD -i "${tap_device}" -o "${UPLINK_IFACE}" -j ACCEPT
  iptables -C FORWARD -i "${UPLINK_IFACE}" -o "${tap_device}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
    iptables -A FORWARD -i "${UPLINK_IFACE}" -o "${tap_device}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
}

configure_tap() {
  local tap_device="$1"
  local host_cidr="$2"
  local network_cidr="$3"

  if ! ip link show "${tap_device}" >/dev/null 2>&1; then
    ip tuntap add dev "${tap_device}" mode tap user "${APP_USER}"
  fi

  ip addr replace "${host_cidr}" dev "${tap_device}"
  ip link set "${tap_device}" up
  ensure_forwarding_rules "${tap_device}" "${network_cidr}"
}

if [[ -n "${TAP_DEVICE_PREFIX}" && -n "${TAP_COUNT}" && -n "${TAP_NETWORK_CIDR}" ]]; then
  while IFS='|' read -r tap_device host_cidr network_cidr; do
    configure_tap "${tap_device}" "${host_cidr}" "${network_cidr}"
  done < <(
    python3 - "${TAP_DEVICE_PREFIX}" "${TAP_COUNT}" "${TAP_NETWORK_CIDR}" "${TAP_SUBNET_PREFIX}" <<'PY'
import ipaddress
import sys

prefix = sys.argv[1]
count = int(sys.argv[2])
network = ipaddress.ip_network(sys.argv[3], strict=True)
subnet_prefix = int(sys.argv[4])

subnets = network.subnets(new_prefix=subnet_prefix)
for index in range(count):
    subnet = next(subnets, None)
    if subnet is None:
        raise SystemExit(f"tap network {network} does not have {count} subnets of /{subnet_prefix}")
    host = subnet.network_address + 1
    print(f"{prefix}{index}|{host}/{subnet_prefix}|{subnet}")
PY
  )
  echo "configured ${TAP_COUNT} tap devices with prefix ${TAP_DEVICE_PREFIX} from ${TAP_NETWORK_CIDR} via ${UPLINK_IFACE}"
  exit 0
fi

configure_tap "${LEGACY_TAP_DEVICE}" "${LEGACY_HOST_CIDR}" "${LEGACY_NETWORK_CIDR}"
echo "configured tap device ${LEGACY_TAP_DEVICE} on ${LEGACY_HOST_CIDR} with NAT via ${UPLINK_IFACE}"
