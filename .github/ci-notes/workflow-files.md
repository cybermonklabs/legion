# Notes for the workflow file check's own workflow

The derivations of `.github/workflows/workflow-files.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its trigger,
its concurrency, its job or its step rests on, and the sections stand in the order their comment
blocks stood in the workflow file. A comment block of at most two lines stays where it is,
beside the value or the step it is about.
In the text moved here, "this workflow" and "here" refer to the workflow,
`.github/workflows/workflow-files.yaml`.

## Why the check has a workflow of its own, and when it runs

A second home for .github/scripts/check-workflow-files.sh, in a file small enough that GitHub
can always read it.

The check's own header says what an oversized workflow file does: above GitHub's cap it starts
no run at all, and it says so with a run that holds no jobs, no annotation and no error. Turn
that on ci.yaml itself and the pull request is stuck with nothing to read. ci.yaml carries this
check in its `ci-filters` job, but a ci.yaml that crossed the cap starts no run, so that job
never runs, `ci-ok` is never reported, main's ruleset holds the pull request on a check that
will never arrive, and nothing on the pull request says why. A check that lives only inside the
file it measures cannot report on the one change that breaks it. This workflow is a few
kilobytes, so it runs whatever ci.yaml weighs, and it reports the reason in words.

It is not a required check and nothing adds it to a ruleset: it reports, and a person reads the
failure and acts on it. The verdict main requires is still `ci-ok`.

A pull request touching .github/workflows runs it, draft or not. The trigger names no `types:`,
so it is GitHub's default of `opened, synchronize, reopened`, which a draft's own pushes arrive
as; and no job or step here reads `github.event.pull_request.draft`, because a draft is exactly
where an oversized ci.yaml is written, and a draft whose CI has otherwise gone quiet is the head
this check has to speak on.

No `merge_group` trigger. .github/scripts/ci_workflow_rules.py's
check_one_merge_group_workflow holds ci.yaml to be the only workflow of this repository that
runs on a merge group, and a merge group runs ci.yaml, whose `ci-filters` job carries this same
check. So a group is covered already, and a run here would both repeat that check and be the
second merge-group workflow that rule refuses.

## What a newer push cancels

Keyed on the pull request, and a newer head cancels the older run, waiting or running: this
check is required by nothing, so a cancelled run leaves no ruleset waiting on a verdict, and the
only verdict worth a runner is the one on the head as it now is. pr-title.yaml keeps its
in-flight runs for the opposite reason, which its concurrency comment sets out: that job's check
run carries a required name, where a cancellation would sit on the head as no pass.

## Why the job holds no scope

No scope at all. The checkout needs no `contents: read`, because this repository is public
and GitHub serves a public repository's git data to any valid token, as
.github/workflows/main-bypass-check.yaml records of its own checkout; the check reads files
of the tree and writes nothing, here or anywhere.

## What the check step measures, and with which copy of the check

A pull request's merge ref is this run's checkout, so a pull request that changes the
check is measured by its own copy of it, as `.github/workflows/ci.yaml#ci-filters` job is. The script
reports every fault of every file in one run; its header says what each fault is.
