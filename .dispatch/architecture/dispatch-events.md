---
title: Event broker and outbound Envoy client
parent: dispatch-server
depends_on: [dispatch-model, dispatch-store]
paths: [packages/envoy/internal/dispatch/events, packages/envoy/internal/dispatch/envoy]
---
The append-only event log every Dispatch write lands in.
An append (`Broker.QueueAppend`) puts its statements on the write phase's batch (`store.WriteBatch`):
the event owner's advisory lock (`store.LockOwner`, which returns at once when the transaction took it in its read phase), then the owner's row `for no key update`, which takes the transaction id (`pg_current_xact_id`) in the same statement and is the write-phase statement that gives a transaction appending its first event its transaction id before the insert takes an id, then one insert that takes the next sequence from the owner row and, in the same statement, advances the row's `last_seq` (and an issue's `updated_at`) to the row it inserted.
It takes no other lock (an ownerless, session-addressed event takes neither, only its transaction id, in a statement of its own queued before the insert), so one owner's events commit in id order while two owners' writes never wait for each other and can commit out of id order.
An `ask.opened` payload records its own event id through the events id sequence's `currval` (`store.LatestEventID`), the same expression a stamp of the source's references uses right after its event.
So nothing is read back in Go between the statements, and the whole append goes to Postgres with the rest of the write phase in one round trip;
the appended event (id, sequence, time, project) reaches the caller through a callback once the batch has run.
`Broker.Append` is the same statements sent at once as a batch of their own.
`LockOwners` takes the locks of several owners up front, sorted and deduplicated by lock key (`store.LockOwners`), so two transactions never take shared owners in opposite orders.
An append a document ledger queues runs in the transaction's write phase, in the order the transaction made it.
The broker writes through the transaction its caller hands it and imports `dispatch-store` for the owner lock and the write batch.
`internal/dispatch/envoy` is the small outbound client that calls the Envoy listener's API;
it exists only to carry broker-produced events onward, so the two are one component.

`Stream[T]` is the in-process fan-out: `Subscribe(buffer)` gives each subscriber a channel of the size it chooses, and a subscriber whose channel is full is dropped and its channel closed rather than waited for.
`Broker` fans this process's committed events out to the outbox through one, and the API's event stream relay publishes every committed event, with its encoded frame, through another.
The relay's is the process's one reader of the event log for live readers:
every events subscription on a page's live socket and the Delivery page's hub take it, so the hub reads no event log of its own.

