import { expect, spyOn, test } from "bun:test"
import {
  BUS_UNANSWERED_PREFIX,
  canonicalTopic,
  legacyDispatchIssueSubject,
  uncapturedTopicWarning,
} from "@legion/contracts"
import { EnvoyApiError, type EnvoyClient } from "@legion/envoy-client/transport"
import {
  type ChannelSession,
  executeEnvoyTool,
  startChannelSession,
} from "../src/envoy-channel-server"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
  directSubject,
  type FakeClient,
  FakeNats,
  noInterest,
  recordingClient,
  sessionOptions,
  settled,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

/**
 * A person's removal of this session's subscriptions, as its own consumer's relay delivers it:
 * every frame, a live removal included, reaches the session that way now.
 */
function relayRemoval(nats: FakeNats, event: string, topics: readonly string[], seq = 1): void {
  nats.emitRelay("ses_claude", {
    subject: directSubject,
    seq,
    reply: `_INBOX.${event}`,
    raw: JSON.stringify({
      event_id: event,
      source: "dispatch",
      topic: directSubject,
      issued_at: 1,
      payload_summary: "DSP-3 subscription removed",
      payload: JSON.stringify({
        issue_key: "DSP-3",
        type: "subscription.removed",
        actor: { kind: "user", id: "alice" },
        payload: { session_id: "ses_claude", by: { kind: "user", id: "alice" }, topics },
      }),
    }),
  })
}

const scratchState = useScratchState()

test("drops local topics named by a human Dispatch subscription removal", async () => {
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const stateDirectory = scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client: recordingClient(calls),
    }),
  )
  const topic = "notifications.dispatch.issue.DSP-3.>"

  try {
    await session.follow([topic])
    relayRemoval(nats, "remove-1", [topic])
    await waitFor(
      async () => !session.topics().includes(topic),
      "the removed topic to leave the session",
    )

    // Both the base and the wildcard leave the session, and the removal's own registry write
    // names the topic it named; the heartbeat's reconciliation takes the rest of the entry back
    // in line with what the session follows.
    expect(session.topics()).toEqual([directSubject])
    expect(calls.filter((call) => call.startsWith("unsubscribe"))).toEqual([
      "unsubscribe ses_claude notifications.dispatch.issue.DSP-3.>",
    ])
  } finally {
    await session.shutdown()
  }
})

test("a resumed server rebuilds the interests its session id already registered and follow reports them as not fresh", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const subscribes: Array<readonly string[]> = []
  const issueTopic = "notifications.dispatch.issue.DSP-3.>"
  const client = recordingClient([], {
    getInterest: async () => ({
      ...noInterest(),
      topics: [
        directSubject,
        "notifications.dispatch.issue.DSP-3",
        issueTopic,
        "notifications.role.reviewer",
      ],
    }),
    subscribe: async (input) => {
      subscribes.push(input.topics)
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
    }),
  )

  try {
    expect(subscribes).toEqual([[directSubject, "notifications.dispatch.issue.DSP-3", issueTopic]])
    // The relay subject (consumer delivery) is the whole subscription set: a registered topic is
    // what the session's consumer is filtered on, never a subscription of its own.
    expect([...nats.subscriptions.keys()]).toEqual(["notifications.envoy.deliver.ses_claude"])
    expect(await session.follow([issueTopic])).toEqual({ added: [], warnings: [] })
  } finally {
    await session.shutdown()
  }
})

test("a registration in flight when a topic is unsubscribed cannot write it back into the entry", async () => {
  const stateDirectory = scratchState()
  // The listener merges registered topics into the entry and only an unsubscribe removes one.
  const entry = new Set<string>()
  let held: PromiseWithResolvers<void> | undefined
  let registrationHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      const topics = [...input.topics]
      if (held !== undefined) {
        registrationHeld = true
        await held.promise
      }
      for (const topic of topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, { client }),
  )
  const dropped = "notifications.dispatch.issue.DSP-1"
  const kept = "notifications.dispatch.issue.DSP-9"

  try {
    await session.follow([dropped])
    expect(entry.has(dropped)).toBe(true)
    const release = Promise.withResolvers<void>()
    held = release
    // This registration reads its topics, `dropped` among them, then waits on the listener.
    const following = session.follow([kept])
    await waitFor(async () => registrationHeld, "the registration to reach the listener")
    held = undefined
    const unfollowing = session.unfollow([dropped])
    await settled()
    release.resolve()
    expect(await unfollowing).toEqual({ removed: [dropped] })
    await following
    expect([...entry].sort()).toEqual([directSubject, kept])
  } finally {
    await session.shutdown()
  }
})

