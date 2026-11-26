---
title: "A durable created where a failed stream delete kept its directory restores the old durable's position"
category: testing
tags:
  - nats
  - jetstream
  - durable-consumer
  - flaky-tests
  - go
date: 2026-11-21
status: active
module: packages/daemon/internal/testnats
symptoms:
  - "legion-go-CAPTURE-dispatch was created with 1 messages of history pending, want 0"
  - "3 facts committed, want only the message published after the consumers were created"
related_issues:
  - "cybermonklabs/legion#4895"
---

# A durable created where a failed stream delete kept its directory restores the old durable's position

- A stream delete can leave the stream's directory in place: when an earlier delete of the same
  name is still removing its `.<stream>`, the rename fails and the server answers success
  ([a-nats-stream-deleted-twice-in-quick-succession-comes-back-with-its-messages.md](a-nats-stream-deleted-twice-in-quick-succession-comes-back-with-its-messages.md)
  says how). Emptying the stream first keeps its messages out of the kept directory, but not its
  consumers' state.
- A durable on a single server writes its position to `streams/<stream>/obs/<consumer>/o.dat` when
  it is created, unless it or its stream uses memory storage (v2.15.1 `server/consumer.go`
  6481-6488, `SetStarting`), and a consumer deleted with its stream leaves that file to the stream's
  removal (`server/filestore.go` 14607-14612).
- A durable of the same name created on the next stream of that name finds the file and restores
  it instead of choosing its start (`server/consumer.go` 1350-1353), so it starts where the earlier
  durable stood, whatever deliver policy it asked for. v2.10.29 does the same (`server/consumer.go`
  962-965).
- A fixture that deletes and recreates streams of one name for each test therefore deletes each
  stream's consumers before the stream. A consumer delete removes the consumer's directory before
  it answers (`server/jetstream_api.go` 5835, `server/filestore.go` 14582-14583), so a stream
  directory a failed rename keeps holds no consumer state. Delete them before the purge too: a
  purge updates each consumer's state (`server/consumer.go` 6691) and starts a write the delete
  would race.

## Evidence

- On 2026-11-21 the daemon's `TestAFreshConsumerStartsAtTheNextMessage`, run under heavy load after
  a test that made the same durables, found them "created with 1 messages of history pending, want
  0" and committed 3 facts where it wanted 1.
- With a non-empty `.ENVOY_NOTIFICATIONS` planted before each delete of `testnats`' reset, against
  a host `nats-server -js -sd <store>` v2.15.1, the directory the delete kept held the earlier
  tests' `o.dat` files (3, then 6) and that test failed with the same three lines, 10 of 10 runs;
  with the reset deleting each consumer first, the kept directory held none and it passed 10 of 10.
- `testnats`' `TestADurableOfAnEarlierTestDoesNotReachTheNextTest` plants such a directory itself
  and checks two recreated deliver-new durables have nothing pending. It fails without the consumer
  deletes, and with a reset that deletes only the first consumer it fails naming the second.