Since a lower id can still be open after a higher one has committed, every reader that pages the log by a global id cursor (the event stream relay, an events subscription's bounded replay and its catch-up after an overflow, `GET /api/v1/events/log`, `GET /api/v1/references?since=`) reads only up to the safe horizon of the server's one `events.Horizon`, and delivers in id order.
An observation is one statement in a transaction of its own that reads A, the newest id its snapshot sees, that snapshot's xmin, its xmax and whether its in-progress list is empty, and X, its own transaction id, assigned after that snapshot.
A later observation whose snapshot xmin is above X proves that no transaction below X still runs, so every id at or below A is final;
the horizon is the A of the newest observation proven so.
The proof rests on two writer invariants:
an append takes its transaction id (`pg_current_xact_id`) before the insert draws the event id, and `events_id_seq` keeps `CACHE 1`, so a transaction holding an id at or below A holds a transaction id below X.
An observation's own snapshot proves it when the snapshot lists nothing in progress and its xmax is X:
every id below that xmax had ended when the snapshot was taken, and xmax = X leaves no id between, so the horizon reaches A in that one observation.
Both conditions are needed: a transaction whose id is at or above the snapshot's xmax is not listed in it as in progress, and the observer's X is then above that xmax.

Snapshots are instance-wide, so a transaction that holds a transaction id anywhere on the Postgres instance delays events to live readers until it ends, and it never blocks a writer.
An event waits for every transaction holding an id when the first observation that sees the event takes its own transaction id, which is after the event's commit (the relay reads 2 ms after the event's notification, the window that collapses one commit's notifications into one read):
so it also waits for transactions that began after it committed, and for id holders that write no event, such as a `SELECT ... FOR UPDATE` or another database's job on the instance.
A read whose own observation proved itself publishes in that same read (`Horizon.Gap` reports `GapNone`), so a read taken while nothing on the instance holds a transaction id costs one observation, not two;
otherwise a reader (the relay, `Horizon.Await`) whose read leaves only its own observation unproven (`GapUnproven`) observes again at once, which proves it unless an older transaction id still runs;
a gap an older transaction holds (`GapHeld`) is read again on the next commit notification, without that window, or after a fallback of 10 ms doubling to 250 ms for a holder that ends without notifying, which runs only while such a gap is open, so an idle process observes nothing.
A Dispatch write transaction is to hold its id only in its write phase, the statements from its first write to its commit, so its document work and write advice never delay a live event.
Most write routes send that phase as one pipelined batch;
the delivery replies (`POST /api/v1/comments/{id}/reply`, `POST /api/v1/messages/{id}/reply`) and the results write (`POST /api/v1/issues/{key}/results`) send theirs a statement at a time, still with no document work inside it.
Some writers do not yet hold the rule: they make a write or take a row lock before they wait on the owner lock of the event they append, so they hold an id while another writer's document work holds that lock.
dispatch://INFRA-3397 converts them and keeps the list of them;
at commit `9e578bcc` of sjawhar/legion#2468 it was `keepDecision`, `putArchitectureSource`, `deleteArchitectureSource`, `putAskSnooze`, `deleteAskSnooze`, `markInboxReadThreads`, `dismissInboxThreads`, the architecture importer's `Importer.project` and `Importer.recordFailure`, and delivery's `UpsertPullRequestHead`, which locks and upserts the pull request's head row before it locks the issues the head moved between.
A pending observation older than five seconds logs the oldest holder.
A live event stream that resumes from an id opens at once and heartbeats while the horizon is held, a cold one opens once its subscription is in place, and shutdown ends a stream still waiting.
A cold stream starts at the relay's published cursor.
The relay runs only while someone subscribes:
the first subscription of a process, and the first after the last one was cancelled, starts a generation, and each generation reads its first cursor afresh.
That cursor is the log's newest id when the generation starts, or the last event below the lowest gap among the ids that filled while the start waited for every id to be final, so an event that commits while the generation starts still reaches the stream, and it may be sent the few events that committed just before it connected above that gap.
The start reads only the ids written since a mark:
the newest of the marks the process keeps (one at boot, and the recent ones its readers take) that is older than every transaction holding a write lock on events, so its cost follows recent writes, not the size of the log.
A transaction that holds a transaction id but has inserted no event holds no event id, so it keeps no mark from counting, however long it stays open;
only an event insert open since before every mark does, and then the start reads the log above the horizon this process proved or the newest one any process persisted (the one-row event_horizon table, written at most once a second as the horizon advances, with the created_at of the event at it so a value from before a reset of the log is passed over):
on a deploy under load, the horizon the process before it proved.
Only with neither, a first boot or a reset log, does it read the whole log, which it logs.
GET /auth/whoami's event_head, which a page resumes its first feed from, comes from the same marks or those horizons and waits on nothing, except on a process with no mark counting and no horizon proven or persisted, which waits up to 2 s and then answers 503 EVENT_HEAD_UNAVAILABLE rather than an event_head of 0, and the page asks again while it does.
After the event log is reset, the relay forgets the horizon the old log proved and ends the generation with every subscription on it;
a cold stream that finds the relay still on the old log's cursor opens once that reset has run, replayed from the newest id below which the new log has no gap.
The delivery hub is ready and takes delivery changes while the horizon is held, because its relay subscription is taken on a goroutine of its own, and it acts on events once that subscription stands.
Each observation uses one transaction id; concurrent callers share one, and an idle process takes none.
The server observes through a two-connection pool of its own (`store.Pool.Horizon`), so the horizon never waits behind requests for a shared connection.
A failed observation is logged, and a reader waiting on the horizon gets the error back once every observation has failed for ten seconds, so a horizon that cannot be read never stalls a reader silently.
Observations and every read capped at the horizon run on the primary.
