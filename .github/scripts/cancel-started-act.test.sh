#!/usr/bin/env bash
# Tests for `cancel-started-act.sh`, the act of ci.yaml's cancel-started job: it lists ci.yaml's
# merge-group runs with gh, hands the list to merge-group-cancels.sh and force-cancels each run
# that script names. Whatever answers those calls in GitHub's place holds gh's token and writes
# gh's answer and the words gh prints on stderr, so no run of eight characters of the marker
# not-a-secret-token (redacttest.Piece's run), which the stand-in puts where GitHub's words would
# be, may be in what the act prints.
#
# The cases run the real script from the repository root, where it finds merge-group-cancels.sh
# and gh-read.sh, with a stand-in `gh` first on PATH: it answers the run list from a file the case
# writes, records each call, and refuses the list or a cancel saying the marker on stderr when the
# case asks it to, recording what it said.
#
# Run from anywhere: .github/scripts/cancel-started-act.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/../.." && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/temp"
marker=not-a-secret-token

cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
echo "\$*" >> "$work/calls"
case "\$1 \$2" in
  'run list')
    if [ -e "$work/list-fails" ]; then echo "gh: $marker (HTTP 502)" | tee -a "$work/said" >&2; exit 1; fi
    cat "$work/runs" ;;
  'api --silent')
    if [ -e "$work/cancel-fails" ]; then echo "gh: $marker (HTTP 409)" | tee -a "$work/said" >&2; exit 1; fi ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

main_tip=04cec161128c5a1cdada47ba40626f02b1fdbe38
main_older=7f3b9c2d5e8a1b4c6d9e0f2a3b5c7d8e9f0a1b2c

# prints_no_marker <text>: true when the text holds no run of eight characters of the marker.
prints_no_marker() {
  local at
  for ((at = 0; at + 8 <= ${#marker}; at++)); do
    if [[ $1 == *"${marker:at:8}"* ]]; then
      echo false
      return
    fi
  done
  echo true
}

# run_act: runs the act for pull request 2700's group on main's tip, run 41871000200, over the
# run list in $work/runs; sets out (all it printed) and status, and shows what it printed.
run_act() {
  rm -f "$work/calls" "$work/said"
  set +e
  out=$(cd "$root" && PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r RUNNER_TEMP="$work/temp" \
    HEAD_BRANCH="gh-readonly-queue/main/pr-2700-$main_tip" RUN_ID=41871000200 \
    timeout 60 .github/scripts/cancel-started-act.sh 2>&1)
  status=$?
  set -e
  while IFS= read -r line; do printf '    printed: %s\n' "$line"; done <<<"$out"
}
calls() { grep -c -- "$1" "$work/calls" 2> /dev/null || true; }
said() { grep -cF -- "$marker" "$work/said" 2> /dev/null || true; }
kept() { if grep -rqF -- "$marker" "$work/temp"; then echo true; else echo false; fi; }

# The list: two older groups of pull request 2700, the marker in one's branch and in the other's
# status, and the asking run itself.
jq -n --arg marker "$marker" --arg older "$main_older" --arg tip "$main_tip" '[
  {databaseId: 41871000100, headBranch: "gh-readonly-queue/\($marker)/pr-2700-\($older)",
   status: "in_progress", createdAt: "2026-11-04T19:00:00Z"},
  {databaseId: 41871000150, headBranch: "gh-readonly-queue/main/pr-2700-\($older)",
   status: $marker, createdAt: "2026-11-04T19:01:00Z"},
  {databaseId: 41871000200, headBranch: "gh-readonly-queue/main/pr-2700-\($tip)",
   status: "in_progress", createdAt: "2026-11-04T19:05:00Z"}]' > "$work/runs"

echo "case: the run list's answer holds the marker in a branch and a status"
run_act
check "the stand-in gh is the one called: one list, two force-cancels (control)" \
  "$(is "$status $(calls '^run list') $(calls 'force-cancel')" '0 1 2')"
check "it cancels the two older groups, as the list says" \
  "$(is "$(calls 'runs/41871000100/force-cancel') $(calls 'runs/41871000150/force-cancel')" '1 1')"
check "twin: the stand-in's answer holds the marker, twice" "$(is "$(grep -cF -- "$marker" "$work/runs")" 2)"
check "prints no run of the marker the answer holds" "$(prints_no_marker "$out")"

echo "case: gh's refusal of a force-cancel says the marker"
touch "$work/cancel-fails"
run_act
rm -f "$work/cancel-fails"
check "warns for each refused cancel and goes on, as it did (control)" \
  "$(is "$status $(grep -c '^::warning::force-cancel of run ' <<<"$out")" '0 2')"
check "twin: the stand-in gh said the marker on stderr, at each cancel" "$(is "$(said)" 2)"
check "prints no run of the marker gh said" "$(prints_no_marker "$out")"
check "the files gh-read.sh keeps gh's words in hold it, where the operator finds it" "$(kept)"

echo "case: gh's refusal of the run list says the marker"
rm -rf "$work/temp" && mkdir "$work/temp"
touch "$work/list-fails"
run_act
rm -f "$work/list-fails"
check "fails the step and cancels nothing, as it did (control)" \
  "$(is "$([ "$status" -ne 0 ] && echo failed) $(calls 'force-cancel')" 'failed 0')"
check "twin: the stand-in gh said the marker on stderr" "$(is "$(said)" 1)"
check "prints no run of the marker gh said" "$(prints_no_marker "$out")"
check "the file gh-read.sh keeps gh's words in holds it, where the operator finds it" "$(kept)"

summary "cancel-started-act.sh prints no word of gh's"
