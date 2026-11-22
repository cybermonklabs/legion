import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LegionOwedResponse } from "@legion/contracts/legion-api";
import { z } from "zod";
import { LOCAL_ENVOY_NOTICE } from "./envoy-notice";
import {
  assistantRanTool,
  assistantText,
  inboundOf,
  LAST_MESSAGE_LIMIT,
  phaseStallEntryFrom,
  quietReminderText,
  recordedPhaseStall,
  stepPhaseStall,
  turnEndOf,
} from "./phase-stall";

const assistant = (...content: unknown[]) => ({ role: "assistant", content });

// Oh My Pi's `message_end` hands `toolCall` (the shape packages/pi-shared/src/agent-stream.ts
// reads); Claude Code names it `tool_use`. Matching one name only once had a working Oh My Pi
// worker reported to its architect as stalled.
test("a tool call under either host's block name is a tool call the turn ran", () => {
  expect(
    assistantRanTool(
      assistant(
        { type: "text", text: "Reading the file." },
        { type: "toolCall", id: "call_1", name: "read", arguments: { path: "main.py" } }
      )
    )
  ).toBe(true);
  expect(
    assistantRanTool(
      assistant({ type: "tool_use", id: "toolu_1", name: "read", input: { path: "main.py" } })
    )
  ).toBe(true);
});

test("a message with no tool-call block, or one that is not the assistant's, ran no tool", () => {
  expect(assistantRanTool(assistant({ type: "text", text: "Done." }))).toBe(false);
  expect(assistantRanTool(assistant({ type: "thinking", thinking: "Hmm." }))).toBe(false);
  expect(
    assistantRanTool({
      role: "user",
      content: [{ type: "toolCall", id: "call_1", name: "read", arguments: {} }],
    })
  ).toBe(false);
  expect(assistantRanTool(undefined)).toBe(false);
});

test("an assistant message's text is its text blocks joined, and nothing of its tool calls", () => {
  expect(
    assistantText(
      assistant(
        { type: "text", text: "Pushed." },
        { type: "toolCall", id: "call_1", name: "bash", arguments: { command: "ls" } },
        { type: "text", text: "WAITING: CI." }
      )
    )
  ).toBe("Pushed.\n\nWAITING: CI.");
});

// Oh My Pi stops a reply the provider refused with `error` and words it in `errorMessage`
// (`AssistantMessage` in `src/omp-host.d.ts`), which is how the turn that ends the run reads at
// `session_stop` and at `agent_end`.
test("a reply the provider refused classifies as the refusal, not as what the worker said", () => {
  expect(
    turnEndOf({
      ...assistant({ type: "text", text: "" }),
      stopReason: "error",
      errorMessage: "Overloaded: the model is busy",
    })
  ).toEqual({ kind: "refused", message: "Overloaded: the model is busy" });
  // A refusal the host could not word still has to read as a refusal.
  expect(turnEndOf({ ...assistant(), stopReason: "error" })).toEqual({
    kind: "refused",
    message: "error",
  });
  expect(
    turnEndOf({ ...assistant({ type: "text", text: "Pushed." }), stopReason: "stop" })
  ).toEqual({ kind: "settle", lastAssistantText: "Pushed." });
});

// Reminding a session the provider is refusing buys another refused turn, which is what left the
// implementer of dispatch://INFRA-264 reminded for ever.
test("a refused turn earns no reminder and moves nothing", () => {
  const active = { state: "active", count: 2 } as const;
  expect(stepPhaseStall(active, { kind: "refused", message: "Overloaded" })).toEqual({
    state: active,
  });
  const waiting = { state: "waiting", count: 1 } as const;
  expect(stepPhaseStall(waiting, { kind: "refused", message: "Overloaded" })).toEqual({
    state: waiting,
  });
});

test("a record an earlier build wrote, with no last message, still reads", () => {
  expect(phaseStallEntryFrom({ state: "active", count: 2, reported: false })).toEqual({
    state: "active",
    count: 2,
    reported: false,
  });
});

// The release before INFRA-3136 recorded a model of the phase (`open`, `closed`), and the build
// before that its own two states (`quiet`, `text-call`). Every one of them reads as the rhythm's
// `active`, the first two with their count: what is owed is the daemon's answer now, so a
// `closed` record resumed on is reminded only when the daemon says the claim owes work.
test("every state an earlier release recorded reads as the rhythm it now is", () => {
  expect(phaseStallEntryFrom({ state: "open", count: 2, reported: true })).toEqual({
    state: "active",
    count: 2,
    reported: true,
  });
  expect(phaseStallEntryFrom({ state: "closed", count: 0, reported: false })).toEqual({
    state: "active",
    count: 0,
    reported: false,
  });
  expect(phaseStallEntryFrom({ state: "waiting", count: 1 })).toEqual({
    state: "waiting",
    count: 1,
    reported: false,
  });
  for (const state of ["quiet", "text-call"]) {
    expect(phaseStallEntryFrom({ state, count: 4 })).toEqual({
      state: "active",
      count: 0,
      reported: false,
    });
  }
  expect(phaseStallEntryFrom({ state: "awaiting-update", count: 0 })).toBeUndefined();
  expect(phaseStallEntryFrom({ state: 7 })).toBeUndefined();
});

