#!/usr/bin/env bash
# Install a job's Ubuntu packages from the .deb files an Actions cache entry holds, with no
# network, and fetch them with bounded retries only when the entry is missing.
#
# Why: daemon-go's `sudo apt-get update && sudo apt-get install tmux` hung 68 minutes in
# `apt-get update` after the runner's azure mirror failed, and the job reached its 70-minute limit
# (run 44523167376, attempt 2) on a change that touched nothing it tests. A step that reads a
# mirror while the job runs fails whenever that mirror does. So the packages come from a cache
# the job restores, which .github/workflows/ci-caches.yaml fills on a push to main, and the
# network is read only on a miss, under a bound the step's own timeout-minutes is above.
#
#   apt-cache.sh image
#     Prints `image=<ID>-<VERSION_ID>` of /etc/os-release (`image=ubuntu-24.04`), for a step to
#     append to $GITHUB_OUTPUT: the release the runner's image is built on, which is what decides
#     which archive the .deb files come from, for the cache key. The image's own build (its
#     ImageVersion, which GitHub moves about weekly) is not in it, because an entry holds each
#     pin's whole dependency closure (below), which installs on every build of that release.
#   apt-cache.sh fill LIST DIR
#     Fetches every pin LIST holds into DIR (ci-caches.yaml's apt job, on a cache miss).
#   apt-cache.sh install LIST DIR PACKAGE...
#     Installs each PACKAGE at the version LIST pins it to. From DIR when DIR holds those pins (a
#     hit: the restored entry): every file is checked against the checksums the fill wrote, and
#     apt installs from DIR alone, with no network. On a miss it fetches those pins first, under
#     the bound below, and then installs from what it fetched in the same way.
#
# LIST holds one pin a line, `name=version` (`tmux=3.4-1ubuntu0.2`); `#` starts a comment. The
# fill, the install and the key's hashFiles all read it, so a version change is one line.
#
# DIR, the cache entry:
#   PINS        `apt-cache layout 1`, then the pins whose closures DIR holds, sorted
#   Packages    the index apt reads DIR with: each .deb's control fields, its Filename, Size and
#               SHA256
#   Release     the SHA-256 of Packages, so apt reads that one index and asks for no other form
#   SHA256SUMS  the SHA-256 of Packages, Release and every .deb (sha256sum's own format)
#   debs/       the .deb files
# A pin's closure is every package its install needs on a system with nothing installed (apt
# resolving against an empty dpkg status, Recommends left out), so the entry does not depend on
# what the image that filled it happened to carry: on another build of the same release, apt takes
# from DIR only what that runner lacks, and leaves alone what it already has at that version or a
# newer one. A pinned package the image already carries at its pin is left as it is (the
# ubuntu-24.04 image carries tmux 3.4-1ubuntu0.2: daemon-go's step printed `tmux is already the
# newest version` in run 44533492400), and one the image carries at another version is moved to
# its pin (--allow-downgrades), so the tests run the version the list names. apt removes nothing
# to get there (--no-remove): a pin that would take another package off the runner fails, naming
# it, rather than changing the image under the job's other steps.
#
# Why apt-get install from DIR, and not `dpkg -i` of the files: apt reads DIR as a local
# repository (`deb [trusted=yes] file:DIR ./`, and no other source, so it reaches no mirror),
# resolves the pins against the runner's installed packages, and refuses before dpkg runs when a
# dependency cannot be met, naming it. `dpkg -i` of a closure would downgrade a library the image
# carries newer, and on a dependency it cannot meet it leaves a package unpacked and unconfigured
# for the steps after it. apt also checks each file against Packages' SHA256 as it installs; the
# check of SHA256SUMS first is there to name the file that does not match. Packages' trust is the
# entry's: only ci-caches.yaml's push to main writes it, from files apt fetched from the runner's
# signed indexes.
#
# The bound on a fetch. Each attempt is an `apt-get update` and a download into private
# directories (no root needed, nothing of the runner's own apt state written), each under
# `timeout`, apt's own Acquire::Retries and its HTTP and HTTPS timeouts. Up to
# APT_CACHE_ATTEMPTS attempts (3), APT_CACHE_PAUSE seconds (5) before the second and twice that
# before the third, all inside APT_CACHE_BUDGET seconds (240) from the fetch's start: an attempt
# never runs past what the budget leaves, so the script fails with its own message before a step
# with `timeout-minutes: 5` is cancelled. Each failed attempt names the pins and the hosts apt could
# not reach, read off apt's own Err and Failed lines; a hang names the hosts it had reached when the
# timeout ended it. With no network at all the resolver fails each attempt at once, so a miss fails
# in seconds. A pin no index holds while the update itself succeeded fails at once: Ubuntu's
# archive keeps only the newest version of a pocket, so that pin has to move, and no retry finds it.
#
# Exits 0 when every PACKAGE is installed at its pin; 1 when a fetch fails every attempt, a file
# does not match its checksum, apt refuses the install or the installed version is not the pin,
# naming what; 2 on a usage error, a LIST line that is no pin, or a PACKAGE LIST does not pin.
#
# Run from anywhere: .github/scripts/apt-cache.sh install .github/apt-packages.txt "$RUNNER_TEMP/apt" tmux
# Its test is .github/scripts/apt-cache.test.sh, in ci.yaml's test job.
set -euo pipefail

