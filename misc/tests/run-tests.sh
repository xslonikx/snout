#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
#
# run-tests.sh - unit tests for the lab scripts in misc/. No root, network or
# OVS needed: the scripts are sourced (their main() is not run) and only the
# pure functions are exercised.
#
#   bash misc/tests/run-tests.sh

set -uo pipefail

MISC_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PASSED=0
FAILED=0

pass() { PASSED=$((PASSED + 1)); }
fail() {
	FAILED=$((FAILED + 1))
	printf 'FAIL: %s\n' "$*" >&2
}

assert_eq() {
	local name=$1 want=$2 got=$3
	if [[ $got == "$want" ]]; then pass; else fail "$name: got [$got], want [$want]"; fi
}

assert_ok() {
	local name=$1
	shift
	if "$@" >/dev/null 2>&1; then pass; else fail "$name: command failed: $*"; fi
}

assert_not() {
	local name=$1
	shift
	if "$@" >/dev/null 2>&1; then fail "$name: command succeeded: $*"; else pass; fi
}

# assert_dies NAME FRAGMENT CODE: CODE runs in a subshell and must exit with
# an error message containing FRAGMENT.
assert_dies() {
	local name=$1 fragment=$2 code=$3 output
	if output=$(bash -c "$code" 2>&1); then
		fail "$name: succeeded, expected an error containing [$fragment]"
	elif [[ $output != *"$fragment"* ]]; then
		fail "$name: error [$output] does not contain [$fragment]"
	else
		pass
	fi
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---------------------------------------------------------------------------
# lib/lab-common.sh
# ---------------------------------------------------------------------------
LIB="set -euo pipefail; . '$MISC_DIR/lib/lab-common.sh'"
(
	set -euo pipefail
	# shellcheck source=../lib/lab-common.sh
	. "$MISC_DIR/lib/lab-common.sh"

	assert_ok "ipv4 valid" is_ipv4 172.31.0.11
	assert_ok "ipv4 zero" is_ipv4 0.0.0.0
	assert_not "ipv4 octet" is_ipv4 172.31.0.256
	assert_not "ipv4 leading zero" is_ipv4 172.031.0.1
	assert_not "ipv4 short" is_ipv4 172.31.0
	assert_ok "cidr valid" is_ipv4_cidr 172.31.0.11/24
	assert_not "cidr no prefix" is_ipv4_cidr 172.31.0.11
	assert_not "cidr prefix" is_ipv4_cidr 172.31.0.11/33
	assert_eq "network /24" 172.31.0.0/24 "$(network_of 172.31.0.11/24)"
	assert_eq "network /30" 10.0.0.4/30 "$(network_of 10.0.0.7/30)"
	assert_eq "network /0" 0.0.0.0/0 "$(network_of 1.2.3.4/0)"
	assert_ok "in subnet" in_subnet 172.31.0.13 172.31.0.11/24
	assert_not "outside subnet" in_subnet 172.31.1.13 172.31.0.11/24
	assert_eq "int roundtrip" 172.31.0.11 "$(int_to_ip "$(ip_to_int 172.31.0.11)")"
) || fail "lib pure functions crashed"

assert_dies "peer duplicate" "duplicates NEIGH1_IP" \
	"$LIB; NEIGH1_IP=172.31.0.12 NEIGH2_IP=172.31.0.12; validate_peer_ips 172.31.0.11 172.31.0.11/24 NEIGH1_IP NEIGH2_IP"
assert_dies "peer is self" "duplicates self" \
	"$LIB; NEIGH1_IP=172.31.0.11; validate_peer_ips 172.31.0.11 172.31.0.11/24 NEIGH1_IP"
assert_dies "peer outside subnet" "outside the lab subnet 172.31.0.0/24" \
	"$LIB; RR_IP=10.0.0.1; validate_peer_ips 172.31.0.11 172.31.0.11/24 RR_IP"
assert_dies "peer invalid" "RR_IP must be an IPv4 address" \
	"$LIB; RR_IP=bad; validate_peer_ips 172.31.0.11 172.31.0.11/24 RR_IP"
assert_eq "peers valid" ok "$(bash -c "$LIB; NEIGH1_IP=172.31.0.12 NEIGH2_IP=172.31.0.13 RR_IP=172.31.0.10; validate_peer_ips 172.31.0.11 172.31.0.11/24 NEIGH1_IP NEIGH2_IP RR_IP && echo ok" 2>&1)"

assert_dies "require_env" "not set: NEIGH2_IP RR_IP" \
	"$LIB; NEIGH1_IP=1; unset NEIGH2_IP RR_IP; require_env NEIGH1_IP NEIGH2_IP RR_IP"

cat >"$TMP/test.env" <<'EOF'
# comment
NEIGH1_IP=172.31.0.12   # trailing comment

QUOTED="value"
UPLINK_IF=
EOF
assert_eq "env file" "172.31.0.12|value|eth9" \
	"$(bash -c "$LIB; UPLINK_IF=eth9; load_env_file '$TMP/test.env'; echo \"\$NEIGH1_IP|\$QUOTED|\$UPLINK_IF\"" 2>&1)"
