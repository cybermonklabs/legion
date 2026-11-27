#!/usr/bin/env bash
# Packs one of this checkout's two Oh My Pi plugins, @sjawhar/pi-envoy (packages/pi-envoy) or
# @sjawhar/pi-legion (packages/pi-legion), exactly as the release packs it, into a tarball holding
# what `npm pack` ships (package.json `files`: dist/ with the one bundle and the prepack's
# dist/skills, the Legion plugin's agents/, the packed manifest). Every script that installs a
# branch-built plugin packs through here (lib/install-plugin-profile.sh, which the stage proofs and
# the skill-scenario rig call), so the plugins they run are the ones the release publishes. The
# worker image packs on its own, as release.yaml does, with the same jq rewrite and `bun pm pack`;
# the prepack (scripts/pi-plugin-prepack.sh) refuses any other omp.extensions.
#
#   scripts/e2e/lib/pack-plugin.sh <pi-envoy|pi-legion> <out dir>
#
# The first argument is the package's directory under packages/, and nothing else is taken: the
# other workspace packages are not plugins. Stdout is one line, the tarball's path; every step's own
# output goes to stderr. <out dir> is created if missing and must hold no tarball already.
#
# The pack runs in a staged copy of the checkout under this run's temp directory, never in the
# checkout itself: a test of this checkout that reads the committed manifest while a pack runs
# beside it (`bun test --parallel`, where a skill-scenario rig row builds a profile from this
# checkout) would otherwise read the rewrite. The stage holds a copy of the package's own files,
# without dist/ (the prepack builds it) and with node_modules linked; every other top-level entry
# of the checkout, and every other package, is a symlink to the checkout's. So the prepack's
# `../../scripts`, its skills/ and the bundle's @legion/* imports (through the workspace root's
# node_modules, `bun install --frozen-lockfile` at the root first) resolve as they do in the
# checkout, and the bundle's code is the one a pack in the checkout builds. Only the source-path
# comments bun writes above each inlined module differ: bun writes them from the stage to the
# module's real path, so they name the checkout by its absolute path where a pack in place writes
# `../../node_modules/…`. Nothing reads those comments.
#   1. stage the package                       this script's own need: the checkout is read, never written
#   2. omp.extensions -> the packed bundle     release.yaml pi_envoy_build/pi_legion_build "Point extensions at the packed bundle"; worker.Dockerfile's `jq '.omp.extensions = …'`
#      pi-legion: legion.daemonApiVersion ->   pi_legion_build's "Set release version"; worker.Dockerfile's same jq
#      the tree's contract (scripts/daemon-contract.sh)
#   3. bun pm pack; its prepack builds dist/   release.yaml "Pack extension"; the prepack's `bun build`
# Each source is cited by what it runs, never by line number: the lines move with every edit
# above them. Of release.yaml's "Set release version", only pi-legion's contract is written here:
# the tarball carries the checkout's own version. No committed manifest names the contract; the
# packed one declares the tree's, the number the daemon built from this tree requires of the
# installed manifest. The bundle is the committed manifest's one extension entry,
# extensions/<entry>.ts, as dist/<entry>.js: the list the prepack requires. The bundle inlines
# package.json, so it is built while the staged manifest names the packed bundle and the
# contract, as the release builds it.
set -euo pipefail

me=pack-plugin
refuse() {
  echo "$me: $*" >&2
  echo "usage: $0 <pi-envoy|pi-legion> <out dir>" >&2
  exit 2
}

[ $# -eq 2 ] || refuse "expected two arguments, the package to pack (pi-envoy or pi-legion) and the directory the tarball is written to"
package=$1
case "$package" in
pi-envoy | pi-legion) ;;
*) refuse "'$package' is not a plugin this checkout packs; the first argument is pi-envoy or pi-legion" ;;
esac
out=$(realpath -m -- "$2")
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
case "$out/" in
"$root"/*) refuse "<out dir> $out is inside the checkout $root, where jj would snapshot the tarball" ;;
esac
mkdir -p "$out"
if compgen -G "$out/*.tgz" >/dev/null; then
  refuse "<out dir> $out already holds a tarball, so the one this run packs could not be told apart"
fi

src=$root/packages/$package
manifest=$src/package.json
work=$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")
# `|| true`: the trap runs under `set -e`, so a stage it cannot remove (a copied directory with no
# owner write, say) would otherwise make a good pack exit non-zero, which every caller reads as a
# failed pack. Such a stage stays in the temp directory and the run's status is the pack's own.
trap 'rm -rf "$work" || true' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
# bash's `Terminated` notice for an interrupted pack, written to a stderr whose reader has gone,
# would otherwise kill the script of SIGPIPE before the EXIT trap removes the stage.
trap '' PIPE

# The stage: every top-level entry of the checkout but packages/, and every package but this one,
# as a symlink; this package's own files copied with their modes (the prepack takes group and other
# write from what it ships, so a copy that kept less would hide what it normalizes), its
# node_modules linked, its dist/ left out.
stage=$work/stage
staged=$stage/packages/$package
mkdir -p "$staged"
shopt -s nullglob dotglob
for path in "$root"/*; do
  [ "${path##*/}" = packages ] || ln -s "$path" "$stage/"
done
for path in "$root"/packages/*; do
  [ "${path##*/}" = "$package" ] || ln -s "$path" "$stage/packages/"
done
for path in "$src"/*; do
  case "${path##*/}" in
  dist) ;;
  node_modules) ln -s "$path" "$staged/" ;;
  *) cp -a "$path" "$staged/" ;;
  esac
done
shopt -u nullglob dotglob

# The committed manifest names one source entry, extensions/<entry>.ts; the packed one names that
# entry's bundle alone, ["dist/<entry>.js"], the only list the prepack packs.
entry=$(jq -r 'if (.omp.extensions | length) == 1 then .omp.extensions[0] else empty end' "$manifest")
case "$entry" in
extensions/*.ts) entry=${entry#extensions/} && entry=${entry%.ts} ;;
*) refuse "$manifest names omp.extensions $(jq -c '.omp.extensions' "$manifest"); the committed list is one source entry, extensions/<entry>.ts" ;;
esac
# pi-legion's packed manifest declares the tree's daemon API contract, which its bundle inlines and
# the daemon's boot gate reads off the installed manifest; pi-envoy speaks no contract.
filter='.omp.extensions = [$bundle]'
contract=null
if [ "$package" = pi-legion ]; then
  contract=$(bash "$root/scripts/daemon-contract.sh")
  filter="$filter | .legion.daemonApiVersion = \$contract"
fi
# Written through the copy's own file, so it keeps the mode the copy took from the checkout's.
jq --arg bundle "dist/$entry.js" --argjson contract "$contract" "$filter" "$manifest" >"$staged/package.json"

(cd "$staged" && bun pm pack --destination "$out") >&2
tarballs=("$out"/*.tgz)
if [ "${#tarballs[@]}" != 1 ] || [ ! -f "${tarballs[0]}" ]; then
  echo "$me: bun pm pack left ${#tarballs[@]} entries matching $out/*.tgz; expected one tarball" >&2
  exit 1
fi
printf '%s\n' "${tarballs[0]}"
