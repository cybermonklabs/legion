# Wakes: a person paused or resumed the project

Part of `skill://legion-controller/references/wakes.md`.
Read this when a `project-paused` or `project-resumed` notice reaches you.
The daemon sends each for a person's `legion project pause` or `legion project resume`; your own `pause` or `resume` sends you none.

## `project-paused`

The notice names the pause: who paused it, when, and why.
While the project is paused, the daemon admits no issue, and it launches no session and hands no task to any role but you.
Each other role stops: an idle one at once, and one in a turn when that turn ends.
Your own tasks, notices and `tick` still run.

1. Read the pause in `read_state`'s summary: its `paused:` line names the roles the pause still waits on and how many rows it holds.
2. Start nothing for another role while the pause stands: `fresh_session` and `compact` answer `PROJECT_PAUSED` for any role but yours.
3. Answer people as usual. A person made this pause, so `resume` it only when a person asks you to: a resume starts every role the pause held something for.

## `project-resumed`

The notice names who ended the pause, and when.
The daemon launches each role the pause held something for at its wake pacing, never all at once, and the other stopped roles wait for their next wake.
Read `read_state`, and handle each wake by its row in `wakes.md` as it arrives: the pause held none of yours, and the resume sends you no wake but this notice.
