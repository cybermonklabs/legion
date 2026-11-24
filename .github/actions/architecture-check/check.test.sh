#!/usr/bin/env bash
# Tests for check.sh's go-path mode: the Go on PATH matches the release packages/envoy/go.mod
# names (match), differs from it (refusal), the caller's environment cannot change the version
# (env pinning), a go line without a patch number is refused, and an unknown mode is refused.
# Each case builds a temporary directory with a stand-in go on PATH and a fixture go.mod, then
# runs check.sh go-path. The stand-in go responds to `env GOVERSION` with a configured version.
# No network.
#
# Run from anywhere: .github/actions/architecture-check/check.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/../../scripts/test-lib.sh"

# gomod_go <dir> <version>: writes a go.mod with a go line only.
gomod_go() {
  mkdir -p "$1/.github/actions/architecture-check"
  mkdir -p "$1/packages/envoy"
  printf 'module example\n\ngo %s\n' "$2" > "$1/packages/envoy/go.mod"
}

# gomod_toolchain <dir> <go-version> <toolchain-version>: writes a go.mod with both lines.
gomod_toolchain() {
  mkdir -p "$1/.github/actions/architecture-check"
  mkdir -p "$1/packages/envoy"
  printf 'module example\n\ngo %s\n\ntoolchain go%s\n' "$2" "$3" > "$1/packages/envoy/go.mod"
}

# standin_go <dir> <version>: writes a stand-in go that responds to `env GOVERSION`.
standin_go() {
  mkdir -p "$1/bin"
  cat > "$1/bin/go" <<SCRIPT
#!/usr/bin/env bash
if [ "\$1" = "env" ] && [ "\$2" = "GOVERSION" ]; then
  echo "$2"
else
  echo "stand-in go: unexpected arguments: \$*" >&2
  exit 1
fi
SCRIPT
  chmod +x "$1/bin/go"
}

# standin_go_env <dir> <pinned-version> <unpinned-version>: a stand-in go that returns
# <pinned-version> when GOTOOLCHAIN=local, GOWORK=off and GOENV=off are all set, and
# <unpinned-version> otherwise. This proves check.sh reads with the build's own settings.
standin_go_env() {
  mkdir -p "$1/bin"
  cat > "$1/bin/go" <<SCRIPT
#!/usr/bin/env bash
if [ "\$1" = "env" ] && [ "\$2" = "GOVERSION" ]; then
  if [ "\$GOTOOLCHAIN" = "local" ] && [ "\$GOWORK" = "off" ] && [ "\$GOENV" = "off" ]; then
    echo "$2"
  else
    echo "$3"
  fi
else
  echo "stand-in go: unexpected arguments: \$*" >&2
  exit 1
fi
SCRIPT
  chmod +x "$1/bin/go"
}

echo "case: the Go on PATH matches go.mod's go line"
d="$work/match"
gomod_go "$d" "1.27.3"
standin_go "$d" "go1.27.3"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "exits 0" "$(is "$status" 0)"

echo "case: the Go on PATH differs from go.mod's go line"
d="$work/mismatch"
gomod_go "$d" "1.27.3"
standin_go "$d" "go1.26.0"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "exits 2" "$(is "$status" 2)"
check "names the Go on PATH" "$(contains "$out" 'go1.26.0')"
check "names the go.mod release" "$(contains "$out" 'go1.27.3')"
check "says to install the go.mod release" "$(contains "$out" 'install go1.27.3 before the action')"

echo "case: go.mod has a toolchain line and the Go on PATH matches it"
d="$work/toolchain-match"
gomod_toolchain "$d" "1.27.0" "1.27.3"
standin_go "$d" "go1.27.3"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "exits 0 when Go matches the toolchain line" "$(is "$status" 0)"

echo "case: go.mod has a toolchain line and the Go on PATH matches the go line, not the toolchain"
d="$work/toolchain-mismatch"
gomod_toolchain "$d" "1.27.0" "1.27.3"
standin_go "$d" "go1.27.0"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "exits 2 when Go matches go line but not toolchain line" "$(is "$status" 2)"
check "names the toolchain release" "$(contains "$out" 'go1.27.3')"
check "says to install the toolchain release" "$(contains "$out" 'install go1.27.3 before the action')"

echo "case: the caller's GOTOOLCHAIN/GOWORK/GOENV cannot change the version"
d="$work/env-pinned"
gomod_go "$d" "1.27.3"
standin_go_env "$d" "go1.27.3" "go1.28.0"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "exits 0 with pinned settings" "$(is "$status" 0)"
# Without the env pinning, the stand-in would return go1.28.0 and the check would refuse.
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  GOTOOLCHAIN=auto GOWORK='' GOENV='' \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "still exits 0 even when the caller sets GOTOOLCHAIN=auto" "$(is "$status" 0)"

echo "case: a go line without a patch number is refused"
d="$work/no-patch"
gomod_go "$d" "1.27"
standin_go "$d" "go1.27.0"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" PATH="$d/bin:$PATH" \
  bash "$check_script" go-path 2>&1) && status=0 || status=$?
check "exits 2" "$(is "$status" 2)"
check "names the incomplete release" "$(contains "$out" 'go1.27')"
check "says path mode needs a full release" "$(contains "$out" 'path mode needs a full release')"

echo "case: an unknown mode is refused"
out=$(GITHUB_ACTION_PATH="$d/.github/actions/architecture-check" \
  bash "$check_script" banana 2>&1) && status=0 || status=$?
check "exits 2" "$(is "$status" 2)"
check "prints usage" "$(contains "$out" 'usage: check.sh')"

summary "check.sh go-path"
