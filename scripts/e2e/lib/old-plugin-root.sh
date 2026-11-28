#!/usr/bin/env bash
# Makes the plugin root of an older daemon API contract, for a live proof that drives the legion
# tool a deployed plugin of that contract carries (decision 22 of dispatch://INFRA-4730: the oldest
# contract in the fleet, 144 on 2026-11-20): a `git archive` of the last commit whose
# packages/daemon/internal/api/version.go declares the contract, unpacked under the cache directory
# and `bun install`ed there, kept by commit so a later run reuses it. It prints the root's path on
# stdout, and everything else on stderr.
#
#   bash scripts/e2e/lib/old-plugin-root.sh <checkout> <contract> [<driver>...]
#
# <checkout> is a git checkout that holds the commit: CI's full checkout, or a local clone. A
# checkout whose history holds no commit declaring the contract — a shallow one among them — is
# refused, naming the fetch it needs. Each <driver> is a path relative to the checkout, copied into
# the root at the same path after the unpack, so the old tool runs under this checkout's driver
# (the old commit never had it). The cache directory is OLD_PLUGIN_ROOT_CACHE, by default
# legion-old-plugin-roots under TMPDIR, or under /tmp when TMPDIR is unset. A root is installed
# under a name of its own and moved into place once `bun install` succeeds, so a run that stopped
# midway leaves nothing a later run takes for an installed root.
set -euo pipefail

checkout=${1:?usage: old-plugin-root.sh <checkout> <contract> [<driver>...]}
contract=${2:?usage: old-plugin-root.sh <checkout> <contract> [<driver>...]}
shift 2
version=packages/daemon/internal/api/version.go

refuse() {
  echo "old-plugin-root: $*" >&2
  exit 1
}
command -v bun >/dev/null || refuse "bun is not on PATH; the old plugin's dependencies are installed with it"
# declares is whether commit's version.go declares the contract.
declares() {
  git -C "$checkout" show "$1:$version" 2>/dev/null | grep -qx "const DaemonAPIVersion = $contract"
}
# The newest commit that added or removed the declaration: the one that moved the contract on, whose
# parent is the last that declares it, or the one that introduced it while it is still current.
newest=$(git -C "$checkout" log -1 --format=%H -S"const DaemonAPIVersion = $contract" -- "$version")
[ -n "$newest" ] || refuse "no commit of $checkout adds or removes daemon API contract $contract in $version; a shallow checkout needs \`git fetch --unshallow\` first"
if declares "$newest"; then
  commit=$newest
else
  commit=$(git -C "$checkout" rev-parse "$newest^")
  declares "$commit" || refuse "commit $commit, the parent of $newest, does not declare daemon API contract $contract in $version"
fi

cache=${OLD_PLUGIN_ROOT_CACHE:-${TMPDIR:-/tmp}/legion-old-plugin-roots}
root=$cache/$commit
if [ ! -f "$root/.old-plugin-root-installed" ]; then
  partial=$root.partial
  rm -rf -- "$partial"
  mkdir -p -- "$partial"
  echo "old-plugin-root: unpacking $commit (daemon API contract $contract) into $root" >&2
  git -C "$checkout" archive "$commit" | tar -x -C "$partial"
  (cd "$partial" && bun install --frozen-lockfile) >&2
  touch "$partial/.old-plugin-root-installed"
  rm -rf -- "$root"
  mv -- "$partial" "$root"
fi
for driver in "$@"; do
  [ -f "$checkout/$driver" ] || refuse "$checkout has no $driver to copy into the old root"
  mkdir -p -- "$root/$(dirname -- "$driver")"
  cp -- "$checkout/$driver" "$root/$driver"
done
echo "$root"