test("a registration in flight when Dispatch removes a subscription cannot write the topic back", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const entry = new Set<string>()
  let held: PromiseWithResolvers<void> | undefined
  let registrationHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      const topics = [...input.topics]
      if (held !== undefined) {
        registrationHeld = true
        await held.promise
      }
      for (const topic of topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
    }),
  )
  const removed = "notifications.dispatch.issue.DSP-3"
  const kept = "notifications.dispatch.issue.DSP-9"

  try {
    await session.follow([removed])
    const release = Promise.withResolvers<void>()
    held = release
    const following = session.follow([kept])
    await waitFor(async () => registrationHeld, "the registration to reach the listener")
    held = undefined
    // Dispatch removes the topic from the entry itself, then tells the session.
    entry.delete(removed)
    relayRemoval(nats, "remove-2", [removed])
    await waitFor(async () => !session.topics().includes(removed), "the removal to be taken")
    release.resolve()
    await following
    await waitFor(async () => !entry.has(removed), `${removed} to stay out of the entry`)
    expect([...entry].sort()).toEqual([directSubject, kept])
  } finally {
    await session.shutdown()
  }
})

test("a registry removal that fails is reconciled by the next heartbeat, without stripping a topic re-followed since", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const entry = new Set<string>([directSubject])
  let failNextUnsubscribe = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      if (failNextUnsubscribe) {
        failNextUnsubscribe = false
        throw new Error("registry unavailable")
      }
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => ({
      ...noInterest(),
      session_id: sessionID,
      topics: [...entry],
    }),
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )
  const staysRemoved = "notifications.dispatch.issue.DSP-3"
  const reFollowed = "notifications.dispatch.issue.DSP-9"

  try {
    await session.follow([staysRemoved, reFollowed])
    failNextUnsubscribe = true
    relayRemoval(nats, "remove-reconcile", [staysRemoved, reFollowed])
    await waitFor(async () => !failNextUnsubscribe, "the removal's own registry write to fail once")
    // Both are already out of local state; re-follow one before any heartbeat
    // reconciles. A stale retry replaying the failure-time topic list would strip
    // it right back out; reconciliation, driven by live forwarder.topics(), must not.
    await session.follow([reFollowed])
    await waitFor(
      async () => !entry.has(staysRemoved),
      "the heartbeat to reconcile the stale entry",
    )
    expect([...entry].sort()).toEqual([directSubject, reFollowed])
  } finally {
    await session.shutdown()
  }
})

// The listener answers its bus-outage 503 also after it wrote the entry, when only the update of
// the consumer's filter met a NATS reconnect, so the line that says the removal could not be
// dropped from the registry is for every other failure.
test("a removal's registry write the listener's message bus did not answer is reported as that outage, and any other failure as not dropped", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const outage = `${BUS_UNANSWERED_PREFIX}: set the filter of ses_claude's session consumer: bus: NATS is reconnecting`
  let failure: unknown
  const client = recordingClient([], {
    unsubscribe: async () => {
      throw failure
    },
  })
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
    }),
  )
  const outageBody = JSON.stringify({ error: outage })
  const url = "http://listener/v1/interests/unsubscribe"
  const notDropped = "envoy-channel: could not drop a removed subscription from the registry"

  try {
    const rows = [
      {
        issue: "DSP-31",
        error: new EnvoyApiError({ method: "POST", url, status: 503, responseBody: outageBody }),
        line: `envoy-channel: the listener's message bus did not answer a request to drop a removed subscription from the registry — ${outage}; the registry may already be without it, and the next heartbeat reconciles the registry and sets the consumer's filter to the topics it holds\n`,
      },
      {
        issue: "DSP-32",
        error: new EnvoyApiError({ method: "POST", url, status: 500, responseBody: outageBody }),
        line: `${notDropped} — ${outage}; the next heartbeat reconciles it\n`,
      },
      {
        issue: "DSP-33",
        error: new TypeError("fetch failed"),
        line: `${notDropped} — fetch failed; the next heartbeat reconciles it\n`,
      },
    ]
    const lines = () => written.filter((line) => line.includes("removed subscription"))
    for (const [index, row] of rows.entries()) {
      const topic = `notifications.dispatch.issue.${row.issue}`
      await session.follow([topic])
      failure = row.error
      relayRemoval(nats, `remove-${row.issue}`, [topic], index + 1)
      await waitFor(async () => lines().length > index, `the line about ${row.issue}'s removal`)
      expect(lines()[index]).toBe(row.line)
    }
  } finally {
    await session.shutdown()
    stderr.mockRestore()
  }
})

