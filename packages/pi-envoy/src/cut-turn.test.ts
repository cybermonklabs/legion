import { describe, expect, test } from "bun:test";
import { renderInbound } from "@legion/envoy-client/delivery";
import { LOCAL_ENVOY_NOTICE } from "@legion/pi-shared/interface";
import { encode } from "@toon-format/toon";
import { cutTurn, cutTurnNoticeText } from "./cut-turn";

const SESSION = "ses_cut";

/** An assistant message as the host writes it: `stopReason` says what ended it. */
function assistant(stopReason: string, errorMessage?: string): Record<string, unknown> {
  return {
    type: "message",
    message: {
      role: "assistant",
      stopReason,
      content: [{ type: "text", text: "working" }],
      ...(errorMessage === undefined ? {} : { errorMessage }),
    },
  };
}

/** A user message as the host writes it: a person's turn, or the daemon's task prompt. */
function user(text: string): Record<string, unknown> {
  return { type: "message", message: { role: "user", content: [{ type: "text", text }] } };
}

/** A delivery card as the host writes it into the transcript (`bootConsumer`'s `written`). */
function card(eventID: string, summary: string): Record<string, unknown> {
  return {
    type: "custom_message",
    customType: "envoy-message",
    display: true,
    details: { event_id: eventID },
    content: renderInbound(
      JSON.stringify({
        event_id: eventID,
        source: "agent",
        topic: `notifications.agent.${SESSION}`,
        source_session: "ses_sender",
        issued_at: Date.parse("2026-11-16T04:00:00Z"),
        payload_summary: summary,
      }),
      SESSION
    ).content,
  };
}

/** One of the plugin's own notices: a card of no delivery (`LOCAL_ENVOY_NOTICE`). */
function localNotice(): Record<string, unknown> {
  return {
    type: "custom_message",
    customType: "envoy-message",
    display: true,
    details: LOCAL_ENVOY_NOTICE,
    content: encode({ envoy: { notice: "session id changed", detail: "it moved" } }),
  };
}

/** The mark the plugin writes when a run of a session Legion drives starts (`ended: false`, at the
 *  host's `agent_start`) and when it ends (`ended: true`, at an `agent_end` that schedules no
 *  continuation). The type is written out rather than imported: it is what transcripts on disk
 *  hold, so a rename here must fail this file. */
function turnMark(ended: boolean): Record<string, unknown> {
  return { type: "custom", customType: "envoy-turn-mark", data: { ended } };
}

/** The tool result Oh My Pi writes at resume for a call whose process exited before it returned
 *  (`createInterruptedToolResults`, `packages/coding-agent/src/session/exit-diagnostics.ts:251` at
 *  the pin). */
function interruptedToolResult(): Record<string, unknown> {
  return {
    type: "message",
    message: {
      role: "toolResult",
      toolCallId: "call-1",
      toolName: "bash",
      content: [
        {
          type: "text",
          text: "Previous OMP process exited before this tool returned; its outcome is unknown.",
        },
      ],
      isError: true,
    },
  };
}

/** The assistant message Oh My Pi writes at resume to close a turn its previous process exited in,
 *  where that process recorded its exit (`createInterruptedTurnAbortMessage`,
 *  `packages/coding-agent/src/session/exit-diagnostics.ts:102,206` at the pin). */
function hostClosedTurn(): Record<string, unknown> {
  return assistant("aborted", "Previous OMP process exited before completing the turn.");
}

/** The entry a session writes for an attempt of a person's direct message it handled, before the
 *  card or the turn goes out (`HANDLED_ATTEMPT_ENTRY`). Written out, as `turnMark` is. */
function handledAttempt(messageID: string): Record<string, unknown> {
  return {
    type: "custom",
    customType: "envoy-dispatch-handled-attempt",
    data: { message_id: messageID, attempt: 1 },
  };
}

/** A person's Send the session accepted as their own turn, as the host writes it: their user
 *  message, carrying the Dispatch message id as the host's `tag` (`pi.sendUserInput`). */
function personsTurn(text: string, messageID: string): Record<string, unknown> {
  return {
    type: "message",
    message: { role: "user", content: [{ type: "text", text }], tag: messageID },
  };
}

