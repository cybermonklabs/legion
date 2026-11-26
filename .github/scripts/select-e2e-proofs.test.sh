#!/usr/bin/env bash
# Tests for `select-e2e-proofs.sh`: the matrix e2e.yaml's proof job runs, and the refusal a line
# this script cannot read gets. The discipline this suite holds is that no input reaches a
# traceback: every refusal names the file, the row and the remedy, so the case that proves a
# hosted row with `jj` deleted is refused by name also asserts that the output holds no
# `Traceback`. A row checks where a refusal is written and the rows, keys, values and figures it
# names (names_at), not the words of the refusal or its remedy (dispatch://INFRA-1545, round 70).
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file e2e.yaml runs. The fixtures' scripts and
# rows are invented. One case runs the script over the repository's own
# .github/e2e-scripts.json and scripts/e2e/, so the real data is checked too.
#
# Run from anywhere: .github/scripts/select-e2e-proofs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/select-e2e-proofs.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — two hosted proofs, one of them with packages and an
# evidence variable, one proof of the `elsewhere` list, and a scripts/e2e/ holding all three.
# Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/scripts/e2e"
  ln -s "$check_script" "$root/.github/scripts/select-e2e-proofs.sh"
  local name
  for name in alpha-proof beta-proof gamma-proof; do
    printf '#!/usr/bin/env bash\necho %s\n' "$name" > "$root/scripts/e2e/$name.sh"
  done
  cat > "$root/.github/e2e-scripts.json" <<'JSON'
{
  "hosted": [
    {
      "script": "alpha-proof.sh",
      "go_version_file": "packages/example/go.mod",
      "apt": [
        "tmux",
        "postgresql-client"
      ],
      "bun": true,
      "evidence_variable": "ALPHA_PROOF_EVIDENCE_DIR",
      "timeout_minutes": 25,
      "run_timeout_minutes": 12,
      "why_hosted": "it takes a checkout, go and the job's database, and starts no container",
      "jj": true
    },
    {
      "script": "beta-proof.sh",
      "go_version_file": "packages/example/go.mod",
      "apt": [],
      "bun": false,
      "evidence_variable": "",
      "timeout_minutes": 20,
      "run_timeout_minutes": 8,
      "why_hosted": "it takes a checkout and go, and keeps its work directory",
      "jj": false
    }
  ],
  "elsewhere": [
    {
      "script": "gamma-proof.sh",
      "needs": "a throwaway cluster on an amd64 Linux host and a worker image by digest"
    }
  ]
}
JSON
  echo "$root"
}

# run_select <root> <event> [requested]: runs the fixture's copy of the script. Sets out, status
# and proofs, the matrix the run wrote to its GITHUB_OUTPUT.
run_select() {
  local root=$1 event=$2 requested=${3:-}
  : > "$root/github-output"
  set +e
  out=$(EVENT="$event" REQUESTED="$requested" GITHUB_OUTPUT="$root/github-output" \
    "$root/.github/scripts/select-e2e-proofs.sh" 2>&1)
  status=$?
  set -e
  proofs=$(sed -n 's/^proofs=//p' "$root/github-output")
}

# write_row_key <root> <list> <index> <key> <json value>: sets one key of one row.
write_row_key() {
  python3 - "$1/.github/e2e-scripts.json" "$2" "$3" "$4" "$5" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data[sys.argv[2]][int(sys.argv[3])][sys.argv[4]] = json.loads(sys.argv[5])
path.write_text(json.dumps(data, indent=2) + "\n")
PY
}

# drop_row_key <root> <list> <index> <key>: deletes one key of one row.
drop_row_key() {
  python3 - "$1/.github/e2e-scripts.json" "$2" "$3" "$4" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
del data[sys.argv[2]][int(sys.argv[3])][sys.argv[4]]
path.write_text(json.dumps(data, indent=2) + "\n")
PY
}

# write_row <root> <list> <index> <json row>: replaces one whole row.
write_row() {
  python3 - "$1/.github/e2e-scripts.json" "$2" "$3" "$4" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data[sys.argv[2]][int(sys.argv[3])] = json.loads(sys.argv[4])
path.write_text(json.dumps(data, indent=2) + "\n")
PY
}

# write_file <root> <text>: replaces the list file with text of any shape, JSON or not.
write_file() {
  printf '%s\n' "$2" > "$1/.github/e2e-scripts.json"
}

# proof_count: how many entries the run wrote.
proof_count() {
  python3 -c 'import json, sys; print(len(json.loads(sys.argv[1])))' "$proofs"
}