test("a persistent reconciliation failure retries once after a delay of its own, then waits for the next heartbeat, rather than hot-looping", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  let getInterestAttempts = 0
  const client = recordingClient([], {
    getInterest: async () => {
      getInterestAttempts++
      throw new Error("listener degraded")
    },
  })
  // The one extra call the failed tick buys, read rather than waited out: a session waits
  // 20–40 s for it, and this fires it by hand.
  const scheduled: { readonly delayMs: number; readonly run: () => void }[] = []
  // A handoff pid makes startup request one heartbeat without waiting out the 10 s
  // interval below, which otherwise contributes no tick in this test's window. No
  // handoff file exists, so that tick's adoptHandoff no-ops, and the watcher's
  // zeroed-stat callback for the missing file must not request a second tick.
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 10_000,
      handoffPid: 991,
      scheduleRetry: (run, delayMs) => {
        scheduled.push({ delayMs, run })
        return () => undefined
      },
    }),
  )

  try {
    // One at startup (`syncRegisteredInterests`, swallowed there) and one from the heartbeat the
    // handoff pid requests. The call that failure buys waits out a delay rather than going at
    // once, so a listener that is already slow is not asked again in the same breath.
    await waitFor(
      async () => getInterestAttempts >= 2 && scheduled.length === 1,
      "the startup read, the first reconciliation attempt, and the one extra call it schedules",
    )
    expect(scheduled[0]?.delayMs).toBeGreaterThanOrEqual(20_000)
    expect(scheduled[0]?.delayMs).toBeLessThanOrEqual(40_000)
    scheduled[0]?.run()
    await waitFor(async () => getInterestAttempts >= 3, "the extra call itself")
    // A real wait, not a guessed one: proving the guard holds needs to observe a
    // window in which nothing further happens, and nothing else here produces an
    // event to wait for instead — a hot loop would have run thousands of times
    // over 150 ms (measured: ~9,000 in 300 ms without this fix).
    await Bun.sleep(150)
    expect(getInterestAttempts).toBe(3)
    // The extra call failed too: the session waits for the next heartbeat rather than asking
    // for a second one.
    expect(scheduled).toHaveLength(1)
  } finally {
    await session.shutdown()
  }
})

test("a registration that times out is retried once inside forty seconds, and a success returns the cadence to the heartbeat", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const claudePid = 993
  let registryDown = false
  let attempted = 0
  let registered = 0
  const client = recordingClient([], {
    subscribe: async () => {
      attempted += 1
      // The symptom from production: the registration call itself times out under listener load.
      if (registryDown) throw new Error("The operation timed out.")
      registered += 1
      return noInterest()
    },
  })
  // The extra call a failed tick buys, read rather than waited out: its delay is the assertion,
  // and the call itself is fired here, so the test spends none of the seconds a session does.
  const scheduled: { readonly delayMs: number; readonly run: () => void }[] = []
  // The handoff file is this test's tick: the server polls it, and one naming the id the session
  // already has adopts nothing, leaving a plain heartbeat behind it. The interval is the
  // production 90 s, so every call here is one a failure or this test asked for.
  const handoff = sessionHandoffFile(stateDirectory, claudePid)
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 90_000,
      handoffPid: claudePid,
      scheduleRetry: (run, delayMs) => {
        scheduled.push({ delayMs, run })
        return () => undefined
      },
    }),
  )

  try {
    // Startup registers, and so does the tick its handoff pid asks for: the ticks this test
    // drives come after both.
    await waitFor(
      async () => registered >= 2,
      "the registrations startup and the tick it asks for make",
    )

    // A tick whose registration times out buys one extra call, well short of the 90 s the
    // interval would otherwise leave the session unregistered for.
    registryDown = true
    await writeSessionHandoff(handoff, "ses_claude")
    await waitFor(
      async () => scheduled.length === 1,
      "the failed registration to schedule one extra call",
    )
    const delayMs = scheduled[0]?.delayMs ?? 0
    expect(delayMs).toBeGreaterThanOrEqual(20_000)
    expect(delayMs).toBeLessThanOrEqual(40_000)

    // It is a registration of its own, and the only one: a retry that fails waits for the next
    // heartbeat rather than asking again.
    const beforeRetry = attempted
    scheduled[0]?.run()
    await waitFor(async () => attempted > beforeRetry, "the extra call to reach the listener")
    await settled()
    expect(scheduled).toHaveLength(1)

    // The next heartbeat lands...
    registryDown = false
    const beforeHealthy = registered
    await writeSessionHandoff(handoff, "ses_claude")
    await waitFor(
      async () => registered > beforeHealthy,
      "the next heartbeat to register the session",
    )

    // ...and the success restored the budget: the failure after it buys an extra call of its own.
    registryDown = true
    await writeSessionHandoff(handoff, "ses_claude")
    await waitFor(async () => scheduled.length === 2, "a fresh extra call after the success")
    expect(scheduled[1]?.delayMs).toBeGreaterThanOrEqual(20_000)
    expect(scheduled[1]?.delayMs).toBeLessThanOrEqual(40_000)
  } finally {
    await session.shutdown()
  }
})

