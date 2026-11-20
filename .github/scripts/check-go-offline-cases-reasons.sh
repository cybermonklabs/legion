# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's file reasons: where a go-offline-file reason is refused, and where
# it covers its file's go commands. Sourced by check-go-offline.test.sh, whose fixtures, run_check
# and counters they use; not run on its own.

echo "case: a go-offline-file reason in a test file is refused, and its command with it"
root=$(fixture file-reason-test)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses the reason, saying why" \
  "$(contains "$out" 'tools/deps.test.sh:1: a go-offline-file reason, which this file may not carry: it is a test file')"
check "and refuses the command it would cover" "$(contains "$out" 'tools/deps.test.sh:3: a go command with no GOPROXY=off')"

echo "case: a go-offline-file reason in a file ci.yaml names is refused"
root=$(fixture file-reason-ci)
put "$root" tools/list-deps.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
printf '      - run: bash tools/list-deps.sh\n' >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses the reason, naming ci.yaml" \
  "$(contains "$out" 'tools/list-deps.sh:1: a go-offline-file reason, which this file may not carry: a required workflow names it (.github/workflows/ci.yaml)')"
check "and refuses the command" "$(contains "$out" 'tools/list-deps.sh:3: a go command with no GOPROXY=off')"

echo "case: a go-offline-file reason in a file a workflow ci.yaml calls names is refused"
root=$(fixture file-reason-called)
put "$root" tools/list-deps.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
printf '  checks:\n    uses: ./.github/workflows/ci-job-checks.yaml\n' >>"$root/.github/workflows/ci.yaml"
put "$root" .github/workflows/ci-job-checks.yaml <<'EOF'
name: CI job checks
on: workflow_call
jobs:
  checks:
    runs-on: ubuntu-24.04
    steps:
      - run: tools/list-deps.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses the reason, naming the called workflow" \
  "$(contains "$out" 'a required workflow names it (.github/workflows/ci-job-checks.yaml)')"

echo "case: a go-offline-file reason in a file only a workflow outside the required set, and a comment of ci.yaml, name passes"
root=$(fixture file-reason-allowed)
put "$root" tools/list-deps.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
printf '      # tools/list-deps.sh is a person'"'"'s.\n' >>"$root/.github/workflows/ci.yaml"
nightly "$root" <<'EOF'
      - run: bash tools/list-deps.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as covered by its file's reason" "$(contains "$out" 'is in a file whose reason covers it (1)')"

# file_reason_at <name> <line>: a fixture whose tools/list-deps.sh holds its file reason on <line>,
# comment lines above it, and one go command after it, run through the check.
file_reason_at() {
  root=$(fixture "$1")
  {
    echo '#!/usr/bin/env bash'
    for ((n = 2; n < $2; n++)); do echo "# line $n"; done
    echo '# go-offline-file: a person runs this by hand.'
    echo 'go list -deps ./... >/dev/null'
  } | put "$root" tools/list-deps.sh
  run_check "$root"
}

echo "case: a file reason counts within the file's first 40 lines, and not on line 41"
file_reason_at file-reason-line-40 40
check "on line 40: exits 0" "$(is "$status" 0)"
check "on line 40: covers the command" "$(contains "$out" 'is in a file whose reason covers it (1)')"
file_reason_at file-reason-line-41 41
refused tools/list-deps.sh 42

# reached_reason <name> <workflow>: a fixture whose tools/inner.sh holds a file reason and a go
# command, run by tools/outer.sh, which a step of <workflow> runs ("ci" adds it to ci.yaml,
# "nightly" to a workflow outside the required set); run through the check.
reached_reason() {
  root=$(fixture "$1")
  put "$root" tools/inner.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand; no CI job runs it.
go list -deps ./... >/dev/null
EOF
  printf '#!/usr/bin/env bash\nbash "$(dirname "$0")/inner.sh"\n' | put "$root" tools/outer.sh
  if [ "$2" = ci ]; then
    printf '      - name: List the modules\n        run: bash tools/outer.sh\n' >>"$root/.github/workflows/ci.yaml"
  else
    printf '      - name: List the modules\n        run: bash tools/outer.sh\n' | nightly "$root"
  fi
  run_check "$root"
}

echo "case: a go-offline-file reason in a script a required job reaches through another script is refused"
reached_reason file-reason-reached ci
check "fails" "$(is "$status" 1)"
check "refuses the reason, naming the step that reaches it" \
  "$(contains "$out" 'tools/inner.sh:1: a go-offline-file reason, which this file may not carry: a step of a required workflow reaches it (.github/workflows/ci.yaml job lint, step "List the modules")')"
check "and refuses the command it would cover" "$(contains "$out" 'tools/inner.sh:3: a go command with no GOPROXY=off')"
reached_reason file-reason-reached-outside nightly
check "reached only from a workflow outside the required set: exits 0" "$(is "$status" 0)"
check "and its reason covers the command" "$(contains "$out" 'is in a file whose reason covers it (1)')"

