# Architecture

## Transport

```text
GitHub webhook  --\
Slack events    ---+--> NATS ── JetStream ──> listener ──> session wake / cold resume
Agent messages  --/       |
                            +── core NATS role lane ──> listener arbitration ──> current role holder
```

## Core pieces

- `packages/contracts`
  - envelope schema
  - env parsing
  - subject helpers
  - signing / verification helpers where envoy owns them
- `cmd/listener`
  - machine-scoped consumer
  - routing and dedupe
  - hot wake vs cold resume
- `cmd/github`
  - webhook verification
  - event normalization
  - NATS publish
- `cmd/slack`
  - request verification
  - challenge handling
  - event normalization
  - NATS publish

## Runtime shape

- one listener container per machine
- one GitHub receiver container on the public EC2 host
- one Slack receiver container on the public EC2 host
- NATS cluster across all machines via Tailscale mesh, with JetStream for durable notifications and core NATS for live role routing
- JetStream stream `ENVOY_NOTIFICATIONS` with 72h retention for non-role notification replay on listener restart, and an optional byte limit (`ENVOY_STREAM_MAX_BYTES`) past which it refuses new publishes (`DiscardNew`; the sender gets the retryable `bus.ErrStreamFull`, which the webhook answers 503) and keeps every stored message for its 72h, so the limit is sized above the stream's 72-hour volume; per-machine durable consumers carry a 7d inactive threshold so the server garbage-collects consumers of decommissioned listeners. A restart resumes the machine's durable at its cursor. A durable the listener creates (under a new machine id, or after its durable was deleted) starts at deliver policy new, so it replays nothing stored before the create: it reports no wake and forwards no role message for an older message, and a deleted durable's unacknowledged messages are skipped (`packages/envoy/deploy/README.md`, "Recreating a listener's durable")
- Role lanes and delivery-exception lanes for role topics use core NATS; they have no durable consumer, retention, or replay. Stream reconciliation excludes role subjects and purges any role message captured while that configuration is updated. The listener forwards a role message to its holder's agent subject, which the stream stores, so a holder on consumer delivery gets the forward through its session consumer also after a restart.
- Session consumers: a session that asks at registration (`delivery`) gets a durable pull consumer `s-<session id>` on the stream, filtered on its agent subject and its stored followed topics; the listener that owns the session relays it in stream order on `notifications.envoy.deliver.<session id>` (core NATS, not stored) and acks a message only on the session's `done`, which the session sends once it has written the message. The one exception is a stored message whose frame (its bytes with the relay's headers) is past the server's max payload: the listener acks it with one WARN naming the session and the event, and the session does not receive it, so that message does not hold back the messages after it. A filter built from a stored row (a relay restart, an unsubscribe, a takeover of the session's role) holds only the topics one judgment keeps (`storedTopicFiltered`, then the row's merges): it drops another session's private stream subject (its inbox, a failed-delivery lane, or a wildcard reaching one), a Legion controller notice lane whose role the session does not hold now, as that role's claim record on the role bucket stream's leader says (`store.Registry.ReadLaneHolders`, `store.LaneHolders.Keeps`), a role lane, a topic the server refuses as a filter subject, a topic that does not begin with `notifications.`, and a wildcard that feeds a merge reaching another session's subjects; it drops no topic for what the deployed stream captures now. Of those drops only the last two are logged. A publish's `recipients` counts a session only through a topic that same judgment keeps. A claim that takes a Legion controller role from another session filters that session's consumer on its stored row again before the claim is answered, inside the claim route's 3.5 s budget, so a notice published on the role's lane after the answer reaches the new holder alone; a narrowing that did not land is logged at WARN. That consumer is the only way a message reaches a session: no plain NATS subscription of the session's own and no fallback path carries one. The consumer is deleted when the session's registration ends (`DELETE /v1/sessions/<id>`) and by the listener's periodic sweep over the sessions the registry judges gone (`ENVOY_SESSION_CONSUMER_SWEEP_INTERVAL`, 5m by default, at most 256 a pass); before each delete the consumer's ack floor is recorded on `notifications.envoy.resume.<session id>`, a lane of the stream no consumer filter takes, so a session that comes back with no resume point of its own is created again there. The inactive threshold, which each listener derives at start from its own windows so that it stands above that sweep bound (`bus.InactiveThresholdFor`; 1h floor), is only the server's backstop for a consumer neither delete reached.
- Session replies: each relay frame's reply subject is the listener's reply inbox, then the session's id, then a token of 128 random bits (`bus.SessionReplySubject`, `crypto/rand.Text`), so no session can work out the token of a frame sent to another from its own. The relay moves a frame only on a reply whose subject names the session the frame was sent to: a reply that names another session, on the frame's token, moves nothing (`sessionRelays.onReply`). The first such reply is logged at WARN, then at most one a minute, each line counting in `held` the replies held back before it (`relayRefusedReplyLogEvery`). The send route's connection probe and Dispatch's agent stream replay (`agentstream.NATS.Replay`), the other two requests a session answers, ask on such a subject too, each on a reply inbox of its own, so neither reaches the relay's reply handler.
- Session trust: a session the NATS auth service admits holds a grant of its own (`natsgrant.Session`): it subscribes its own relay subject and control subject alone, and publishes its replies under its own id alone (`bus.SessionReplies`, `_INBOX.*.<session id>.>`), so it can neither read another session's frames nor answer one. A holder of the shared session user (`core.SessionPermissions`, a `gcp:NatsHub`'s `sessionUser`) or of a host user (`core.HostPermissions`) can still read any session's relay subject and publish `done` to the reply subject a frame carries, which acknowledges that session's message away, since the relay reads the session the subject names and not who published the reply. That stays open while a hub renders either user.
- The stream and every KV bucket hold the deployment's replica count, `ENVOY_NATS_REPLICAS` (1 to 5, default 1; more than 1 needs a clustered NATS), and the stream at least the byte limit `ENVOY_STREAM_MAX_BYTES` when set: a start creates them at that count and limit, raises one found with fewer copies or a smaller limit (or none), and never lowers either, and every program that owns the stream (each listener, the Dispatch server) runs a keeper that every 30 seconds raises the stream's copies and byte limit back to its settings, puts back the discard policy and `envoy_stream_id`, and raises its own buckets' copies. An operator's raise of the limit stands, and programs with different settings do not fight: the largest stands. JetStream's stream update has no compare-and-swap, so a start with other or no settings can put back what it read until the next keeper pass; a clustered deployment sets the same `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES` in every program
- JetStream KV bucket `envoy_interests` for session subscriptions. A subscription too large for one NATS message (the server's max payload) is stored in chunks under `=chunk.<generation>.<index>` and a manifest under the session's id, written last; readers join and check the chunks, so they read the old subscription or the new one whole (`internal/kvchunk`). The known-session records of `envoy_known_sessions` are stored the same way
- JetStream KV bucket `envoy_sessions` for session host data, plus the `plugin` and `plugin_version` each registration names (the npm package the session registers through and that plugin's build; absent for a session whose plugin predates them). A listener that finds no bucket creates it with a `MaxAge` of 2h, the default gone horizon plus one hour (`session.BucketMaxAge`). `ENVOY_SESSION_MAX_AGE` sets the bucket's `MaxAge` at start; unset, the listener leaves an existing bucket as it is. That age bounds how long a record survives, not how long a session is alive: liveness is computed from each record's `UpdatedAt` (see “Session state”), never from the presence of a key
- JetStream KV bucket `legion_agents`, no TTL and never deleted, for the Legion agent registry: one entry per Legion session the daemon ran, under `session.<id>` and the same entry under `issue.<ISSUE>.<id>` and `role.<role_token>.<id>` (`project.*` is the daemon's own and no listener read reaches it)
- Each listener serves both buckets from an in-memory copy its watcher keeps (`internal/kvwatch`), and the role claims of `envoy_roles` too. A watcher reads its bucket again on a reconnect, a recreated bucket or a failed watch without emptying the copy: every row stays until the new read completes, each delivered record replaces its row as it arrives, and only the completed read removes the rows it did not find. An incomplete read removes nothing, and the listener reads again itself. The role claim watcher also reads its bucket in full one minute after each read that completed, by the same rules, so a claim the copy lost is put back and one the bucket no longer holds is taken out within 2 minutes. No act decides from that copy: each reads the role's claim record from the bucket when it acts.

## Listener API

All `/v1/*` endpoints answer 503 `service starting` until NATS is connected and the interest and session caches are warm; they do not wait for the durable consumer. The two list reads, `GET /v1/sessions` and `GET /v1/interests/`, answer that time with the refilling 503 the routes themselves give (below).
A listener's caches follow their buckets through watchers (`internal/kvwatch`): the interest,
session and role claim buckets, and the CI bucket on a listener that mounts the GitHub webhook
route. A start reads each bucket in full, and so does a rewatch after a NATS reconnect or a
self-health rebuild (`kvwatch.Watcher.Rewatch`), which resumes from no revision: the copy keeps
every row while the new read runs, each delivered record replaces its row as it arrives, and only
a completed read removes the rows it did not find, so an incomplete read removes nothing and the
listener reads again itself. A rewatch that cannot be created is logged at WARN. The role claim
watcher also reads its bucket in full one minute after each read that completed
(`kvwatch.WithScheduledRead`, `roleClaimRepairInterval` in `internal/store/role_claim_repair.go`),
by the same rules, and that read repairs both directions of its copy: it puts back a claim the
copy lost and takes out one the bucket no longer holds. Every completed read counts, the start's
and a rewatch's included, and the bound on the age of the last one is 2 minutes
(`store.RoleClaimRepairBound`, no setting). `/healthz` reports that read as `role_claim_repair`
(below), and `/metrics` as `envoy_role_claim_repair_completed_timestamp_seconds`,
`envoy_role_claim_repair_put_back` and `envoy_role_claim_repair_took_out`, with no sample before
the first read completes. The copy decides no act: the role lane, the kept lane's handover and
sweep, the renewal, the deregistration release and the controller notice lane each read the
role's claim record from the bucket. JetStream
delivers no event when the session bucket's `MaxAge` expires a key, so the session cache gives
each entry a deadline one `MaxAge` after its write, and a timer removes the entry at that
deadline. That deadline bounds the cache, not a session's life: state is computed from the
record's `UpdatedAt`, and an entry the cache no longer holds is read through to the bucket, where
a record the bucket does not hold is `gone` and nothing else.
A list read answers `503` with `Retry-After: 5` and `{"error":"envoy: the agent list is refilling; try again in a moment","code":"refilling"}` while a session or interest copy it reads cannot be served: before that copy's first complete read, and once it has not followed its bucket (a read under way, an ended watcher, a recreated bucket, a lost NATS connection) for longer than 2 minutes, or half the bucket's `MaxAge` when that is shorter. A list from such a copy could be missing a live agent that a caller would take for absent. The `envoy_active_sessions` and `envoy_active_interests` gauges have no sample then; `0` is only ever a complete empty copy.
Every `/v1` request with a body must set `Content-Type: application/json` (parameters such as
`charset=utf-8` are accepted). The listener returns a JSON 415 error otherwise. Webhook routes
retain their signature and content handling. Every route that reads a body reads it to a limit,
and a longer body is a 413 (see “Request body limits” below).
A listener with no bearer configured serves `/v1` only to a `Host` that names it: loopback, or a
name in `ENVOY_ALLOWED_HOSTS` (port stripped, case folded, one trailing dot dropped). Any other
`Host` gets a JSON 421, so a DNS-rebinding page cannot reach the bearer-free API. A listener that
requires a bearer checks no `Host`.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/sessions` | GET | Lists the `live` sessions across all machines, and the `unresponsive` ones too when asked with `include=unresponsive` — joins `envoy_interests` (topics, dir) with `envoy_sessions` (self-subscribed delivery mode, `plugin`, `plugin_version`, `client_instance_id`), and reports as each session's `roles` those whose claims in `envoy_roles` name it, from the listener's watched view of that bucket, whatever role topics its record carries. Every row's `port` is 0, since a registration naming a port is refused and the field stays only because every released client parses it. Each row carries the session's `state` (`live` or `unresponsive`), its `last_seen`, computed from its registration's `UpdatedAt`, and `client_instance_id`, the client process that wrote that registration, left out where the entry carries none: a row and the single answer of `/v1/registry/{session_id}` are one shape built one way (`sessionInfoWith`), so every field of one is a field of the other. The list is not itself the liveness answer, and a row's absence is not death. A cache view, and a list cannot be made authoritative key by key, so a caller holding one session id asks `/v1/registry/{session_id}` instead. Answers the refilling 503 while either copy cannot be served. A row's topics come from the listener's interest copy alone (`store.Registry.Cached`, `listedSessionInfo`), with no read of the bucket per row, which over a slow KV leader would hold the answer for seconds per row; a copy that can be served has completed a read of the whole bucket, so a session it holds no interest record for subscribed to nothing. Such a session is listed with `topics` as `[]`, never `null`, as `packages/contracts/fixtures/envoy-session-no-interests.json` states, and so is one whose record's topics are null and one whose id the interest bucket refuses as a key (an earlier listener's id past its get bound that `envoy_sessions` still lists, or one outside nats.go's key alphabet) that the copy holds no record for. The roles of every row are read in one pass (`store.Registry.RolesBySession`), and a claims view that is not complete is a read that did not answer: a 503 with `Retry-After: 1`, never a row with no roles. The claims view answers only while it is complete (`store.Registry.RolesHeldBy`): the role claim watcher's first scan delivered every claim, and after a recreated bucket's reset only once the refill's scan has; until then it is `store.ErrRoleViewNotReady`, never a holder without its role. A scan kvwatch's own idle wait ended is read again 5 s later (`rereadDelay`) until one completes. The view can drift from the bucket while it follows it, so the role claim watcher reads the bucket whole again a minute after each read that completed: a role a row lacks, or still names after its claim moved, is right again within 2 minutes (`store.RoleClaimRepairBound`), and the list keeps answering from the view meanwhile |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` marks a session that holds its own NATS connection, one the listener relays its consumer to ("Session consumers" under "Runtime shape", above, states how a session's messages reach it). A registration whose `port` is above 0 is refused `400 {"error":"port is not supported: …","expected":["port"]}` before anything is stored: the listener delivers to no port. A stored record this build cannot decode (`store.ErrMalformedInterest`) is replaced, not merged, logged at WARN by the registry; the session's role claims are kept. A write route (see “Write route answers” below). A consumer call that meets a NATS reconnect answers `503` with `Retry-After: 1` and a body that opens `envoy: the message bus did not answer` and names `bus: NATS is reconnecting`, inside 3.5 s of the request's arrival (`sendRequestBudget`); the pass that meets it writes nothing, since its consumer call comes before its row. Each read of the connection's state made once the budget has ended can add up to 100 ms (`ConnLockWait`), and a send or subscribe that meets a handshake begun just after its read waits for that attempt. A client of this release retries it, holds it for two minutes from the first such answer, and then tells its session that the registration is not recorded for certain; a pi-envoy from 17.8.0, or a claude-envoy, of a release before this one tells it at once as a session with no durable consumer that receives nothing. A `notifications.` topic that neither the deployed stream nor this build captures, and that is no role lane, is left out of the stored row and named in `warnings`, the registration's other topics stored and the answer still 200, since nothing would ever be delivered on it and refusing the whole registration would leave that session no consumer at all; a caller that names one itself is refused by name instead, at `/v1/messages/publish` and at either plugin's subscribe tool, and told to follow a `notifications.team.<name>` topic. A topic this build stores is kept also while the deployed stream does not capture it, as during a hub move's quiesce, and the session's consumer filter drops no topic of its stored row for what the deployed stream captures, so a message stored on such a topic once the stream captures it again is delivered. A topic that reaches a private stream subject other than the session's own agent subject (another session's `notifications.agent.<other>`, a failed-delivery lane `notifications.envoy.exceptions.notifications.agent.<id>` including its own, a Legion daemon notice under `notifications.legion.>` such as the controller lane `notifications.legion.<project>.controller`, or a wildcard such as `notifications.agent.>`, `notifications.envoy.>` or `notifications.>`) is not stored and is named in `warnings`, so neither the fan-out nor a session's own consumer hands a session another session's inbox, a failed delivery's payload or the controller's notices. Two more kinds of topic are left out of the session's consumer filter, and so of the stored row: a topic that does not begin with the literal `notifications.` (the stream stores nothing outside it, and one with a wildcard first token, such as `*.team.foo`, would widen the filter through the reduction below), and a wildcard topic that fed a merge of the filter's reduction reaching another session's private subjects (the server takes no overlapping filters, so two wildcards that each pass the rule above alone, such as `notifications.envoy.exceptions.foo.*` and `notifications.envoy.exceptions.*.bar`, would reduce to `notifications.envoy.exceptions.>`). Each is named in `warnings` with the reason on every registration answered 200 and logged at WARN (`listener left a topic out of the session's consumer filter`, with the session and the topic) at most once per session and topic an hour, the stored row's own copy from an earlier build included, so no publish counts the session as a recipient for a topic its consumer never delivers; until that session's next registration, a publish counts no session as a recipient through such a topic of a row or a known-session record an earlier build stored, and the durable path skips such a session for a message it matches only through one, logging the same line (`from` `fanout`, with the subject) at most once per session and topic every ten minutes. A change of a session consumer's filter, an update in place or a restart at the ack floor that deletes and creates it again, that drops a subject no subject of the new filter contains while the stream holds messages on it from the consumer's position on passes them over (the server goes on from that position with the new filters), and the same line names it (`from` `narrowing`, `topics` the dropped subjects, `pending_by_topic` the messages on each and `pending_passed_over` their sum): an update's once the server took it, from the consumer's next sequence, and a restart's once its delete landed, from its ack floor plus one, before its create, so a create that fails leaves the loss named. A restart whose delete brings no answer (a timeout, a lost connection, an answer that does not decode), which the server may have applied, reads the consumer back within the restart's own bound, for at most 2 s: a consumer gone gives the line as a landed delete does, one still there gives none (the next restart counts from its floor), and a read back that fails too, one the end of the restart's bound leaves unsent included, gives the line with `delete_confirmed` `false`; an answer the server sent, its refusal under the grant included, is definite and gives none. Each count is read on its own subject just before the change, so a message stored on the subject or delivered from it between that read and the change makes the real loss differ from it by one; a widening, which drops a subject for one that contains it, passes nothing over and logs nothing. A create where no consumer stands counts nothing, since nothing records the filter of the consumer it replaces: the resume record of a consumer the listener deleted holds its floor and the stream's identity alone, the server's own removal of an idle consumer records nothing, and a restart's retry at its pending start reads no consumer. Such a create passes over at most the messages the stream holds on the subjects the replaced consumer carried and its filter does not contain, from its start (the resume floor plus one, the pending start, or the caller's) to the stream's last sequence at the create; a restart's retry, only on subjects its filter drops beyond those the restart's own line counted. An optional `delivery` object (`consumer`, `epoch`, `resume`) asks for a session consumer, created or refiltered before the call answers. A registration of `@sjawhar/pi-envoy` 14.1.0 or later that asks for none is given one as if it had asked, with no `epoch` and no `resume`, since every such build reads its relay subject whether it asked or not ("Session consumers" under "Runtime shape", above). The answer's `delivery` says `consumer` (with the relay `subject` and `stream_id`), and a registration the listener can give no consumer is refused by name — `503 no_grant`, `consumer_limit` or `consumer_error`, `413 too_large` for a filter whose request is past the server's max payload, `409 not_requested` or `invalid_session_id` — with nothing written (`subscribeHandler` in `cmd/listener/api.go` decides before either write; the reasons are in `cmd/listener/session_delivery.go`). A registration is answered `200` only once the session's known-session record is written (`answerRegistration`, `cmd/listener/api.go`), the registration given a consumer it did not ask for included, with two exceptions below. That write comes last, fenced on the registration's admission, and has a bound of its own, one second, armed as the write starts (`knownRecordWriteTimeout`). A write that does not land in that bound is answered as the bus-unavailable `503` with `Retry-After: 1`, which every client retries, and so is any other write nothing answered, and a failed write whose leader read after it nothing answered, whatever the bucket's leader holds; nothing the registration stored is undone. A write the bucket refused while its leader holds no record of the session, or one naming no recorded principal, gets the store's own refusal (`413` or `400`), which the client does not retry. A write the bucket refused while its leader names another principal, and a write that a record written since the admission beat (`session.ErrPrincipalUnrecorded`), are answered `503` with `Retry-After: 1` naming it, and the client registers again. The first exception is a write the bucket refused while its leader holds the session's record naming this registration's principal (`session.ErrRecordStands`): that is a WARN beside the `200`, since the session stays recorded as the principal it registers as. The second is a listener whose known-session bucket is not open, which writes no record and still answers `200` |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session and answer in `removed` every topic it named that the record no longer holds, with every other form of them the record held, and never a role lane topic the record kept, so a named topic the record never held is in it too; a consumer-delivery session's consumer stops filtering on them. A stored record this build cannot decode is left as it is (it routes none of its topics), logged at WARN. A session the bucket holds no record for has nothing removed, and the route answers 200 with `removed` as `[]`. The request carries `client_instance_id`, the process handing the interests back, and that decides which role claims it frees: of the roles the topics name, only the claims naming that same process, so a second process that resumed this session's id frees none of the holder's. An absent process matches nothing: a release that states no process frees no claim, and a claim that names no process (one whose claimant stated none: an older client's, or a session pod's claim of its launch role) is freed by no release this route makes; the role claim reaper frees it. A role lane topic whose claim this release did not free stays in the record and is left out of `removed`, because one record serves every process running under one session id and that topic is the interest half of a claim that still stands, written there by the claim itself; no delivery rides on it, since a role lane never enters a session's consumer filter and a role message goes to its holder's agent subject with the holder read off the claim. Such a release — stating no process, not forcing, and naming that role's own lane topic — is a hand-back, and it marks the claim it could not free (`handed_back`), whether that claim names a process or none: no registration renews or adopts a marked claim, `GET /v1/roles/<role>` answers `handed_back` meanwhile, and the role claim reaper frees the role at its first pass past the live window, unless the holder, which reads a marked claim as a role it does not hold, claims it back at its next heartbeat. A release that loses its conditional delete to a record written since reads the claim again, and one still naming that same session and that same process, and carrying the same `grant_id`, is its own holder's renewal, so the delete goes out again at the revision that renewal left, three deletes in all; a claim renewed under every one of them is left standing and fails the route naming the role, rather than reporting a release it did not make. A claim granted again since the release read the claim, to any process, the one the release states included, and in any millisecond, is no claim the release read, and it is left standing as the hand-back's mark leaves it. This route never forces a release; `DELETE /v1/interests/{session_id}` is the only caller that does. The consumer's filter update runs after the record is written, and one that meets a NATS reconnect answers `503` with `Retry-After: 1` and a body that opens `envoy: the message bus did not answer` and names `bus: NATS is reconnecting`, inside 3.5 s of the request's arrival (`sendRequestBudget`). Each read of the connection's state made once the budget has ended can add up to 100 ms (`ConnLockWait`), and a send or subscribe that meets a handshake begun just after its read waits for that attempt. On that 503 the record may already be written. envoy-client's retry removes the topics again and sets the filter; a retry after a first attempt that took the record's last topic finds no record and answers 200 with `removed` as `[]` once its own filter update is answered. A caller whose retry finds the topics gone leaves the filter to the session's next registration. A write route |
| `/v1/interests/` | GET | List persisted interests. Answers the refilling 503 while the interest copy cannot be served |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests. A GET reads a cache miss from the bucket within 3.5 s, and up to `ConnLockWait` (100 ms) later when a reconnect's handshake holds nats.go's connection lock: 404 only for a key the bucket does not hold, 413 or 400 for an id NATS will not take as a key, `500 {"error":"envoy: the interest record of session <id> is unreadable: <reason>"}` with no `Retry-After` for a record this build cannot decode (`store.ErrMalformedInterest`), 503 with `Retry-After: 1` for a read that did not answer. A DELETE removes a record this build cannot decode too, forcing the release of every role claim the session holds whichever process holds it; it is the one caller that forces a release, because an operator clearing a session's interests states no process and a release naming none frees nothing a process the claim names holds. It names no role's lane topic either, so it is no hand-back and marks nothing. A DELETE answers 204. Its consumer's filter update runs after the record is deleted, and one that meets a NATS reconnect answers `503` with `Retry-After: 1` and a body that opens `envoy: the message bus did not answer` and names `bus: NATS is reconnecting`, inside 3.5 s of the request's arrival (`sendRequestBudget`); the record is deleted all the same. Each read of the connection's state made once the budget has ended can add up to 100 ms (`ConnLockWait`), and a send or subscribe that meets a handshake begun just after its read waits for that attempt. It is a write route |
| `/v1/roles/set` | POST | Claim a role; the claim makes the session the holder, and the route also adds the role's topic to its record. The request carries `client_instance_id`, the process making the claim (the id every registration carries: one per Oh My Pi Envoy extension instance and one per Claude Code channel-server process, minted when that instance starts), and the claim record names it as the process holding the role. An explicit claim reads no holder: it writes with the revision of the claim record it read and, on a conflict, reads and writes again until its request's budget ends, so the last write wins; one that met a conflict at every try answers `503` with `Retry-After: 1` and writes nothing. Before each try whose read names another session it writes a takeover record (below); a try whose record did not store writes no claim and answers `503` with `Retry-After: 1`, and the Legion claim boot claims again. When nothing answered the record's write, that `503`'s body opens `envoy: the message bus did not answer` and names the takeover record, as the claim's own write that the bus did not answer does; a full stream, a denied publish and a refused record keep the `503` that names the record alone. A claim of either kind that lands and names another session as `previous_session_id`, the session it replaces, writes that session's handover record (below) when the claim's credential, the claimant's last registration and that session's last registration name one principal, so that session's unread messages reach the claimant whether or not it still held the role; a handover record that did not store, and a rule the listener could not read (the known-session bucket's leader not answering, or the claimant's own registration not recorded with its principal yet), answer `503` with `Retry-After: 1`, the claim standing, and the client's next claim writes it. A claim that does not name one principal, and every such claim through a listener whose known-session bucket is not open, stands, writes no handover record, and is answered `200` with a warning that no message of that session is handed over and why. A soft claim is granted when the role is unheld, when its holder is not `live` — `gone`, or `unresponsive` past the live window, which is how a relaunched Legion role recovers its own role — and when its holder is the session this one continues (`previous_session_id`) and that predecessor is this claimant's to take: the predecessor is not `live`, or its live entry carries the claimant's own `client_instance_id`, or either side carries no instance id. The last case is a client older than the field, and the listener logs that takeover with both session ids; the predecessor rule reads the claimant's instance from the request and falls back to its stored entry only where the request states none. A role already held under the claimant's **own** session id is decided on the standing claim and never on the session record, which names whichever process registered that id last: granted where that claim names this very process, granted where nobody has answered for the claim inside the listener's configured live window (`renewed_at`, which every registration of the holding process stamps) and this listener can vouch that the holder stopped, and otherwise refused the 409 naming that session, logged once with the holder's instance and the claimant's. The claim record reads the same whether its holder stopped registering or this listener failed to renew it, so that renewal arm refuses the claim, naming the state and claiming nothing rather than hand a live holder's role to a second process, in three states: while the role claim view cannot be served (`store.Registry.RoleViewErr`, the state a listener restart leaves until its first read of the bucket completes), while the copy's last whole read of the bucket is past its 2-minute bound or none has completed (`store.RoleClaimRepairReport.Err`), and while this listener's own renewal of that role for that holder failed inside the live window (`store.Registry.RenewalFailedWithin`: a claim read or write that did not answer or failed, or a registration whose store work failed before its renewal ran). The first is the bus-unavailable 503 with `Retry-After: 1`. The other two are the grant guard's own: the bus answered every read they rest on, so they get the 409 a second process gets while the holder still answers for its claim, `{"error":"envoy: the role grant guard refused the claim because this listener cannot vouch that its holder stopped renewing it: <state>","holder":…,"role":…}` with the claimant's own session id as `holder` and no `Retry-After` (`grantGuardPrefix`, `writeGrantGuardRefusal`), logged once with the arm that refused as well. It is a 409 and not a 503 because a 503 says the claim wrote nothing and may be made again, which the Legion claim boot does after a growing wait (`packages/pi-shared/src/legion-claim-core.ts`), while this refusal names the session that holds the role: both harnesses wait out a 409 that names their own session, claude-envoy for at most the live window and 10 s. The record of failed renewals is kept by role and holder session, not by process: a renewal of that claim that writes clears it, but a failure another process of the same session recorded while the claim names a process that has stopped (a restarted process whose registration or claim read failed) is cleared by no renewal, and refuses until one live window after the last failure. A relaunched process's own registrations clear no failure recorded against the claim that names the process before it, and record none while their store calls answer, so that arm stops refusing the relaunched process one live window after the last failure, inside claude-envoy's wait unless one of this listener's store calls for that session fails again more than about 10 s after the relaunched process's first claim. That record is this listener's own and a restart loses it, so the repair's bound and not the record is what bounds the harm. The arm that grants to the process the standing claim names is not gated: it rests on no renewal, and it is that holder re-claiming its own role after a listener restart. So a second process that resumed a live session's id cannot take the role that session holds, and reads `/v1/roles/{role}`'s `client_instance_id` to see which process does. A `live` holder refuses the claim atomically, a predecessor still live under another client instance among them, which gets the 409 any other live holder gets. The predecessor's entry is read from the `envoy_sessions` bucket itself, not from this listener's cache of it, and a predecessor read that did not answer is the bus-unavailable 503. The claimant's entry is read from the bucket too, falling back to the cached entry when that read does not answer. The answer names the previous holder's `holder_state`. A claim carrying `if_claim_revision` is conditional: it lands only while the role's claim record is still at that `envoy_roles` revision, the one `/v1/roles/{role}` reported as `claim_revision`, where `0` names a role with no claim record at all. Any other revision, a record where the claim names none, or none where it names one is `409 {error, role, holder, claim_revision}` carrying the revision the bucket does hold, and so is a claim that lost its conditional write to a claim that landed in between; no claim record is written, so the role keeps the holder the standing claim gave it. A hard claim with no `if_claim_revision` is weighed by the last-claim-wins rule alone, while a soft one with none carries a precondition the route supplies in the caller's place: the revision of the claim record the route decided on, `0` where no record stands, since the route decides on one read of that record and the write reads it again, and without the precondition two processes of one session id claiming one unheld role in the same moment were both granted it. A record replaced under such a claim is no answer for a caller that asked nothing about revisions: the route reads the claim again, decides the whole thing again on what the second read finds, and tries once more, a second mismatch answered with the same 409. A claim stating its own `if_claim_revision` is never decided again, that revision being the caller's own precondition. A write route |
| `/v1/registry/{session_id}` | GET | The authoritative answer for one session — reads through the cache to the session bucket, so a session the cache has not read yet is still found, and returns the same body a `/v1/sessions` row carries, roles from the claims included, with the session's `state`, `last_seen` and `client_instance_id`, the client process that wrote the registration, left out where the entry carries none as `/v1/roles/{role}` leaves its own out. It takes the list's selector: a session whose state is `unresponsive` is a 200 asked with `include=unresponsive`, and without it the 404 below, which names that session's `state` and `last_seen` all the same, so a caller reads `state` rather than the status code to decide whether the session ended. A live session with no interest record is a 200 with `topics` as `[]`, as the list shows it, and so is one whose id only the interest bucket refuses as a key and that the listener's interest copy holds no record for, logged at WARN naming it: Dispatch's envoy client reads a 413 or 400 here as a session that cannot exist. It refuses with 413 or 400 for an id the session bucket will not take as a key, `404 {"error":"no live session <id>", "state":…, "last_seen":…}` for a key the bucket does not hold, which is `gone`, and for an `unresponsive` session the caller did not ask for, the unreadable-interest-record 500 `/v1/interests/{session_id}` gives, and 503 with `Retry-After: 1` for a read that did not answer, the session's, its interest record's or the claims view's, all within a 3.5 s budget. `view=registration` asks instead for the stored registration alone (`listenerwire.SessionRegistration`: `session_id`, `state`, `last_seen`, `updated_at`, `client_instance_id`), read from the session bucket itself and never from this listener's cache, with no interest record and no role claims joined to it, so it answers whenever that bucket answers, and no state filter applies to it: every entry the bucket holds is a 200 carrying its record's own state, `gone` included, and the 404 is the bucket holding no record. It is what the Legion daemon's clean stop of a role whose process ended (a probe judged it gone, or a suspension's own stop ended it) reads before it deletes — the full answer's two extra reads are none of the stop's business, and either refusing would leave a dead session's claims standing to the gone horizon. Any other value of `view` is a 400 naming the selector |
| `/v1/sessions/{session_id}` | DELETE | Remove a session's registration; the session is `gone` from that moment, with no live window to wait out. A caller that must not remove a registration another process wrote names a condition, and the registration is removed only while the stored entry meets it, answered `200 {"deleted":true}` for one that is gone and `200 {"deleted":false,"reason":…}` for one that stayed. With `client_instance_id` it is removed only while it carries that id or carries none, so a departing client instance never removes the registration a later instance made under the same session id; with `if_not_registered_after` (unix milliseconds, and a value that is not a positive whole number of them is a 400) only while it was last written at or before that moment. The entry is kept when **either** condition refuses it (`session.DeleteConditions.keeps`), so naming both narrows what a delete takes and widens nothing. The Legion daemon's clean stop of a role's session, sent when its probe judged the role's process gone or when a suspension's own stop ended that process, names both at once (dispatch://INFRA-2064). Its moment is the one its own probe judged the process gone, or the one that suspension's stop returned, with nothing subtracted: on clocks that agree this listener stamped the ended process's last heartbeat before that moment, so the moment deletes it, where a stop dated earlier would read the same heartbeat as a registration written since and refuse the stop that was owed. Its instance id, read from `/v1/registry/{session_id}?view=registration` a moment before, is what makes carrying that moment undated safe: where the read answered the dead process's own entry, a relaunch's registration is refused by its own identity whatever the two clocks say, and where the read was answered after the relaunch had registered, the id names that successor and the moment keeps the registration instead. A write route |
| `/v1/sessions/{session_id}/successors` | GET | `{"session_id":…,"principal":…,"successors":[…]}`: the sessions that replaced that session, sorted and each once, `[]` when none (`handoverSuccessors`): each session the stream's handover records name as its successor (`bus.Client.HandoverSuccessors`), and each that replaced one of those in turn, however long the chain, since a message handed to a session replaced before it read it is handed on with its delivery unchanged. Successors are named only between sessions of one principal: a session is named only while its last registration names the principal the asked-about session last registered as, read from the known-session bucket's leader, so a session that registered as another principal since its claim is named no more, nor is any session only it leads to; a session whose principal no registration of this build recorded has none. `principal` is that principal, which each named session last registered as too (in the form that names the credential's kind and scope, `internal/ciphertoken/principal.go`), shown only to the caller whose verified identity is the listener's `ENVOY_DISPATCH_PRINCIPAL`, and only beside a named session: every other caller (a session's broker token, the shared token, another identity, an open `/v1`, and every caller of a listener that sets no `ENVOY_DISPATCH_PRINCIPAL`) reads `""`, and so does an answer whose `successors` is empty. A listener whose known-session bucket is not open names none, `200` with `[]`, as it hands nothing over, until its store opens the bucket. Dispatch accepts a successor's reply on a broker token only where the token acts for that principal. The records are those the claim route writes for a claim that names the session it replaces (see "Role delivery and exceptions" below), read by their subjects alone, one `STREAM.INFO` for each form a role takes in a subject; a finished record still names its successor while the stream keeps it. Dispatch reads it before a reply's transaction, so the session that replaced the one a `session:` attempt names may answer that attempt, a copy of which the listener handed it. A read of the stream or the bucket that did not answer within 3.5 s, the whole chain's included, is the bus-unavailable 503 with `Retry-After: 1`, never a list naming part of the chain. A broker token is not narrowed to its own session here, as the registry read is not. An edge listener reads the shared listeners' records, which its grant's `STREAM.INFO` allows, and names a successor only where the token's projects show that session, confirmed by its stored record as a row of `/v1/sessions` is |
| `/v1/legion/agents/sessions/{session_id}` | GET/PUT | One Legion session's registry entry. GET adds `live` (the session's own state in the session registry, true only while it reads `live`; an `unresponsive` session reads false and has not ended, see “Session state”) and `replaced_by`/`replaced_at` (the next session of its role token by `started_at`), both derived at the read, or answers `404 {"error":"unknown legion agent <id>"}`; PUT records the daemon's entry for the session, and a write moving a recorded session to another issue or role is a 409, concurrent writes included: a write claims `session.<id>` by compare-and-swap before it writes the index keys. All three registry routes answer 503 naming the missing grant on a listener whose NATS user lacks the `legion_agents` grant; every other route serves |
| `/v1/legion/agents/roles/{role_token}` | GET | `{"entries":[…]}`: a role token's sessions, newest first (the first is the current holder), `[]` when none |
| `/v1/legion/agents` | GET/PUT | GET `?issue={KEY}`: an issue's sessions by role in workflow order, then newest first, `[]` when none. PUT: a batch of at most 256 entries and 4 MiB, the `/v1` body limit, every entry validated before the first write (400 naming the bad entry's index); a batch that fails after that names how many were written (`written`) with the failed entry's own status, a 503 to retry for a write the bucket did not answer. Every read comes from the bucket itself, never a cache |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message. The target is read from the session store itself, and a session the store holds no entry within the gone horizon for is looked up in the known-session store (every session that registered in the last 30 days): `200` with `state` `connected`, `no_receiver` or `stopped` stores the message, a `409` refuses a stopped target the request asked `connected_only` of, and a 404 means neither store knows the session; a store read or a publish the bus did not answer is a 503 with `Retry-After: 1` (see “Send and publish answers” below). An optional `role`, the role a Dispatch message for a `role:` route is sent for, goes on the envelope; it is taken only from the principal `ENVOY_DISPATCH_PRINCIPAL` names, as `apiAuth` verified it, and refused `403` naming `role` from every other caller. When the role's claim names another session than the target, or none, the message is kept on the role's kept lane and answered `202` with `kept` and `kept_until`. |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. A topic the stream captures is answered once JetStream stored it, bounded to five seconds; `notifications.team.<name>` is such a topic, and each session that follows it reads the stored copy from its own consumer. That answer carries `recipients`, the sessions other than the sender whose interest row or known-session record follows the topic through a topic the filter built from its stored row holds ("Session consumers" above), and `recipients_connected`, how many of those are live; a topic no session follows that way answers `recipients` 0. Counting a controller notice lane reads that role's claim record from the role bucket stream's leader, within 500 ms, and a read that did not answer leaves both counts out. A role topic is forwarded to its live holder's agent subject and answered once the stream stored that forward, or with a 504 after four seconds (see "Role delivery and exceptions"). Any other `notifications.` topic that neither the deployed stream nor this build captures is refused `400` by name, since no session would receive it. A topic this build stores that the deployed stream does not capture, as during a hub move's quiesce, is sent nowhere and answered the 503 below that says the message was not sent, with `Retry-After: 1` (`bus.ErrSubjectNotDeployed`): the caller may send it again later. A topic under `notifications.dispatch.` or `notifications.github.` is refused `400` with an error that names the lane, and nothing is published; an edge listener refuses every topic but its projects' role subjects before that, with `403` `topic_not_published` (`edgePublishRefusal`), so a session at the edge gets that answer for these lanes: Dispatch and the listener that receives GitHub's webhook publish those lanes straight to NATS, and no grant refuses a caller of this route on the listener whose own user holds the lane. An agent topic (`notifications.agent.`) is refused `400` too, since a direct message goes through `/v1/messages/send`; so are `notifications.legion.wake`, which only the listener publishes, and the listener's own lanes (`notifications.envoy.kept.`, `notifications.envoy.resume.` and `notifications.envoy.takeover.`), each by name. A publish of a captured topic the bus did not serve, and a role-holder read at a role send's first resolution that did not answer, are both the 503 below rather than a 500. |
| `/healthz` | GET | Health check (always available, even during startup); its bus checks answer within 3 s, except that the reads after its flush (`SubOK`, `Connected`, `CoreSubOK` and the durable's `ConsumerInfo`) can wait out a reconnect's handshake that begins after the flush, and a rebuild of the KV watches that has not succeeded for a minute after a reconnect is `200 degraded` naming the `KV watch rebuild` - not a refusal, which would take the probes' only listener out and replace it with one that rebuilds into the same failure on empty caches, so the state pages through the `<N>-reconnect-hooks-stalled` alarm instead - while a rebuild that fails and is run again under that minute leaves the answer it had (see “Webhook and health check bounds” below). The healthy answer, and every degraded one that keeps its fields, carries `role_claim_repair`: `completed_at` (RFC 3339, UTC) and `age_seconds` of the role claim watcher's last whole read of the role bucket, both `null` before one completes, `bound_seconds` (120, `store.RoleClaimRepairBound`), and `put_back` and `took_out`, the claims that read put back into the copy and took out of it. A read older than the bound is `200 degraded` naming `role claim repair unavailable`, never `503`, and `GET /v1/sessions` still answers `200` from the copy then |

## Session state

A session is `live`, `unresponsive` or `gone`, computed from its registration record's `UpdatedAt`
and from nothing else — never from whether a cache, a list or the bucket's key set holds it:

| State | Rule |
| --- | --- |
| `live` | It registered within `ENVOY_SESSION_LIVE_WINDOW` (a Go duration, default 5m) |
| `unresponsive` | Its last registration is older than the live window and newer than `ENVOY_SESSION_GONE_HORIZON` (a Go duration, default 1h). It is not dead: it keeps its role claims and its issue claims, a message sent to it is stored and reaches it when it answers again, and nothing starts a replacement for it |
| `gone` | Its last registration is older than the gone horizon, or the registry holds no record of it |

Every list row, every one-session lookup, every send or publish refusal that names a session and
every role answer carries that session's `state` and `last_seen`. A role answer names the holder's
state as `holder_state` and says, with `claim_released`, whether that answer released the claim;
it is always `false`, because no answer releases one. `GET /v1/roles/<role>` also carries
`handed_back` where the holder's claim carries the hand-back mark, on the 200 and on a lapsed
holder's 404 alike, and leaves the key out where it carries none. Both clients read a marked
claim as a role they do not hold (`roleHeldBy` in `@legion/envoy-client/transport`), and the mark
is read nowhere in `POST /v1/roles/set`, which weighs a marked claim by the process it names and
then by its renewal exactly as it weighs any other; every grant writes a fresh record, so a grant
to the process the claim names clears the mark.

No lookup, publish, role-lane delivery attempt, Dispatch outbox forward or daemon probe releases a
role claim, whatever the holder's state. Only the role claim reaper does. A claim renewed inside
the live window is kept whatever the session bucket says of the id its holder registered under: a
holder renews its claim by registering (`renewed_at`), and that is what keeps the role of a
process whose session id a second process resumed and then deregistered, leaving the record gone
and the holder running. Past that window the rule is the one dispatch://INFRA-392 settled,
unchanged: an unmarked claim is kept while its holder's own record reads short of `gone` and
freed once that session is gone, for a claim that names a process and a claim that names none
alike, so an `unresponsive` holder keeps its role and the paths that need a live holder report it
instead. The one claim freed past a holder session that still answers is one a hand-back marked
(`handed_back`), and only once its own renewal has lapsed: the mark is the holder's own word that
it gave the role up and no registration renews a marked claim, so a role an older client handed
back comes free one live window plus up to one reap interval later — about ten minutes with the
shipped defaults — while that client is still registering. The mark lands only on the grant the
hand-back read, and a release's delete frees only the grant the release read: every grant writes
a `grant_id` it draws at random, which renewals and the adoption carry over, so once every
listener runs a build that writes it, a claim granted again before the mark's write or the
delete, in the same millisecond or later, is left unmarked and standing. A record with no
`grant_id`, which an earlier build wrote, is told from a grant by its `claimed_at`, as that build
tells it. While a listener of an earlier build still writes the bucket, three gaps stay open: a
grant that build writes in the millisecond of the claim a hand-back read reads as that claim; a
hand-back that listener serves follows its own `claimed_at` rule and drops `grant_id` from the
claim it marks; and a grant written in that millisecond with a `grant_id`, which that listener
renews before the mark reads the claim again, has lost its id and is marked, which no code can
prevent. All three close when the last listener of the earlier build is upgraded. A registration
renews every unmarked
claim its own process holds among the roles it weighs: the role topics of the interest record its
own write read from the bucket, and the roles the listener's copy of the claims names for the
session, each renewed as its own claim record says, whatever topics the registration carries. One
that states no process renews the claims of its own session that name none, adopts none, and
notes on the claim that it renewed it (`renewed_without_process`), so no claim of a client that
states no process ages out while it runs; a registration that states a process adopts a claim
that names none only while that note is absent, at the first heartbeat after the claim was
written. The reaper runs on a period of its own,
`ENVOY_ROLE_CLAIM_REAP_INTERVAL` (a Go duration, default 5m), and not on the session reaper's:
while the session bucket's maximum age is below the gone horizon, as the 5m of a bucket an older
build created is, a record the bucket expires reads `gone` too, so a period shorter than that age
takes a merely stalled holder's role that much sooner. Once the maximum age covers the horizon, as
the 2h of a bucket the listener creates covers the default 1h, no record goes early, and the period
decides only how long past the horizon a gone holder keeps its role, so it belongs well inside the
horizon. A Legion role whose agent process exits is `gone` at once rather than at the end of the
live window, so its successor's soft claim takes the role without waiting the window out; a soft
claim replaces the holder, it does not release the claim.

Dispatch reads `ENVOY_SESSION_LIVE_WINDOW` and `ENVOY_SESSION_GONE_HORIZON` too and judges a role
route's holder by them (`roles.Windows`, `cmd/dispatch`'s `roleHolderWindows`), so a deployment
gives its listeners and its Dispatch server the same two values.

`ENVOY_SESSION_MAX_AGE` sets the `envoy_sessions` bucket's `MaxAge`; unset, the listener leaves the
bucket's own. A bucket the listener creates gets 2h, an hour past the default gone horizon
(`session.BucketMaxAge`); a bucket an older build created keeps its 5m until this setting raises
it. A gone horizon longer than that age reads a record the bucket expired as `gone` before the
horizon, so the operator raises the bucket past the horizon. A listener whose bucket's age is at
or below the gone horizon logs one warning at its start that names both values and
`ENVOY_SESSION_MAX_AGE`. `/healthz` reports the age in use as `session_max_age` and the horizon as
`session_gone_horizon`, each a Go duration string such as `"2h0m0s"`.

## Send and publish answers

`POST /v1/messages/send` resolves its target from the session store, not from this listener's
cache of it: the cache answers a hit, and a miss is read through to the `envoy_sessions` bucket
within one second (`liveSessionEntry`, `cmd/listener/role_holder.go`), the same direct get the role
paths already make. That read and the publish share one 3.5 s budget taken from the caller's own
request context, so a store or a bus this listener cannot reach is answered inside the caller's
attempt:

| Outcome of the attempt | Answer |
| --- | --- |
| The store holds an entry within the gone horizon, and the connection probe of the session's relay subject was answered, met silence or failed any other way, or was not made because the entry records no consumer delivery | `200`, `state` `connected`, `recipient_state` `live` or `unresponsive` by the entry's age and `last_seen` the entry's; the message is stored. A probed entry that advertises no delivery mode gets the next row instead |
| The store holds an entry within the gone horizon that records consumer delivery and advertises no delivery mode (`capabilities` empty), as a Claude Code terminal does while no mod is connected to it, and the probe passed as in the row above | `200`, `state` `no_receiver`, no `recipient_state` and `last_seen` the entry's; the message is stored. The session runs, but nothing writes the message into its conversation now, so the answer does not say `live`. It gets the message from its own consumer if it takes messages within 72 hours. Only a send whose frame names no `delivery.mode` gets here; one that names a mode is the `403` below. `connected_only` does not refuse it, and it raises no wake |
| The store holds an entry within the gone horizon, and the probe met no responders | `200`, `state` `stopped`, `recipient_state` by the entry's age and `last_seen` the entry's; the message is stored |
| The store has no entry within the gone horizon (`nats.ErrKeyNotFound`, or an entry past it), and the known-session store holds a record of the session | `200`, `state` `stopped`, `recipient_state` `gone` and `last_seen` the record's; the message is stored, and the session gets it from its own consumer if it runs again under the same id within 72 hours |
| The target is stopped (either `stopped` row above) and the request set `connected_only` | `409 {"error":"the session is stopped"}`, recorded `session_stopped`, with nothing stored. No other state is refused |
| Neither store knows the session (no entry within the gone horizon and no record, which also covers a session id no session can register under) | `404 {"error":"no live session <id>"}` |
| The store read did not answer (its second ran out, or the read failed any other way) | `503 {"error":"envoy: the message bus did not answer: <reason>"}` with `Retry-After: 1` |
| The target's entry, or for a target with no entry its known-session record when that record carries capabilities, does not advertise the frame's `delivery.mode`, or that mode cannot be read unambiguously | `403` naming the session, and the mode when it could be read |
| The request body is longer than 4 MiB, the `/v1` body limit | `413 {"error":"body too large: the limit is 4194304 bytes (4 MiB)"}`, recorded `too_large`, with nothing read past the limit and nothing stored (see “Request body limits”) |
| The publish is refused as too large (`bus.ErrTooLarge`) | `413` naming the size against the server's max payload |
| The publish is refused for its subject or key (`bus.ErrRefused`) | `400` naming the refusal, with no `Retry-After` |
| The stream refuses the publish because it holds its byte limit (`bus.ErrStreamFull`) | `503 {"error":"envoy: the message bus refused the message because its stream is full: <reason>"}` with `Retry-After: 1`, recorded `stream_full` |
| The stream answers the publish as a duplicate of another request's copy (a repeat under the same key), and the listener reads that copy back at the sequence the verdict named | `200` as the rows above, with `duplicate: true` and an answer read from the stored copy (`publishHeld`, `cmd/listener/stored_copy.go`): the copy's envelope whole (its `event_id`, `issued_at` and body) to the request's own copy and to the copy's own sender (the same `source` and the `source_session` the copy names, and, where the request presents a broker token, the session it proves; a copy that names no session is whole only to its own request); to anyone else, where the request names `Envoy-Reads: key-taken` (`listenerwire.ReadsHeader`), only the copy's `event_id`, times, `topic`, `dedupe_key` and `expects_reply`, and otherwise the envelope the listener built for the request (`requestSender.answer`); the record line names the copy's `event_id` and the request's own sender; a verdict on the bus's own resend that names this request's first send is answered as a new copy, with no `duplicate`; a role send's repeat from a session that has since taken the role, which forwards nothing, is answered from the forward this listener's record names for the earlier holder for the same request (`settledBeforeForward`): the record is the listener's own and lasts 10 minutes; a repeat after that or on another listener answers its own new envelope, as at main |
| The stream answers the publish as a duplicate, and the copy cannot be read back: the read fails or does not answer, the verdict names sequence 0, or that sequence no longer holds a copy with the request's dedupe key | `503 {"error":"envoy: the stream already knows this message's id, and no stored copy of it was read: <reason>"}` with `Retry-After: 1` and no envelope, recorded `stored_copy_unreadable`, and with `"code":"maybe_stored"` where an earlier send of the same publish went unanswered (`bus.ErrUnacknowledged`); the same request again gets the copy once it can be read |
| The server denies the publish under this listener's own grant (`bus.ErrPublishDenied`) | `403 {"error":"envoy: the message bus denied the publish under this listener's own permissions: <reason>"}` with no `Retry-After`, recorded `publish_denied` (below) |
| NATS was reconnecting: the publish waited for the connection until the budget ended, or the connection dropped under the publish after that wait (`bus.ErrReconnecting`) | `503 {"error":"envoy: the message bus did not answer: <reason>"}` with `Retry-After: 1`, whose reason says `bus: NATS is reconnecting`; nothing was sent, and nothing is sent later, unless the reason also says that the stream did not acknowledge the publish, so it may hold the message (`bus.ErrUnacknowledged`): an earlier send of it went out before the connection closed |
| A clustered stream refuses the publish because an earlier publish under the same message id is still in process (`bus.ErrDuplicateInProcess`) | The listener publishes again every 50 ms until the stream names the earlier copy, and answers that copy as the duplicate rows above do (`publishPastInProcess`, `publishHeld`); a request whose budget ends first gets `503 {"error":"envoy: an earlier attempt of this message is still being stored: <reason>","code":"maybe_stored"}` with `Retry-After: 1`, recorded `earlier_attempt_in_process`, which costs its sender nothing unless a send of its own went out unanswered |
| The deployed stream, as this listener's client last read its subjects, does not capture the target's agent subject, a subject this build stores, as during a hub move's quiesce (`bus.ErrSubjectNotDeployed`) | `503 {"error":"envoy: the message was not sent, and may be sent again later: <reason>"}` with `Retry-After: 1`, recorded `subject_not_deployed`, with nothing sent or stored; the prefix is `MESSAGE_NOT_SENT_PREFIX` in `packages/contracts`, which `envoy-client` reads as a message nothing stored, so the caller may send it again later |
| Any other publish failure | `503` with the prefix `envoy: the message bus did not answer` and `Retry-After: 1`, carrying the bus's own reason |
| Before the store read, the sending agent session's count under the operator's sender limit holds no message (below) | `429` with `Retry-After`, recorded `sender_rate_limited`, with nothing read, reported or stored |

A target whose entry the store holds within the gone horizon is resolved whatever its state, so a
send to an `unresponsive` session is published to its inbox and waits there for it. The `404` is
only a target with no entry within the gone horizon and no known-session record; it reports that
target's `state` as `gone`, and its `last_seen` while the registry still remembers it (two hours
past the last registration, `session.LastSeenMemory`), so a caller never reads a refusal as the end
of a session that is merely quiet. A listener whose known-session store does not answer (no store,
a bucket its grant does not open yet, its first read not complete, or a copy past its stale bound)
holds no records, so a target with no entry within the gone horizon gets the `404` there, and one
whose entry stands is still judged by its probe, as above.

`POST /v1/messages/publish` answers the publish of a topic the stream captures that failed, and a
role send's first role-holder resolution that did not answer, exactly the same way; that
resolution (the claim read and the holder's session read; it writes nothing to the role bucket, so
a holder it finds past the live window keeps its claim) runs
inside the same 3.5 s budget, and one the budget runs out on is that 503 too, naming the call that
did not answer. Its 413, its 400s and its 403 for a denied publish are unchanged. Its role 404s
(`unclaimed`, `holder_lapsed`) keep their bodies, and a role send gives one only when the listener
kept nothing: the request sent `keep: false`; the role's token is one `roles.Valid` refuses, which
no claim can ever name; or the keep itself failed, also a keep whose publish went out and was
never acknowledged, which may have stored the copy, as at main. A role send whose message the
listener kept answers `202` instead, both for a role nobody claims and for a claim whose holder
is not live. A
role topic's send then runs on
within its own 4 s and answers its 200, 202, 403 `capability` or 504 as "Role delivery and
exceptions" says.

A publish the server denies under this listener's own grant (`bus.ErrPublishDenied`) is the
table's `403` on either route. A publish on either transport can be denied this way: a core
publish reads the denial from the connection's last error across its flush (`confirmPublished`,
`internal/bus/confirm.go`), and a JetStream publish, which the stream acknowledges with nothing
when the server refuses it, reads the violation the server reported under its own subject
(`deniedPublish`, `internal/bus/denials.go`). So a topic the stream captures is answered that 403
too, and so is `POST /v1/messages/send`, which publishes `notifications.agent.<id>` over
JetStream. That is a change for a client: such a publish and such a send were the retried `503`
with `Retry-After: 1` before, and are now a `403` with none, which
`packages/envoy-client/src/transport.ts` does not retry (it retries every status at or past 500 and
none below it).

Both routes also hold each agent session to the operator's sender limit
(`cmd/listener/sender_limit.go`). `ENVOY_SENDER_MESSAGES_PER_MINUTE` is how many messages one
session may send in a minute, a whole number from 0 to 60000, and an unset rate or 0 sets no
limit. `ENVOY_SENDER_MESSAGE_BURST` is how many it may send at once, a whole number of 1 or more,
and unset it equals the rate. A burst with no rate, or any other value, stops the start with an
error that names the variable (`parseSenderLimit`). The start logs one line,
`listener sender limit: <rate> messages a minute, <burst> at once, for each agent session in this
process`, or `listener sender limit: none`. Where the rate is 1, the line reads
`1 message a minute`.

The listener keeps one count for each sending session, for both routes together
(`ratelimit.Keyed`). The count starts at the burst and gains one message every 60 seconds divided
by the rate, up to the burst. It is the listener process's own, so each replica keeps its own
count and a restart fills each count again. The sender is the session a secrets-broker token
proves, else the request's `source_session`. Only a message whose `source` is `agent` or absent
and that has a sender is counted. So a message of Dispatch (`dispatch`) or of Envoy itself
(`envoy`) is not counted, and neither is the Legion daemon's notice, which names no sender. The
check comes after the request's own checks, the publish route's `ReachesNoSession` 400 included,
and before the route reads a session, resolves a role, reports a wake or stores anything
(`senderLimit.admit`).

A message that finds its count at 0 is answered `429`, recorded `sender_rate_limited`, and the
listener stores it nowhere, keeps it for no later holder of a role and reports no wake for it
(`requestRecord.refuseSenderRateLimited`). `Retry-After` gives the whole seconds, rounded up and
at least 1, until the count holds one message again (`ratelimit.Keyed.WaitAt`). The JSON body
holds `reason: "sender_rate_limited"`, `retry_after_seconds` (the same seconds),
`messages_per_minute`, `burst`, and an `error` that is the sentence `Your sending is rate limited.
This message was not sent. This listener lets one session send <rate> messages a minute and
<burst> at once. You can send it again after <seconds> seconds.` with the values filled in. Where
a value in the sentence is 1 its noun is singular: `1 message a minute`, `after 1 second`. No
client sends such a message again by itself (`packages/envoy-client/src/transport.ts` retries
nothing below 500). The one exception is the Legion stall notice: its sender waits the seconds
and sends it again, at most 3 times (`packages/pi-legion/src/stall-evidence.ts`). This is not the
429 of the broker token check, `too many access tokens checked from this address`, whose log
line's reason is `rate_limited`.

A counted message costs its sender one where the route stores it, may have stored it or reports
a wake for it, whatever it answers (`senderCharge`). So a send to a target with no entry costs
one once the route reports its wake. A role send's `504` costs one whether or not a forward went
out, also when the window ended before the first forward (`roleSend.timedOut`), and so does its
`503` for a window that ended with its forward in process (`roleSend.maybeStored`). A role send
costs one for a forward the stream confirmed, also one it answered as a duplicate. The listener does
not tell a `504` that forwarded nothing from one whose forward the stream may hold. A `503` after
the listener sent the message to the stream and the write's acknowledgement did not arrive in time
costs one too (`bus.ErrUnacknowledged`, `senderCharge.published`), since the stream may hold it.
A refusal before any store or wake report, such as a `400`, `403`, `413` or a `503` before the
listener sent anything, one while it waits for NATS to reconnect among them, costs nothing. A
repeat the listener recognises costs nothing too: the stream's `duplicate` answer to a send or a
topic publish, and a role send's repeat of a kept copy (`keptRoleCopy.repeat`) or of a forward in
the route's own 10-minute record. So does an echo, a role message from the session that holds the
role. A `duplicate` that answers the bus's own resend after the connection closed under its first
send (`bus.StoredAck.Resent`) is no such repeat, since that first send may have stored the copy it
names, so it costs one. A role send's echo, repeat or `403` costs nothing only while the send has
made no forward. After an earlier forward of the same send it costs one, as any answer then does,
since that forward may have stored the message. A repeat that finds the count at 0 is refused as a
new message is, because the check comes first.

A role-holder read that did not answer, within its cap or the 3.5 s budget, is the bus-unavailable
503 above with `Retry-After: 1`, naming the call, on the publish route and on
`GET /v1/roles/<role>` alike, and a key NATS refuses keeps its 413 or 400. On both routes a
claim record no read can decode (`store.ErrMalformedRoleClaim`: one naming no holder, or JSON that
is not a claim) is `500 {"error":"envoy: the claim record of role <role> is unreadable: <reason>"}`
with no `Retry-After` (`writeRoleClaimUnreadable`), recorded `role_claim_unreadable` on a publish:
the bus answered, and a retry reads the same record. One resolution is one claim read: neither
route retries, because a holder that stopped answering keeps its claim and there is no release for
a second read to see past. `store.Registry` bounds that claim read by the caller's context itself
(`RoleClaim`), since nats.go's KV calls take none.

A send and a publish stamp the envelope's `sender` from the sender's interest record and session
entry (`senderStamp`, `cmd/listener/api.go`), whose cache misses are read through to their buckets
within one second of the same budget (`senderStampReadTimeout`; `store.Registry.Get` bounds the
bucket read alone, so a cache hit answers in the caller's goroutine). The `machine` and `cwd` are
the interest record's, and the session entry's where that record is not found or not read, since
each registration writes both; a sender whose entry is not read either, from this listener's cache
or from the bucket within the same window, gets neither. The stamp is only enrichment: a read that
does not answer leaves it the session id and what this listener's session cache holds, and the
publish that follows meets the same silent bus and answers the 503 above inside the caller's
attempt. The stamp's `roles` are the roles whose claims name the sender, from the claims view the
session routes read; a view that is not complete (`store.ErrRoleViewNotReady`) leaves them out,
and the send goes on.

A publish whose caller's context has already ended puts nothing on the wire; on these two routes it
is answered the 503 above. A role send's forward, published as a core message whose reply carries
the stream's acknowledgement, checks its context
immediately before it publishes (`publishConfirmed`, `internal/bus/confirm.go`): one check sees
both a spent deadline and a cancel while the deadline is still ahead, so a cancel during a wait for
a reconnect sends nothing (a cancel after that check cannot be undone); the
JetStream path has no check of its own and relies on nats.go's, whose request returns the context's
error before it sends anything. It matters because a route's reads run before its publish under the
same deadline, and one that answers just as the deadline ends leaves the publish none: a core
subject has no duplicate window, and the client retries every 5xx
(`packages/envoy-client/src/transport.ts`), so a message emitted behind a failure answer would
reach its subscriber twice.

Each inbound send and publish is recorded in one line, INFO when accepted and WARN when refused:
`listener send` (`target_session`) and `listener publish` (`topic`), both carrying `event_id`,
`source`, `source_session`, `payload_summary`, `outcome` and `status`, and `holder` as well when a
role topic resolved one. The `event_id` of an accepted repeat is the stored copy's, whatever its
answer carries. `outcome` is one of `accepted`, `invalid_request`, `too_large`,
`bus_unavailable`, `stream_full`, `stored_copy_unreadable`, `earlier_attempt_in_process`,
`subject_not_deployed` and `sender_rate_limited`, plus `no_live_session` for send,
`session_stopped` for a
`connected_only` send whose target is stopped, `capability_refused` for send and a role send,
`publish_denied` for send and publish alike, since a publish on either transport can be denied,
and `role_unclaimed`, `role_lapsed`, `role_no_source_session`,
`role_claim_unreadable` and a role send's one 504 reason, `delivery_failed`, for publish, where a
role send whose window ends with its forward in process is `earlier_attempt_in_process`. A role
send whose message the listener kept is `accepted`, because its 202 is an answer and not a
refusal, so `role_unclaimed` and `role_lapsed` name a send that kept nothing, and
`role_no_source_session` a send that named no source session and so was kept for nobody. Two
refusals carry no such line, because neither reaches a handler: the 401 the API auth wrapper
writes, and a request the starting gate turns away while the routes are still opening, which logs
`request refused while starting` itself. The line of a message the sender limit refused, and of
each message that cost its sender one, also carries `sender`, the session the limit counted. No
other line of the listener carries that field.

`envoy_messages_delivered_total` counts one delivery attempt per recipient, labelled
`delivery_status`:

| `delivery_status` | Attempt it counts |
| --- | --- |
| `delivered` | A role forward the stream acknowledged storing for its holder |
| `skipped` | Nothing to deliver: the session's own consumer holds the message and the relay of that consumer delivers it, a message on a session's agent subject finds the session's record, the durable's subject matches no session of this machine, or a role message would echo back to the session that holds the role |
| `dedupe` | A repeat the dedupe cache recognised as already delivered, or the attempt cache as already attempted |
| `kept` | A role message no live holder could take, kept for one hour for the next session to hold the role live, and dropped at the end of that hour if none does |
| `failed` | The delivery, the role forward, or the delivery-mode guard before either, failed |
| `wake_reported` | A message on the agent subject of a session with no record, once the stream stored the session's wake report and the listener acknowledged the message. A report the stream never stored is the WARN `listener agent wake not stored`, which no label counts |

`envoy_delivery_duration_seconds` carries the same label for the attempts it times.

## Write route answers

`POST /v1/interests/subscribe`, `POST /v1/interests/unsubscribe`, `POST /v1/roles/set`,
`DELETE /v1/interests/{session_id}` and `DELETE /v1/sessions/{session_id}` do every store read and
write inside the same 3.5 s budget taken from the request's own context, where a silent bus held
each for the 10 s JetStream wait: nats.go's KV reads, writes and deletes take no context, so each
bucket call waits through `bus.Bounded` (`internal/bus/kv.go`), which stops waiting when the
context ends. While NATS reconnects, `bus.Bounded` first waits for the bucket's connection within
the same context, since with the reconnect buffer off nats.go refuses every send at once then.
Store work that did not answer, whatever it names, is
`503 {"error":"envoy: the message bus did not answer: <reason>"}` with `Retry-After: 1`
(`writeStoreError`, `cmd/listener/api.go`). The reason of a call whose context ended in that
wait, or had ended before the call started while NATS reconnects, or that the connection dropped
under after the wait, says `bus: NATS is reconnecting` (`bus.ErrReconnecting`), and never
nats.go's `nats: outbound buffer limit exceeded`. The store's
own answers keep the answers they had: a refused key its 413 or 400, a held role its 409, and no
key, an undecodable record or a lost compare-and-swap the status the route always gave it; an
error a claim joins from several is one only when every part of it is. A write still waiting for
the connection when the budget ended was never sent and never lands. A write the budget abandoned
after it was sent may still land after the 503: a put rewrites the record the caller's retry
writes again, and a delete or a claim is conditional on the revision it read
(`store.Registry.UpsertWithin`, `Remove`, `SetRoleWithPrevious`, `session.SessionRegistry.PutWithin`
and `Delete` say why each is safe). A registration's known-session record write is the one store
write outside that budget: it comes last, on a bound of its own of one second, so a registration
answers within 4.5 s (`knownRecordWriteTimeout`, `answerRegistration`), but for two gaps: each
read of the connection's state made once a bound has ended can add up to 100 ms (`ConnLockWait`),
and a send or subscribe that meets a handshake begun just after its read waits for that attempt.
A session holds a role exactly when the role's claim in `envoy_roles` names it, so a claim's
rollback and its old holder's topic cleanup are best-effort tidying of interest topics within the
same budget: a claim that landed answers 200 whatever the cleanup did, and a topic left behind
gives nobody the role. Two writes of a session consumer's filter follow such store work before the
answer. An unsubscribe's (`refollow`) runs on the route's request context, so it ends within the
same 3.5 s of the request's arrival: `storedFilterWrite` bounds it by `sessionConsumerCallTimeout`
(5 s) as well, and the earlier deadline applies. The write first waits for the session's lock
(`lockWithin`), which this listener's registrations, relay restarts and deletes of that session
also take. It then sets the filter from the row the bucket holds, in at most five fenced passes
(`fenceFilterWrite`). If the lock is not free before the budget ends, the write gives up and
sends nothing: the listener logs a WARN line, the filter keeps the removed topics until a later
filter write such as the session's next heartbeat, and the route answers 200. If the last pass
meets a NATS reconnect, the route answers 503.
A claim that took a Legion controller role from another session narrows
that session's filter (`narrowFormerHolder`: a role claim read from the role bucket stream's
leader, a row read, the wait for that session's lock and one consumer call) within what is left
of the claim's own 3.5 s, on a context the client's hang-up does not cancel, so `POST /v1/roles/set`
still answers inside its budget, and a narrowing the budget cut short, or whose filter write met a
NATS reconnect, is logged at WARN naming the role, its lane and both sessions.

## Request body limits

Every route that reads a request body reads it to a limit above the largest real request of that
route: a `/v1` route through `http.MaxBytesReader`, and a webhook route through the webhook body
budget's read (`webhook.BodyBudget`, below). A body past the limit is answered `413` with an error
that names the limit in bytes and in MiB, whatever the body holds. The route then does nothing
else: it writes to no store, publishes nothing and makes no consumer. A `/v1` route refuses a
request whose `Content-Length` is past the limit before it reads the body. A `/v1` body that is not
JSON stops the decode early, so the route reads the rest of it to the limit: a body past the limit
is a `413`, and any other is a `400 invalid json`.

| Routes | Limit | Largest real request |
| --- | --- | --- |
| Every `/v1` route that reads a body (`apiBodyLimit`, `cmd/listener/body_limit.go`) | 4 MiB (4,194,304 bytes) | A send or a publish whose message and payload each come near the NATS max payload, the server's default of 1 MiB, which no deployment changes: about 2 MB. The envelope must fit that payload, and it carries the payload and only a one-line summary of the message. The largest registration the live listener held on 2026-11-16 named 802 topics in about 35 KB |
| `/webhook/github` (`githubMaxBody`) | 25 MiB (26,214,400 bytes) | GitHub's documented cap, 25 MB. No envelope carries the body whole, so a body past the NATS max payload is served |
| `/webhook/slack` (`slackMaxBody`) | 1 MiB (1,048,576 bytes) | Slack documents no cap on an event. It cuts a message's text at 40,000 characters, and the text and the blocks that repeat it come to 480,000 bytes at six bytes a character |
| `/webhook/ghostwispr` (`ghostWisprMaxBody`) | 1 MiB (1,048,576 bytes) | A session's times and links: a current unit sends no title or summary text |

A `/v1` route answers `413 {"error":"body too large: the limit is 4194304 bytes (4 MiB)"}`, and
the send and publish routes record it `too_large`. A webhook route answers the same words as text,
`body too large: the limit is <bytes> bytes (<n> MiB)`, and does not check the signature of a body
it did not read whole.

## Webhook bodies

Every webhook route reads its body, checks the sender's signature on the bytes it read, and only
then parses them: an event whose signature does not check is a `401` whatever its body holds,
Slack's `url_verification` handshake included, which Slack signs as it signs every request. The
three routes read against one byte budget the listener holds, `ENVOY_WEBHOOK_BODY_BUDGET` bytes
(64 MiB when unset), which the start logs as `listener webhook body budget: <bytes> bytes (<MiB>
MiB) held at once across every webhook route` (`webhook.BodyBudget`). A body is charged as its
bytes arrive, never past its route's cap (GitHub 25 MiB, Slack and Ghost Wispr 1 MiB) or its
declared length, and stays charged until its handler returns, a refusal included, so a sender
that sends only headers holds nothing. One read at a time may take the total past the budget, by
at most its own body. A delivery whose next bytes do not fit waits for room at most its route's
room wait, in all: Slack 1 s, GitHub 5 s, Ghost Wispr 4 s. It is then answered `503` with
`Retry-After: 1`, the listener logs `webhook body refused path=<route> ...`, and
`envoy_webhook_body_refused_total{path}` counts it. Slack retries a failed event nearly at once,
then after one minute and after five. GitHub does not redeliver: the redelivery sweep
(`internal/dispatch/redeliver`) asks again only for the GitHub App webhook's failed deliveries, so
a delivery from any other GitHub webhook is lost. No Ghost Wispr retry policy is recorded.

Anonymous senders can keep the budget full. One read that holds the total past the limit stops
every other read from charging a byte, and a stalled GitHub read keeps its charge for 60 s at a
25 MiB cap, so several senders taking turns, at about 32 MiB of body every 60 s (4.5 Mbit/s),
keep every signed delivery at a `503` for as long as they send. The listener has no cheap bound
for that; the bound is a limit per source in front of it, such as a relay that refuses unsigned
requests.

## Webhook and health check bounds

The webhook routes and `/healthz` do their bus work within the request; the last two rows below
are the checks `/healthz` makes with no call of its own, one from a stall the bus already recorded
and one from the time the role claim copy's last whole read completed. The webhook routes keep
every answer and status they had, and `/healthz` every answer but a check's that did not answer;
what changed is how long a silent bus can hold them.

| Path | Bus work | Bound | A call that did not answer |
| --- | --- | --- | --- |
| `/webhook/github`, a check_suite or check_run | CI record writes (`cistore.Store.Record`, `RecordSuite`) | each record 4 s (`recordWait`: the wait for a write it queues behind and its own 2 s write budget), every KV call included; all of it ends when GitHub drops the delivery or at the route's write deadline | `503 service unavailable`, as a failed record |
| `/webhook/github`, any other event | publishes | the request, the route's write deadline, and the bus's 5 s a publish | `503 service unavailable` |
| `/webhook/slack` | publishes | 2.5 s from the handler's start (`slackAnswerBudget`), inside the 3 s Slack waits | `503 service unavailable` |
| `/webhook/ghostwispr` | publish | the request, and the bus's 5 s | `503 service unavailable` |
| `/healthz` | flush, each cache's KV ping, the durable consumer's info read | 3 s from the request's start, shared (`healthzBudget`) | `503 unhealthy` naming the dependency and `context deadline exceeded`, so the container health check fails, as it did when the 10 s wait outran it; a check that answered with a failure keeps its answer (`200 degraded` or `503 unhealthy`) |
| `/healthz`, the KV watch rebuild | none: the stall the bus client recorded for its reconnect hooks (`bus.Client.HookStall`) | a minute of hooks due with no run that succeeded (`bus.RepairGrace`) | `200 degraded`, `KV watch rebuild unavailable: <last rewatch error> (no rebuild has succeeded for 1m0s)`, the stall alone when no run has failed behind it; deliberately not a refusal, since the ECS container health check and GKE's readiness probe both read `/healthz` on a single task, so refusing would replace or unroute the only listener and its replacement would rebuild into the same failing rewatch with caches that warm up empty, losing the fanout messages the running task's stale caches still place, and the state pages through the `<N>-reconnect-hooks-stalled` alarm on the bus's ERROR `envoy nats reconnect hooks have not succeeded` instead; a shorter stall keeps the answer the checks above gave, since the bus runs the hooks again within seconds of a failed run |
| `/healthz`, the role claim repair | none: the time the role claim watcher's last whole read of the role bucket completed (`store.Registry.RoleClaimRepair`), a read it makes again one minute after each that completed (`roleClaimRepairInterval`) | 2 minutes since that read completed (`store.RoleClaimRepairBound`, a constant and no setting: the default 5-minute live window less two 90-second heartbeats) | `200 degraded`, `role claim repair unavailable: the last whole read of the role claims completed <age> ago, past the 2m0s bound`, with every field of the healthy answer kept, and judged only while the role claim view answers, since a view that cannot answer is reported as itself; before the first whole read completes the view cannot answer, so the answer then is the role claim view's `200 degraded`, `role claim view unavailable: ...`; deliberately not a refusal, since a readiness probe that runs `curl -f` against `/healthz` would take the listener out of service on a `503`, and `POST /v1/roles/set` refuses meanwhile to take a role held under the claimant's own session id |

nats.go's KV calls take no context and wait out the JetStream MaxWait (10 s), so a CI write's KV
calls and `/healthz`'s pings are waited for through a context (`kvCall`, `bus.Bounded`) that
stops waiting when the bound ends. A CI write given up on may still land after the 503: it is a
compare-and-swap at the revision it read (a create when it read none), so it never overwrites a
write since, and a redelivery of its observations writes nothing. A ping given up on runs on to its
own end and changes nothing a later ping would not.

## Subscription visibility

An OMP extension keeps its direct NATS subscriptions locally and persists them through
`/v1/interests/subscribe`. `envoy_list()` reports their union: each topic is marked
`live` (local only), `registry` (persisted only), or `both`. This exposes live delivery
state even while an interest registration or cleanup request is delayed or unavailable.
A session's messages do not come from those subscriptions: they come from its own durable
consumer, which the listener relays ("Session consumers" under "Runtime shape", above).

## Role delivery and exceptions

`notifications.role.<role>` is a live, exactly-one-holder lane. A role message sent through
`/v1/messages/publish` is delivered by the route itself: it resolves the current live role holder
and forwards the envelope, which keeps the original role topic, to
`notifications.agent.<session_id>`, where the stream stores it for that session's own consumer.
No holder answers a receipt, because a session receives what its consumer delivers and nothing
else (INFRA-771), so the stream's acknowledgement that it stored the forward is the whole of the
attempt's outcome. Each attempt waits at most two seconds for that acknowledgement; a stream that
refuses the forward because an earlier forward under its `Nats-Msg-Id` is still in process is sent
the same forward again every 50 ms inside that attempt, until it acknowledges the earlier copy
(`startRoleForward`, `publishPastInProcess`). Without an acknowledgement the route resolves the
holder again and forwards the same envelope 100 ms later (one `event_id` and one `Nats-Msg-Id`, so
the stream keeps one copy however many attempts there are), until the stream acknowledges one or
four seconds from the request's arrival have passed. No attempt
forwards to a holder that is not live. It answers `200` with the `holder` and its `holder_state`
once the stream stored the forward. It answers `202` when no live session holds the role and the
listener kept the message for one hour for the role's next live holder. The 202 carries
`kept: true`, `kept_reason` and `kept_until`, the end of that hour in Unix milliseconds.
`kept_reason` is `unclaimed` when nothing claims the role, and `holder_lapsed` when the claim's
holder is not live, which also names that holder, its `holder_state` (`unresponsive` inside the
gone horizon, `gone` past it) and its `last_seen`. Such a claim stands: the publish is not
forwarded to its holder, nothing releases the claim, and an `unresponsive` holder is not dead, so
the sender starts no replacement for it. It answers `404` with `reason` `unclaimed` or
`holder_lapsed` when no live session holds the role and nothing was kept: the sender asked for
that with `keep: false`, the publish names no `source_session`, or the keep was refused or
failed. A publish that names no `source_session` is never kept, because the dead letter of a
copy that expires goes to that session and there is none: its 404's `error` names the role and
says that nothing was kept and why, and the listener reads that the role has no live holder from
the role bucket stream's leader and the holder's registration from the session bucket stream's
leader, never from a copy (`roleSend.resolve`). A `holder_lapsed` 404 names the same
holder fields and `claim_released: false`. Also the 503 or 500 `GET /v1/roles/<role>` gives when
its first resolution of
the holder, within 3.5 s of the request's arrival, fails (see "Send and publish answers"); `403`
with `reason: "capability"` when the holder does not take the
payload's delivery mode; and `504` with `reason`, `event_id`, `dedupe_key`, `holder` and
`attempts` once the four seconds passed, which keeps nothing, since keeping is the 202 answer
above. That reason is always `delivery_failed`, the one 504 reason the route gives: the message
is not known to have been stored for its holder (no live holder to forward to, a holder lookup
that failed or ran out of time, a publish or flush to the server that failed or timed out, or the
stream did not acknowledge storing the forward,
`bus.ErrForwardNotStored`). A forward the stream stored later than `roles.ForwardFenceAge` and the
route's 3.5 s budget after the resolution that chose its holder (`roles.ForwardStoredTooLate`)
may sit on a holder that lost the role. The route reads the role's claim from the role bucket
stream's leader before it answers (`withdrawLateForward`). When the claim still names the
forward's holder, the holder reads the forward from its own subject, a takeover copies it should
the holder lose the role later, and the send is answered `200`. When the claim names another
session or none, the route withdraws the forward. It publishes a withdrawal mark to the role's
kept lane under the `Nats-Msg-Id` that a takeover pass's or the session relay's copy of that frame
takes, and the stream's own duplicate window (72 hours, longer than any takeover record's life)
decides which landed first. When the mark landed first, no copy of the frame can store, and the
send is answered `504 delivery_failed` with an error that wraps `roles.ErrForwardStoredTooLate`;
the sender's retry resolves the holder again and is the message's delivery, unless the role is
back with the forward's holder by then, when the retry meets the withdrawn frame as a duplicate
and is answered `200` for a frame no takeover copies. When a copy landed first, the mark comes
back a duplicate, the copy is already on the kept lane, and the send is answered `200`. When the
claim cannot be read, or no mark can be published at all, the send is answered `503` with
`code: "maybe_stored"` and `Retry-After: 1`. That branch can give the role's holder a second copy
under a new event id if a later pass copies the forward and the sender retries: that duplicate is
accepted in place of a loss nothing reports. Nothing is deleted: the forward stays on the holder's
subject and the mark on the kept lane, where the walk ends it with an Info line and forwards it to
no one. A send whose last attempt ended while the stream still refused its forward as in process
is answered instead `503` with `code: "maybe_stored"`, `Retry-After: 1` and the error `envoy: an
earlier attempt of this message is still being stored: <reason>`, recorded
`earlier_attempt_in_process`: the stream may yet store the earlier forward for the holder
(`roleSend.maybeStored`). The route publishes no exception: its sender has the answer.

The listener keeps a role message on a stored subject of its own,
`notifications.envoy.kept.role.<role>`. No session may register that subject, and no consumer of
the notification stream delivers it, so a kept copy never re-enters the delivery path it was
taken out of. `notifications.role.>` is still not a stream subject. The next
session that claims the role is handed the role's kept copies off its own claim request, and the
claim is answered before they are forwarded. One walk of a role's lane is bounded - twenty
forwards, two hundred copies read, sixty seconds - and the handover around it walks the lane again,
for the role's holder as it resolves it each time, until the lane is empty, a walk with no live
holder stops at a copy inside its hour, or three walks running make no progress at all; a walk
that failed is waited out on a widening cadence while the holder is live. The role's cursor,
`notifications.envoy.kept.cursor.<role>`, carries the lane's progress and one walk's claim of it,
so two listeners walking one lane do not forward a copy twice. A handover this process lost - a
restart, a rolling deploy - is picked up by a sweep every five minutes, which hands over every lane
that holds copies after its cursor, whether the role has a live holder, a holder that is not live
or no claim at all. Each listener sweeps at a phase of its own in each five minutes of the clock,
read from a hash of its host name (in Kubernetes, the pod's name), so its first sweep comes within
five minutes of its start and two listeners started together do not sweep together. A walk that
finds no live holder ends each copy whose hour ended and stops at the first copy inside its hour,
which waits for the role's next live holder. The walk and the handover, the sweep's included, read
who holds the role from its claim record in the role bucket, never from the listener's copy of the
claims: a walk reads the record before each copy and goes on only while it names the holder the walk
forwards to, and a read that did not answer ends the walk as a failed one. Each forwarded copy
carries `kept_at`, the Unix-millisecond time the listener kept it, so the holder can judge the
message's age.

Each copy is kept for one hour from the time the stream stored it. No walk forwards a copy whose
hour ended: the first walk that reads it drops it, whether or not any session holds the role, and
counts it on `envoy_role_kept_expired_total`. When the listener kept that copy and its publish
named a sender session, the walk sends that session a dead letter; the publish route keeps no copy
for a publish that names none. The walk also sends that
session a dead letter for such a copy inside its hour whose live holder does not take the copy's
delivery mode, and `envoy_role_dead_letters_total` counts both. The dead letter is a stored
envelope from source `envoy` on the sender's agent subject, and it names the role, the copy's
event id, its first line, why Envoy dropped it, and when Envoy kept it and dropped it. The summary
of a copy from source `dispatch` is the head of a Dispatch comment's or message's body, which its
author can edit or delete before the copy's hour ends, so that copy's dead letter quotes none of
it: it names the post by its id and its `dispatch://` reference, read from the Dispatch event the
copy's payload holds, or by the copy's event id alone where that event names no post
(`keptCopyNamed`). The stream
holds one dead letter for each copy, however many walks send it. A copy whose hour ended more
than an hour before the walk, and that no walk has claimed, is dropped as stale with no dead
letter. A copy Dispatch's outbox kept gets no dead letter at all: whatever its age, the walk tells
Dispatch with an expiry notice instead (below). While a lane holds a copy the listener kept with
at least twenty minutes of its hour left, and no live session holds the role, each sweep raises
the role's wake report again, at most once for a role in each five-minute interval across every
listener. `GET /v1/roles/<role>`
still answers 404 for an unheld or lapsed role: only the publish route gained the kept answer, and
the send route's for a send Dispatch marks with its role, whose role moved before it arrived. A
sender that keeps a queue of its own and redelivers from it asks not to be kept (`keep: false` in
the publish body) and gets the 404. The Legion daemon's notice queue does. The opt-out is the
route's alone: a message published straight onto a role lane is always kept. Dispatch's
role-routed events do not cross the lane: the outbox keeps its own copies on the kept lane (below).

A released Legion role gets the copies kept for it when it comes back. The release ends the
role's process. A pi-envoy stop that finishes gives back each role claim the process holds, and
then deletes the session's record (`releaseRoleClaims`, `packages/pi-envoy/extensions/envoy.ts`).
Once the claims are given back the role has no claim, so a message to it is kept as `unclaimed`
at once. A stop that gives back nothing leaves the claim standing. claude-envoy's stop gives back
nothing and only deletes the record, an older pi-envoy's stop gives back nothing, and so may a
pi-envoy stop whose request to give the claims back is not answered within its share of the stop
(250 ms), since the stop goes on without the answer. Such a stop still deletes the
record when its DELETE reaches the listener, and the holder then reads `gone`. A message to the
role is then kept as `holder_lapsed`, and as `unclaimed` once the role claim reaper releases the
claim, at its first pass after the claim's last renewal is older than the live window
(`internal/store/reaper.go`). When such a stop deletes nothing either, the record stays. The
holder then reads live for the live window after its last heartbeat, and a message to the role
is forwarded to its agent subject, not kept. Past the live window the holder reads
`unresponsive`, and a message to the role is kept as `holder_lapsed`. The record reads `gone` at
the gone horizon in a bucket this listener creates, or at its 5-minute age in a bucket an older
build created, about when the live window ends. The role claim reaper releases the claim at its
first pass after that, and a message to the role is kept as `unclaimed` from then. Each keep
raises the role's wake report. When the role comes back, its session registers again or claims
the role, and either one hands it each copy kept for the role whose hour has not ended, as an
Envoy card that carries `kept_at` (a registration that makes the session live again starts that
handover, in `subscribeHandler`). The card can arrive before the task the daemon gives the woken
role (`WakeTask`, `packages/daemon/internal/workflow/wake.go`). `envoy_inbox` lists it, but the
inbox is the memory of the running process, so in a new pod it starts empty and holds only what
that process received.
To check one message, read the session's transcript for the card with that event id and `kept_at`.
The listener's log holds the forward of that event id, `listener role forwarded` with `kept_at`, or
the drop line `listener dropped a kept role message older than the keep window`. The sender's
session holds the dead letter that names that event id, when the publish named that session and
the copy was not dropped as stale.

A role taken by a different session gets the role messages its previous holder did not read. A
holder stays live for five minutes after its last registration, and every role route judges it by
that age alone, so a message sent to the role while the holder had crashed inside that window is
stored on the holder's own subject (`notifications.agent.<holder>`), where only that session
reads it. Before each try of a claim's write, when the claim record that try read names another
session, the claim route publishes a takeover record on a lane of the listener's own,
`notifications.envoy.takeover.<role>.<previous holder>.<attempt>`, with the claim's time. The
stream carries that lane, no consumer filter takes it and no session may register it, as the
resume lane. Every claim writes with the revision of the claim record it read, and an outright
claim reads again and writes again on a conflict until its request's budget ends, then answers
`503` with `Retry-After: 1` and writes nothing, so the record names the holder the claim really
replaced. A try whose record did not store - a full stream, a write nothing answered within a
second, a leader election - writes no claim either and answers `503` with `Retry-After: 1`, so a
claim that lands always has its record; the role stays with the holder the claim read, and the
Legion claim boot claims again after a wait that starts at one second and doubles up to thirty. A
pass of a record first reads the role's claim. While it names
the record's previous holder - the claim did not land yet or at all, or the role went back to it -
the pass copies nothing, since that holder receives its role messages from its own consumer and a
copy would come back to the subject it was taken from as a new frame. The first such pass of a
record that had moved sets it back to reading that holder's floor anew, so the messages the holder
reads while it holds the role are not copied when it loses the role again. Otherwise the pass reads
the previous holder's subject
from the acknowledgement floor of its session consumer on (or the floor the listener recorded when
it deleted that consumer, or, with neither, from an hour before the claim, on the sweep's next
pass), and copies each role message of that role stored less than an hour before the claim or
after it to the role's kept lane. A record that found its previous holder holding the role again
writes when it did, and counts that hour from then instead, since the holder read its messages
until then. A pass writes the record when it changed it (a new sequence, a missed floor, the
record finished) and otherwise only once its last write is fifteen minutes old, so a record nothing
moves costs four writes an hour however many listeners sweep it, and never ages out while a pass
needs it. A role message is one whose topic is the role's, or whose `role` field names it. A pass,
the relay, the kept lane's walk and an edge listener's frame rule tell it by one read
(`readRoleFrame`): past a leading UTF-8 byte order mark, the message's top-level fields are split
by their brackets and strings, at any depth,
as the plugins' `JSON.parse` splits them, and only its `topic` and `role` fields are read, each by
its exact key, the last of a repeated key winning. The plugins read `topic` the same way; they
never read `role` from an inbound frame, so the rule for it is the listener's own. So a later
`"TOPIC"` key, an event id written as a number, or a value nested deeper than the 10,000 levels
`encoding/json` reads does not change what it is. That read keeps the event id beside the two and
no other field, so the relay's read of a frame makes no allocation for each key the frame holds,
and its error names a field and its JSON type, or a key quoted and cut to 40 characters of the
quote, never a whole value: the walk logs it. Only for a role message to copy is the envelope then
read, field by field from every field, a field whose value its type does not take left unset, so
the message is still copied whatever the listener's wire rules or the envelope's own say of it.
An `urgency` or `source` word a newer build adds is kept in the copy, for the holder's reader to
judge, and an `issued_at`
written as `1795191371979.0` is read as that integer, while one written as a JSON string is left
out, as `encoding/json` leaves it.
An `expects_reply` word the envelope does not know is a value that field cannot hold, so the copy
leaves it out, and the holder reads the message with no `expects_reply`. A message
that is not a JSON object, or whose topic or `role` is not a string and names no role, is not
copied: it stays for the previous holder's own consumer, as a message that is no role message
does. The kept lane's walk ends a copy of that kind, which no listener copies there. The keep that
meets a copy as a duplicate and the sweep's wake scan read a kept copy the same way as the walk, so
a copy the walk forwards is held by the one and wakes its role in the other. The copy
is the envelope without the forward mark, with the message's event id and a dedupe key of its own
in the minted form (32 hex digits of the SHA-256 of the message's key and the frame's stream
sequence, after `publish.` when the message's key has it, as the publish route's keys do, and after
`agent.takeover.` otherwise, so a copy of a copy is still the listener's and not Dispatch's outbox's
by its key), under the kept message id followed by `:frame:` and that sequence, so two
copies of one frame are stored once and no copy is dropped as a repeat of an earlier kept copy of
the same message. A minted key names its event, so every walk, a listener of the release before
included, forwards the copy under that key: a role taken back reaches its previous holder, where
the role lane's own forward of the message to it would have the stream drop the copy, and a host
that already showed the message drops the copy by its event id. A pass reads at most
two hundred messages and writes the record again with the sequence it reached. The claim route's
handover runs passes before it walks the lane, and the sweep runs passes of every record that is
not finished every five minutes, so a listener that stopped at any step delays a message and loses
none. A record is finished by the first pass that starts more than one hour after the claim, the
keep window, and reads to the end of the subject; no pass runs it again or writes over it, each
listener reads it again only when the age limit takes one of its subject's older messages, and it
ages out with the stream. Every forward that resolved the previous holder before the
claim starts inside the fence below, so it is stored before that pass reads. A listener whose NATS
user holds none of the session consumers' calls, Dispatch's and a host's, runs no pass and reads no
record: it asks the server once per ten minutes whether its user may read a session consumer, and
the listeners whose users may run every pass. A release of the role, the reaper's delete of its
claim and a claim no read answers leave the records where they are.

An edge listener's frame rule (`edgeFrameVerdict`, `cmd/listener/edge_frames.go`) tells a role
message by the same read, inside a rule of its own. Before it reads the role, it withholds a frame
that names `topic` or `role` under more than one key or under a key that differs from the name
only in case (`ambiguous_key`), so a later `"TOPIC"` or `"ROLE"` key withholds the frame there
rather than leaving its role as it is, a frame with a top-level `__proto__` key, and a Dispatch
frame whose payload is no event of a project the token lists, unless it is a person's message to
the session in its own conversation on its own agent subject (`dispatchDirectMessage`). It then
judges a role message by its role only when its topic is a role lane or an agent subject in the old
form or in the new form under one of the token's projects (`roleFrameVerdict`,
`roleMarkOnAgentSubject`); a role-marked frame with any other topic, or none, it judges by its
topic, as a frame with no role mark.

A session that replaces another of its own principal (below) gets the replaced session's unread
messages, whether or not the replaced session's claim of the role still stands. The role claim
reaper frees a dead holder's claim, so the claim of the session that replaces it often reads no
holder and writes no takeover record; the claimant names the session it replaces instead, in
`previous_session_id`, on a soft claim or an outright one (a Legion session names the session its
claim abandoned, which the daemon's registration answers as `replaces`). Once such a claim lands,
the claim route writes a handover record, a takeover record whose attempt token is
`handover-<claimant>` (`bus.HandoverSubject`): one subject for a claimant and the session it
names, so a claim made again at each registration costs no second record, and a write that finds
the record is no error.
A record that did not store, and a rule the listener could not read (below), answer the claim
`503` with `Retry-After: 1` and the code `handover_unrecorded` (`listenerwire.CodeHandoverUnrecorded`):
the claim stands, and the client's next claim judges and writes the record. A `previous_session_id`
no session consumer can be named for (one holding `.`, `*`, `>`, `/`, `\` or whitespace) is
answered `400` before anything is written. A handover record's passes are a takeover record's,
with five differences.

First, while the replaced session is `live` they copy nothing and set the record back as a pass
that finds the previous holder holding the role does: a session that answers reads its own
messages, and a person's direct messages go to no other session while that person's session
answers. A handover record's life runs from the last pass that found the replaced session live, or
holding the role, where that is later than the claim, and such a pass never finishes it: a session
that stays live past the record's life and then stops is handed over then.

Second, they copy nothing either, and set the record back the same way without moving its life,
while the two sessions' last registrations do not name one principal: a session that registered
as another principal since the claim, or while the pass runs, is handed nothing more. The two
rules are one gate (`handoverFence`), judged before the pass and again as the last step before
each copy: the session's state from the listener's cache and, where that does not read it live,
from the session bucket's leader (`sessionLiveNow`), and both registrations from the known-session
bucket's leader (`handoverStands`), each copy held to the principal the pass started under. So a
registration that lands while a pass runs, through any listener, ends it before its next copy.

Third, they copy more of the replaced session's subject: a role message of any role goes to that
role's kept lane, whose walk hands it to the role's next live holder within the hour (the
claimant, for each role it claims), and a message whose topic is the replaced session's own agent
subject, in either form - a direct message to it, or a dead letter - goes to the claimant's own
agent subject, with the claimant's subject as its topic, the message's event id and sender, and a
dedupe key in the minted form, under the message id
`notifications.agent.<claimant>:handover:<event id>`, so the stream keeps one copy of a message
for a session however many passes, records and hops make it, and two sessions that each replaced
the other copy a message to and fro once. A message that names no event id is given one on its
first copy, drawn from the frame it was first read from (`handedOverEventID`), and every later
copy keeps it. A direct message has no hour: it is copied however long before the claim it was
stored, from the replaced session's floor on.

Fourth, with no floor known, a pass starts at the first message the stream holds on the replaced
session's subject, not an hour before the claim as a takeover record's does, so a direct message
stored earlier and never read is handed over too, along with what the session read in the
stream's retention.

Fifth, a handover record lives the stream's retention, 72 hours (`handoverRecordLife`), not the
one-hour keep window: nothing fences a direct send to a session as the role lane's fence fences a
forward, and a sender can write to the replaced session long after the claim (a person who answers
a Dispatch ask the session made, a peer that knows its id). Each message sent to the replaced
session within that time is handed over; one sent after the record finished stays on the replaced
session's own subject for the stream's retention, as every message to a session that is not live
did before handovers. While it lives, each sweep's pass of the record on each listener that runs
passes reads the role's claim, the replaced session's state, both principals and the replaced
session's subject, and writes the record again every 15 minutes.

Delivery is at least once, as for a takeover: a message the replaced session acknowledged out of
order above its floor reaches the claimant too. A handover record's stored body names no role and
no previous session, which its subject names, so a listener of the release before leaves it out as
a record no pass can act on, rather than run it as a takeover record and move it past direct
messages it did not copy. An edge listener writes no handover record, as it writes no takeover
record, and answers such a claim with a warning that says so.

A claim hands over only between sessions of one principal (`handoverRefusal`). The principal a
request's credential acts for names the credential's kind and scope beside the person or service
(`requestPrincipal`; `internal/ciphertoken/principal.go` spells each form): a broker token scoped
to its person, a broker token an operator's entry gave a person for a project (a worker pod's), a
broker token naming no person, an identity token, and none for the shared token or an open
`/v1`. So a person's own session and an agent session acting for that person on a project entry
are two principals.
Each registration records its credential's principal, none included,
on the session's known-session record, and marks it recorded (`session.KnownSession.Principal`,
`PrincipalRecorded`). The record's other writes, an interest change and a clean stop, keep the
principal the bucket's stream leader holds, writing over the revision they read it at, so a
listener whose copy has not applied another listener's registration yet never writes the
principal that registration replaced back over it; where the leader does not answer they write
nothing. Before a registration changes anything of the session (its entry, its consumer, its
relay, a role claim it releases or renews), the listener reads the session's record from the
bucket's leader, and where it names another principal, recorded, writes it again with no
principal recorded over the revision the read found, keeping its topics, title, capabilities and
last_seen (`session.KnownSessions.AdmitRegistration`): from then on the gate refuses every
handover to or from the session until the registration's own write records the new principal,
while a send to the session and a publish's count still find the record if that write never
comes, as for a stopped session whose registration is refused after its admission. A record
stored in chunks is joined and written again with every topic it held, and one whose chunks are
gone is written whole from its manifest's view. A leader that does not answer, and a clear the
bucket refuses, answer the registration `503` with `Retry-After: 1`
(`session.ErrPrincipalUnrecorded`) with nothing of it admitted. A listener whose known-session
bucket is not open (its NATS grant does not carry the bucket, or its store's first attempt, which
waits 2 s, did not open it) can read and write no record until an attempt of its store's own
opens it, 1 s doubling to at most 5 minutes apart, which takes a grant that lands later with no
restart: it admits each registration, heartbeats included, with no principal recorded, answers it
with a warning that says so, and logs that at most once a minute; and it refuses every claim's
handover until its bucket opens. A registration admitted while the bucket was not open writes no
record even where the bucket opened since (`session.Admission`). The
admission answers the revision it read or wrote (`session.Admission`), and the registration's
own write is a compare-and-swap at exactly that revision: a record written since - by another
registration of the session, through any listener, whose admission cleared this principal or
whose write recorded its own, or by an interest change or a stop - is left as it is, and the
registration writes nothing and is answered the same `503`, so a registration whose write is
delayed past another one's admission never puts back a principal that admission cleared. Of two
registrations of one session admitted before either writes, the one that writes first stands,
and the other registers again at once; until it does, its session entry can stand beside a
record naming the first one's principal. A registration whose record a listener's copy holds
unchanged inside the refresh age is skipped only where its admission found the leader's record
naming its principal and the leader still holds that revision; otherwise it is the
compare-and-swap write, so a skip never answers `200` over a record written since the admission.
A registration whose write failed is answered the same `503` unless the leader confirms the
record names its principal, none recorded, or nothing; so the session registers again until the
record names the principal it registers as. What no step fences is a registration whose listener
stops between the session entry's write and the record's write: the record another registration
wrote since stands beside its entry until its client registers again. The claim
route writes the handover record
only when the claim's credential, the claimant's last registration and the replaced session's
last registration name one principal, none equal to none, each registration read from the
bucket's leader (`session.KnownSessions.Read`). A replaced session with no record, one whose
principal no registration of this build recorded, a listener with no known-session store, and a
listener whose known-session bucket is not open are refused. A refused claim stands, writes no
handover record, is answered `200` with a warning that
no message of that session is handed over, and is logged at INFO with the role and both sessions;
the replaced session's consumer keeps its messages for that session. A refused claim that took
the role from the session it names still moves that role's own unread messages to the role's
holder, as every takeover does, since they were sent to the role, and its warning says so. What
the listener could not read is no refusal: a known-session leader that did not answer, and a
claimant whose own registration is not recorded with its principal yet, answer the
claim `503` with `Retry-After: 1`, the claim standing, and the client's next claim judges it
again. A session's own broker token proves that the claimant is itself, not that it continues the
session it names, so without this rule any session could read a stopped session's messages by
naming it, a person's among them. An agent session that names a person's session is refused by
design: its credential acts for its service, for another principal, or for that person on a
project an operator's entry names, never as the person's own token. An agent that runs on its
person's own sign-in token is that person here, since nothing the broker answers tells the two
apart. Dispatch
lets a session answer a delivery attempt of the session it replaced only where the successor read
names it, which holds both sessions to this rule and follows a chain of replacements of one
principal (`handoverSuccessors`), and, on a credential that proves the replying session (a broker
token), only where that credential's principal, in the same form, is the principal the read
names (`actsForSuccessorPrincipal`). The read names that principal only to Dispatch's own verified
identity (`ENVOY_DISPATCH_PRINCIPAL`), and answers every other caller the same successors with no
principal, so no session reads which person or service another session registered as; a listener
whose known-session bucket is not open names no successor, as it hands nothing over. A listener of an earlier build reads a
known-session record with its principal unknown and writes it back without one, so a record such
a listener rewrote names no recorded principal until its session's next registration through
this build, and a claim naming that session is refused until then, whatever its credential.

The session relay delivers a role message to a session only while the role's claim names that
session. It reads the claim from the role bucket for each frame that is a role message; when the
claim names another session, or none, it copies the frame to the role's kept lane as a pass does,
acknowledges it only once the stream stored the copy, and logs `listener session relay copied a
role message to the role's kept lane` with the session, the role, the holder and the event id. A
frame under such a claim that the listener's wire rules or the envelope's own refuse is copied the
same way, with the values the copy keeps as above, and the walk forwards it. A frame whose role
no read can tell (not a JSON object, or a topic or `role` that is not a string and names no role)
is sent to the session whose subject holds it, as a frame that is no role message is. So the relay
ends no message: every role message it tells is copied or delivered. A claim read or a copy that
failed gives the frame back with a delay, neither delivered nor acknowledged. A frame that is no
role message costs the read of its topic and role and nothing
else. A forward to a holder from the role lane, the publish route or Dispatch's outbox whose
resolution is 55 minutes old when its publish starts (`roles.ForwardFenceAge`, the keep window
less one sweep interval) resolves the holder again first, so a forward the previous holder was
chosen for is stored inside the window a takeover's passes read. The outbox judges the stream's
acknowledgement of its forward the same way: one stored later than that fence and its
acknowledgement window (`roleForwardAckTimeout`, 5 s) after the resolution is no delivery
(`roles.ForwardStoredTooLate`), and the event stays pending for the next read to resolve the
holder again. Delivery stays at least once: a
frame the previous holder was being handed at the takeover, one it acknowledged out of order above
its floor, and every role message of the hour before a takeover whose holder's floor is unknown
can reach both sessions.

Every other `notifications.` topic the route takes is published to the stream and answered `200`
once the stream stored it. The route forwards it to nobody: each session that registered the
topic reads it from its own consumer, which the listener holding that session relays, whether the
session was connected at the publish or comes back later. A topic that neither the deployed
stream nor this build captures is refused `400` by name (`bus.ReachesNoSession`), since nothing
would store it and no session would ever receive it, and no retry turns that into a delivery;
`notifications.team.<name>`, the one namespace an agent names itself, is captured and stored. A
topic this build stores is never sent over core NATS: while the deployed stream, as the
listener's client last read its subjects, does not capture it (a hub move's quiesce, an
operator's edit of the subjects), the bus sends nothing and refuses the publish
(`bus.ErrSubjectNotDeployed`), and the route answers the `503` that says the message was not
sent, with `Retry-After: 1` (the send route's answer for an agent subject above). The caller may
send it again later: `envoy-client` sends it once more 250 ms later and then hands the caller the
error, which no wait that short outlasts. The refusal ends at the client's next read of the
subjects after the stream captures the topic again, at most 70 s when that read succeeds.

The role lane, a core-NATS queue subscriber in each listener, carries a role topic a client
publishes straight onto NATS rather than through the route. It resolves the holder at delivery
time, from the role's claim record read from the role bucket within two seconds and never from the
listener's copy of the claims, and forwards once. A role with no live holder, unclaimed or with
a holder that is not live, is kept for the role's next live holder first, as the publish route
keeps one, and a message that was kept publishes no exception and counts as `kept`. A claim
read that did not answer keeps the message too, under the reason `claim_unread`: the lane has
no redelivery, so a kept copy is how the message waits for a read that answers. The handover
the keep starts, or else the sweep's, forwards it only to a live holder: a handover that finds
no live holder forwards nothing, so no exception is published for the copy, and it waits until
a handover finds a live holder inside its hour. Once its hour ends, the first walk that reads it
drops it with a WARN, and sends the dead letter above when the copy names a sender session. A
keep that failed leaves its branch as it was. The lane's outcome is then one of two exception
reasons:
`no_holder` when no session claims the role and its keep failed, and `delivery_failed` when the
holder is not live and its keep failed (an `unresponsive` holder among them, whose claim the
lane leaves alone), when the holder's session read failed, when a claim read failed and its
keep failed too, or when the stream did not acknowledge storing the forward. A role claimant
does not subscribe directly to its role subject.
The forward is stored on the holder's agent subject (one copy per message and holder, by its
`Nats-Msg-Id`), so a holder on consumer delivery that was away gets it from its session consumer
when it is back.

Dispatch's outbox forwards its own role-routed events to the holder itself: it resolves the
holder through the one resolution the listener's routes and its lane use (`roles.ResolveHolder`,
`internal/roles`) and sends the envelope, its role topic kept, to the holder's agent subject, so
Dispatch's events no longer cross `notifications.role.*`. That resolution writes nothing to the
role bucket here either. A role with no claim, and one whose holder is `gone`, is kept on that
role's kept lane for one hour for the holder that claims it next, as the listener's own copies
are, and the `gone` holder's claim is left where it is for the role claim reaper. When an outbox
copy's hour ends with no session taking it, the walk that drops it sends no dead letter: it stores
an expiry notice on `notifications.envoy.kept.expired.<role>` instead (`bus.KeptCopyExpiry`: the
event id, the role, the attempt the copy was kept for, its dedupe key, its stream sequence, and when
it was kept and dropped), one per copy under the message id `envoy.kept-expired.<role>.<sequence>`,
counted on `envoy_role_kept_expiry_notices_total`. The subject lies under the listener's own kept
lane. So only a NATS user whose grant publishes every notification topic writes it: `Listener`,
`Dispatch` (its envoy-listener runs beside envoy-dispatch), and the legacy `ListenerHost` and
`Host` sets built on the listener's (`natsgrant/natsgrant.go`). Dispatch reads it with the
stream's stored-message read its grant holds; `ListenerHost` and `Host` also subscribe every
notification topic, so they receive it. No session's own set (`Session`, `SessionPattern`) can
read it: no consumer filter takes it, no session may register an interest reaching it, those sets
hold no stream read, and the publish route refuses it. A legacy host's sessions that share its
`Host` user share that user's reach. Dispatch's outbox reads the notices from the stream every
30 seconds, and after every scan, with the stored-message read it already holds
(`readKeptExpiries`). Its cursor in `outbox_kept_expiry_cursor` names the stream it counts in by
the stream's identity (`bus.StreamIDMetadataKey`), which the outbox reads at most every 30
seconds: a stream created again numbers its notices below a cursor of the one before it, so the
outbox sets such a cursor back to the new stream's start, with one Error line, and reads its
notices from the first. It passes over a notice whose role is no valid role name, that was stored
on another role's expiry subject, whose event was never routed to that role, or that does not
name that event's copy: the notice's sequence is not below its own; the role's kept lane holds a
message there that no read can decode, or another copy than Dispatch's copy of that event under
the notice's dedupe key; or the lane holds nothing there, and the sequence is at or past the
first message the stream still holds, so the stream's retention did not remove a copy there: the
stream removes messages oldest first by its age alone, since it discards new messages at its byte
limit, and this assumes it has no per-subject limit, which would remove older messages mid-stream. A
passed-over notice writes no row, so nothing is kept again and nothing is recorded undelivered.
It records each other notice in `outbox_role_copy_expiries`, keyed by the event, the role and
the attempt, which is the outbox's own next attempt for that pair (the notice's own attempt is
logged), so a repeated notice reopens the event's delivery to that role once. It then keeps the
event's copy again, read
back from the kept lane by its sequence, under the next attempt (`bus.KeptAttemptMsgID`), or
forwards it to the session that holds the role by then (`keepExpiredCopiesAgain`); nothing else
of the event is sent again. After the third keep (`maxRoleKeeps`) has expired, the outbox stops
and appends a `role.undelivered` event on the event's own issue, document or project, written by
a system actor so it reaches no role, which a person reads in that owner's history. It stops the
same way, with fewer keeps, when a copy cannot be kept again: the stream's retention has removed
it; the stream holds at its sequence a message no read can decode or another event's copy, which
only a stream created again since the notice's record can hold there; or NATS refused its keep or
its forward for good. Where the bound
is reached or no other copy carries its delivery on, every expiry of the event still owed a keep
is settled, and its one `role.undelivered` record is appended, so nothing keeps it again. A copy
that cannot be kept again ends nothing while another copy of the event carries its delivery on,
that is, while another expiry of it is still owed a keep or was kept, forwarded or skipped after
this one was recorded; its row is settled `carried`, and nothing else is written. The outbox also
notes the event in
`outbox_role_undelivered_notes` for a message on the event's issue, or on the issue of the
document a document event is about: the first such note of a role on an issue opens a one-hour
window, every later note of that role there joins it, and once the hour has ended the outbox
posts one message naming every event of the window, through the issue message route's own writer
(`api.Background.PostSystemMessage`) under the same system actor, so it wakes no route either.
The window is kept in Postgres, and its message is written under an id derived from the issue,
the role, the moment the window opened and the events it names. So a restart does not lose the
window, and a post made again after a crash between the post and its mark writes no second
message. A note joins only a window whose message is not posted yet. A long window's events go in
several lists, each at most 50 events and 16,000 UTF-16 units, so no block of the message passes
the writer's 32,000-unit block bound. A reference over 4,096 units, which only a document kept
from before the name bound can have, is named by the document's id and the reference's length
instead. Only the publisher hold's holder records a notice, claims an expiry or posts a window.
Each write that may append a `role.undelivered` record - a notice's record, and a record of a
copy that cannot be kept again - is one holder transaction: it reads the hold's row first with
no lock, and locks it for share only as its last statement before the commit, rolling back when
the hold is no longer its own. So a takeover of the hold either lands before that lock and makes
the record roll back, or waits for its commit, and the new holder reads every write it made; and
the hold's renewal never waits behind a record that is itself waiting on an issue's lock. The
pass's other writes to `outbox_role_copy_expiries` are single statements that check the hold
themselves. An event of
no issue and
of no issue's document never has a copy kept for a role, since the route is read from the issue
alone (`readBatchSQL`, `coalesce(i.route, ai.route)`), and such an expiry logs one Error line. An
`unresponsive` holder's event stays pending and is retried, as a read that failed is, until that
session answers or its record passes the gone horizon: a copy kept for it would spend one of
those three keeps on a session that is still alive. The outbox raises no exception.
That forward is stored only: one copy on the holder's agent subject under its own
`Nats-Msg-Id` (`bus.ForwardMsgID` of the forward's dedupe key and that subject), counted
reached on the stream's acknowledgement of the copy and on nothing else, with no receipt from
any holder, since the holder reads the copy from its own session consumer; a forward the stream
did not acknowledge stays pending in the outbox and is retried.
The listener's role lane names its own forward by the same rule with one exception
(`roleForward.msgID`): the forward's dedupe key and the agent subject when the original
envelope's dedupe key, before the forward's `envoy.role.forward.` prefix is added, names its
event, and the envelope's `event_id` and the agent subject otherwise, since two distinct
envelopes can share a key their sender chose.
So the stream stores one copy of a message per recipient however often it is forwarded, and
two messages under one key as two.

When a role delivery has no holder, or the stream stored no forward for the holder it found,
Envoy emits an envelope on `notifications.envoy.exceptions.<original-topic>`. So does a role-lane
publish the API refuses because no live session holds the role: that refusal is still answered
`404`, and the exception goes out behind the answer, on a goroutine of its own, so it never delays
it. This adds a bus signal only: no Legion consumer acts on a peer's refusal. Upgrade every Legion
daemon before the listener: an older daemon reads either refusal reason as malformed and logs
`ERROR role-lane exception not re-held` once for each refused attempt to publish its own notice. A
held notice row tries again after 1, 2, 4, 8, 16 and 32 s and then every 64 s, and only the oldest
held notice of each architect publishes, so that is about seven ERROR lines in the first minute and
then one every 64 s for each architect whose role is unheld while it has a notice to deliver. The
exception payload preserves the original delivery fields:

| Field | Meaning |
| --- | --- |
| `original_topic` | Original envelope topic |
| `event_id` | Original envelope event ID |
| `reason` | A delivery that failed: `no_holder` or `delivery_failed` (defined above). A publish the API refused: `unclaimed` or `holder_lapsed`, the refusal's own state, told apart from `no_holder` because nothing was forwarded |
| `payload_summary`, `payload` | Original human summary and distinct machine payload. A refusal's exception carries the payload only up to 64 KiB (`refusalExceptionBound`) and leaves a larger one out, so a refusal never becomes a publish NATS refuses whole |
| `dedupe_key` | Original envelope dedupe key |
| `source`, `source_session` | Original envelope source fields |

A message on a session's agent subject raises no exception. The session's own consumer delivers
every such message, so the listener's durable delivers it to nobody, retries no delivery and gives
no message up: it decides only whether the session needs a wake report. A message to a session
with a record, `live` or `unresponsive`, is acknowledged and counted `skipped`, whatever the
session's consumer holds: that consumer delivers it, or the consumer of the session's next
registration does. A message whose session's interest row names another listener is that
listener's to decide, and this one acknowledges it. A message to a session with no record, or
with a record past the gone horizon, waits once. Its first delivery is sent back for
`ENVOY_UNPLACEABLE_RETRY_WINDOW` (default 60 s), counted from that delivery and not from the
message's age in the stream, so a session that is only restarting registers again and gets the
message from its consumer with no wake. When the session still has no record at the next
delivery, the listener publishes one wake report on `notifications.legion.wake` and waits up to
2 s for the stream to store it. A stored report logs INFO `listener agent wake reported`, counts
`wake_reported`, and the message is acknowledged. A report the stream did not store sends the
message back for the same wait, and at the stream's 20th and last delivery the listener logs WARN
`listener agent wake not stored` and acknowledges the message. A read of the record that does not
answer is neither a record nor the lack of one: the message is sent back for the wait, the
listener logs WARN `listener agent record unread` once for that message, and at the last delivery
it reports the wake as for no record. A message to a session with no record reports at once under
a wait at or below zero, on a core delivery, and on a durable delivery whose metadata does not
parse. A read that did not answer reports at once only on the last two; under a wait at or below
zero its message comes back with no delay until the last delivery.

At most 64 first deliveries wait at once — a quarter of the durable's 256 ack-pending slots, one
of which a waiting message holds for its whole wait — and a first delivery that finds that cap
reached reports its wake at once. A message sent back after a report the stream did not store, or
after a read that did not answer, takes a free slot when there is one and is sent back whether or
not it got one, so the 64 bounds the first deliveries' waits alone. The 64 is one budget for the
whole listener: a single set, made once at start and shared by every delivery handler built from
it, the one a self-health rebind builds after a NATS flap included, so a rebind opens no second
budget beside the first. The set is keyed on each message's JetStream stream sequence, so only
that message's next delivery gives its slot back, and a redelivery of a message this listener
never sent back (an expired ack wait) frees nothing. A slot whose message never comes back here
(the stream dropped or purged it, or its redelivery reached another subscriber) expires after the
wait plus two 60 s ack waits, 180 s at the default, the longest a waiting message can stay
pending, so such slots cannot hold the budget for longer than that.

A role lane publishes its exception on the attempt itself, since nothing redelivers a core message.
No path NAKs a message because its exception could not be published. Such a failure logs ERROR
`listener exception publish failed`. A fan-out refusal whose exception failed also clears the
attempt record the refusal wrote, so an identical redelivery decides the refusal again and
publishes its exception, where it would otherwise be skipped as already attempted. Role
exceptions use core NATS with the role lane. A team topic's exception keeps JetStream, since the
stream captures its lane. Exception topics do not produce further exception events.
