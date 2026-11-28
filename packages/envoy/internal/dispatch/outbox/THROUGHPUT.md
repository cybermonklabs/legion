# Outbox throughput readings

Readings taken 2026-10-26/27 on a shared development machine under load from other test runs.
Treat the numbers as order-of-magnitude figures.

## Method

`TestTheOutboxHoldsManySendsOutstandingUnderRemoteLatency` (`throughput_test.go`):

- The test uses real Postgres and JetStream. The outbox sends through a TCP proxy.
- Each chunk waits for half of the configured 50 ms or 150 ms round trip on the proxy's fake
  clock. The test moves that clock to the next pending proxy deadline.
- The backlog spans 20 issues and fills one batch with another round of events left to drain.
  The historical runs below used two full batches (200 events).
- In the messages load, each event routes to `role:reviewer`. The stream acknowledges each
  forward to its self-subscribed holder. Half the events reply to a session's message, which
  takes an author lookup and sends a third copy. Before #2395, the role routes were core
  publishes to `notifications.role.reviewer`, confirmed by a flush.
- The busy-pool case holds all eight connections of a shared pool through the drain. The outbox
  uses the separate pool derived from it. The historical runs below used concurrent queries
  with `pg_sleep` to fill the shared pool instead.
- The single-issue burst: 100 events on one issue, every one of them bound for the same three
  destinations.
- The test logs the drain time and rate on the proxy clock, not the wall clock. Do not compare
  these numbers with the historical wall-time measurements below.
- A wrapper around each publish window counts unanswered sends (`outstandingSends`). The backlog
  cases hold answers until 20 sends are outstanding. A drain that waits for its answers before it
  issues enough sends cannot open this gate. The single-issue burst has no gate.

## Historical wall-time results

| Design | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy | 50 ms burst | 150 ms burst |
|---|---|---|---|---|---|---|
| Old (`origin/main` at 1357b95c2, one synchronous publish and one write per destination) | 2.7 ev/s (5.3 run alone) | 3.0 ev/s | 1.7 ev/s | 1.9 ev/s | | |
| One window per batch, every send before any answer, one outcome write, outbox pool of its own | 1,100-1,340 ev/s | 1,100-1,230 ev/s | 540-570 ev/s | 480-540 ev/s | | |
| The same one-window design, the #2395 harness commit `bed0fdc4` (`main` at `c3d09eec` plus the burst cases), 2 cores, 2026-10-27 | 749.1 ev/s | 219.0 ev/s | 443.7 ev/s | 357.9 ev/s | 725.9 ev/s (100 in 138 ms) | 251.4 ev/s (100 in 398 ms) |
| Chains: each issue's sends to each destination one at a time, issues and destinations side by side (#2395, two runs on 2026-10-27) | 321-323 ev/s | 169-277 ev/s | 122-124 ev/s | 113-117 ev/s | 18.6 ev/s | 6.5 ev/s |
| Chains, role routes forwarded to the holder with a receipt (#2395, two runs on 2026-10-27) | 196-199 ev/s | 171-188 ev/s | 73-74 ev/s | 71-72 ev/s | 17.3-17.6 ev/s | 6.2 ev/s |
| The same, the #2395 head `891e854b`, same machine as the harness-commit row | 175.5 ev/s | 183.9 ev/s | 71.3 ev/s | 70.5 ev/s | 16.7 ev/s (100 in 5.99 s) | 6.2 ev/s (100 in 16.2 s) |

The results above predate the fake clock and the answer gate. The test checks the backlog's peak
count, and checks the order and destination counts in every case. It asserts no rate.
Chains cost one round trip per event per destination of one issue: a batch of the backlog holds
five events of each issue, so it takes five round trips, and a burst on one issue takes one round
trip per event. That lets each destination take an issue's events in order whatever fails
(`planChains`). With role routes forwarded to the holder and answered by its receipt, the
backlog cases took about 1.6 times as long as with role routes published to the role's topic,
and the burst cases about as long.

## Two real processes at the production step and ten times it

`TestTheOutboxLoadSteps` (`cmd/dispatch/outbox_load_steps_test.go`, build tag `multiprocess`, run
only with `DISPATCH_OUTBOX_LOAD_STEPS=1`) is black box: real `envoy-dispatch` processes on one
Postgres and one NATS (no added latency), comments on 40 issues written through the processes'
HTTP API (alternating between them with two processes), one issue routed to a role whose holder
the stream acknowledges every forward for, and a test session that counts each event once at its first
arrival. The publish rate is distinct events received per second while writing; the latency runs
from the event's commit (`IssuedAt`) to that arrival. The burst writes 100 comments on one issue
through one process. Same file, same 2-core machine, 2026-10-27: "before" at the harness commit
`bed0fdc4` (`main` at `c3d09eec` plus this test), "after" at `891e854b`.

| Run | Processes | Before: published/s | Before: p50 / p99 ms | After: published/s | After: p50 / p99 ms |
|---|---|---|---|---|---|
| 4 events/s for 60 s | 1 | 4.00 | 9.2 / 19.7 | 4.00 | 9.2 / 19.9 |
| 40 events/s for 60 s | 1 | 40.00 | 8.9 / 1,063.3 | 39.99 | 8.4 / 530.4 |
| 4 events/s for 60 s | 2 | 4.00 | 10.8 / 19.7 | 4.00 | 9.8 / 34.7 |
| 40 events/s for 60 s | 2 | 39.99 | 12.4 / 1,271.3 | 39.99 | 10.6 / 1,054.4 |
| Burst of 100 on one issue, drain | 1 | 981 ms (p50 9.7, p99 70.4) | | 863 ms (p50 16.7, p99 29.8) | |
| Burst of 100 on one issue, drain | 2 | 728 ms (p50 10.1, p99 17.4) | | 1,462 ms (p50 17.7, p99 48.7) | |

The tester ran the same harness at `63e05008` (the head after two merges of `main`) on the same
kind of 2-core pod: 4 events/s p50 / p99 8.8 / 40.8 ms (one process) and 9.3 / 35.9 ms (two);
40 events/s 39.99 published/s with p99 1,153.8 ms (one) and 1,106.0 ms (two); the burst drained in
1,729 ms (one) and 732 ms (two). So the 40 events/s p99 moves between about 0.5 and 1.3 s from run
to run on this machine, before and after, and these runs show no change in latency either way;
the 530.4 ms reading above is one run, not an improvement.

Every step delivered every written event, the role holder's included, before and after, in every
run. The burst's drain time includes about one second of writing one comment after another. With
NATS on the same host a chain's round trip is about a millisecond, so ordering per destination
costs the steady steps nothing measurable; the remote-latency rows above are where it costs.

## Comment replies to the claimant and the thread (2026-10-27, sjawhar/legion#2366)

This change, as measured, sent a person's comment on an issue with no route to the session that
claimed the issue. When no session claimed the issue, it sends the comment to every session that
wrote the issue's primary document. It also sends a reply to every session in its thread and to
every session that wrote the thread's document. For the whole batch, it reads the threads'
sessions in one query (`loadCommentRecipients`) and the primary documents' sessions in one query
(`loadPrimaryDocumentAuthors`).

Since the merge with #2225, the claimant's copy comes from main's claimant route (the claim read
in the scan), on a routed issue too; no load here has a claimed routed issue, so each load's stream
count is unchanged and the runs below were not repeated.

