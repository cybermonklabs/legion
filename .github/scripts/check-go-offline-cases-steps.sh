# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 2: a job that runs testbuild's tests fetches their commands
# first, and every offline build a step reaches is fetched in a step before it. Sourced by
# check-go-offline.test.sh, whose fixtures, run_check and counters they use; not run on its own.

echo "case: a job that runs internal/daemon's tests with no fetch step is refused"
root=$(daemon_fixture job-bare)
nightly "$root" <<'EOF'
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workflow, the job and the step" \
  "$(contains "$out" '.github/workflows/nightly.yaml:1: job proof, step "Test the daemon", runs the tests of packages/daemon/internal/daemon')"
check "names the fetch to add, with both commands" \
  "$(contains "$out" 'add `.github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener` before it')"

echo "case: the same job with the fetch step before the tests passes"
root=$(daemon_fixture job-fetched)
nightly "$root" <<'EOF'
      - name: Fetch the modules of the commands the tests build
        run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" "each of the 1 steps that run testbuild's tests comes after its fetch")"

echo "case: a fetch step after the tests is refused"
root=$(daemon_fixture job-late)
nightly "$root" <<'EOF'
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
      - name: Fetch the modules of the commands the tests build
        run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step" "$(contains "$out" 'job proof, step "Test the daemon"')"

echo "case: a fetch step that leaves a command out is refused"
root=$(daemon_fixture job-partial)
nightly "$root" <<'EOF'
      - name: Fetch the modules of the Dispatch server
        run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step" "$(contains "$out" 'job proof, step "Test the daemon"')"

echo "case: a fetch step in another job does not count"
root=$(daemon_fixture job-other)
nightly "$root" <<'EOF'
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
  fetch:
    runs-on: ubuntu-24.04
    steps:
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job without the fetch" "$(contains "$out" 'job proof, step "Test the daemon"')"

# As e2e.yaml's proof job runs them: the step runs an entry script, which sources the library that
# compiles the daemon's tests into a driver.
echo "case: a step that runs the tests through a script and the library it sources is refused, and passes after the fetch"
root=$(daemon_fixture job-script)
put "$root" scripts/e2e/proof.sh <<'EOF'
#!/usr/bin/env bash
root=$(cd "$(dirname "$0")/../.." && pwd)
. "$root/scripts/e2e/lib/proof.sh"
EOF
put "$root" scripts/e2e/lib/proof.sh <<'EOF'
# go-offline-file: a person runs the proof that sources this, and the nightly job.
go -C "$root/packages/daemon" test -c -o "$work/driver" ./internal/daemon
EOF
nightly "$root" <<'EOF'
      - name: Run the proof
        run: bash scripts/e2e/proof.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step that runs the script" "$(contains "$out" 'job proof, step "Run the proof", runs the tests of packages/daemon/internal/daemon')"
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Run the proof
        run: bash scripts/e2e/proof.sh
EOF
run_check "$root"
check "passes once the job fetches first" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"

# needs_fetch <name> <step name> <steps> [<script>]: a job of those steps alone is refused, naming
# the step, and the same job with the fetch step first passes, judging that one step. <script>,
# when given, is written to scripts/daemon-tests.sh in both trees.
needs_fetch() {
  local root fetch='      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener'
  root=$(daemon_fixture "$1")
  printf '%s\n' "$3" | nightly "$root"
  [ -z "${4:-}" ] || printf '%s\n' "$4" | put "$root" scripts/daemon-tests.sh
  run_check "$root"
  check "fails without the fetch" "$(is "$status" 1)"
  check "names the step" "$(contains "$out" "job proof, step \"$2\", runs the tests of packages/daemon/internal/daemon")"
  root=$(daemon_fixture "$1-fetched")
  printf '%s\n%s\n' "$fetch" "$3" | nightly "$root"
  [ -z "${4:-}" ] || printf '%s\n' "$4" | put "$root" scripts/daemon-tests.sh
  run_check "$root"
  check "passes with the fetch before it" "$(is "$status" 0)"
  check "judges the step" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"
}

