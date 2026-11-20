# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 7: each offline go command a step of a required Go job runs
# loads only packages a fetch before it in the job names, by its packages (a /... pattern ends at a
# nested module), its tests, its tags and its platform; a command it cannot read, or one no fetch
# holds, passes only with a go-offline reason; a step that goes online on purpose is not read. Each
# form of a command's directory, packages, tags and settings the rule's header names has a case of
# its own.
# Sourced by check-go-offline.test.sh, whose fixtures, run_check and counters they use; not run on
# its own.

# fetched <name> <fetch file> <steps>: a fixture whose job lint restores ~/go/pkg/mod, runs the
# fetch file <fetch file> (as .github/go-fetch/lint.txt), then <steps> (indented to sit under
# `steps:`); packages/app is a Go module. Run through the check.
fetched() {
  fetched_with "$1" "$2" "" "" "$3"
}

# fetched_with <name> <fetch file> <workflow lines> <job lines> <steps>: fetched's fixture, with
# <workflow lines> added to the workflow's top level above its jobs and <job lines> to job lint
# above its steps, each written at its own indentation; either may be empty. The fetch step runs
# from the root whatever defaults.run those lines give, as go-fetch.sh reads its file from there.
fetched_with() {
  local root text
  root=$(fixture "$1")
  printf 'module example.com/app\n\ngo 1.21\n' | put "$root" packages/app/go.mod
  printf '%s\n' "$2" | put "$root" .github/go-fetch/lint.txt
  text=$(cat "$root/.github/workflows/ci.yaml")
  [ -z "$3" ] || text=${text/$'\njobs:\n'/$'\n'"$3"$'\njobs:\n'}
  [ -z "$4" ] || text=${text/$'\n    steps:\n'/$'\n'"$4"$'\n    steps:\n'}
  {
    printf '%s\n' "$text"
    cat <<'EOF'
      - name: Restore the module cache
        uses: actions/cache/restore@v4
        with:
          path: ~/go/pkg/mod
          key: go-app
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh file .github/go-fetch/lint.txt
        working-directory: .
EOF
    printf '%s\n' "$5"
  } >"$root/.github/workflows/ci.yaml"
  run_check "$root"
}

# The pass line's count of commands held and of those that say why: held <n> <said>.
held() {
  contains "$out" "each of the $1 offline go commands the steps of required Go jobs run loads only packages a fetch before it in its job names ($2 say"
}

# The refusal of a command rule 7 cannot read: unread <command as the check shows it>.
unread() {
  contains "$out" "runs \`$1\`, whose packages, tags, directory or GOOS, GOARCH, CGO_ENABLED or GOWORK this rule cannot read"
}

TEST_X='      - name: Test the app
        run: go test -count=1 ./internal/x/
        working-directory: packages/app'

echo "case: a go step whose packages a deps line of the fetch before it holds passes, and is counted"
fetched fetched-held 'deps packages/app -test ./...' "$TEST_X"
check "exits 0" "$(is "$status" 0)"
check "counts it" "$(contains "$out" 'each of the 1 offline go commands the steps of required Go jobs run loads only packages a fetch before it in its job names (0 say')"

echo "case: a go step whose packages no deps line holds is refused, naming the line to add"
fetched fetched-short 'deps packages/app -test ./other/' "$TEST_X"
check "fails" "$(is "$status" 1)"
check "names the job, the step, the command and the line" \
  "$(contains "$out" 'job lint, step "Test the app", runs `go test -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it in the job names them: add `deps packages/app -test ./internal/x`')"

echo "case: a go test needs the line's -test"
fetched fetched-no-test 'deps packages/app ./...' "$TEST_X"
check "fails" "$(is "$status" 1)"
check "names the line with -test" "$(contains "$out" 'add `deps packages/app -test ./internal/x`')"

echo "case: a go step's tags must be the line's"
VET_LIVE='      - name: Vet the live tests
        run: go vet -tags live ./...
        working-directory: packages/app'
