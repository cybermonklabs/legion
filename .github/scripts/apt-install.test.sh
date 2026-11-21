#!/usr/bin/env bash
# Tests for `apt-install.sh`: no apt call for a package the runner image carries, and for one it
# lacks every apt call bounded inside the step's budget, retried by count, and a failure that names
# itself after the last attempt.
#
# Each case runs a command with stand-ins first on PATH: a `sudo` that runs its arguments, a
# `uname` that answers the architecture the case names, a `dpkg-query` that answers `installed`
# for the packages STANDIN_INSTALLED names and nothing for any other, and an `apt-get` that records
# every call and, told to, hangs on its first k calls - holding a child process, as apt holds its
# acquire methods, and ignoring TERM where the case says so - or refuses them with exit 100, then
# succeeds. Cases count those calls. Time enters only as the bound under test - a 44 s budget,
# which with no wait between attempts gives each call 44 / 4 - 10 = 1 s - and as the `timeout`
# that ends a command which never ends by itself.
#
# Run from anywhere: .github/scripts/apt-install.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
export STANDIN_DIR="$work" GITHUB_JOB=pi-envoy APT_RETRY_SECONDS=0

cat > "$work/bin/sudo" <<'SH'
#!/usr/bin/env bash
exec "$@"
SH
cat > "$work/bin/uname" <<'SH'
#!/usr/bin/env bash
echo "${STANDIN_ARCH:-x86_64}"
SH
cat > "$work/bin/dpkg-query" <<'SH'
#!/usr/bin/env bash
# dpkg-query -W -f=<format> <package>: the status and version of an installed package.
package=${!#}
for installed in ${STANDIN_INSTALLED:-}; do
  if [ "$installed" = "$package" ]; then
    echo "installed 9.9-standin"
    exit 0
  fi
done
echo "dpkg-query: no packages found matching $package" >&2
exit 1
SH
cat > "$work/bin/apt-get" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STANDIN_DIR/calls"
calls=$(wc -l < "$STANDIN_DIR/calls")
if [ "$calls" -le "${APT_HANG_FIRST:-0}" ]; then
  # 30 s: past the 10 s every case is killed at, so a hanging call never returns inside a case,
  # and short enough that a child a broken script leaves behind ends by itself. Its output goes
  # nowhere: a child left holding the case's stdout would hold run_case until it ended, and the
  # check for a child left behind would then find none.
  [ -z "${APT_IGNORE_TERM:-}" ] || trap '' TERM
  sleep 30 > /dev/null 2>&1 &
  echo "$!" >> "$STANDIN_DIR/children"
  wait
fi
if [ "$calls" -le "${APT_REFUSE_FIRST:-0}" ]; then
  echo "E: Failed to fetch http://azure.archive.ubuntu.com/ubuntu/dists/noble/InRelease" >&2
  exit 100
fi
if [[ " $* " == *" install "* ]]; then
  printf '%s\n' "$*" >> "$STANDIN_DIR/installs"
  if [ "$(wc -l < "$STANDIN_DIR/installs")" -le "${APT_REFUSE_INSTALLS:-0}" ]; then
    echo "E: Failed to fetch http://azure.archive.ubuntu.com/ubuntu/pool/main/z/zsh/zsh.deb" >&2
    exit 100
  fi
fi
echo "apt-get $* done"
SH
chmod +x "$work/bin/sudo" "$work/bin/uname" "$work/bin/dpkg-query" "$work/bin/apt-get"

# run_case [VAR=value...] -- <command>...: runs the command with the stand-ins under a `timeout` of
# CASE_SECONDS (10 by default), the stand-in for the job's own timeout; sets out, err, status and
# calls (one apt-get call a line, its arguments).
run_case() {
  local vars=()
  while [ "$1" != -- ]; do
    vars+=("$1")
    shift
  done
  shift
  rm -f "$work/calls" "$work/children" "$work/installs"
  set +e
  out=$(env PATH="$work/bin:$PATH" "${vars[@]}" timeout "${CASE_SECONDS:-10}" "$@" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
  calls=$(cat "$work/calls" 2> /dev/null || true)
}

call_count() { if [ -z "$1" ]; then echo 0; else wc -l <<<"$1" | tr -d ' '; fi; }

# children_alive: how many of the children the hanging calls started still run.
children_alive() {
  local alive=0 pid
  [ -e "$work/children" ] || { echo 0; return; }
  while read -r pid; do
    kill -0 "$pid" 2> /dev/null && alive=$((alive + 1))
  done < "$work/children"
  echo "$alive"
}

x64_options="-o Acquire::Retries=0 -o Acquire::http::Timeout=5 -o Acquire::https::Timeout=5"
script="$script_dir/apt-install.sh"
budget=(--budget 44)

echo "control: the step as it stood, against an apt-get whose first call hangs"
run_case APT_HANG_FIRST=99 -- bash -c 'sudo apt-get update && sudo apt-get install -y --no-install-recommends tmux'
check "it never ends by itself: the job's timeout kills it" "$(is "$status" 124)"
check "having made one call, which never returned" "$(is "$calls" "update")"
check "and named nothing" "$(is "$(contains "$err" '::error::')" false)"

echo "case: the runner image carries the package"
run_case STANDIN_INSTALLED=tmux -- "$script" "${budget[@]}" tmux
check "it answers" "$(is "$status" 0)"
check "having called no apt-get at all" "$(is "$calls" "")"
check "and says the image carries it, and which version" \
  "$(is "$out" "tmux 9.9-standin is on the runner image already: no apt call for it")"

echo "case: the runner image carries one package of two"
run_case STANDIN_INSTALLED=tmux -- "$script" "${budget[@]}" tmux postgresql-client
check "it installs" "$(is "$status" 0)"
check "the one it lacks, and only that one" "$(is "$calls" "$x64_options update
$x64_options install -y --no-install-recommends postgresql-client")"

echo "case: a package pinned to the version the image carries"
run_case STANDIN_INSTALLED=tmux -- "$script" "${budget[@]}" tmux=9.9-standin
check "it answers" "$(is "$status" 0)"
check "having called no apt-get at all" "$(is "$calls" "")"
check "and says the image carries it" \
  "$(is "$out" "tmux 9.9-standin is on the runner image already: no apt call for it")"

echo "case: a package pinned to a version the image does not carry"
run_case STANDIN_INSTALLED=tmux -- "$script" "${budget[@]}" tmux=3.4-1ubuntu0.2
check "it installs" "$(is "$status" 0)"
check "the pinned version, as written" "$(is "$calls" "$x64_options update
$x64_options install -y --no-install-recommends tmux=3.4-1ubuntu0.2")"

echo "case: a healthy mirror"
run_case -- "$script" "${budget[@]}" tmux
check "it installs" "$(is "$status" 0)"
check "with one update and one install, each carrying the x64 acquire options" \
  "$(is "$calls" "$x64_options update
$x64_options install -y --no-install-recommends tmux")"

echo "case: the first call hangs, and the mirror answers after it"
run_case APT_HANG_FIRST=1 -- "$script" "${budget[@]}" tmux
check "it installs" "$(is "$status" 0)"
check "on its second attempt: update, update, install" "$(is "$(call_count "$calls")" 3)"
check "it says the first attempt hung and was killed at its bound" \
  "$(contains "$err" '^attempt 1 of 2 to install tmux failed: apt-get update hung past its 1 s bound and was killed; trying again in 0 s$')"
check "the hung call's children went with it" "$(is "$(children_alive)" 0)"

echo "case: every call hangs"
run_case APT_HANG_FIRST=99 -- "$script" "${budget[@]}" tmux
check "it fails, by itself" "$(is "$status" 1)"
check "after two attempts, each one update that hung" "$(is "$calls" "$x64_options update
$x64_options update")"
# shellcheck disable=SC2016 # the backticks are the error line's own text, not an expansion
check "naming the job, the packages, the attempts, the bound and the last call" \
  "$(contains "$err" '^::error::job pi-envoy could not install tmux with apt-get: 2 attempts of `apt-get update` and `apt-get install`, each call bounded at 1 s within the step'"'"'s 44 s budget, all failed; the last: apt-get update hung past its 1 s bound and was killed$')"
check "no hung call's children are left" "$(is "$(children_alive)" 0)"

echo "case: every call hangs and ignores TERM, so only KILL ends it"
# The worst case the budget is divided for: each call runs to its 1 s bound, ignores TERM, and is
# killed 10 s later. Two updates are 2 x 11 s, inside the 44 s budget, and the case's own bound is
# that budget, so a script that spent more than its budget is killed here and fails the first check.
CASE_SECONDS=44 run_case APT_HANG_FIRST=99 APT_IGNORE_TERM=1 -- "$script" "${budget[@]}" tmux
check "it fails by itself, inside its budget" "$(is "$status" 1)"
check "after two attempts, each one update that was killed" "$(is "$(call_count "$calls")" 2)"
check "naming the kill" "$(contains "$err" "the last: apt-get update hung past its 1 s bound and was killed\$")"
check "no child that ignored TERM is left" "$(is "$(children_alive)" 0)"

echo "case: the budget is divided over two attempts of two calls and the wait between them"
# 300 s with a 1 s wait: (300 - 1) / 4 - 10 = 64 s a call. Every call is refused at once, so the
# case reads the bound from the error line without waiting for it.
run_case APT_REFUSE_FIRST=99 APT_RETRY_SECONDS=1 -- "$script" --budget 300 tmux
check "it fails after two refused attempts" "$(is "$status" 1)"
check "each call bounded at 64 s within the 300 s budget" \
  "$(contains "$err" "each call bounded at 64 s within the step's 300 s budget, all failed")"

echo "case: a budget that leaves no second for a call"
run_case -- "$script" --budget 43 tmux
check "it refuses" "$(is "$status" 2)"
check "and says why" "$(contains "$err" "^::error::apt-install.sh's budget of 43 s leaves no second for a call: 2 attempts of 2 calls, each with 10 s between TERM and KILL, and 0 s of waiting between attempts\$")"
check "having called no apt-get" "$(is "$calls" "")"

echo "case: no budget named"
run_case -- "$script" tmux
check "it refuses" "$(is "$status" 2)"
check "and says the budget comes first" "$(contains "$err" '^::error::apt-install.sh takes --budget <seconds> first')"
check "having called no apt-get" "$(is "$calls" "")"

echo "case: every call is refused"
run_case APT_REFUSE_FIRST=99 -- "$script" "${budget[@]}" tmux postgresql-client
check "it fails" "$(is "$status" 1)"
check "after two attempts, each a refused update" "$(is "$(call_count "$calls")" 2)"
check "naming every package and how the last call ended" \
  "$(contains "$err" '^::error::job pi-envoy could not install tmux postgresql-client with apt-get: 2 attempts .*; the last: apt-get update exited 100$')"

echo "case: the update is served and the install refused once, then served"
run_case APT_REFUSE_INSTALLS=1 -- "$script" "${budget[@]}" zsh
check "it installs, on its second attempt" "$(is "$status" 0)"
check "update, install (refused), update, install" "$(is "$(call_count "$calls")" 4)"
check "it says how the first attempt ended" \
  "$(contains "$err" '^attempt 1 of 2 to install zsh failed: apt-get install exited 100; trying again in 0 s$')"

echo "case: an arm64 runner keeps the image's own retries, which ports.ubuntu.com needs"
run_case STANDIN_ARCH=aarch64 -- "$script" "${budget[@]}" zsh
check "it installs" "$(is "$status" 0)"
check "passing apt no option of its own" \
  "$(is "$calls" "update
install -y --no-install-recommends zsh")"

echo "case: no package named"
run_case -- "$script" "${budget[@]}"
check "it refuses" "$(is "$status" 2)"
check "and says what is missing" "$(contains "$err" '^::error::apt-install.sh needs the packages to install$')"
check "having called no apt-get" "$(is "$calls" "")"

summary "the bounded apt install of the CI steps that need a package"