test("a registry read that fails at startup does not let the heartbeat delete the registered interests", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-5"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  let reads = 0
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => {
      reads++
      // The listener is down for exactly the startup read — a redeploy landing on
      // a plugin reload — and healthy from then on.
      if (reads === 1) throw new Error("listener unavailable")
      return { ...noInterest(), session_id: sessionID, topics: [...entry] }
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    await waitFor(async () => reads >= 4, "several heartbeats after the failed startup read")
    // A read the session never completed is not evidence that the entry drifted.
    // Nothing restores a topic deleted from it: `unregisterSession` leaves the
    // interests entry behind, so the stripped one is what the next resume reads.
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
    // Adopting it is also what stops the session being deaf to it until a restart.
    expect(session.topics()).toContain(alreadyRegistered)
  } finally {
    await session.shutdown()
  }
})

test("a listener error that is not a 404 is not an absence, so the heartbeat still adopts", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-7"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  let reads = 0
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => {
      reads++
      // The pair of the test above, with the error kind that separates the two
      // conjuncts of the not-found predicate. A plain `Error` is rethrown by any
      // form of it; an `EnvoyApiError` carrying a status that is not 404 is
      // rethrown only while the status is still checked. Dropping that conjunct
      // leaves something that still reads as a not-found handler, and no other
      // test in this suite objects: the read is swallowed as an absence, the adopt
      // branch completes over an empty list and sets the gate, and because
      // adoption runs once the session never hears this topic again.
      if (reads === 1) {
        throw new EnvoyApiError({
          method: "GET",
          url: "http://127.0.0.1:9020/v1/interests/ses_claude",
          status: 500,
          responseBody: "upstream unavailable",
        })
      }
      return { ...noInterest(), session_id: sessionID, topics: [...entry] }
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    await waitFor(
      async () => session.topics().includes(alreadyRegistered),
      "the heartbeat to adopt the topic the failed startup read could not",
    )
    // And the gate staying shut is what keeps the entry whole: a session that
    // adopted nothing would read every topic in it as drift on the next tick.
    await waitFor(async () => reads >= 4, "several heartbeats after the failed startup read")
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
  } finally {
    await session.shutdown()
  }
})

test("a shutdown landing before the heartbeat's registry read leaves the registry entry alone", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-1"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  const release = Promise.withResolvers<void>()
  let holdRegistration = false
  let registrationHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      const topics = [...input.topics]
      if (holdRegistration) {
        registrationHeld = true
        await release.promise
      }
      for (const topic of topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => ({
      ...noInterest(),
      session_id: sessionID,
      topics: [...entry],
    }),
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    expect(session.topics()).toContain(alreadyRegistered)
    holdRegistration = true
    await waitFor(async () => registrationHeld, "a heartbeat to reach the listener")
    holdRegistration = false
    // The tick is held in `register()`, so shutdown lands before the sync is
    // entered at all: this pins the window the entry check covers. The window
    // after the read is the test below.
    const shuttingDown = session.shutdown()
    await waitFor(async () => session.topics().length === 0, "the forwarder to close")
    release.resolve()
    await shuttingDown
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
  } finally {
    release.resolve()
    await session.shutdown()
  }
})

