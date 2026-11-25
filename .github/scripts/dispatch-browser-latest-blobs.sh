#!/usr/bin/env bash
# The newest attempt of each leg's blob report, and no other, for the merge job of
# .github/workflows/dispatch-browser.yaml.
#
# A run's artifact list holds every attempt's artifacts, and the merge job used to download them
# with `merge-multiple: true`: a leg re-run by `gh run rerun --failed` then had two artifacts
# matching the download pattern (`dispatch-browser-blob-shard-1-1` and
# `dispatch-browser-blob-shard-1-2`), whose inner paths are the same file names
# (`blob-report/report-shard-1.zip`, `rows/shard-1.txt`), so the two attempts overwrote one
# another in an order nothing fixes. Driven with real Playwright 1.64.1 blobs, one copy order
# gave a merged report of {"expected":2,"unexpected":0,"flaky":0} and the other
# {"expected":0,"unexpected":1,"flaky":0}: a green re-run could be published red, or a red one
# green.
#
# So the merge job downloads without `merge-multiple`, which leaves each artifact in a directory
# named for it whenever more than one matches, and this script picks the directories to merge.
# The semantics are the ones .github/scripts/ci-ok-report.sh already gives the same problem for
# the jobs' ci-ok reports (`group_by(.job) | map(max_by(.attempt | tonumber))`): the highest
# attempt of a leg wins, and a leg the re-run did not run keeps the artifact of the attempt that
# ran it, since a failed-only re-run uploads nothing for the legs that were green.
#
# It takes the directory `actions/download-artifact` filled, the directory to write, and the id
# of the job that calls the workflow, the merge job's `${{ inputs.job }}`:
#   .github/scripts/dispatch-browser-latest-blobs.sh <download-dir> <out-dir> <job>
# Every artifact name of the workflow starts with that id, so the blob artifacts of ci.yaml's
# callers that share a run are told apart by it: a pull request labelled full-ci runs
# dispatch-browser beside dispatch-browser-pr, and one that changes what the reference rows test
# runs dispatch-references beside dispatch-browser-pr (`dispatch-browser-blob-shard-1-1`,
# `dispatch-browser-pr-blob-shard-1-1` and `dispatch-references-blob-shard-1-1`); the download
# step's pattern already takes only this caller's.
#
# That directory comes in one of two layouts, and which one is the run's, not this job's: it is
# how many artifacts the download's pattern matches that decides it, and a run's artifact list
# holds every attempt's artifacts.
#
# Several artifacts matched: each entry of <download-dir> must be a directory named
# `<job>-blob-<leg>-<attempt>`, where <attempt> is the run of digits at the end and <leg> is
# everything between the prefix and it (`shard-1`, `alone`). A name that does not read is a
# failure, not a file passed over: a blob silently dropped is a merged report short of a leg's
# rows, which reads as a shorter suite rather than as an error.
#
# One artifact matched: actions/download-artifact extracts it into <download-dir> itself and not
# into a directory named for it; the note that cites the action's source is
# .github/ci-notes/dispatch-browser.md#why-every-legs-blob-report-is-downloaded-without-merge-multiple-and-the-two-layouts-it-lands-in.
# So <download-dir> holds that artifact's blob-report/ and rows/ at its top. A run whose pattern
# matches one artifact - one leg uploaded a blob, on one attempt - downloads it so, and read as
# an entry's name, blob-report/ is no leg's artifact: the step failed that run's merge job when
# it had a blob to merge. No artifact the pattern matches is named blob-report or rows, so
# either of those at the top is this layout, and any other entry beside them is a failure. One
# artifact is one leg at one attempt, so there is nothing to choose and it is taken whole; its
# name, which holds the attempt, is not on disk, so the line this prints names the files it took.
# This layout reads no prefix: the merge job's download pattern is its own caller's
# (`<job>-blob-*`), so the one artifact it matched is that caller's.
#
# It writes <out-dir>/blob-report and <out-dir>/rows, holding each leg's selected attempt's files
# and no other's. Where several artifacts matched it prints one line per leg saying which attempt
# it took and which it passed over - the merge job's log is where a reader looks when a re-run's
# number surprises them; where one matched it prints one line naming the files it took.
#
# An empty or absent <download-dir> is not a failure here: it writes the two empty directories
# and says so. The merge step after it already fails when there is nothing to merge, and the
# merge job's reading one names the legs that owed a blob.
#
# Run from anywhere: .github/scripts/dispatch-browser-latest-blobs.sh <download-dir> <out-dir> <job>
# CI runs it in dispatch-browser.yaml's merge job, at the step "Take the newest attempt of each
# leg's blob report", and its tests in ci.yaml's test job, at "Test the browser lane's blob
# attempt selection".
set -euo pipefail
shopt -s inherit_errexit

if [ "$#" -ne 3 ] || [ -z "$3" ]; then
  echo "::error::dispatch-browser-latest-blobs.sh takes the download directory, the directory" \
    "to write and the id of the job that calls dispatch-browser.yaml, and was given $#" \
    "argument(s):" \
    ".github/scripts/dispatch-browser-latest-blobs.sh <download-dir> <out-dir> <job>"
  exit 1
fi
download_dir=$1
out_dir=$2
prefix=$3-blob-

mkdir -p "$out_dir/blob-report" "$out_dir/rows"

if [ ! -d "$download_dir" ] || [ -z "$(ls -A "$download_dir" 2>/dev/null)" ]; then
  echo "No leg's blob artifact was downloaded into $download_dir, so $out_dir/blob-report and" \
    "$out_dir/rows are empty."
  exit 0
