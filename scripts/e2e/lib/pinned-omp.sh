#!/usr/bin/env bash
# The one way a devbox proof or operator script gets the Oh My Pi a mise tool spec names: installs
# it when mise has none, and holds the install to the digest the checkout records for it before
# anything runs it, as .github/actions/install-omp does in CI and the worker image's tools stage
# does in the build.
#
#   bash scripts/e2e/lib/pinned-omp.sh <root> <pin>
#
# <root> is a Legion checkout and <pin> a mise tool spec (github:cybermonklabs/oh-my-pi@<version>).
# When <pin> is the text of <root>/.omp-pin, <root>/.github/scripts/omp-digest.sh checks the
# install directory (`mise where <pin>`) against <root>/.omp-pin.sha256: bin/omp's bytes, and
# nothing else in the directory. The pin names a fork's release tag, which can be moved, so until
# that check passes the bytes mise installed are not known to be the ones the pin was tested on. A
# refusal fails this script with omp-digest.sh's own message on stderr, and the caller runs
# nothing; so does a <root> with no .omp-pin, or with no .omp-pin.sha256 beside it.
#
# Any other pin is one an operator names in place of .omp-pin (install-plugin-profile.sh's --omp,
# the operator's own build under the box-login route, an override variable). No digest is recorded
# for it, so its install is not checked, and one stderr line says so, naming the pin.
#
# Stdout is one line, the install's bin/omp (so a caller can write `omp=$(…)`). mise's install
# output and every refusal go to stderr; a check that passes prints nothing of its own
# (omp-digest.sh's line is dropped). It exits 1 when mise names no install of the pin, or one with
# no executable bin/omp, and when the check refuses.
set -euo pipefail

me=pinned-omp
if [ "$#" -ne 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
  echo "usage: $0 <root> <pin>" >&2
  exit 2
fi
root=$1 pin=$2

# Read before anything is installed, and a root without one fails here: a .omp-pin that cannot be
# read must not pass for another pin and skip the check.
if [ ! -f "$root/.omp-pin" ] || [ ! -r "$root/.omp-pin" ]; then
  echo "$me: $root/.omp-pin cannot be read" >&2
  exit 1
fi
recorded=$(<"$root/.omp-pin")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
dir=$(mise where "$pin") || {
  echo "$me: mise has no install of $pin" >&2
  exit 1
}
if [ "$pin" = "$recorded" ]; then
  bash "$root/.github/scripts/omp-digest.sh" "$dir" "$root/.omp-pin.sha256" >/dev/null || exit 1
else
  echo "$me: $pin is not $root/.omp-pin's build, and no digest is recorded for it: its install $dir is not checked" >&2
fi
if [ ! -f "$dir/bin/omp" ] || [ ! -x "$dir/bin/omp" ]; then
  echo "$me: mise has no executable bin/omp for $pin under $dir" >&2
  exit 1
fi
printf '%s\n' "$dir/bin/omp"
