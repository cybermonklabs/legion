# stage3-devbox-workflow.sh's architect-closes-a-no-change-root: whether the architect's reason,
# which the daemon posts as "Closed by its architect before its first phase: <reason>", is on the
# issue before its close: the first such message comes before the first issue.closed, by seq.
# Input: the issue's events (dispatch_events). Run with -e: the exit status is the answer.
# A message its author deleted reads with deleted_at set and no body (dispatch://INFRA-1523): it
# counts as not posted, so it is passed over before its body is read.
(map(select(.type == "message.created" and .payload.deleted_at == null and (.payload.body | contains("Closed by its architect before its first phase:")))) | first | .seq) as $message
| (map(select(.type == "issue.closed")) | first | .seq) as $closed
| $message != null and $closed != null and $message < $closed
