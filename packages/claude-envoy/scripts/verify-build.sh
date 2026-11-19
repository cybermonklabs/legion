#!/usr/bin/env bash
# verify-build.sh <tar> — build the plugin from this tree, check the build is one the release
# publishes, and pack it into <tar>, leaving the checkout as it found it.
#
# This is the one statement of what the published plugin must hold. release.yaml's
# claude_envoy_build runs it and then packs the tarball it publishes to npm; ci.yaml's required
# test job and its claude-envoy job run the very same command on a pull request, so a pull request
# fails a check for whatever the release would refuse instead of merging and then failing every
# release on main.
#
# It writes no file of the checkout: nothing commits dist/ or legion/, and jj records whatever a
# command writes into the working copy. Every build goes into a scratch directory outside the
# repository, and <tar> must be outside it too. The jobs that then put <tar> into a checkout are
# named at `release-push.sh extract-built`.
#
# Every file this script and scripts/build.ts read is a build input, so scripts/build-inputs.txt
# must name it: that list is what release.yaml's $CLAUDE_ENVOY_VERSION_ARGS covers, so a build
# input missing from it is one no release bumps a version for. tests/build-inputs.test.ts checks
# the list against what the bundles and the Legion plugin read, but not against what this script
# or build.ts read by path, which the list's header names as listed by hand
# (release-push.sh, .bun-version, the scripts build.ts imports or runs, and setup-bun): a new file
# read here goes into the list by hand.
#
# In order, whatever the caller's directory:
#   1. `bun scripts/build.ts --out` twice, into two scratch directories, and the two builds'
#      dist/ and legion/ compared byte for byte, so what the release publishes is what the source
#      builds to, not one run's accident;
#   2. `tar -cf <tar>` of the first build's dist/ and legion/, the tar the build job extracts;
#   3. `.github/scripts/release-push.sh check-built <tar>`, the member checks: no symlink, no
#      executable file, no name outside letters, digits, '.', '_' and '-', and every bundle and
#      file a build holds (a skill pi-legion ships is staged into legion/ with `cp -R`, which
#      keeps its modes and symlinks).
# Any failure exits non-zero.
#
# The private-name check of the build is not here. Every job that runs this script runs
# dependency code, so none holds the deployment's term list, which that check reads. A job of its
# own runs it over the packed tarball's dist/ and legion/, the very bytes this script tars, after
# the job that packed it: ci-job-claude-envoy.yaml's private-names and release.yaml's
# claude_envoy_names.
#
# Run from packages/claude-envoy: bun run verify-build <tar outside the repository>
set -euo pipefail

[ $# -eq 1 ] || {
  echo "::error::verify-build.sh: usage: verify-build.sh <tar outside the repository>" >&2
  exit 2
}
tar_file=$1

package_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
repo_root=$(cd "$package_dir/../.." && pwd -P)

# <tar>, made absolute from the caller's directory, must sit outside the repository, where jj
# would record it.
tar_dir=$(cd "$(dirname -- "$tar_file")" 2>/dev/null && pwd -P) || {
  echo "::error::verify-build.sh: the directory of $tar_file does not exist" >&2
  exit 2
}
tar_file=$tar_dir/$(basename -- "$tar_file")
case $tar_dir/ in
  "$repo_root"/*)
    echo "::error::verify-build.sh: $tar_file is inside the repository ($repo_root); name a tar outside it, such as under \$TMPDIR" >&2
    exit 2
    ;;
esac

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
first=$scratch/first
second=$scratch/second

cd "$package_dir"
bun scripts/build.ts --out "$first"
bun scripts/build.ts --out "$second"
if ! differ=$(diff -r -q "$first/dist" "$second/dist" && diff -r -q "$first/legion" "$second/legion"); then
  echo "::error::verify-build.sh: two builds of this tree differ, so its build is not reproducible and the release would publish one run's output: ${differ//$'\n'/; }" >&2
  exit 1
fi

tar -cf "$tar_file" -C "$first" dist legion
"$repo_root/.github/scripts/release-push.sh" check-built "$tar_file"
echo "verify-build.sh: $tar_file holds a build the release publishes"
