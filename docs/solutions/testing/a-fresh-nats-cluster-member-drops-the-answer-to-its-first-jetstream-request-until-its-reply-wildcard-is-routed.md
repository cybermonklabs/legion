---
title: "A fresh NATS cluster member drops the answer to its first JetStream request until its reply wildcard is routed"
category: testing
tags:
  - nats
  - jetstream
  - cluster
  - testnats
  - flaky-tests
date: 2026-11-26
status: active
module: packages/envoy/internal/testnats
---

# A fresh NATS cluster member drops the answer to its first JetStream request until its reply wildcard is routed

Extends docs/solutions/testing/the-in-process-nats-cluster-restarts-a-mute-member-and-a-single-server-is-a-false-pass-for-a-replica-raise-pr2362.md.

- A JetStream request that gets "nats: timeout" on a cluster, while the request itself took effect
  (the consumer was updated, the stream was made) and the same request sent again is answered,
  may have lost its answer rather than met a slow election. Check whether the member the client
  entered through had ever passed a JetStream request before, and whether a third member, neither
  that one nor the meta leader, sent the answer.
- nats-server (v2.15.1, unchanged in v2.15.3) makes the system account's reply wildcard,
  `_R_.<prefix>.>`, at the first request that crosses an account's `$JS.API.>` import on a member
  (`newServiceReply`, `server/accounts.go:2504-2522`), and sends it to the routes then
  (`updateRouteSubscriptionMap`, `server/route.go:2553-2723`). The request goes on at once. A
  member that hears the request from the entry member hears the wildcard first, on the same route,
  so its answer comes back: the meta leader's answer to an account info
  (`server/jetstream_api.go:1461-1475`), and a consumer leader's to a consumer info. The answer to
  a consumer update comes from the consumer's leader once it applies the meta leader's Raft
  commit (`server/jetstream_cluster.go:7840-7856` and `9045-9145` for a one-copy consumer, as the
  row's is; `7891-7928` for a replicated one). When that leader is a third member, the commit
  reaches it on another route than the wildcard, can arrive first, and the answer is dropped.
  nats-server commit ed85c34e2c (pull request 8929, the fix for issue 8823, after v2.15.3) makes
  concurrent first requests on one server wait for the local subscription
  (`server/accounts.go:2530-2539` at main 4d459637232a); it does not change when the routes hear
  it.
- `testnats.Cluster.AwaitReady` now waits for the fact: it asks every running member's JetStream
  for the account's info, which makes that member's wildcard, then waits until every other
  running member's routes hold it (`replyRoutesReady`, which reads `Subsz` on the member and
  `Routez` on the others). `StartClusterWithCallout` lists a static user of the fixture's own,
  which may ask for account info only, so it runs the same `AwaitReady` as `StartCluster`.
- A member restarted with `Start` makes a new wildcard. Call `AwaitReady` after it, as the cluster
  tests already do.

## Evidence

- `TestThePrepareOperatorRaisesThresholdsAndWritesTheCohort` failed at
  `operator_prepare_test.go:47`, `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.session-a got no
  answer`, in envoy-go job 140166810178 (run 45894976056), with no meta leader change after the
  cluster formed.
- The row alone in the build cluster, four processes on two CPUs: on main, 6 of 48,000 plain runs
  failed the same way (3 of 16,000 and 3 of 32,000 in two runs) and 0 of 7,680 `-race` runs; with
  this change, 0 of 32,000 plain and 0 of 7,680 `-race` runs, the change's runs beside main's
  second run. Every failure was at line 47.
- A copy of the row on main that logged, for the update, the member the operator entered through,
  whether that member held a reply wildcard before it, the meta leader and the consumer's leader
  lost the answer in 1 of 64,000 runs. In that run the entry member had no wildcard, the
  consumer's leader was neither it nor the meta leader, the update had landed (inactive
  threshold 2h), and the same update sent again was answered. 11,854 of the 64,000 runs had that
  shape; none of the other 52,146 lost an answer.
- A standalone program of three members of unmodified nats-server, whose routes run through
  relays it owns, delays only the bytes from the entry member to the consumer's leader by 300 ms
  and sends a consumer update as the first request through the entry member. On v2.15.1 the
  answer was lost in 100 of 100 trials (one-copy consumer) and 30 of 30 (three copies), each time
  with the update applied and the update sent again answered; with no delay, 0 of 180; with one
  account info through the entry member and a wait until the leader's routes hold its wildcard,
  0 of 100 and 0 of 30. On v2.15.3, 100 of 100.
- The first account info request through each member of 1,000 fresh three-member clusters was
  answered every time (3,000 of 3,000), through the meta leader and through the other members.
- Planted faults, both in one nats-server copy whose only change is in `updateRouteSubscriptionMap`,
  and both arms of each built against it; the change's arm also logs each retry of the readiness
  wait's account info.
- Planted fault A holds every route's copy of a reply wildcard back 2 s
  (`ENVOY_PLANT_REPLY_ROUTE_DELAY`). On main the row failed 61 of 100 runs (24 at line 47, 37 at
  line 36, `CONSUMER.INFO ... got no answer`); with this change, 100 of 100 passed. This plant
  breaks the same-route order too, so it also loses the consumer info at line 36, which the race
  alone never did, and it loses the readiness wait's own account info: the change's arm sent it
  again 200 times in its 100 runs, and the retry outlasts the 2 s. So it cannot show that the wait
  on the routes is needed.
- Planted fault B holds the wildcard back 30 s on every route but the one to the meta leader
  (`ENVOY_PLANT_SPARE_META_LEADER`), so the account info is answered at once. It still holds the
  route to a consumer leader that is not the meta leader, so it can lose a consumer info as well
  as a consumer update. On main the row failed 27 of 80 runs (at lines 29, 36 and 47); with this
  change but no wait on the routes, 14 of 120 (at lines 29 and 36); with this change, 0 of 120.
  In those last 240 runs the readiness wait never sent an account info request again, and each of
  the change's passing runs took 34 s or more: it waited out the held wildcard.
