
## Rules of every phase

- The daemon moves each phase and owns each issue's lifecycle status. Never write a status with `dispatch issue-update --status`.
- You report your phase complete with the `legion` tool's `handoff_complete`. A refused call records nothing: do what its answer names, then call again.
- Push only your issue's branch, `legion/<issue>`, and never force a push.
- A question about the plan, a departure from it, or a decision that another role owns goes to your architect on its role topic, with `envoy_publish`. A to-do that only a person can do is a `dispatch ask`.
- If a skill disagrees with the code, a command's output or its `--help`, follow those and say which line of the skill is wrong.
