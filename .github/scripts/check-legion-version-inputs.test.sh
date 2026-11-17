#!/usr/bin/env bash
# Tests for `check-legion-version-inputs.sh`: release.yaml's LEGION_GO_INPUTS block is what
# `go list -deps ./cmd/legion` names beyond $LEGION_VERSION_ARGS's paths, the version step passes
# it, the jobs that carry each release decision are pinned as release_version_inputs.py names
# them, and release-envoy-listener.yaml's $LEGION_ENVOY_VERSION_ARGS covers what go names for the
# archives that workflow builds.
#
# Each case builds a fixture, a miniature repository under a temporary directory, with
# check-legion-version-inputs-fixtures.sh, and changes one piece of it. No module of a fixture
# requires anything outside it, so its go commands load every package with nothing downloaded.
# This file is the one entry point, and runs every case: those of what decides whether a release
# job runs (its needs:, its own if:, the triggers and the push paths) sit in
# check-legion-version-inputs-run-cases.sh, which it sources where they run.
#
# Run from anywhere: .github/scripts/check-legion-version-inputs.test.sh
# CI runs it in ci.yaml's test job, after its setup-go.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'proxy_recorders_stop; rm -rf "$work"' EXIT

# Each library sits beside this file; the directives name it by SCRIPTDIR, so shellcheck follows
# each one from wherever it runs.
# shellcheck source=SCRIPTDIR/test-lib.sh
source "$script_dir/test-lib.sh"
# shellcheck source=SCRIPTDIR/go-offline-test-lib.sh
source "$script_dir/go-offline-test-lib.sh"

# shellcheck source=SCRIPTDIR/check-legion-version-inputs-fixtures.sh
source "$script_dir/check-legion-version-inputs-fixtures.sh"

echo "case: write fills the block, and the check passes it"
root=$(fixture green)
run_check "$root" write
check "write exits 0" "$(is "$status" 0)"
check "says what it wrote" "$(contains "$out" "wrote .github/workflows/release.yaml's LEGION_GO_INPUTS: 7 paths")"
text=$(release "$root")
for path in go.work go.work.sum packages/envoy/alpha/ packages/envoy/beta/ packages/envoy/go.mod \
  packages/envoy/go.sum packages/envoy/internal/delta/; do
  check "the block names $path" "$(contains "$text" "^    $path\$")"
done
check "the block leaves out the package nothing links" "$(is "$(contains "$text" 'packages/envoy/gamma/')" false)"
check "the block leaves out what packages/daemon/ covers" "$(is "$(contains "$text" '^    packages/daemon/')" false)"
check "the block is a folded value" "$(contains "$text" '^  LEGION_GO_INPUTS: >-$')"
run_check "$root"
check "check exits 0" "$(is "$status" 0)"
check "says what it compared" \
  "$(contains "$out" '3 package directories and 4 module files, over linux/amd64, darwin/arm64')"
check "says what the listener's paths cover" \
  "$(contains "$out" "release-envoy-listener.yaml's LEGION_ENVOY_VERSION_ARGS covers what go decides the archives from: 5 package directories and 6 module files, over 4 listings")"
run_check "$root" write
check "a second write exits 0" "$(is "$status" 0)"
check "and changes nothing" "$(contains "$out" 'already holds what go names: 7 paths')"

echo "case: a package taken out of the block"
root=$(written dropped)
replace "$root/.github/workflows/release.yaml" $'    packages/envoy/internal/delta/\n' ''
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package and the fix" \
  "$(contains "$out" 'it lacks packages/envoy/internal/delta/, which go names; run `.github/scripts/check-legion-version-inputs.sh write`')"
check "points at the block, line 11 below the fixture's on: and env: heads" \
  "$(contains "$out" '::error file=.github/workflows/release.yaml,line=11::')"

echo "case: a package no target links, added to the block"
root=$(written added)
replace "$root/.github/workflows/release.yaml" '    packages/envoy/go.mod' $'    packages/envoy/gamma/\n    packages/envoy/go.mod'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'it names packages/envoy/gamma/, which go does not')"

echo "case: cmd/legion imports a package the block does not name"
root=$(written imported)
printf 'package main\n\nimport _ "example.com/envoy/gamma"\n' > "$root/packages/daemon/cmd/legion/gamma.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package" "$(contains "$out" 'it lacks packages/envoy/gamma/, which go names')"
run_check "$root" write
check "write exits 0" "$(is "$status" 0)"
check "write adds it" "$(contains "$(release "$root")" '^    packages/envoy/gamma/$')"
run_check "$root"
check "the written block passes" "$(is "$status" 0)"

echo "case: a package one target alone links is named for that target"
root=$(written targets)
printf '{"legion": ["linux/amd64"], "envoy": ["amd64"]}\n' > "$root/.github/go-release-targets.json"
run_check "$root"
check "exits 1 when no target is darwin" "$(is "$status" 1)"
check "names the darwin-only package" "$(contains "$out" 'it names packages/envoy/beta/, which go does not')"

echo "case: a path of LEGION_VERSION_ARGS covers what the block would name"
root=$(written covered)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args packages/envoy/"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names what the hand path covers" \
  "$(contains "$out" 'it names packages/envoy/alpha/, packages/envoy/beta/, packages/envoy/go.mod, packages/envoy/go.sum, packages/envoy/internal/delta/, which go does not')"
run_check "$root" write
check "write keeps the workspace files alone" "$(contains "$(release "$root")" $'^    go.work.sum\n  # END generated$')"
check "the written block passes" "$(run_check "$root"; is "$status" 0)"
replace "$root/.github/workflows/release.yaml" "$hand_args packages/envoy/" "$hand_args packages/envoy/ go.work go.work.sum"
run_check "$root" write
check "a block with nothing left is written empty" "$(contains "$(release "$root")" '^  LEGION_GO_INPUTS: ""$')"
run_check "$root"
check "and passes" "$(is "$status" 0)"

# The version step's run: must be release_version_inputs.py's text, character for character, and
# the check names the first line that differs. Most cases below change it so that bash would not
# hand release-push.sh the generated paths; the added line holds the strictness itself, since a
# line that is harmless today is refused all the same.
exact="legion_build's step \`version\`'s run: must be, character for character, the one .github/scripts/release_version_inputs.py names for it"
echo "case: the version step does not pass the block"
root=$(written handoff)
replace "$root/.github/workflows/release.yaml" ' $LEGION_GO_INPUTS >>' ' >>'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the rule" "$(contains "$out" "$exact")"
check "and the line it runs instead" \
  "$(contains "$out" 'its line 2 is `.github/scripts/release-push.sh version legion-v $LEGION_VERSION_ARGS >> "$GITHUB_OUTPUT"` where')"

