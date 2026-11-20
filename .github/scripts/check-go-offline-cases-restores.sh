# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 3: a fetch step of a required job comes after the job's
# restore of ~/go/pkg/mod. Sourced by check-go-offline.test.sh, whose fixtures, run_check and
# counters they use; not run on its own.

echo "case: a required job whose fetch step comes before its module cache restore is refused"
root=$(fixture fetch-first)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
      - uses: actions/cache/restore@v4
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the fetch step" \
  "$(contains "$out" 'job lint, step "Fetch the modules the tests build", runs go-fetch.sh with no step before it that restores ~/go/pkg/mod')"

echo "case: the same job with the restore first passes, and lists the fetch step with that restore"
root=$(fixture fetch-after)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Restore the module cache
        uses: actions/cache/restore@v4
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "lists the pair" \
  "$(contains "$out" '^  .github/workflows/ci.yaml job lint: "Fetch the modules the tests build" after "Restore the module cache"')"
check "counts it" "$(contains "$out" 'each of the 1 fetch steps of a required job comes after a restore of ~/go/pkg/mod')"

echo "case: a restore of another path does not count"
root=$(fixture fetch-other-cache)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - uses: actions/cache@v4
        with:
          path: ~/.cache/go-build
          key: go-build
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the fetch step" "$(contains "$out" 'step "Fetch the modules the tests build", runs go-fetch.sh with no step before it')"

# called_fetch <name> <fetch line> <first>: a fixture whose ci.yaml calls ci-job-checks.yaml, whose
# one job runs <fetch line> as a step, before its restore of ~/go/pkg/mod when <first> is "first"
# and after it otherwise; run through the check.
called_fetch() {
  local root fetch="      - name: Fetch the modules the tests build
        run: $2" restore='      - name: Restore the module cache
        uses: actions/cache/restore@v4
        with:
          path: ~/go/pkg/mod
          key: go-app'
  root=$(fixture "$1")
  printf '  checks:\n    uses: ./.github/workflows/ci-job-checks.yaml\n' >>"$root/.github/workflows/ci.yaml"
  printf 'tools\n' | put "$root" tools.txt
  printf 'deps packages/app ./cmd/app\n' | put "$root" .github/go-fetch/checks.txt
  {
    printf 'name: CI job checks\non: workflow_call\nenv:\n  GOPROXY: "off"\n  GOFLAGS: -mod=readonly\njobs:\n  checks:\n    runs-on: ubuntu-24.04\n    steps:\n'
    if [ "$3" = first ]; then printf '%s\n%s\n' "$fetch" "$restore"; else printf '%s\n%s\n' "$restore" "$fetch"; fi
  } | put "$root" .github/workflows/ci-job-checks.yaml
  run_check "$root"
}

echo "case: a fetch step in a workflow ci.yaml calls is held to its restore too"
called_fetch fetch-called-first '.github/scripts/go-fetch.sh deps packages/app ./cmd/app' first
check "before the restore: fails" "$(is "$status" 1)"
check "naming the called workflow's job" \
  "$(contains "$out" '.github/workflows/ci-job-checks.yaml:1: job checks, step "Fetch the modules the tests build", runs go-fetch.sh with no step before it')"
called_fetch fetch-called-after '.github/scripts/go-fetch.sh deps packages/app ./cmd/app' after
check "after the restore: exits 0" "$(is "$status" 0)"
check "and lists the pair" \
  "$(contains "$out" '^  .github/workflows/ci-job-checks.yaml job checks: "Fetch the modules the tests build" after "Restore the module cache"')"

echo "case: go-fetch.sh tools and go-fetch.sh file are fetch steps too"
called_fetch fetch-tools-first '.github/scripts/go-fetch.sh tools tools.txt' first
check "tools before the restore: fails" "$(is "$status" 1)"
check "tools: names the step" "$(contains "$out" 'job checks, step "Fetch the modules the tests build", runs go-fetch.sh with no step before it')"
called_fetch fetch-file-first '.github/scripts/go-fetch.sh file .github/go-fetch/checks.txt' first
check "file before the restore: fails" "$(is "$status" 1)"
check "file: names the step" "$(contains "$out" 'job checks, step "Fetch the modules the tests build", runs go-fetch.sh with no step before it')"
called_fetch fetch-file-after '.github/scripts/go-fetch.sh file .github/go-fetch/checks.txt' after
check "file after the restore: exits 0, the pair listed" "$(is "$status" 0)"
check "file: counts it" "$(contains "$out" 'each of the 1 fetch steps of a required job comes after a restore')"

echo "case: actions/cache with ~/go/pkg/mod in its path restores it, as actions/cache/restore does"
root=$(fixture fetch-cache-action)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Cache the modules
        uses: actions/cache@v4
        with:
          path: ~/go/pkg/mod
          key: go-app
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "lists the pair" "$(contains "$out" '^  .github/workflows/ci.yaml job lint: "Fetch the modules the tests build" after "Cache the modules"')"
