# The `worker-stalled` wake

Read this on a `worker-stalled` wake of a tree's own architect (`role: "architect"`), when `skill://legion-controller/references/wakes.md` sends you here.

## `worker-stalled` (architect)

The daemon ended a stalled turn of the tree's architect; the notice says what became of the process and what the owed task waits on. Where it names a parked launch, `read_state`'s `wakesWaiting` says whether that launch still waits its turn. Do nothing, unless it names the failed end (its `worker-died` follows) or a stop the runtime did not confirm, which is a finding to put to a person with the notice in it. Each stall sends one copy, so a second copy for the same architect is a finding too, with the records you were sent in it.
