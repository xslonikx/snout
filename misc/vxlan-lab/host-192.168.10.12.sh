#!/bin/sh

set -eu

BRIDGE=${BRIDGE:-ovsbr0}
VNI=${VNI:-100}
VXLAN_PORT=${VXLAN_PORT:-4789}
LOCAL_IP=192.168.10.12

if [ "$(id -u)" -ne 0 ]; then
	echo "run this script as root" >&2
	exit 1
fi

ovs-vsctl --may-exist add-br "$BRIDGE"

ovs-vsctl --may-exist add-port "$BRIDGE" vxlan-11 \
	-- set Interface vxlan-11 type=vxlan options:local_ip="$LOCAL_IP" options:remote_ip=192.168.10.11 options:key="$VNI" options:dst_port="$VXLAN_PORT" \
	-- set Port vxlan-11 protected=true

ovs-vsctl --may-exist add-port "$BRIDGE" vxlan-13 \
	-- set Interface vxlan-13 type=vxlan options:local_ip="$LOCAL_IP" options:remote_ip=192.168.10.13 options:key="$VNI" options:dst_port="$VXLAN_PORT" \
	-- set Port vxlan-13 protected=true

ovs-vsctl --may-exist add-port "$BRIDGE" vxlan-mikrotik \
	-- set Interface vxlan-mikrotik type=vxlan options:local_ip="$LOCAL_IP" options:remote_ip=192.168.10.1 options:key="$VNI" options:dst_port="$VXLAN_PORT" \
	-- set Port vxlan-mikrotik protected=true

ip link set "$BRIDGE" up
ip address replace 10.100.0.12/24 dev "$BRIDGE"

ovs-vsctl show
