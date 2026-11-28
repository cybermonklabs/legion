---
title: "A shutdown budget bounds only the wait, so name what cuts the work and sum its own bounds"
category: best-practices
tags:
  - shutdown
  - go
  - context
  - graceful-stop
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2404"
---

# A shutdown budget bounds only the wait, so name what cuts the work and sum its own bounds

## The rule

1. A `context.WithTimeout` handed to a stop bounds how long the caller waits for it. It does not
   stop work the stop started in its own goroutines with their own bounds. Add up those bounds
   (every grace, flush, release and drain) and write the sum beside the budget, including
   the worst case when each one runs to its limit.
2. When the sum can exceed the budget, say what happens to the work still running: which later
   step of the shutdown ends it (a connection drain, a close, `os.Exit`). Do not write "the
   runtime kills it". If the process's own steps cut the work, a longer grace period in the
   runtime changes nothing, and an operator who raises it based on your text gains nothing.
3. Make what an overrun can lose safe. Then write down what it is: here, a hand-back cut while it
   drains, whose messages come back to the next owner at their ack wait, inside its safe hold.
4. When you add a step to a stop, update every place that states the sum: the code comment, the
   README and the changelog.

## Evidence

- `packages/envoy/cmd/listener/main.go:3150` gives the receive stop 10 s
  (`shutdownCtx`). Its parts come to 9 s at most: a 3 s grace, a 1 s cancel wait, a 1 s flush,
  a 2 s release and a 2 s drain.
- Past the budget the shutdown goes on to `server.Shutdown` (`:3169`), then `client.Drain`
  (`:3205`), which ends a hand-back's subscription still draining, then `os.Exit(1)`. The comment
  at `:3138-3149` says this.
- Round 7 of #2404 found the sum missing the mark write. Round 8 found the text blaming the
  runtime's kill. Round 9 found it naming `os.Exit` for what `client.Drain` does. Each finding
  was one line of prose, but a wrong line here misleads anyone sizing the pod's
  `terminationGracePeriodSeconds`.