assert_eq "env file does not override environment" "172.31.0.99" \
	"$(bash -c "$LIB; NEIGH1_IP=172.31.0.99; load_env_file '$TMP/test.env'; echo \$NEIGH1_IP" 2>&1)"
printf 'not an assignment\n' >"$TMP/bad.env"
assert_dies "env file syntax" "bad.env:1: expected KEY=VALUE" "$LIB; load_env_file '$TMP/bad.env'"
assert_dies "env file missing" "cannot read env file" "$LIB; load_env_file '$TMP/missing.env'"

printf 'ASN=65001\nUPLINK_IF=eth1\n' >"$TMP/opts.env"
assert_eq "options: CLI > env > file" "65002|eth5|br7" \
	"$(bash -c "$LIB; usage() { :; }; ASN=65009 UPLINK_IF=eth5; parse_options ASN UPLINK_IF BRIDGE -- --env '$TMP/opts.env' --asn 65002 --bridge br7; echo \"\$ASN|\$UPLINK_IF|\$BRIDGE\"" 2>&1)"
assert_eq "options: file fills unset" "65001|eth1" \
	"$(bash -c "$LIB; usage() { :; }; unset ASN UPLINK_IF; parse_options ASN UPLINK_IF -- --env '$TMP/opts.env'; echo \"\$ASN|\$UPLINK_IF\"" 2>&1)"
assert_dies "options: unknown" "unknown option: --neigh1-ip" \
	"$LIB; usage() { :; }; parse_options ASN -- --neigh1-ip 1.2.3.4"
assert_dies "options: missing value" "requires a value" \
	"$LIB; usage() { :; }; parse_options ASN -- --asn"
assert_dies "options: positional" "unexpected argument: extra" \
	"$LIB; usage() { :; }; parse_options ASN -- extra"

printf 'ID=almalinux\nVERSION_ID="10.1"\n' >"$TMP/alma"
printf 'ID="ol"\nVERSION_ID="10.0"\n' >"$TMP/ol"
printf 'ID=rocky\nVERSION_ID="10.0"\n' >"$TMP/rocky"
printf 'ID=almalinux\nVERSION_ID="9.6"\n' >"$TMP/alma9"
assert_eq "os almalinux" almalinux "$(bash -c "$LIB; OS_RELEASE_FILE='$TMP/alma'; require_supported_os; echo \$OS_ID" 2>&1)"
assert_eq "os oracle" ol "$(bash -c "$LIB; OS_RELEASE_FILE='$TMP/ol'; require_supported_os; echo \$OS_ID" 2>&1)"
assert_dies "os rocky" "unsupported distribution 'rocky'" "$LIB; OS_RELEASE_FILE='$TMP/rocky'; require_supported_os"
assert_dies "os version" "unsupported release 9.6" "$LIB; OS_RELEASE_FILE='$TMP/alma9'; require_supported_os"

assert_eq "install_content new" "0|one" \
	"$(bash -c "$LIB; echo one | install_content '$TMP/f' 0644 $(id -un):$(id -gn) 1; echo \"\$?|\$(cat '$TMP/f')\"" 2>&1)"
assert_eq "install_content same" "1|no-backup" \
	"$(bash -c "$LIB; echo one | install_content '$TMP/f' 0644 $(id -un):$(id -gn) 1 || echo -n 1; echo \"|\$([[ -f '$TMP/f.bak' ]] && echo backup || echo no-backup)\"" 2>&1)"
assert_eq "install_content backup" "two|one" \
	"$(bash -c "$LIB; echo two | install_content '$TMP/f' 0644 $(id -un):$(id -gn) 1; echo \"\$(cat '$TMP/f')|\$(cat '$TMP/f.bak')\"" 2>&1)"

# ---------------------------------------------------------------------------
# bootstrap-hypervisor.sh
# ---------------------------------------------------------------------------
HV="set -euo pipefail; . '$MISC_DIR/bootstrap-hypervisor.sh'"