test("a shutdown landing inside the heartbeat's registry read leaves the registry entry alone", async () => {
  const stateDirectory = scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-2"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  const release = Promise.withResolvers<void>()
  const removals: string[][] = []
  let holdRead = false
  let readHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      removals.push([...input.topics])
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => {
      if (holdRead) {
        readHeld = true
        await release.promise
      }
      return { ...noInterest(), session_id: sessionID, topics: [...entry] }
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    expect(session.topics()).toContain(alreadyRegistered)
    holdRead = true
    await waitFor(async () => readHeld, "a heartbeat to reach the registry read")
    holdRead = false
    // The tick is past the entry check and suspended on the read when shutdown
    // empties the forwarder, so a check evaluated before the await passes going in
    // and finds an empty topic list coming out — every topic drift, the direct
    // subject with them.
    const shuttingDown = session.shutdown()
    await waitFor(async () => session.topics().length === 0, "the forwarder to close")
    release.resolve()
    await shuttingDown
    // The assertion is that the resumed tick does not write to the registry at
    // all, not that the entry happens to survive: keeping the direct subject out of
    // the drift set on its own caps the damage at `[[DSP-2]]` — the entry survives
    // and still holds the direct subject while a live subscription is silently
    // deleted from durable state anyway. Only re-checking `shuttingDown` after the
    // read gets to no write. (Without either, the drift set is both topics, and the
    // real listener deletes the entry outright rather than emptying it:
    // `removeInterestTopics` calls `deleteInterest`, internal/store/kv.go, and the
    // deregistration behind it removes only the sessions row, so a `--resume`
    // before `Registry.Reap` reads nothing back.)
    expect(removals).toEqual([])
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
  } finally {
    release.resolve()
    await session.shutdown()
  }
})

test("a Dispatch subscription removal that names no topics removes nothing", async () => {
  const stateDirectory = scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client: recordingClient(calls),
    }),
  )
  const topic = "notifications.dispatch.issue.DSP-3"

  try {
    await session.follow([topic])
    calls.length = 0
    relayRemoval(nats, "remove-empty", [])
    await waitFor(
      async () => nats.repliesOn("_INBOX.remove-empty").includes("done"),
      "the removal frame's done",
    )
    expect(calls.filter((call) => call.includes("unsubscribe"))).toEqual([])
    expect(session.topics()).toEqual([directSubject, topic])
  } finally {
    await session.shutdown()
  }
})