echo "case: a step that runs the tests after a cd on its own line is judged"
needs_fetch job-cd "Test the daemon after a cd" "$(cat <<'EOF'
      - name: Test the daemon after a cd
        run: cd packages/daemon && go test ./internal/daemon/
EOF
)"

echo "case: a step that runs the tests with go test -C is judged"
needs_fetch job-test-chdir "Test the daemon with go test -C" "$(cat <<'EOF'
      - name: Test the daemon with go test -C
        run: go test -C packages/daemon ./internal/daemon/
EOF
)"

echo "case: a step whose script runs the tests in a ( cd ... && go test ) group is judged"
needs_fetch job-script-cd "Run the tests of the daemon" "$(cat <<'EOF'
      - name: Run the tests of the daemon
        run: bash scripts/daemon-tests.sh
EOF
)" "$(cat <<'EOF'
#!/usr/bin/env bash
root=$(cd "$(dirname "$0")/.." && pwd)
(cd "$root/packages/daemon" && GOPROXY=off go test ./internal/daemon/)
EOF
)"

echo "case: a step that compiles the tests to /dev/null runs none and needs no fetch"
root=$(daemon_fixture job-compile)
nightly "$root" <<'EOF'
      - name: Compile the daemon's tests
        run: go test -c -o /dev/null ./internal/daemon/
        working-directory: packages/daemon
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "exits 0, the compile before the fetch refused nothing" "$(is "$status" 0)"
check "judges the test step alone" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"

echo "case: a step that runs the tests through bash -c is judged"
needs_fetch job-bash-c "Test the daemon through bash -c" "$(cat <<'EOF'
      - name: Test the daemon through bash -c
        run: bash -c 'cd packages/daemon && go test ./internal/daemon/'
EOF
)"

# --- The second rule's other half: every offline build a step reaches ---------------------------
# A job of any workflow whose step reaches a go build made with GOPROXY=off - in its run text, in
# a script it runs or sources, in a function such a script calls, or in a script the TypeScript of
# a bun or Playwright test run names - fetches what that build builds in a step before.

# broker_script <root>: scripts/run-broker.sh, which builds two commands of packages/envoy offline
# from its own directory, as the Dispatch launchers do.
broker_script() {
  mkdir -p "$1/packages/envoy"
  put "$1" scripts/run-broker.sh <<'EOF'
#!/usr/bin/env bash
cd "$(dirname "$0")/../packages/envoy"
env GOPROXY=off go build -o ./out/ ./cmd/broker ./cmd/dev-oidc-issuer
EOF
}

echo "case: a job whose step runs a script that builds offline, with no fetch before it, is refused"
root=$(fixture offline-bare)
broker_script "$root"
nightly "$root" <<'EOF'
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the step, the build and its packages" \
  "$(contains "$out" 'job proof, step "Start the broker", reaches scripts/run-broker.sh:3, an offline go command of packages/envoy: ./cmd/broker ./cmd/dev-oidc-issuer')"
check "names the fetch to add" "$(contains "$out" 'add `.github/scripts/go-fetch.sh deps packages/envoy ./cmd/broker ./cmd/dev-oidc-issuer`')"

echo "case: the same job passes after a fetch of both commands, and not after a fetch of one"
root=$(fixture offline-fetched)
broker_script "$root"
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/broker ./cmd/dev-oidc-issuer
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" 'each of the 1 steps that reach an offline build comes after a fetch')"
root=$(fixture offline-partial)
broker_script "$root"
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/broker
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "fails when the fetch leaves a command out" "$(is "$status" 1)"
check "naming the one it leaves out" "$(contains "$out" 'reaches scripts/run-broker.sh:3')"

