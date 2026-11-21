# The `worker-stalled` wake

Read this on a `worker-stalled` wake of a tree's own architect (`role: "architect"`), when `skill://legion-controller/references/wakes.md` sends you here.

## `worker-stalled` (architect)

The daemon's stall backstop acted on a stalled turn of the tree's architect: it ended the turn, took the turn off its claim where the runtime did not confirm the stop, or took back its task without ending the turn where it parked the launch. The notice says what became of the process and what the owed task waits on. Where it names a parked launch, `issues.<KEY>.architect.wake` (`read_state` with `issue`) says whether that launch still waits its turn; no `wake` means it waits no more. Do nothing, unless it names the failed end (its `worker-died` follows) or a stop the runtime did not confirm, which is a finding to put to a person with the notice in it. Each stall sends one copy, so a second copy for the same architect is a finding too, with the records you were sent in it.