// No topic is judged uncaptured here on the stream subjects this build was compiled with: the
// listener judges that on the deployed stream's, which during a roll hold a newer build's too, so
// every followed topic is registered (`registrationTopics`). The caller of the `envoy_subscribe`
// tool is shown the listener's uncaptured-topic warning only for a topic it named in this call: not
// for the base an `X.>` adds (`expandSubscriptionTopics`), nor for another topic the session
// follows (`warningsForNamedTopics`). `notifications.x.y` stands for a topic only a newer build's
// subject captures (`notifications.x.>`, which this build does not compile).
test("registers a topic no compiled stream subject captures, however it was followed, and shows the listener's warning only for a topic the caller named", async () => {
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const stateDirectory = scratchState()
  // What the listener, reading the deployed stream, finds nothing captures yet.
  const uncaptured = new Set([
    "notifications.team",
    "notifications.lxspec.probe",
    "notifications.y.z",
  ])
  const subscribes: Array<readonly string[]> = []
  const client = recordingClient(calls, {
    getInterest: async () => ({
      ...noInterest(),
      // The row an earlier build left: it expanded `notifications.team.>` to the bare base
      // beside it, which no stream subject captures.
      topics: [directSubject, "notifications.team", "notifications.team.deploys"],
    }),
    subscribe: async (input) => {
      subscribes.push(input.topics)
      return {
        ...noInterest(),
        warnings: input.topics
          .filter((topic) => uncaptured.has(topic))
          .map((topic) => uncapturedTopicWarning(topic, input.sessionID)),
      }
    },
  })
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, { connection: nats, client }),
  )

  try {
    // The bare base came back with the row, is followed, and every registration carries it.
    expect(session.topics()).toEqual([
      directSubject,
      "notifications.team",
      "notifications.team.deploys",
    ])
    expect(subscribes).toEqual([
      [directSubject, "notifications.team", "notifications.team.deploys"],
    ])

    const subscribe = (topics: readonly string[]) =>
      executeEnvoyTool(
        { identity, client: client as unknown as EnvoyClient, session, clientInstanceID: "x" },
        "envoy_subscribe",
        { topics },
      )

    // A wildcard whose base no stream subject captures: the base is registered, and its warning,
    // like the restored `notifications.team`'s, is not shown, since the caller named neither.
    expect(await subscribe(["notifications.y.z.>"])).not.toHaveProperty("warnings")
    expect(subscribes.at(-1)).toEqual([
      directSubject,
      "notifications.team",
      "notifications.team.deploys",
      "notifications.y.z",
      "notifications.y.z.>",
    ])

    // A topic the caller named itself still has its warning shown, and that one alone.
    expect(await subscribe(["notifications.x.y", "notifications.lxspec.probe"])).toMatchObject({
      warnings: [uncapturedTopicWarning("notifications.lxspec.probe", "ses_claude")],
    })
    expect(session.topics()).toEqual(
      expect.arrayContaining(["notifications.x.y", "notifications.lxspec.probe"]),
    )
    expect(subscribes.at(-1)).toEqual(
      expect.arrayContaining([
        "notifications.team",
        "notifications.x.y",
        "notifications.lxspec.probe",
      ]),
    )

    // `follow` answers the same: the base's warning is left out, a named topic's kept.
    expect(await session.follow(["notifications.w.v.>", "notifications.lxspec.probe"])).toEqual({
      added: ["notifications.w.v", "notifications.w.v.>"],
      warnings: [uncapturedTopicWarning("notifications.lxspec.probe", "ses_claude")],
    })
  } finally {
    await session.shutdown()
  }
})

/** The tools' runtime over `session`, as `runEnvoyChannelServer` builds it. */
function toolRuntime(identity: SessionIdentity, client: FakeClient, session: ChannelSession) {
  return { identity, client: client as unknown as EnvoyClient, session, clientInstanceID: "x" }
}

// pi-envoy's `envoy_subscribe` answers a registration that failed as a success that names the
// error, and records the follow (`packages/pi-envoy/extensions/envoy.ts`, the subscribe case). A
// tool that throws there records nothing, so a restart forgets the topic.
test("a subscribe whose registration fails answers its error, and a restart follows the topic again", async () => {
  const stateDirectory = scratchState()
  let refusing = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      if (refusing) throw new Error("the listener is down")
      return noInterest(input.sessionID)
    },
  })
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const start = () => startChannelSession(sessionOptions(identity, stateDirectory, { client }))
  const topic = "notifications.dispatch.issue.DSP-4.>"

  const first = await start()
  try {
    refusing = true
    const answer = await executeEnvoyTool(toolRuntime(identity, client, first), "envoy_subscribe", {
      topics: [topic],
    })
    expect(answer).toEqual({
      added: ["notifications.dispatch.issue.DSP-4", topic],
      registrationError: "the listener is down",
    })
    expect(first.topics()).toContain(topic)
  } finally {
    await first.shutdown()
  }

  refusing = false
  const second = await start()
  try {
    expect(second.topics()).toContain(topic)
  } finally {
    await second.shutdown()
  }
})

