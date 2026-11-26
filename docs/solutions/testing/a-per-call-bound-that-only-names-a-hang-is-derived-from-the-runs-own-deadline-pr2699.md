---
title: "A per-call bound that only names a hang is derived from the run's own deadline"
category: testing
tags:
  - go
  - timeout
  - t-deadline
  - race-detector
  - loaded-runner
  - flaky-tests
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2699"
---

# A per-call bound that only names a hang is derived from the run's own deadline

A fixed wall-clock bound over a measurement whose wall time scales with contention is not a bound:
it is a second failure mode. Under `-race`, on a shared runner, a healthy call takes whatever the
machine gives it.

- Where a per-call timeout exists only so that a hang fails by name rather than as the package's
  whole-run timeout, derive it from the run's own deadline: `t.Deadline()` minus a reporting
  margin, capped for a run with no deadline. Do not pick a constant.
- Fail by name when the deadline leaves no more than the margin, and name the cause. A `-timeout`
  no longer than the margin leaves no test room to wait, and the text asks for a larger one. A
  longer `-timeout` was used up by what ran before the wait, such as an earlier test whose own wait
  ran to its bound; calling that a misconfigured run sends the reader to the wrong fix.
- Do not raise the constant instead. A raised constant moves the cliff and keeps the shape that
  made the test fail on a loaded machine, and it costs the next reader the same measurement.
- Record the measurement that forced it in the comment beside the bound: the healthy call's
  observed time, on which machine, against the bound it replaced. The next person to touch the
  number needs it, and a number with no measurement invites another guess.
- Take the span and that failure from `testwait.Remaining(t, margin)`
  (`packages/envoy/internal/testwait/remaining.go`), with `min` against a cap for a run with no
  deadline, rather than a copy of the shape. The local copies each kept their own failure text, and
  each of them told the reader to raise a `-timeout` that an earlier test had used up.

## Evidence

sjawhar/legion#2699 (INFRA-436) replaced a fixed 60 s per-call bound in
`packages/envoy/internal/dispatch/pmdoc/linear_test.go` with `perCallDeadline(t)`:
`t.Deadline()` less `perCallReportMargin`, capped at `perCallHangCap`. The comment carries the
measurement — a healthy 1 MiB call took 50.26 s, 84 % of the bound it replaced, in a two-CPU gVisor
pod — inside a file whose two linearity tests took 1849.7 s loaded and 1410 s unloaded under
`-race`. `packages/envoy/internal/broker/enroll/serving_test.go` took the same shape independently
for its own wait.

Six such copies, and then agentloop's `runnerTime`, said "raise -timeout", most of them "this looks
like a misconfigured run; raise -timeout", when an earlier test had used the deadline up.
sjawhar/legion#4847 made `testwait.Remaining` the one bound and moved those six onto it,
`perCallDeadline` among them; agentloop's `runnerTime` followed.

The rejected alternative is on record: sjawhar/legion#2495 raised the same constant to two minutes
and dropped the mebibyte case, which moves the cliff and loses the shape the mutation control has
to fail on.
