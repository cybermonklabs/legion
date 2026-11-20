#!/usr/bin/env bash
# Tests for `apt-cache.sh`: an install from a cached set, a miss that fetches, a miss whose every
# attempt fails, a set whose file does not match its checksum, and each list it refuses.
#
# Each case runs the real script as a subprocess under stand-ins first on PATH: an `apt-get` that
# answers `update`, a download (`install --download-only`) and an install from the one local source
# the script names (and refuses a call that names that source with no empty
# Dir::Etc::SourceParts, beside which real apt reads the runner's own sources.list.d, its
# mirrors), a `dpkg-deb` and a `dpkg-query` that read what that apt-get wrote, a `sudo` that
# records itself and runs its command, and an `id` that answers a uid other than root, so the
# script takes the path a runner's user takes. The stand-in mirror is a list of the pins it serves;
# APT_TEST_NET says whether it answers (`up`), fails as a cut network does (`down`: the resolver
# fails, apt-get update prints Err and W lines and exits 0, as apt 2.8 does), or hangs (`hang`).
# A fake .deb is its control text, which the stand-in dpkg-deb prints. No network, no root and no
# real apt are used.
#
# Run from anywhere: .github/scripts/apt-cache.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
apt_cache="$script_dir/apt-cache.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir -p "$work/bin"
cat > "$work/bin/apt-get" <<'STANDIN'
#!/usr/bin/env bash
echo "apt-get $*" >> "$APT_TEST_CALLS"
lists= archives= sourcelist= parts= download= local_dir=
args=()
while [ $# -gt 0 ]; do
  case $1 in
    -o)
      shift
      case $1 in
        Dir::State::Lists=*) lists=${1#*=} ;;
        Dir::Cache::archives=*) archives=${1#*=} ;;
        Dir::Etc::SourceList=*) sourcelist=${1#*=} ;;
        Dir::Etc::SourceParts=*) parts=${1#*=} ;;
      esac
      ;;
    --download-only) download=1 ;;
    -*) ;;
    *) args+=("$1") ;;
  esac
  shift
done
if [ -n "$sourcelist" ]; then
  local_dir=$(sed -n 's/^deb \[trusted=yes\] file:\(.*\) \.\/$/\1/p' "$sourcelist")
  [ -n "$local_dir" ] || { echo "the stand-in reads only a list of one local source" >&2; exit 3; }
  # Real apt reads sources.list.d (Dir::Etc::SourceParts) beside the list: the runner's mirrors.
  [ -n "$parts" ] || { echo "the stand-in reads only the one local source: no Dir::Etc::SourceParts, so apt reads the runner's sources.list.d too" >&2; exit 3; }
  [ -z "$(ls -A "$parts" 2>/dev/null)" ] || { echo "the stand-in reads only the one local source: $parts holds another" >&2; exit 3; }