// pi-envoy's `envoy_unsubscribe` sends the listener no role's topic: the listener's release of one
// frees this process's role claim, and the role claim holds that topic, not the follow. The topic
// still leaves the follows. The answer carries an empty `kept`, which pi-envoy leaves out.
test("an unsubscribe of a role's topic drops the follow and sends the listener no release of it", async () => {
  const stateDirectory = scratchState()
  const releases: Array<readonly string[]> = []
  const client = recordingClient([], {
    unsubscribe: async (input) => {
      releases.push(input.topics)
    },
  })
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const session = await startChannelSession(sessionOptions(identity, stateDirectory, { client }))
  const runtime = toolRuntime(identity, client, session)
  const roleTopic = "notifications.role.reviewer"
  const issueTopic = "notifications.dispatch.issue.DSP-6"

  try {
    await session.follow([roleTopic, issueTopic])
    const named = await executeEnvoyTool(runtime, "envoy_unsubscribe", { topics: [roleTopic] })
    expect(releases).toEqual([])
    expect(named).toEqual({ removed: [roleTopic], kept: [] })
    expect(session.topics()).not.toContain(roleTopic)

    // Every other topic is still released, and the role's topic is never in the release.
    await session.follow([roleTopic])
    const all = await executeEnvoyTool(runtime, "envoy_unsubscribe", {})
    expect(releases).toEqual([[issueTopic]])
    expect(all).toEqual({ removed: [issueTopic, roleTopic], kept: [] })
  } finally {
    await session.shutdown()
  }
})

// pi-envoy's `envoy_unsubscribe` answers a release that failed as a success that names the error.
// The topics stay unfollowed and recorded, and the heartbeat's interest reconciliation takes them
// out of the entry.
test("an unsubscribe whose release fails answers its error, and a restart does not follow the topic again", async () => {
  const stateDirectory = scratchState()
  let refusing = false
  const client = recordingClient([], {
    unsubscribe: async () => {
      if (refusing) throw new Error("the listener is down")
    },
  })
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const start = () => startChannelSession(sessionOptions(identity, stateDirectory, { client }))
  const topic = "notifications.dispatch.issue.DSP-8"

  const first = await start()
  try {
    await first.follow([topic])
    refusing = true
    const answer = await executeEnvoyTool(
      toolRuntime(identity, client, first),
      "envoy_unsubscribe",
      { topics: [topic] },
    )
    expect(answer).toEqual({
      removed: [topic],
      kept: [],
      registrationError: "the listener is down",
    })
    expect(first.topics()).not.toContain(topic)
  } finally {
    await first.shutdown()
  }

  const second = await start()
  try {
    expect(second.topics()).not.toContain(topic)
  } finally {
    await second.shutdown()
  }
})

// The first read of the interests that succeeds adopts the entry's topics, because before it the
// followed set holds only the direct subject. A topic the session unfollowed while no read had
// succeeded - the startup read failed with anything but a 404 - is still in the entry when its
// release failed too, and that adopt must not follow it again: the person unsubscribed.
// pi-envoy's resume follows the entry's topics too, and both clients release such a topic by one
// rule, `releasedOnResume` in @legion/envoy-client/delivery-ledger, which states when it holds.
test("a topic unfollowed before the first good read of the interests is not adopted back by that read", async () => {
  const stateDirectory = scratchState()
  const topic = "notifications.dispatch.issue.DSP-9"
  // The listener's entry: a registration merges its topics in, and only a release takes one out.
  const entry = new Set<string>([directSubject])
  let listenerUp = false
  let releasing = true
  let reads = 0
  const registered: Array<readonly string[]> = []
  const client = recordingClient([], {
    getInterest: async (sessionID) => {
      reads++
      if (!listenerUp) throw new Error("listener degraded")
      return { ...noInterest(sessionID), topics: [...entry] }
    },
    subscribe: async (input) => {
      registered.push([...input.topics])
      for (const subscribed of input.topics) entry.add(subscribed)
      return noInterest(input.sessionID)
    },
    unsubscribe: async (input) => {
      if (!releasing) throw new Error("the listener is down")
      for (const released of input.topics) entry.delete(released)
    },
  })
  const identity = new SessionIdentity("ses_claude", "/tmp")
  // The reconciliation's one extra call after a failed read, fired by hand.
  const scheduled: Array<() => void> = []
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, {
      client,
      heartbeatMs: 10_000,
      handoffPid: 991,
      scheduleRetry: (run) => {
        scheduled.push(run)
        return () => undefined
      },
    }),
  )

  try {
    // The startup read and the read of the heartbeat the handoff pid requests both fail: no read
    // of the interests has succeeded yet.
    await waitFor(
      async () => reads >= 2 && scheduled.length === 1,
      "two failed reads of the interests",
    )
    await session.follow([topic])
    expect(entry.has(topic)).toBe(true)
    releasing = false
    const answer = await executeEnvoyTool(
      toolRuntime(identity, client, session),
      "envoy_unsubscribe",
      {
        topics: [topic],
      },
    )
    expect(answer).toEqual({
      removed: [topic],
      kept: [],
      registrationError: "the listener is down",
    })
    expect(session.topics()).not.toContain(topic)
    // The release failed, so the entry still holds the topic when the listener answers again.
    expect(entry.has(topic)).toBe(true)
    const registrationsBefore = registered.length

    listenerUp = true
    scheduled[0]?.()
    await waitFor(async () => reads >= 3, "the first read of the interests that succeeds")
    await settled()

    expect(session.topics()).not.toContain(topic)
    expect(registered.slice(registrationsBefore).flat()).not.toContain(topic)
  } finally {
    await session.shutdown()
  }
})