fetched fetched-tags-other 'deps packages/app -test ./...' "$VET_LIVE"
check "untagged line: fails" "$(is "$status" 1)"
check "names the tagged line to add" "$(contains "$out" 'add `deps packages/app -test -tags live ./...`')"
fetched fetched-tags-same 'deps packages/app -test -tags live ./...' "$VET_LIVE"
check "the same tags: exits 0" "$(is "$status" 0)"

echo "case: a go step's GOOS must be the line's"
VET_DARWIN='      - name: Vet for darwin
        run: GOOS=darwin go vet ./...
        working-directory: packages/app'
fetched fetched-goos-other 'deps packages/app -test ./...' "$VET_DARWIN"
check "a line for this platform: fails" "$(is "$status" 1)"
check "names the darwin line to add" "$(contains "$out" 'add `GOOS=darwin deps packages/app -test ./...`')"
fetched fetched-goos-same 'GOOS=darwin deps packages/app -test ./...' "$VET_DARWIN"
check "a darwin line: exits 0" "$(is "$status" 0)"

echo "case: a /... pattern ends at a nested module, as go's does"
fetched fetched-nested 'deps packages/app -test ./...' '      - name: Test the nested module
        run: go test ./...
        working-directory: packages/app/sub'
check "with no go.mod there, the parent line holds it: exits 0" "$(is "$status" 0)"
root=$(fixture fetched-nested-module)
printf 'module example.com/app\n\ngo 1.21\n' | put "$root" packages/app/go.mod
printf 'module example.com/sub\n\ngo 1.21\n' | put "$root" packages/app/sub/go.mod
printf 'deps packages/app -test ./...\n' | put "$root" .github/go-fetch/lint.txt
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Restore the module cache
        uses: actions/cache/restore@v4
        with:
          path: ~/go/pkg/mod
          key: go-app
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh file .github/go-fetch/lint.txt
      - name: Test the nested module
        run: go test ./...
        working-directory: packages/app/sub
EOF
run_check "$root"
check "with its own go.mod, it does not: fails" "$(is "$status" 1)"
check "names the nested module's line" "$(contains "$out" 'add `deps packages/app/sub -test ./...`')"

echo "case: a fetch after the go step holds nothing for it"
root=$(fixture fetched-after)
printf 'module example.com/app\n\ngo 1.21\n' | put "$root" packages/app/go.mod
printf 'deps packages/app -test ./...\n' | put "$root" .github/go-fetch/lint.txt
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Restore the module cache
        uses: actions/cache/restore@v4
        with:
          path: ~/go/pkg/mod
          key: go-app
      - name: Test the app
        run: go test -count=1 ./internal/x/
        working-directory: packages/app
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh file .github/go-fetch/lint.txt
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step before the fetch" "$(contains "$out" 'job lint, step "Test the app", runs `go test -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it')"

echo "case: a go step this cannot read is refused, and passes with a reason that names its fetch"
fetched fetched-unread 'deps packages/app -test ./...' '      - name: Test one package
        run: go test -count=1 "$PKG"
        working-directory: packages/app'
check "fails" "$(is "$status" 1)"
check "names the command it cannot read" \
  "$(contains "$out" 'job lint, step "Test one package", runs `go test -count=1 \$PKG`, whose packages, tags, directory or GOOS, GOARCH, CGO_ENABLED or GOWORK this rule cannot read')"
fetched fetched-unread-reason 'deps packages/app -test ./...' '      # go-offline: PKG is one package under packages/app, which the deps line of lint.txt holds.
      - name: Test one package
        run: go test -count=1 "$PKG"
        working-directory: packages/app'
check "with a reason above its step's name: exits 0" "$(is "$status" 0)"
check "counted as one that says why" "$(contains "$out" 'loads only packages a fetch before it in its job names (1 say which fetch covers them')"
fetched fetched-short-reason 'deps packages/app -test ./other/' '      - name: Test the app
        run: |
          # go-offline: internal/x imports nothing outside the standard library.
          go test -count=1 ./internal/x/
        working-directory: packages/app'
