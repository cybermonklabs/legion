#!/usr/bin/env bash
# states-read-check.sh: the model check that the reviewer and the architect each decide by the
# state of each check at their head, as `dispatch results` prints it, and by no verdict of the
# tester. It runs the whole check from the checkout it is in, with that checkout's own rig.
#
#   SKILL_SCENARIOS_LABEL_PATCHES=<dir> bash states-read-check.sh
#   bash states-read-check.sh judge <reviewer-own-head score> <architect-pass-and-fail score>
#
# The first form does these steps in order, each with this checkout's own tools, and stops at the
# first step that fails:
#   1. label-checkouts.sh <this checkout> <its HEAD> <dir>/*.patch: the label checkouts, cut from
#      the committed HEAD. <dir> holds one <label>.patch for each label, ablate-states-read among
#      them: the labels are the patches' names. label-checkouts.sh refuses a patch that does not
#      apply at HEAD or changes nothing there. So in a checkout whose texts do not hold what the
#      ablate-states-read patch cuts, the check stops here, before the rig starts a service or a
#      model;
#   2. rig.sh profile for the two labels the check compares, head and ablate-states-read;
#   3. rig.sh batch reviewer-own-head 5 head ablate-states-read and rig.sh score reviewer-own-head,
#      then the same for architect-pass-and-fail. A batch's exit is printed; the score decides;
#   4. the judgement of the two scores, which the second form also gives for scores saved before.
# The check passes (exit 0) only when the judgement passes.
#
# The judgement reads the table of counts that each score ends with: for each row, its pass column
# (passed/scored) and its rig errors (runs the rig could not score, which it leaves out of both
# counts). It judges a row only when the row is complete: all five runs of the batch scored, and no
# rig error. A row the score does not hold, or one that is not complete, reads UNKNOWN, since a rate
# over fewer runs measures less than the check asks: one scored run beside four rig errors is no
# control. The judgement passes when each of these rows is complete and holds:
#   - head 5/5 on <scenario>:no-verdict-fail, for reviewer-own-head (the reviewer requests changes
#     and names the check) and for architect-pass-and-fail (the architect sends the issue back and
#     names the check). In that case the tester's fail is at the head, and the task names no verdict
#     of the tester;
#   - head 5/5 on <scenario>:no-verdict-pass, for each scenario (the reviewer approves; the
#     architect signs off). In that case a pass of each role is at the head;
#   - ablate-states-read below head on <scenario>:no-verdict-fail, for each scenario: the check's
#     negative control. That label cuts the sentences that send each role to the state line of its
#     head and writes "Tester's verdict: pass." into each task. The control is compared only when
#     both rows of the case are complete, so both count the same five runs.
# A complete row that does not hold reads FAIL, and decides the judgement even beside an UNKNOWN.
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
# exit: 0 the check passes; 1 a step failed, or a complete row does not hold; 2 a refused argument;
# 3 no row fails but a row is UNKNOWN, so the check cannot be judged (run its batch again)
set -euo pipefail

me=states-read-check
runs=5
usage="usage: SKILL_SCENARIOS_LABEL_PATCHES=<dir> bash $me.sh
       bash $me.sh judge <reviewer-own-head score> <architect-pass-and-fail score>"

refuse() {
  echo "$me: $*" >&2
  exit 2
}

# fail <step> <why>: the step's FAIL line, and the run's exit.
fail() {
  echo "FAIL $1: $2" >&2
  exit 1
}