# proof_scripts: every entry's script, in the order the run wrote them.
proof_scripts() {
  python3 -c 'import json, sys; print(" ".join(e["script"] for e in json.loads(sys.argv[1])))' "$proofs"
}

# proof_keys <index>: one entry's keys, sorted and comma-separated.
proof_keys() {
  python3 -c 'import json, sys; print(",".join(sorted(json.loads(sys.argv[1])[int(sys.argv[2])])))' \
    "$proofs" "$1"
}

# proof_field <index> <key>: one key of one entry. A string is printed as it stands, every other
# value in its JSON form, so a boolean reads true or false.
proof_field() {
  python3 -c 'import json, sys
value = json.loads(sys.argv[1])[int(sys.argv[2])][sys.argv[3]]
print(value if isinstance(value, str) else json.dumps(value))' "$proofs" "$1" "$2"
}

# Where the script refuses the list file, and the keys a hosted row carries, in the order the
# script's MATRIX lists them and a refusal of a missing key names them all.
list_error='::error file=.github/e2e-scripts.json::'
matrix_keys='script, go_version_file, apt, bun, jj, evidence_variable, timeout_minutes, run_timeout_minutes'

echo "case: a workflow_dispatch of a hosted script emits one matrix entry with every key"
root=$(fixture dispatch-green)
run_select "$root" workflow_dispatch alpha-proof
check "exits 0" "$(is "$status" 0)"
check "says what it selected" "$(contains "$out" '^1 proof(s): alpha-proof.sh$')"
check "one entry" "$(is "$(proof_count)" 1)"
check "the entry carries every key the matrix reads" \
  "$(is "$(proof_keys 0)" "apt,bun,evidence_variable,go_version_file,jj,proof,run_timeout_minutes,script,timeout_minutes")"
check "script is the row's" "$(is "$(proof_field 0 script)" alpha-proof.sh)"
check "proof is that name without its .sh" "$(is "$(proof_field 0 proof)" alpha-proof)"
check "go_version_file is the row's" "$(is "$(proof_field 0 go_version_file)" packages/example/go.mod)"
check "apt is the row's list, joined by spaces" "$(is "$(proof_field 0 apt)" "tmux postgresql-client")"
check "bun is a boolean" "$(is "$(proof_field 0 bun)" true)"
check "jj is a boolean" "$(is "$(proof_field 0 jj)" true)"
check "evidence_variable is the row's" "$(is "$(proof_field 0 evidence_variable)" ALPHA_PROOF_EVIDENCE_DIR)"
check "timeout_minutes is a whole number" "$(is "$(proof_field 0 timeout_minutes)" 25)"
check "run_timeout_minutes is a whole number" "$(is "$(proof_field 0 run_timeout_minutes)" 12)"
run_select "$root" workflow_dispatch alpha-proof.sh
check "a name given with its .sh selects the same row" "$(is "$(proof_field 0 script)" alpha-proof.sh)"
run_select "$root" workflow_dispatch "  alpha-proof  "
check "a name given with spaces around it selects the same row" "$(is "$(proof_field 0 script)" alpha-proof.sh)"

echo "case: a label run and the nightly emit every hosted row"
root=$(fixture every-row)
run_select "$root" schedule
check "exits 0" "$(is "$status" 0)"
check "every hosted row, in name order" "$(is "$(proof_scripts)" "alpha-proof.sh beta-proof.sh")"
check "says how many and which" "$(contains "$out" '^2 proof(s): alpha-proof.sh, beta-proof.sh$')"
check "a row with no package carries an empty apt" "$(is "$(proof_field 1 apt)" "")"
check "and the row's own limit" "$(is "$(proof_field 1 timeout_minutes)" 20)"
check "and its proof step's own limit" "$(is "$(proof_field 1 run_timeout_minutes)" 8)"
run_select "$root" pull_request
check "the e2e label selects the same rows" "$(is "$(proof_scripts)" "alpha-proof.sh beta-proof.sh")"