check "a command no line holds, with a reason on the line above it: exits 0" "$(is "$status" 0)"

echo "case: a step that sends its go online on purpose is not held to the fetch"
fetched fetched-online 'deps packages/app -test ./other/' '      # go-offline: the pack goes online as the release does, and runs no test.
      - name: Pack the app
        env:
          GOPROXY: https://proxy.golang.org
        run: go build -o out/app ./cmd/app
        working-directory: packages/app'
check "exits 0" "$(is "$status" 0)"
check "and counts nothing of it, held or reasoned" "$(held 0 0)"

echo "case: a cd before the go command is its directory, and go run's arguments are no packages"
fetched fetched-cd 'deps packages/app -test ./internal/x/
deps packages/app ./cmd/tool' '      - name: Test from the root
        run: |
          cd packages/app && go test -count=1 ./internal/x/
          go run ./cmd/tool check --root ../..'
check "exits 0" "$(is "$status" 0)"
check "counts both" "$(contains "$out" 'each of the 2 offline go commands')"

echo "case: a cd inside a subshell holds for that subshell alone"
fetched fetched-subshell 'deps packages/app -test ./internal/x/' '      - name: Test in a subshell, then outside it
        run: |
          (cd packages/app && go test -count=1 ./internal/x/)
          go test -count=1 ./internal/x/'
check "fails" "$(is "$status" 1)"
check "names the command after the subshell, run from the root" \
  "$(contains "$out" 'step "Test in a subshell, then outside it", runs `go test -count=1 ./internal/x/`, which loads internal/x, and no fetch before it')"
check "and only it, the one inside the subshell held" "$(contains "$out" '^1 problems over')"

echo "case: a race step's fetch line cut to one of its packages is refused, naming the rest"
fetched fetched-cut 'deps packages/app -test ./b/' '      - name: Race the app
        run: go test -count=1 -race ./a/... ./b/
        working-directory: packages/app'
check "fails" "$(is "$status" 1)"
check "names the packages the line lacks" "$(contains "$out" 'which loads packages/app/a/... packages/app/b, and no fetch before it in the job names them')"

# Each form the rule's header says it reads, alone: a command no deps line holds is refused by
# name, and the same command beside the line that holds it passes and is counted, so a reading
# that loses the form fails here.
echo "case: go -C DIR runs in DIR"
C_TEST='      - name: Test the app from the root
        run: go -C packages/app test -count=1 ./internal/x/'
fetched fetched-dash-c 'deps packages/app -test ./other/' "$C_TEST"
check "a line for another package: fails" "$(is "$status" 1)"
check "names the package under DIR and the line to add" \
  "$(contains "$out" 'runs `go -C packages/app test -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it in the job names them: add `deps packages/app -test ./internal/x`')"
fetched fetched-dash-c-held 'deps packages/app -test ./internal/x/' "$C_TEST"
check "the line for it: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"

echo "case: go -C=DIR runs in DIR"
C_EQ_TEST='      - name: Test the app from the root
        run: go -C=packages/app test -count=1 ./internal/x/'
fetched fetched-dash-c-eq 'deps packages/app -test ./other/' "$C_EQ_TEST"
check "a line for another package: fails" "$(is "$status" 1)"
check "names the package under DIR" \
  "$(contains "$out" 'runs `go -C=packages/app test -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it')"
fetched fetched-dash-c-eq-held 'deps packages/app -test ./internal/x/' "$C_EQ_TEST"
check "the line for it: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"

echo "case: go list -m reads the module graph, which no deps line can hold"
# No operand: `all` would be refused as an import path even if -m were lost.
fetched fetched-list-m 'deps packages/app -test ./...' '      - name: List the module updates
        run: go list -m -u
        working-directory: packages/app'
check "fails beside a line that holds every package" "$(is "$status" 1)"
check "names it as a command this rule cannot read" "$(unread 'go list -m -u')"