echo "case: the block's dollar escaped, so bash hands release-push.sh the variable's name"
root=$(written escaped)
replace "$root/.github/workflows/release.yaml" ' $LEGION_GO_INPUTS >>' ' \$LEGION_GO_INPUTS >>'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the escaped line" "$(contains "$out" 'its line 2 is `.github/scripts/release-push.sh version legion-v $LEGION_VERSION_ARGS \\$LEGION_GO_INPUTS')"

echo "case: a \${{ }} line GitHub renders into an assignment that empties the block"
root=$(written expression)
replace "$root/.github/workflows/release.yaml" "          $handoff" "          \${{ 'LEGION_GO_INPUTS=' }}"$'\n'"          $handoff"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the expression line" "$(contains "$out" "its line 2 is \`\${{ 'LEGION_GO_INPUTS=' }}\` where")"

echo "case: a line added after the command"
root=$(written added-line)
replace "$root/.github/workflows/release.yaml" "$handoff >> \"\$GITHUB_OUTPUT\"" "$handoff >> \"\$GITHUB_OUTPUT\""$'\n'"          echo done"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the added line" "$(contains "$out" 'its line 3 is `echo done` where that one.s is nothing')"

echo "case: the listener's release_build step changed"
root=$(written listener-step)
replace "$root/.github/workflows/release-envoy-listener.yaml" ' $LEGION_ENVOY_VERSION_ARGS >> ' ' "$LEGION_ENVOY_VERSION_ARGS" >> '
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names that step" "$(contains "$out" "release_build's step \`version\`'s run: must be, character for character")"

# The job is pinned up to and including the version step: each case below gives the command a
# value in place of the workflow's, or runs code the check does not read before it, one way each.
plan_file=.github/scripts/release_version_inputs.py
env_rule="legion_build's step \`version\`'s env: must be, key for key, the one $plan_file names for it"
before_rule="before legion_build's step \`version\`, is not one of $plan_file's STEPS_BEFORE_VERSION_STEPS"
echo "case: the version step's own env: sets the block's variable"
root=$(written stepenv)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        env:\n          LEGION_GO_INPUTS: ""\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the rule and the key" "$(contains "$out" "$env_rule (nothing), .*it sets LEGION_GO_INPUTS, which that one does not")"

echo "case: the job's env: sets the hand-written variable"
root=$(written jobenv)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: ubuntu-24.04\n    env:\n      LEGION_VERSION_ARGS: -- packages/daemon/\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job's env:" \
  "$(contains "$out" "legion_build sets env: (\`{\"LEGION_VERSION_ARGS\": \"-- packages/daemon/\"}\`), which $plan_file's JOB_KEYS does not list")"

echo "case: the command left in a shell comment"
root=$(written comment)
replace "$root/.github/workflows/release.yaml" "          $handoff" "          # $handoff"$'\n'"          echo skipped"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the commented line" "$(contains "$out" "its line 2 is \`# $handoff")"

echo "case: the block's variable quoted, so its paths are one word"
root=$(written quoted)
replace "$root/.github/workflows/release.yaml" ' $LEGION_GO_INPUTS >>' ' "$LEGION_GO_INPUTS" >>'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "shows the quoted word" "$(contains "$out" 'legion-v $LEGION_VERSION_ARGS "$LEGION_GO_INPUTS" >>')"

echo "case: the run: empties the variable before the command"
root=$(written assigned)
replace "$root/.github/workflows/release.yaml" "          $handoff" "          LEGION_GO_INPUTS="$'\n'"          $handoff"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the assignment" "$(contains "$out" 'its line 2 is `LEGION_GO_INPUTS=` where')"

echo "case: an earlier step of the job writes \$GITHUB_ENV"
root=$(written githubenv)
replace "$root/.github/workflows/release.yaml" $'    steps:\n' $'    steps:\n      - run: echo "LEGION_GO_INPUTS=" >> "$GITHUB_ENV"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the rule" "$(contains "$out" "step 1 of legion_build, $before_rule")"
check "and says it is no listed command" \
  "$(contains "$out" 'its run: is no listed command (its line 1 is `echo "LEGION_GO_INPUTS=" >> "$GITHUB_ENV"`)')"

echo "case: the version step runs under another shell"
root=$(written shell)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        shell: python\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the shell" "$(contains "$out" "its own shell: is \`python\`, where legion_build's step \`version\` must run under \`bash\` itself")"

# A file of shell a shell sources before the run:, and a shell line other than `bash` itself, can
# each set the variables in place of the workflow's: version-startup.sh below is the file each case
# names, and it empties the block.
startup='LEGION_GO_INPUTS='
echo "case: the workflow's env: sets BASH_ENV"
root=$(written bashenv)
printf '%s\n' "$startup" > "$root/version-startup.sh"
replace "$root/.github/workflows/release.yaml" $'env:\n  LEGION_VERSION_ARGS' $'env:\n  BASH_ENV: ./version-startup.sh\n  LEGION_VERSION_ARGS'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the variable and the list" \
  "$(contains "$out" ".github/workflows/release.yaml's workflow env: sets BASH_ENV, which $plan_file's WORKFLOW_ENV_KEYS does not list for it")"

echo "case: the job's env: sets ENV"
root=$(written jobenv-env)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: ubuntu-24.04\n    env:\n      ENV: ./version-startup.sh\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job's env:" "$(contains "$out" "legion_build sets env: (\`{\"ENV\": \"./version-startup.sh\"}\`), which")"

