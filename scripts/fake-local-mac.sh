#!/bin/sh

set -eu

BRIDGE=${BRIDGE:-br0}
PORT=${PORT:-fake-vm0}
MAC=${MAC:-02:00:00:00:00:42}
VLAN=${VLAN:-0}
VXLAN_PORT=${VXLAN_PORT:-vxlan-test}
CREATE_VXLAN=${CREATE_VXLAN:-0}

usage() {
	cat <<EOF
Usage: $0 {add|del|show|cleanup}

Environment variables:
  BRIDGE        OVS bridge name               (default: br0)
  PORT          fake local interface name     (default: fake-vm0)
  MAC           fake local MAC address        (default: 02:00:00:00:00:42)
  VLAN          OVS FDB VLAN                   (default: 0)
  CREATE_VXLAN  create a flow VXLAN port       (default: 0)
  VXLAN_PORT    test VXLAN interface name      (default: vxlan-test)

Examples:
  sudo $0 add
  sudo MAC=02:00:00:00:00:43 $0 add
  sudo $0 del
  sudo $0 cleanup
EOF
}

need_command() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "required command not found: $1" >&2
		exit 1
	fi
}

bridge_has_vxlan() {
	for interface in $(ovs-vsctl list-ifaces "$BRIDGE"); do
		type=$(ovs-vsctl get Interface "$interface" type | tr -d '"')
		if [ "$type" = "vxlan" ]; then
			return 0
		fi
	done
	return 1
}

add_mac() {
	if ! ovs-vsctl br-exists "$BRIDGE"; then
		echo "OVS bridge does not exist: $BRIDGE" >&2
		exit 1
	fi

	if ! bridge_has_vxlan; then
		if [ "$CREATE_VXLAN" != "1" ]; then
			echo "$BRIDGE has no VXLAN interface; Snout will ignore it" >&2
			echo "set CREATE_VXLAN=1 to create a temporary flow-based VXLAN port" >&2
			exit 1
		fi
		ovs-vsctl --may-exist add-port "$BRIDGE" "$VXLAN_PORT" \
			-- set Interface "$VXLAN_PORT" type=vxlan options:remote_ip=flow options:key=flow \
			external_ids:snout-fake-mac-test=true
	fi

	ovs-vsctl --may-exist add-port "$BRIDGE" "$PORT" \
		-- set Interface "$PORT" type=internal external_ids:snout-fake-mac-test=true
	if command -v ip >/dev/null 2>&1; then
		ip link set "$PORT" up
	fi
	ovs-appctl fdb/add "$BRIDGE" "$PORT" "$VLAN" "$MAC"

	echo "added $MAC to $BRIDGE on $PORT (VLAN $VLAN)"
	ovs-appctl fdb/show "$BRIDGE"
}

delete_mac() {
	ovs-appctl fdb/del "$BRIDGE" "$VLAN" "$MAC"
	echo "deleted $MAC from $BRIDGE; Snout should withdraw it after the next poll"
}

cleanup() {
	ovs-appctl fdb/del "$BRIDGE" "$VLAN" "$MAC" >/dev/null 2>&1 || true
	ovs-vsctl --if-exists del-port "$BRIDGE" "$PORT"
	if [ "$(ovs-vsctl --if-exists get Interface "$VXLAN_PORT" external_ids:snout-fake-mac-test 2>/dev/null | tr -d '"')" = "true" ]; then
		ovs-vsctl --if-exists del-port "$BRIDGE" "$VXLAN_PORT"
	fi
	echo "removed test MAC and ports from $BRIDGE"
}

case ${1:-} in
	-h|--help|help)
		usage
		exit 0
		;;
esac

need_command ovs-vsctl
need_command ovs-appctl

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
