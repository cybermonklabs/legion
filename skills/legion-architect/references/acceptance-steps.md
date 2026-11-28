# The steps of an acceptance

Read this when `skill://legion-architect/references/acceptance.md` sends you here; that file states the rule each section here carries out.

## A merge hold

Before every `sign_off`, read the project's merge holds: `dispatch doc-read --project <PROJECT> --artifact merge-holds-md`, `<PROJECT>` the project of the issue's key. A line under `Holds` reads `- <repositories> | <reason> | <end>`. It holds the sign-off when it names the pull request's repository, or `all`, and its end is not past: an end is past only when it is an RFC 3339 time before now, so `until the controller lifts it` and an end of any other form never are. `not found by slug` is no hold. Any other failed read decides nothing: sign off nothing, and end the turn with a WAITING line that names the failed read.

1. While a line holds the sign-off, end the turn with a WAITING line that names the hold, and subscribe to nothing for it.
2. On a `merge-holds-changed` notice, read the document again: wait again while a line holds, else accept.
3. Read the document again before you sign off on any wake.

The controller lifts a hold by deleting its line; send it no message about a hold.

## Two steps of one role

`dispatch handoff-read --issue <issue> --phase test` reads the newest tester handoff, and `--version <n>` an earlier step's: read the handoff of each tester step before yours.
No reviewer step approved the commits of an implementer step between that step and your acceptance, as in `implementer:a reviewer implementer:a acceptance:owner`.

## The last round's close

Once the children close:

1. Run each check with no `ci_check` on the default branch and record each result, yourself or by a subagent you dispatch; `wait` for a subagent's report.

2. Read each `ci_check` result in `dispatch results`.

3. Post the results in the acceptance comment, then `sign_off`.
