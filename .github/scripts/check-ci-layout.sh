#!/usr/bin/env bash
# ci.yaml selects each job by package area: the changes job's dorny/paths-filter step reads
# .github/ci-areas.yaml, and each job's `if:` tests the one output named after the job (or the
# output CI_OK_SELECTED_BY maps it to). A selected job runs every leg of its matrix, and ci-ok
# judges every job by its class. This check holds that layout, reading ci.yaml, every workflow a
# job of it calls, every local composite action they run (.github/actions/), and the scripts those
# run, and refuses, each refusal naming the file, the job and the step:
#
#    2. a job whose if: tests an output the changes job does not declare: the output is empty and
#       the job is skipped on every run.
#    7. ci-ok's classes: a job its if: gates on an output of changes is selected by it (its own id,
#       or the exception CI_OK_SELECTED_BY maps, an entry to the job's own id being refused);
#       CI_OK_ALWAYS_RUN names a job no output gates (lint, which runs check-test-reach.sh, among
#       them); CI_OK_EXEMPT the jobs outside ci-ok's accounting; each job in exactly one class,
#       every job but changes and the exempt ones ending with the report step (a called workflow's
#       job reporting for its caller), ci-ok needing every job but itself and the exempt ones,
#       CI_OK_PULL_REQUEST_ONLY the jobs whose if: admits pull_request alone, the two checks main's
#       ruleset requires keeping their names, pr-title needing no job, the ref_is_main output, the
#       hold step and the reconcile step held once each with their if:s as they are, and ci-ok.jq
#       reading the name of every job ci-ok judges, the legs' and the carriers' included.
#   10. an if: that tests a push event or compares github.ref with a branch ref, in ci.yaml, in a
#       workflow only ci.yaml calls, or in a step of a composite action no other workflow runs
#       (an action ci-caches.yaml also runs, as the architecture and spec checks, saves its cache
#       on a push).
#   11. a cache family ci-caches.yaml saves, in its own steps or inside an action they run, whose
#       key is neither exactly that of every restore of it (in ci.yaml, a workflow it calls, any
#       other workflow, or an action any of them runs) nor that key followed by
#       `-${{ github.sha }}`, or whose path is not that restore's; a family saved both under
#       `-${{ github.sha }}` and without it; a save whose key holds hashFiles() or names
#       `steps.<id>.outputs.cache-primary-key` of no restore of its job; a restore of a family
#       saved under `-${{ github.sha }}` that sets lookup-only, or whose restore-keys are not
#       exactly its key followed by `-`, so they hold its Go version (main's #5141 and #5148); a
#       family nothing restores; and any other lookup that is not lookup-only or has restore-keys.
#   12. a job of ci.yaml whose if: lacks `(github.event_name != 'merge_group' ||
#       github.run_attempt == '1')` (or, admitting merge_group alone, `github.run_attempt == '1'`),
#       and ci-ok or pr-title holding either.
#   14. a workflow other than ci.yaml that runs on merge_group.
#   16. a race build of ci-caches.yaml's daemon fill job whose packages are not, in order, those of
#       the daemon's race step (ci_pins.DAEMON_RACE_STEP), and a fill that reaches a race script
#       otherwise than by its one compile call (ci_pins.RACE_FILL_SCRIPTS).
#   19. an output of changes reading a step that is neither its dorny/paths-filter step (and a
#       filter of the file it reads), nor its unnamed-file step (`found`, beside a filter), nor the
#       step ref_is_main reads.
#   20. a darwin vet step (ci_pins.DARWIN_VETS) that does not run the vet over the tag sets
#       .github/scripts/go_build_tags.py prints, or a file of its module that darwin builds under
#       some tags and none of those sets compiles, which go list decides.
#   22. a test one job runs by name in a -run list that another job runs too, its package in that
#       job's packages and its name in no -skip list there: the -run of one job and the -skip of
#       another split one package between them.
#   23. a step, a leg, an env, a name or an input of a called workflow that tests an output of
#       changes: only a job's own if: and its report step may, ci-ok aside.
#   24. a `go test` that runs tests outside .github/scripts/go-test.sh, which retries on a merge
#       group with --rerun-fails-run-root-test and reads the jsonfile after gotestsum ends; a
#       go-test.sh call with no --jsonfile; GO_TEST_RETRY=off anywhere but kind's runtime legs and
#       ci_pins.NO_RETRY_SCRIPT, which must turn it off for its runs against a mutant.
#   25. a job of ci.yaml or of a workflow it calls that a run can skip and whose name: is no
#       expression, or an expression that reads no context, so its skipped record carries a plain
#       name: GitHub evaluates a name of string literals alone even for a skipped job (pull request
#       run 45440587594's skipped check run 138611450495 carries `kind / image`, from
#       `${{ 'kind / image' }}`), and leaves a name that reads one, `${{ github.event_name && … }}`,
#       as its text. A job of ci.yaml that calls a reusable workflow of this repository is the
#       exception: GitHub writes its skipped record under the name it evaluates all the same (run
#       45523492636's jobs 138901138215, `worker-image`, and 138901137695,
#       `dispatch-browser-pr`). So a caller's evaluated name, as ci-ok.jq reads it, that is a
#       check-run name the layout writes (a called job's `<caller> / <job>` among them) or one a
#       check reads (ci_pins.MAIN_CHECK_RUN_NAMES) is refused too.
#   26. a job of ci_pins.ARM_JOBS on another runner.
#   27. a group of liveGroups that is not in exactly one kind leg's -run (ci_pins.LIVE_GROUPS).
#   28. an if: of a job, a leg, a step or an inner job, or a matrix entry, that names the event or
#       a label and is certainly false on a pull request (with or without full-ci) or a merge
#       group, but those ci_pins.EVENT_GATES names; a step that uses another repository's
#       action runs no test and is passed over.
#   29. a matrix without fail-fast: false.
#   30. a check-run name of ci.yaml at main (ci_pins.MAIN_CHECK_RUN_NAMES) that no job, leg or
#       called job of the layout carries.
#   31. a timed step of main's ci-job files or of the jobs the layout added after them
#       (ci_pins.TIMED_STEPS) that lost its own bound: timeout-minutes on a uses: step of ci.yaml, a
#       `timeout` command in a run: body moved into an action, as many of each as there were.
#   32. a split job (legs named `<name> (<i>/<n>)`) with no carrier, one with two, or a carrier
#       (a job that runs .github/scripts/leg-verdict.sh) that is not exactly a sparse checkout of
#       .github, leg-verdict.sh over the split job's legs and the report, named `<name> /
#       <name>`, needing the split job and gated as it is under !cancelled().
#   33. a `## ` section of .github/ci-notes/*.md that no why: link of .github names, a why: link
#       to a section that is not there, and a `## <name>: ` heading whose name is neither an
#       output of changes nor a job of ci.yaml.
#   34. the legs of a split job (`<name> (<i>/<n>)`) that name two counts of legs, or that leave an
#       index from 1 to n out, name one twice or name one outside 1 to n; a leg whose matrix `part`
#       is not its own index; and a step that runs in a leg and hands it another count than n as
#       its `with: legs` (written out, or read off the leg's matrix). A leg reads its shard of a
#       corpus by its index and count (CORPUS_SHARD=<i>/<n>), so the legs read each document once
#       between them only when each index names one leg and every step divides by the same n.
#   35. a script .github/scripts/go_test_steps.py's AUDITED_SCRIPTS reads whose SHA-256 is not its
#       entry's: the entry is a person's reading of what the script runs, and a script edited past
#       it is read wrong by every reader of the steps. Held here, in the lint job, so the pull
#       request that edits the script fails, and a step the reader then cannot read is named
#       rather than ending the check.
#
# Rule 20 runs go list over each vetted module with GOPROXY=off over an empty module cache and the
# module's own toolchain, so nothing is fetched and a cold runner answers as a warm machine does.
# Unset GOROOT, which would choose another go than the one PATH and go.work name.
#
# Run from anywhere: .github/scripts/check-ci-layout.sh
# CI runs it in ci.yaml's lint job (.github/actions/lint/lint), and its test in the scripts-held job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The engine (ci_layout.py), the rules that read the workflow directory (ci_workflow_rules.py),
# the pins (ci_pins.py) and the step reader (go_test_steps.py) sit beside this script in the
# repository it checks. PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c '
import sys
import ci_layout
sys.exit(ci_layout.main())
'
