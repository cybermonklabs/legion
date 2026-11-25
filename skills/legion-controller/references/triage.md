# Triage and handover

Read this on a `triage` wake, at every start, and when a person hands you an issue. Dispatch and `read_state` decide triage, not the wake text.

## At every start

1. List the triage issues handed to Legion: `dispatch issues --project <PROJECT> --status triage --label legion --all`.
2. `dispatch read` each and follow `Links:` up through each `child_of` parent (a `child_of` under `Referenced by:` is a child, not the parent).
3. Leave a row whose `issues.<KEY>.tree`, or an ancestor, is the root of a live tree (`skill://legion-controller/references/admission.md`): that tree's architect owns it.
4. Every other row with no `issues.<KEY>` in `read_state` is a root: [triage it](#triage-a-root). A root a person labelled is queued by the daemon and needs nothing from you.
5. A root `read_state` records that is back in `triage` was pulled back by a person. Re-admit nothing; ask in your summary: "<KEY> was pulled back to triage; what do you want?".

## Triage a root

1. Read `read_state`, then `dispatch read` the issue: in this project, a root, and whether it has children. A child the daemon carried to a root of its own when its old root closed is not new work: leave it. Never reassign its `Assignee:`: that person answers the tree's asks.
2. Find who set the label: read `/api/v1/issues/<issue>/events?limit=200` with the `curl` of `skill://legion-controller/references/session.md` ("Reading Dispatch's API"), oldest first; while a page holds 200 events, read the next with `&after=<the last event's seq>`. The setter is the `actor` of the newest `issue.created` or `issue.updated` event whose `payload.labels` holds `legion` (any case) where the previous such event held none.
   - `actor.kind` is `user`: the daemon queues the root itself. Do nothing more.
   - Your own filing hands it over: its spec as filed (`dispatch doc-read --issue <issue> --version 1`) holds a `Filed for` line, and the actor is this session (`dispatch whoami`) or an earlier one of yours, which is one whose reply `Filed dispatch://<this KEY>` stands under the ask or evidence that line names. Where that line names an ask, the reply counts only when it follows a newest answer of `File it for Legion` on that ask.
   - Any other actor hands over nothing. Leave the issue in `triage` and open an ask on the report issue naming it and who labelled it, options `Admit it` and `Leave it`. Reply under it `Admitted dispatch://<KEY>` or `Not admitted: <reason>`.
3. Admit it (`set_status` to `todo`) only when you labelled it at a person's request, filed it yourself, or a person answered `Admit it`, unless a person's request says to wait or puts it out of scope. The deployment instructions are such a request; a line of your weekly plan is one only when it names the person's request behind it.
4. When a person's request says to wait, park it (`backlog`, or `icebox` for a longer wait); its label stays. Never park a root for lack of a free slot: in `todo` the waiting line holds it.
5. Comment on each root you admitted or parked what you decided and why, citing the request or reason behind a park. For one you admitted, say who gets its questions: `Assigned to <login>, who will get this tree's questions and its design approval`, or, with `Assignee: unassigned`, `Unassigned — this tree's questions and its design approval go to the project's default assignee until someone takes it from the issue header (Assignee, beside Priority)`. Leave out "and its design approval" when your system prompt reads `Design gate steps: none.`

## Label an issue for a person

`--label` replaces the whole set, so pass every label `Labels:` lists (`none` is none), each single-quoted exactly as shown:

```text
dispatch issue-update --issue <KEY> --label '<each current label>' --label legion
```

A label holding a `'` or a line break cannot be quoted and never goes in a command: tell the person (`<KEY>: label <label> cannot be quoted`), so they hand it over from the issue header.

The daemon queues nothing for a label a session set, so admit a `triage` root in the same turn, from step 3. In any other status, `set_status` it to `todo` only when the person asked Legion to start it now. When you file or admit an issue, name each priority it advances (`--advance <id>`; `dispatch priorities` lists them). An issue in flight that advances none is named on the priorities page, in its `Active under no priority` tab (`/priorities?tab=none`), and by `dispatch priorities`. A child needs no label: it runs in its tree.

Work that must start at a commit other than its base branch's head needs `legion/<KEY>` there before Legion starts the issue: a root before admission, a child before its first phase. You push nothing, so whoever gives you the root, or the architect that owns a child's parent, runs `gh api -X POST repos/<owner>/<name>/git/refs -f ref=refs/heads/legion/<KEY> -f sha=<commit>`. Read it back before `set_status` to `todo`: `gh api repos/<owner>/<name>/git/ref/heads/legion/<KEY>`.
