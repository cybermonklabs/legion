# Decisions kept for a tree out of the workflow

Read this on a `kept-decision` wake, and for the walk of roots out of the workflow at every start and on each `tick`. Such a root is a labelled root in `backlog` or `icebox` whose `issues.<KEY>.phase` is `done`. Its architect is gone, so nobody in it hears a person's decision. The daemon sends you each one as `kept-decision` (`decision`, `root`, `ref`, `actor`, and a spec decision's `version` and `reason`); the walk finds those sent while no controller ran.

A person's own approval of such a root's primary document while no tree is live is no decision for you: the daemon sets the root `todo` itself, or sends a `kept-decision` whose `reason` says the root's status changed after the approval: decide it below. A carried approval, or one on a root that keeps a Dispatch parent, is a decision. A session's reply on such a tree is none.

A held root is not out of the workflow: `todo` on a root at `phase: held` releases the hold at the same generation. A root still in the workflow whose architect stopped takes `backlog`, then `todo`, which re-admits the tree.

## Deciding

1. **Where the tree stands.** Read `issues.<root>`.
   - `phase` is not `done`: the tree runs. When its `architect.state` is `failed` or `retired`, nobody heard: judge it in steps 3 and 4, and re-admit a tree whose decision gives a next step with `backlog`, then `todo`, once the `worker-died` cause is gone (`skill://legion-controller/references/wakes.md`). In those comments write `<root>'s architect has stopped, so it` for `<root> is in <status>, so its architect` and `The tree waits.` for `The tree stays in <status>.`. Otherwise its architect runs again by the time you read it and reads the decision itself: take nothing.
   - `phase` is `done`: `dispatch read --issue <root>`. In `backlog` or `icebox`, go on. In any other status, set nothing and name `ref` in your summary.
2. **Who moved it.** Under the root's `Events:`, each `issue.created`, `issue.updated` and `issue.closed` line ends `status <status>`, except that a forced create adds `· forced …` after it. The move is the oldest line of the newest unbroken run of those lines showing the current status. It is your own hold only when that line is by `session legion-daemon:<PROJECT>` and, within 10 minutes after it, a session's comment begins with the wait mark of `skill://legion-controller/references/backlog-asks.md` step 4. This lists them, older wordings included:

   ```text
   notes=$(dispatch api --path '/api/v1/issues/<root>/comments') || { printf 'Dispatch read failed: %s\n' "$notes" >&2; exit 1; }
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then [.[] | select(.author.kind == "session" and (.body | test("^([A-Za-z]+ ){1,3}by Legion while it waits on:"))) | {id, created_at, mark: (.body | split("\n")[0])}] else error("Dispatch did not answer the list") end'
   ```

   Every other move is someone else's. `Events:` shows only the last 10 events: when the run reaches the oldest one shown, read `/api/v1/issues/<root>/events?order=desc&limit=200` with the `dispatch api` of `skill://legion-controller/references/session.md`; when it still does, you cannot read who moved it, so judge only the events newer than the oldest you read, and name older decisions in your summary as unplaced. A decision older than the move is no reason to run.
3. **Read once.** A decision-read comment from any session, newer than the decision, on the root or the decision's issue, has judged it: do nothing. With that issue's comments in `notes` (step 2), this lists them, older wordings and header-led ones included:

   ```text
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then [.[] | select(.author.kind == "session" and (.body | test("(^|\n)[A-Z][a-z]+ decision read:"))) | {id, created_at, ask_id, reply_to}] else error("Dispatch did not answer the list") end'
   ```
4. **Judge the words, not the kind.** Read the decision (`dispatch read --ref <ref>`; for an approval, `issues.<root>.designGate` and `gatedSteps`). An approval and its approval request's `ask.answered` are one decision, `ref` the approved document's (`dispatch://<KEY>/spec` for the spec).
   - It gives the work a next step: an approval (every `gatedSteps` entry `approved`; an `approvedVersion` below `currentVersion` counts), a change request that says what to change, an answer that picks an option or supplies the fact, a reply that tells the agent to go on.
     - Your own hold: `set_status` the root to `todo`, and comment `Kept decision read: <ref> by <login> gives this tree its next step (<one sentence>). Legion runs it again.`
     - Another's move, or one you cannot read, whose words ask Legion to start the tree: `todo`, and comment `Kept decision read: <ref> by <login> asks Legion to start this tree. Legion runs it again.`
     - Another's move otherwise: set nothing, and comment `Kept decision read: <ref> by <login> gives this tree its next step (<one sentence>).`, then `<mover> moved the tree to <status>, so it stays there until a person sets it to todo, approves its spec when it has no parent, or asks Legion to start it.` (the login, `session <id>`, or "The tree was moved to <status> through Legion (session legion-daemon:<PROJECT>) for no hold of its own"), then, for an ask's answer or reply, `Its architect then starts with this decision.`
   - It only questions the work: move nothing, and reply `Kept decision read: <ref> by <login> asks <the question in a few words>.`, then the answer and where in the record it is (issue, spec, handoffs, pull request) and `The tree stays in <status>. Set <root> to todo to run it.`; or, when the record does not answer it, `The record does not answer it, and <root> is in <status>, so its architect cannot answer either. Set <root> to todo to bring its architect back to answer.` For another's move, end with that move's sentence instead of `Set <root> to todo …`.
   - Post on the issue where the decision was made: under its ask, `dispatch comment --issue <KEY> --reply-to-ask <ask id> --turn human --body-file -`; under a reply, `--reply-to <comment id>`; for a change request, a plain comment on that issue. Post a short copy on the root when that issue is not it. Several decisions on one tree get one comment naming each `ref`.

## The walk

1. List the roots out of the workflow: `dispatch issues --project <PROJECT> --status backlog --label legion --all`, then with `--status icebox`. Keep those `read_state` records at `phase: done`.
2. Read each root, then each issue under it, level by level (`dispatch issues --project <PROJECT> --parent <KEY> --all`).
3. A decision is an event by a `user`, newer than the root's move and every decision-read comment on its issue ([Deciding](#deciding) step 3), of type `artifact.approved`, `artifact.changes_requested`, `ask.answered`, `ask.resolved` or `comment.created`. Skip an `artifact.approved` that is the person's own handover: the root's `parent` is `null`, the event's `payload.artifact_id` is the root's `primary_artifact_id` (the same `dispatch api` of `/api/v1/issues/<root>`), and its `payload.reason` does not begin `Carried forward from the approval of version`. When that read fails, judge it. Find an ask or comment's id under `Open asks:` or with `dispatch search`; `dispatch read --ref dispatch://<KEY>/ask/<id>` prints its `Ask id:`.
4. Decide each tree's decisions as [Deciding](#deciding) says.
5. For each root that is your own hold and had no decision this walk, check its wait: for an `event:` key, read what its reference names; for any other mark, judge the cause by step 3 of `skill://legion-controller/references/escalation.md#an-escalated-hold` (a missing `capabilities` row proves nothing: such a tree runs no session). When the event has happened or the cause is gone, `set_status` the root to `todo` with a comment that cites the mark and what you checked. Otherwise move nothing, and name the root and its wait in your summary.
