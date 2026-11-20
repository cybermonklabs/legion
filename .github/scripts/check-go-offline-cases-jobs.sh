# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rules 4 and 5, which it calls from go_offline_jobs.py and
# package_installs.py: a required Go job whose env lacks GOPROXY=off, and a bare apt-get install in a
# required job, each fail the go check at their line; an UNKNOWN of either is the check's; a clean
# tree's line carries both rules' clauses. Sourced by check-go-offline.test.sh, whose fixtures,
# run_check and counters they use; not run on its own. Each rule's own forms are its own harness's
# (go-offline-jobs.test.sh, package-installs.test.sh); these hold the call.

# line_of <file> <fixed string>: the line number of the first line of <file> holding the string.
line_of() { grep -n -m1 -F -- "$2" "$1" | cut -d: -f1; }

echo "case: a clean tree's line carries rules 4 and 5's clauses"
root=$(fixture jobs-clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "rule 4's clause" "$(contains "$out" '1 Go jobs of 1 required workflows read, each with GOPROXY=off and GOFLAGS=-mod=readonly')"
check "rule 5's clause" "$(contains "$out" 'no required job installs a package off the cached path: 1 required workflows and 0 local actions')"

echo "case: a required Go job whose env lacks GOPROXY=off fails the go check, at the job's line"
root=$(fixture jobs-online)
sed -i '/^  GOPROXY: "off"$/d' "$root/.github/workflows/ci.yaml"
line=$(line_of "$root/.github/workflows/ci.yaml" '  lint:')
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job's line and what it lacks" \
  "$(contains "$out" "\.github/workflows/ci\.yaml:$line: job lint of \.github/workflows/ci\.yaml runs go (first at step \"actions/setup-go@v5\") and its env lacks GOPROXY=off (it is unset)")"
check "annotates that line" "$(contains "$out" "::error file=\.github/workflows/ci\.yaml,line=$line::")"

echo "case: a bare apt-get install in a required job fails the go check, at its line"
root=$(fixture jobs-apt)
printf '      - name: Install tmux\n        run: sudo apt-get install -y tmux\n' >>"$root/.github/workflows/ci.yaml"
line=$(line_of "$root/.github/workflows/ci.yaml" 'sudo apt-get install')
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step's line and the command" \
  "$(contains "$out" "\.github/workflows/ci\.yaml:$line: \.github/workflows/ci\.yaml, job lint, step \"Install tmux\" runs \`apt-get install -y tmux\`")"
check "annotates that line" "$(contains "$out" "::error file=\.github/workflows/ci\.yaml,line=$line::")"

echo "case: rule 4's UNKNOWN, a required workflow with no Go job, is the check's"
root=$(fixture jobs-no-go)
sed -i '/actions\/setup-go/d' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says it found no Go job" "$(contains "$out" '^UNKNOWN: read 1 required workflows (\.github/workflows/ci\.yaml) and found no Go job')"

echo "case: rule 5's UNKNOWN, a required workflow with no run step, is the check's"
root=$(fixture jobs-no-run)
sed -i '/- run: echo lint/d' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says it read no run step" "$(contains "$out" '^UNKNOWN: read no run step of the 1 required workflows')"