On #2395's chains, the two queries are two of the batch's lookup statements (`loadLookups`), the
claimant route is `addClaimantRoute` and the primary document's sessions
`addPrimaryDocumentAuthors`. The three loads run in
`TestTheOutboxHoldsManySendsOutstandingUnderRemoteLatency` beside the burst, which runs on the
`messages` load alone, and their stream counts there include each role forward, which the stream
captures on the holder's agent subject. The rows below were measured before #2395 and were not
repeated on its chains.

Method of these historical runs: each of the three loads has 200 events across 20 issues,
so each batch of 100 has every kind of event in its load.

- `messages` (`seedThroughputBacklog`): the load of the Method section above.
- `comment replies` (`seedCommentReplyBacklog`): half are a person's replies in a comment thread,
  on 10 claimed issues with no route. Each thread is on a passage of the issue's document and
  holds the person's root comment and a reply from the session that wrote the document. The
  other half are messages on 10 issues routed to `role:reviewer`, as in the `messages` load.
- `unclaimed comments` (`seedUnclaimedCommentBacklog`): all are a person's comments on 20 issues
  with no route and no claim. A session wrote the primary document of each issue. Half are
  comments on the issue's conversation. The other half are replies in a thread on a passage of
  the primary document, which holds the person's root comment and a reply from a second session.
  Every batch runs `loadPrimaryDocumentAuthors` and `loadCommentRecipients`.

The trees and the runs:

- Main before #2225: `origin/main` at 1b8d5c13e, whose outbox package was the same as at this
  branch's merge base then (c3d09eec9); its `internal/bus` package had changes that head did not
  have. Head: c2a223fcb, before the merge with #2225. Main ran head's `throughput_test.go` in an
  exported copy of its tree. The rows marked "main" are that tree, not main as it is now.
- Postgres 17, 2 CPUs. Three runs of each tree, main and head in turn, back to back. Each run
  ran all three loads.
