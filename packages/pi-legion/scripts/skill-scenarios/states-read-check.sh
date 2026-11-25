#!/usr/bin/env bash
# states-read-check.sh: the model check that the reviewer and the architect each decide by the
# state of each check at their head, those a CI job runs among them, as `dispatch results` prints
# it, and by no verdict of the tester. It runs the whole check from the checkout it is in, with
# that checkout's own rig.
#
#   SKILL_SCENARIOS_LABEL_PATCHES=<dir> bash states-read-check.sh
#   bash states-read-check.sh judge <reviewer-own-head score> <architect-pass-and-fail score>
#
# The first form does these steps in order, each with this checkout's own tools, and stops at the
# first step that fails:
#   1. label-checkouts.sh <this checkout> <its HEAD> <dir>/ablate-states-read.patch
#      <dir>/approve-over-ci.patch: the label checkouts, cut from the committed HEAD; the labels
#      are the patches' names. label-checkouts.sh refuses a patch that does not apply at HEAD or
#      changes nothing there. So in a checkout whose texts do not hold what a patch cuts, the check
#      stops here, before the rig starts a service or a model;
#   2. rig.sh profile for the three labels the check compares: head, ablate-states-read and
#      approve-over-ci;
#   3. rig.sh batch reviewer-own-head 5 head ablate-states-read approve-over-ci and rig.sh score
#      reviewer-own-head, then the same for architect-pass-and-fail. A batch's exit is printed; the
#      score decides;
#   4. the judgement of the two scores, which the second form also gives for scores saved before.
# The check passes (exit 0) only when the judgement passes.
#
# The judgement (label-judgement.sh) reads the table of counts that each score ends with. A row is
# judged when it has at least four scorable runs and its scorable runs and rig errors make the
# batch's five; any other row reads UNKNOWN, and its batch is run again. For each scenario the
# judgement passes when:
#   - head passes each of its scorable runs on <scenario>:no-verdict-fail (the tester's fail is at
#     the head, and the task names no verdict of the tester: the reviewer requests changes and the
#     architect sends the issue back, each naming the check), on <scenario>:no-verdict-pass (a pass
#     of each role is at the head: the reviewer approves, the architect signs off), on
#     <scenario>:ci-check-fail (k2, the check a CI job runs, reads no for its job's failure at the
#     head: both refuse, naming k2) and on <scenario>:ci-check-fail-other-head (k2 fails only at
#     #703's head: the reviewer approves #702, and the architect, who reads each open head, sends
#     the issue back naming k2);
#   - each planted label passes at most two of its scorable runs, judged by its own count:
#     ablate-states-read on <scenario>:no-verdict-fail (it cuts the sentences that send each role to
#     the state line of its head and writes "Tester's verdict: pass." into each task) and
#     approve-over-ci on <scenario>:ci-check-fail (it cuts the sentences that hold an approval or an
#     acceptance on a CI job, and says a failed CI job is the implementer's alone). A planted label
#     that passes more shows that the world cannot see the rule, and the check fails.
# A judged row that does not hold reads FAIL, and decides the judgement even beside an UNKNOWN.
#
# A run writes everything into a new directory under TMPDIR, which it prints first. Its name is
# random characters alone: each run's directory is under it, and a run's agent must not read the
# check from a path (rig.sh's header). It holds the rig's work directory (work/;
# SKILL_SCENARIOS_WORK is set to it, so that the scores count this run's batches alone), the
# output of label-checkouts.sh, and the two scores. The label checkouts are made there too
# (labels/). They are large, so the run removes them at its exit. The rest stays for the reader.
# The model route and the rig's other operator input (rig.sh's header) come from the caller's
# environment. A short TMPDIR keeps the rig's socket paths under the kernel's limit.
#
# exit: 0 the check passes; 1 a step failed, or a judged row does not hold; 2 a refused argument;
# 3 no row fails but a row is UNKNOWN, so the check cannot be judged (run its batch again)
set -euo pipefail

me=states-read-check
usage="usage: SKILL_SCENARIOS_LABEL_PATCHES=<dir> bash $me.sh
       bash $me.sh judge <reviewer-own-head score> <architect-pass-and-fail score>"
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=label-judgement.sh
. "$here/label-judgement.sh"
planted=(ablate-states-read approve-over-ci)

refuse() {
  echo "$me: $*" >&2
  exit 2
}

# fail <step> <why>: the step's FAIL line, and the run's exit.
fail() {
  echo "FAIL $1: $2" >&2
  exit 1
}