echo "case: a dispatch_only row runs on its dispatch alone"
root=$(fixture dispatch-only)
write_row_key "$root" hosted 1 dispatch_only true
run_select "$root" schedule
check "the nightly passes over it" "$(is "$(proof_scripts)" "alpha-proof.sh")"
check "and says so in its count" "$(contains "$out" '^1 proof(s): alpha-proof.sh$')"
run_select "$root" pull_request
check "the e2e label passes over it too" "$(is "$(proof_scripts)" "alpha-proof.sh")"
run_select "$root" workflow_dispatch beta-proof
check "its dispatch selects it" "$(is "$(proof_scripts)" "beta-proof.sh")"
check "and the entry carries no dispatch_only" "$(is "$(proof_keys 0)" "apt,bun,evidence_variable,go_version_file,jj,proof,run_timeout_minutes,script,timeout_minutes")"
write_row_key "$root" hosted 1 dispatch_only false
run_select "$root" schedule
check "a false dispatch_only is a row like any other" "$(is "$(proof_scripts)" "alpha-proof.sh beta-proof.sh")"
write_row_key "$root" hosted 1 dispatch_only '"yes"'
run_select "$root" schedule
check "a dispatch_only that is no boolean fails" "$(is "$status" 1)"
check "and names the file, the row, the key and its value" \
  "$(names_at "$out" "$list_error" beta-proof.sh "dispatch_only 'yes'")"

echo "case: a hosted row with jj deleted is refused by name, with no traceback"
root=$(fixture missing-jj)
drop_row_key "$root" hosted 0 jj
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file, the row, the one key it lacks and every key the matrix reads" \
  "$(names_at "$out" "$list_error" alpha-proof.sh "$matrix_keys" jj --not go_version_file evidence_variable)"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"
check "and writes no matrix" "$(is "$proofs" "")"

echo "case: a hosted row missing several keys names every one of them"
root=$(fixture missing-keys)
drop_row_key "$root" hosted 1 apt
drop_row_key "$root" hosted 1 timeout_minutes
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file, the row and both keys it lacks" \
  "$(names_at "$out" "$list_error" beta-proof.sh "$matrix_keys" apt timeout_minutes --not go_version_file evidence_variable)"

echo "case: a hosted key holding the wrong kind of value"
root=$(fixture wrong-timeout)
write_row_key "$root" hosted 0 timeout_minutes '"25"'
run_select "$root" schedule
check "a quoted timeout_minutes fails" "$(is "$status" 1)"
check "names the file, the row, the key and its value" \
  "$(names_at "$out" "$list_error" alpha-proof.sh "timeout_minutes '25'")"

root=$(fixture boolean-timeout)
write_row_key "$root" hosted 0 timeout_minutes true
run_select "$root" schedule
check "a boolean timeout_minutes fails, though Python counts one as a number" "$(is "$status" 1)"
check "and is named, with its value" "$(names_at "$out" "$list_error" alpha-proof.sh "timeout_minutes True")"

root=$(fixture apt-string)
write_row_key "$root" hosted 0 apt '"tmux"'
run_select "$root" schedule
check "an apt that is one string fails" "$(is "$status" 1)"
check "and is named, with its value" "$(names_at "$out" "$list_error" alpha-proof.sh "apt 'tmux'")"

root=$(fixture apt-numbers)
write_row_key "$root" hosted 0 apt '["tmux", 7]'
run_select "$root" schedule
check "an apt holding a number fails" "$(is "$status" 1)"
check "and is named, with its value" "$(names_at "$out" "$list_error" alpha-proof.sh "apt ['tmux', 7]")"

root=$(fixture bun-string)
write_row_key "$root" hosted 0 bun '"yes"'
run_select "$root" schedule
check "a bun that is a string fails" "$(is "$status" 1)"
check "and is named, with its value" "$(names_at "$out" "$list_error" alpha-proof.sh "bun 'yes'")"

root=$(fixture jj-number)
write_row_key "$root" hosted 0 jj 1
run_select "$root" schedule
check "a jj that is a number fails" "$(is "$status" 1)"
check "and is named, with its value" "$(names_at "$out" "$list_error" alpha-proof.sh "jj 1")"

echo "case: run_timeout_minutes, the limit of the step that runs the script"
root=$(fixture missing-run-timeout)
drop_row_key "$root" hosted 0 run_timeout_minutes
run_select "$root" schedule
check "a row with no run_timeout_minutes fails" "$(is "$status" 1)"
check "names the file, the row and the key it lacks" \
  "$(names_at "$out" "$list_error" alpha-proof.sh "$matrix_keys" run_timeout_minutes --not go_version_file evidence_variable)"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"
check "and writes no matrix" "$(is "$proofs" "")"

root=$(fixture quoted-run-timeout)
write_row_key "$root" hosted 0 run_timeout_minutes '"12"'
run_select "$root" schedule
check "a quoted run_timeout_minutes fails" "$(is "$status" 1)"
check "names the file, the row, the key and its value" \
  "$(names_at "$out" "$list_error" alpha-proof.sh "run_timeout_minutes '12'")"