fi
case ${args[0]} in
  update)
    if [ -n "$local_dir" ]; then
      echo "Get:1 file:$local_dir ./ Packages"
      exit 0
    fi
    case ${APT_TEST_NET:-up} in
      up)
        echo "Hit:1 http://mirror.example/ubuntu noble InRelease"
        touch "$lists/indexed"
        ;;
      down)
        echo "Ign:1 http://mirror.example/ubuntu noble InRelease"
        echo "Err:1 http://mirror.example/ubuntu noble InRelease"
        echo "  Temporary failure resolving 'mirror.example'"
        echo "W: Failed to fetch http://mirror.example/ubuntu/dists/noble/InRelease  Temporary failure resolving 'mirror.example'"
        echo "W: Some index files failed to download. They have been ignored, or old ones used instead."
        ;;
      hang)
        echo "Get:1 http://slow.example/ubuntu noble InRelease"
        exec sleep 60
        ;;
    esac
    exit 0
    ;;
  install)
    if [ -n "$download" ]; then
      [ -z "$local_dir" ] || { echo "the stand-in downloads only from the mirror" >&2; exit 3; }
      if [ ! -e "$lists/indexed" ]; then
        for pin in "${args[@]:1}"; do echo "E: Unable to locate package ${pin%%=*}"; done
        exit 100
      fi
      for pin in "${args[@]:1}"; do
        grep -qxF -- "$pin" "$APT_TEST_MIRROR" || { echo "E: Version '${pin#*=}' for '${pin%%=*}' was not found"; exit 100; }
        printf 'Package: %s\nVersion: %s\nArchitecture: amd64\nDepends: libfake\n' "${pin%%=*}" "${pin#*=}" \
          > "$archives/${pin%%=*}_${pin#*=}_amd64.deb"
      done
      printf 'Package: libfake\nVersion: 1.0\nArchitecture: amd64\n' > "$archives/libfake_1.0_amd64.deb"
      exit 0
    fi
    [ -n "$local_dir" ] || { echo "the stand-in installs only from a local source" >&2; exit 3; }
    if [ "$local_dir" = "${APT_TEST_REFUSE_DIR:-}" ]; then
      echo "E: Unmet dependencies. tmux : Depends: libevent-core-2.1-7t64 but it is not installable"
      exit 100
    fi
    for pin in "${args[@]:1}"; do
      awk -v want="$pin" 'BEGIN { RS = "" } { name = ""; version = ""
          n = split($0, lines, "\n")
          for (i = 1; i <= n; i++) {
            if (lines[i] ~ /^Package: /) name = substr(lines[i], 10)
            if (lines[i] ~ /^Version: /) version = substr(lines[i], 10)
          }
          if (name "=" version == want) found = 1 }
        END { exit !found }' "$local_dir/Packages" || { echo "E: Version '${pin#*=}' for '${pin%%=*}' was not found"; exit 100; }
      echo "${pin%%=*} ${APT_TEST_INSTALLS_AS:-${pin#*=}}" >> "$APT_TEST_STATUS"
      echo "Setting up ${pin%%=*} (${pin#*=}) ..."
    done
    exit 0
    ;;
esac
echo "the stand-in apt-get answers no ${args[0]}" >&2
exit 3
STANDIN
cat > "$work/bin/dpkg-deb" <<'STANDIN'
#!/usr/bin/env bash
[ "$1" = -f ] && [ $# -eq 2 ] || { echo "the stand-in dpkg-deb answers -f FILE alone" >&2; exit 3; }
cat "$2"
STANDIN
cat > "$work/bin/dpkg-query" <<'STANDIN'
#!/usr/bin/env bash
format=${2#-f=} name=$3
line=$(grep "^$name " "$APT_TEST_STATUS" 2>/dev/null | tail -n 1) || exit 1
[ -n "$line" ] || exit 1
case $format in
  *Status*) printf 'installed %s' "${line#* }" ;;
  *) printf '%s' "${line#* }" ;;
esac
STANDIN
cat > "$work/bin/sudo" <<'STANDIN'
#!/usr/bin/env bash
echo "sudo $*" >> "$APT_TEST_CALLS"
exec "$@"
STANDIN
cat > "$work/bin/id" <<'STANDIN'
#!/usr/bin/env bash
[ "$*" = -u ] && { echo 1001; exit 0; }
echo "the stand-in id answers -u alone" >&2
exit 3
STANDIN
chmod +x "$work/bin"/*

# The stand-in mirror's pins, and the list the cases install from.
printf '%s\n' tmux=3.4-1ubuntu0.2 zsh=5.9-6ubuntu2 > "$work/mirror"
printf '# CI'"'"'s packages\ntmux=3.4-1ubuntu0.2  # the daemon needs it\n\nzsh=5.9-6ubuntu2\n' > "$work/list"

# run <args...>: runs the script under the stand-ins with no waits between attempts. Leaves its
# stdout in `out`, its stderr in `err`, its exit code in `rc`, its run time in `took` (whole
# seconds), the stand-ins' calls in `calls` and what is installed in `installed`. APT_TEST_NET
# and the other stand-in settings are the caller's.
run() {
  rm -f "$work/calls" "$work/status"
  local started=$SECONDS
  set +e
  PATH="$work/bin:$PATH" APT_TEST_CALLS="$work/calls" APT_TEST_STATUS="$work/status" APT_TEST_MIRROR="$work/mirror" \
    APT_CACHE_PAUSE=0 "$apt_cache" "$@" > "$work/stdout" 2> "$work/stderr"
  rc=$?
  set -e
  took=$((SECONDS - started))
  out=$(cat "$work/stdout")
  err=$(cat "$work/stderr")
  calls=$(cat "$work/calls" 2>/dev/null || true)
  installed=$(cat "$work/status" 2>/dev/null || true)
}

# A call of the stand-in apt-get that read the mirror: an update or a download with no local
# source, or with one and the runner's sources.list.d beside it. A hit must make none.
mirror_calls() { grep -E '^apt-get ' <<<"$calls" | awk '!/Dir::Etc::SourceList=/ || !/Dir::Etc::SourceParts=/' || true; }

echo "case: a list line that is no pin is refused before apt runs"
printf 'zsh=5.9-6ubuntu2\ntmux\n' > "$work/list-bare"
run install "$work/list-bare" "$work/none" tmux
check "exits 2" "$(is "$rc" 2)"
check "names the file, the line and the text" "$(contains "$err" "list-bare:2: 'tmux' is not a pin, name=version")"
check "runs no apt-get" "$(is "$calls" "")"
printf 'tmux=latest\n' > "$work/list-word"
run install "$work/list-word" "$work/none" tmux
check "a version that is a word is no pin either" "$(contains "$err" "list-word:1: 'tmux=latest' is not a pin")"
check "and exits 2" "$(is "$rc" 2)"
printf 'tmux=3.4-1ubuntu0.2\ntmux=3.4-1build1\n' > "$work/list-twice"
run install "$work/list-twice" "$work/none" tmux
check "a package pinned twice is refused, naming its second line" "$(contains "$err" "list-twice:2: tmux is pinned twice")"
printf '# nothing\n' > "$work/list-empty"
run fill "$work/list-empty" "$work/none"
check "a list that pins nothing is refused" "$(contains "$err" "pins no package")"
check "and exits 2" "$(is "$rc" 2)"
run install "$work/list" "$work/none" jq
check "a package the list does not pin is refused, naming it" "$(contains "$err" "jq is not in the list; add a pin")"
check "and exits 2 with no apt-get run" "$(is "$rc:$calls" "2:")"
run install "$work/list"
check "an install that names no package is a usage error" "$(is "$rc" 2)"

echo "case: the fill fetches every pin's closure and writes the entry"
run fill "$work/list" "$work/cache"
check "exits 0" "$(is "$rc" 0)"
check "PINS is the layout line, then the pins sorted" "$(is "$(cat "$work/cache/PINS")" "$(printf 'apt-cache layout 1\ntmux=3.4-1ubuntu0.2\nzsh=5.9-6ubuntu2')")"
check "the .deb files of both pins and their dependency" \
  "$(is "$(cd "$work/cache/debs" && ls)" "$(printf 'libfake_1.0_amd64.deb\ntmux_3.4-1ubuntu0.2_amd64.deb\nzsh_5.9-6ubuntu2_amd64.deb')")"
check "Packages gives each its Filename, Size and SHA256" "$(contains "$(cat "$work/cache/Packages")" "^Filename: debs/tmux_3.4-1ubuntu0.2_amd64.deb$")"
check "SHA256SUMS checks Packages, Release and every .deb" "$( (cd "$work/cache" && sha256sum --check --strict --quiet SHA256SUMS) && is "$(wc -l < "$work/cache/SHA256SUMS")" 5)"
check "Release names Packages' own SHA-256" "$(contains "$(cat "$work/cache/Release")" " $(sha256sum "$work/cache/Packages" | cut -d' ' -f1) [0-9]* Packages$")"
check "reads the mirror with one update and one download of both pins" \
  "$(is "$(mirror_calls | sed -E 's/ -o [^ ]+//g')" "$(printf 'apt-get update\napt-get -y --no-install-recommends install --download-only tmux=3.4-1ubuntu0.2 zsh=5.9-6ubuntu2')")"
check "against an empty dpkg status, so the closure is whole" "$(contains "$calls" "Dir::State::status=.*/status ")"
check "and runs nothing as root" "$(is "$(grep -c '^sudo' <<<"$calls" || true)" 0)"

echo "case: a hit installs from the cached set, with the network cut"
APT_TEST_NET=down run install "$work/list" "$work/cache" tmux
check "exits 0" "$(is "$rc" 0)"
check "tmux is installed at its pin" "$(is "$installed" "tmux 3.4-1ubuntu0.2")"
check "says it installed from the cached set" "$(contains "$err" "installing tmux=3.4-1ubuntu0.2 from the cached set in $work/cache, with no network")"
check "reads no mirror" "$(is "$(mirror_calls)" "")"
check "the install is apt's, through sudo, with the entry as its one source" \
  "$(contains "$calls" "^sudo env DEBIAN_FRONTEND=noninteractive apt-get -o Dir::Etc::SourceList=[^ ]* -o Dir::Etc::SourceParts=")"
check "moving a package the image carries at another version to its pin, and removing nothing" \
  "$(contains "$calls" "^sudo env .* -y --no-install-recommends --allow-downgrades --no-remove install tmux=3.4-1ubuntu0.2$")"
check "prints the pins it installed" "$(is "$out" "apt-cache: tmux=3.4-1ubuntu0.2 installed")"

echo "case: a miss fetches the pin, then installs what it fetched"
APT_TEST_NET=up run install "$work/list" "$work/empty" zsh
check "exits 0" "$(is "$rc" 0)"
check "warns that the cache held no set of the pin" "$(contains "$err" "::warning::apt-cache: the restored cache holds no set of zsh=5.9-6ubuntu2")"
check "reads the mirror once, for that pin alone" \
  "$(is "$(mirror_calls | sed -E 's/ -o [^ ]+//g')" "$(printf 'apt-get update\napt-get -y --no-install-recommends install --download-only zsh=5.9-6ubuntu2')")"
check "zsh is installed at its pin" "$(is "$installed" "zsh 5.9-6ubuntu2")"

echo "case: an entry without the pin, or of another layout, is a miss"
mkdir -p "$work/partial"
cp -R "$work/cache/." "$work/partial/"
printf 'apt-cache layout 1\ntmux=3.4-1ubuntu0.2\n' > "$work/partial/PINS"
APT_TEST_NET=up run install "$work/list" "$work/partial" zsh
check "a restored entry that holds another pin set fetches" "$(contains "$err" "holds no set of zsh=5.9-6ubuntu2")"
check "and exits 0" "$(is "$rc" 0)"
printf 'apt-cache layout 0\ntmux=3.4-1ubuntu0.2\nzsh=5.9-6ubuntu2\n' > "$work/partial/PINS"
APT_TEST_NET=up run install "$work/list" "$work/partial" tmux
check "an entry of another layout fetches" "$(contains "$err" "holds no set of tmux=3.4-1ubuntu0.2")"

echo "case: a miss whose every attempt fails names the pin and the mirror, in seconds"
APT_TEST_NET=down APT_CACHE_ATTEMPTS=3 run install "$work/list" "$work/empty" tmux
check "exits 1" "$(is "$rc" 1)"
check "names the pin, the attempts and the host it could not reach" \
  "$(contains "$err" "::error::apt-cache: could not fetch tmux=3.4-1ubuntu0.2 after 3 attempts in [0-9]*s: apt-get could not reach mirror.example$")"
check "each attempt says so" "$(is "$(grep -c 'failed (apt-get exit 100): could not reach mirror.example' <<<"$err")" 3)"
check "installs nothing" "$(is "$installed" "")"
check "within seconds" "$( [ "$took" -lt 10 ] && echo true || echo false)"

echo "case: a pin no index holds fails at once, with no second attempt"
printf 'tmux=9.9\n' > "$work/list-gone"
APT_TEST_NET=up run install "$work/list-gone" "$work/empty" tmux
check "exits 1" "$(is "$rc" 1)"
check "names the pin and says why it moves" "$(contains "$err" "holds Version '9.9' for 'tmux':.*move that pin")"
check "names the host the update read" "$(contains "$err" "no index the runner's apt reads (mirror.example)")"
check "makes one attempt" "$(is "$(grep -c 'attempt [0-9] of' <<<"$err")" 1)"

echo "case: an attempt that hangs is ended by its bound, naming the host it had reached"
APT_TEST_NET=hang APT_CACHE_ATTEMPTS=2 APT_CACHE_ATTEMPT_SECONDS=1 run install "$work/list" "$work/empty" tmux
check "exits 1" "$(is "$rc" 1)"
check "says the attempt did not finish and names the host" "$(contains "$err" "attempt 1 for tmux=3.4-1ubuntu0.2 did not finish within 1s; it had reached slow.example")"
check "names the pin and the host in its error" "$(contains "$err" "::error::apt-cache: could not fetch tmux=3.4-1ubuntu0.2 after 2 attempts .*slow.example")"
check "in about the two attempts' bound" "$( [ "$took" -lt 15 ] && echo true || echo false)"
APT_TEST_NET=hang APT_CACHE_ATTEMPTS=3 APT_CACHE_ATTEMPT_SECONDS=5 APT_CACHE_BUDGET=1 run install "$work/list" "$work/empty" tmux
check "an attempt never runs past the budget" "$(contains "$err" "at most 1s")"
check "and the budget ends the fetch" "$(is "$rc" 1)"

echo "case: a file that does not match its checksum is named, and nothing is installed"
mkdir -p "$work/bad"
cp -R "$work/cache/." "$work/bad/"
echo "changed" >> "$work/bad/debs/tmux_3.4-1ubuntu0.2_amd64.deb"
APT_TEST_NET=down run install "$work/list" "$work/bad" tmux
check "exits 1" "$(is "$rc" 1)"
check "names the file" "$(contains "$err" "does not match its checksums: debs/tmux_3.4-1ubuntu0.2_amd64.deb\.")"
check "runs no install and reads no mirror" "$(is "$(grep -c 'install' <<<"$calls" || true)" 0)"
check "installs nothing" "$(is "$installed" "")"
mkdir -p "$work/extra"
cp -R "$work/cache/." "$work/extra/"
printf 'Package: stray\nVersion: 1\n' > "$work/extra/debs/stray_1_amd64.deb"
APT_TEST_NET=down run install "$work/list" "$work/extra" tmux
check "a .deb SHA256SUMS does not list is named" "$(contains "$err" "holds files SHA256SUMS does not list: debs/stray_1_amd64.deb")"
check "and exits 1" "$(is "$rc" 1)"

echo "case: a cached set apt refuses on this runner is fetched again, once"
APT_TEST_NET=up APT_TEST_REFUSE_DIR="$work/cache" run install "$work/list" "$work/cache" tmux
check "exits 0" "$(is "$rc" 0)"
check "warns that apt would not install from the cached set" "$(contains "$err" "::warning::apt-cache: apt-get would not install tmux=3.4-1ubuntu0.2 from the cached set")"
check "and installs what it fetched" "$(is "$installed" "tmux 3.4-1ubuntu0.2")"
APT_TEST_NET=down APT_TEST_REFUSE_DIR="$work/cache" APT_CACHE_ATTEMPTS=1 run install "$work/list" "$work/cache" tmux
check "when that fetch fails too, it names the mirror" "$(contains "$err" "could not fetch tmux=3.4-1ubuntu0.2 after 1 attempts .*mirror.example")"

echo "case: an install that leaves another version is refused"
APT_TEST_NET=down APT_TEST_INSTALLS_AS=3.3a run install "$work/list" "$work/cache" tmux
check "exits 1" "$(is "$rc" 1)"
check "names the package, what is installed and the pin" "$(contains "$err" "tmux is installed at '3.3a', not at its pin 3.4-1ubuntu0.2")"

echo "case: image is the release the os-release file names"
printf 'NAME="Ubuntu"\nID=ubuntu\nVERSION_ID="24.04"\n' > "$work/os-release"
APT_CACHE_OS_RELEASE="$work/os-release" run image
check "prints image=ubuntu-24.04" "$(is "$rc:$out" "0:image=ubuntu-24.04")"
printf 'NAME="Something"\n' > "$work/os-release"
APT_CACHE_OS_RELEASE="$work/os-release" run image
check "a file with no ID and VERSION_ID is refused" "$(is "$rc" 1)"
check "naming it" "$(contains "$err" "names no ID and VERSION_ID")"

summary "apt-cache.sh"
