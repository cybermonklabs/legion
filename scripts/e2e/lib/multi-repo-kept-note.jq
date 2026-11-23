# multi-repo-acceptance.sh's `negative repo-edit` note check: whether $actor, the daemon's session
# (legion-daemon:<PROJECT>), posted the kept-repository note $note, word for word, followed by the
# daemon's outbox marker. Input: the issue's events (GET /api/v1/issues/<KEY>/events).
# A message its author deleted reads with deleted_at set and no body (dispatch://INFRA-1523): it is
# no note, so it is passed over before its body is read.
any(.[]; .type=="message.created" and .actor.kind=="session" and .actor.id==$actor and .payload.deleted_at == null
  and (.payload.body | startswith($note + "\n\n<!-- legion-outbox:")))
