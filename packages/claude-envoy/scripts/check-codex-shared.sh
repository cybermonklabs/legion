#!/usr/bin/env bash
# Run one real-Codex case on the pinned builds. A missing prerequisite is UNKNOWN (2).
set -euo pipefail

usage() {
  echo 'Usage: check-codex-shared.sh {listed|terminals|delivered|isolated|ended|resumed|own-process} [--no-trust] [--plugin-ref COMMIT]'
}

die() {
  echo "UNKNOWN: $*" >&2
  exit 2
}

case ${1:-} in
  -h|--help) usage; exit 0 ;;
  listed|terminals|delivered|isolated|ended|resumed|own-process) case_name=$1; shift ;;
  *) usage >&2; exit 2 ;;
esac

no_trust=0
plugin_ref=''
while (( $# )); do
  case $1 in
    --no-trust) no_trust=1; shift ;;
    --plugin-ref)
      (( $# >= 2 )) || die '--plugin-ref needs a commit'
      plugin_ref=$2
      shift 2
      ;;
    *) usage >&2; exit 2 ;;
  esac
done
if (( no_trust )) && [[ $case_name == terminals || $case_name == own-process ]]; then
  die '--no-trust applies to cases driven through the shared app-server control socket'
fi

[[ -n ${DISPATCH_TEST_DATABASE_URL:-} ]] || die 'set DISPATCH_TEST_DATABASE_URL to a test Postgres server'
for tool in bun npm go git tar; do
  command -v "$tool" >/dev/null || die "$tool is missing from PATH"
done
if [[ $case_name == terminals || $case_name == own-process ]]; then
  command -v tmux >/dev/null || die 'tmux is missing from PATH'
fi
[[ $(uname -sm) == 'Linux x86_64' ]] || die 'only the Linux x64 npm builds are pinned'

package_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
repo=$(cd -- "$package_root/../.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/codex-shared.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

source=$package_root
if [[ -n $plugin_ref ]]; then
  commit=$(git -C "$repo" rev-parse --verify --end-of-options "$plugin_ref^{commit}") ||
    die "cannot resolve plugin commit $plugin_ref"
  mkdir "$scratch/source"
  git -C "$repo" archive "$commit" | tar -xf - -C "$scratch/source" ||
    die "cannot archive plugin commit $commit"
  if ! (cd "$scratch/source" && bun install --frozen-lockfile) >&2; then
    die "cannot install the dependencies of plugin commit $commit"
  fi
  source=$scratch/source/packages/claude-envoy
  printf 'Plugin source commit %s\n' "$commit"
fi

install_codex() {
  local version=$1 npm_pin native_pin prefix=$scratch/codex-$1 recorded binary
  case $version in
    0.170.0)
      npm_pin=sha512-dwoughgqjiik++CFaNkjQ5y6pJO6LUzu5P1z8Hy4LUeJ+aIYcX/QO1P5rOQnHmBG4agOM7Y+hc4LtVo/a4GFiw==
      native_pin=sha512-6cCZJaZL17tiTAvhd1Pirt8vsV1dP8CfG94+IHygUO4m24oYLQrIInJJLyKzcMv/uUonbu1dF2JmcXhA/s5bsg==
      ;;
    0.171.0)
      npm_pin=sha512-1IOFzL2Xg86tLdw6kDo3GoKbkQENgQi7TI6rlEvN132R7ngSw7SNOIQoTcnKzSZZ8kf9N9GynCweSoaFbqk2Eg==
      native_pin=sha512-l66NZkQgdk2C3pbcfULDz42yEUB581MVA6I+Y43Wp4OIqEy73HCdmZ0GTjeAHY2ycuH4N00d9VsLrwT1jxshTw==
      ;;
    0.154.0)
      npm_pin=sha512-FV/x1OHXYv/ifjf3mXj9ThTTAWcUZN6cGIRQRhRxkKNOPuImu1WW0c8ev1vUkE9XGH90dEnYG1tBjIkxRikg0w==
      native_pin=sha512-a4FI3A8sGtwGrOqltrPbrS2hajrHQG591EwmRfiRoLMb10VxdBtUGW4gu6IJVYENiYGA7k3P4jlRHEoCZU/s9Q==
      ;;
    *) die "Codex $version has no integrity pin" ;;
  esac

  if ! npm install --prefix "$prefix" --registry https://registry.npmjs.org \
    --no-save --no-audit --no-fund --ignore-scripts --loglevel=error --fetch-retries=0 \
    "@openai/codex@$version" >"$scratch/npm-$version.log" 2>&1; then
    cat "$scratch/npm-$version.log" >&2
    die "npm did not install Codex $version"
  fi
  recorded=$(bun -e '
    const packages = (await Bun.file(process.argv[1]).json()).packages ?? {};
    console.log(packages["node_modules/@openai/codex"]?.integrity,
      packages["node_modules/@openai/codex-linux-x64"]?.integrity);
  ' "$prefix/node_modules/.package-lock.json") || die "npm recorded no integrity for Codex $version"
  [[ $recorded == "$npm_pin $native_pin" ]] || die "Codex $version does not match its npm integrity pins"

  local installed=("$prefix"/node_modules/@openai/codex-linux-x64/vendor/*/bin/codex)
  binary=${installed[0]}
  [[ -x $binary ]] || die "npm left no Codex $version binary"
  local printed
  printed=$("$binary" --version 2>/dev/null) || die "the Codex $version binary does not print its version"
  [[ $printed == "codex-cli $version" ]] || die "Codex $version printed '$printed'"
  printf '%s\n' "$binary"
}

old_binary=''
versions=(0.171.0 0.170.0)
if [[ $case_name == own-process ]]; then
  old_binary=$(install_codex 0.154.0)
  versions=(0.171.0)
fi

verdict=0
for version in "${versions[@]}"; do
  binary=$(install_codex "$version")
  log=$scratch/$version.log
  result=0
  (
    cd "$package_root"
    LEGION_TEST_CODEX_SHARED_CASE=$case_name \
    LEGION_TEST_CODEX_SHARED_BINARY=$binary \
    LEGION_TEST_CODEX_SHARED_OLD_BINARY=$old_binary \
    LEGION_TEST_CODEX_SHARED_SOURCE=$source \
    LEGION_TEST_CODEX_SHARED_NO_TRUST=$no_trust \
      bun test tests/codex-shared-real.test.ts \
        --test-name-pattern " $case_name$"
  ) >"$log" 2>&1 || result=$?
  sed "s/^/Codex $version | /" "$log"
  if grep -qE 'CODEX_SHARED_SETUP_FAILED|^[[:space:]]*[1-9][0-9]* skip' "$log"; then
    echo "UNKNOWN Codex $version $case_name"
    verdict=2
  elif (( result == 0 )) && grep -qE '^[[:space:]]*1 pass[[:space:]]*$' "$log" && \
    grep -qE '^[[:space:]]*0 fail[[:space:]]*$' "$log"; then
    echo "Codex $version completed $case_name"
  else
    echo "FAIL Codex $version $case_name"
    if (( verdict == 0 )); then verdict=1; fi
  fi
done
if (( verdict == 0 )); then
  echo "PASS $case_name on Codex ${versions[*]}"
fi
exit "$verdict"
