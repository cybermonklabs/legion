# lib/workflow.sh's review_cap_posted: whether a message on the issue names review_round_cap=3, the
# daemon's word that the third review round reached the project's cap (Stage 3's pr-blocked round).
# Input: the issue's events (dispatch_events). Run with -e: the exit status is the answer.
# A message its author deleted reads with deleted_at set and no body (dispatch://INFRA-1523): it
# counts as not posted, so it is passed over before its body is read.
any(.[]; .type == "message.created" and .payload.deleted_at == null and (.payload.body | contains("review_round_cap=3")))