echo "case: a command that names an import path is one no deps line can hold"
fetched fetched-import-path 'deps packages/app -test ./...' '      - name: Run a tool at a version
        run: go run golang.org/x/tools/cmd/stringer@v0.30.0 -help
        working-directory: packages/app'
check "fails beside a line that holds every package" "$(is "$status" 1)"
check "names it as a command this rule cannot read" "$(unread 'go run golang.org/x/tools/cmd/stringer@v0.30.0 -help')"

echo "case: a command that names no package loads its directory"
BUILD_HERE='      - name: Build the tool
        run: go build
        working-directory: packages/app/cmd/tool'
fetched fetched-no-package 'deps packages/app ./internal/x/' "$BUILD_HERE"
check "a line for another package: fails" "$(is "$status" 1)"
check "names the directory and the line to add" \
  "$(contains "$out" 'runs `go build`, which loads packages/app/cmd/tool, and no fetch before it in the job names them: add `deps packages/app ./cmd/tool`')"
fetched fetched-no-package-held 'deps packages/app ./cmd/tool' "$BUILD_HERE"
check "the line for it: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"

echo "case: -tags=T is the command's tags, as -tags T is"
VET_LIVE_EQ='      - name: Vet the live tests
        run: go vet -tags=live ./...
        working-directory: packages/app'
fetched fetched-tags-eq-other 'deps packages/app -test ./...' "$VET_LIVE_EQ"
check "an untagged line: fails" "$(is "$status" 1)"
check "names the tagged line to add" "$(contains "$out" 'add `deps packages/app -test -tags live ./...`')"
fetched fetched-tags-eq-same 'deps packages/app -test -tags live ./...' "$VET_LIVE_EQ"
check "the same tags: exits 0" "$(is "$status" 0)"

echo "case: a setting of the step's env is the command's"
VET_STEP_DARWIN='      - name: Vet for darwin
        env:
          GOOS: darwin
        run: go vet ./...
        working-directory: packages/app'
fetched fetched-step-env 'deps packages/app -test ./...' "$VET_STEP_DARWIN"
check "a line for this platform: fails" "$(is "$status" 1)"
check "names the darwin line to add" "$(contains "$out" 'add `GOOS=darwin deps packages/app -test ./...`')"
fetched fetched-step-env-held 'GOOS=darwin deps packages/app -test ./...' "$VET_STEP_DARWIN"
check "a darwin line: exits 0" "$(is "$status" 0)"

BUILD_TOOL='      - name: Build the tool
        run: go build ./cmd/tool
        working-directory: packages/app'
echo "case: a setting of the job's env is the command's"
fetched_with fetched-job-env 'deps packages/app ./cmd/tool' '' '    env:
      GOARCH: arm64' "$BUILD_TOOL"
check "a line for this architecture: fails" "$(is "$status" 1)"
check "names the arm64 line to add" "$(contains "$out" 'add `GOARCH=arm64 deps packages/app ./cmd/tool`')"
fetched_with fetched-job-env-held 'GOARCH=arm64 deps packages/app ./cmd/tool' '' '    env:
      GOARCH: arm64' "$BUILD_TOOL"
check "an arm64 line: exits 0" "$(is "$status" 0)"

echo "case: a setting of the workflow's env is the command's"
fetched_with fetched-workflow-env 'deps packages/app ./cmd/tool' '  CGO_ENABLED: "0"' '' "$BUILD_TOOL"
check "a line with cgo: fails" "$(is "$status" 1)"
check "names the line without cgo to add" "$(contains "$out" 'add `CGO_ENABLED=0 deps packages/app ./cmd/tool`')"
fetched_with fetched-workflow-env-held 'CGO_ENABLED=0 deps packages/app ./cmd/tool' '  CGO_ENABLED: "0"' '' "$BUILD_TOOL"
check "a line without cgo: exits 0" "$(is "$status" 0)"

echo "case: GOARCH, CGO_ENABLED and GOWORK on the command's own line are its settings"
fetched fetched-goarch-line 'deps packages/app ./cmd/tool' '      - name: Build the tool for arm64
        run: GOARCH=arm64 go build ./cmd/tool
        working-directory: packages/app'
