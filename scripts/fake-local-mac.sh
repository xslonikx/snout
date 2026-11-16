#!/bin/sh
#
# fake-local-mac.sh - add a synthetic "VM" MAC to an OVS bridge so that Snout
# announces it as an EVPN Type-2 route.
#
# The script creates an internal OVS port (an access port of VLAN on a
# VLAN-aware bridge, untagged on a flat bridge) and installs a static FDB
# entry for MAC on it. Static entries do not age out, so the route stays
# announced until "del" or "cleanup".

set -eu

BRIDGE=${BRIDGE:-br0}
PORT=${PORT:-fake-vm0}
MAC=${MAC:-02:00:00:00:00:42}
VLAN=${VLAN:-100}

MARKER=external_ids:snout-fake-mac-test

usage() {
	cat <<EOF
Usage: $0 {add|del|show|cleanup}

Environment variables:
  BRIDGE  OVS bridge name                              (default: br0)
  PORT    fake local interface name                    (default: fake-vm0)
  MAC     fake local MAC address                       (default: 02:00:00:00:00:42)
  VLAN    VLAN of the fake port; 0 = flat bridge       (default: 100)

VLAN must be an active VLAN of BRIDGE in snout.toml (listed in vlans or in a
[[bridge.vni]] entry); use VLAN=0 for a flat bridge (vni = ...). MACs in
other VLANs are ignored by Snout.

Examples:
  sudo $0 add
  sudo VLAN=200 PORT=fake-vm1 MAC=02:00:00:00:00:43 $0 add
  sudo $0 del
  sudo $0 cleanup
EOF
}

die() {
	echo "$*" >&2
	exit 1
}

need_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

validate() {
	case $VLAN in
	'' | *[!0-9]*) die "VLAN must be a number from 0 to 4094, got '$VLAN'" ;;
	esac
	[ "$VLAN" -le 4094 ] || die "VLAN must be a number from 0 to 4094, got '$VLAN'"
	echo "$MAC" | grep -Eq '^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$' || die "invalid MAC address '$MAC'"
	if [ -z "$PORT" ] || [ ${#PORT} -gt 15 ]; then
		die "PORT must be 1-15 characters, got '$PORT'"
	fi
}

require_bridge() {
	ovs-vsctl br-exists "$BRIDGE" || die "OVS bridge does not exist: $BRIDGE"
}

add_mac() {
	require_bridge
	if [ "$VLAN" -eq 0 ]; then
		ovs-vsctl --may-exist add-port "$BRIDGE" "$PORT" \
			-- set Interface "$PORT" type=internal "$MARKER=true" \
			-- clear Port "$PORT" tag vlan_mode
	else
		ovs-vsctl --may-exist add-port "$BRIDGE" "$PORT" \
			-- set Interface "$PORT" type=internal "$MARKER=true" \
			-- set Port "$PORT" tag="$VLAN" vlan_mode=access
	fi
	if command -v ip >/dev/null 2>&1; then
		ip link set "$PORT" up
	fi
	ovs-appctl fdb/add "$BRIDGE" "$PORT" "$VLAN" "$MAC"

	echo "added $MAC to $BRIDGE on $PORT (VLAN $VLAN)"
	ovs-appctl fdb/show "$BRIDGE"
}

delete_mac() {
	ovs-appctl fdb/del "$BRIDGE" "$VLAN" "$MAC"
	echo "deleted $MAC from $BRIDGE (VLAN $VLAN); Snout withdraws it after the next poll"
}

# cleanup removes the FDB entry and every port this script created on BRIDGE,
# including ports left by older versions of the script.
cleanup() {
	ovs-appctl fdb/del "$BRIDGE" "$VLAN" "$MAC" >/dev/null 2>&1 || true
	for interface in $(ovs-vsctl --data=bare --no-heading --columns=name find Interface "$MARKER=true"); do
		if [ "$(ovs-vsctl port-to-br "$interface" 2>/dev/null || true)" = "$BRIDGE" ]; then
			ovs-vsctl --if-exists del-port "$BRIDGE" "$interface"
			echo "removed test port $interface"
		fi
	done
	echo "removed test MAC and ports from $BRIDGE"
}

case ${1:-} in
-h | --help | help)
	usage
	exit 0
	;;
esac

need_command ovs-vsctl
need_command ovs-appctl
validate

case ${1:-} in
add)
	add_mac
	;;
del)
	delete_mac
	;;
show)
	ovs-appctl fdb/show "$BRIDGE"
	;;
cleanup)
	cleanup
	;;
*)
	usage >&2
	exit 2
	;;
esac