test("a record's last message reads back no longer than the notice quotes", () => {
  expect(
    phaseStallEntryFrom({ state: "active", count: 1, reported: false, lastMessage: "Pushed." })
  ).toEqual({ state: "active", count: 1, reported: false, lastMessage: "Pushed." });
  const long = "x".repeat(LAST_MESSAGE_LIMIT + 40);
  expect(phaseStallEntryFrom({ state: "active", count: 1, lastMessage: long })?.lastMessage).toBe(
    long.slice(0, LAST_MESSAGE_LIMIT)
  );
  expect(phaseStallEntryFrom({ state: "active", count: 1, lastMessage: 7 })).toEqual({
    state: "active",
    count: 1,
    reported: false,
  });
});

// A message from outside the session moves the rhythm's count and nothing else of the machine:
// news (an Envoy delivery, a person's direct message) and a daemon prompt (its task, the update
// round's) each start the count again, and neither says anything of what the claim owes. The two
// differ in whether they end a wait the worker declared, which is the reminder's to report
// (`src/phase-reminder.ts`), not the rhythm's. A message's kind says which it is: a user message
// is a daemon prompt, and an Envoy card is news under the card's own key, where it carries one.
test("news and a daemon prompt each start the count again, and the reminder's own message is neither", () => {
  expect(inboundOf({ role: "user", content: "Run the update." })).toEqual({ kind: "prompt" });
  expect(inboundOf({ role: "custom", customType: "envoy-message", details: {} })).toEqual({
    kind: "news",
  });
  expect(
    inboundOf({
      role: "custom",
      customType: "envoy-message",
      details: { event_id: "evt-1", dedupe_key: "publish.6f2a9c1e4b7d40a8b3e5c9d1f0a2b4c6" },
    })
  ).toEqual({ kind: "news", deliveryKey: "publish.6f2a9c1e4b7d40a8b3e5c9d1f0a2b4c6" });
  expect(
    inboundOf({ role: "custom", customType: "envoy-message", details: LOCAL_ENVOY_NOTICE })
  ).toBeUndefined();
  expect(inboundOf({ role: "custom", customType: "legion-phase-reminder" })).toBeUndefined();
  expect(inboundOf({ role: "assistant", content: [] })).toBeUndefined();
  for (const kind of ["news", "prompt"] as const) {
    for (const state of ["active", "waiting", "awaiting-update"] as const) {
      expect(stepPhaseStall({ state, count: 3 }, { kind })).toEqual({
        state: { state: "active", count: 0 },
      });
    }
  }
});

// INFRA-484: the worker has answered the daemon's update message (`ready_for_update`), so it is to
// start nothing until the daemon comes back to it. A reminder here would continue the answering
// turn in the same run: the work that continuation did would go with the pod, and the daemon
// would see no turn end.
test("a worker that answered the update message stands in awaiting-update, where a settle earns no reminder", () => {
  const answered = stepPhaseStall({ state: "active", count: 2 }, { kind: "update-answered" });
  expect(answered).toEqual({ state: { state: "awaiting-update", count: 0 } });
  expect(
    stepPhaseStall(answered.state, { kind: "settle", lastAssistantText: "Pushed and answered." })
  ).toEqual({ state: { state: "awaiting-update", count: 0 } });
  // The answering tool call itself, which the host reports from its own hook, leaves the wait as
  // it is, whichever of the two the host applies first.
  expect(stepPhaseStall(answered.state, { kind: "tool-call" })).toEqual({
    state: { state: "awaiting-update", count: 0 },
  });
  // What ends this wait is the daemon: the round replaces the pod, and the task it held behind
  // the round arrives in the process that comes back as its own prompt.
  expect(stepPhaseStall(answered.state, { kind: "prompt" })).toEqual({
    state: { state: "active", count: 0 },
  });
  // Nothing reminds into this wait, so `reminded` can only be a timer armed before the answer;
  // it is counted and the rhythm is active again, as it is out of background work's wait.
  expect(stepPhaseStall(answered.state, { kind: "reminded" })).toEqual({
    state: { state: "active", count: 1 },
  });
});

// The record holds no `awaiting-update`: a record is read only by the process a relaunch starts,
// and that relaunch is the round the wait belonged to ending, so what is restored is the plain
// wait the reminder times out.
test("awaiting-update is recorded as the waiting it is a kind of", () => {
  expect(recordedPhaseStall("awaiting-update")).toBe("waiting");
  for (const state of ["active", "waiting"] as const) {
    expect(recordedPhaseStall(state)).toBe(state);
  }
  expect(phaseStallEntryFrom({ state: "waiting", count: 0 })).toEqual({
    state: "waiting",
    count: 0,
    reported: false,
  });
});