fi

# One artifact, extracted into <download-dir> itself: the head says why this is its own layout.
if [ -e "$download_dir/blob-report" ] || [ -e "$download_dir/rows" ]; then
  strays=()
  while IFS= read -r -d '' path; do
    name=${path##*/}
    case "$name" in
      blob-report | rows) [ -d "$path" ] || strays+=("$name") ;;
      *) strays+=("$name") ;;
    esac
  done < <(find "$download_dir" -mindepth 1 -maxdepth 1 -print0 | LC_ALL=C sort -z)
  if [ "${#strays[@]}" -gt 0 ]; then
    for name in "${strays[@]}"; do
      echo "::error::$download_dir/$name is not part of a leg's blob report: $download_dir" \
        "holds blob-report/ or rows/ at its top, which is how actions/download-artifact" \
        "extracts an artifact when exactly one matched, and a leg's artifact holds the" \
        "directories blob-report/ and rows/ and nothing else. This entry is in that one" \
        "artifact: upload only those two directories from the leg in" \
        ".github/workflows/dispatch-browser.yaml."
    done
    exit 1
  fi
  held=
  for part in blob-report rows; do
    [ ! -d "$download_dir/$part" ] || held="$held${held:+ and }$part/"
  done
  cp -R "$download_dir/." "$out_dir/"
  list=
  while IFS= read -r -d '' file; do
    list="$list${list:+, }$file"
  done < <(cd "$out_dir" && find blob-report rows -type f -print0 | LC_ALL=C sort -z)
  echo "$download_dir holds $held at its top, which is how actions/download-artifact extracts" \
    "an artifact when exactly one matched, rather than into a directory named for it. One" \
    "artifact is one leg at one attempt, so it is taken whole: ${list:-no file}."
  exit 0
fi

# The highest attempt per leg, and the attempts it beat. Read in name order, so two runs over one
# download directory report the legs in the same order.
declare -A taken=()        # leg -> the attempt selected, as a number
declare -A taken_dir=()    # leg -> the directory that attempt is in
declare -A passed_over=()  # leg -> the other attempts, in the order they were read
unreadable=()
while IFS= read -r -d '' path; do
  name=${path##*/}
  # <attempt> is the trailing run of digits: the expansion below drops everything up to the last
  # character that is not one. A greedy `(.+)-([0-9]+)$` would read
  # dispatch-browser-blob-shard-1-10 as leg `shard-1-1` at attempt 0.
  attempt=${name##*[!0-9]}
  rest=${name%"$attempt"}
  leg=
  case "$rest" in "$prefix"?*-) leg=${rest#"$prefix"}; leg=${leg%-} ;; esac
  if [ ! -d "$path" ] || [ -z "$attempt" ] || [ -z "$leg" ]; then
    unreadable+=("$name")
    continue
  fi
  # Base 10 explicitly: an attempt written 08 is eight, and `$((08))` is a refusal.
  number=$((10#$attempt))
  if [ -z "${taken[$leg]:-}" ]; then
    taken[$leg]=$number
    taken_dir[$leg]=$path
  elif [ "$number" -gt "${taken[$leg]}" ]; then
    passed_over[$leg]="${passed_over[$leg]:-}${passed_over[$leg]:+, }${taken[$leg]}"
    taken[$leg]=$number
    taken_dir[$leg]=$path
  else
    passed_over[$leg]="${passed_over[$leg]:-}${passed_over[$leg]:+, }$number"
  fi
done < <(find "$download_dir" -mindepth 1 -maxdepth 1 -print0 | LC_ALL=C sort -z)

if [ "${#unreadable[@]}" -gt 0 ]; then
  for name in "${unreadable[@]}"; do
    echo "::error::$download_dir/$name is not a leg's blob report: this step reads a directory" \
      "named ${prefix}<leg>-<attempt>, where <attempt> is the run of digits at the end" \
      "(${prefix}shard-1-2, ${prefix}alone-1). Name the upload in" \
      ".github/workflows/dispatch-browser.yaml that way, or narrow the download step's pattern" \
      "so it does not match this artifact."
  done
  exit 1
fi

# Each leg's selected directory, copied whole: its blob report and the row count that travels
# with it come from the one attempt, so a leg contributes both or neither.
#
# One leg per line, not `for leg in $(…)`: a leg name is an artifact name's tail, and reading it
# with word splitting would read a name holding a space as two legs, neither with a blob or a row
# count of its own. No leg this workflow uploads holds a space today - the names are
# `shard-<N>` and `alone`, built from the matrix number and a literal
# (.github/workflows/dispatch-browser.yaml) - so this is a defence and not a live case, kept
# because what it defends against is silent: a split name selects nothing and the merged report
# comes out a leg short rather than red. A here-string rather
# than a pipe only to match the merge job's own loop, where it is load-bearing because that loop
# accumulates its totals; this loop accumulates nothing, so a pipe would have served as well.
while IFS= read -r leg; do
  [ -n "$leg" ] || continue
  src=${taken_dir[$leg]}
  for part in blob-report rows; do
    if [ -d "$src/$part" ]; then
      cp -R "$src/$part/." "$out_dir/$part/"
    fi
  done
  if [ -n "${passed_over[$leg]:-}" ]; then
    echo "$leg: attempt ${taken[$leg]}, passing over attempt(s) ${passed_over[$leg]}"
  else
    echo "$leg: attempt ${taken[$leg]}"
  fi
done <<< "$(printf '%s\n' "${!taken[@]}" | LC_ALL=C sort)"

echo "Selected ${#taken[@]} leg(s) into $out_dir."
