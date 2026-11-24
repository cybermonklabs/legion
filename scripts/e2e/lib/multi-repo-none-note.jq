# multi-repo-acceptance.sh's `negative no-repo` note check: whether the latest repository note
# among the messages of $actor, the daemon's session (legion-daemon:<PROJECT>), is the exact
# none-note $none followed by the daemon's outbox marker; false when the daemon posted no note.
# A repository note opens with "Legion did not admit this issue:" or "Legion keeps ". Input: the
# issue's events (GET /api/v1/issues/<KEY>/events).
# A message its author deleted reads with deleted_at set and no body (dispatch://INFRA-1523): it is
# no note, so it is passed over before its body is read, and the latest standing note decides.
[.[] | select(.type=="message.created" and .actor.kind=="session" and .actor.id==$actor and .payload.deleted_at == null
  and ((.payload.body | startswith("Legion did not admit this issue:")) or (.payload.body | startswith("Legion keeps "))))]
| sort_by(.seq) | last
| if . == null then false else (.payload.body | startswith($none + "\n\n<!-- legion-outbox:")) end