describe("the cut turn a resumed transcript holds", () => {
  test("a transcript whose last assistant message ended its turn holds no cut turn", () => {
    expect(
      cutTurn([user("do it"), card("evt-1", "a notice"), assistant("endTurn")])
    ).toBeUndefined();
  });

  test("an empty transcript holds no cut turn", () => {
    expect(cutTurn([])).toBeUndefined();
  });

  test("a turn whose last assistant message called a tool is cut", () => {
    const cut = cutTurn([user("do it"), assistant("toolUse")]);
    expect(cut).toBeDefined();
    expect(cut?.deliveries).toEqual([]);
  });

  test("a cut turn names each delivery written into it", () => {
    const cut = cutTurn([
      user("do it"),
      card("evt-notice", "the controller tick"),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries).toEqual([
      { id: "evt-notice", from: "ses_sender", summary: "the controller tick" },
    ]);
  });

  test("a cut turn names no delivery of the turn before it", () => {
    const cut = cutTurn([
      card("evt-old", "an older notice"),
      assistant("endTurn"),
      card("evt-new", "a newer notice"),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-new"]);
  });

  test("a cut turn names a person's Send that became the session's own turn, by its Dispatch message id", () => {
    const cut = cutTurn([
      turnMark(false),
      user("do it"),
      turnMark(true),
      handledAttempt("msg-send"),
      personsTurn("rebase onto main first", "msg-send"),
      turnMark(false),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries).toEqual([{ id: "msg-send", from: "dispatch" }]);
  });

  test("a `/skill:` Send, which the host writes as a skill prompt carrying the tag, is named too", () => {
    const cut = cutTurn([
      handledAttempt("msg-skill"),
      {
        type: "custom_message",
        customType: "skill-prompt",
        content: "the skill's prompt",
        display: true,
        tag: "msg-skill",
      },
      turnMark(false),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries).toEqual([{ id: "msg-skill", from: "dispatch" }]);
  });

  // A control, not a red row: it passes before and after the change that names a person's Send,
  // and it holds that change to what it must not name.
  test("control: a tag no handled attempt records names nothing, and a person's Send before the cut turn is not named", () => {
    const cut = cutTurn([
      handledAttempt("msg-old"),
      personsTurn("an older request", "msg-old"),
      assistant("endTurn"),
      personsTurn("not from Dispatch", "some-other-tag"),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries).toEqual([]);
  });

  test("a turn the person interrupted is no cut turn", () => {
    expect(
      cutTurn([user("do it"), card("evt-1", "a notice"), assistant("error", "Interrupted by user")])
    ).toBeUndefined();
  });

  test("a delivery the model never answered is a cut turn", () => {
    const cut = cutTurn([card("evt-wake", "a tick wake")]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-wake"]);
  });

  test("the plugin's own notices are no delivery, and start no turn of their own", () => {
    expect(cutTurn([localNotice()])).toBeUndefined();
    const cut = cutTurn([user("do it"), localNotice(), assistant("toolUse")]);
    expect(cut?.deliveries).toEqual([]);
  });

  test("the note says the turn was cut, names each delivery, and says to read what it did", () => {
    const cut = cutTurn([
      user("do it"),
      card("evt-notice", "the controller tick"),
      assistant("toolUse"),
    ]);
    if (cut === undefined) throw new Error("the turn reads as ended");
    const note = cutTurnNoticeText(cut);
    expect(note).toContain("Your previous turn was interrupted.");
    expect(note).toContain("evt-notice");
    expect(note).toContain("the controller tick");
    expect(note).toContain("Before you start any of that work again, check what it already did.");
    // No delivery is sent again, and the note says so rather than leaving the reader to wonder
    // whether a message it names is still to come.
    expect(note).toContain("No message is sent to you again.");
  });

  test("a cut turn with no delivery still says the turn was cut", () => {
    const cut = cutTurn([user("do it"), assistant("toolUse")]);
    if (cut === undefined) throw new Error("the turn reads as ended");
    expect(cutTurnNoticeText(cut)).toContain("Your previous turn was interrupted.");
  });

  test("a turn a delivery started after a completed turn is cut, and names that delivery", () => {
    const cut = cutTurn([
      user("do it"),
      card("evt-old", "a notice of the completed turn"),
      assistant("endTurn"),
      card("evt-next", "the tick that started the next turn"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-next"]);
  });

  test("a turn a user message started after a completed turn is cut", () => {
    const cut = cutTurn([user("do it"), assistant("endTurn"), user("the next task")]);
    expect(cut).toBeDefined();
    expect(cut?.deliveries).toEqual([]);
  });

  test("a plugin notice after a completed turn starts no turn", () => {
    expect(cutTurn([user("do it"), assistant("endTurn"), localNotice()])).toBeUndefined();
  });

  // A transcript the plugin marks says where each run ended, so a turn whose run has no end mark
  // is cut whatever its last reply carries: the host writes a failed attempt it will retry as it
  // writes any provider error, and marks it retried only once the retry closes.
  test("a turn whose process ended while a provider retry waited is cut", () => {
    const cut = cutTurn([
      turnMark(false),
      user("do it"),
      card("evt-1", "a notice"),
      assistant("error", "Overloaded"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-1"]);
  });

  test("a provider retry that ended leaves its error the end of the turn", () => {
    expect(
      cutTurn([
        turnMark(false),
        user("do it"),
        card("evt-1", "a notice"),
        assistant("error", "Overloaded"),
        turnMark(true),
      ])
    ).toBeUndefined();
  });

  test("a turn waiting in its second retry names the deliveries from before its first", () => {
    const cut = cutTurn([
      turnMark(false),
      card("evt-old", "an older notice"),
      assistant("endTurn"),
      turnMark(true),
      card("evt-start", "the notice that started the turn"),
      turnMark(false),
      assistant("error", "Overloaded"),
      assistant("error", "Overloaded"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-start"]);
  });

  // Oh My Pi can wait inside a retry before it announces one: it redeems a saved usage reset, or
  // waits out another account's short block (`#applyRetryPolicy`,
  // `packages/coding-agent/src/session/turn-recovery.ts:2975-3131` at the pin). The failed
  // attempt is in the transcript then, and nothing after it.
  test("a turn whose process ended in a usage reset's redemption or an account rotation is cut", () => {
    const cut = cutTurn([
      turnMark(false),
      user("do it"),
      card("evt-1", "a notice"),
      assistant("error", "429 usage limit reached for this account"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-1"]);
  });

  // The host saves the failed attempt first, and only then decides on a retry and announces it: a
  // process killed between the two leaves the attempt and nothing after it.
  test("a turn whose process ended after its failed attempt was saved and before its retry is cut", () => {
    const cut = cutTurn([
      turnMark(false),
      user("the first task"),
      assistant("endTurn"),
      turnMark(true),
      user("do it"),
      turnMark(false),
      card("evt-1", "a notice"),
      assistant("error", "Overloaded"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-1"]);
  });

  test("a run whose final reply was saved and whose end mark was not is cut", () => {
    expect(cutTurn([turnMark(false), user("do it"), assistant("endTurn")])).toBeDefined();
  });

  test("a run that started after the last end mark and wrote nothing yet is cut", () => {
    expect(
      cutTurn([
        turnMark(false),
        user("do it"),
        assistant("endTurn"),
        turnMark(true),
        turnMark(false),
      ])
    ).toBeDefined();
  });

  test("a marked run that ended holds no cut turn", () => {
    expect(
      cutTurn([turnMark(false), user("do it"), assistant("endTurn"), turnMark(true), localNotice()])
    ).toBeUndefined();
  });

  test("a delivery after the last end mark starts a cut turn, and is the only one named", () => {
    const cut = cutTurn([
      turnMark(false),
      card("evt-old", "a notice of the run that ended"),
      assistant("endTurn"),
      turnMark(true),
      card("evt-next", "the tick that started the next turn"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-next"]);
  });

  test("a marked transcript's first cut run names no delivery of the unmarked turns before it", () => {
    const cut = cutTurn([
      card("evt-old", "a notice of an unmarked turn"),
      assistant("endTurn"),
      card("evt-next", "the tick that started the first marked run"),
      turnMark(false),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-next"]);
  });

  test("an unmarked transcript whose last reply is a provider error keeps the earlier reading", () => {
    expect(
      cutTurn([user("do it"), card("evt-1", "a notice"), assistant("error", "Overloaded")])
    ).toBeUndefined();
  });

  test("a turn the daemon aborted is no cut turn", () => {
    expect(
      cutTurn([
        user("do it"),
        card("evt-1", "a notice"),
        assistant("aborted", "Request was aborted"),
      ])
    ).toBeUndefined();
  });

  test("a turn the resumed host closed because its process exited in a tool call is cut", () => {
    const cut = cutTurn([
      user("do it"),
      card("evt-1", "a notice"),
      assistant("toolUse"),
      interruptedToolResult(),
      hostClosedTurn(),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-1"]);
  });

  test("a turn the resumed host closed after a completed turn is cut", () => {
    const cut = cutTurn([
      user("do it"),
      assistant("endTurn"),
      card("evt-next", "the tick that started the next turn"),
      hostClosedTurn(),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-next"]);
  });

  test("the host's close of an earlier cut turn is where a later turn starts", () => {
    const cut = cutTurn([
      card("evt-old", "a notice of the earlier cut turn"),
      assistant("toolUse"),
      interruptedToolResult(),
      hostClosedTurn(),
      card("evt-new", "a notice of the later turn"),
      assistant("toolUse"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-new"]);
  });

  test("a delivery after the host's close of an earlier cut turn starts the cut turn", () => {
    const cut = cutTurn([
      card("evt-old", "a notice of the earlier cut turn"),
      assistant("toolUse"),
      interruptedToolResult(),
      hostClosedTurn(),
      card("evt-new", "the tick that started the later turn"),
    ]);
    expect(cut?.deliveries.map((delivery) => delivery.id)).toEqual(["evt-new"]);
  });
});
