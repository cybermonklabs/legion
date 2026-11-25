# A check CI proves

## What it is

`ci_check` names the GitHub check run that proves the check at the head, as GitHub writes it: Dispatch matches the name exactly, and a job of a called workflow writes its check run as `<caller> / <job>` (on cybermonklabs/legion, `test / test` or `dispatch-browser-pr / merge`).
Such a check has no `run`, its `when` is `before_merge`, and it names its `negative_control`.

## No role runs it

No role runs its work in the pod or records a result for it: Dispatch refuses one with `409 CHECK_PROVED_BY_CI`.
`dispatch results --issue <issue>` reads the named run at the head of the issue's pull request and prints the one result it derives, its `A pass stands:` line, and the line for the PR body.

When the run gives no result, the line reads `CI run <name> has no result (<case>)`, the case one of:

- `not_completed`: the run is still going.

- `absent`: no run of that name is at the head.

- `no_head`: Dispatch holds no pull request head of the issue.

- `neutral`, `skipped` or `cancelled`: the run concluded and proved nothing.

- `github_unreadable`: Dispatch could not read GitHub; treat it as a run still going.

## What each role does

- The implementer copies each such check's `Pull request line` into the `Checks` block.
  The next role that writes the body reads `dispatch results` again and replaces a no-result line.

- A `ci_check` check goes in `failures` only when its derived result is a fail, its run concluded `skipped`, `neutral` or `cancelled`, or it names a job held back without its label, whose run is `absent` until the label is on (the last bullet below).
  A run still going fails no round: say so in `implementerProof.how`.

- A job the repository runs on a pull request only under a label (on cybermonklabs/legion, the slow suites) does not run without it.
  The implementer adds the label, `full-ci` or that job's own `lane:<output>`, then pushes, because the workflow starts on a push and not on a label.
  The label stays on while a `ci_check` of the issue names such a job.
  On cybermonklabs/legion a check of the edited-migrations, gofmt, OpenAPI or pmdoc steps names `envoy-go-build / envoy-go-build`, which every pull request runs; envoy-go-checks is envoy's script tests, a held-back slow suite, and a check naming it for those steps passes under its label with none of them run.

## A name that no run answers

When the head's checks have settled and no run of the name answers:

1. Read the merge state first (`gh pr view <n> --json mergeStateStatus`): a `DIRTY` head gets no `pull_request` run, and the forward merge cures it.

2. A held-back job's check is `absent`, not `skipped`, on a pull request without the job's label when the job calls a workflow, as each held-back job on cybermonklabs/legion does (`test / test`, `dispatch-browser / merge`): the calling job concludes `skipped` under its bare name, and no job of the called file starts.
   The check's words are right: the implementer adds the label and pushes, and each other role sends the issue back to the implementer for it, as for any check with no pass.

3. Otherwise the check's words are wrong, as a called workflow's job named without its caller is (`test` for `test / test`).
   Each tester and reviewer step sends it to the architect as a finding with `envoy_publish`; with neither step, the implementer sends it during retro.
   No role fails the round or requests changes for it.