echo "case: the listener's confirm step's own env: sets BASH_ENV"
root=$(written stepenv-bashenv)
replace "$root/.github/workflows/release-envoy-listener.yaml" "$confirm_version"$'\n' \
  "$confirm_version"$'\n          BASH_ENV: ./version-startup.sh\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the key" \
  "$(contains "$out" "release_publish's step \`confirm\`'s env: must be, key for key, .*it sets BASH_ENV, which that one does not")"

echo "case: the job's defaults.run.shell is a bash that sources a file first"
root=$(written rcfile)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' \
  $'    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: bash --noprofile --rcfile ./version-startup.sh -i {0}\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job's defaults" \
  "$(contains "$out" "legion_build sets defaults: (\`{\"run\": {\"shell\": \"bash --noprofile --rcfile ./version-startup.sh -i {0}\"}}\`), which")"

echo "case: the workflow's defaults.run.shell is sh"
root=$(written workflow-shell)
replace "$root/.github/workflows/release.yaml" $'jobs:\n' $'defaults:\n  run:\n    shell: sh -e {0}\njobs:\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" "the workflow's defaults.run shell: is \`sh -e {0}\`")"

echo "case: a defaults.run.working-directory runs the relative release-push.sh from another tree"
root=$(written working-directory)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' \
  $'    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        working-directory: elsewhere\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the defaults" "$(contains "$out" "legion_build sets defaults: (\`{\"run\": {\"working-directory\": \"elsewhere\"}}\`), which")"
root=$(written step-working-directory)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        working-directory: elsewhere\n'
run_check "$root"
check "exits 1 on the step's own" "$(is "$status" 1)"
check "names the key" "$(contains "$out" "legion_build's step \`version\` sets working-directory: (\`elsewhere\`), which a checked version step does not")"

echo "case: an earlier step of the job writes \$GITHUB_PATH"
root=$(written githubpath)
replace "$root/.github/workflows/release.yaml" $'    steps:\n' $'    steps:\n      - run: echo "$PWD/fake-bin" >> "$GITHUB_PATH"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the rule" "$(contains "$out" "step 1 of legion_build, $before_rule")"

# An action is code the check does not read, and one can write $GITHUB_ENV or $GITHUB_PATH for
# every later step: before a version step only the module's steps run, each exactly as listed.
echo "case: an action the module does not list runs before the version step"
root=$(written unlisted-action)
replace "$root/.github/workflows/release.yaml" $'    steps:\n' $'    steps:\n      - uses: example/setup-environment@v1\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the action and the list" \
  "$(contains "$out" "step 1 of legion_build, $before_rule.*it runs the action \`example/setup-environment@v1\`, which no listed step runs")"

echo "case: a listed action at another pin runs before the listener's confirm step"
root=$(written other-pin)
replace "$root/.github/workflows/release-envoy-listener.yaml" "      - uses: $login_uses" "      - uses: docker/login-action@v3"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "step 3 of release_publish, before release_publish's step \`confirm\`, .*it runs the action \`docker/login-action@v3\`")"

echo "case: the listed checkout with another ref, so the step reads another commit's release-push.sh"
root=$(written checkout-ref)
replace "$root/.github/workflows/release.yaml" $'          persist-credentials: false\n' \
  $'          persist-credentials: false\n          ref: refs/heads/elsewhere\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names its with:" "$(contains "$out" "step 1 of legion_build, $before_rule.*its with: is \`{\"fetch-depth\": 0, \"persist-credentials\": false, \"ref\": \"refs/heads/elsewhere\"}\`")"

echo "case: the version step's job runs on a self-hosted runner"
root=$(written self-hosted)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: [self-hosted, linux]\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the runner and the module's" \
  "$(contains "$out" "legion_build's runs-on: is \`\[\"self-hosted\", \"linux\"\]\`, where $plan_file names \`ubuntu-24.04\` for it")"
root=$(written self-hosted-confirm)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'  release_publish:\n    runs-on: ubuntu-24.04\n' \
  $'  release_publish:\n    runs-on: self-hosted\n'
run_check "$root"
check "exits 1 on the listener's confirm job" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release_publish's runs-on: is \`self-hosted\`")"

echo "case: the version step's job runs in a container"
root=$(written container)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: ubuntu-24.04\n    container: debian:trixie-slim\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key" "$(contains "$out" "legion_build sets container: (\`debian:trixie-slim\`), which $plan_file's JOB_KEYS does not list")"

echo "case: the version step's job calls a reusable workflow"
root=$(written reusable)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: ubuntu-24.04\n    uses: ./.github/workflows/other.yaml\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the workflow" "$(contains "$out" "legion_build sets uses: (\`./.github/workflows/other.yaml\`), which")"

# The round-5 review's class: the version step's decision thrown away after it runs. The publish
# steps read it as needs.<job>.outputs.version and .skip, so an output that is not the step's, or
# a step that does not run or ends with its outputs empty, skips the release the step chose.
echo "case: the job output skip is \"true\" in place of the version step's"
root=$(written output-skip)
replace "$root/.github/workflows/release.yaml" $'      skip: "${{ steps.version.outputs.skip }}"\n' $'      skip: "true"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the output and the module's binding" \
  "$(contains "$out" "legion_build's output skip is \`true\`, where $plan_file names \`\${{ steps.version.outputs.skip }}\` for it")"

echo "case: a build job's notes output set to 'false' (N3b)"
root=$(written output-notes)
replace "$root/.github/workflows/release.yaml" $'      notes: "${{ steps.notes.outputs.notes }}"\n' $'      notes: \'false\'\n'
run_check "$root"
check "exits 1 on legion_build's notes: 'false' (N3b)" "$(is "$status" 1)"
check "names the output and the module's value" \
  "$(contains "$out" "legion_build's output notes is \`false\`, where $plan_file names \`\${{ steps.notes.outputs.notes }}\` for it")"
root=$(written output-notes-listener)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'      notes: "${{ steps.notes.outputs.notes }}"\n' $'      notes: \'false\'\n'
run_check "$root"
check "exits 1 on release_build's notes: 'false'" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release_build's output notes is \`false\`, where")"

echo "case: the listener's job output version is \"0.0.0\""
root=$(written output-version)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'      version: "${{ steps.version.outputs.version }}"\n' $'      version: "0.0.0"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release_build's output version is \`0.0.0\`, where")"
root=$(written outputs-expression)
replace "$root/.github/workflows/release.yaml" $'    outputs:\n      version: "${{ steps.version.outputs.version }}"\n      skip: "${{ steps.version.outputs.skip }}"\n      notes: "${{ steps.notes.outputs.notes }}"\n' \
  $'    outputs: ${{ fromJSON(needs.setup.outputs.decision) }}\n'
run_check "$root"
check "exits 1 on an expression in place of the outputs" "$(is "$status" 1)"
check "names it" "$(contains "$out" "legion_build's outputs: is \`\${{ fromJSON(needs.setup.outputs.decision) }}\`, not a literal mapping")"

echo "case: the version step's if: false"
root=$(written step-if-false)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        if: false\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the if:" "$(contains "$out" "legion_build's step \`version\`'s if: is \`false\`, where $plan_file names nothing for it")"
root=$(written confirm-if)
replace "$root/.github/workflows/release-envoy-listener.yaml" " && needs.release_build.outputs.skip != 'true'\"" " && false\""
run_check "$root"
check "exits 1 on the confirm step's if: changed" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release_publish's step \`confirm\`'s if: is \`needs.release_build.result == 'success' && false\`")"

echo "case: continue-on-error: or timeout-minutes: on a version step"
root=$(written continue-on-error)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'      - id: confirm\n' $'      - id: confirm\n        continue-on-error: true\n'
run_check "$root"
check "exits 1 on continue-on-error" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release_publish's step \`confirm\` sets continue-on-error: (\`true\`), which a checked version step does not")"
root=$(written timeout)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        timeout-minutes: 1\n'
run_check "$root"
check "exits 1 on timeout-minutes" "$(is "$status" 1)"
check "names it" "$(contains "$out" "legion_build's step \`version\` sets timeout-minutes: (\`1\`), which a checked version step does not")"

# The steps after the decision are the same class: a publish step's if: that reads the decision,
# or an earlier reader's outcome, can be edited to skip the release the version step chose. Each
# reader's if: is DECISION_READERS' entry, by id:, name: or position, and a new reader is refused.
readers_rule="$plan_file's DECISION_READERS names"
echo "case: the legion publish step's if: changed to skip (release.yaml's legion_pair)"
root=$(written reader-skip)
replace "$root/.github/workflows/release.yaml" "needs.legion_build.outputs.skip != 'true' }}\"" "false }}\""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the module's if:" \
  "$(contains "$out" "the step with id legion_pair of publish has the if: \`\${{ !cancelled() && steps.pair.outputs.ok == 'true' && needs.legion_build.result == 'success' && false }}\`, where $readers_rule")"

echo "case: a step of a publish chain, an unnamed build step and a confirm reader each skipped"
root=$(written reader-chain)
replace "$root/.github/workflows/release.yaml" "steps.legion_pair.outcome == 'success' }}\"" "false }}\""
run_check "$root"
check "exits 1 on a chained reader" "$(is "$status" 1)"
check "names it" "$(contains "$out" "the step with id legion_download of publish has the if: \`\${{ !cancelled() && false }}\`")"
root=$(written reader-positional)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'      - run: "true"\n        if: "steps.confirm.outputs.release == \'true\'"\n' \
  $'      - run: "true"\n        if: "false"\n'
run_check "$root"
check "exits 1 on a reader the module names by position" "$(is "$status" 1)"
check "names its position" "$(contains "$out" "step 6 of release_publish has the if: \`false\`, where $readers_rule")"
root=$(written reader-gone)
replace "$root/.github/workflows/release.yaml" $'      - name: "Smoke test"\n        if: "steps.version.outputs.skip != \'true\'"\n        run: "true"\n' ''
run_check "$root"
check "exits 1 on a reader taken out" "$(is "$status" 1)"
check "says it is not there" "$(contains "$out" "in .github/workflows/release.yaml's job legion_build, the step \`Smoke test\`, which $readers_rule with the if: .*, is not there")"

echo "case: two listed readers swapped, every if: and id: as it was"
root=$(written reader-order)
swap_steps "$root/.github/workflows/release.yaml" "      - id: legion_pair" "      - id: legion_download"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the two steps and the module's order" \
  "$(contains "$out" "in .github/workflows/release.yaml's job publish, the step with id legion_download is step 2, before the step with id legion_pair, step 3, where $plan_file's DECISION_READERS lists it after")"

# A step whose output a reader's if: reads is listed with the readers: pair, whose ok output
# legion_pair reads, skips the chain with an `if: false` as surely as a reader would.
echo "case: pair, the step legion_pair reads, skipped"
root=$(written pair-skipped)
replace "$root/.github/workflows/release.yaml" $'        if: "${{ !cancelled() }}"\n' $'        if: false\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names pair and the module's if:" \
  "$(contains "$out" "the step with id pair of publish has the if: \`false\`, where $readers_rule \`\${{ !cancelled() }}\` for it")"
root=$(written pair-unlisted)
rm "$root/.github/scripts/release_version_inputs.py"
cp "$script_dir/release_version_inputs.py" "$root/.github/scripts/release_version_inputs.py"
replace "$root/.github/scripts/release_version_inputs.py" $'        Reader("${{ !cancelled() }}", id="pair"),\n' ''
run_check "$root"
check "exits 1 on a module that lists legion_pair without the step it reads" "$(is "$status" 1)"
check "names the step read and the reader" \
  "$(contains "$out" "the step with id legion_pair of publish reads steps.pair, which $plan_file's DECISION_READERS does not list for publish")"

# What decides whether a release job runs at all - the jobs' needs: and own if:, the triggers and
# the push paths - has its cases in a file of its own, sourced here, where they run.
# shellcheck source=SCRIPTDIR/check-legion-version-inputs-run-cases.sh
source "$script_dir/check-legion-version-inputs-run-cases.sh"

echo "case: continue-on-error: on a listed reader, and on pair"
root=$(written reader-continue)
replace "$root/.github/workflows/release.yaml" $'      - id: legion_pair\n' $'      - id: legion_pair\n        continue-on-error: true\n'
run_check "$root"
check "exits 1 on legion_pair" "$(is "$status" 1)"
check "names it" "$(contains "$out" "the step with id legion_pair of publish sets continue-on-error:, which a listed reader does not")"
root=$(written pair-continue)
replace "$root/.github/workflows/release.yaml" $'      - id: pair\n' $'      - id: pair\n        continue-on-error: true\n'
run_check "$root"
check "exits 1 on pair" "$(is "$status" 1)"
check "names it" "$(contains "$out" "the step with id pair of publish sets continue-on-error:")"

# The reader rule reads dotted references, matched without regard to case; an if: of the two
# workflows that reaches the needs or steps context another way is refused outright.
echo "case: a new reader spelled with index syntax, in upper case, or through toJSON"
root=$(written spelled-index)
printf "      - name: Extra\n        if: needs['legion_build'].outputs.skip == 'true'\n        run: echo extra\n" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on index syntax" "$(is "$status" 1)"
check "names it" "$(contains "$out" "the step \`Extra\` of publish has the if: \`needs\['legion_build'\].outputs.skip == 'true'\`, which uses index syntax")"
root=$(written spelled-upper)
printf "      - name: Extra\n        if: NEEDS.LEGION_BUILD.OUTPUTS.SKIP == 'true'\n        run: echo extra\n" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on upper case" "$(is "$status" 1)"
check "reads it as a reader the list does not name" \
  "$(contains "$out" "the step \`Extra\` of publish has the if: \`NEEDS.LEGION_BUILD.OUTPUTS.SKIP == 'true'\`, which reads a checked version step's decision")"
root=$(written spelled-tojson)
printf "      - name: Extra\n        if: fromJSON(toJSON(needs)).legion_build.outputs.skip == 'true'\n        run: echo extra\n" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on a context reached through toJSON" "$(is "$status" 1)"
check "names it" "$(contains "$out" "which uses a needs or steps reference that is not")"
root=$(written spelled-filter)
printf "      - name: Extra\n        if: contains(needs.legion_build.outputs.*, 'true')\n        run: echo extra\n" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on an object filter" "$(is "$status" 1)"
check "names it" "$(contains "$out" "which uses an object filter")"

# GitHub's expression lexer skips whitespace before every token, so a reference broken after a
# dot still reads the decision; and outputs read whole name no output the reader rule can match.
echo "case: a new reader with a newline or a space after a dot, or with outputs read whole (N7c, N7d, N7e)"
root=$(written spelled-newline)
printf '      - name: Extra\n        if: %s\n        run: echo extra\n' '"needs.legion_build.\noutputs.skip == '"'true'"'"' \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on a newline after a dot (N7c)" "$(is "$status" 1)"
check "reads it as a reader the list does not name" \
  "$(contains "$out" "the step \`Extra\` of publish has the if: \`needs.legion_build. outputs.skip == 'true'\`, which reads a checked version step's decision")"
root=$(written spelled-space)
printf "      - name: Extra\n        if: needs.legion_build. outputs.skip == 'true'\n        run: echo extra\n" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on a space after a dot (N7d)" "$(is "$status" 1)"
check "reads it as a reader the list does not name" \
  "$(contains "$out" "the step \`Extra\` of publish has the if: \`needs.legion_build. outputs.skip == 'true'\`, which reads a checked version step's decision")"
root=$(written spelled-whole)
printf "      - name: Extra\n        if: fromJSON(toJSON(needs.legion_build.outputs)).skip == 'true'\n        run: echo extra\n" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on outputs read whole (N7e)" "$(is "$status" 1)"
check "names what it reads whole" \
  "$(contains "$out" "the step \`Extra\` of publish has the if: \`fromJSON(toJSON(needs.legion_build.outputs)).skip == 'true'\`, which reads needs.legion_build.outputs whole")"
root=$(written spelled-whole-step)
printf "      - name: Extra\n        if: toJSON(steps.version.outputs) != '{}'\n        run: echo extra\n" > "$work/extra-step"
python3 - "$root/.github/workflows/release.yaml" "$work/extra-step" <<'PY'
import sys
from pathlib import Path

path, extra = Path(sys.argv[1]), Path(sys.argv[2]).read_text()
text = path.read_text()
anchor = "\n  pi_envoy_build:\n"
if text.count(anchor) != 1:
    sys.exit("  FAIL: no single pi_envoy_build job to insert before")
path.write_text(text.replace(anchor, "\n" + extra.rstrip("\n") + anchor, 1))
PY
run_check "$root"
check "exits 1 on a checked step's outputs read whole in its own job" "$(is "$status" 1)"
check "names it" "$(contains "$out" "of legion_build has the if: \`toJSON(steps.version.outputs) != '{}'\`, which reads steps.version.outputs whole")"

echo "case: a decoy of a named reader carries its if: while the reader skips"
root=$(written reader-decoy)
named_reader=$'      - name: "legion: Write the changelog into the release body"\n        if: "${{ !cancelled() && steps.legion_notes.outcome == \'success\' }}"\n'
replace "$root/.github/workflows/release.yaml" "$named_reader" \
  "$named_reader"$'        run: echo decoy\n      - name: "legion: Write the changelog into the release body"\n        if: false\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job and the name" \
  "$(contains "$out" "release.yaml's job publish, which holds listed decision readers, has 2 steps named \`legion: Write the changelog into the release body\`")"

# publish's version and tag outputs carry its release on to worker_image_release, whose own if:
# reads the tag: both are pinned, and an if: that reads them is a reader (the review's N3).
echo "case: publish's tag output emptied (N3), and worker_image_release's if: changed"
root=$(written publish-tag)
replace "$root/.github/workflows/release.yaml" $'      tag: "${{ steps.legion_release.outputs.tag }}"\n' $'      tag: \'\'\n'
run_check "$root"
check "exits 1 on tag: '' (N3)" "$(is "$status" 1)"
check "names the output and the module's value" \
  "$(contains "$out" "publish's output tag is \`\`, where $plan_file names \`\${{ steps.legion_release.outputs.tag }}\` for it in JOB_OUTPUTS")"
root=$(written worker-release-if)
replace "$root/.github/workflows/release.yaml" "needs.publish.outputs.tag != ''\"" "false\""
run_check "$root"
check "exits 1 on worker_image_release's if: changed" "$(is "$status" 1)"
check "names its if:" "$(contains "$out" "the job's own if: of worker_image_release has the if: \`always() && .*false\`, where $readers_rule")"
check "with a reader's reason, since its if: reads publish's tag" \
  "$(contains "$out" "of worker_image_release has the if: .*: it reads a checked version step's decision")"
root=$(written publish-reader-new)
printf '      - name: Extra\n        if: needs.publish.outputs.version != %s\n        run: echo extra\n' "''" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on a new reader of publish's version" "$(is "$status" 1)"
check "names it" "$(contains "$out" "the step \`Extra\` of publish has the if: \`needs.publish.outputs.version != ''\`, which reads a checked version step's decision")"

echo "case: a new step that reads release"
root=$(written reader-new)
printf '      - name: Announce the release\n        if: steps.confirm.outputs.release == %s\n        run: echo released\n' "'true'" \
  >> "$root/.github/workflows/release-envoy-listener.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the new reader" \
  "$(contains "$out" "the step \`Announce the release\` of release_publish has the if: \`steps.confirm.outputs.release == 'true'\`, which reads a checked version step's decision, and $plan_file's DECISION_READERS names no such reader")"
root=$(written reader-new-job)
printf '  announce:\n    runs-on: ubuntu-24.04\n    needs: [legion_build]\n    if: needs.legion_build.outputs.skip != %s\n    steps:\n      - run: echo\n' "'true'" \
  >> "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 on a new job that reads the decision" "$(is "$status" 1)"
check "names the job's if:" "$(contains "$out" "the job's own if: of announce has the if: \`needs.legion_build.outputs.skip != 'true'\`, which reads")"

# The round-4 review's reproduction: the runner starts a service and waits for its health check
# before the job's first step, so one with a writable host mount can replace host tools - a git
# wrapper that answers `git log` with nothing - while every pinned step stays as listed.
echo "case: a service container with a writable host mount and a health-check command"
root=$(written services)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' \
  $'    runs-on: ubuntu-24.04\n    services:\n      tools:\n        image: alpine:3.22\n        volumes:\n          - /usr/local/bin:/host-bin\n        options: --health-cmd "cp /wrap/git /host-bin/git"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key and the list" "$(contains "$out" "legion_build sets services: (\`{\"tools\": .*/usr/local/bin:/host-bin.*}\`), which $plan_file's JOB_KEYS does not list")"
root=$(written services-confirm)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'  release_publish:\n    runs-on: ubuntu-24.04\n' \
  $'  release_publish:\n    runs-on: ubuntu-24.04\n    services:\n      tools:\n        image: alpine:3.22\n'
run_check "$root"
check "exits 1 on the listener's confirm job" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release_publish sets services: (")"
root=$(written strategy)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: ubuntu-24.04\n    strategy:\n      matrix:\n        n: [1]\n'
run_check "$root"
check "exits 1 on a key no checked job sets today" "$(is "$status" 1)"
check "names it" "$(contains "$out" "legion_build sets strategy: (")"

echo "case: the listener's confirm step sets its paths' variable"
root=$(written confirmenv)
replace "$root/.github/workflows/release-envoy-listener.yaml" "$confirm_version"$'\n' \
  "$confirm_version"$'\n          LEGION_ENVOY_VERSION_ARGS: -- packages/envoy/\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the key" \
  "$(contains "$out" "release_publish's step \`confirm\`'s env: must be, key for key, .*it sets LEGION_ENVOY_VERSION_ARGS, which that one does not")"

# The round-3 review's three reproductions, each of which the check passed before the job was
# pinned while the unchanged command then reported skip=true, and the class each belongs to.
echo "case: an expression in place of an env: mapping, which can carry BASH_ENV"
root=$(written env-expression)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        env: ${{ fromJSON(needs.setup.outputs.env) }}\n'
run_check "$root"
check "exits 1 on the version step's" "$(is "$status" 1)"
check "names it" "$(contains "$out" "$env_rule (nothing), .*it is \`\${{ fromJSON(needs.setup.outputs.env) }}\`, an expression, not a literal mapping")"
root=$(written job-env-expression)
replace "$root/.github/workflows/release.yaml" $'    runs-on: ubuntu-24.04\n' $'    runs-on: ubuntu-24.04\n    env: ${{ fromJSON(needs.setup.outputs.env) }}\n'
run_check "$root"
check "exits 1 on the job's" "$(is "$status" 1)"
check "names it" "$(contains "$out" "legion_build sets env: (\`\${{ fromJSON(needs.setup.outputs.env) }}\`), which")"
root=$(written earlier-env-expression)
replace "$root/.github/workflows/release.yaml" $'        with:\n' $'        env: ${{ fromJSON(needs.setup.outputs.env) }}\n        with:\n'
run_check "$root"
check "exits 1 on an earlier step's" "$(is "$status" 1)"
check "names it" "$(contains "$out" "step 1 of legion_build, $before_rule.*its env: is \`\${{ fromJSON(needs.setup.outputs.env) }}\` where \`checkout\`'s is nothing")"

echo "case: an earlier step writes \$GITHUB_PATH without spelling it, or through a helper"
root=$(written indirect-path)
replace "$root/.github/workflows/release.yaml" $'    steps:\n' \
  $'    steps:\n      - run: |\n          f=GITHUB_; f+=PATH\n          echo "$PWD/shim" >> "${!f}"\n'
run_check "$root"
check "exits 1 on the indirect spelling" "$(is "$status" 1)"
check "names it" "$(contains "$out" "step 1 of legion_build, $before_rule.*its run: is no listed command (its line 1 is \`f=GITHUB_; f+=PATH\`)")"
root=$(written helper-path)
replace "$root/.github/workflows/release.yaml" $'    steps:\n' $'    steps:\n      - name: Prepare\n        run: .github/scripts/prepare.sh\n'
run_check "$root"
check "exits 1 on a helper" "$(is "$status" 1)"
check "names the step by its name" "$(contains "$out" "step 1 of legion_build (\`Prepare\`), $before_rule")"

echo "case: a PATH override, which picks the bash release-push.sh's #!/usr/bin/env bash runs"
root=$(written path-override)
replace "$root/.github/workflows/release.yaml" $'      - id: version\n' $'      - id: version\n        env:\n          PATH: ./shim:/usr/bin:/bin\n'
run_check "$root"
check "exits 1 on the version step's" "$(is "$status" 1)"
check "names it" "$(contains "$out" "$env_rule (nothing), .*it sets PATH, which that one does not")"
root=$(written path-workflow)
replace "$root/.github/workflows/release.yaml" $'env:\n  LEGION_VERSION_ARGS' $'env:\n  PATH: ./shim:/usr/bin:/bin\n  LEGION_VERSION_ARGS'
run_check "$root"
check "exits 1 on the workflow's" "$(is "$status" 1)"
check "names it" "$(contains "$out" "workflow env: sets PATH, which $plan_file's WORKFLOW_ENV_KEYS does not list for it")"

# The same class beyond the review's three spellings: a variable git or gh reads, and the loader's.
echo "case: GH_REPO in the workflow's env:, and LD_PRELOAD in a listed step's env:"
root=$(written gh-repo)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'env:\n  LEGION_ENVOY_VERSION_ARGS' $'env:\n  GH_REPO: elsewhere/fork\n  LEGION_ENVOY_VERSION_ARGS'
run_check "$root"
check "exits 1 on GH_REPO" "$(is "$status" 1)"
check "names it" "$(contains "$out" "release-envoy-listener.yaml's workflow env: sets GH_REPO, which")"
root=$(written ld-preload)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'          OWN_DIGEST: "${{ needs.image_index.outputs.digest }}"\n' \
  $'          OWN_DIGEST: "${{ needs.image_index.outputs.digest }}"\n          LD_PRELOAD: ./hook.so\n'
run_check "$root"
check "exits 1 on LD_PRELOAD" "$(is "$status" 1)"
check "names the listed step it differs from" \
  "$(contains "$out" "step 2 of release_publish, before release_publish's step \`confirm\`, .*its env: is .*where \`Determine latest\`'s is")"

echo "case: an exclude among LEGION_VERSION_ARGS's paths drops a path go names"
root=$(written magic)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args :(exclude)packages/envoy/alpha"
before=$(release "$root")
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the exclude and what it drops" \
  "$(contains "$out" 'LEGION_VERSION_ARGS'"'"'s `:(exclude)packages/envoy/alpha` drops packages/envoy/alpha/, which go names for ./cmd/legion for linux/amd64')"
run_check "$root" write
check "write refuses too" "$(is "$status" 1)"
check "and writes nothing" "$(is "$(release "$root")" "$before")"
root=$(written glob)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args :(glob)packages/envoy/*"
run_check "$root"
check "exits 1 on other magic" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'LEGION_VERSION_ARGS names `:(glob)packages/envoy/\*`, a pathspec with magic this check cannot read')"

echo "case: an exclude inside a package directory go names"
root=$(written inside)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args :(exclude)packages/envoy/alpha/alpha.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and the package directory it is in" \
  "$(contains "$out" 'drops packages/envoy/alpha/alpha.go, inside packages/envoy/alpha/, a package directory go names for ./cmd/legion for linux/amd64')"
root=$(written inside-short)
replace "$root/.github/workflows/release-envoy-listener.yaml" "$envoy_args" "$envoy_args :!packages/envoy/internal/delta/delta.go"
run_check "$root"
check "exits 1 on the listener's short form too" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" 'LEGION_ENVOY_VERSION_ARGS'"'"'s `:!packages/envoy/internal/delta/delta.go` drops packages/envoy/internal/delta/delta.go, inside packages/envoy/internal/delta/')"

echo "case: an exclude with a wildcard"
root=$(written wildcard)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args :(exclude)packages/envoy/*.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" 'LEGION_VERSION_ARGS names `:(exclude)packages/envoy/\*.go`, an exclude with a wildcard')"

# git reads each of these as an exclude of packages/envoy/alpha/alpha.go, a file of a package the
# binary links; a compare of paths as written does not. Each is refused, naming the canonical
# spelling.
echo "case: an exclude of a linked file spelled another way git reads as the same path"
i=0
for spelled in ':(exclude)./packages/envoy/alpha/alpha.go|:(exclude)' ':!./packages/envoy/alpha/alpha.go|:!' \
  ':!/packages/envoy/alpha/alpha.go|:!' ':(exclude)packages/envoy//alpha/alpha.go|:(exclude)' \
  ':(exclude)packages/envoy/beta/../alpha/alpha.go|:(exclude)'; do
  i=$((i + 1))
  word=${spelled%|*} form=${spelled#*|}
  root=$(written "spelled-$i")
  replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args $word"
  run_check "$root"
  check "exits 1 on $word" "$(is "$status" 1)"
  check "names the canonical spelling of $word" \
    "$(contains "$out" "an exclude whose path is not canonical: .*write it \`$form""packages/envoy/alpha/alpha.go\`")"
done
root=$(written caret)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args :^./packages/envoy/alpha/alpha.go"
run_check "$root"
check "exits 1 on :^" "$(is "$status" 1)"
check "names both canonical spellings" \
  "$(contains "$out" "git's \`:^\` spelling of an exclude, which this check reads in two spellings alone; write it \`:(exclude)packages/envoy/alpha/alpha.go\` or \`:!packages/envoy/alpha/alpha.go\`")"
root=$(written listener-spelled)
replace "$root/.github/workflows/release-envoy-listener.yaml" "$envoy_args" "$envoy_args :!packages/envoy//internal/delta/delta.go"
run_check "$root"
check "exits 1 on the listener's paths too" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" 'LEGION_ENVOY_VERSION_ARGS names `:!packages/envoy//internal/delta/delta.go`, an exclude whose path is not canonical')"