// pi-envoy's `envoy_unsubscribe` reads an explicit empty `topics` as no topic named, so it removes
// nothing, and an absent `topics` as every topic the session follows (`topicsFor`).
test("an unsubscribe of an explicit empty topic list removes nothing, and one with no list removes every followed topic", async () => {
  const stateDirectory = scratchState()
  const releases: Array<readonly string[]> = []
  const client = recordingClient([], {
    unsubscribe: async (input) => {
      releases.push(input.topics)
    },
  })
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const session = await startChannelSession(sessionOptions(identity, stateDirectory, { client }))
  const runtime = toolRuntime(identity, client, session)
  const followed = ["notifications.dispatch.issue.DSP-2", "notifications.team.deploy"]

  try {
    await session.follow(followed)
    const none = await executeEnvoyTool(runtime, "envoy_unsubscribe", { topics: [] })
    expect(none).toEqual({ removed: [], kept: [] })
    expect(releases).toEqual([])
    expect(session.topics()).toEqual([directSubject, ...followed])

    const all = await executeEnvoyTool(runtime, "envoy_unsubscribe", {})
    expect(all).toEqual({ removed: followed, kept: [] })
    expect(releases).toEqual([followed])
    expect(session.topics()).toEqual([directSubject])
  } finally {
    await session.shutdown()
  }
})

// The listener's record keeps each topic as its caller wrote it, the old form of an issue's name
// included (packages/envoy/internal/store/legacy_topics.go), and the heartbeat's reconciliation
// (`syncRegisteredInterests`) unsubscribes every topic of the record the session does not follow
// by that exact name. So a session that follows an issue in the old form reads its record back as
// its own and unsubscribes nothing. The second row is the control the first leans on: a record
// rewritten to the new form reads to this very comparison as a lost registration, unsubscribed on
// one heartbeat and written back by the next, so a store that rewrote it would show here.
test("a followed topic the record keeps in its old form is never unsubscribed, where a rewritten one would be", async () => {
  const followed = legacyDispatchIssueSubject("INFRA-12", ">")
  const rewritten = canonicalTopic(followed).canonical
  expect(rewritten).toBe("notifications.dispatch.issue.INFRA.INFRA-12.>")
  for (const { name, store, unsubscribed } of [
    { name: "as the caller wrote it", store: (topic: string) => topic, unsubscribed: false },
    {
      name: "rewritten to the new form",
      store: (topic: string) => canonicalTopic(topic).canonical,
      unsubscribed: true,
    },
  ]) {
    const stateDirectory = scratchState()
    const entry = new Set<string>()
    const unsubscribes: string[] = []
    let reads = 0
    const client = recordingClient([], {
      subscribe: async (input) => {
        for (const topic of input.topics) entry.add(store(topic))
        return noInterest()
      },
      unsubscribe: async (input) => {
        unsubscribes.push(...input.topics)
        for (const topic of input.topics) entry.delete(topic)
      },
      getInterest: async (sessionID) => {
        reads++
        return { ...noInterest(), session_id: sessionID, topics: [...entry] }
      },
    })
    const session = await startChannelSession(
      sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
        connection: new FakeNats(),
        client,
        heartbeatMs: 20,
      }),
    )
    try {
      await session.follow([followed])
      const followedAt = reads
      await waitFor(async () => reads >= followedAt + 3, `three heartbeats with the record ${name}`)
      expect(unsubscribes.includes(rewritten), name).toBe(unsubscribed)
      if (!unsubscribed) expect(unsubscribes, name).toEqual([])
      expect(session.topics(), name).toContain(followed)
    } finally {
      await session.shutdown()
    }
  }
})
