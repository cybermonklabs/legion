# Triage and handover

Dispatch and `read_state` decide triage, not the wake text.

## At every start

1. List the triage issues handed to Legion: `dispatch issues --project <PROJECT> --status triage --label legion --all`.
2. `dispatch read` each, and follow `Links:` up through each `child_of` parent. A `child_of` under `Referenced by:` is a child, not the parent.
3. Leave a row whose `issues.<KEY>.tree`, or an ancestor, is a root in `admission.trees`: that tree's architect owns it.
4. Each other row with no `issues.<KEY>` in `read_state` is a root: [triage it](#triage-a-root).
5. A root that `read_state` records and that is back in `triage` was pulled back by a person. Re-admit nothing. Ask in your summary: "<KEY> was pulled back to triage. What do you want?"

## Triage a root

1. `dispatch read` the issue. A child that the daemon carried to a root of its own when its old root closed is not new work: leave it. Never change its `Assignee:`, because that person answers the tree's asks.
2. Find who set the label. Read `dispatch api --path '/api/v1/issues/<KEY>/events?limit=200'`, oldest first, and the next page with `&after=<the last event's seq>` while a page holds 200 events. The setter is the `actor` of the newest `issue.created` or `issue.updated` event whose `payload.labels` holds `legion` (any case) where the previous such event held none.
   - `actor.kind` is `user`: the daemon queues the root itself. Do nothing more.
   - Your own filing: its spec as filed (`dispatch doc-read --issue <KEY> --version 1`) holds a `Filed for` line, and the actor is this session (`dispatch whoami`) or an earlier session of yours. An earlier session is yours when its reply `Filed dispatch://<KEY>` stands under the ask or the evidence that the `Filed for` line names. Where that line names an ask, the reply counts only when it follows a newest answer of `File it for Legion` on that ask.
   - Any other actor's label is no request. Leave the issue in `triage`.
     Open an ask on the report issue that names the issue and who labelled it, with the options `Admit it` and `Leave it`.
     On the answer, reply under that ask `Admitted dispatch://<KEY>` or `Not admitted: <reason>`.
3. Admit the root (`set_status` to `todo`) only when you labelled it at a person's request, filed it yourself, or a person answered `Admit it`. Do not admit it when a person's request says to wait or puts it out of scope. The deployment instructions are such a request.
4. When a person's request says to wait, park the root (`backlog`, or `icebox` for a longer wait). Its label stays. Never park a root for lack of a free slot: in `todo`, the waiting line holds it.
5. Comment on each root that you admitted or parked: what you decided and why, with the request behind it. For an admitted root, name who gets its questions and its design approval: its assignee, or, with `Assignee: unassigned`, the project's default assignee until someone takes the issue from its header. Leave out the design approval when your system prompt reads `Design gate steps: none.`

## Label an issue for a person

```text
dispatch issue-update --issue <KEY> --labels-add legion
```

The daemon queues nothing for a label that a session set. So admit a `triage` root in the same turn, as step 3 says. In any other status, `set_status` the issue to `todo` only when the person asked Legion to start it now. When you file or admit an issue, name each priority it advances (`--advance <id>`, listed by `dispatch priorities`). A child needs no label: it runs in its tree.

Work that must start at a commit other than its base branch's head needs `legion/<KEY>` at that commit before Legion starts the issue: a root before admission, a child before its first phase. You push nothing. So whoever gives you the root, or the architect that owns a child's parent, runs `gh api -X POST repos/<owner>/<name>/git/refs -f ref=refs/heads/legion/<KEY> -f sha=<commit>`. Read the branch back before you `set_status` the issue to `todo`: `gh api repos/<owner>/<name>/git/ref/heads/legion/<KEY>`.
