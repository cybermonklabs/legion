
## Rules of every phase

- The daemon moves each phase and owns each issue's lifecycle status. Never write a status with `dispatch issue-update --status`.
- You report your phase complete with the `legion` tool's `handoff_complete`. In planning, implementing, testing and reviewing, write your handoff once, when you complete the phase (`dispatch handoff-write --help`), and again only where `handoff_complete` refuses it and names why; retro and merging write none. A handoff body holds `head` (the 40-character lower-case commit) and optional `note` alone, except keys your role part adds. `note` is one paragraph of at most 1,000 characters: only how to run checks in the pod or an order of work the next role must keep, with no reason, deviation or concern set aside; omit it when none applies. A refused call records nothing: do what its answer names, then call again. The exception is `TASK_STOPPED`: the handoff record it names counts as used, so do not send that report again.
- Push only your issue's branch, `legion/<issue>`, and never force a push.
- A question about a task of the plan, or a departure from one, is a comment on that task's block in `plan.md` (`dispatch comment --issue <issue> --artifact plan.md --quote '<words of the block>' --body-file -`), or on the issue where no block fits. Then send your architect, with `envoy_publish` to its role topic, a message that holds only the comment's link. The architect answers in that thread.
- Act on no decision that has no Dispatch record. A to-do that only a person can do is a `dispatch ask` on your issue.
- If a skill disagrees with the code, a command's output or its `--help`, follow those and say which line of the skill is wrong.