# read_row <score> <row> <label>: "complete <passed>" when that row of the score's table of counts
# counts all $runs runs of the batch scored and no rig error, and otherwise "short <why>", why
# saying what the score holds for the row.
read_row() {
  local found pass errors scored
  read -r found pass errors <<<"$(awk -F'\t' -v row="$2" -v label="$3" '
    $0 == "scenario\tlabel\tpass\tpass+ref\trig errors\tflagged" { table = 1; next }
    table && $0 == "" { table = 0 }
    table && $1 == row && $2 == label { line = $3 " " $5; found++ }
    END { print found + 0, line }
  ' "$1")"
  if [ "$found" = 0 ]; then
    echo "short the score holds no such row"
  elif [ "$found" != 1 ]; then
    echo "short the score holds $found such rows"
  elif ! [[ $pass =~ ^[0-9]+/[0-9]+$ && $errors =~ ^[0-9]+$ ]]; then
    echo "short its pass column '$pass' and rig errors '$errors' are no counts"
  else
    scored=${pass#*/}
    if [ "$scored" = "$runs" ] && [ "$errors" = 0 ] && [ "${pass%/*}" -le "$runs" ]; then
      echo "complete ${pass%/*}"
    else
      echo "short $pass with $errors rig error$([ "$errors" = 1 ] || echo s); the judgement needs all $runs runs of the batch scored and no rig error"
    fi
  fi
}

# judge <reviewer-own-head score> <architect-pass-and-fail score>: the judgement of the header.
# Each row it reads prints one line, ok, FAIL or UNKNOWN; it returns 0, 1 or 3 as the header says.
judge() {
  local scenarios=(reviewer-own-head architect-pass-and-fail) scores=("$1" "$2") failed=0 unknown=0
  local i scenario score row state head_state ablate_state head_passed ablate_passed
  for i in 0 1; do
    scenario=${scenarios[i]} score=${scores[i]}
    for row in "$scenario:no-verdict-fail" "$scenario:no-verdict-pass"; do
      state=$(read_row "$score" "$row" head)
      case $state in
      complete\ *)
        head_passed=${state#complete }
        if [ "$head_passed" = "$runs" ]; then
          echo "ok $row head $runs/$runs"
        else
          echo "FAIL $row head: $head_passed/$runs, want $runs/$runs" >&2
          failed=1
        fi
        ;;
      *)
        echo "UNKNOWN $row head: ${state#short }" >&2
        unknown=1
        ;;
      esac
    done
    row=$scenario:no-verdict-fail
    head_state=$(read_row "$score" "$row" head)
    ablate_state=$(read_row "$score" "$row" ablate-states-read)
    case $ablate_state in
    short\ *)
      echo "UNKNOWN $row ablate-states-read: ${ablate_state#short }" >&2
      unknown=1
      ;;
    *)
      ablate_passed=${ablate_state#complete }
      case $head_state in
      short\ *)
        echo "UNKNOWN $row ablate-states-read: $ablate_passed/$runs, but head's row is not complete, so the control has no count to be below" >&2
        unknown=1
        ;;
      *)
        head_passed=${head_state#complete }
        if ((ablate_passed < head_passed)); then
          echo "ok $row ablate-states-read $ablate_passed/$runs, below head's $head_passed/$runs"
        else
          echo "FAIL $row ablate-states-read: $ablate_passed/$runs, not below head's $head_passed/$runs, so the label does not show the rule decides" >&2
          failed=1
        fi
        ;;
      esac
      ;;
    esac
  done
  if ((failed)); then
    echo "FAIL judgement: a row above does not hold" >&2
    return 1
  fi
  if ((unknown)); then
    echo "UNKNOWN judgement: a row above is not complete, so the check cannot be judged; run its batch again" >&2
    return 3
  fi
  echo "ok judgement"
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
[ -n "$patches_dir" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES is unset: name the directory of the label patches, one <label>.patch for each label, ablate-states-read among them"
[ -d "$patches_dir" ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES ($patches_dir) is no directory"
shopt -s nullglob
patches=("$patches_dir"/*.patch)
shopt -u nullglob
[ ${#patches[@]} -gt 0 ] || refuse "SKILL_SCENARIOS_LABEL_PATCHES ($patches_dir) holds no <label>.patch"

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
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
ablate_dir=$(awk -F'\t' '$1 == "ablate-states-read" { print $2 }' "$tsv")
[ -n "$ablate_dir" ] && [ -d "$ablate_dir" ] ||
  fail "label checkouts" "$tsv names no checkout for ablate-states-read; no service or model started"
echo "ok label checkouts"

for label in head ablate-states-read; do
  echo "== profile $label"
  if [ "$label" = head ]; then dir=$head_dir; else dir=$ablate_dir; fi
  bash "$here/rig.sh" profile "$label" "$dir" || fail "profile $label" "rig.sh profile $label exited $?"
  echo "ok profile $label"
done

for scenario in reviewer-own-head architect-pass-and-fail; do
  echo "== batch $scenario"
  status=0
  bash "$here/rig.sh" batch "$scenario" "$runs" head ablate-states-read || status=$?
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
