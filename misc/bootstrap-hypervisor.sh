#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
#
# bootstrap-hypervisor.sh - prepare an AlmaLinux 10 / Oracle Linux 10 VM as a
# Snout lab hypervisor (spec: docs/workflows/phase_1.txt, section H).
#
# Copy the whole misc/ directory to the VM and run as root:
#   bash misc/bootstrap-hypervisor.sh --env misc/lab/hv1.env

set -Eeuo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT_NAME=$(basename "${BASH_SOURCE[0]}")
# shellcheck source=lib/lab-common.sh
. "$SCRIPT_DIR/lib/lab-common.sh"

# Options that can be passed as --kebab-case CLI options or environment
# variables. Topology variables (NEIGH*_IP, RR_IP, SEGMENTS, VNI_OFFSET,
# OVERLAY_<vlan>, TRUNK_IF) are environment-only.
ADDRESS=${ADDRESS:-}
OPTION_VARS=(UPLINK_IF ADDRESS HOST_NAME BRIDGE OVERLAY_MTU ASN GOBGP_VERSION QA_USER QA_PASSWORD QA_SSH_KEY SNOUT_REPO SNOUT_REF)

usage() {
	cat <<EOF
Usage: $SCRIPT_NAME [--env FILE] [options]

Required environment variables (or lines in --env FILE):
  NEIGH1_IP, NEIGH2_IP   underlay IPs of the two other hypervisors
  RR_IP                  underlay IP of the route reflector
  UPLINK_IF              lab vNIC (must not carry the default route)
  ADDRESS                static lab address, e.g. 172.31.0.11/24

Optional environment variables:
  SEGMENTS               VLANs, items "vlan" or "vlan:vni" (default: 100,200)
  VNI_OFFSET             VNI = VLAN + VNI_OFFSET for plain items (default: 10000)
  OVERLAY_<vlan>         IP of test port test<vlan>, CIDR or "none"
                         (default: 10.<vlan>.0.<last underlay octet>/24 for VLAN <= 255)
  TRUNK_IF               extra NIC added to the bridge as a trunk of the SEGMENTS VLANs

Options (also accepted as environment variables, e.g. --overlay-mtu = OVERLAY_MTU):
  --env FILE             load KEY=VALUE lines from FILE first
  --uplink-if NAME       lab vNIC
  --address CIDR         static lab address
  --host-name NAME       set the hostname
  --bridge NAME          OVS bridge (default: br0)
  --overlay-mtu N        MTU of the test ports (default: 1450)
  --asn N                iBGP AS number (default: 65000)
  --gobgp-version VER    GoBGP release or "latest" (default: latest)
  --qa-user NAME         sudo user (default: QA)
  --qa-password PASS     password of the QA user
  --qa-ssh-key KEY|FILE  authorized SSH key of the QA user
  --snout-repo URL       Snout git repository (default: git@github.com:xslonikx/snout.git)
  --snout-ref REF        branch, tag or commit to build (default: main)
  -h, --help             show this help

Precondition: root's ~/.ssh must hold a key that can clone the Snout repo.
EOF
}

# ---------------------------------------------------------------------------
# Input
# ---------------------------------------------------------------------------
apply_defaults() {
	BRIDGE=${BRIDGE:-br0}
	OVERLAY_MTU=${OVERLAY_MTU:-1450}
	ASN=${ASN:-65000}
	SEGMENTS=${SEGMENTS:-100,200}
	VNI_OFFSET=${VNI_OFFSET:-10000}
	TRUNK_IF=${TRUNK_IF:-}
	GOBGP_VERSION=${GOBGP_VERSION:-latest}
}

