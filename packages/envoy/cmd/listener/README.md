# envoy-listener

The listener takes webhooks in, serves the `/v1` API, and delivers the events of one machine id (`ENVOY_MACHINE_ID`) to the sessions it owns.
Any number of listener processes may run for one machine id, in Kubernetes as pods of one Deployment:
one of them, the active pod, delivers what the machine's durable consumer `listener-<machine id>` carries;
every other is a standby that serves everything else and takes over delivery when the active pod stops or is lost.
This file holds how those pods share one machine id.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts packages/envoy/cmd/listener/README.md` checks the layout, and `--write` fixes it.

## Run model: an active pod and standbys

Every pod of a machine id, active or standby:

- serves `/v1`, `/healthz`, `/metrics` and the webhook routes;

- authenticates `/v1` by the same settings and counts what it admits in its own `envoy_listener_requests_total` (`apiauth.go`), warning once a minute of its own shared-token, credential-less and unverified broker-token admissions;
  a census of the shared token, the open `/v1` or an unverified broker token therefore reads every pod's `/metrics`, never one pod's;

- is a member of the role lane's core-NATS queue group: `envoy-roles` (`bus.RoleLaneQueue`), the one every listener of every machine shares, or, where its NATS user may not subscribe in that group, its machine's `envoy-listener-<machine id>`, chosen in that order at its start (`bus.ConnectRoleLane`) and at each resend of a subscription the server refused (`bus.Client.ResendRefusedCore`, which the self-health check calls);
  NATS hands each message of a queue group to one member, so two pods in one group never both forward a role message,
  and listeners left in different groups each forward it, of which the stream keeps one copy for the holder by the forward's `Nats-Msg-Id` (`roleForward.msgID`, `role_lane.go`);

- runs the self-health monitor over its caches and its own role lane; an inactive role lane is logged at each probe and is never a reason to rebuild or restart the pod (`isUnrecoverableSelfHealthFailure`);

- reports the Legion wakes the routes it serves raise
  — a role send with no live holder, a send to a session that is gone
  — and says it is alive and reporting on the same topic at its start and every `listenerReadyInterval` after it (`wake.go`).
  Every pod reports, because every pod serves those routes and one request reaches one of them;
  the report an agent delivery raises when it gives up is the holder's, off the durable path.
  Each report carries a fresh event id (`wake.go`), so the two pods share none;
  what makes two pods saying it the same answer twice is what the daemon keeps of a report:
  the greatest time any report named (`RecordListenerReady`, `packages/daemon/internal/store/wake.go`), which one live reporter is enough to hold up.

Only the pod holding the machine's **delivery lease** receives the durable consumer, writes its policy, and runs the loops one deliverer runs (`holderLoops`, `main.go`):
the interest reaper, the role-claim reaper, the session reaper, the interest marker collector, the sweep of the kept role lanes with the takeover passes it runs (`startKeptRoleSweep`), the session consumer sweep (`startSessionConsumerSweep`), on a listener with the GitHub webhook route the CI summary loop and, on an edge listener, the edge repository loop (`runEdgeRepositories`).
The one exception is a listener without a lease, whose NATS user lacks the grant for `envoy_listener` (`openDeliveryLease`, `main.go`):
it does all three with no lease, and receives while no other subscriber holds the durable ("Without the grant: no-lease mode" below).
A standby runs none of them, never creates or updates the durable, and registers no JetStream subscription, so a NATS restart leaves it nothing to re-subscribe.

### The delivery lease

The lease is the key `lease.<durable>` (`lease.listener-<machine id>`) in the KV bucket `envoy_listener` (`bus.ListenerStateBucket`), which the listener creates at `ENVOY_NATS_REPLICAS` replicas, file storage, one value per key and a 20 minute `MaxAge` (`listenerStateMaxAge`), raising a bucket an earlier build created with a shorter one on every open.
The `<durable>` of the lease's key is the durable's name as one KV key token (`bus.ListenerLeaseKey`): each byte outside nats.go's key alphabet, and a `.`, is `/` and two upper-case hex digits, since nats.go refuses the `%` a dotted machine id's durable spells its dots with.
So `host.example.internal`, whose durable is `listener-host%2Eexample%2Einternal`, holds the lease `lease.listener-host/252Eexample/252Einternal`, and an id with none of the characters the durable escapes keeps the key it has always had.
The 20 minutes are the lease's: a pod that starts within them after a killed holder's last renewal finds the holder's record and counts a push binding it finds as that holder's ("Two builds of the listener" below).
The lease record's value is JSON: `holder` (the holding pod's instance), `boot` (random per process start), `deliver_subject`, `service_since`, `acquired_at`, `released`, and `stale_binding` and `unsettled` ("Take-over" below);
a field this build does not know is kept across every rewrite.

| Constant (`lease.go`) | Value | What it does |
| --- | --- | --- |
| `leaseRenewEvery` | 1 s | The holder renews the record with a compare-and-set at its last revision. |
| `leaseTTL` | 5 s | A standby takes an unreleased record that has not changed for this long, measured on its own monotonic clock from when it first saw that revision. |
| `leaseMargin` | 1 s | The holder counts itself the holder until the start of its last successful renewal plus `leaseTTL` minus this, so its term ends before any standby can take the record. |
| `leaseStandbyPoll` | 500 ms | How often a standby reads the record. |
| `leaseReceiveLost` | 3 s | How long this pod's receive connection may be down before the holder stops renewing and releases the record ("Take-over" below). |

A standby takes an absent record with a create, a released one at once, and an unreleased one only once the TTL has passed; no wall clocks are compared.
A holder whose renewal finds its record taken (a compare-and-set conflict) steps down and unsubscribes.
The holder receives with a core subscription on the durable's deliver subject, never `nats.Bind`, and handles a message only while it holds the lease:
one received while it does not (the moment before an unsubscribe lands, or a subscription nats.go sent again after a reconnect in place) is dropped with no ack and no NAK, and the durable redelivers it to the holder after its ack wait.

Renewing is not the whole of holding.
The lease renews on a NATS connection of its own (`envoy-listener-lease <instance>`), while the durable is received on the pod's main client (`envoy-listener <instance>`), so the one can be up while the other is down.
The receive loop gives the lease one signal for that (`durableReceiver.receiveUp`, `lease.SetReceiveSignal`), the same state that makes `/healthz` 503 on the active pod:
the main client has a connected connection, and a term that is receiving is subscribed on that very connection.
A pod still preparing - reading the durable, waiting out a binding, settling before it has subscribed - counts as receiving, as `/healthz` counts it a standby rather than an inactive subscription.
The lease judges that signal once a round, for the holder's release and the standby's take alike ("Take-over" below).

The term's connection is compared with the bus client's own (`bus.Client.ConnectedConn`), not with the receive loop's copy of it:
a run of the reconnect hooks stops at the first hook that fails and main registers the KV rewatch hook ahead of the receive loop's (`durableReceiver.reconnected`), so a rewatch that keeps failing leaves the loop's copy naming the connection the client replaced - the same closed connection the term is subscribed on, which read against each other said this pod was receiving while it delivered nothing.
A reconnect in place keeps the connection and nats.go re-sends its subscriptions, so the signal reads up throughout one, its hooks included.
The loop's copy must name the client's connection too, term or no term:
the loop subscribes on its copy, so a standby whose copy still names a replaced connection takes no lease, since every term it started would subscribe nowhere.

The signal loads atomics and waits for nothing.
The lease reads it from the loop that renews the record, so it may wait on no lock a call into nats.go can hold:
nats.go holds its connection's lock across every reconnect dial and the connect init after it, which a route that drops the pod's packets leaves hanging for the 5 s connect timeout, and the receive loop's own lock is held across the subscribe that waits for that same lock.
The receive loop keeps its atomic current from each term's subscribe and end, and the bus client keeps its connected flag from its connection's connect, reconnect, disconnect and close callbacks (`bus.Client.Connected`), so no caller of it waits on a connection either.
Those events are the flag's only source:
an install sets it from what its connection's events have reported so far, under the same lock the events take, and never asks the connection, so no event that lands around an install is lost.
nats.go runs those callbacks one at a time on one goroutine per connection, so the client runs a connection event's own work - restoring its subscriptions and running the reconnect hooks, which make requests of the server - on a goroutine of its own (`Client.queueReconnect`):
on the callback goroutine, a cut during a slow hook waited behind it and the flag read up for as long as the hook ran.

The durable's ack wait is 6 s (`consumerAckWait`).
The holder sends `InProgress` every 2 s for every message it has received and not settled, queued or in hand, so a message waits out the ack wait only once the pod holding it has died or lost the lease.

The listener needs nats-server v2.15.0 or later, and the production hubs run v2.15.1 (the deployment's `nats:2.15.1-alpine`, and the clustered hub's chart).
The durable carries `MaxDeliver` (`consumerMaxDeliver`, 20), and with it set every nats-server from v2.10.29 through v2.14.x loses a message it is pushing to the durable's deliver subject as that subject's only subscriber goes:
the server counts the write undelivered (`server/stream.go` `internalLoop`, `consumer.didNotDeliver`) and takes one off the message's delivery count, which underflows a first delivery's count, having none, to 2^64-1 (`decDeliveryCount`, v2.10.29 `server/consumer.go:3533-3538`);
`hasMaxDeliveries` (`:1738-1765`) then drops the message at the ack wait, with no redelivery and no advisory, so no settle hold, hand-back or retry brings it back.
v2.15.0 fixed that subtraction (v2.15.1 `server/consumer.go:4866-4883`), where such a message comes back one ack wait (6 s) later like every other outstanding message.
The Go tests link the server `packages/envoy/go.mod` pins, v2.15.1, and `TestEveryBackloggedMessagePushedAtAVanishingSubscriberComesBackExactlyOnce` (`durable_backlog_cut_test.go`) holds it to that:
it fails with messages missing on v2.10.29.

### Take-over

- **Graceful stop** (SIGTERM on the active pod):
  the stopping pod stops handing messages to the handler, gives the message in hand its grace, and hands back everything it has not handled:
  it NAKs those messages with a 2 s delay (`handBackDelay`), as one batch in stream order, and releases the lease marked `unsettled`.
  A standby takes the released lease at its next poll and settles (below):
  it subscribes at once and holds what it receives until every outstanding message is in its hands, then delivers in stream order, so per-session order is kept.
  Then the stopping pod drains its subscription.

- **Hard stop** (SIGKILL, node or network loss): a standby takes the expired lease about 5 to 6 s after the last renewal (the TTL and one poll).
  Its term must settle: it subscribes at once, beside the dead pod's push binding when the server still counts a lost node's connection, and holds every message it receives, keeping it alive with `InProgress`, until every message the durable has outstanding is in its hands.
  It reads the durable every 250 ms (`settlePoll`) and ends the hold once `NumAckPending` is no more than the messages it holds, or at the cap of one ack wait and a second (`settleMargin`), 7 s after it subscribed;
  then it delivers the held batch in stream order and goes on live.
  The messages the dead pod had in hand come back one ack wait after its last `InProgress`, and those the server pushed to a lost node's connection alone before the standby subscribed one ack wait after that push, so all are back within the cap:
  they land in the held batch and a session keeps the order of its events.
  With nothing outstanding the hold ends at its first read and messages reach their sessions about 5 to 6 s after the last renewal;
  otherwise once the dead pod's messages are back, and at the cap, about 12 to 13 s after it, when a message waits out a retry's delay (a failed delivery's 30 s, an unplaceable delivery's 5 s):
  that message stays outstanding until its delay ends and arrives after the hold, as it would have on the dead pod.
  The tests bound it at 15 s.

- **Graceful stop after a hard one.**
  The lease record carries two flags for the next holder.
  `stale_binding` is set by a holder that subscribed beside a push binding it did not wait out (a lost node's lingering connection), carried by every renewal, release and take-over, and cleared by a holder that finds the durable not push-bound.
  `unsettled` is written from the acquiring write of a term that must settle until its hold ends, and a release keeps it, so a pod stopped mid-hold hands the hold on;
  a graceful stop that hands messages back always releases it set, and so does a stop taken while the term's renewals have lapsed, whatever its own flags stood at:
  the lapse dropped what it held without an ack.
  A pod that takes a released record carrying `stale_binding` or `unsettled` settles too, subscribing at once beside any binding rather than waiting it out as a pre-lease build's.
  The cap is one ack wait and a second:
  the messages the stopping pod handed back come back after the hand-back delay, but one that went to a lost node's binding alone, or to no subscriber at all, or that a dead pod before the stopping one still held, comes back one ack wait later.

- **A holder that cannot use the durable.**
  A holder whose reads, creations, policy corrections or subscribes of the durable fail retries every second (`durableBindPoll`) and times the failures from the start of the first since its term began or an attempt last succeeded.
  Once they have lasted 135 s (`durableTakeoverAlarmAfter`) it logs `lease holder cannot deliver from the durable` at ERROR, with the failure as `reason` and `waited_ms`, which the AWS `subscribe-exhausted` alarm matches, and gives the lease up:
  it releases the record, keeping `unsettled` as its term left it, so the next holder settles as this one would have, and a standby takes the record at its next poll.
  The pod goes back to standby and does not take back the record it released for two standby polls (1 s), so every other standby has its chance first;
  a pod that runs alone takes it back after that and tries again, alarming again at the end of each stuck term.
  A listener without a lease has no one to give it to: it keeps retrying and logs the line again every 5 min (`durableTakeoverAlarmEvery`).
  A read, create or policy correction of the durable that the NATS grant refuses is the one failure that gives no lease up: every pod of the machine connects as the same NATS user and would be refused the same way, and an exit, as #4343 had it, would send each pod in turn to the lease, to the refusal and into a crash loop whose backoff delays the recovery by minutes once the grant is fixed.
  The holder keeps the lease and tries again, logging `durable consumer refused by the NATS grant; retrying` at ERROR at every try, with the refused `subject`, the server's text in `error` and `retry_in`, which starts at 1 s and doubles up to 30 s (`durableRefusedRetryCap`);
  its alarm line comes again every 5 min while the refusal lasts, and `/healthz` answers 503 from the first refused try until a try succeeds.

- **A holder whose receive connection is lost.**
  The lease and the durable ride two connections, so a pod can renew a lease it receives nothing on:
  the main client's link freezes or its hub stops answering, nats.go declares the connection stale two to three seconds later (`bus.WithSilentCutDetection`) and reconnects about a second after that, while the lease's own connection renews throughout.
  A holder whose receive signal has been down for `leaseReceiveLost` (3 s) stops renewing and releases the record with the WARN `listener lease released: the receive
  connection is down`, naming `down_ms`; a connection back inside those 3 s costs it nothing, not even a new term.
  The release carries `unsettled` and `stale_binding`, whatever the term's own flags stood at:
  what the durable pushed to the lost connection comes back only at its ack wait, and that connection's subscription can still be bound at the server.
  So the standby takes the released record at its next poll, subscribes at once beside that binding and holds its full 7 s cap.
  The receive loop ends the term as it ends any lost lease - it drops the subscription and leaves what it queued to come back at its ack wait - since nothing can be NAKed over a connection that is down.
  A message published as the connection was cut reaches its session about 14.5 s later at worst:
  up to 3 s before nats.go declares the connection stale (`bus.WithSilentCutDetection`), up to the holder's 1 s renewal round before the lease reads the signal down (`leaseRenewEvery`), the 3 s bound, the standby's 0.5 s poll (`leaseStandbyPoll`) and its 7 s settle hold (`consumerAckWait` and `settleMargin`), plus the standby's subscribe and the delivery itself, inside the 15 s a hard take-over is bounded at.
  The settle hold is the durable's: a session's own messages ride its consumer, which the cut pod's relay still pulls until the term ends, so what waits is a message published between that end and the successor's first pull (the standby's poll and its start).
  No standby takes the lease while its own receive connection is down - not an absent record, not a released one, not an expired one - so the cut pod does not take back what it just released;
  it stands by again, and may take the lease, once the connection is up.
  A holder whose renewals have lapsed still renews while its connection has been down less than those 3 s, and so resumes its own record; at 3 s it releases the record instead.
  A release the bucket does not take keeps the hold:
  nothing was handed over, so the pod goes on as a holder whose bucket calls fail, its renewals lapse, a standby takes its record once it expires, and it tries the release again at its next turn.
  That holds whatever the lost route does to the pod's reconnects.
  A route that drops its packets rather than refusing them leaves every reconnect dial hanging for the connect timeout, with nats.go's connection lock held throughout, so the lease would read the signal only between dials:
  it reads atomics instead ("The delivery lease" above), and the release lands on its own bound.

The lines to read are `listener lease acquired` (with `from`:
`absent`, `released` or `expired`, and `settle` and `stale_binding`), `listener lease term must settle; receiving at
once and holding what arrives until the durable's outstanding messages are in hand` (with `cap`), `durable still push-bound by an earlier lease holder; receiving beside it`, `settle hold ended;
delivering the held messages in stream order` (with `outcome` `settled` or `cap`, `held_ms`, `messages` and `ack_pending`), `durable receiving` (with `lease`, `waited_ms` and `settle`), `handed the durable's messages back to the next holder` (with `handed_back`, `renaked_repeats` and `dropped_repeats`), `cancelling the message in hand after the shutdown grace` (with `grace`), `the message
in hand was still being handled after its cancel`, `durable consumer read failed; retrying` and `durable subscribe failed;
retrying` (with `error`), `durable consumer refused by the NATS grant; retrying` (ERROR, with `subject`, `error` and `retry_in`), `lease holder cannot deliver from the durable` (ERROR, with `reason` and `waited_ms`), `giving the listener lease up: this pod cannot deliver from the durable`, `listener
lease lost: another listener holds it`, `listener lease released: the receive connection is down` (WARN, with `down_ms` and the `revision` it released) and `listener lease released`.

### Graceful stop order

On SIGTERM or SIGINT, once `/v1` serves (`shutdownListener`):

1. the self-health monitor stops;

2. receiving stops, inside a 10 s budget the HTTP stop shares:
   the holder-only loops are told to stop, the handler takes no more messages, the message in hand gets 3 s (`inflightGrace`) before its handling is cancelled (a cancelled handling is a retryable failure:
   the message is NAKed) and a second more (`inflightCancelWait`) for its handler to return, every message not handled (queued, or held by a settle hold) is NAKed with the 2 s hand-back delay in stream order, the lease is marked released and `unsettled`, and the subscription drains (2 s at most):
   9 s at most with the flush (1 s) and the release (2 s), besides a lease call that hangs.
   The session relays are not in that sum: the term's end only cancels them.
   The budget bounds none of those waits: it is how long the shutdown waits for the receive stop.
   Past it the shutdown goes on to the relays and the HTTP stop with no time left, then to the NATS drains and `os.Exit(1)`, while the hand-back may still be running.
   The listener's own shutdown, not the runtime's kill, cuts that hand-back off:
   `client.Drain` ends the subscription of a hand-back still draining.
   A listener without a lease drains its subscription and NAKs what it has not handled at once, in stream order;

3. the session relays stop, and each returns with a nak what it still holds;

4. the HTTP server shuts down within what is left of the budget;

5. the role lane drains (5 s), the cache watchers stop, and the NATS client drains (13 s).

The process exits 1.
A runtime must allow about 28 s after SIGTERM; give it 30 s.
The hub's listener pod gives it 40 s, the 5 s pre-stop wait included (`terminationGracePeriodSeconds` in `infra/gcp/testdata/clustered-hub-listener-pod.golden`).
A lease left unreleased (the budget ran out) is taken by a standby once it expires, as after a hard stop.

## Start: what ends it and what it waits out

From `main` until `/v1` serves and the lease starts, each failure of the start falls in one of three groups, by what decides it (`main.go`).
This section is the one statement of that rule; the code and the other guides point here.

- **The listener's own configuration ends the start.**
  An error that the listener's settings alone decide ends the process, exit 1, with one line that names it:
  no retry gets past it, and a crash loop that carries its name is the signal an operator needs.
  These are `config.Load`'s refusals, the instance name (`listenerInstance`), a missing, malformed or out-of-range setting (the API auth, OIDC, Google, broker-token and sender-limit settings, the webhook settings, the session windows and the periods), the listening socket, an origin `ciphertoken.New` refuses, and the NATS credential: a seed the connect cannot read, or a credential the server refuses.
  A credential the server refuses ends the start only once the start has waited it out: a hub's servers refuse a new nkey until each has reloaded a configuration that names it, so each connect of the start, the main client's (`connectListener`), the session consumers', an edge listener's relay sockets' and the role lane's (`openStartConnection`), tries again after 1 s, each wait doubling up to 10 s, for up to 180 s counted from the start's first try (`listenerCredentialWait`, `bus.WithCredentialWait`).
  An edge listener reads the stream in place of the reconcile, inside the same wait: a refusal of that read, an authorization one or the grant's, is waited out the same way.
  An authorization refusal of the read that outlasts the wait ends the start as a refused connect does, the read wrapping it beside its own error (`startCredentialRefused`).
  Every other failure of the read is the start rule's below, since the grant, another program or NATS lifts it: a grant that still refuses the read once the wait is over, a stream the server does not hold, and a read that did not answer (`edgeStreamPresent`, `connectListener`).
  Each refusal is logged at WARN with the wait that follows it (`envoy nats credential refused at start; trying again`), `/healthz` answers `503` `unhealthy` with the error `nats credential refused: <refusal>` meanwhile, and `/livez` `204`; from the moment a connect gets in, `/healthz` answers as before.
  A refusal that outlasts the 180 s ends the process, exit 1, its last line naming the credential (`nkey user <public key>`) and the refusal: `connect to NATS and reconcile the ENVOY_NOTIFICATIONS stream: ...; the NATS credential: ...` for the main client (`startFailureLine`), and the step's ERROR with `nats_credential` for a later connect (`natsStart.stepFailed`).
  So does a connect begun once the 180 s have passed, which waits no refusal out, when the last of its ten dials, a second apart, is refused: its last line names the credential the same way (`connectListener` and `openStartConnection` record the refusal).
  A connect's last try decides: one that got past a refusal and then failed ten dials in a row for another reason ends with the last of those errors, naming the earlier refusal beside it, and the start judges it by that last error (`bus.FinalTryRefused`), so a later connect's connection the server refuses for its connection limit is tried again, below.
  The main connect judges its last try the same way (`bus.LastTryError`): an edge listener whose read of the stream the grant refused, and whose later dials then reach a server that takes no nkey, ends the start on that server's error, naming the refusal beside it, and does not wait for the grant as if the read were still refused.
  A grant that refuses the stream's reconcile after the connect got in is no refusal of the credential, and neither is a grant that still refuses an edge listener's read once the 180 s have passed: each is the grant's, below.
  A restarted pod reads its credential again, so a fixed seed is taken at its next start.
  A durable that carries an idle heartbeat or ack policy NATS cannot change in place ends it too (`subscribe refused, shutting down`, "Recreating the durable" below): only an operator who recreates it gets past it.
  The NATS connect ends it too on a refusal that a setting of the listener answers: a server that takes no nkey (`nats: nkeys not supported by the server`), and a stream whose byte limit or copies the account has no room or tier for, `ENVOY_STREAM_MAX_BYTES` above the account's limit (JetStream error code 10122) or unset where the account requires one (10113), and no tiered limit for the copies `ENVOY_NATS_REPLICAS` asks (10120).

- **An answer from the NATS cluster's state, or a refusal only the grant, the account, the server or another program can lift, keeps the pod up, and the start tries again.**
  A server answers some requests from its cluster's state and not from the request, even where the answer reads like a refused setting:
  no member can place the copies `ENVOY_NATS_REPLICAS` asks for (`no suitable peers for placement`, JetStream error code 10005; `replicas > 1 not supported in non-clustered mode`, 10074), JetStream is temporarily unavailable (the meta group has no leader, `JetStream system temporarily unavailable`, 10008; `insufficient resources`, 10023), or the member asked leads no longer (10009).
  A NATS rolling restart and a cluster that is still forming answer so, and the same start succeeds once the members are back, with no setting changed; the start reads the code, never the text (`bus.ClusterStateAnswer`).
  The start's opens of the stream (the NATS connect, `connectListener`), the spent-proof bucket (`openSpentProofBucket`), the store buckets (`openStartStore`) and the listener state bucket (`openDeliveryLease`) each log `start refused by the NATS cluster's state; retrying` at every such try, with what was refused (`stream`, `bucket` or `buckets`), the `setting` it asked (`ENVOY_NATS_REPLICAS=<n>`), the answer's `code`, the server's text in `error` and `retry_in`, and try again after a wait that starts at 1 s and doubles up to 30 s (`logClusterState`, `nextRefusedWait`).
  The line is an ERROR from the first try, except for 10008 and 10023, what a routine JetStream leader change answers until its election ends: those are a WARN until the step's first of them came 30 s before, and an ERROR at each try after (`clusterUnavailableWarnFor`).
  A NATS grant that refuses the stream's read or create at the reconcile, or an edge listener's read of it once the 180 s credential wait is over (`notification stream refused by the NATS grant; retrying`, with the `stream` and the `grant` it lacks, the edge's own on an edge listener), the durable (`durable consumer refused by the NATS grant; retrying`, "A holder that cannot use the durable" above), the spent-proof bucket (`spent-proof bucket refused by the NATS grant; retrying`), a store bucket (`KV bucket refused by the NATS grant; retrying`, with the `bucket` and the `grant` it lacks), the role lane in both its queue groups (`role lane refused by the NATS grant; retrying`) or the session relay's reply subscription (`session relay reply subscription refused by the NATS grant; retrying`) keeps the pod up and backs off the same way, each try logging its line at ERROR with the refused `subject`, the server's text in `error` and `retry_in`:
  every pod of the machine connects as the same NATS user and would be refused alike, and an exit would only send each pod into a crash loop whose backoff delays the recovery by minutes once the grant is fixed.
  A nats-server release the listener refuses (`natsversion`'s floor, asked as the session consumers' connection opens) is waited out the same way, `nats-server release below the listener's floor; retrying` at ERROR at each try with the server's `release`, the `floor` the listener needs and `retry_in`: the session consumers open before the role lane, so meanwhile no role message is forwarded and no frame relayed, no durable is received, and `/v1` answers 503 `service starting`.
  A server that refuses the role lane's or the session consumers' connection itself, as an account at its connection limit does, is named and backed off the same way (`role lane connection failed; retrying`, `session consumer connection failed; retrying` at ERROR): those connections take the settings the NATS connect already took, so no failure of theirs is the listener's configuration's (`openStartConnection`).
  The NATS connect itself that a connection limit refuses, the server's own or the account's (`nats: maximum connections exceeded`, as the server's own limit answers the connect's handshake, or `nats: server maximum connections exceeded` on a connection already connected; `nats: maximum account active connections exceeded`), is backed off the same way too, `NATS connect refused by a connection limit; retrying` at ERROR at each try (`connectListener`, `connectionLimitRefusal`): the limit is the account's or the server's state, which other connections change, and every pod of the machine connects as one NATS user.
  An account at one of its JetStream limits is backed off the same way at the stream's create or update (`notification stream refused by the NATS account's limits or the server's state; retrying`, with the `stream`), the spent-proof bucket (`spent-proof bucket refused by the NATS account's limits or the server's state; retrying`, with the `bucket`) and a store's buckets (`KV bucket refused by the NATS account's limits or the server's state; retrying`, with the `buckets`), each try logging its line at ERROR with the answer's `code`, the server's text in `error` and `retry_in` (`bus.AccountStateAnswer`, `waitOutAccountState`).
  Those limits are the account's resources (JetStream error code 10002), its consumers (10026, which the stream's compiled consumer cap of 24000 meets in an account whose `max_consumers` is lower), its streams (10027), its memory or storage (10028, 10047), and, at a bucket, whose byte limit is the listener's code and no setting, an account that requires every stream to set one (10113).
  Any other answer whose HTTP status is 500 or more, the server failing a request it took, is backed off the same way, a 503 that the third group below reads first waited out there instead, but for a configuration the server finds invalid (10052, which an `ENVOY_SESSION_MAX_AGE` below the session bucket's two-minute duplicate window draws), which ends the start.
  An operator who raises the account's limit or frees what it counts, or the server's recovery, lifts each with no change to the listener, and every pod of the machine shares the account.
  An edge listener whose read finds no stream on the server is backed off the same way, `notification stream not held by the NATS server, and an edge listener may not create it; retrying until a shared listener or Dispatch creates it` at ERROR at each try with the `stream`, the read's `error` and `retry_in` (`connectListener`, `errEdgeStreamMissing`): a shared listener or Dispatch, whose grant may create the stream, lifts it.

- **A NATS that does not answer keeps the pod up, and the start waits for it.**
  A dial no server answers, a server that ends the handshake because the connect's CONNECT came after its `auth_timeout` (`nats: Authentication Timeout`), a request no server answers, an edge listener's read of the stream that drew no answer, a connection that closed under a step (the stream's ensure finding it closed as it reads the server's version among them), and a step the start's hang guard ends (36 s for one open of the stream or of a bucket, `bus.EnsureKeyValue`) are tried again every 2 s (`startRetry`, `natsConnectionError`) with a WARN, for as long as NATS takes:
  `NATS connect failed; retrying`, `<store> open failed; retrying`, `spent-proof bucket open failed; retrying`, `listener state bucket open failed; retrying`, and, once the ten dials of the session consumers' or the role lane's connection found no server, `session consumer connection failed; retrying` and `role lane connection failed; retrying`.

Meanwhile the pod is live (`/livez` 204) and not ready (`/healthz` `503 starting`, or `503 unhealthy` naming the refusal while a connect waits out a refused credential), `/v1` stays closed, and a SIGTERM or SIGINT ends it at once, since the start installs no handler for either until `/v1` serves.

The listener state bucket's grant refusal ends nothing either: the pod delivers without a lease ("Without the grant: no-lease mode" below).

## Readiness: `/healthz`

`/healthz` takes no credential and answers within 5 s (`healthzDeadline`): a check that has not answered by then makes the answer `503 {"status":"unhealthy"}`.

- `503 {"status":"starting"}`:
  Until the pod can serve: NATS connected, with broker access tokens configured the spent-proof bucket open, the webhook and `/v1` gates open, the receive loop started, and the interest copy's first read of its bucket since the process started complete (`interestCopyRead`).
  A NATS that does not answer keeps the pod here, up and retrying, and so do an answer from its cluster's state and a grant that refuses the spent-proof bucket ("Start" above); it never exits for any of them.
  An identity token issuer it has not read yet keeps it nowhere: the pod becomes ready, reads the issuer in the background, and answers that issuer's tokens `503` with `Retry-After: 1` and the code `identity_unchecked` until it has read it (`refuseIdentityToken`), a check made in `apiAuth` before any route runs.
  Every answer, `starting` included, carries `oidc` and `google`: `false` until the listener has read that issuer, `true` after, `null` for a pair not configured; they never decide the status (`issuer_health.go`).
  The start waits at most 30 s for the interest watcher and opens `/v1` when that wait runs out too, or once the watcher's first scan timed out, so `/v1` serves the routes that do not need the interest copy while `GET /v1/interests/` still answers `503` `refilling`:
  the pod stays here until that first read completes.
  Only the first read gates readiness.
  Once the pod has been ready, a later read of the copy (after a reconnect, a scan that timed out, or a copy past its stale bound) leaves it ready, and the routes that need the copy answer their own `503` `refilling` meanwhile:
  readiness that followed the copy at all times would turn a NATS blip, which makes every pod of the machine read its buckets again at once, into a Service with no endpoints, and every route would fail, not only the list.

- `200 {"status":"healthy","role":"active",…}`:
  The pod receives the durable.
  The body carries the durable's lag, `num_pending` and `num_ack_pending`.

- `200 {"status":"healthy","role":"standby",…}`:
  The pod is ready, does not deliver, and another pod of the machine holds the lease: `active_instance` names it.

- `200 {"status":"degraded",…}`:
  A transient KV failure the monitor and the NATS reconnect retry, or, on the active pod, a durable whose lag could not be read.
  Also a pod that is the machine's deliverer and does not receive yet - it holds the lease, or runs without one, or has read no holder at all, or the record still names this pod while it no longer holds it (its renewals lapsed, and no other pod has taken the record yet):
  nothing of the durable reaches a session until it receives, so no such answer is healthy.
  `role` is `standby` and `receive` says what it waits for (`preparing the durable`, or `waiting for another subscriber's push binding to end`);
  when its reads, creations, corrections or subscribes of the durable fail, `error` is `durable consumer unavailable: <failure>` and `receive_failing_ms` how long it has failed.
  It stays `200`, as a standby does, so a readiness probe keeps it in service.

- `503 {"status":"unhealthy",…}`:
  A NATS flush failed (`nats unavailable`), the role lane's subscription is not valid (`role lane subscription inactive`), a cache watcher stopped;
  on the active pod also its subscription is not valid, the durable is gone, or the durable delivers to another subject than its subscription's (`not bound to this listener's subscription`, which a durable deleted and created again under it leaves).
  An active pod answers `subscription inactive` for `leaseReceiveLost` (3 s) at most:
  the lease reads the same signal and releases the record, after which the pod answers as a standby ("Take-over" above).
  Also the failing durable above once it has failed for 135 s:
  a lease holder gives the lease up then and answers as a standby, so only a listener without a lease, which keeps retrying, stays unready.
  And the durable the NATS grant refuses, from the first refused try until a try succeeds: the holder keeps the lease and stays unready, and `/livez` answers `204` throughout, so nothing restarts it ("A holder that cannot use the durable" above).

Every healthy answer also carries the body fields it always did (`ci_cache`, the session registry's fields).
A standby is ready: route traffic to it.
A pod answers `503 starting` only until it can serve, its interest list included, so a readiness probe that sends traffic on `200` sends none too early; a ready pod whose interest copy reads its bucket again stays ready (above).

A probe contract that fits these answers:

- readiness: an exec of `curl -fsS http://127.0.0.1:<port>/healthz`, `timeoutSeconds` 6 or more, above the handler's 5 s deadline;

- liveness: the same request without `-f`, with `--max-time 8`, so a `503` (starting, or a dependency down) is no liveness failure and only a process that does not answer is restarted;

- `terminationGracePeriodSeconds` 30 or more, for the graceful stop above.

## State the pods share

Every pod of a machine id reads and writes the same records, so a pod that takes over, or a standby answering `/v1`, answers as the active pod would:

- **When a session can see a message twice.**
  The listener delivers a stored message to a session through that session's own consumer, relayed as a frame, and the consumer's ack floor is what says the session has it.
  Every restart of a session's consumer (`bus.SessionConsumers.RestartAtFloor`) sends again every frame above the consumer's ack floor.
  That includes a frame the session answered `done` out of order above one it is still holding:
  the server cannot move the floor past a frame still unacked, so the frames acked after it are above the floor and come again.
  A consumer with a message that awaits an ack is deleted and created again at the sequence after its ack floor.
  A consumer with nothing that awaits an ack has no frame above its floor, so the restart updates it in place (`keepAtFloor`):
  the sequence it examines next is already the one after its floor, and the restart sends no delete and no create.
  The exception is a consumer whose next sequence is past the stream's end, which the restart deletes and creates again too.
  Every repeat carries the message's own event id, and the client's dedupe writes it once
  — it joins a copy of a frame it still holds, and drops one it has already written (`packages/envoy-client`'s consumer delivery).

  A relay restarts when:

  - a receive term ends and the pod that takes over starts the session's relay:
    every frame left above the consumer's ack floor is sent again, as above;

  - its session-consumer link drops and comes back.
    The frame's reply and the consumer's acknowledgement ride that one link, so a frame answered under the drop was recorded nowhere, and the relay restarts at the ack floor as soon as the link is back;

  - a pod is killed or its node is lost: nothing settled the frames it had out, and the pod that takes the lease over restarts the consumer at the floor;

  - a frame drew no reply inside the relay's receipt wait, so the relay pauses and probes again after 2, 4, 8, 16 and then every 30 s.
    This one can therefore repeat a frame more than once;

  - a pull, a publish or a restart failed and the run retries;

  - the session registers again under a new epoch;

  - the session leaves the set this pod relays and rejoins it.

- **Role-claim grace** counts from the machine's service start, `service_since` in the lease record:
  the server's time of the write that took the lease when no pod of the machine was serving (no record, or one last written more than two TTLs before).
  A claim stored before `service_since` keeps its grace for one session TTL after it; every pod answers a claim alike, whenever it started.

- **Registry writes** are compare-and-set at the revision the pod read, a write over a key that held no entry included, and what each answers once it loses that race differs:

  - A session registration and an interest are merged again over the bucket's value on a conflict, and a role claim's release or hand-back weighs the claim again at the revision it now stands at.
    Each answers `503` with `Retry-After: 1` once its attempts are spent:
    32 for a session registration (`entryWriteLosses`, `internal/session/registry.go`, the bound every write of a session's entry shares), 3 for an interest (`interestWriteAttempts`, `internal/store/interest_writes.go`) and 3 for a release or hand-back (`roleClaimReleaseAttempts`, `internal/store/role_release.go`).

  - A role claim's renewal (`renewRoleClaim`, `internal/store/role_renewal.go`) makes one attempt.
    A renewal that a claim landed under is dropped and logged, the registration that sent it still answers `200`, and the next registration renews the claim that stands.

  - A role claim writes at the revision of the entry it read: the claim's own, the delete marker of the release that ended the last claim, or none for a key that never held one (`roleClaimToWrite`, `internal/store/role_claim.go`), never through a create, so an abandoned write that lands late lands over nothing written since its read.
    A hard claim with no `if_claim_revision` reads again and writes again on a conflict until its request's budget ends, so the last write still wins; one that met a conflict at every try answers `503` with `Retry-After: 1` (`ErrRoleClaimContended`) and writes nothing, its interest topic rolled back unless the claim that stands names its session (`rollBackUnlessHeld`).
    A soft claim, or one that carries `if_claim_revision`, makes one try, and one that a claim landed under is refused with `409` (`ErrRoleHeld`, `ErrRoleClaimRevisionMismatch`);
    `/v1/roles/set` decides a soft claim that named no revision once more first, on the claim that replaced the one it read.

  - A takeover record (`bus.TakeoverSubject`, `cmd/listener/role_takeover.go`) is a message of the stream rather than a key, written by a pass only while its subject's newest message is the one its pass read (`bus.Client.WriteTakeoverRecordAt`), and never over a finished record (`bus.ErrTakeoverRecordFinished`); a write that lost that race, or whose record the stream removed meanwhile, stores nothing (`bus.ErrTakeoverRecordMoved`) unless the stream refused a resend after a first send nobody answered, which may have stored the record, and the record's next pass reads it again either way.
    The claim route's first write of a record stores only while the record's subject holds nothing (`bus.Client.WriteTakeoverRecord`), so no send of it lands over a pass's write, a finish included; a resend after a first send nobody answered, refused because the subject holds a message, counts as stored only when the subject's newest record names the same claim (role, previous holder and claim time), and fails the write otherwise.
    The claim route writes a new record and runs its passes on whichever pod served the claim, within its handover window (10 min, `roleKeepHandoverWindow`) and not the request's;
    the kept lane's sweep runs the passes of every unfinished record on the lease holder alone.
    Two pods' passes over one record store each copy once, under its message id (`takenFrameMsgID`), which the stream's 72-hour duplicate window keeps for as long as the frame it copies is stored.

  A pod whose cache has not seen a registration reads the bucket before `/v1/messages/send` or `/v1/roles/set` answers that the session is unknown.

Some records are each pod's own:
the delivery caches of what the pod delivered and attempted (`listenerDeliveryHandlerConfig`'s `dedupeCache` and `attemptCache`, `internal/dedupe`'s in-process caches), so a pod that takes over can deliver again, under its own event id, a message the pod before it delivered;
and, deliberately, the **publish route's confirmation record** (`listenerDeps.confirmed`, `internal/dedupe`'s in-process cache), which holds each (dedupe key, session) the stream stored a forward of through `/v1/messages/publish` on this pod, with the stream sequence of that stored copy, and each such forward under the request that made it (`forwardRequest`: the source, the source session, the role topic and the dedupe key).
Only a forward the route itself made counts.
So a repeat of a role send to the same holder is answered from this record's sequence (`dedupe.Cache.Stored`), and an echo, the repeat of a sender that has since taken the role, from the forward recorded under its request (`dedupe.Cache.Forward`, `roleSend.settledBeforeForward`).
No answer reads another pod's record.
A send retried against another pod of the machine forwards again, and the stream keeps one copy of that forward under its `Nats-Msg-Id` (`bus.ForwardMsgID`), so that pod answers the duplicate from the copy the stream holds.
An echo that reaches the other pod, after a take-over say, finds no forward recorded there under its request:
it forwards nothing, as an echo never does, and is answered 200 with its own envelope, whose event id no stored message carries, so it gives no session a second copy.
The control of `TestARoleRepeatFromTheSenderThatTookTheRoleAnswersTheStoredForward`, a holder's first publish to its own role, shows that answer.

## Two builds of the listener

During a roll two builds run for one machine id, and each must deliver what the other expects:

- **The durable takes every stored message.**
  Its filter is `>` (`consumerFilterAll`), not a build's own list of stream subjects, so a build whose stream carries a subject the other does not know still has it delivered.
  It is `>` and not empty because nats-server 2.10 accepts an update that clears a push durable's filter and then delivers nothing published after it.
  A holder that finds the durable push-bound writes the policy only once that binding has ended, unless its term must settle: such a term writes it beside the binding, which it counts as an earlier holder's ("Take-over" above).

- **A session's consumer takes what its filter captures.**
  A relay's restart writes the filter from the session's followed topics: each one a consumer can filter on, whatever either build's stream subjects capture (`sessionConsumerFilter`, `bus.CanFilterOn`), in every form a reader takes it in (`bus.SessionConsumerFilters`).
  A consumer keeps its position when its filter changes, so a topic either build knows is delivered from the sequence the session had reached.
  Once the newer build's start adds a subject that captures a topic in the filter, the consumer delivers what the stream stores on it.
  A topic left out until the next registration would lose every message the stream stored on it in between, because the consumer moves past a message its filter does not match.

- **A publish on a topic only the newer build captures is stored.**
  The publish route refuses such a topic only on a read of the stream's subjects that started after the request asked (`bus.SubjectsAsk.Uncaptured`), and not when no such read can be made, so the older pod stores such a publish too.
  The one exception is a pod whose NATS user may not read the stream: it refuses on the subjects it holds.

- **Fields a build does not know are kept.**
  Session registrations, interests, role claims and the lease record keep every JSON field they carry across every read-modify-write, and a known-session record (`session.KnownSession`) across every rewrite this listener makes of its copy of it.

A build from before INFRA-557 binds the durable with `nats.Bind` and knows no lease.
Run one beside this build only during a roll from the old build to this one:
this build's lease holder, finding the durable push-bound when it took an absent record or a released one carrying neither `stale_binding` nor `unsettled`, waits (`/healthz` 200 standby, `receive` `waiting for another
subscriber's push binding to end`) and subscribes within a second of that binding's end.
Past 135 s it logs `lease holder cannot deliver from the durable` at ERROR, and again every 5 min, which the AWS `subscribe-exhausted` alarm matches; it never exits for it.

An unreleased record stays in the bucket for the bucket's `MaxAge`, 20 minutes, where an earlier build of this release kept it 10:
after every pod of this build died hard, a pod starting within 20 minutes finds that record, takes it as an expired one and counts a push binding it finds as the dead holder's rather than a pre-lease build's - the window in which that reading holds is now twice as long.
A pod starting later finds no record and waits such a binding out as a pre-lease build's, as it always has.

Rolling back to a build before this release works: that build corrects the durable's filter to its own list and binds once this build's pods have stopped.
But a hard stop of this build's lease holder while a build from before the release is up and polling to bind can deliver messages twice:
the old build binds as soon as the dead holder's connection is gone, and the standby that takes the expired lease counts that binding as the dead holder's and receives beside it.
A rollback that stops every pod of this build before starting the old one carries no such risk.

## Without the grant: no-lease mode

The listener's NATS user needs the `envoy_listener` bucket in its grant (`infra/core` `ListenerPermissions`, `DispatchPermissions` and `HostPermissions`, and the NATS auth service's answer to a listener or Dispatch connection, carry it from this release).
That grant names only the machine's own keys, `lease.listener-<machine id>`, `handback.listener-<machine id>.*` and `delivery.listener-<machine id>.*.*` (each `listener-<machine id>` in the key spelling "The delivery lease" gives): it writes each of them and reads each by key, and its other calls on the bucket are the bucket stream's info, create and update, which the open and the raises send (`natsgrant.ListenerState` names the call behind each subject);
that is a subject's reach, and what every listener user of the fleet can still do to the whole bucket is in `packages/envoy/infra/README.md` ("Permission sets").
The grant reads each of the listener's other buckets by key too, a direct get on the key's own subject, and on every bucket it refuses a get by revision, a direct get on the stream's bare subject.
So the listener reads back each write it must stamp, the lease's acquiring write and a session registration in `envoy_sessions`, by key, and counts it as its own only at the revision it wrote;
another revision there means a newer write landed first.
A listener whose grant refuses `envoy_listener` does not exit: it logs one ERROR, `listener lease unavailable: the NATS user lacks the grant for KV bucket envoy_listener; delivering without a lease`, and delivers as the machine's one deliverer, receiving from the durable only while no other subscriber holds it push-bound and waiting for as long as one does.
`/healthz` says so in `lease`: `"lease":"unavailable: missing NATS grant for KV bucket envoy_listener"`.
The shared state above falls back to the pod's own: the role-claim grace counts from the pod's own start.
Run one pod per machine id in this mode: it has no fencing against a second deliverer after a reconnect in place.

Deploy prerequisite: render and apply the NATS grant carrying `envoy_listener` (for a `gcp:NatsHub`, its users;
for a host, its NATS authorization) before running a second pod of a machine id.

An edge listener (`ENVOY_EDGE_PROJECTS` set) always runs in this mode.
Its grant (`natsgrant.Edge`) holds no call of `envoy_listener`, so it opens no lease connection, logs one INFO, `delivery lease not opened: an edge listener's grant holds no listener state bucket; delivering without a lease`, and never tries the bucket again (`main.go`).
`/healthz` says `"lease":"unavailable: missing NATS grant for KV bucket envoy_listener"` as above.
Run one edge pod per machine id, since a second one has no lease to stand by under:
a second edge pod for one machine id, a rolling update's surge pod included, can deliver a message twice, as an edge listener of a build before this release can.
So an edge Deployment runs one replica under the `Recreate` strategy.
The relays of an edge listener acknowledge a frame they withhold (a GitHub event while no fresh repository answer stands, or a frame of no project the session's token lists, `edge_frames.go`), or give it back (`acknowledgeWithheld`, `giveBackWithheld`, `session_relay.go`), because an edge listener holds no lease and runs one pod:
an edge standby would need that withhold fenced by the lease, with the change to the edge's grant that its owner approves.
An edge listener serves its sessions' relay sockets (`relay_socket.go`) on a NATS connection of their own, and opens that connection by the start rule, as it opens the session consumers' (`openStartConnection`, "Start: what ends it and what it waits out" above).
A server that refuses that connection, or dials that find no server, log `relay socket connection failed; retrying` (and a grant refusal `relay socket connection refused by the NATS grant; retrying`) and the start waits, since an exit would only send the pod into a crash loop.
At a stop the relay sockets close, each with 1012, after the session relays stop (`shutdownListener`), and before the HTTP server shuts down, which does not wait for a hijacked connection.

## Environment

The settings every listener reads are listed on the docs site's Envoy page ("Running the listener").
These concern the pods of one machine id:

- `ENVOY_MACHINE_ID`:
  The machine id.
  Every pod with the same id shares one durable, one lease and one queue group; two machines whose ids collide silently become each other's standby.

- `ENVOY_INSTANCE_ID`:
  The pod's instance name: the lease record's `holder`, `/healthz`'s `active_instance`, the `instance` field of every JSON log line, and the NATS client names (`envoy-listener <instance>`, `envoy-listener-lease <instance>`).
  Default: the host name, the pod's name in Kubernetes.
  Go's text log lines carry no instance prefix, since the deployed text metric filters match by position.

- `ENVOY_NATS_REPLICAS`:
  Replicas of every bucket the listener creates, `envoy_listener` included.

## Recreating the durable

Recreating `listener-<machine id>` (to drop a setting NATS cannot change in place, as `subscribe
refused, shutting down` names) keeps its cursor only while no pod of the machine runs: stop every pod of the machine id first, active and standby.
A lease holder that finds the durable missing creates it at deliver policy `all`, which replays every retained message, and a standby takes the lease and does the same when the active pod stops.
Then recreate it as `packages/envoy/deploy/README.md` ("Recreating a listener's durable") shows and start the pods;
the lease holder stamps the rest of the policy onto it and subscribes.

## Tests

The two-process and two-build tests start real listener binaries against one in-process NATS server, each behind a relay that can cut, freeze or delay its link.
They carry the build tag `multiprocess`, and CI runs them in the `listener-multiprocess` job of `.github/workflows/ci.yaml`, whose steps are in `.github/workflows/ci-job-broker-multiprocess.yaml`.
The job runs only when a file its `listener_multiprocess` filter selects changes, and on a pull request only when the pull request also carries the label `full-ci` or `lane:listener_multiprocess`.
No file of the repository holds that filter: ci.yaml's `changes` job generates it at each run with `.github/scripts/check-ci-filters.sh generate` (through `.github/scripts/ci-filters-generate.sh`), from what `.github/ci-gates/listener-multiprocess.py` declares the job reads.
The generation lists the packages the job's go commands compile (`go list`), so a new import needs no edit; a test that comes to read a file outside its package through `..`, or to start a go command, adds a `//ci:` line for it at the top of its test file, which the generation reads too.
The job's test step runs this command:

```bash
cd packages/envoy
go test -tags multiprocess -count=1 -timeout 20m -v -run '^Test(TwoPods|TwoBuilds)' ./cmd/listener
```

`TestTwoBuildsTheFirstRollFromTheReleasedBuildSkipsOrDoublesNoMessage` skips if `ENVOY_LISTENER_OLD_BINARY` is unset or empty.
To run it, set that variable to the path of a listener built from the commit behind production's dispatch-listener image.
CI's `listener-multiprocess` step does not set the variable and skips the row.
Check `c-oldbinary` in the change's Verification records the run with the released build.

The lease and receive rows that need no second process (`lease_test.go` and the `lease_*_test.go` files beside it, `receive_test.go` and the `receive_*_test.go` files beside it) run with the package's other tests.
