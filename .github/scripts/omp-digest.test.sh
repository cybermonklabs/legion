#!/usr/bin/env bash
# Tests for `omp-digest.sh`: an install of the pinned Oh My Pi holds exactly the bytes
# .omp-pin.sha256 records, and nothing else.
#
# Each case builds an install directory under a temporary directory, as `mise where "$pin"`
# leaves it, and a digest file for it, and runs the real script on them.
#
# The worker image copies the whole install directory to /opt/omp, and /opt/omp/bin stands on
# every role's PATH ahead of /usr/local/bin and /usr/bin. So a release asset that keeps bin/omp
# byte for byte and adds bin/kubectl beside it would put an unchecked kubectl in front of the
# real one for every role. The check that holds bin/omp to its digest must also refuse an
# install directory holding any file the digest file does not name.
#
# Run from anywhere: .github/scripts/omp-digest.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
digest_script="$script_dir/omp-digest.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# install <name>: an install directory holding bin/omp alone, and a digest file naming it.
# Echoes the directory; the digest file is <directory>.sha256.
install() {
  local dir="$work/$1"
  mkdir -p "$dir/bin"
  printf '#!/bin/sh\necho omp/0.0.0\n' >"$dir/bin/omp"
  chmod +x "$dir/bin/omp"
  (cd "$dir" && sha256sum bin/omp) >"$dir.sha256"
  echo "$dir"
}

# run <dir>: runs the script on the install and its digest file, leaving its exit status in
# $status and its stderr in $stderr.
run() {
  status=0
  stderr=$("$digest_script" "$1" "$1.sha256" 2>&1 >/dev/null) || status=$?
}

echo "Case: an install holding bin/omp alone, at the recorded digest, passes"
dir=$(install clean)
run "$dir"
check "exits 0" "$(is "$status" 0)"

echo "Case: bin/omp at another digest is refused, naming both digests"
dir=$(install changed)
printf '#!/bin/sh\necho omp/9.9.9\n' >"$dir/bin/omp"
run "$dir"
check "exits 1" "$(is "$status" 1)"
check "names the digest it expected" "$(contains "$stderr" "expected sha256 $(cut -c1-64 "$dir.sha256")")"

echo "Case: bin/omp at the recorded digest beside another file in bin/ is refused"
dir=$(install extra-bin)
printf '#!/bin/sh\necho planted\n' >"$dir/bin/kubectl"
chmod +x "$dir/bin/kubectl"
run "$dir"
check "exits non-zero" "$( [ "$status" -ne 0 ] && echo true || echo false)"
check "names the file the digest file does not" "$(contains "$stderr" "bin/kubectl")"

echo "Case: bin/omp at the recorded digest beside a file in another directory is refused"
dir=$(install extra-lib)
mkdir -p "$dir/lib"
printf 'not a library\n' >"$dir/lib/libplanted.so"
run "$dir"
check "exits non-zero" "$( [ "$status" -ne 0 ] && echo true || echo false)"
check "names the file the digest file does not" "$(contains "$stderr" "lib/libplanted.so")"

echo "Case: bin/omp at the recorded digest beside a symbolic link is refused"
dir=$(install extra-link)
ln -s /bin/sh "$dir/bin/sh"
run "$dir"
check "exits non-zero" "$( [ "$status" -ne 0 ] && echo true || echo false)"
check "names the link the digest file does not" "$(contains "$stderr" "bin/sh")"

summary "omp-digest.sh holds an install to exactly the file its digest names"