assert_eq "segments default" "100=10100 200=10200 |" \
	"$(bash -c "$HV; SEGMENTS=100,200 VNI_OFFSET=10000; parse_segments; for v in \"\${SEGMENT_VLANS[@]}\"; do printf '%s=%s ' \$v \${SEGMENT_VNI[\$v]}; done; echo \"|\${!SEGMENT_EXPLICIT[*]}\"" 2>&1)"
assert_eq "segments explicit" "100=10100 200=5000 |200" \
	"$(bash -c "$HV; SEGMENTS='100, 200:5000' VNI_OFFSET=10000; parse_segments; for v in \"\${SEGMENT_VLANS[@]}\"; do printf '%s=%s ' \$v \${SEGMENT_VNI[\$v]}; done; echo \"|\${!SEGMENT_EXPLICIT[*]}\"" 2>&1)"
assert_dies "segments duplicate vlan" "VLAN 100 is listed more than once" "$HV; SEGMENTS=100,100:5 VNI_OFFSET=0; parse_segments"
assert_dies "segments duplicate vni" "VNI 10100 is used by VLAN 100 and VLAN 200" "$HV; SEGMENTS=100,200:10100 VNI_OFFSET=10000; parse_segments"
assert_dies "segments vlan range" "VLAN 4095 is outside 1-4094" "$HV; SEGMENTS=4095 VNI_OFFSET=0; parse_segments"
assert_dies "segments vlan zero" "VLAN 0 is outside 1-4094" "$HV; SEGMENTS=0 VNI_OFFSET=1; parse_segments"
assert_dies "segments vni range" "VNI 16777300 for VLAN 300" "$HV; SEGMENTS=300 VNI_OFFSET=16777000; parse_segments"
assert_dies "segments bad item" "invalid item 'x'" "$HV; SEGMENTS=x VNI_OFFSET=0; parse_segments"
assert_dies "segments empty" "SEGMENTS is empty" "$HV; SEGMENTS=, VNI_OFFSET=0; parse_segments"
assert_dies "segments offset" "VNI_OFFSET must be 0-16777214" "$HV; SEGMENTS=100 VNI_OFFSET=16777215; parse_segments"

assert_eq "overlay default" 10.100.0.11/24 "$(bash -c "$HV; ADDRESS_IP=172.31.0.11; overlay_address 100" 2>&1)"
assert_eq "overlay override" 192.168.5.1/24 "$(bash -c "$HV; ADDRESS_IP=172.31.0.11; OVERLAY_100=192.168.5.1/24; overlay_address 100" 2>&1)"
assert_eq "overlay none" "" "$(bash -c "$HV; ADDRESS_IP=172.31.0.11; OVERLAY_100=none; overlay_address 100" 2>&1)"
assert_eq "overlay vlan > 255" "" "$(bash -c "$HV; ADDRESS_IP=172.31.0.11; overlay_address 300" 2>&1)"

SNOUT_TOML=$(bash -c "$HV; SEGMENTS='100,200,300:5000' VNI_OFFSET=10000 ADDRESS_IP=172.31.0.11 ASN=65000 BRIDGE=br0; parse_segments; snout_config" 2>&1)
assert_eq "snout.toml" '# Generated by bootstrap-hypervisor.sh
gobgp_grpc_socket    = "127.0.0.1:50051"
log_level            = "info"
syslog_facility      = "local0"
bridge_scan_interval = "5s"
ovs_poll_interval    = "5s"
vtep                 = "172.31.0.11"
asn                  = 65000

[[bridge]]
name       = "br0"
vni_offset = 10000
vlans      = [100, 200]

  [[bridge.vni]]
  vlan = 300
  vni  = 5000' "$SNOUT_TOML"
assert_eq "snout.toml explicit only" "0" \
	"$(bash -c "$HV; SEGMENTS='300:5000' VNI_OFFSET=10000 ADDRESS_IP=172.31.0.11 ASN=65000 BRIDGE=br0; parse_segments; snout_config | grep -c '^vlans'" 2>&1)"

