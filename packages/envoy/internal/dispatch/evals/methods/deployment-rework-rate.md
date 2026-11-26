**How it is measured.** A deploy is rework when it restores a counted failure: it is a revert's own deploy, or the deploy that a person names with the drill-down's Mark restored (`production_events.restored_by_deploy`). For each UTC day, `measures.ReworkRateDay` builds one share, `deploys`: the deploys that finished that day and restored a counted failure, over every deploy that finished that day. `measures.FoldReworkRate` adds up the numerators and the denominators of a window's days and divides the sums. It never takes the mean of the daily shares.

The deploys and the failures it counts are those change-fail-rate counts: a deploy that counts for the project, and a failure that is unrejected and from a failure source the project turned on.

**From which day.** The project's `failures_measured_since` is the first day. A window that starts before it reads "no data" and names that day. From that day, a window with deploys and no restore reads 0 percent.

**What it does not count.** A deploy that restores only a rejected failure, or only a failure from a source the project did not turn on.