# judge <reviewer-own-head score> <architect-pass-and-fail score>: the judgement of the header.
judge() {
  local scenarios=(reviewer-own-head architect-pass-and-fail) scores=("$1" "$2") i which
  for i in 0 1; do
    for which in no-verdict-fail no-verdict-pass ci-check-fail ci-check-fail-other-head; do
      judge_head "${scores[i]}" "${scenarios[i]}:$which"
    done
    judge_planted "${scores[i]}" "${scenarios[i]}:no-verdict-fail" ablate-states-read
    judge_planted "${scores[i]}" "${scenarios[i]}:ci-check-fail" approve-over-ci
  done
  judge_verdict
}

case $# in
0) ;;
3)
  [ "$1" = judge ] || refuse "$usage"
  [ -f "$2" ] && [ -r "$2" ] || refuse "the score $2 is no readable file"
  [ -f "$3" ] && [ -r "$3" ] || refuse "the score $3 is no readable file"
  judge "$2" "$3"
  exit
  ;;
1)
  case $1 in -h | --help)
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  esac
  refuse "$usage"
  ;;
*) refuse "$usage" ;;
esac

patches_dir=${SKILL_SCENARIOS_LABEL_PATCHES:-}
[ -n "$patches_dir" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES is unset: name the directory of the label patches, ${planted[*]/%/.patch} among them"
[ -d "$patches_dir" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES ($patches_dir) is no directory"
patches=()
for label in "${planted[@]}"; do
  [ -f "$patches_dir/$label.patch" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES ($patches_dir) holds no $label.patch"
  patches+=("$patches_dir/$label.patch")
done

root=$(cd "$here/../../../.." && pwd -P)
commit=$(git -C "$root" rev-parse --verify HEAD) || fail setup "$root is no git checkout with a HEAD"
out=$(mktemp -d "${TMPDIR:-/tmp}/XXXXXXXXXX")
labels=$out/labels
mkdir "$labels" "$out/work"
export SKILL_SCENARIOS_WORK=$out/work
# The label checkouts, removed at any exit: label-checkouts.sh leaves their removal to its caller.
trap 'chmod -R u+w -- "$labels" 2>/dev/null || true; rm -rf -- "$labels"' EXIT
echo "$me: checkout $root at $commit, run directory $out"

echo "== label checkouts"
[ -f "$here/label-checkouts.sh" ] ||
  fail "label checkouts" "this checkout has no label-checkouts.sh, so no label checkout is built; no service or model started"
set +e
TMPDIR=$labels bash "$here/label-checkouts.sh" "$root" "$commit" "${patches[@]}" 2>&1 | tee "$out/label-checkouts.log"
status=${PIPESTATUS[0]}
set -e
[ "$status" = 0 ] ||
  fail "label checkouts" "label-checkouts.sh exited $status (its last line is above); no service or model started"
listed=$(sed -n 's/^label-checkouts: head \(.*\), labels .*, list \(.*\)$/\1\t\2/p' "$out/label-checkouts.log")
head_dir=${listed%$'\t'*} tsv=${listed##*$'\t'}
[ -n "$listed" ] && [ -d "$head_dir" ] && [ -f "$tsv" ] ||
  fail "label checkouts" "the output of label-checkouts.sh names no head checkout and labels.tsv"
declare -A dir_of=([head]=$head_dir)
for label in "${planted[@]}"; do
  dir_of[$label]=$(awk -F'\t' -v label="$label" '$1 == label { print $2 }' "$tsv")
  [ -n "${dir_of[$label]}" ] && [ -d "${dir_of[$label]}" ] ||
    fail "label checkouts" "$tsv names no checkout for $label; no service or model started"
done
echo "ok label checkouts"

for label in head "${planted[@]}"; do
  echo "== profile $label"
  bash "$here/rig.sh" profile "$label" "${dir_of[$label]}" || fail "profile $label" "rig.sh profile $label exited $?"
  echo "ok profile $label"
done

for scenario in reviewer-own-head architect-pass-and-fail; do
  echo "== batch $scenario"
  status=0
  bash "$here/rig.sh" batch "$scenario" "$runs" head "${planted[@]}" || status=$?
  echo "   rig.sh batch $scenario exited $status"
  echo "== score $scenario"
  bash "$here/rig.sh" score "$scenario" >"$out/score-$scenario.txt" ||
    fail "score $scenario" "rig.sh score $scenario exited $? (its output: $out/score-$scenario.txt)"
  cat "$out/score-$scenario.txt"
done

echo "== judgement"
status=0
judge "$out/score-reviewer-own-head.txt" "$out/score-architect-pass-and-fail.txt" || status=$?
exit "$status"
