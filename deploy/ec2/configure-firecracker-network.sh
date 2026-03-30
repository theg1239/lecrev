#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  exec sudo bash "$0" "$@"
fi

APP_USER="${APP_USER:-lecrev}"
TAP_DEVICE="${TAP_DEVICE:-tap0}"
HOST_CIDR="${HOST_CIDR:-172.16.0.1/30}"
NETWORK_CIDR="${NETWORK_CIDR:-172.16.0.0/30}"
UPLINK_IFACE="${UPLINK_IFACE:-$(ip route show default | awk '/default/ { print $5; exit }')}"
SYSCTL_FILE="${SYSCTL_FILE:-/etc/sysctl.d/99-lecrev-firecracker.conf}"

if [[ -z "${UPLINK_IFACE}" ]]; then
  echo "failed to determine uplink interface; set UPLINK_IFACE explicitly" >&2
  exit 1
fi

dnf install -y iproute iptables

if ! ip link show "${TAP_DEVICE}" >/dev/null 2>&1; then
  ip tuntap add dev "${TAP_DEVICE}" mode tap user "${APP_USER}"
fi

ip addr replace "${HOST_CIDR}" dev "${TAP_DEVICE}"
ip link set "${TAP_DEVICE}" up

cat >"${SYSCTL_FILE}" <<'EOF'
net.ipv4.ip_forward=1
EOF
sysctl -w net.ipv4.ip_forward=1 >/dev/null

iptables -t nat -C POSTROUTING -s "${NETWORK_CIDR}" -o "${UPLINK_IFACE}" -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -s "${NETWORK_CIDR}" -o "${UPLINK_IFACE}" -j MASQUERADE
iptables -C FORWARD -i "${TAP_DEVICE}" -o "${UPLINK_IFACE}" -j ACCEPT 2>/dev/null || \
  iptables -A FORWARD -i "${TAP_DEVICE}" -o "${UPLINK_IFACE}" -j ACCEPT
iptables -C FORWARD -i "${UPLINK_IFACE}" -o "${TAP_DEVICE}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
  iptables -A FORWARD -i "${UPLINK_IFACE}" -o "${TAP_DEVICE}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

echo "configured tap device ${TAP_DEVICE} on ${HOST_CIDR} with NAT via ${UPLINK_IFACE}"
