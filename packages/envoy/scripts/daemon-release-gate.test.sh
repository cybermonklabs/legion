#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.test.sh
#
# Proves daemon-release-gate.sh's verdicts with no network: a fake `gh` answers the stack files, the
# deployment's head and archive, the release tags, the marketplace and the compare calls from files
# this test writes; a fake `docker` answers `buildx imagetools inspect` the way the real one answers
# a single image and an OCI image index (an index's `.Image.Config.Labels` is `null`, and its
# platforms are read one at a time) and `run` from an image's files; and a fake `npm` answers `npm
# pack --dry-run --json`, running the real npm for `npm exec` and where a case asks, so the suite
# needs npm and node. Each records every call, so a case asserts what the gate printed and what
# it asked. The deployment repository is the one `--deployment` names, and each stack's config keys
# carry the project name its own Pulumi.yaml declares; the fake gh answers that repository alone,
# so a gate that read any other would fail every row. Each run's HOME is the case's own, so no row
# reads the machine's dotfiles.
#
# Every scenario is in a group file, lib/daemon-release-gate-cases-<group>.sh, and this file runs
# the groups. Each group runs in a process of its own (lib/daemon-release-gate-group.sh), with its
# own temporary directory, its own fakes and its own case roots, so no group reads another's
# files; the gate itself does no waiting, so the groups' cost is the processes they start, and
# running them one at a time left three of a 4-vCPU runner's cores idle for the whole step. Up to
# one group per core runs at a time (GATE_TEST_GROUP_SLOTS names another number, 1 to read a
# group's output as it runs), longest group first. Each group's output is printed whole, in the
# reading order below, once every group has ended, with the seconds it took; the run fails when
# any group fails a check or ends any other way, and the count of failed checks is the sum.
set -euo pipefail

# Its developer-box rows hide a directory with `chmod 000` to show the gate's refusal of what it
# cannot read, and root reads it anyway, so those rows fail as root for no fault of the gate's
# (lib/daemon-release-gate-cases-developer-box.sh, "this user cannot search the hidden directory").
# Say so once, rather than as a dozen row failures.
if [ "$(id -u)" = 0 ]; then
  echo "daemon-release-gate.test.sh: run it as a user other than root: its rows hide a directory with chmod 000, which root reads anyway" >&2
  exit 1
fi

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly runner="${project_root}/packages/envoy/scripts/lib/daemon-release-gate-group.sh"

# The groups in reading order: the daemon leg, then the --listeners leg and the VM hub it reads,
# then --clients (the images' clients, the developer box and its versions), then a project's
# fields, which both legs read, then the reads that can come back short in every leg.
readonly groups=(daemons listeners vm-hub image-clients developer-box versions project-fields short-reads)
# The same groups, longest first, which is the order they are started in: a group that is started
# last cannot be the one the run waits for. The seconds are what each took in CI run 45054793501
# (job 137308591036), where the whole file took 122.6 s in one process.
readonly schedule=(developer-box image-clients short-reads versions vm-hub daemons listeners project-fields)
if [ "$(printf '%s\n' "${groups[@]}" | sort)" != "$(printf '%s\n' "${schedule[@]}" | sort)" ]; then
  echo "daemon-release-gate.test.sh: the reading order and the start order name different groups" >&2
  exit 1
fi

slots=${GATE_TEST_GROUP_SLOTS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)}
[[ $slots =~ ^[1-9][0-9]*$ ]] || {
  echo "daemon-release-gate.test.sh: GATE_TEST_GROUP_SLOTS is ${slots}, which is no count of groups to run at a time" >&2
  exit 2
}
[ "$slots" -le "${#groups[@]}" ] || slots=${#groups[@]}

outputs=$(mktemp -d)
readonly outputs
trap 'rm -rf "$outputs"' EXIT

# now_ms: the clock in milliseconds. A `date` with no %N, which is BSD's, gives whole seconds, so
# a group's time is read in seconds there rather than failing the arithmetic below.
now_ms() {
  local now
  now=$(date +%s%3N)
  [[ $now =~ ^[0-9]+$ ]] || now=$(($(date +%s) * 1000))
  printf '%s\n' "$now"
}

# run_group <group>: the group in a process of its own, its output, its tally, its exit and the
# milliseconds it took in files of its own, so the caller prints them in the reading order.
run_group() {
  local group=$1 started ended state=0
  started=$(now_ms)
  bash "$runner" "$group" "${outputs}/${group}.tally" \
    >"${outputs}/${group}.out" 2>"${outputs}/${group}.err" || state=$?
  ended=$(now_ms)
  printf '%s\n' "$state" >"${outputs}/${group}.state"
  printf '%s\n' "$((ended - started))" >"${outputs}/${group}.ms"
}

started=0
for group in "${schedule[@]}"; do
  if [ "$started" -ge "$slots" ]; then
    wait -n || true
    started=$((started - 1))
  fi
  run_group "$group" &
  started=$((started + 1))
done
wait

failures=0
cases=0
checks=0
for group in "${groups[@]}"; do
  cat "${outputs}/${group}.out"
  cat "${outputs}/${group}.err" >&2
  tally=$(cat "${outputs}/${group}.tally" 2>/dev/null || true)
  state=$(cat "${outputs}/${group}.state" 2>/dev/null || true)
  milliseconds=$(cat "${outputs}/${group}.ms" 2>/dev/null || echo 0)
  group_cases=$(grep -c '^case: ' "${outputs}/${group}.out" || true)
  group_checks=$(grep -cE '^(PASS|FAIL): ' "${outputs}/${group}.out" || true)
  cases=$((cases + group_cases))
  checks=$((checks + group_checks))
  if [[ ! $tally =~ ^(0|[1-9][0-9]*)$ ]]; then
    # A group that wrote no tally never reached its own last line: its checks are not all in.
    printf 'FAIL: the %s group wrote no count of its failed checks; it ended with exit %s\n' "$group" "${state:-none}"
    failures=$((failures + 1))
  else
    failures=$((failures + tally))
    expected=1
    [ "$tally" != 0 ] || expected=0
    if [ "$state" != "$expected" ]; then
      printf 'FAIL: the %s group ended with exit %s after %s failed check(s)\n' "$group" "${state:-none}" "$tally"
      failures=$((failures + 1))
    fi
  fi
  if [ "$group_checks" -eq 0 ]; then
    # A group whose file ran no check at all, which a group named in the lists and nowhere else
    # would do, is no group that passed.
    printf 'FAIL: the %s group ran no check\n' "$group"
    failures=$((failures + 1))
  fi
  printf 'group %s: %s case(s), %s check(s), %s failed, %s.%ss\n' \
    "$group" "$group_cases" "$group_checks" "${tally:-unknown}" "$((milliseconds / 1000))" "$(((milliseconds % 1000) / 100))"
done

printf '%s case(s) and %s check(s) in %s group(s)\n' "$cases" "$checks" "${#groups[@]}"
if [ "$failures" -ne 0 ]; then
  printf '%s check(s) failed\n' "$failures"
  exit 1
fi
echo "all checks passed"
