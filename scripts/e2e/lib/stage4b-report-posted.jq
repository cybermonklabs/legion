# stage4b-sandbox-tree.sh's report_posted (daily-report): whether the controller's session $s
# posted a message that names tree 1's root $tree and the free slots, within the skill's
# 2,000-character budget for the report, which is the report's own length rule and not the
# Dispatch server's body cap. Input: the report issue's events (dispatch_events). Run with -e: the
# exit status is the answer.
# A message its author deleted reads with deleted_at set and no body (dispatch://INFRA-1523): it
# counts as not posted, so it is passed over before its body is read.
any(.[]; .type == "message.created" and .actor.id == $s and .payload.deleted_at == null and (.payload.body | test("(^|[^0-9A-Za-z-])" + $tree + "($|[^0-9])")) and (.payload.body | test("slot"; "i")) and (.payload.body | length) <= 2000)
