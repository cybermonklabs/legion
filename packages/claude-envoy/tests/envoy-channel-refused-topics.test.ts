// The topics a channel session's registration may not carry: a topic an edge listener refuses for
// the session's projects, which the session drops. A topic no stream subject this build compiles
// captures is registered all the same, since the listener judges capture on the deployed stream's
// subjects (envoy-channel-registry.test.ts).
import { expect, test } from "bun:test"
import { projectRefusal } from "@legion/envoy-client/test/subscribe-answer"
import { EnvoyApiError } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import {
  directSubject,
  FakeNats,
  FakeNotifier,
  noInterest,
  recordingClient,
  sessionOptions,
  settled,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const scratchState = useScratchState()

// An edge listener refuses a whole registration that names a topic of no project of the session's
// token, naming each refused topic. The session drops exactly those, so its next heartbeat is not
// refused for them again, and keeps every other topic it follows.
test("a topic the edge refuses for the session's projects is dropped, and the heartbeats register the rest", async () => {
  const refusedRoot = "notifications.github.acme.priv"
  const subscribes: Array<readonly string[]> = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input.topics)
      const refused = input.topics.filter((topic) => topic.startsWith(refusedRoot))
      if (refused.length > 0) {
        throw new EnvoyApiError({
          method: "POST",
          url: "http://listener.example/v1/interests/subscribe",
          status: 403,
          responseBody: JSON.stringify(projectRefusal(refused)),
        })
      }
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      connection: new FakeNats(),
      heartbeatMs: 25,
      client,
    }),
  )

  try {
    await session.follow([`${refusedRoot}.>`, "notifications.dispatch.issue.DSP-7.>"])
    const refusedAt = subscribes.findIndex((topics) =>
      topics.some((topic) => topic.startsWith(refusedRoot)),
    )
    await waitFor(
      async () => subscribes.length >= refusedAt + 3,
      "two heartbeats after the refusal",
    )

    const carriedAgain = subscribes
      .slice(refusedAt + 1)
      .filter((topics) => topics.some((topic) => topic.startsWith(refusedRoot)))
    if (carriedAgain.length > 0) {
      throw new Error(
        `a registration carried a refused topic again: ${JSON.stringify(carriedAgain)}`,
      )
    }
    expect(session.topics()).toEqual([
      directSubject,
      "notifications.dispatch.issue.DSP-7",
      "notifications.dispatch.issue.DSP-7.>",
    ])
    expect(subscribes.at(-1)).toEqual([
      directSubject,
      "notifications.dispatch.issue.DSP-7",
      "notifications.dispatch.issue.DSP-7.>",
    ])
  } finally {
    await session.shutdown()
  }
})

// The edge can start refusing a topic it accepted before, as when the broker's answer for the
// token's projects stops naming a repository: the heartbeat's registration is refused, the topic
// is dropped, and the session is told once, naming the topic and the edge's reason, rather than
// going on believing it follows it.
test("a topic the edge refuses at a heartbeat is told to the session once, naming it and the reason", async () => {
  const refusedRoot = "notifications.github.acme.priv"
  let refusing = false
  const subscribes: Array<readonly string[]> = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input.topics)
      const refused = input.topics.filter((topic) => topic.startsWith(refusedRoot))
      if (refusing && refused.length > 0) {
        throw new EnvoyApiError({
          method: "POST",
          url: "http://listener.example/v1/interests/subscribe",
          status: 403,
          responseBody: JSON.stringify(projectRefusal(refused)),
        })
      }
      return noInterest()
    },
  })
  const notifier = new FakeNotifier()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      connection: new FakeNats(),
      heartbeatMs: 25,
      client,
      notifier,
    }),
  )
  const told = () =>
    notifier.notifications
      .filter((written) => written.eventId === undefined)
      .map((written) => written.content)
      .filter((content) => content.includes(refusedRoot))

  try {
    await session.follow([`${refusedRoot}.>`, "notifications.dispatch.issue.DSP-7.>"])
    expect(told()).toEqual([])
    refusing = true
    const refusedAt = subscribes.length
    await waitFor(
      async () => subscribes.length >= refusedAt + 4,
      "three heartbeats after the refusal",
    )

    expect(told()).toHaveLength(1)
    expect(told()[0]).toContain(`${refusedRoot}, ${refusedRoot}.>`)
    expect(told()[0]).toContain("repository_not_in_project")
    expect(session.topics()).not.toContain(`${refusedRoot}.>`)
  } finally {
    await session.shutdown()
  }
})

// A stopping session registers nothing and is told nothing (R24 of dispatch://INFRA-3589). A
// heartbeat's registration is on the wire when the shutdown begins, and only then does the edge
// answer it a project refusal: the rest is not registered again, and the session is not told of
// the refused topic.
test("a project refusal answered after the shutdown began registers nothing again and tells nothing", async () => {
  const refusedRoot = "notifications.github.acme.priv"
  let holding = false
  const held: Array<() => void> = []
  const subscribes: Array<readonly string[]> = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input.topics)
      const refused = input.topics.filter((topic) => topic.startsWith(refusedRoot))
      if (holding && refused.length > 0) {
        // Answered only once the test lets it go, after the shutdown began.
        const answer = Promise.withResolvers<void>()
        held.push(answer.resolve)
        await answer.promise
        throw new EnvoyApiError({
          method: "POST",
          url: "http://listener.example/v1/interests/subscribe",
          status: 403,
          responseBody: JSON.stringify(projectRefusal(refused)),
        })
      }
      return noInterest()
    },
  })
  const notifier = new FakeNotifier()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      connection: new FakeNats(),
      heartbeatMs: 25,
      client,
      notifier,
    }),
  )

  try {
    await session.follow([`${refusedRoot}.>`, "notifications.dispatch.issue.DSP-7.>"])
    holding = true
    await waitFor(async () => held.length === 1, "a heartbeat's registration on the wire")
    const heldAt = subscribes.length

    const stopping = session.shutdown()
    for (const answer of held.splice(0)) answer()
    await stopping
    await settled()

    expect(subscribes.slice(heldAt)).toEqual([])
    expect(
      notifier.notifications
        .filter((written) => written.eventId === undefined)
        .map((written) => written.content)
        .filter((content) => content.includes(refusedRoot)),
    ).toEqual([])
  } finally {
    for (const answer of held.splice(0)) answer()
    await session.shutdown()
  }
})