LAYOUT="apt-cache layout 1"
attempts=${APT_CACHE_ATTEMPTS:-3}
pause=${APT_CACHE_PAUSE:-5}
budget=${APT_CACHE_BUDGET:-240}
# One attempt may take this long at most: an `apt-get update` of every source the runner reads and
# the download of a closure, about 30 MB, take seconds on a healthy mirror.
attempt_cap=${APT_CACHE_ATTEMPT_SECONDS:-120}

usage() {
  echo "usage: apt-cache.sh image | apt-cache.sh fill LIST DIR | apt-cache.sh install LIST DIR PACKAGE..." >&2
  exit 2
}

fail() { # fail MESSAGE: an error annotation, exit 1
  echo "::error::apt-cache: $*" >&2
  exit 1
}

# as_root <command...>: runs it as root, through sudo when this is not root already.
as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

# read_list LIST: sets PIN (name -> version) and NAMES (in LIST's order) from LIST; exit 2 on a
# line that is no pin, a name pinned twice, or a LIST that pins nothing.
declare -A PIN=()
NAMES=()
read_list() {
  local file=$1 line entry n=0
  [ -f "$file" ] || { echo "apt-cache: no list $file" >&2; exit 2; }
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    entry=${line%%#*}
    entry=${entry#"${entry%%[![:space:]]*}"}
    entry=${entry%"${entry##*[![:space:]]}"}
    [ -n "$entry" ] || continue
    # A Debian package name and an exact version (Debian Policy 5.6.1 and 5.6.12): a line without a
    # version would install whatever the mirror serves that day, which no key would then name.
    if ! [[ "$entry" =~ ^([a-z0-9][a-z0-9+.-]+)=([0-9]+:)?[0-9][A-Za-z0-9.+~-]*$ ]]; then
      echo "apt-cache: $file:$n: '$entry' is not a pin, name=version (tmux=3.4-1ubuntu0.2)" >&2
      exit 2
    fi
    local name=${BASH_REMATCH[1]}
    if [ -n "${PIN[$name]+set}" ]; then
      echo "apt-cache: $file:$n: $name is pinned twice" >&2
      exit 2
    fi
    PIN[$name]=${entry#*=}
    NAMES+=("$name")
  done < "$file"
  [ "${#NAMES[@]}" -gt 0 ] || { echo "apt-cache: $file pins no package" >&2; exit 2; }
}

# hosts_of FILE PATTERN: the distinct hosts of the URLs on FILE's lines matching the extended
# regex PATTERN, comma-separated; also each host apt names in a "Could not resolve", "Could not
# connect to" or "Unable to connect to" there.
hosts_of() {
  local file=$1 pattern=$2
  {
    grep -E -- "$pattern" "$file" | grep -Eo '[a-z][a-z0-9+]*://[^/ ]+' | sed -E 's#^[^:]+://##; s#:[0-9]+$##' || true
    grep -Eo "Could not resolve '[^']+'" "$file" | sed -E "s/^Could not resolve '//; s/'$//" || true
    grep -Eo '(Could not connect to|Unable to connect to) [^: ]+' "$file" | sed -E 's/^.* //' || true
  } | awk 'NF && !seen[$0]++' | paste -sd, - | sed 's/,/, /g'
}

# fetch DEST PIN...: each PIN's closure into DEST (made fresh), with its Packages, SHA256SUMS and
# PINS, under the bound the header gives; exit 1 naming the pins and the hosts when every attempt
# fails.
fetch() {
  local dest=$1
  shift
  local pins=("$@") work started now left per n=1 wait=$pause log reached failed
  work=$(mktemp -d)
  mkdir -p "$work/lists/partial" "$work/cache" "$work/archives/partial"
  : > "$work/status"
  local opts=(
    -o "Dir::State::Lists=$work/lists" -o "Dir::State::status=$work/status"
    -o "Dir::Cache=$work/cache" -o "Dir::Cache::archives=$work/archives"
    -o Debug::NoLocking=1 -o Acquire::Languages=none
    -o Acquire::Retries=2 -o Acquire::http::Timeout=20 -o Acquire::https::Timeout=20
  )
  # As root, apt hands downloads to its _apt user, which cannot write a directory mktemp made.
  if [ "$(id -u)" -eq 0 ]; then
    opts+=(-o APT::Sandbox::User=root)
  fi
  log=$work/apt.log
  started=$SECONDS
  while :; do
    now=$((SECONDS - started))
    left=$((budget - now))
    if [ "$left" -le 0 ]; then
      rm -rf "$work"
      fail "fetched none of ${pins[*]} within the ${budget}s budget (APT_CACHE_BUDGET) after $((n - 1)) attempts"
    fi
    per=$(( left < attempt_cap ? left : attempt_cap ))
    echo "apt-cache: fetching ${pins[*]} with what they depend on (attempt $n of $attempts, at most ${per}s)" >&2
    : > "$log"
    rm -rf "$work/archives"/*.deb
    local status=0
    timeout -k 5 "$per" apt-get "${opts[@]}" update >> "$log" 2>&1 || status=$?
    if [ "$status" -eq 0 ]; then
      left=$((budget - (SECONDS - started)))
      if [ "$left" -le 0 ]; then
        status=124
      else
        per=$(( left < per ? left : per ))
        timeout -k 5 "$per" apt-get "${opts[@]}" -y --no-install-recommends install --download-only "${pins[@]}" \
          >> "$log" 2>&1 || status=$?
      fi
    fi
    if [ "$status" -eq 0 ]; then
      break
    fi
    failed=$(hosts_of "$log" '^(Err:|[EW]: Failed to fetch)')
    if [ -z "$failed" ] && [ "$status" -ne 124 ] && [ "$status" -ne 137 ] &&
        grep -Eq "^E: (Version '[^']+' for '[^']+' was not found|Unable to locate package )" "$log"; then
      sed 's/^/  /' "$log" >&2
      reached=$(hosts_of "$log" '^(Get|Hit|Ign):')
      local missing
      missing=$(grep -Eo "^E: (Version '[^']+' for '[^']+'|Unable to locate package [^ ]+)" "$log" | sed -E "s/^E: //" | paste -sd';' -)
      rm -rf "$work"
      fail "no index the runner's apt reads (${reached:-no source answered}) holds $missing:" \
        "Ubuntu's archive keeps only the newest version of a pocket, so move that pin in the list to a version" \
        "\`apt-cache policy NAME\` shows on the runner"
    fi
    sed 's/^/  /' "$log" >&2
    if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then
      reached=$(hosts_of "$log" '^(Get|Hit|Ign|Err):')
      echo "apt-cache: attempt $n for ${pins[*]} did not finish within ${per}s; it had reached ${failed:-${reached:-no host}}" >&2
    else
      echo "apt-cache: attempt $n for ${pins[*]} failed (apt-get exit $status): could not reach ${failed:-a host it does not name}" >&2
    fi
    if [ "$n" -ge "$attempts" ]; then
      rm -rf "$work"
      fail "could not fetch ${pins[*]} after $n attempts in $((SECONDS - started))s: apt-get could not reach ${failed:-${reached:-a host it does not name}}"
    fi
    left=$((budget - (SECONDS - started)))
    if [ "$wait" -ge "$left" ]; then
      rm -rf "$work"
      fail "could not fetch ${pins[*]} after $n attempts, and the ${budget}s budget leaves no room for another: apt-get could not reach ${failed:-${reached:-a host it does not name}}"
    fi
    echo "apt-cache: trying again in ${wait}s" >&2
    sleep "$wait"
    n=$((n + 1))
    wait=$((wait * 2))
  done

  rm -rf "$dest"
  mkdir -p "$dest/debs"
  local deb count=0
  for deb in "$work/archives"/*.deb; do
    [ -e "$deb" ] || continue
    mv "$deb" "$dest/debs/"
    count=$((count + 1))
  done
  rm -rf "$work"
  [ "$count" -gt 0 ] || fail "apt fetched no .deb file for ${pins[*]}"
  write_index "$dest" "${pins[@]}"
  echo "apt-cache: fetched $count .deb files for ${pins[*]} into $dest" >&2
}

# write_index DIR PIN...: DIR's Packages, Release, SHA256SUMS and PINS, from the files in DIR/debs.
write_index() {
  local dir=$1 deb
  shift
  (
    cd "$dir"
    : > Packages
    for deb in debs/*.deb; do
      dpkg-deb -f "$deb" >> Packages
      printf 'Filename: %s\nSize: %s\nSHA256: %s\n\n' "$deb" "$(stat -c %s "$deb")" \
        "$(sha256sum "$deb" | cut -d' ' -f1)" >> Packages
    done
    # A Release that lists Packages alone: without one apt asks the directory for each compressed
    # form of the index first, and names each one it does not find as an error. The Date is the
    # fill's, in the form apt reads.
    printf 'Date: %s\nSHA256:\n %s %s Packages\n' "$(LC_ALL=C date -u '+%a, %d %b %Y %H:%M:%S UTC')" \
      "$(sha256sum Packages | cut -d' ' -f1)" "$(stat -c %s Packages)" > Release
    sha256sum Packages Release debs/*.deb > SHA256SUMS
  )
  { echo "$LAYOUT"; printf '%s\n' "$@" | sort; } > "$dir/PINS"
}

# holds DIR PIN...: true when DIR is an entry of this layout that holds every PIN.
holds() {
  local dir=$1 pin
  shift
  [ -f "$dir/PINS" ] && [ -f "$dir/Packages" ] && [ -f "$dir/Release" ] && [ -f "$dir/SHA256SUMS" ] || return 1
  [ "$(head -n 1 "$dir/PINS")" = "$LAYOUT" ] || return 1
  for pin in "$@"; do
    grep -qxF -- "$pin" "$dir/PINS" || return 1
  done
}

# verify DIR: every file of SHA256SUMS matches, and every .deb of DIR/debs is in it; exit 1 naming
# each file that does not.
verify() {
  local dir=$1 out deb line unsummed=()
  local -A summed=()
  if ! out=$(cd "$dir" && sha256sum --check --strict --quiet SHA256SUMS 2>&1); then
    echo "$out" | sed 's/^/  /' >&2
    fail "the cached set in $dir does not match its checksums: $(echo "$out" | grep -Eo '^[^:]+: FAILED.*' | sed 's/: FAILED.*//' | paste -sd' ' -)." \
      "A cache entry is never rewritten, so this is a file changed after the fill; read it before trusting the entry"
  fi
  while IFS= read -r line; do
    summed[${line#*  }]=1
  done < "$dir/SHA256SUMS"
  for deb in "$dir"/debs/*.deb; do
    [ -e "$deb" ] || continue
    [ -n "${summed[debs/${deb##*/}]+set}" ] || unsummed+=("debs/${deb##*/}")
  done
  [ "${#unsummed[@]}" -eq 0 ] || fail "the cached set in $dir holds files SHA256SUMS does not list: ${unsummed[*]}"
}

# install_from DIR PIN...: apt-get install of each PIN with DIR as the one source; returns apt's
# status, its output on stderr.
install_from() {
  local dir=$1 work status=0
  shift
  work=$(mktemp -d)
  mkdir -p "$work/parts" "$work/lists/partial" "$work/cache" "$work/archives/partial"
  printf 'deb [trusted=yes] file:%s ./\n' "$(cd "$dir" && pwd)" > "$work/sources.list"
  local opts=(
    -o "Dir::Etc::SourceList=$work/sources.list" -o "Dir::Etc::SourceParts=$work/parts"
    -o "Dir::State::Lists=$work/lists" -o "Dir::Cache=$work/cache" -o "Dir::Cache::archives=$work/archives"
    -o Acquire::Languages=none
  )
  if [ "$(id -u)" -eq 0 ]; then
    opts+=(-o APT::Sandbox::User=root)
  fi
  apt-get "${opts[@]}" -o Debug::NoLocking=1 update >&2 || status=$?
  if [ "$status" -eq 0 ]; then
    # DPkg::Lock::Timeout: wait for another dpkg the runner is running rather than fail at once.
    # --allow-downgrades and --no-remove: the header says why.
    as_root env DEBIAN_FRONTEND=noninteractive apt-get "${opts[@]}" -o DPkg::Lock::Timeout=60 \
      -y --no-install-recommends --allow-downgrades --no-remove install "$@" >&2 || status=$?
  fi
  rm -rf "$work"
  return "$status"
}

# installed_at NAME VERSION: true when dpkg has NAME installed at VERSION.
installed_at() {
  [ "$(dpkg-query -W -f='${db:Status-Status} ${Version}' "$1" 2>/dev/null)" = "installed $2" ]
}

[ $# -ge 1 ] || usage
mode=$1
shift
case "$mode" in
  image)
    [ $# -eq 0 ] || usage
    # APT_CACHE_OS_RELEASE is for the test, which plants a release file of its own.
    release=${APT_CACHE_OS_RELEASE:-/etc/os-release}
    [ -r "$release" ] || fail "no $release, so the runner's release is unknown"
    # shellcheck disable=SC1090
    image=$(. "$release" && echo "${ID:-}-${VERSION_ID:-}")
    [[ "$image" =~ ^[a-z0-9]+-[0-9.]+$ ]] || fail "$release names no ID and VERSION_ID ('$image')"
    echo "image=$image"
    ;;
  fill)
    [ $# -eq 2 ] || usage
    read_list "$1"
    dir=$2
    pins=()
    for name in "${NAMES[@]}"; do
      pins+=("$name=${PIN[$name]}")
    done
    fetch "$dir" "${pins[@]}"
    verify "$dir"
    ;;
  install)
    [ $# -ge 3 ] || usage
    read_list "$1"
    dir=$2
    shift 2
    pins=()
    for name in "$@"; do
      [ -n "${PIN[$name]+set}" ] || { echo "apt-cache: $name is not in the list; add a pin for it (name=version)" >&2; exit 2; }
      pins+=("$name=${PIN[$name]}")
    done
    from=$dir
    fetched=
    if holds "$dir" "${pins[@]}"; then
      verify "$dir"
      echo "apt-cache: installing ${pins[*]} from the cached set in $dir, with no network" >&2
    else
      echo "::warning::apt-cache: the restored cache holds no set of ${pins[*]}, so this step fetches it from the runner's mirrors" >&2
      fetched=$(mktemp -d)
      from=$fetched/apt
      fetch "$from" "${pins[@]}"
      verify "$from"
    fi
    if ! install_from "$from" "${pins[@]}"; then
      if [ -n "$fetched" ]; then
        fail "apt-get would not install ${pins[*]} from the set this step fetched (its output is above)"
      fi
      # A set the fill made on another build of the release, which this runner cannot take: fetch
      # the closure this runner's mirrors serve today and install that instead, once.
      echo "::warning::apt-cache: apt-get would not install ${pins[*]} from the cached set in $dir (its output is above), so this step fetches it from the runner's mirrors" >&2
      fetched=$(mktemp -d)
      from=$fetched/apt
      fetch "$from" "${pins[@]}"
      verify "$from"
      install_from "$from" "${pins[@]}" || fail "apt-get would not install ${pins[*]} from the set this step fetched (its output is above)"
    fi
    [ -z "$fetched" ] || rm -rf "$fetched"
    for name in "$@"; do
      installed_at "$name" "${PIN[$name]}" ||
        fail "$name is installed at '$(dpkg-query -W -f='${Version}' "$name" 2>/dev/null || true)', not at its pin ${PIN[$name]}"
    done
    echo "apt-cache: ${pins[*]} installed"
    ;;
  *)
    usage
    ;;
esac
