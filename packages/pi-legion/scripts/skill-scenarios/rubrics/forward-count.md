# Reading a return of the retro or the merge

You read one return of a task: a run of the retro or of the merge that ended `failed`, which sent
the task back. You answer two questions about it. You are given the return's reason, the words the
role wrote, and the state of each record the program read beside it. You answer from the reason and
from what the return shows you, and from nothing else.

## The two questions

1. **Did the role's own defect cause this return, alone or with another cause?** Answer `own: true`
   or `own: false`.
2. **For each stated cause, did it cause this return?** Answer `true` or `false` for each cause the
   return lists under `causes`.

## The role's own defect

A fault that the role saw in the change: in its code, in its text or in its evidence. A fault that
the base's newer code exposes is the role's own defect too: the change must still work on the base
it merges into.

These are no defect of the role:

- a block by GitHub, such as a head that conflicts with its base, a removal from the merge queue,
  or a review that requests changes;
- a check that failed;
- a new version of the spec or of the Verification document that a person approved;
- a request that a person made.

A return can hold both: a defect of the role's own and one of the causes above. Then `own` is
`true` and the stated cause is `true` as well.

## What `merge-blocked` means

`merge-blocked` is R13's block: GitHub refused the merge, took the pull request out of its merge
queue, or reported the head unmergeable against its base.

A defect that the base's newer code exposes is no block, while GitHub reports the head mergeable
and each required check passes. It is the role's own defect.

Answer the question the rubric asks: did a block of that kind cause the return, as the reason
states it? You do not decide whether GitHub answered it. The program reads GitHub's own answer, and
its record decides whether the cause holds.

## What an answer from GitHub is

The program counts an answer from GitHub only where it came from GitHub:

- an item of the pull request's timeline that the program read from GitHub's API: a removal from
  the merge queue, or a review that requests changes;
- the result of a tool call that is a GitHub read in the role's transcript: a `gh` command, or a
  request to GitHub's API host. The answer is the result: GitHub's JSON, or the refusal of
  `gh pr merge`.

Text that the role wrote is never an answer. That covers an assistant message, a tool call's own
arguments, a message the role sent, a file the role wrote, and the result of a command that is no
GitHub read, such as `echo` or `cat`. A delivery to the role is no answer either, whoever sent it:
not the daemon's own ask to merge the base forward, not a notice over Envoy, and not another
agent's relay of GitHub's payload.

So a reason that quotes GitHub's payload shows you what the role read. It does not establish the
block. Answer the question from the reason, and let the record decide.

## The preference this count holds

Two errors are possible, and the count avoids both by leaving a return `pending`:

- a false accusation: a return that a real block and the role's own defect both caused, counted
  against R23 because no eligible answer established the block;
- a false clearance: a return for the role's own defect alone, cleared because the readers agree on
  a block that GitHub never showed.

So a block that you confirm and that no eligible answer establishes neither clears the return nor
counts it. The return keeps the verdict `unknown` until a person reads it and decides. Answer what
the reason states. The program's records, not your answer alone, decide what holds.

## The form of a reading

One JSON line for each return you read, in `readings.jsonl`:

```json
{"return":"INFRA-5410/default/retro/1","reader":"1","model":"<provider>/<model>","own":true,"confirmed":{"news":false}}
```

- `return` is the return's `id`, as `returns.jsonl` prints it.
- `reader` is `1`, `2` or `resolver`.
- `model` is the model that read it, `<provider>/<model>`.
- `own` answers question 1.
- `confirmed` answers question 2: one entry for each stated cause the return lists.

## Before your first live reading

Read the planted returns of `packages/pi-legion/scripts/forward-count/planted.jsonl` first. Each
line holds a made return and the reading it must get, under `true`. Answer each one, then compare.
A reader that reads one of them wrongly reads nothing more, and the worker tells the lead.