echo "case: a \${{ }} expression GitHub renders into an exclude"
root=$(written expression-paths)
replace "$root/.github/workflows/release.yaml" "$hand_args" "$hand_args \${{ ':(exclude)packages/envoy/alpha' }}"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'LEGION_VERSION_ARGS holds a `${{ }}` expression, which GitHub renders into the paths')"

echo "case: no block, or two"
root=$(fixture unmarked)
replace "$root/.github/workflows/release.yaml" $'  # BEGIN generated by `.github/scripts/check-legion-version-inputs.sh write`\n' ''
run_check "$root" write
check "write exits 1 with none" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'it holds 0')"
root=$(fixture twice)
replace "$root/.github/workflows/release.yaml" $'  # END generated\n' \
  $'  # END generated\n  # BEGIN generated by `.github/scripts/check-legion-version-inputs.sh write`\n  # END generated\n'
run_check "$root"
check "exits 1 with two" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'it holds 2')"

# go list reads a file's package clause and imports alone, so it is an import it cannot read that
# leaves the set unknown: a clause that does not parse, or a package of the workspace's own
# modules that is not there, which go lists with no directory.
echo "case: a package of the workspace go list cannot load"
root=$(written broken)
printf 'package alpha\n\nimport (\n\t_ "example.com/envoy/internal/delta"\n' > "$root/packages/envoy/alpha/alpha.go"
before=$(release "$root")
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package" "$(contains "$out" 'go list cannot load example.com/envoy/alpha for ./cmd/legion for linux/amd64')"
run_check "$root" write
check "write refuses too" "$(is "$status" 1)"
check "and writes nothing" "$(is "$(release "$root")" "$before")"
root=$(written missing)
printf 'package alpha\n\nimport _ "example.com/envoy/internal/gone"\n' > "$root/packages/envoy/alpha/alpha.go"
run_check "$root"
check "exits 1 on an import of the envoy module that is not there" "$(is "$status" 1)"
check "names that import" "$(contains "$out" 'go list cannot load example.com/envoy/internal/gone for ./cmd/legion for linux/amd64')"

