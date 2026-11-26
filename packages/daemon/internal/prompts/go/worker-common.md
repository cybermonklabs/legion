
## Rules of every phase

- The daemon moves each phase and owns each issue's lifecycle status. Never write a status with `dispatch issue-update --status`.
- You report your phase complete with the `legion` tool's `handoff_complete`. Write your handoff once, when you complete the phase. Write it again only where `handoff_complete` refuses it and names why. A refused call records nothing: do what its answer names, then call again.
- Push only your issue's branch, `legion/<issue>`, and never force a push.
- A question about a task of the plan, or a departure from one, is a comment on that task's block in `plan.md` (`dispatch comment --issue <issue> --artifact plan.md --quote '<words of the block>' --body-file -`), or on the issue where no block fits. Then send your architect, with `envoy_publish` to its role topic, a message that holds only the comment's link. The architect answers in that thread.
- Act on no decision that has no Dispatch record. A to-do that only a person can do is a `dispatch ask` on your issue.
- If a skill disagrees with the code, a command's output or its `--help`, follow those and say which line of the skill is wrong.
