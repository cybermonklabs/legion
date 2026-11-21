# Writing a check CI proves

Part of `skill://legion-architect/references/design-gate.md`, "A check CI proves". Read this
before you write a check that carries `ci_check`, or a check whose `run` names tests.

A check carrying `ci_check` is CI's: `ci_check` names the GitHub check run that proves it at the
head; it takes no `run`, its `when` is `before_merge`, and no role runs it or records its result:
`dispatch results` derives its one result from the named run
(`skill://legion-worker/references/ci-checks.md`). Where a CI job that runs on an ordinary pull
request already makes a check's proof, write that check as `ci_check` rather than as a command. A
`run` that names tests names specific tests or files; a whole suite is the CI job that runs it.
Any other `run` is the procedure a role drives on the check's surface.

1. Name the check run of the job that runs the proof, never a roll-up. On cybermonklabs/legion
   never `ci-ok`, which passes when the jobs it did not select were skipped
   (`.github/scripts/ci-ok.sh`).
2. Name a job held back from ordinary pull requests too, because a `run` in its place leaves the
   pod as the only place its proof runs. The implementer adds the label that lets the held-back
   jobs run (`full-ci` on cybermonklabs/legion), then pushes, because the workflow starts on a push
   and not on a label; `dispatch results` takes the result from the run of that push. Without the
   label the job reads `skipped`, so the label stays on for as long as a check names such a job.
3. Write in `negative_control` the case inside that job that fails when the property breaks: a
   step with `continue-on-error` keeps its job green.
