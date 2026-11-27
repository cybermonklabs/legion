# A root the plan defers

Read this when the weekly plan says to defer a labelled root, and when a person answers a deferral ask.

A labelled root the daemon queued or runs, which the weekly plan says to defer, goes to `backlog` or `icebox` at your next pass only when the plan's line names a person's request: `set_status` to `backlog` (`icebox` for longer), citing that request in a comment. A plan row with no such request moves nothing; ask instead:

1. Find your earlier ask: read the root's comments into `notes` with the `dispatch api` of step 1 of `skill://legion-controller/references/backlog-asks.md`, then list your deferral asks:

   ```text
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then [.[] | select(.author.kind == "session" and (.body | startswith("Asked whether to defer: "))) | {id, created_at, body}] else error("Dispatch did not answer the list") end'
   ```

   Take the newest whose second line is the plan row, and `dispatch read --ref` the ask its first line names. Open or resolved with no answer, or your reply starts `Deferred` or `Let run`: do nothing. Answered with no reply of yours: step 4. Your reply starts `Not deferred`: go on to step 2.
2. Check that Legion runs the root: it carries the label, stands in `admission.active` or `admission.waiting`, and its `phase` is not `done`. If not, do nothing. Else open a `dispatch ask` naming the root and the plan row, options `Defer it` and `Let it run`, follow it, and comment on the root: first line `Asked whether to defer: dispatch://<ASK KEY>/ask/<id>`, second line the plan row exactly.
3. Name the row and the ask in the day's report while it is open.
4. On `Defer it` while step 2's check holds, move the root as above, citing the answer. On `Let it run`, move nothing and take the deferral off that plan row with `dispatch doc-edit`. Reply under the ask, first line exactly `Deferred dispatch://<root>`, `Not deferred dispatch://<root>` or `Let run dispatch://<root>` (`dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -`).