check "GOARCH, a line for this architecture: fails" "$(is "$status" 1)"
check "names the arm64 line to add" "$(contains "$out" 'add `GOARCH=arm64 deps packages/app ./cmd/tool`')"
fetched fetched-cgo-line 'deps packages/app ./cmd/tool' '      - name: Build the tool without cgo
        run: CGO_ENABLED=0 go build ./cmd/tool
        working-directory: packages/app'
check "CGO_ENABLED, a line with cgo: fails" "$(is "$status" 1)"
check "names the line without cgo to add" "$(contains "$out" 'add `CGO_ENABLED=0 deps packages/app ./cmd/tool`')"
GOWORK_TEST='      - name: Test the module alone
        run: GOWORK=off go test -count=1 ./internal/x/
        working-directory: packages/app'
fetched fetched-gowork-line 'deps packages/app -test ./internal/x/' "$GOWORK_TEST"
check "GOWORK, a line in the workspace: fails" "$(is "$status" 1)"
check "names the line outside the workspace to add" "$(contains "$out" 'add `GOWORK=off deps packages/app -test ./internal/x`')"
fetched fetched-gowork-line-held 'GOWORK=off deps packages/app -test ./internal/x/' "$GOWORK_TEST"
check "a GOWORK=off line: exits 0" "$(is "$status" 0)"

echo "case: the job's and the workflow's defaults.run working-directory is the command's directory"
TEST_X_HERE='      - name: Test the app
        run: go test -count=1 ./internal/x/'
fetched_with fetched-job-defaults 'deps packages/app -test ./internal/x/' '' '    defaults:
      run:
        working-directory: packages/app' "$TEST_X_HERE"
check "the job's: the line for the package there: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"
fetched_with fetched-job-defaults-short 'deps packages/app -test ./other/' '' '    defaults:
      run:
        working-directory: packages/app' "$TEST_X_HERE"
check "the job's: a line for another package: fails, naming the package there" \
  "$(contains "$out" 'which loads packages/app/internal/x, and no fetch before it')"
fetched_with fetched-workflow-defaults 'deps packages/app -test ./internal/x/' 'defaults:
  run:
    working-directory: packages/app' '' "$TEST_X_HERE"
check "the workflow's: the line for the package there: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"

echo "case: go test -C DIR, the -C as the subcommand's first flag, runs in DIR"
C_AFTER_TEST='      - name: Test the app from the root
        run: go test -C packages/app -count=1 ./internal/x/'
fetched fetched-dash-c-after 'deps packages/app -test ./other/' "$C_AFTER_TEST"
check "a line for another package: fails" "$(is "$status" 1)"
check "names the package under DIR" \
  "$(contains "$out" 'runs `go test -C packages/app -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it')"
fetched fetched-dash-c-after-held 'deps packages/app -test ./internal/x/' "$C_AFTER_TEST"
check "the line for it: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"

# The environment a go command reads its settings from, as rule 1 reads a shell's: an export
# before it, env's words, and GOFLAGS, whose -tags go reads where the command gives none.
echo "case: a setting exported before the go command is its setting"
VET_EXPORTED='      - name: Vet for darwin
        run: |
          export GOOS=darwin
          go vet ./...
        working-directory: packages/app'
fetched fetched-export 'deps packages/app -test ./...' "$VET_EXPORTED"
check "a line for this platform: fails" "$(is "$status" 1)"
check "names the darwin line to add" "$(contains "$out" 'add `GOOS=darwin deps packages/app -test ./...`')"
fetched fetched-export-held 'GOOS=darwin deps packages/app -test ./...' "$VET_EXPORTED"
check "a darwin line: exits 0" "$(is "$status" 0)"
fetched fetched-export-subshell 'deps packages/app -test ./...' '      - name: Vet for darwin in a subshell, then for this platform
        run: |
          (export GOOS=darwin; true)
          go vet ./...
        working-directory: packages/app'
