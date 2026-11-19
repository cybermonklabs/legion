---
title: "A step pinned whole is refused when it drifts, and is read only under the steps before it"
category: github
tags:
  - ci-checks
  - ci-filters
  - paths-filter
  - fail-wide
  - github-actions
date: 2026-11-19
status: active
module: .github/scripts
related_issues:
  - "cybermonklabs/legion#4442"
---

# A step pinned whole is refused when it drifts, and is read only under the steps before it

Extends docs/solutions/github/a-coverage-check-scores-what-it-cannot-reduce-as-reading-everything-and-reduces-only-a-whole-step-under-an-allowlisted-environment-pr2316.md.

That note's rule is to fail wide: a command the check cannot reduce counts as reading every Go
file. The rule answers one question, which files a job reads, and a step pinned whole (an exact
entry of `.github/scripts/ci_job_reads.py`) answers a second: which tests the step runs. Failing
wide answers nothing about the second.

- **Refuse a step that is not its pin; never read it wide.** `ci_job_reads.py` builds the
  text-check job's guard step's `-run` from `TEXT_CHECK_TESTS`, the same list envoy-go's plain
  step leaves out. While a step off its pin was read as reading everything, a guard test dropped
  from that `-run` passed `check-ci-filters.sh`, because the `text_checks` filter already selects
  every Go file, and the test then ran in no job. The check now refuses the step. It names the
  file, the job, the step and the first line that differs, or both directories, and on an
  anchored `-run` or `-skip` list each test one side names and the other lacks.
- **What surrounds a pinned step can change which tests it runs without changing its text.**
  `GOFLAGS: -skip=^TestOnlyTheBrokerInsertsEvents$` in the guard step's env leaves the script, and
  so the pin, as they were, and go1.27.3 then skips that test though the `-run` names it. So a
  pinned step is refused, naming the cause, under an env name outside `ENV_ALLOWED`, a `shell:`,
  a job `container:`, or an environment write that reaches it. A name with which go compiles the
  same files and runs the same tests (`GOMAXPROCS`) joins `ENV_ALLOWED` instead.
- **Read only the steps that come before.** GitHub applies a step's `$GITHUB_ENV` or
  `$GITHUB_PATH` write to the steps after it, never to earlier ones. A check that scans every
  step of the job, whatever its order, refuses for a write that reaches nothing. Three readers
  had to learn this, each from its own false refusal:
  - a pinned step reads the steps before it, and not its own script, whose lines the pin holds;
  - a claimed simple go command reads the steps up to and including its own, since its own
    `go env -w` reaches its later go commands;
  - a script's go command reads each job's steps up to the last one that starts the script, and
    every step of a job that never names the script, since that job may start it in a way the
    check does not see.
- **Say exactly what starts a script.** A step starts it by its path from the root or from the
  step's directory, as a whole word: at the start of a line, after a space, a quote, an `=`, a
  `(` or a backtick, as `./<script>`, or after a `/` that ends a variable, a `}` or a `)`. Those
  last three cover `"$GITHUB_WORKSPACE/<script>"`, `${{ github.workspace }}/<script>` and
  `$(git rev-parse --show-toplevel)/<script>`. A `/` after any other text ends a directory of
  another path: `tools/scripts/e2e-local.sh` is not `packages/envoy/scripts/e2e-local.sh` run from
  `packages/envoy`. Too narrow a match misses a start after a write; too wide a match reads a job
  only up to another script of the same name.
- **Run the check as CI does, and make it fail first.** An exported GOROOT names the go that
  `go env GOROOT` reports, whichever go `PATH` finds first. On a dev box that exports mise's
  go1.27.0, every closure failed with `go.work requires go >= 1.27.3`, which is 1618 problems
  where CI finds none. Unset GOROOT and put the toolchain that `go.work` pins first on `PATH`;
  the header of `check-ci-filters.sh` gives the recipe. Each new row of the test file that a fix
  answers fails over the previous round's engine, and a row that guards behaviour the fix keeps
  fails on an engine broken on purpose.

## Evidence

- In cybermonklabs/legion#4442, `.github/scripts/ci_filter_coverage.py`:
  - `refuse_off_pin` refuses a step off its pin, or under what can move it;
  - `pin_difference` and `name_list_difference` name what differs;
  - `job_up_to` gives the steps a step sees;
  - `names_script` and `SCRIPT_START` say what starts a script;
  - `around_job` reads a script's surroundings.
- On the real tree, with a plant each, at main and then at the head:
  - `TestOnlyTheBrokerInsertsEvents` dropped from the guard step's `-run`: exit 0 at main, exit 1
    with one problem at the head.
  - A test added to the `-run` and not to `TEXT_CHECK_TESTS`: exit 0, then 1.
  - A test added to `TEXT_CHECK_TESTS` and not to the step: exit 0, then 1.
  - The `GOFLAGS` skip above: exit 0, then 1.
- A `$GITHUB_PATH` step at the end of text-checks refused the pinned guard step before it, at the
  round that read every step. A `$GITHUB_PATH` step at the end of envoy-go-race-api gave 23
  problems, at the round that read every step for scripts. Each exits 0 now.
- A `GOFLAGS` write followed by a `"$GITHUB_WORKSPACE/.github/scripts/pg-connection-ceiling.sh"`
  start in envoy-go-race-api exited 0 while `names_script` took a path only at the start of a
  line, after a space, a quote or an `=`, or as `./<path>`. It exits 1 with 23 problems now.
