#!/usr/bin/env bash
# Tests for `check-gitleaks-version.sh`: the gitleaks the release lane fetches is the gitleaks the
# daemon's module requires. The case that matters is the one the check exists for — a pin edited
# alone, in either file — so both directions are here.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-gitleaks-version.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-gitleaks-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — a go.mod requiring gitleaks 8.30.1 and a release script
# fetching that release's Linux asset. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/daemon"
  ln -s "$check_script" "$root/.github/scripts/check-gitleaks-version.sh"
  cat > "$root/packages/daemon/go.mod" <<'MOD'
module github.com/example/daemon

go 1.27.3

require (
	github.com/example/other v1.2.3
	github.com/zricethezav/gitleaks/v8 v8.30.1 // indirect
)
MOD
  cat > "$root/.github/scripts/release-push.sh" <<'SH'
#!/usr/bin/env bash
# The leak guard's engine: gitleaks, the version packages/daemon/go.mod holds (8.30.1).
leak_engine_url=https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz
leak_engine_sha256=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb
SH
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-gitleaks-version.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the release lane fetches the version the module requires"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it compared" \
  "$(contains "$out" "the release lane fetches gitleaks 8.30.1, the version packages/daemon/go.mod requires for the leak guard")"

echo "case: the module pin moved and the release lane's did not"
root=$(fixture module-moved)
sed -i 's|gitleaks/v8 v8.30.1|gitleaks/v8 v8.31.0|' "$root/packages/daemon/go.mod"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both files, both versions and the fix" \
  "$(contains "$out" "release-push.sh:3 fetches gitleaks 8.30.1, and packages/daemon/go.mod:7 requires gitleaks 8.31.0: move both to one version, with leak_engine_sha256 taken from that release's own gitleaks_8.31.0_checksums.txt")"

echo "case: the release lane's pin moved and the module's did not"
root=$(fixture asset-moved)
sed -i 's|v8.30.1/gitleaks_8.30.1_linux_x64|v8.31.0/gitleaks_8.31.0_linux_x64|' "$root/.github/scripts/release-push.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both files, both versions and the fix" \
  "$(contains "$out" "release-push.sh:3 fetches gitleaks 8.31.0, and packages/daemon/go.mod:7 requires gitleaks 8.30.1: move both to one version, with leak_engine_sha256 taken from that release's own gitleaks_8.30.1_checksums.txt")"

echo "case: an asset URL whose tag and file name are two versions"
root=$(fixture asset-halves)
sed -i 's|gitleaks_8.30.1_linux_x64|gitleaks_8.30.0_linux_x64|' "$root/.github/scripts/release-push.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both halves" \
  "$(contains "$out" 'fetches the file of gitleaks 8.30.0 from the release of gitleaks 8.30.1')"

echo "case: a version that is no exact release"
root=$(fixture module-floating)
sed -i 's|gitleaks/v8 v8.30.1|gitleaks/v8 v8.30.1-0.20261101120000-abcdef123456|' "$root/packages/daemon/go.mod"
run_check "$root"
check "exits 1 for a module pseudo-version" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" 'github.com/zricethezav/gitleaks/v8 v8.30.1-0.20261101120000-abcdef123456 is no exact gitleaks release; write vX.Y.Z')"

root=$(fixture asset-floating)
sed -i 's|leak_engine_url=.*|leak_engine_url=https://github.com/gitleaks/gitleaks/releases/latest/download/gitleaks_linux_x64.tar.gz|' \
  "$root/.github/scripts/release-push.sh"
run_check "$root"
check "exits 1 for a floating asset URL" "$(is "$status" 1)"
check "names it and the shape it wants" \
  "$(contains "$out" 'leak_engine_url=https://github.com/gitleaks/gitleaks/releases/latest/download/gitleaks_linux_x64.tar.gz is no exact gitleaks release asset; write https://github.com/gitleaks/gitleaks/releases/download/vX.Y.Z/gitleaks_X.Y.Z_linux_x64.tar.gz')"

echo "case: no pin, or two"
root=$(fixture module-none)
sed -i '/gitleaks\/v8/d' "$root/packages/daemon/go.mod"
run_check "$root"
check "exits 1 with no module requirement" "$(is "$status" 1)"
check "says how many" \
  "$(contains "$out" 'must require github.com/zricethezav/gitleaks/v8 exactly once, as vX.Y.Z; it does 0 times')"

root=$(fixture module-twice)
printf 'require github.com/zricethezav/gitleaks/v8 v8.30.1\n' >> "$root/packages/daemon/go.mod"
run_check "$root"
check "exits 1 with two module requirements" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'it does 2 times')"

root=$(fixture asset-none)
sed -i '/leak_engine_url=/d' "$root/.github/scripts/release-push.sh"
run_check "$root"
check "exits 1 with no asset URL" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'must set leak_engine_url exactly once, to https://github.com/gitleaks/gitleaks/releases/download/vX.Y.Z/gitleaks_X.Y.Z_linux_x64.tar.gz; it does 0 times')"

root=$(fixture asset-twice)
printf 'leak_engine_url=https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz\n' \
  >> "$root/.github/scripts/release-push.sh"
run_check "$root"
check "exits 1 with two asset URLs" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'it does 2 times')"

echo "case: a file that is not there"
root=$(fixture module-missing)
rm "$root/packages/daemon/go.mod"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it, without a traceback" "$(contains "$out" 'packages/daemon/go.mod is missing')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

root=$(fixture asset-missing)
rm "$root/.github/scripts/release-push.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it, without a traceback" "$(contains "$out" 'release-push.sh is missing')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

summary "check-gitleaks-version.sh"
