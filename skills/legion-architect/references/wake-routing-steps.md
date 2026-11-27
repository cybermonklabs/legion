# The steps of a wake's routing

Read this when `skill://legion-architect/references/wake-routing.md` sends you here; its table names the wake each section here answers.

## A lost session

On `session-lost` or `session-forgotten`, check the fresh agent's account of the work against the record, then send it with `envoy_publish` each decision it can no longer read.
Your own lost session: read your record and your tree's issues first.

## A full issue volume

On `volume-near-full`, sort the bytes by where they are:

- Mostly cold bytes in `scratch/tmp` or a workspace: ask the pod's roles on their topics to delete by exact path what they made and no longer need.

- Bytes warm across the clone, workspaces and caches: open a finding ask with the sizes, since `issue_volume` is the operator's.

- Carry a trimmer that switched itself off as `skill://legion-worker/references/findings.md` says.

## A reply on an ask

Answer in its thread (`dispatch comment --reply-to <comment id>`) when it carries a question, a disagreement, a correction or a fact you lack.
Under an approval request you must revise or hand back, answer with `--reply-to-ask <ask id> --turn agent`, since a default reply gives the turn to the person.
An approval or a settled answer draws no reply.
Relay it to the responsible role.

## The issue's pull requests

An issue's pull requests are its tasks' and each one it links by its URL, in any repository.
The work of an issue ends when no pull request of the issue is open.

- `pr-closed-others-open`: a pull request closed without a merge while the others its reason names stay open; nothing moved.
  Decide whether its work reopens or ends; a phase worker makes the repository change.

- `pr-holds-close`: each pull request it names holds the issue's close.
  Remove its link, have it merged, or give Legion what the reason says it lacks.
  One linked after its task's review ran has no approval, so the merger leaves it: send the issue back to that step while the task's own pull request has not merged, or remove the link.

- `pr-link-refused`: Legion takes no fork's pull request and no pull request another issue holds.
  Remove the link; where the change belongs to this issue, link a pull request from a branch of that repository.