echo "case: a listing that names nothing of cmd/legion"
root=$(written gone)
rm "$root"/packages/daemon/cmd/legion/*.go
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the set is unknown" "$(contains "$out" 'so what it links is unknown')"

echo "case: the block's header edited by hand"
root=$(written header)
replace "$root/.github/workflows/release.yaml" 'An edit by' 'A change by'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the text is not write's" "$(contains "$out" "its text is not the block write writes")"

echo "case: the block outside the workflow env:"
root=$(written moved)
block=$(sed -n '/^  # BEGIN generated/,/^  # END generated$/p' "$root/.github/workflows/release.yaml")
replace "$root/.github/workflows/release.yaml" $'\n'"$block" ''
replace "$root/.github/workflows/release.yaml" $'on:\n' $'on:\n'"$block"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says it is not env's" \
  "$(contains "$out" "is not the LEGION_GO_INPUTS of .github/workflows/release.yaml's workflow env:")"

echo "case: a mode the check does not take"
root=$(written modes)
run_check "$root" fix
check "exits 1 on an unknown word" "$(is "$status" 1)"
check "names the two modes" "$(contains "$out" 'this check takes one optional mode: `check` (the default)')"
run_check "$root" check write
check "exits 1 on two words" "$(is "$status" 1)"

echo "case: the listener's paths as they were, without the workspace's module files"
root=$(fixture workspace)
replace "$root/.github/workflows/release-envoy-listener.yaml" $'\n'"$envoy_modules" ''
run_check "$root" write
check "exits 1" "$(is "$status" 1)"
check "write still writes the legion block, which the listener's paths do not decide" \
  "$(contains "$out" "wrote .github/workflows/release.yaml's LEGION_GO_INPUTS: 7 paths")"
for path in go.work go.work.sum; do
  check "names $path" "$(contains "$out" "::error file=.github/workflows/release-envoy-listener.yaml::LEGION_ENVOY_VERSION_ARGS names no path that covers $path, which go names for go env GOWORK in packages/envoy")"
done
for path in packages/daemon/go.mod packages/daemon/go.sum; do
  check "names $path" "$(contains "$out" "LEGION_ENVOY_VERSION_ARGS names no path that covers $path, which go names for go list -m in packages/envoy")"
done
check "names no path the list covers" "$(is "$(contains "$out" 'covers packages/envoy/')" false)"

echo "case: the listener imports, under cgo alone, a package outside the list"
root=$(written cgo)
printf '//go:build cgo\n\npackage main\n\nimport _ "example.com/daemon/lib"\n' > "$root/packages/envoy/cmd/listener/cgo.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it and the cgo listing" \
  "$(contains "$out" 'covers packages/daemon/lib/, which go names for ./cmd/listener for linux/amd64 with CGO_ENABLED=1')"

echo "case: agent-secrets for a client target alone imports a package outside the list"
root=$(written client)
printf '//go:build darwin\n\npackage main\n\nimport _ "example.com/daemon/lib"\n' > "$root/packages/envoy/cmd/agent-secrets/darwin.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it and the client listing" \
  "$(contains "$out" 'covers packages/daemon/lib/, which go names for ./cmd/agent-secrets for darwin/arm64')"

echo "case: an archive links what the listener's exclude drops"
root=$(written excluded)
printf 'package main\n\nimport _ "example.com/envoy/infra/x"\n\nfunc main() {}\n' > "$root/packages/envoy/cmd/agent-secrets-helper/main.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the exclude and what it drops" \
  "$(contains "$out" 'LEGION_ENVOY_VERSION_ARGS'"'"'s `:(exclude)packages/envoy/infra` drops packages/envoy/infra/x/, which go names for ./cmd/agent-secrets and ./cmd/agent-secrets-helper for linux/amd64')"

echo "case: a listener program gone, or the listener's paths"
root=$(written nolistener)
rm "$root/packages/envoy/cmd/listener/main.go"
run_check "$root"
check "exits 1 with the program gone" "$(is "$status" 1)"
check "says the set is unknown" "$(contains "$out" 'so what it links is unknown')"
root=$(written noargs)
replace "$root/.github/workflows/release-envoy-listener.yaml" 'LEGION_ENVOY_VERSION_ARGS:' 'OTHER_ARGS:'
run_check "$root"
check "exits 1 with the paths gone" "$(is "$status" 1)"
check "names the key" "$(contains "$out" "release-envoy-listener.yaml's workflow env: holds no LEGION_ENVOY_VERSION_ARGS string")"

# The environment go_offline.py runs every go command in, seen by a stand-in go
# (go-offline-test-lib.sh): what a cold runner and a warm machine agree on only while it holds.
echo "case: every go command of both lines runs offline over an empty module cache"
root=$(fixture offline)
go_spy "$work/spy-offline"
set +e
out=$(PATH="$work/spy-offline/shim:$PATH" "$root/.github/scripts/check-legion-version-inputs.sh" write 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "every go command ran with GOPROXY=off, GONOPROXY=none and GOENV=off over an empty cache of its own" \
  "$(is "$(go_spy_verdict "$work/spy-offline")" offline)"
check "the stand-in saw both lines' listings" \
  "$(is "$(($(wc -l < "$work/spy-offline/log") >= 10))" 1)"

# GOPROXY=off is not offline while a GONOPROXY or GOPRIVATE the machine sets matches a module:
# go fetches it directly first. Here the daemon requires a third-party module, with its go.sum
# lines, that the empty cache lacks, and the machine's settings match it; a proxy recorder stands
# in for the network, and go-offline-test-lib.sh's network_control is its own control.
echo "case: an inherited GONOPROXY sends no go command of either line to the network"
root=$(written network)
printf 'module example.com/daemon\n\ngo 1.22\n\nrequire (\n\texample.com/envoy v0.0.0\n\t%s v1.0.0\n)\n\nreplace example.com/envoy => ../envoy\n' \
  "$GO_OFFLINE_VENDOR" > "$root/packages/daemon/go.mod"
go_offline_vendor_sums "$root/packages/daemon/go.sum"
printf 'package main\n\nimport _ "%s"\n' "$GO_OFFLINE_VENDOR" > "$root/packages/daemon/cmd/legion/vendor.go"
network_recorder "$work/network"
check "the recorder sees a go list under GOPROXY=off alone try example.org" \
  "$(network_control "$work/network" "$root/packages/daemon" ./cmd/legion)"
set +e
out=$("${GO_OFFLINE_INHERITED[@]}" "$root/.github/scripts/check-legion-version-inputs.sh" 2>&1)
status=$?
set -e
check "the check passes, the third-party module aside" "$(is "$status" 0)"
check "no go command of the check tried the network" "$(is "$(network_requests "$work/network")" 0)"

summary "check-legion-version-inputs.sh"