# parse_segments: fills SEGMENT_VLANS (ordered), SEGMENT_VNI[vlan],
# SEGMENT_EXPLICIT[vlan]=1 for "vlan:vni" items.
parse_segments() {
	SEGMENT_VLANS=()
	declare -gA SEGMENT_VNI=() SEGMENT_EXPLICIT=()
	declare -A owner=()
	local item vlan vni
	if ! is_uint "$VNI_OFFSET" || ((10#$VNI_OFFSET >= 16777215)); then
		die "VNI_OFFSET must be 0-16777214, got '$VNI_OFFSET'"
	fi
	IFS=',' read -r -a items <<<"$SEGMENTS"
	for item in "${items[@]}"; do
		item=${item//[[:space:]]/}
		[[ -n $item ]] || continue
		if [[ $item =~ ^([0-9]+):([0-9]+)$ ]]; then
			vlan=$((10#${BASH_REMATCH[1]}))
			vni=$((10#${BASH_REMATCH[2]}))
			SEGMENT_EXPLICIT[$vlan]=1
		elif [[ $item =~ ^[0-9]+$ ]]; then
			vlan=$((10#$item))
			vni=$((vlan + 10#$VNI_OFFSET))
		else
			die "SEGMENTS: invalid item '$item' (expected vlan or vlan:vni)"
		fi
		((vlan >= 1 && vlan <= 4094)) || die "SEGMENTS: VLAN $vlan is outside 1-4094"
		((vni >= 1 && vni <= 16777215)) || die "SEGMENTS: VNI $vni for VLAN $vlan is outside 1-16777215"
		[[ -z ${SEGMENT_VNI[$vlan]:-} ]] || die "SEGMENTS: VLAN $vlan is listed more than once"
		[[ -z ${owner[$vni]:-} ]] || die "SEGMENTS: VNI $vni is used by VLAN ${owner[$vni]} and VLAN $vlan"
		owner[$vni]=$vlan
		SEGMENT_VNI[$vlan]=$vni
		SEGMENT_VLANS+=("$vlan")
	done
	((${#SEGMENT_VLANS[@]} > 0)) || die "SEGMENTS is empty"
}

# overlay_address VLAN: the test port address for VLAN, or empty for none.
overlay_address() {
	local vlan=$1 var="OVERLAY_$1" value last
	value=${!var:-}
	if [[ -z $value ]]; then
		if ((vlan > 255)); then
			echo ""
			return
		fi
		last=${ADDRESS_IP##*.}
		value="10.$vlan.0.$last/24"
	fi
	[[ $value == none ]] && value=""
	echo "$value"
}

validate() {
	require_root
	require_supported_os
	require_env NEIGH1_IP NEIGH2_IP RR_IP UPLINK_IF ADDRESS
	apply_defaults

	is_ipv4_cidr "$ADDRESS" || die "ADDRESS must be IPv4 CIDR, got '$ADDRESS'"
	ADDRESS_IP=${ADDRESS%/*}
	validate_peer_ips "$ADDRESS_IP" "$ADDRESS" NEIGH1_IP NEIGH2_IP RR_IP
	validate_lab_iface UPLINK_IF "$UPLINK_IF"
	if [[ -n $TRUNK_IF ]]; then
		validate_lab_iface TRUNK_IF "$TRUNK_IF"
		[[ $TRUNK_IF != "$UPLINK_IF" ]] || die "TRUNK_IF must differ from UPLINK_IF"
	fi

	[[ $BRIDGE =~ ^[A-Za-z0-9_.-]{1,15}$ ]] || die "invalid bridge name '$BRIDGE'"
	if ! is_uint "$OVERLAY_MTU" || ((OVERLAY_MTU < 576 || OVERLAY_MTU > 9000)); then
		die "OVERLAY_MTU must be 576-9000, got '$OVERLAY_MTU'"
	fi
	if ! is_uint "$ASN" || ((ASN < 1 || ASN > 65535)); then
		die "ASN must be 1-65535 (default route targets <asn>:<vni> need a 2-byte ASN), got '$ASN'"
	fi

	parse_segments
	local vlan address var
	for vlan in "${SEGMENT_VLANS[@]}"; do
		var="OVERLAY_$vlan"
		address=$(overlay_address "$vlan")
		if [[ -n $address ]]; then
			is_ipv4_cidr "$address" || die "$var must be IPv4 CIDR or 'none', got '$address'"
		elif [[ -z ${!var:-} ]] && ((vlan > 255)); then
			warn "VLAN $vlan > 255 has no default test address; set $var to ping through test$vlan"
		fi
	done
}

# ---------------------------------------------------------------------------
# Packages
# ---------------------------------------------------------------------------
NFV_KEY=/etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-SIG-NFV

enable_nfv_repo() {
	if [[ $OS_ID == almalinux ]]; then
		log "Enabling the CentOS NFV SIG Open vSwitch repository"
		dnf -y -q install centos-release-nfv-openvswitch
		return
	fi
	log "Adding the CentOS Stream 10 NFV SIG Open vSwitch repository"
	[[ -s $NFV_KEY ]] || curl -fsSL -o "$NFV_KEY" https://www.centos.org/keys/RPM-GPG-KEY-CentOS-SIG-NFV
	install_content /etc/yum.repos.d/snout-nfv-openvswitch.repo 0644 root:root <<EOF || true
[snout-nfv-openvswitch]
name=CentOS Stream 10 - NFV SIG - Open vSwitch
baseurl=https://mirror.stream.centos.org/SIGs/10-stream/nfv/\$basearch/openvswitch-2/
gpgcheck=1
gpgkey=file://$NFV_KEY
enabled=1
EOF
}

install_packages() {
	enable_nfv_repo
	local ovs
	ovs=$(dnf -q repoquery --qf '%{name}\n' 'openvswitch3.*' 2>/dev/null | grep -E '^openvswitch3\.[0-9]+$' | sort -uV | tail -n 1 || true)
	[[ -n $ovs ]] || die "no openvswitch3.* package found in the enabled repositories"
	log "Installing $ovs, NetworkManager-ovs, git, make, golang and tools"
	dnf -y -q install "$ovs" NetworkManager-ovs git make golang curl tar iproute tcpdump
	systemctl enable --now openvswitch >/dev/null 2>&1
	# NetworkManager loads its OVS plugin only at start-up.
	systemctl restart NetworkManager
	wait_for 30 nmcli -t general status || die "NetworkManager did not come back"
	OVS_PACKAGE=$ovs
}

# ---------------------------------------------------------------------------
# GoBGP
# ---------------------------------------------------------------------------
gobgp_config() {
	cat <<EOF
# Generated by $SCRIPT_NAME - Snout lab hypervisor
[global.config]
  as = $ASN
  router-id = "$ADDRESS_IP"

[[neighbors]]
  [neighbors.config]
    neighbor-address = "$RR_IP"
    peer-as = $ASN
    description = "route-reflector"
  [neighbors.transport.config]
    local-address = "$ADDRESS_IP"
  [neighbors.timers.config]
    connect-retry = 5
  [[neighbors.afi-safis]]
    [neighbors.afi-safis.config]
      afi-safi-name = "l2vpn-evpn"
EOF
}

# ---------------------------------------------------------------------------
# OVS through NetworkManager
# ---------------------------------------------------------------------------
nm_ensure() {
	local name=$1
	shift
	nm_con_exists "$name" || nmcli connection add con-name "$name" "$@" >/dev/null
}

set_test_port_mtu() {
	local con=$1 iface=$2
	if ! nmcli connection modify "$con" 802-3-ethernet.mtu "$OVERLAY_MTU" 2>/dev/null; then
		MTU_FALLBACK+=("$iface")
	fi
}

configure_bridge() {
	local br_con="ovs-bridge-$BRIDGE" port_con="ovs-port-$BRIDGE" if_con="ovs-if-$BRIDGE"
	if ovs-vsctl br-exists "$BRIDGE" 2>/dev/null && ! nm_con_exists "$br_con"; then
		die "OVS bridge $BRIDGE exists but is not managed by NetworkManager; delete it with 'ovs-vsctl del-br $BRIDGE' or choose another BRIDGE"
	fi
	log "Creating OVS bridge $BRIDGE through NetworkManager"
	nm_ensure "$br_con" type ovs-bridge conn.interface "$BRIDGE"
	nm_ensure "$port_con" type ovs-port conn.interface "$BRIDGE" master "$br_con"
	nm_ensure "$if_con" type ovs-interface conn.interface "$BRIDGE" master "$port_con" ovs-interface.type internal
	nmcli connection modify "$br_con" connection.autoconnect yes ovs-bridge.stp-enable no
	nmcli connection modify "$port_con" connection.autoconnect yes
	nmcli connection modify "$if_con" connection.autoconnect yes ipv4.method disabled ipv4.addresses "" ipv6.method disabled

	MTU_FALLBACK=()
	local vlan iface address wanted=()
	for vlan in "${SEGMENT_VLANS[@]}"; do
		iface="test$vlan"
		wanted+=("ovs-port-$iface" "ovs-if-$iface")
		nm_ensure "ovs-port-$iface" type ovs-port conn.interface "$iface" master "$br_con"
		nmcli connection modify "ovs-port-$iface" connection.autoconnect yes ovs-port.tag "$vlan" ovs-port.vlan-mode access
		nm_ensure "ovs-if-$iface" type ovs-interface conn.interface "$iface" master "ovs-port-$iface" ovs-interface.type internal
		nmcli connection modify "ovs-if-$iface" connection.autoconnect yes ipv6.method disabled
		address=$(overlay_address "$vlan")
		if [[ -n $address ]]; then
			nmcli connection modify "ovs-if-$iface" ipv4.method manual ipv4.addresses "$address" ipv4.gateway "" ipv4.never-default yes
		else
			nmcli connection modify "ovs-if-$iface" ipv4.method disabled ipv4.addresses ""
		fi
		set_test_port_mtu "ovs-if-$iface" "$iface"
	done

	if [[ -n $TRUNK_IF ]]; then
		local trunk_port="ovs-port-$TRUNK_IF" trunk_if="ovs-eth-$TRUNK_IF" vlans
		vlans=$(
			IFS=,
			echo "${SEGMENT_VLANS[*]}"
		)
		wanted+=("$trunk_port" "$trunk_if")
		nm_ensure "$trunk_port" type ovs-port conn.interface "$TRUNK_IF" master "$br_con"
		nmcli connection modify "$trunk_port" connection.autoconnect yes ovs-port.vlan-mode trunk ovs-port.trunks "$vlans"
		nm_ensure "$trunk_if" type ethernet conn.interface "$TRUNK_IF" master "$trunk_port" slave-type ovs-port
		nmcli connection modify "$trunk_if" connection.autoconnect yes
	fi

	remove_stale_nm_connections "${wanted[@]}"

	nmcli connection up "$if_con" >/dev/null
	for vlan in "${SEGMENT_VLANS[@]}"; do
		nmcli connection up "ovs-if-test$vlan" >/dev/null
	done
	[[ -z $TRUNK_IF ]] || nmcli connection up "ovs-eth-$TRUNK_IF" >/dev/null
	wait_for 20 ovs-vsctl br-exists "$BRIDGE" || die "OVS bridge $BRIDGE did not appear"

	for iface in "${MTU_FALLBACK[@]}"; do
		warn "NetworkManager refused the MTU on $iface; using OVS mtu_request"
		ovs-vsctl set Interface "$iface" mtu_request="$OVERLAY_MTU"
	done
}

# remove_stale_nm_connections WANTED...: delete test-port and trunk
# connections of this bridge that are no longer configured (re-run with
# different SEGMENTS or without TRUNK_IF).
remove_stale_nm_connections() {
	local -A keep=()
	local name
	for name in "$@"; do keep[$name]=1; done
	while IFS= read -r name; do
		case $name in
		ovs-port-test* | ovs-if-test* | ovs-eth-*) ;;
		ovs-port-*)
			[[ $name == "ovs-port-$BRIDGE" ]] && continue
			[[ $(nmcli -g ovs-port.vlan-mode connection show "$name" 2>/dev/null) == trunk ]] || continue
			;;
		*) continue ;;
		esac
		[[ -n ${keep[$name]:-} ]] && continue
		log "Removing stale NetworkManager connection $name"
		nmcli connection delete "$name" >/dev/null
	done < <(nmcli -t -f NAME connection show)
}

# ---------------------------------------------------------------------------
# Snout configuration
# ---------------------------------------------------------------------------
snout_config() {
	local vlan plain=()
	for vlan in "${SEGMENT_VLANS[@]}"; do
		[[ -n ${SEGMENT_EXPLICIT[$vlan]:-} ]] || plain+=("$vlan")
	done
	cat <<EOF
# Generated by $SCRIPT_NAME
gobgp_grpc_socket    = "$GOBGP_API"
log_level            = "info"
syslog_facility      = "local0"
bridge_scan_interval = "5s"
ovs_poll_interval    = "5s"
vtep                 = "$ADDRESS_IP"
asn                  = $ASN

[[bridge]]
name       = "$BRIDGE"
vni_offset = $VNI_OFFSET
EOF
	if ((${#plain[@]} > 0)); then
		local joined
		joined=$(
			IFS=,
			echo "${plain[*]}"
		)
		printf 'vlans      = [%s]\n' "${joined//,/, }"
	fi
	for vlan in "${SEGMENT_VLANS[@]}"; do
		[[ -n ${SEGMENT_EXPLICIT[$vlan]:-} ]] || continue
		cat <<EOF

  [[bridge.vni]]
  vlan = $vlan
  vni  = ${SEGMENT_VNI[$vlan]}
EOF
	done
}

write_snout_config() {
	if snout_config | install_content /etc/snout/snout.toml 0644 root:root 1; then
		log "Wrote /etc/snout/snout.toml"
		[[ ! -f /etc/snout/snout.toml.bak ]] || log "Previous version kept as /etc/snout/snout.toml.bak"
	else
		log "/etc/snout/snout.toml is up to date"
	fi
}

# ---------------------------------------------------------------------------
# Checks
# ---------------------------------------------------------------------------
precheck_underlay() {
	local peer route
	for peer in "$NEIGH1_IP" "$NEIGH2_IP" "$RR_IP"; do
		if ! ping -c 1 -W 1 -I "$UPLINK_IF" "$peer" >/dev/null 2>&1; then
			warn "no ping reply from $peer over $UPLINK_IF (the host may not be up yet)"
		fi
		route=$(ip -4 route get "$peer" 2>/dev/null || true)
		if [[ $route != *" dev $UPLINK_IF "* || $route != *" src $ADDRESS_IP "* ]]; then
			warn "traffic to $peer does not leave $UPLINK_IF with source $ADDRESS_IP: $route"
		fi
	done
}

snout_port_exists() {
	local vni=$1 vtep=$2
	[[ -n $(ovs-vsctl --data=bare --no-heading --columns=name find Interface \
		external_ids:snout-owned=true "external_ids:snout-vni=$vni" "external_ids:snout-vtep=$vtep" 2>/dev/null) ]]
}

postcheck() {
	if wait_for 30 bgp_established "$RR_IP"; then
		log "BGP session with route reflector $RR_IP is established"
	else
		warn "BGP session with route reflector $RR_IP is not established (is the RR bootstrapped?)"
	fi
	systemctl is-active --quiet snout || return 0
	local vlan vni peer
	for vlan in "${SEGMENT_VLANS[@]}"; do
		vni=${SEGMENT_VNI[$vlan]}
		for peer in "$NEIGH1_IP" "$NEIGH2_IP"; do
			if wait_for 30 snout_port_exists "$vni" "$peer"; then
				log "VNI $vni (VLAN $vlan): tunnel to $peer is up"
			else
				warn "VNI $vni (VLAN $vlan): no tunnel to $peer yet (neighbour not running Snout yet?)"
			fi
		done
	done
}

summary() {
	local vlan
	echo
	log "Hypervisor bootstrap finished"
	cat <<EOF
  Lab NIC       : $UPLINK_IF $ADDRESS (VTEP)
  Neighbours    : $NEIGH1_IP, $NEIGH2_IP; route reflector $RR_IP
  OVS           : ${OVS_PACKAGE:-installed}, bridge $BRIDGE${TRUNK_IF:+, trunk $TRUNK_IF}
  GoBGP         : $GOBGP_VERSION, AS $ASN, gRPC $GOBGP_API
  QA user       : ${QA_USER:-QA} (NOPASSWD sudo)
  Segments      :
EOF
	for vlan in "${SEGMENT_VLANS[@]}"; do
		printf '    VLAN %-5s -> VNI %-8s test port test%s %s\n' "$vlan" "${SEGMENT_VNI[$vlan]}" "$vlan" "$(overlay_address "$vlan")"
	done
	cat <<EOF

Useful commands:
  gobgp neighbor                         # session with the RR
  gobgp global rib -a evpn               # EVPN routes
  ovs-vsctl show                         # Snout tunnels are the sn-* ports
  ovs-appctl fdb/show $BRIDGE            # MAC table
  journalctl -u snout -f                 # Snout log
  VLAN=${SEGMENT_VLANS[0]} sh /usr/local/src/snout/scripts/fake-local-mac.sh add   # fake VM MAC
EOF
}

main() {
	trap 'die "command failed at line $LINENO: $BASH_COMMAND"' ERR
	parse_options "${OPTION_VARS[@]}" -- "$@"
	validate
	set_hostname
	setup_qa_user
	disable_firewall
	install_packages
	install_gobgp
	gobgp_config | install_gobgp_config
	configure_underlay "$UPLINK_IF" "$ADDRESS"
	configure_bridge
	write_snout_config
	start_gobgp
	precheck_underlay

	local update_failed=0
	log "Building and installing Snout"
	if ! SNOUT_REPO=${SNOUT_REPO:-} SNOUT_REF=${SNOUT_REF:-} bash "$SCRIPT_DIR/snout-update.sh"; then
		update_failed=1
		warn "snout-update.sh failed; network, OVS and GoBGP are configured, Snout is not running"
	fi
	postcheck
	summary
	if ((update_failed)); then
		die "Snout was not installed; fix the error above and run 'bash $SCRIPT_DIR/snout-update.sh'"
	fi
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
	main "$@"
fi