- Each cell is the range of the three runs, in events per second.

| Load | Tree | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy | Destinations per event |
|---|---|---|---|---|---|---|
| `messages` | main | 445-693 | 550-610 | 420-437 | 366-386 | 2.5 |
| `messages` | head | 492-764 | 363-693 | 211-436 | 288-345 | 2.5 |
| `comment replies` | main | 391-572 | 337-394 | 324-379 | 315-332 | 1.75 (reply 1, message 2.5) |
| `comment replies` | head | 707-793 | 615-656 | 357-434 | 354-381 | 2.75 (reply 3, message 2.5) |
| `unclaimed comments` | main | 620-630 | 302-559 | 381-391 | 291-363 | 1 (comment 1, reply 1) |
| `unclaimed comments` | head | 676-856 | 604-682 | 438-447 | 305-383 | 2.5 (comment 2, reply 3) |

On main before #2225, a person's comment on an issue with no route reached only its issue topic.
It did not reach the claimant, the primary document's writer or the thread's sessions, so that
tree failed the test's count of stream messages: 250 of 450 for `comment replies` and 200 of 500
for `unclaimed comments`. It passed the 25 ev/s requirement in every case. Main now has #2225's
claimant route, so in the `comment replies` load today's main reaches the claimant too; the
claimant copy is main's, and what this change adds is the thread's sessions and the documents'
writers.

The `messages` load at 150 ms was lower on head in these three runs. Five more runs of that case
alone, main and head in turn, read 395-431 (idle) and 286-387 (busy) ev/s on main and 306-430
(idle) and 297-371 (busy) on head, so the difference is in the spread of the runs.

Judgement: against main before #2225, the change cost no measurable throughput: both comment loads
were as fast or faster on head, although each comment reached two or three destinations in place
of one, and the `messages` load was the same within the spread of the runs. Not re-measured
against main with #2225.

## Order and crash safety

| Test | Old design | One window per batch | Chains |
|---|---|---|---|
| `TestABacklogPublishedAcrossANATSOutageKeepsItsOrder` (NATS down before the scan, then back) | Fails: an issue's subject held `[3 5 7 … 19 1]`, because event 1 was backed off | Passes: in id order, 0.5-0.7 s after NATS came back | Passes: each issue's events in id order on each destination |
| `TestABatchCutOffInFlightKeepsItsOrder` (connection cut with acknowledgements in flight at 150 ms round trip) | Fails on the first pipelined build: 100 events backed off, then reordered | Passes: every event released, no attempt counted, stream in id order with no duplicates | Passes: every event released, no attempt counted, each destination in id order, no duplicates |
| `TestAnOutboxThatDiesBetweenAckAndMarkPublishesAgainWithoutDuplicates` | Passes | Passes: republished once the claim lapses, the stream's MsgId drops every repeat | Passes: republished once the dead holder's hold lapses, the stream's MsgId drops every repeat |
| `TestABackedOffEventHoldsBackOnlyItsDestination` (one session's subject fails for an event; or the session holds the issue's role and the stream does not acknowledge the forward of that event) | n/a | Fails: the session takes the later events first, `[2 3 1]` | Passes: the topic family takes the later events at once, the session all three in order; the role variant fails before the role forward, since the holder then receives nothing from the outbox |
| `TestAnIssuesEventsGoToEachDestinationOneAtATime` | n/a | n/a | Passes: nothing of an owner is marked or backed off until every answer of that owner is in |
| `TestAnOutboxStoppedMidBatchMarksTheAnsweredAndReleasesTheRest` | n/a | Passes; fails if the outcome write uses the cancelled context | Passes |

## What order consumers rely on

- The Legion daemon applies an issue's facts by their per-issue `seq` and drops one at or below the
  last it applied (`daemon/internal/workflow/engine.go`, `admit/admit.go`). So on the success
  path each issue's events must reach its subject in id order. A late older event is lost. One
  issue's ids commit in id order, since the broker draws them under the issue's row lock; two
  issues' ids do not, and no consumer of NATS relies on an order across issues
  (`TestOneIssuesEventsReachItsSubjectInSeqOrderWhileAnotherIssueCommitsAcrossThem`).
- The Envoy listener dedupes by `dedupe_key` and relies on no order (`cmd/listener/delivery.go`).
- The SSE stream reads Postgres, not NATS.

## Known limits, not addressed here

- A failed outcome write resends what it was for: an owner settled as its own chains finished, or
  every owner the batch's own write covers. JetStream drops the repeats by MsgId, a role forward's
  included, and a role's holder drops a repeat forward by its dedupe key.