check "and is refused once, not compared with the job's limit as well" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"

root=$(fixture fraction-run-timeout)
write_row_key "$root" hosted 0 run_timeout_minutes 12.5
run_select "$root" schedule
check "a run_timeout_minutes with a fraction fails" "$(is "$status" 1)"
check "and is named, with its value" "$(names_at "$out" "$list_error" alpha-proof.sh "run_timeout_minutes 12.5")"

root=$(fixture equal-run-timeout)
write_row_key "$root" hosted 0 run_timeout_minutes 25
run_select "$root" schedule
check "a run_timeout_minutes equal to the row's timeout_minutes fails" "$(is "$status" 1)"
check "names the file, the row, its run_timeout_minutes and the timeout_minutes it is not below" \
  "$(names_at "$out" "$list_error" alpha-proof.sh "run_timeout_minutes 25" "timeout_minutes 25")"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"
check "and writes no matrix" "$(is "$proofs" "")"

root=$(fixture above-run-timeout)
write_row_key "$root" hosted 1 run_timeout_minutes 30
run_select "$root" schedule
check "a run_timeout_minutes above the row's timeout_minutes fails" "$(is "$status" 1)"
check "names the file, the row, its run_timeout_minutes and the timeout_minutes it is not below" \
  "$(names_at "$out" "$list_error" beta-proof.sh "run_timeout_minutes 30" "timeout_minutes 20")"

root=$(fixture just-below-run-timeout)
write_row_key "$root" hosted 0 run_timeout_minutes 24
run_select "$root" workflow_dispatch alpha-proof
check "a run_timeout_minutes one minute below timeout_minutes passes" "$(is "$status" 0)"
check "and is carried as the row gives it" "$(is "$(proof_field 0 run_timeout_minutes)" 24)"

echo "case: a row that is not an object, or that has no script string"
root=$(fixture row-not-object)
write_row "$root" hosted 0 '["alpha-proof.sh"]'
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file, the list and the row, and what the row is" \
  "$(names_at "$out" "$list_error" "hosted[0]" "a list")"

root=$(fixture row-no-script)
drop_row_key "$root" elsewhere 0 script
run_select "$root" schedule
check "a row with no script fails" "$(is "$status" 1)"
check "and names the file, the list, the row and the key" \
  "$(names_at "$out" "$list_error" "elsewhere[0]" '"script"')"

root=$(fixture script-not-string)
write_row_key "$root" hosted 1 script 7
run_select "$root" schedule
check "a script that is no string fails" "$(is "$status" 1)"
check "and names the file, the row and the key" "$(names_at "$out" "$list_error" "hosted[1]" '"script"')"

echo "case: an elsewhere row with no needs text"
root=$(fixture no-needs)
drop_row_key "$root" elsewhere 0 needs
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file, the row and the key" \
  "$(names_at "$out" "$list_error" gamma-proof.sh needs)"

root=$(fixture blank-needs)
write_row_key "$root" elsewhere 0 needs '"   "'
run_select "$root" schedule
check "needs holding spaces alone fails too" "$(is "$status" 1)"
check "and is named" "$(names_at "$out" "$list_error" gamma-proof.sh needs)"

echo "case: a file that is not JSON, or that this script cannot read"
root=$(fixture not-json)
write_file "$root" '{"hosted": ['
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file and the line and column where the syntax breaks" \
  "$(names_at "$out" "$list_error" "line 2 column 1")"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"

root=$(fixture no-file)
rm "$root/.github/e2e-scripts.json"
run_select "$root" schedule
check "a missing file fails" "$(is "$status" 1)"
check "and names the file and why it cannot be read" \
  "$(names_at "$out" "$list_error" "No such file or directory")"

echo "case: a top level that is not an object holding both lists"
root=$(fixture top-level-list)
write_file "$root" '[]'
run_select "$root" schedule
check "a list at the top level fails" "$(is "$status" 1)"
check "and names the file and what its top level is" \
  "$(names_at "$out" "$list_error" "a list")"

root=$(fixture no-hosted)
write_file "$root" '{"elsewhere": []}'
run_select "$root" schedule
check "a file with no hosted list fails" "$(is "$status" 1)"
check "and names the file and that list alone" \
  "$(names_at "$out" "$list_error" '"hosted"' --not '"elsewhere"')"

root=$(fixture elsewhere-not-list)
write_file "$root" '{"hosted": [], "elsewhere": {}}'
run_select "$root" schedule
check "an elsewhere that is an object fails" "$(is "$status" 1)"
check "and names the file and that list alone" \
  "$(names_at "$out" "$list_error" '"elsewhere"' --not '"hosted"')"

