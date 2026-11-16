# shellcheck shell=bash
#
# lab-common.sh - helpers shared by the Snout lab bootstrap scripts.
# Sourced by misc/bootstrap-hypervisor.sh and misc/bootstrap-route-reflector.sh;
# not meant to be executed directly.

GOBGP_API=127.0.0.1:50051
GOBGP_API_HOST=${GOBGP_API%:*}
GOBGP_API_PORT=${GOBGP_API##*:}

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------
log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Options
# ---------------------------------------------------------------------------

# load_env_file FILE: export the KEY=VALUE assignments of FILE as defaults.
# Variables already set in the environment win, so
# "SEGMENTS=100 bash script --env hv1.env" uses SEGMENTS=100. Comments and
# blank lines are allowed; values must not need shell quoting.
load_env_file() {
	local file=$1 line key value number=0
	[[ -r $file ]] || die "cannot read env file '$file'"
	while IFS= read -r line || [[ -n $line ]]; do
		number=$((number + 1))
		line=${line%%#*}
		line=${line#"${line%%[![:space:]]*}"}
		line=${line%"${line##*[![:space:]]}"}
		[[ -z $line ]] && continue
		[[ $line =~ ^([A-Z][A-Z0-9_]*)=(.*)$ ]] || die "$file:$number: expected KEY=VALUE, got '$line'"
		key=${BASH_REMATCH[1]}
		value=${BASH_REMATCH[2]}
		value=${value#\"}
		value=${value%\"}
		[[ -n ${!key:-} ]] && continue
		export "$key=$value"
	done <"$file"
}

# parse_options ALLOWED_VARS... -- ARGS...: handles --env FILE, -h/--help and
# "--some-option VALUE", which sets SOME_OPTION when it is in ALLOWED_VARS.
# Precedence: CLI option > environment variable > env file.
parse_options() {
	local -a allowed=() args=()
	while [[ $# -gt 0 && $1 != -- ]]; do
		allowed+=("$1")
		shift
	done
	[[ $# -gt 0 ]] && shift
	args=("$@")

	local index
	for ((index = 0; index < ${#args[@]}; index++)); do
		if [[ ${args[index]} == --env ]]; then
			[[ $((index + 1)) -lt ${#args[@]} ]] || die "option --env requires a value"
			load_env_file "${args[index + 1]}"
		fi
	done

	local option name var known
	set -- "${args[@]}"
	while [[ $# -gt 0 ]]; do
		option=$1
		case $option in
		-h | --help)
			usage
			exit 0
			;;
		--env)
			shift 2
			continue
			;;
		--*) ;;
		*)
			usage >&2
			die "unexpected argument: $option"
			;;
		esac
		name=${option#--}
		var=${name^^}
		var=${var//-/_}
		known=0
		for name in "${allowed[@]}"; do
			[[ $name == "$var" ]] && known=1
		done
		[[ $known == 1 ]] || {
			usage >&2
			die "unknown option: $option"
		}
		[[ $# -ge 2 && -n $2 ]] || die "option $option requires a value"
		printf -v "$var" '%s' "$2"
		shift 2
	done
}

# require_env NAME...: fail with a clear message when a variable is unset.
require_env() {
	local name missing=()
	for name in "$@"; do
		[[ -n ${!name:-} ]] || missing+=("$name")
	done
	((${#missing[@]} == 0)) || die "required environment variable(s) not set: ${missing[*]}"
}

# ---------------------------------------------------------------------------
# Validation helpers
# ---------------------------------------------------------------------------
is_uint() { [[ $1 =~ ^[0-9]+$ ]]; }

is_ipv4() {
	local octet
	[[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
	for octet in "${BASH_REMATCH[@]:1}"; do
		[[ $octet == 0 || $octet != 0* ]] || return 1
		((10#$octet <= 255)) || return 1
	done
}

is_ipv4_cidr() {
	[[ $1 == */* ]] || return 1
	is_ipv4 "${1%/*}" || return 1
	[[ ${1#*/} =~ ^[0-9]{1,2}$ ]] && ((10#${1#*/} <= 32))
}

ip_to_int() {
	local a b c d
	IFS=. read -r a b c d <<<"$1"
	echo $(((10#$a << 24) | (10#$b << 16) | (10#$c << 8) | 10#$d))
}

int_to_ip() {
	local n=$1
	echo "$(((n >> 24) & 255)).$(((n >> 16) & 255)).$(((n >> 8) & 255)).$((n & 255))"
}

prefix_mask() {
	local length=$1
	((length == 0)) && {
		echo 0
		return
	}
	echo $(((0xffffffff << (32 - length)) & 0xffffffff))
}

# network_of CIDR: 172.31.0.11/24 -> 172.31.0.0/24
network_of() {
	local length=${1#*/} mask
	mask=$(prefix_mask "$length")
	echo "$(int_to_ip $(($(ip_to_int "${1%/*}") & mask)))/$length"
}

# in_subnet IP CIDR
in_subnet() {
	local mask
	mask=$(prefix_mask "${2#*/}")
	(($(($(ip_to_int "$1") & mask)) == $(($(ip_to_int "${2%/*}") & mask))))
}

# validate_peer_ips SELF_IP CIDR NAME...: each named variable must be a valid
# IPv4 address inside CIDR, unique, and different from SELF_IP.
validate_peer_ips() {
	local self=$1 cidr=$2 name value
	shift 2
	declare -A seen=([$self]=self)
	for name in "$@"; do
		value=${!name}
		is_ipv4 "$value" || die "$name must be an IPv4 address, got '$value'"
		in_subnet "$value" "$cidr" || die "$name ($value) is outside the lab subnet $(network_of "$cidr")"
		[[ -z ${seen[$value]:-} ]] || die "$name ($value) duplicates ${seen[$value]}"
		seen[$value]=$name
	done
}

# ---------------------------------------------------------------------------
# Host checks
# ---------------------------------------------------------------------------
OS_RELEASE_FILE=${OS_RELEASE_FILE:-/etc/os-release}

require_root() {
	[[ $EUID -eq 0 ]] || die "run as root"
}

require_supported_os() {
	[[ -r $OS_RELEASE_FILE ]] || die "$OS_RELEASE_FILE not found"
	local ID VERSION_ID
	# shellcheck disable=SC1090
	ID=$(. "$OS_RELEASE_FILE" && echo "${ID:-}")
	# shellcheck disable=SC1090
	VERSION_ID=$(. "$OS_RELEASE_FILE" && echo "${VERSION_ID:-}")
	case $ID in
	almalinux | ol) ;;
	*) die "unsupported distribution '${ID:-unknown}', expected AlmaLinux 10 or Oracle Linux 10" ;;
	esac
	[[ ${VERSION_ID%%.*} == 10 ]] || die "unsupported release ${VERSION_ID:-unknown}, expected 10"
	# shellcheck disable=SC2034 # used by the sourcing scripts
	OS_ID=$ID
}

default_route_iface() {
	ip -4 route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}'
}

# validate_lab_iface NAME VALUE: the NIC must exist and must not carry the
# default route (that is the management NIC).
validate_lab_iface() {
	local name=$1 iface=$2 management
	[[ -n $iface ]] || die "$name is required (the lab vNIC)"
	ip link show dev "$iface" >/dev/null 2>&1 || die "$name: interface '$iface' does not exist"
	management=$(default_route_iface)
	[[ $iface != "$management" ]] || die "$name ($iface) carries the default route; it must be the dedicated lab vNIC, not the management NIC"
}

# ---------------------------------------------------------------------------
# Files
# ---------------------------------------------------------------------------

# install_content PATH MODE OWNER:GROUP [BACKUP]: write stdin to PATH when the
# content differs. With BACKUP=1 an existing different file is kept as
# PATH.bak. Returns 0 when the file changed, 1 otherwise.
install_content() {
	local path=$1 mode=$2 owner=$3 backup=${4:-0} tmp
	tmp=$(mktemp)
	cat >"$tmp"
	if [[ -f $path ]] && cmp -s "$tmp" "$path"; then
		rm -f "$tmp"
		return 1
	fi
	if [[ $backup == 1 && -f $path ]]; then
		cp -p "$path" "$path.bak"
	fi
	install -D -m "$mode" -o "${owner%:*}" -g "${owner#*:}" "$tmp" "$path"
	rm -f "$tmp"
	return 0
}

# wait_for SECONDS COMMAND...: retry COMMAND once per second.
wait_for() {
	local seconds=$1 i
	shift
	for ((i = 0; i < seconds; i++)); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 1
	done
	"$@" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# QA user and firewall
# ---------------------------------------------------------------------------
setup_qa_user() {
	local user=${QA_USER:-QA}
	[[ $user =~ ^[A-Za-z_][A-Za-z0-9_.-]*$ ]] || die "invalid QA_USER '$user'"
	log "Creating user $user with password-less sudo"
	id -u "$user" >/dev/null 2>&1 || useradd --create-home --shell /bin/bash --comment "Snout QA" "$user"
	usermod -aG wheel "$user"

	if [[ -n ${QA_PASSWORD:-} ]]; then
		printf '%s:%s\n' "$user" "$QA_PASSWORD" | chpasswd
	fi
	if [[ -n ${QA_SSH_KEY:-} ]]; then
		local key=$QA_SSH_KEY home
		[[ -f $key ]] && key=$(<"$key")
		home=$(getent passwd "$user" | cut -d: -f6)
		install -d -m 0700 -o "$user" -g "$user" "$home/.ssh"
		touch "$home/.ssh/authorized_keys"
		grep -qxF -- "$key" "$home/.ssh/authorized_keys" || printf '%s\n' "$key" >>"$home/.ssh/authorized_keys"
		chown "$user:$user" "$home/.ssh/authorized_keys"
		chmod 0600 "$home/.ssh/authorized_keys"
		restorecon -R "$home/.ssh" 2>/dev/null || true
	fi
	if [[ -z ${QA_PASSWORD:-} && -z ${QA_SSH_KEY:-} ]]; then
		warn "$user has no password and no SSH key; use 'su - $user' or set QA_PASSWORD/QA_SSH_KEY"
	fi

	local tmp
	tmp=$(mktemp)
	printf '%s ALL=(ALL) NOPASSWD: ALL\n' "$user" >"$tmp"
	if ! visudo -cqf "$tmp"; then
		rm -f "$tmp"
		die "generated sudoers file is invalid"
	fi
	install -m 0440 -o root -g root "$tmp" "/etc/sudoers.d/90-${user,,}"
	rm -f "$tmp"
}

disable_firewall() {
	if ! systemctl list-unit-files firewalld.service >/dev/null 2>&1; then
		return 0
	fi
	log "Disabling firewalld"
	systemctl disable --now firewalld >/dev/null 2>&1 || true
	systemctl mask firewalld >/dev/null 2>&1 || true
}

set_hostname() {
	[[ -n ${HOST_NAME:-} ]] || return 0
	log "Setting hostname to $HOST_NAME"
	hostnamectl set-hostname "$HOST_NAME"
}

# ---------------------------------------------------------------------------
# NetworkManager: lab underlay
# ---------------------------------------------------------------------------
nm_con_exists() { nmcli -t -f NAME connection show | grep -qxF -- "$1"; }

# configure_underlay IFACE CIDR: static address on the dedicated lab vNIC,
# without gateway, DNS or default route. The management NIC is not touched.
configure_underlay() {
	local iface=$1 cidr=$2 con
	con=$(nmcli -g GENERAL.CONNECTION device show "$iface" 2>/dev/null || true)
	if [[ -z $con || $con == "--" ]]; then
		con="lab-$iface"
		nm_con_exists "$con" || nmcli connection add type ethernet ifname "$iface" con-name "$con" >/dev/null
	fi
	log "Setting lab address $cidr on $iface (connection '$con', no gateway)"
	nmcli connection modify "$con" \
		connection.autoconnect yes \
		ipv4.method manual \
		ipv4.addresses "$cidr" \
		ipv4.gateway "" \
		ipv4.dns "" \
		ipv4.ignore-auto-dns yes \
		ipv4.never-default yes \
		ipv6.method disabled
	nmcli connection up "$con" >/dev/null
}

# ---------------------------------------------------------------------------
# GoBGP
# ---------------------------------------------------------------------------
gobgp_arch() {
	case $(uname -m) in
	x86_64) echo amd64 ;;
	aarch64) echo arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
	esac
}

resolve_gobgp_version() {
	local version=${GOBGP_VERSION:-latest} url
	if [[ $version == latest ]]; then
		if ! url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/osrg/gobgp/releases/latest); then
			die "cannot reach github.com to resolve the latest GoBGP release; check internet access or set GOBGP_VERSION"
		fi
		version=${url##*/}
	fi
	[[ $version =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "cannot resolve GoBGP version from '${GOBGP_VERSION:-latest}'"
	GOBGP_VERSION=${version#v}
}

install_gobgp() {
	resolve_gobgp_version
	local arch tarball base tmp
	arch=$(gobgp_arch)
	if [[ -x /usr/local/bin/gobgpd ]] && /usr/local/bin/gobgpd --version 2>/dev/null | grep -qw "$GOBGP_VERSION"; then
		log "GoBGP $GOBGP_VERSION is already installed"
	else
		log "Installing GoBGP $GOBGP_VERSION ($arch) from GitHub"
		base="https://github.com/osrg/gobgp/releases/download/v${GOBGP_VERSION}"
		tarball="gobgp_${GOBGP_VERSION}_linux_${arch}.tar.gz"
		tmp=$(mktemp -d)
		curl -fsSL -o "$tmp/$tarball" "$base/$tarball"
		curl -fsSL -o "$tmp/checksums.txt" "$base/gobgp_${GOBGP_VERSION}_checksums.txt"
		(cd "$tmp" && grep " ${tarball}\$" checksums.txt | sha256sum -c --quiet -) ||
			die "checksum verification failed for $tarball"
		tar -xzf "$tmp/$tarball" -C "$tmp" gobgp gobgpd
		install -m 0755 "$tmp/gobgp" "$tmp/gobgpd" /usr/local/bin/
		rm -rf "$tmp"
		restorecon /usr/local/bin/gobgp /usr/local/bin/gobgpd 2>/dev/null || true
	fi

	id -u gobgpd >/dev/null 2>&1 ||
		useradd --system --no-create-home --home-dir /var/lib/gobgpd --shell /sbin/nologin gobgpd
	install -d -m 0750 -o root -g gobgpd /etc/gobgp
	gobgpd_unit | install_content /etc/systemd/system/gobgpd.service 0644 root:root || true
	systemctl daemon-reload
}

gobgpd_unit() {
	cat <<EOF
[Unit]
Description=GoBGP routing daemon
Documentation=https://github.com/osrg/gobgp
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=gobgpd
Group=gobgpd
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ExecStart=/usr/local/bin/gobgpd -t toml -f /etc/gobgp/gobgpd.toml --api-hosts ${GOBGP_API}
ExecReload=/bin/kill -HUP \$MAINPID
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
}

# install_gobgp_config: stdin is the gobgpd TOML. Validated with --dry-run
# when the installed gobgpd supports it.
install_gobgp_config() {
	local cfg=/etc/gobgp/gobgpd.toml
	install_content "$cfg" 0640 root:gobgpd || true
	if /usr/local/bin/gobgpd --help 2>&1 | grep -q -- '--dry-run'; then
		/usr/local/bin/gobgpd -t toml -f "$cfg" --dry-run >/dev/null || die "gobgpd rejected $cfg"
	fi
}

start_gobgp() {
	log "Starting gobgpd"
	systemctl enable gobgpd >/dev/null 2>&1
	systemctl restart gobgpd
	if ! wait_for 5 systemctl is-active --quiet gobgpd; then
		journalctl -u gobgpd -n 30 --no-pager >&2 || true
		die "gobgpd failed to start"
	fi
}

gobgp_cli() {
	/usr/local/bin/gobgp -u "$GOBGP_API_HOST" -p "$GOBGP_API_PORT" "$@"
}

# bgp_established PEER: true when the session with PEER is Established.
bgp_established() {
	gobgp_cli neighbor 2>/dev/null | awk -v peer="$1" '$1 == peer && /Establ/ {found = 1} END {exit !found}'
}