echo "case: a fetch file's deps lines fetch what they name, a setting, -test and -tags aside"
root=$(fixture offline-file)
broker_script "$root"
put "$root" .github/go-fetch/proof.txt <<'EOF'
# the broker and its issuer, which the stack builds
GOOS=linux deps packages/envoy -test -tags e2e ./cmd/broker ./cmd/dev-oidc-issuer  # one line
EOF
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh file .github/go-fetch/proof.txt
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" 'each of the 1 steps that reach an offline build comes after a fetch')"
root=$(fixture offline-file-partial)
broker_script "$root"
put "$root" .github/go-fetch/proof.txt <<'EOF'
deps packages/envoy ./cmd/broker
# deps packages/envoy ./cmd/dev-oidc-issuer
EOF
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh file .github/go-fetch/proof.txt
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "fails when the file leaves a command out, a comment line fetching nothing" "$(is "$status" 1)"
check "naming the build" "$(contains "$out" 'reaches scripts/run-broker.sh:3')"

echo "case: a build in a sourced function, called with its module and packages, is judged"
root=$(fixture offline-function)
mkdir -p "$root/packages/envoy"
put "$root" lib/go-build-offline.sh <<'EOF'
go_build_offline() {
  local dir=$1 out=$2 root
  shift 2
  root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
  [[ "$dir" == /* ]] || dir="$root/$dir"
  (cd -- "$dir" && GOPROXY=off go build -o "$out" "$@")
}
EOF
put "$root" scripts/start.sh <<'EOF'
#!/usr/bin/env bash
. "$(dirname "$0")/../lib/go-build-offline.sh"
go_build_offline packages/envoy ./bin/ ./cmd/listener
EOF
nightly "$root" <<'EOF'
      - name: Start the listener
        run: scripts/start.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the build in the function, with the caller's module and package" \
  "$(contains "$out" 'reaches lib/go-build-offline.sh:6, an offline go command of packages/envoy: ./cmd/listener')"

echo "case: a Playwright run whose TypeScript names a script that builds offline is judged"
root=$(fixture offline-harness)
mkdir -p "$root/packages/envoy"
put "$root" packages/dispatch/e2e/harness.ts <<'EOF'
export const serverScript = join(import.meta.dir, "run-server.sh");
EOF
put "$root" packages/dispatch/e2e/run-server.sh <<'EOF'
#!/usr/bin/env bash
cd "$(dirname "$0")/../../envoy"
env GOPROXY=off go build -o ./dispatch-e2e ./cmd/dispatch
EOF
nightly "$root" <<'EOF'
      - name: Run the browser suite
        run: bunx playwright test --config e2e/playwright.config.ts
        working-directory: packages/dispatch
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the script the harness starts" \
  "$(contains "$out" 'step "Run the browser suite", reaches packages/dispatch/e2e/run-server.sh:3, an offline go command of packages/envoy: ./cmd/dispatch')"

echo "case: a step that says why it needs no fetch passes, counted as such"
root=$(fixture offline-reasoned)
broker_script "$root"
nightly "$root" <<'EOF'
      - name: Start the broker
        run: |
          # go-offline: the binaries job built the broker; DISPATCH_E2E_BROKER_BIN_DIR names it.
          bash scripts/run-broker.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts its reason" "$(contains "$out" '(1 say why they need none)')"

echo "case: a reason in a YAML comment within the five lines above the step's name counts, and one six lines up does not"
root=$(fixture offline-reason-above)
broker_script "$root"
nightly "$root" <<'EOF'
      # go-offline: the binaries job built the broker; DISPATCH_E2E_BROKER_BIN_DIR names it.
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts its reason" "$(contains "$out" '(1 say why they need none)')"
root=$(fixture offline-reason-far)
broker_script "$root"
nightly "$root" <<'EOF'
      # go-offline: the binaries job built the broker; DISPATCH_E2E_BROKER_BIN_DIR names it.
      #
      #
      #
      #
      #
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "six lines up: fails" "$(is "$status" 1)"
check "six lines up: names the step" "$(contains "$out" 'job proof, step "Start the broker", reaches scripts/run-broker.sh:3')"

echo "case: a go test -list runs no test, so it needs no fetch"
root=$(daemon_fixture job-list)
nightly "$root" <<'EOF'
      - name: List the daemon's tests
        run: go test -list '.*' ./internal/daemon/
        working-directory: packages/daemon
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "exits 0, the listing before the fetch refused nothing" "$(is "$status" 0)"
check "judges the test step alone" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"

echo "case: an offline go install, run, list or generate a step reaches is a build it needs a fetch for"
for sub in install run list generate; do
  root=$(fixture "offline-$sub")
  mkdir -p "$root/packages/envoy"
  printf '#!/usr/bin/env bash\ncd "$(dirname "$0")/../packages/envoy"\nenv GOPROXY=off go %s ./cmd/broker\n' "$sub" |
    put "$root" scripts/run-broker.sh
  printf '      - name: Start the broker\n        run: bash scripts/run-broker.sh\n' | nightly "$root"
  run_check "$root"
  check "go $sub: fails with no fetch" "$(is "$status" 1)"
  check "go $sub: names the build" \
    "$(contains "$out" 'reaches scripts/run-broker.sh:3, an offline go command of packages/envoy: ./cmd/broker')"
done

# tool_fixture <name> <fetch step>: a job whose script installs a pinned helm offline, after
# <fetch step> when it is not empty, with tools.txt listing the pin and a fetch file naming it.
tool_fixture() {
  root=$(fixture "$1")
  mkdir -p "$root/packages/envoy"
  printf '#!/usr/bin/env bash\ncd "$(dirname "$0")/../packages/envoy"\nenv GOPROXY=off go install helm.sh/helm/v3/cmd/helm@v3.20.2\n' |
    put "$root" scripts/helm.sh
  printf 'helm.sh/helm/v3/cmd/helm@v3.20.2  # the chart test renders with it\n' | put "$root" tools.txt
  printf 'tools tools.txt\n' | put "$root" .github/go-fetch/proof.txt
  { [ -z "$2" ] || printf '      - run: %s\n' "$2"; printf '      - name: Install helm\n        run: bash scripts/helm.sh\n'; } |
    nightly "$root"
  run_check "$root"
}

echo "case: an offline go install of a pinned tool needs go-fetch.sh tools of a file that lists the pin, or a fetch file's tools line"
tool_fixture offline-tool ""
check "with no fetch: fails" "$(is "$status" 1)"
check "with no fetch: names the pin" \
  "$(contains "$out" 'reaches scripts/helm.sh:3, an offline go command of packages/envoy: helm.sh/helm/v3/cmd/helm@v3.20.2')"
tool_fixture offline-tool-fetched ".github/scripts/go-fetch.sh tools tools.txt"
check "after go-fetch.sh tools: exits 0" "$(is "$status" 0)"
tool_fixture offline-tool-file ".github/scripts/go-fetch.sh file .github/go-fetch/proof.txt"
check "after a fetch file's tools line: exits 0" "$(is "$status" 0)"

echo "case: a goCommand of a TypeScript file a bun test run reaches builds its packages: fetched before, or refused"
for fetched in no yes; do
  root=$(fixture "offline-gocommand-$fetched")
  put "$root" packages/web/e2e/build.test.ts <<'EOF'
test("builds", async () => {
  await goCommand({ cwd: dir, args: ["build", "./cmd/listener"], packages: ["./cmd/listener"] });
});
EOF
  { [ "$fetched" = no ] || printf '      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/listener\n'
    printf '      - name: Test the web package\n        run: bun test e2e/\n        working-directory: packages/web\n'; } |
    nightly "$root"
  run_check "$root"
done
check "fetched: exits 0" "$(is "$status" 0)"
root=$work/offline-gocommand-no
run_check "$root"
check "not fetched: fails" "$(is "$status" 1)"
check "not fetched: names the goCommand and its packages" \
  "$(contains "$out" 'packages/web/e2e/build.test.ts:2 (goCommand), an offline go command of its module: ./cmd/listener')"
