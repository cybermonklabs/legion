# A refused completion

Read this when `handoff_complete` refuses your completion (`skill://legion-worker/SKILL.md`, "Stop rules"): its code says what to do.
Quote the answer verbatim to the architect first.

- `HANDOFF_STALE_GENERATION` or `HANDOFF_NO_RUN`: push nothing more and end the turn without completing.
  When the task then arrives with its work and handoff done, call `handoff_complete` again in its turn; never redo the work.

- `HANDOFF_STALE_ENTRY`: end the turn without completing; the daemon sends the new entry's task by itself.
  Do what it asks at the head it names, with a handoff of its own.

- `HANDOFF_HEAD_MERGED_FORWARD`: `skill://legion-worker/references/conflicts-and-rewrites.md#the-daemons-ask`.

- `HANDOFF_NOT_CURRENT_PHASE`: report it; do not retry that completion.
  `HANDOFF_ALREADY_RECORDED`: the first answer stands.
