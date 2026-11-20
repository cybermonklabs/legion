// The phase-hook endpoint after a turn the provider refused: Claude Code's `StopFailure`, the
// daemon's provider-error ladder, and a claim the daemon has failed. The endpoint and its harness
// are legion-phase-hook.test.ts's (legion-phase-hook-harness.ts).
import { expect, test } from "bun:test"
import { REMINDER } from "@legion/pi-shared/phase-stall"
import { waitFor } from "./channel-session-harness"
import { harness, implementer, recorded } from "./legion-phase-hook-harness"

// `StopFailure` runs instead of `Stop` when the turn ended on an API error, and its
// `${last_assistant_message}` is the error text rather than anything the worker said ("Hooks
// reference", code.claude.com/docs/en/hooks). That is the provider refusing the turn, so the
// answer carries no continuation: the host would turn one into another turn at once and the
// provider would refuse that one the same way, as it did on every turn of an implementer whose
// provider refused them all. One timer is armed all the same, because a turn no daemon task
// started has no other wake: a single transient refusal would otherwise leave the issue idle. This
// claim is off the daemon's provider-error ladder; the test below holds that a claim on it is sent
// nothing by that timer.
test("a turn that ended on an API error carries no continuation, and is woken once by its timer", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 0, { lastMessage: "I pushed the branch." }),
    claim: implementer,
    onTheLadder: false,
  })
  try {
    expect(
      await endpoint.handle({
        event: "StopFailure",
        lastAssistantMessage: "API Error: Rate limit reached",
      }),
    ).toEqual({})
    // The words the worker last said stand: the provider's error text is not one of them, and a
    // turn the worker never read moves nothing.
    expect(await record()).toEqual({
      state: "open",
      count: 0,
      reported: false,
      lastMessage: "I pushed the branch.",
    })
    // Nothing is sent as the turn ends; the reminder comes from the timer instead.
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the reminder the refused turn's timer carries")
    expect(inbox).toEqual([REMINDER])
    // One wake, not a run of them: the next refusal arms the next one. An assertion that nothing
    // more arrives has no signal to await, so this is one of the file's fixed waits, three times
    // the reminder interval the harness sets.
    await Bun.sleep(60)
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({
      state: "open",
      count: 1,
      reported: false,
      lastMessage: "I pushed the branch.",
    })
  } finally {
    await close()
  }
})

// While the daemon's provider-error ladder holds this claim's task, it sends that task again
// after its own pause, so the refused turn's timer sends nothing: a reminder before the ladder's
// send would be one more prompt into a session the provider is refusing.
test("a turn that ended on an API error while the ladder holds the task is reminded of nothing", async () => {
  const { endpoint, record, inbox, lines, close } = await harness({
    record: recorded("open", 0, { lastMessage: "I pushed the branch." }),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "StopFailure",
        lastAssistantMessage: "API Error: 529 Overloaded",
      }),
    ).toEqual({})
    // The line is written inside the timer's own record check, which answers that no reminder
    // goes, so once it is seen the timer has decided: no fixed wait is needed.
    await waitFor(
      async () =>
        lines.some((line) => line.includes("provider-error ladder holds this claim's task")),
      "the timer's record check to defer to the ladder",
    )
    expect(inbox).toEqual([])
    expect(await record()).toEqual({
      state: "open",
      count: 0,
      reported: false,
      lastMessage: "I pushed the branch.",
    })
  } finally {
    await close()
  }
})

// The daemon failing this role's claim is its word that this session is finished: the claim's
// process is stopped and the issue waits for its architect's retry. A refusal whose error cannot
// clear fails the claim on the turn's own report, so this is what ends the reminding here, since
// this host sees a refused turn only as the `StopFailure` of the turn it refused.
test("no reminder is sent once the daemon has failed this role's claim", async () => {
  const { endpoint, record, inbox, lines, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
    claimState: "failed",
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    // A case asserting that nothing happens has no condition to wait for, so this is one of the
    // file's fixed waits (the harness says why a fake clock settles nothing here), three times
    // the reminder interval.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({
      state: "open",
      count: 1,
      reported: false,
      lastMessage: "Still here.",
    })
    expect(lines.some((line) => line.includes("the daemon has failed this claim"))).toBe(true)
  } finally {
    await close()
  }
})