check "an export inside a subshell holds for that subshell alone: exits 0" "$(is "$status" 0)"

echo "case: a setting env gives the go command is its setting"
fetched fetched-env-words 'deps packages/app -test ./...' '      - name: Vet for darwin
        run: env GOOS=darwin go vet ./...
        working-directory: packages/app'
check "a line for this platform: fails" "$(is "$status" 1)"
check "names the darwin line to add" "$(contains "$out" 'add `GOOS=darwin deps packages/app -test ./...`')"

echo "case: the -tags of GOFLAGS are the command's tags, where it gives none"
fetched fetched-goflags-tags 'deps packages/app -test ./...' "      - name: Vet the live tests
        run: GOFLAGS='-mod=readonly -tags=live' go vet ./...
        working-directory: packages/app"
check "an untagged line: fails" "$(is "$status" 1)"
check "names the tagged line to add" "$(contains "$out" 'add `deps packages/app -test -tags live ./...`')"
fetched fetched-goflags-own-tags 'deps packages/app -test -tags other ./...' "      - name: Vet the other tests
        run: GOFLAGS='-mod=readonly -tags=live' go vet -tags other ./...
        working-directory: packages/app"
check "the command's own -tags over GOFLAGS': exits 0" "$(is "$status" 0)"
fetched fetched-goflags-unread 'deps packages/app -test ./...' '      - name: Vet with the flags given
        run: GOFLAGS="$FLAGS" go vet ./...
        working-directory: packages/app'
check "a GOFLAGS that is not literal: fails" "$(is "$status" 1)"
check "names it as a command this rule cannot read" "$(unread 'go vet ./...')"

echo "case: a go command a shell's -c or a pipe hands to a shell is read"
fetched fetched-shell-c 'deps packages/app -test ./other/' "      - name: Test through bash -c
        run: bash -c 'go test -count=1 ./internal/x/'
        working-directory: packages/app"
check "bash -c: fails" "$(is "$status" 1)"
check "names the command bash -c runs" \
  "$(contains "$out" 'step "Test through bash -c", runs `go test -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it')"
fetched fetched-shell-pipe 'deps packages/app -test ./other/' "      - name: Test through a pipe
        run: echo 'go test -count=1 ./internal/x/' | bash
        working-directory: packages/app"
check "echo | bash: fails" "$(is "$status" 1)"
check "names the command the pipe feeds bash" \
  "$(contains "$out" 'step "Test through a pipe", runs `go test -count=1 ./internal/x/`, which loads packages/app/internal/x, and no fetch before it')"
fetched fetched-shell-c-held 'deps packages/app -test ./internal/x/' "      - name: Test through bash -c
        run: bash -c 'cd ../app && go test -count=1 ./internal/x/'
        working-directory: packages/app"
check "bash -c, from the step's directory and its own cd, beside its line: exits 0" "$(is "$status" 0)"
check "and counts it" "$(held 1 0)"

echo "case: a setting or a working-directory that is a \${{ }} expression is one this rule cannot read"
fetched fetched-expr-setting 'deps packages/app ./cmd/tool' '      - name: Build the tool for each architecture
        env:
          GOARCH: ${{ matrix.arch }}
        run: go build ./cmd/tool
        working-directory: packages/app'
check "a setting: fails" "$(is "$status" 1)"
check "names it as a command this rule cannot read" "$(unread 'go build ./cmd/tool')"
check "and asks for no fetch line of the expression" "$(is "$(contains "$out" 'add `GOARCH=\${{')" false)"
fetched fetched-expr-directory 'deps packages/app -test ./internal/x/' '      - name: Test each module
        run: go test -count=1 ./internal/x/
        working-directory: ${{ matrix.module }}'
check "a working-directory: fails" "$(is "$status" 1)"
check "names it as a command this rule cannot read" "$(unread 'go test -count=1 ./internal/x/')"
check "and asks for no fetch line of the expression" "$(is "$(contains "$out" 'matrix.module }}/internal/x')" false)"