echo "case: a script under scripts/e2e/ that neither list names"
root=$(fixture unlisted-script)
printf '#!/usr/bin/env bash\necho delta\n' > "$root/scripts/e2e/delta-proof.sh"
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file and the script" \
  "$(names_at "$out" "$list_error" scripts/e2e/delta-proof.sh)"

echo "case: a row naming a script the tree does not hold"
root=$(fixture stale-row)
rm "$root/scripts/e2e/gamma-proof.sh"
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file and the row" \
  "$(names_at "$out" "$list_error" gamma-proof.sh)"

echo "case: a workflow_dispatch for a script of the elsewhere list"
root=$(fixture dispatch-elsewhere)
run_select "$root" workflow_dispatch gamma-proof
check "fails" "$(is "$status" 1)"
check "names the script and what its row says it needs" \
  "$(names_at "$out" '::error::' scripts/e2e/gamma-proof.sh "a throwaway cluster on an amd64 Linux host and a worker image by digest")"

echo "case: a workflow_dispatch for a name in neither list"
root=$(fixture dispatch-unknown)
run_select "$root" workflow_dispatch nothing-here
check "fails" "$(is "$status" 1)"
check "names the script and every name it could have been given, the two hosted rows and not the elsewhere one" \
  "$(names_at "$out" '::error::' scripts/e2e/nothing-here.sh alpha-proof.sh beta-proof.sh --not gamma-proof.sh)"
run_select "$root" workflow_dispatch ""
check "a dispatch naming nothing fails" "$(is "$status" 1)"
check "and is refused by the name it was given" \
  "$(names_at "$out" '::error::' scripts/e2e/.sh)"
check "  no traceback" "$(is "$(contains "$out" Traceback)" false)"

echo "case: the environment e2e.yaml's select job sets"
root=$(fixture environment)
set +e
out=$(env -u EVENT GITHUB_OUTPUT="$root/github-output" \
  "$root/.github/scripts/select-e2e-proofs.sh" 2>&1)
status=$?
set -e
check "an unset EVENT fails" "$(is "$status" 1)"
check "and is named" "$(names_at "$out" '::error::' EVENT)"
check "  no traceback" "$(is "$(contains "$out" Traceback)" false)"
set +e
out=$(env -u GITHUB_OUTPUT EVENT=schedule \
  "$root/.github/scripts/select-e2e-proofs.sh" 2>&1)
status=$?
set -e
check "an unset GITHUB_OUTPUT fails" "$(is "$status" 1)"
check "and is named" "$(names_at "$out" '::error::' GITHUB_OUTPUT)"
check "  no traceback" "$(is "$(contains "$out" Traceback)" false)"

# The real data, through the real script: every hosted row of .github/e2e-scripts.json against the
# scripts/e2e/ of this repository.
hosted_scripts=$(python3 -c 'import json, pathlib, sys
rows = json.loads(pathlib.Path(sys.argv[1]).read_text())["hosted"]
print(" ".join(sorted(row["script"] for row in rows if not row.get("dispatch_only", False))))' "$script_dir/../e2e-scripts.json")
echo "case: the repository's own .github/e2e-scripts.json and scripts/e2e/ ($hosted_scripts)"
real_output="$work/real-output"
: > "$real_output"
set +e
out=$(EVENT=schedule REQUESTED= GITHUB_OUTPUT="$real_output" "$check_script" 2>&1)
status=$?
set -e
proofs=$(sed -n 's/^proofs=//p' "$real_output")
check "a schedule run of the repository's own data passes" "$(is "$status" 0)"
check "and emits every hosted row a dispatch_only does not keep to its dispatch" "$(is "$(proof_scripts)" "$hosted_scripts")"
for name in $hosted_scripts; do
  : > "$real_output"
  set +e
  out=$(EVENT=workflow_dispatch REQUESTED="$name" GITHUB_OUTPUT="$real_output" "$check_script" 2>&1)
  status=$?
  set -e
  proofs=$(sed -n 's/^proofs=//p' "$real_output")
  check "a workflow_dispatch of $name passes" "$(is "$status" 0)"
  check "  and emits that row alone" "$(is "$(proof_scripts)" "$name")"
  check "  with every key the matrix reads" \
    "$(is "$(proof_keys 0)" "apt,bun,evidence_variable,go_version_file,jj,proof,run_timeout_minutes,script,timeout_minutes")"
done

summary "select-e2e-proofs.sh"
