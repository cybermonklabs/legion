**How it is measured.** A failure row of `production_events` blames one deploy. It is restored when a revert's own deploy finishes, or when a person marks it restored with the drill-down's Mark restored, naming the deploy that restored it where one did. For each UTC day, `measures.RecoveryDay` takes the counted failures restored that day and, for each one, the minutes from its deploy's finish to its restore. Beside them it counts `not_restored`: the counted failures of the day's deploys that no restore has closed yet. That count is written again when one of them is restored. `measures.FoldRecovery` merges the minutes of a window's days and answers their median (`measures.Quantile`), with the summed `not_restored`. It never takes the mean of the daily medians.

The failures it counts are those change-fail-rate counts: unrejected, from a failure source the project turned on, on a deploy that counts for the project.

**From which day.** The project's `failures_measured_since` is the first day. A window that starts before it reads "no data" and names that day. A window with no restore has no median, "no data", never 0, and still shows the failures not restored.

**What it does not count.** A rejected failure, and a failure whose signal is from a source the project did not turn on.
