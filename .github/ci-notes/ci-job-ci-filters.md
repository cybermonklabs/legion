# ci-job-ci-filters.yaml

## The guard that no CI file compares the spec files

No CI job compares the spec files with the approved spec: R2 of The Spec gives that comparison at
each push to Dispatch. The guard reads every file under .github/workflows, .github/actions,
.github/scripts and scripts as text, names each place where the command's name and then the word
compare follow each other, a comment or a quoted text too, and fails on it. It does not find a
command that an expansion, an escape or an alias builds, or one with another word between the two.
It fails when it read no workflow.

It runs in this job because this job's filter already takes every workflow file, so a change to one
job's own file selects that job and this one, and no other job for the guard. The command is
packages/envoy's, whose go.mod pins the same Go as the packages/daemon/go.mod the setup-go above
reads.