HV_GOBGP=$(bash -c "$HV; ASN=65000 ADDRESS_IP=172.31.0.11 RR_IP=172.31.0.10; gobgp_config" 2>&1)
RR="set -euo pipefail; . '$MISC_DIR/bootstrap-route-reflector.sh'"
RR_GOBGP=$(bash -c "$RR; ASN=65000 ROUTER_ID=172.31.0.10 CLUSTER_ID=172.31.0.10 ADDRESS_IP=172.31.0.10 NEIGH1_IP=172.31.0.11 NEIGH2_IP=172.31.0.12 NEIGH3_IP=172.31.0.13; gobgp_config" 2>&1)
if command -v python3 >/dev/null 2>&1 && python3 -c 'import tomllib' 2>/dev/null; then
	check_toml() {
		python3 -c 'import sys, tomllib, json; print(json.dumps(tomllib.loads(sys.stdin.read()), sort_keys=True))'
	}
	assert_eq "hv gobgpd.toml" '{"global": {"config": {"as": 65000, "router-id": "172.31.0.11"}}, "neighbors": [{"afi-safis": [{"config": {"afi-safi-name": "l2vpn-evpn"}}], "config": {"description": "route-reflector", "neighbor-address": "172.31.0.10", "peer-as": 65000}, "timers": {"config": {"connect-retry": 5}}, "transport": {"config": {"local-address": "172.31.0.11"}}}]}' \
		"$(check_toml <<<"$HV_GOBGP")"
	assert_eq "rr gobgpd.toml neighbours" '["172.31.0.11", "172.31.0.12", "172.31.0.13"]' \
		"$(check_toml <<<"$RR_GOBGP" | python3 -c 'import sys, json; d = json.load(sys.stdin); print(json.dumps([n["config"]["neighbor-address"] for n in d["neighbors"] if n["config"]["peer-group"] == "hypervisors"]))')"
	assert_eq "rr gobgpd.toml peer group" '{"route-reflector-client": true, "route-reflector-cluster-id": "172.31.0.10"}' \
		"$(check_toml <<<"$RR_GOBGP" | python3 -c 'import sys, json; d = json.load(sys.stdin); print(json.dumps(d["peer-groups"][0]["route-reflector"]["config"], sort_keys=True))')"
	assert_eq "snout.toml syntax" '[100, 200]' "$(check_toml <<<"$SNOUT_TOML" | python3 -c 'import sys, json; print(json.load(sys.stdin)["bridge"][0]["vlans"])')"
else
	echo "skipping TOML syntax checks (python3 >= 3.11 not found)"
fi

# ---------------------------------------------------------------------------
# snout-update.sh
# ---------------------------------------------------------------------------
UP="set -euo pipefail; . '$MISC_DIR/snout-update.sh'"
assert_ok "go version equal" bash -c "$UP; version_at_least 1.26.5 1.26.5"
assert_ok "go version newer patch" bash -c "$UP; version_at_least 1.26.8 1.26.5"
assert_ok "go version newer minor" bash -c "$UP; version_at_least 1.27 1.26.5"
assert_not "go version older" bash -c "$UP; version_at_least 1.26.4 1.26.5"
assert_not "go version older minor" bash -c "$UP; version_at_least 1.25.9 1.26"
printf 'module x\n\ngo 1.26.5\n\nrequire (\n\tgo.example v1\n)\n' >"$TMP/go.mod"
assert_eq "go.mod version" 1.26.5 "$(bash -c "$UP; go_mod_version '$TMP/go.mod'" 2>&1)"
assert_dies "update arguments" "unexpected argument 'now'" "bash '$MISC_DIR/snout-update.sh' now"

# ---------------------------------------------------------------------------
# misc/lab/*.env and misc/snout.service
# ---------------------------------------------------------------------------
for file in "$MISC_DIR"/lab/*.env; do
	assert_ok "env file $(basename "$file")" bash -c "$LIB; load_env_file '$file'"
done
for n in 1 2 3; do
	assert_eq "hv$n peers" ok "$(bash -c "$LIB; load_env_file '$MISC_DIR/lab/hv$n.env'; validate_peer_ips \${ADDRESS%/*} \$ADDRESS NEIGH1_IP NEIGH2_IP RR_IP && echo ok" 2>&1)"
done
assert_eq "rr peers" ok "$(bash -c "$LIB; load_env_file '$MISC_DIR/lab/rr.env'; validate_peer_ips \${ADDRESS%/*} \$ADDRESS NEIGH1_IP NEIGH2_IP NEIGH3_IP && echo ok" 2>&1)"
assert_eq "unit ExecStart" "ExecStart=/usr/local/sbin/snout -f -c /etc/snout/snout.toml" "$(grep '^ExecStart=' "$MISC_DIR/snout.service")"

echo "passed: $PASSED, failed: $FAILED"
((FAILED == 0))
