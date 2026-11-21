# The `provider-errors` wake

Read this on a `provider-errors` wake, when `skill://legion-controller/references/wakes.md` sends you here.

## `provider-errors`

Six turns of `role` in a row ended in a provider error; the daemon keeps prompting. Read the role's claim in `read_state` (`issues.<KEY>.workers.<role>.claim`, or `issues.<KEY>.architect`). Name the run and its model in the day's report, and `envoy_publish` the key, role and reason to the architect that owns the issue (for a child's own architect, the one above it). Re-admit nothing and park nothing, because the task is not lost and the session still runs. When that claim still shows `providerErrorRun` at your next `tick`, it stays stuck: `compact` it, and `fresh_session` it only when a later notice says it is still refused. A dead role (`CLAIM_NOT_RUNNING`) takes `fresh_session`.
