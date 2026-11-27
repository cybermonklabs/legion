# multi-repo-acceptance.sh's `negative repo-edit` note check: whether $actor, the daemon's session
# (legion-daemon:<PROJECT>), posted its note for a change of repository it cannot apply yet
# (noteRepositoryChangeWaits, in packages/daemon/internal/workflow/repository.go), followed by the
# daemon's outbox marker. The note opens with $opening, which every variant shares and which holds
# the baseline's own spelling of the kept repository, and its closing sentence is $lead, X and a
# full stop, where X is Dispatch's spelling, so only X is compared with $x, case-insensitively.
# What lies between names the refusal, which depends on why the deployment does not serve X.
# Input: the issue's events (GET /api/v1/issues/<KEY>/events).
# A message its author deleted reads with deleted_at set and no body (dispatch://INFRA-1523): it is
# no note, so it is passed over before its body is read.
def closing:
  (($lead | length) + ($x | length) + 1) as $n
  | length >= $n
    and (.[-$n:] as $t
      | ($t | startswith($lead)) and ($t | endswith("."))
        and (($t[($lead | length):-1] | ascii_downcase) == $x));
any(.[]; .type=="message.created" and .actor.kind=="session" and .actor.id==$actor and .payload.deleted_at == null
  and (.payload.body | startswith($opening))
  and (.payload.body | contains("\n\n<!-- legion-outbox:"))
  and (.payload.body | split("\n\n<!-- legion-outbox:")[0] | closing))
