---
title: "Two log lines order an event only when the rule that orders them is named"
category: testing
tags:
  - log-assertions
  - transaction
  - daemon-runtime
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2727"
  - "cybermonklabs/legion#4533"
---

# Two log lines order an event only when the rule that orders them is named

Extends docs/solutions/testing/a-status-endpoint-names-a-new-leader-before-that-leader-logs-it-so-the-log-is-a-condition-of-the-wait-pr2639.md, which covers a log line that trails an endpoint.
Here the order has a named cause in the tree, which is what makes the order evidence rather than timing.

- **In the Legion daemon a line that says what a transaction wrote is a hook of that transaction's commit (`store.OnCommit`).**
  It is written once the transaction commits, and never for a transaction that rolls back.
  A transaction's hooks run in the order they were registered, on the goroutine that commits it, before `store.Transact` returns; only a commit whose outcome the store could not say leaves them to the outcome watcher, which runs them later.
  So the relative order of two such lines carries a fact that neither the final state nor either line alone carries: which transaction, and which registration within it, came first.

- **Say in the test, next to the assertion, what orders the two lines.**
  Without that sentence the ordering assertion is folklore, and the next reader deletes it or inverts it.

- **Let a state assertion carry the claim and the log carry only the order.**
  This does not contradict docs/solutions/testing/measure-the-mechanism-not-the-symptom-count-the-work-the-system-does-not-do-INFRA-228.md, which says a log line is a proxy that drifts.
  The proxy here is anchored to a code fact rather than to a duration, and the thing it proves, the order, is not available from the state at all, which the same test reads separately.

- **An absence of a line within a window is the strongest half.**
  "No line of this kind stands between these two" is the assertion a reordering defeats, where "the right line exists" passes either way.

## The current case

Admission's release of held roots (`Admission.release`, `packages/daemon/internal/admit/admit.go`) registers its line, `admission: the Dispatch consumer has caught up; releasing held roots`, before its pass takes any slot.
Each slot the pass takes registers its own `admission: slot taken` line after it (`Admission.logSlot`).
So within the release's transaction the release line comes first, and a slot line before the release line is a slot that an earlier transaction took while the root was still held.

`TestAHeldRootKeepsItsPlaceOnTheRuntimeUntilItsOwnReleaseAdmitsIt` (`packages/daemon/internal/daemon/hold_release_test.go`) scans the captured log once, asserts that no slot line comes before the release line, says beside that check what orders the two, and reads the slots table with `tickSlotted` separately.
`TestAnAdmissionLineOfACommittedWriteIsWrittenOnlyOnceItsTransactionCommits` (`packages/daemon/internal/admit/committed_lines_test.go`) holds the other half: the release line, like each other line of admission that states a write, is written for no transaction whose COMMIT never reached the server, and once for the one that commits.

Its three operator-visible lines are the ones a production check reads from `kubectl logs`:

```
msg="admission reconcile: holding roots for the Dispatch consumer to reach a stream position" count=1 target=1000 ack_floor=0
msg="admission: the Dispatch consumer has caught up; releasing held roots" count=1 ack_floor=0 idle=true admitted=1
msg="admission: slot taken" issue=LEGION-HELD slots=1 cap=1
```

The assertion that fails on a broken order is the absence one: no `admission: slot taken` between the first line and the second.

A log sink a test shares with a daemon goroutine must be mutex-guarded, or CI's `-race` lane on the package reports a data race.
The package's own `syncBuffer` (`packages/daemon/internal/daemon/controller_watch_test.go`) is that sink, and this test reuses it rather than adding a third copy.

## History: the fix of sjawhar/legion#2727

This is the evidence the learning was first written from; the mechanism it names has since been replaced.
When #2727 landed, the slot line deferred through `intake.OnCommit`, which no longer exists, and the release line was written inline inside its transaction.
The two lines were then ordered by opposite sides of the commit, which is how the first version of this note stated the rule.
The proof had to show that no slot was taken before the hold's release on the real daemon runtime, and the absence assertion is what failed on the pre-fix code: with the bug, a slot line naming the lower-ranked root stood between the hold line and the release line.
Since cybermonklabs/legion#4533 both lines are hooks of the one commit, ordered by registration, and the same assertion holds.
