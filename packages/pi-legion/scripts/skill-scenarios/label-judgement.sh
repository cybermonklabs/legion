# shellcheck shell=bash
# label-judgement.sh: the judgement of the rows of a score's table of counts that a model check
# reads, sourced by states-read-check.sh and tester-ci-check.sh, so both judge at one floor and one
# bar. A row's pass column is passed/scored, and its rig errors are the runs the rig could not score,
# which it leaves out of both counts.
#
# The floor: a row is judged when it has at least $min_scorable scorable runs and its scorable runs
# and rig errors make the batch's $runs. Any other row reads UNKNOWN: a rate over fewer runs
# measures less than the check asks, and one scored run beside four rig errors is no control. Such
# a row's batch is run again.
#
# The bar: head passes each of its scorable runs (5/5, or 4/4 beside one rig error). A planted label
# (a patch that takes the rule out of the texts its role reads) passes at most $planted_max of its
# scorable runs, judged by its own count and never against head's: a planted label that passes more
# shows that the world cannot see the rule, and one pass fewer than head shows nothing.
#
# Each judge_* call prints one line, ok on stdout, FAIL or UNKNOWN on stderr, and sets
# judge_failed or judge_unknown; judge_verdict prints the judgement and returns 0, 1 (a judged row
# does not hold, whatever else is UNKNOWN) or 3 (no row fails, but a row is UNKNOWN).

runs=5
min_scorable=4
planted_max=2
judge_failed=0
judge_unknown=0

# read_row <score> <row> <label>: "judged <passed> <scored>" when the floor judges that row of the
# score's table of counts, and otherwise "short <why>", why saying what the score holds for it.
read_row() {
  local found pass errors passed scored
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
    passed=${pass%/*} scored=${pass#*/}
    if ((scored >= min_scorable && scored + errors == runs && passed <= scored)); then
      echo "judged $passed $scored"
    else
      echo "short $pass with $errors rig error$([ "$errors" = 1 ] || echo s); a row is judged with at least $min_scorable scorable runs that make the batch's $runs with its rig errors"
    fi
  fi
}

# judge_head <score> <row>: head's row of <row> passes each of its scorable runs.
judge_head() {
  local state passed scored
  state=$(read_row "$1" "$2" head)
  case $state in
  judged\ *)
    read -r _ passed scored <<<"$state"
    if [ "$passed" = "$scored" ]; then
      echo "ok $2 head $passed/$scored"
    else
      echo "FAIL $2 head: $passed/$scored, want $scored/$scored" >&2
      judge_failed=1
    fi
    ;;
  *)
    echo "UNKNOWN $2 head: ${state#short }" >&2
    judge_unknown=1
    ;;
  esac
}

# judge_planted <score> <row> <label>: the planted label's row of <row> passes at most
# $planted_max of its scorable runs.
judge_planted() {
  local state passed scored
  state=$(read_row "$1" "$2" "$3")
  case $state in
  judged\ *)
    read -r _ passed scored <<<"$state"
    if ((passed <= planted_max)); then
      echo "ok $2 $3 $passed/$scored, at most $planted_max"
    else
      echo "FAIL $2 $3: $passed/$scored, more than $planted_max, so the world cannot see the rule" >&2
      judge_failed=1
    fi
    ;;
  *)
    echo "UNKNOWN $2 $3: ${state#short }" >&2
    judge_unknown=1
    ;;
  esac
}

# judge_verdict: the judgement of every row judged above.
judge_verdict() {
  if ((judge_failed)); then
    echo "FAIL judgement: a row above does not hold" >&2
    return 1
  fi
  if ((judge_unknown)); then
    echo "UNKNOWN judgement: a row above is not judged, so the check cannot be judged; run its batch again" >&2
    return 3
  fi
  echo "ok judgement"
}
