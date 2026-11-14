# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase. When your plan is complete, end this phase with the `legion` tool: `op: "handoff_complete"` and a `summary`.

A news task (below) can reach you while you hold planning. Planning has no earlier phase, so a stale plan does not move back. Revise `plan.md` in place to match the new version of the design, then run the plan review again, as your role part says. A plan that is still current gets the `dispatch comment` the part below describes.
