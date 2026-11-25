#!/usr/bin/env bash
# tester-ci-check.sh: the model check that the tester reads no CI: in a world whose CI job is late
# or red at the head it tests, a tester runs and records the check that no CI job runs, and neither
# reads CI nor waits for it. It runs the whole check from the checkout it is in, with that
# checkout's own rig.
#
#   SKILL_SCENARIOS_LABEL_PATCHES=<dir> bash tester-ci-check.sh
#   bash tester-ci-check.sh judge <tester-reads-no-ci score>
#
# The first form does these steps in order, each with this checkout's own tools, and stops at the
# first step that fails:
#   1. label-checkouts.sh <this checkout> <its HEAD> <dir>/restore-tester-ci.patch: the label
#      checkout, cut from the committed HEAD. label-checkouts.sh refuses a patch that does not apply
#      at HEAD or changes nothing there, so the check stops here, before the rig starts a service
#      or a model. The patch puts back the sentence that sent the tester to read CI for everything
#      static;
#   2. rig.sh profile for head and restore-tester-ci;
#   3. rig.sh batch tester-reads-no-ci 5 head restore-tester-ci and rig.sh score tester-reads-no-ci.
#      The batch's exit is printed; the score decides;
#   4. the judgement of the score, which the second form also gives for a score saved before.
# The check passes (exit 0) only when the judgement passes.
#
# The judgement (label-judgement.sh) reads the table of counts the score ends with. A row is judged
# when it has at least four scorable runs and its scorable runs and rig errors make the batch's
# five; any other row reads UNKNOWN, and its batch is run again. It passes when head passes each of
# its scorable runs on tester-reads-no-ci:ci-late (k3's job is still going at the head) and on
# tester-reads-no-ci:ci-red (k3's job failed there), and the planted label restore-tester-ci passes
# at most two of its scorable runs on tester-reads-no-ci:ci-late, judged by its own count: more
# shows that the world cannot see the rule, and the check fails. A judged row that does not hold
# reads FAIL, and decides the judgement even beside an UNKNOWN.
#
# A run writes everything into a new directory under TMPDIR, which it prints first, named as
# states-read-check.sh names its own, with the rig's work directory (work/), the output of
# label-checkouts.sh and the score; the label checkouts (labels/) are removed at its exit. The model
# route and the rig's other operator input (rig.sh's header) come from the caller's environment.
#
# exit: 0 the check passes; 1 a step failed, or a judged row does not hold; 2 a refused argument;
# 3 no row fails but a row is UNKNOWN, so the check cannot be judged (run its batch again)
set -euo pipefail

me=tester-ci-check
usage="usage: SKILL_SCENARIOS_LABEL_PATCHES=<dir> bash $me.sh
       bash $me.sh judge <tester-reads-no-ci score>"
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=label-judgement.sh
. "$here/label-judgement.sh"
planted=restore-tester-ci

refuse() {
  echo "$me: $*" >&2
  exit 2
}

# fail <step> <why>: the step's FAIL line, and the run's exit.
fail() {
  echo "FAIL $1: $2" >&2
  exit 1
}

# judge <tester-reads-no-ci score>: the judgement of the header.
judge() {
  judge_head "$1" tester-reads-no-ci:ci-late
  judge_head "$1" tester-reads-no-ci:ci-red
  judge_planted "$1" tester-reads-no-ci:ci-late "$planted"
  judge_verdict
}

case $# in
0) ;;
2)
  [ "$1" = judge ] || refuse "$usage"
  [ -f "$2" ] && [ -r "$2" ] || refuse "the score $2 is no readable file"
  judge "$2"
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
[ -n "$patches_dir" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES is unset: name the directory of the label patches, $planted.patch among them"
[ -f "$patches_dir/$planted.patch" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES ($patches_dir) holds no $planted.patch"

root=$(cd "$here/../../../.." && pwd -P)
commit=$(git -C "$root" rev-parse --verify HEAD) || fail setup "$root is no git checkout with a HEAD"
out=$(mktemp -d "${TMPDIR:-/tmp}/XXXXXXXXXX")
labels=$out/labels
mkdir "$labels" "$out/work"
export SKILL_SCENARIOS_WORK=$out/work
# The label checkout, removed at any exit: label-checkouts.sh leaves its removal to its caller.
trap 'chmod -R u+w -- "$labels" 2>/dev/null || true; rm -rf -- "$labels"' EXIT
echo "$me: checkout $root at $commit, run directory $out"

echo "== label checkouts"
[ -f "$here/label-checkouts.sh" ] ||
  fail "label checkouts" "this checkout has no label-checkouts.sh, so no label checkout is built; no service or model started"
set +e
TMPDIR=$labels bash "$here/label-checkouts.sh" "$root" "$commit" "$patches_dir/$planted.patch" 2>&1 | tee "$out/label-checkouts.log"
status=${PIPESTATUS[0]}
set -e
[ "$status" = 0 ] ||
  fail "label checkouts" "label-checkouts.sh exited $status (its last line is above); no service or model started"
listed=$(sed -n 's/^label-checkouts: head \(.*\), labels .*, list \(.*\)$/\1\t\2/p' "$out/label-checkouts.log")
head_dir=${listed%$'\t'*} tsv=${listed##*$'\t'}
[ -n "$listed" ] && [ -d "$head_dir" ] && [ -f "$tsv" ] ||
  fail "label checkouts" "the output of label-checkouts.sh names no head checkout and labels.tsv"
planted_dir=$(awk -F'\t' -v label="$planted" '$1 == label { print $2 }' "$tsv")
[ -n "$planted_dir" ] && [ -d "$planted_dir" ] ||
  fail "label checkouts" "$tsv names no checkout for $planted; no service or model started"
echo "ok label checkouts"

for label in head "$planted"; do
  echo "== profile $label"
  if [ "$label" = head ]; then dir=$head_dir; else dir=$planted_dir; fi
  bash "$here/rig.sh" profile "$label" "$dir" || fail "profile $label" "rig.sh profile $label exited $?"
  echo "ok profile $label"
done

echo "== batch tester-reads-no-ci"
status=0
bash "$here/rig.sh" batch tester-reads-no-ci "$runs" head "$planted" || status=$?
echo "   rig.sh batch tester-reads-no-ci exited $status"
echo "== score tester-reads-no-ci"
bash "$here/rig.sh" score tester-reads-no-ci >"$out/score-tester-reads-no-ci.txt" ||
  fail "score tester-reads-no-ci" "rig.sh score tester-reads-no-ci exited $? (its output: $out/score-tester-reads-no-ci.txt)"
cat "$out/score-tester-reads-no-ci.txt"

echo "== judgement"
status=0
judge "$out/score-tester-reads-no-ci.txt" || status=$?
exit "$status"
