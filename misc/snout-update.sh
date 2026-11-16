#!/usr/bin/env bash
#
# snout-update.sh - build Snout from git and replace the installed binary
# (spec: docs/workflows/phase_1.txt, section U).
#
#   sudo bash misc/snout-update.sh
#   sudo SNOUT_REF=feature-x bash misc/snout-update.sh
#
# Environment:
#   SNOUT_REPO        git URL      (default: git@github.com:xslonikx/snout.git)
#   SNOUT_REF         branch, tag or commit (default: main)
#   SNOUT_SRC_DIR     checkout     (default: /usr/local/src/snout)
#   SNOUT_SKIP_TESTS  1 skips "go test ./..." (default: 0)

set -Eeuo pipefail

SNOUT_REPO=${SNOUT_REPO:-git@github.com:xslonikx/snout.git}
SNOUT_REF=${SNOUT_REF:-main}
SNOUT_SRC_DIR=${SNOUT_SRC_DIR:-/usr/local/src/snout}
SNOUT_SKIP_TESTS=${SNOUT_SKIP_TESTS:-0}

BINARY=/usr/local/sbin/snout
CONFIG=/etc/snout/snout.toml
UNIT=/etc/systemd/system/snout.service
STATE_DIR=/var/lib/snout
COMMIT_FILE=$STATE_DIR/commit

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2
	exit 1
}

usage() {
	sed -n '2,/^$/s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"
}

# version_at_least HAVE NEED: compares Go versions such as 1.26.8 and 1.26.5.
version_at_least() {
	[[ $(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n 1) == "$2" ]]
}

# go_mod_version FILE: the version of the go directive.
go_mod_version() {
	awk '$1 == "go" {print $2; exit}' "$1"
}

check_toolchain() {
	command -v go >/dev/null 2>&1 || die "go is not installed (dnf install golang)"
	local have need
	have=$(GOTOOLCHAIN=local go env GOVERSION)
	have=${have#go}
	need=$(go_mod_version "$SNOUT_SRC_DIR/go.mod")
	[[ -n $need ]] || die "cannot read the go version from $SNOUT_SRC_DIR/go.mod"
	version_at_least "$have" "$need" || die "Go $have is older than go $need required by go.mod; install a newer golang package"
	log "Using Go $have (go.mod requires $need)"
}

fetch_source() {
	export GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes"
	if [[ ! -d $SNOUT_SRC_DIR/.git ]]; then
		log "Cloning $SNOUT_REPO into $SNOUT_SRC_DIR"
		install -d -m 0755 "$(dirname "$SNOUT_SRC_DIR")"
		git clone --quiet "$SNOUT_REPO" "$SNOUT_SRC_DIR" ||
			die "git clone failed; make sure root's ~/.ssh has a key with read access to $SNOUT_REPO"
	else
		git -C "$SNOUT_SRC_DIR" remote set-url origin "$SNOUT_REPO"
		git -C "$SNOUT_SRC_DIR" fetch --quiet --prune --tags --force origin ||
			die "git fetch failed; make sure root's ~/.ssh has a key with read access to $SNOUT_REPO"
	fi
	local target=$SNOUT_REF
	if git -C "$SNOUT_SRC_DIR" rev-parse --verify --quiet "refs/remotes/origin/$SNOUT_REF" >/dev/null; then
		target="origin/$SNOUT_REF"
	fi
	git -C "$SNOUT_SRC_DIR" rev-parse --verify --quiet "$target^{commit}" >/dev/null ||
		die "ref '$SNOUT_REF' not found in $SNOUT_REPO"
	git -C "$SNOUT_SRC_DIR" checkout --quiet --force --detach "$target"
	git -C "$SNOUT_SRC_DIR" clean --quiet -ffdx
	NEW_COMMIT=$(git -C "$SNOUT_SRC_DIR" rev-parse --short HEAD)
	log "Checked out $SNOUT_REF at $NEW_COMMIT"
}

build() {
	BUILD_DIR=$(mktemp -d)
	export GOTOOLCHAIN=local
	if [[ $SNOUT_SKIP_TESTS != 1 ]]; then
		log "Running go test ./..."
		(cd "$SNOUT_SRC_DIR" && go test ./...) || die "unit tests failed; the installed Snout was not touched"
	fi
	log "Building Snout"
	(cd "$SNOUT_SRC_DIR" && CGO_ENABLED=0 go build -trimpath -o "$BUILD_DIR/snout" ./cmd/snout) ||
		die "build failed; the installed Snout was not touched"
}

install_unit() {
	local source=$SNOUT_SRC_DIR/misc/snout.service
	[[ -f $source ]] || die "$source not found in the checkout"
	if [[ ! -f $UNIT ]] || ! cmp -s "$source" "$UNIT"; then
		log "Installing $UNIT"
		install -m 0644 "$source" "$UNIT"
	fi
	systemctl daemon-reload
	systemctl enable snout >/dev/null 2>&1
}

rollback() {
	warn "snout failed to start with commit $NEW_COMMIT"
	journalctl -u snout -n 30 --no-pager >&2 || true
	if [[ -f $BINARY.prev ]]; then
		warn "restoring the previous binary"
		install -m 0755 "$BINARY.prev" "$BINARY"
		systemctl restart snout || true
		if systemctl is-active --quiet snout; then
			warn "previous version ($OLD_COMMIT) is running again"
		else
			warn "previous version failed to start as well"
		fi
	fi
	die "update to $NEW_COMMIT failed"
}

install_binary() {
	log "Stopping snout"
	systemctl stop snout 2>/dev/null || true
	if [[ -f $BINARY ]]; then
		cp -p "$BINARY" "$BINARY.prev"
	fi
	install -m 0755 "$BUILD_DIR/snout" "$BINARY"
	restorecon "$BINARY" 2>/dev/null || true
	rm -rf "$BUILD_DIR"

	log "Starting snout"
	systemctl start snout || rollback
	sleep 5
	systemctl is-active --quiet snout || rollback
	install -d -m 0755 "$STATE_DIR"
	echo "$NEW_COMMIT" >"$COMMIT_FILE"
}

main() {
	case ${1:-} in
	-h | --help)
		usage
		exit 0
		;;
	"") ;;
	*) die "unexpected argument '$1' (configure with environment variables, see --help)" ;;
	esac
	[[ $EUID -eq 0 ]] || die "run as root"
	[[ -f $CONFIG ]] || die "$CONFIG is missing; run misc/bootstrap-hypervisor.sh first"
	command -v git >/dev/null 2>&1 || die "git is not installed"

	OLD_COMMIT=$(cat "$COMMIT_FILE" 2>/dev/null || echo none)
	fetch_source
	check_toolchain
	build
	install_unit
	install_binary
	log "Snout updated: $OLD_COMMIT -> $NEW_COMMIT ($("$BINARY" -v))"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
	main "$@"
fi