// A worker whose turn ends while its own background job runs is not stalled: that job's
// result starts the next turn, and a reminder in between would be telling a working worker that
// it has done nothing. It buys the quiet wait, which is what bounds it: a job that never finishes
// is a wait the host's waiting delay ends.
test("a settle with the worker's own background work running earns no reminder", () => {
  expect(
    stepPhaseStall(
      { state: "active", count: 2 },
      { kind: "settle", lastAssistantText: "Started the build.", backgroundWork: true }
    )
  ).toEqual({ state: { state: "waiting", count: 2 } });
});

test("a settle with no background work left running earns a reminder, as it always did", () => {
  for (const backgroundWork of [false, undefined]) {
    expect(
      stepPhaseStall(
        { state: "active", count: 2 },
        { kind: "settle", lastAssistantText: "Started the build.", backgroundWork }
      )
    ).toEqual({ state: { state: "active", count: 2 }, remind: true });
  }
});

// dispatch://INFRA-2040: a WAITING line buys nothing. A worker that waits on something outside
// its own work declares the wait (`declare_wait`), and the daemon's answer says while it stands;
// a reply that only says so earns what any other reply earns.
test("a settle whose reply ends with a WAITING line earns the reminder a settle without it earns", () => {
  for (const lastAssistantText of [
    "WAITING: CI on the pull request",
    "Pushed the change.\n**WAITING** on the reviewer.",
    "> WAITING for the architect's answer",
  ]) {
    expect(
      stepPhaseStall({ state: "active", count: 2 }, { kind: "settle", lastAssistantText })
    ).toEqual(
      stepPhaseStall(
        { state: "active", count: 2 },
        { kind: "settle", lastAssistantText: "Pushed the change." }
      )
    );
  }
});

// The record carries the wait the plugin knows of and every report of news the daemon has not
// answered, so a relaunched process reports news against that wait and sends those reports
// again. A malformed report is dropped rather than refusing the record, and a record an earlier
// build wrote, with neither key, still reads.
test("a record carries the standing wait and the unsent reports of news, and reads them back", () => {
  expect(
    phaseStallEntryFrom({
      state: "active",
      count: 1,
      reported: false,
      waitId: "wait-1",
      unsentNews: [
        { waitId: "wait-1", deliveryKey: "publish.0123456789abcdef0123456789abcdef" },
        { waitId: "wait-1" },
        { waitId: "" },
        { deliveryKey: "publish.0123456789abcdef0123456789abcdef" },
        { waitId: "wait-1", deliveryKey: "" },
        "wait-1",
      ],
    })
  ).toEqual({
    state: "active",
    count: 1,
    reported: false,
    waitId: "wait-1",
    unsentNews: [
      { waitId: "wait-1", deliveryKey: "publish.0123456789abcdef0123456789abcdef" },
      { waitId: "wait-1" },
      { waitId: "wait-1" },
    ],
  });
  expect(phaseStallEntryFrom({ state: "active", count: 1, waitId: 7, unsentNews: "none" })).toEqual(
    { state: "active", count: 1, reported: false }
  );
});

test("a settle in a quiet wait earns none, and the wait ends on a reminder", () => {
  expect(
    stepPhaseStall(
      { state: "waiting", count: 2 },
      { kind: "settle", lastAssistantText: "Done.", backgroundWork: true }
    )
  ).toEqual({ state: { state: "waiting", count: 2 } });
  expect(stepPhaseStall({ state: "waiting", count: 2 }, { kind: "reminded" })).toEqual({
    state: { state: "active", count: 3 },
  });
});

/** `packages/contracts/fixtures/daemon-api/merger-closed-note.json`: one tree, the closed gate
 * that holds its merger as the owed answer gives it, and the note the daemon builds from them
 * (`mergerClosedNote` in `packages/daemon/internal/workflow/gate.go`). */
const MergerClosedNote = z.object({
  tree: z.string().min(1),
  hold: LegionOwedResponse.shape.hold.unwrap(),
  note: z.string().min(1),
});

// D15 of dispatch://INFRA-2040's plan: the reminder a held merger gets for a tool call written as
// text ends with the daemon's own closed note, word for word. The daemon's test
// (TestMergerClosedGateNoteIsTheContractFixturesNote in `packages/daemon/internal/workflow`) holds
// its sentence to the same fixture, so a change to the sentence on one side alone fails a test.
test("the reminder during a hold ends with the daemon's own closed note, word for word", () => {
  const fixture = MergerClosedNote.parse(
    JSON.parse(
      readFileSync(
        path.resolve(
          import.meta.dir,
          "../../contracts/fixtures/daemon-api/merger-closed-note.json"
        ),
        "utf8"
      )
    )
  );
  const call = '<invoke name="legion"><parameter name="op">handoff_complete</parameter>';

  expect(quietReminderText(call, fixture.tree, fixture)).toEndWith(` ${fixture.note}`);
  expect(quietReminderText("Pushed.", fixture.tree, fixture)).toBeUndefined();
});