echo "case: a go-offline-file reason in a script a required step sources is refused"
root=$(fixture file-reason-sourced)
put "$root" tools/lib.sh <<'EOF'
# go-offline-file: a person sources this by hand.
go list -deps ./... >/dev/null
EOF
# ci.yaml names tools/setup.sh alone, which sources the library.
printf '#!/usr/bin/env bash\n. "$(dirname "$0")/lib.sh"\n' | put "$root" tools/setup.sh
printf '      - name: Source the library\n        run: bash tools/setup.sh\n' >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "refuses the reason" \
  "$(contains "$out" 'tools/lib.sh:1: a go-offline-file reason, which this file may not carry: a step of a required workflow reaches it')"

echo "case: a script a required step reaches only by a guess from a variable, or that a TypeScript test names, is not held"
root=$(fixture file-reason-guessed)
put "$root" tools/inner.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand; no CI job runs it.
go list -deps ./... >/dev/null
EOF
# The walk reads the sourced path as a guess, tools/*.sh, since $TOOL is set where it cannot see.
printf '      - name: Source one of the tools\n        run: . "tools/$TOOL.sh"\n' >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "a script guessed from a variable's name: exits 0" "$(is "$status" 0)"
check "a script guessed from a variable's name: its reason covers the command" \
  "$(contains "$out" 'is in a file whose reason covers it (1)')"
root=$(fixture file-reason-ts-named)
put "$root" packages/web/scripts/inner.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand; no CI job runs it.
go list -deps ./... >/dev/null
EOF
printf 'test("reads the script", () => { void Bun.file(`${import.meta.dir}/../scripts/inner.sh`).text(); });\n' |
  put "$root" packages/web/e2e/names.test.ts
printf '      - name: Test the web package\n        run: bun test e2e/\n        working-directory: packages/web\n' \
  >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "a script a TypeScript test names in a string: exits 0" "$(is "$status" 0)"
check "a script a TypeScript test names in a string: its reason covers the command" \
  "$(contains "$out" 'is in a file whose reason covers it (1)')"

echo "case: a go-offline-file reason in a program a required job runs, or in TypeScript a required test run loads, is refused"
root=$(fixture file-reason-program)
put "$root" tools/list_deps.py <<'EOF'
# go-offline-file: a person runs this by hand.
import subprocess
subprocess.run(["go", "list", "-deps", "./..."])
EOF
# ci.yaml names tools/run.sh alone, which hands the program to python3.
printf '#!/usr/bin/env bash\npython3 "$(dirname "$0")/list_deps.py"\n' | put "$root" tools/run.sh
printf '      - name: List the modules\n        run: bash tools/run.sh\n' >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "a python3 program: refused" \
  "$(contains "$out" 'tools/list_deps.py:1: a go-offline-file reason, which this file may not carry: a step of a required workflow reaches it')"
root=$(fixture file-reason-test-run)
put "$root" packages/web/src/build.ts <<'EOF'
// go-offline-file: a person runs this by hand.
export const build = () => Bun.spawnSync(["go", "build", "./..."]);
EOF
printf '      - name: Test the web package\n        run: bun test\n        working-directory: packages/web\n' \
  >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "TypeScript under a bun test run's directory: refused" \
  "$(contains "$out" 'packages/web/src/build.ts:1: a go-offline-file reason, which this file may not carry: a step of a required workflow reaches it')"

# outside_reason <name> <listed command>: a fixture whose tools/build.sh holds this harness's stdin
# and whose .github/go-offline-reasons.yaml gives tools/build.sh's <listed command> a reason; run.
outside_reason() {
  root=$(fixture "$1")
  put "$root" tools/build.sh
  printf -- '- path: tools/build.sh\n  command: %s\n  reason: a person builds this by hand.\n' "$2" |
    put "$root" .github/go-offline-reasons.yaml
  run_check "$root"
}

echo "case: an entry of .github/go-offline-reasons.yaml covers the site it names, counted as a reason"
outside_reason outside-listed 'go build ./cmd/app' <<'EOF'
#!/usr/bin/env bash
  go build ./cmd/app
EOF
check "exits 0" "$(is "$status" 0)"
check "counts it as a reason" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a listed site whose command changed is refused, and so is the entry, which names no site"
outside_reason outside-changed 'go build ./cmd/app' <<'EOF'
#!/usr/bin/env bash
go build ./cmd/other
EOF
refused tools/build.sh 2
check "refuses the entry at its line" \
  "$(contains "$out" '.github/go-offline-reasons.yaml:1: the reason for tools/build.sh names `go build ./cmd/app`, which is no go command of that file that needs a reason')"

echo "case: a site the list does not name, in a file it names, is refused"
outside_reason outside-unlisted 'go build ./cmd/app' <<'EOF'
#!/usr/bin/env bash
go build ./cmd/app
go list -deps ./...
EOF
refused tools/build.sh 3
check "and that site alone, over the two" "$(contains "$out" '^1 problems over the 2 go commands')"

echo "case: an entry with no reason is refused"
root=$(fixture outside-malformed)
printf '#!/usr/bin/env bash\ngo build ./cmd/app\n' | put "$root" tools/build.sh
printf -- '- path: tools/build.sh\n  command: go build ./cmd/app\n' | put "$root" .github/go-offline-reasons.yaml
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry" \
  "$(contains "$out" '.github/go-offline-reasons.yaml:1: an entry that is no {path, command, reason}, each one set')"
