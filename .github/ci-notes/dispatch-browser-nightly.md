# Notes for the Dispatch browser nightly workflow

The derivations of the `quarantine-check` job of `.github/workflows/dispatch-browser-nightly.yaml`,
which points into this file. Each `# why:` line of that job names the section below that holds
the derivation its job or its step rests on, and the sections stand in the order their comment
blocks stood in the workflow file. A budget's own arithmetic stays beside the budget, in the
workflow file. The workflow's other comments still stand in it: its bodies are not moved yet
(.github/ci-notes/README.md), and this job is the first of it to hold its derivations here.
In the text moved here, "this workflow" refers to the workflow,
`.github/workflows/dispatch-browser-nightly.yaml`.

## The quarantine check, what it reads and when it runs

The quarantine's checks that depend on the date (R4 of dispatch://INFRA-983):
packages/dispatch/e2e/tools/quarantine-check.ts reads the `dispatch-browser flaky row` notices
of the merge jobs of the last 14 days of scheduled runs of this workflow and merge-group runs
of ci.yaml, and fails on a row flaky in two of them that packages/dispatch/e2e/quarantine.ts
does not hold, and on an entry of that file held longer than 14 days, each named in its
summary with the runs and the lines to add or to remove. A pull request's runs are not read,
since a pull request's head can carry its own defect; the tool's head says the rest. It needs
no other job of this run: what it reads is runs that have already ended, this one's not among
them, so a flaky row of tonight's full lane is counted by tomorrow's check.

The 02:17 schedule and a workflow_dispatch alone, not the 13:41 one: a second reading of the
same 14 days half a day later says nothing the first did not, and each reading spends about
2,100 calls of the repository's API budget (the measurement in
.github/ci-notes/dispatch-browser-nightly.md#the-quarantine-checks-measured-steps-and-its-budget).

## Why the quarantine check holds those three scopes, and what it reads of the checkout

`actions: read` is the run listing of the two workflows, `checks: read` the merge jobs' check
runs and their annotations, and `contents: read` the checkout. The tool writes nothing to
GitHub. For a row flaky in two runs it reads the row's own lines from a listing of this
checkout and their last change from the file's blame at this commit (one GraphQL request per
file, which the tool sends itself with the job's token), so it needs the install and the
contracts build the listing loads; it reads no git history, so the checkout stays shallow. It
counts a flake only from a run created after that change and after the row's fix, where
e2e/quarantine.ts's `fixes` names one, and it reads each fix's commit against this one
through GitHub's compare.

## The quarantine check's measured steps and its budget

The budget, by the rule .github/ci-notes/dispatch-browser.md states for every job. The check
step was measured in this issue's pod against the history of 2026-11-05 to 2026-11-19: about
190 s for 1,641 runs (20 scheduled, 1,621 merge-group) in 2,067 calls, four at a time, and
23 s for the listing, so 213 s. 213 x 1.15 = 245 s, so 5 minutes. The steps without a budget,
at their maximum over the 12 jobs of this workflow's run 44545596758 that ran them: Set up job
2 s, checkout 12 s, setup-bun 4 s, `bun install` 11 s, the contracts build 1 s, and the post
and completion steps 3 s: 33 s, and 300 + 33 = 333 s rounds up to 6 minutes. The step's time
grows with the merge queue's runs, one call each, so a busier fortnight moves it: a step that
outran its budget names itself.

## Why the quarantine check builds contracts

The step of this name in .github/workflows/dispatch-browser.yaml's shard job, for the reason
.github/ci-notes/dispatch-browser.md#why-every-job-that-runs-the-suite-builds-contracts-first
gives: the listing loads e2e/playwright.config.ts, which imports `@legion/contracts` out of the
build this step makes.
