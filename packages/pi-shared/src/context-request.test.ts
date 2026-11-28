import { expect, test } from "bun:test";
import { SESSION_ANSWERS_MARKER } from "@legion/envoy-client/session-answers";
import {
  ADDED_MESSAGE_TIMESTAMP,
  type HostAgentRegistry,
  isHandoffPrompt,
  isSideTurn,
  messageKey,
  nextConversion,
  sessionCompacting,
} from "./context-request";

// Message shapes as Oh My Pi hands them to a `context` handler.
const ask = { role: "user", content: [{ type: "text", text: "Fix the bug." }], timestamp: 1 };
const reply = {
  role: "assistant",
  content: [{ type: "text", text: "Fixed." }],
  stopReason: "stop",
  timestamp: 4,
};
const readResult = {
  role: "toolResult",
  toolCallId: "call-1",
  toolName: "read",
  content: [{ type: "text", text: "export const x = 1;" }],
  isError: false,
  timestamp: 3,
};
const grepResult = {
  role: "toolResult",
  toolCallId: "call-2",
  toolName: "grep",
  content: [{ type: "text", text: "x.ts:3: the bug" }],
  isError: false,
  timestamp: 6,
};

// What the pinned Oh My Pi adds after the conversation for a side turn (`runEphemeralTurn`) and for
// the handoff, as the binary builds them.
const sideTurnNotice = {
  role: "developer",
  content: [
    {
      type: "text",
      text: "<system-reminder>\nEphemeral side-channel turn; reuses current conversation context.\nTool catalog attached only to keep prompt cache warm; tools NOT available this turn.\nDo NOT emit tool calls; reply plain text only. Tool calls discarded without execution.\n</system-reminder>\n",
    },
  ],
  attribution: "agent",
  timestamp: 10,
};
const sidePrompt = {
  role: "user",
  content: [{ type: "text", text: "<btw>\nQuestion:\nWhat is up?\n</btw>" }],
  attribution: "agent",
  timestamp: 11,
};
const handoffPrompt = {
  role: "user",
  content: [
    {
      type: "text",
      text: "<critical>\nWrite a handoff document for another instance of yourself.\nThe handoff MUST be sufficient for seamless continuation without access to this conversation.\n</critical>",
    },
  ],
  attribution: "agent",
  timestamp: 12,
};

// pi-envoy's people's-answers message as its handler adds it after a request's last message: a
// user message stamped `ADDED_MESSAGE_TIMESTAMP` whose first block is the list.
const answers = {
  role: "user",
  content: [{ type: "text", text: `${SESSION_ANSWERS_MARKER}\n\n- ACME-2694: approved.` }],
  timestamp: ADDED_MESSAGE_TIMESTAMP,
};

test("a side turn ends with the host's prompt after the host's notice", () => {
  const conversation = [ask, reply];
  expect(isSideTurn([...conversation, sideTurnNotice, sidePrompt])).toBe(true);
  // A /btw follow-up: the earlier question and answer sit between the notice and the prompt.
  const earlier = { ...sidePrompt, timestamp: 8 };
  const earlierAnswer = { ...reply, content: [{ type: "text", text: "Not much." }], timestamp: 9 };
  expect(isSideTurn([...conversation, sideTurnNotice, earlier, earlierAnswer, sidePrompt])).toBe(
    true
  );
});

test("a host reminder before a host prompt in the main loop is no side turn", () => {
  const reminder = {
    role: "developer",
    content: [{ type: "text", text: "<system-reminder>\nFinish the todos.\n</system-reminder>" }],
    attribution: "agent",
    timestamp: 5,
  };
  expect(isSideTurn([ask, reply, reminder, { ...sidePrompt, timestamp: 6 }])).toBe(false);
});

test("the handoff prompt is the host's: the same words from the person are their prompt", () => {
  expect(isHandoffPrompt([ask, reply, handoffPrompt])).toBe(true);
  expect(isHandoffPrompt([ask, reply, { ...handoffPrompt, attribution: "user" }])).toBe(false);
});

test("tool results with one timestamp are told apart by their call ids", () => {
  const sameTime = { ...grepResult, timestamp: readResult.timestamp };
  expect(messageKey(readResult)).toBe("toolResult@3#call-1");
  expect(messageKey(sameTime)).toBe("toolResult@3#call-2");
  expect(messageKey(reply)).toBe("assistant@4");
});

// pi-envoy's handler, run first while the session compacts, adds an answers message after the last
// message of a remote compaction's first conversion and none to its repeat.
test("where a request ends is read without the answers messages pi-envoy added after its last message", () => {
  const entries = [ask, reply].map((message, index) => ({
    type: "message",
    id: `entry-${index}`,
    message,
  }));
  const first = nextConversion(undefined, [ask, reply, answers], entries);
  expect(first.kind).toBe("new");
  expect(first.conversion).toEqual({ key: "entry-1\nuser@1\nassistant@4", afterCut: false });
  expect(nextConversion(first.conversion, [ask, reply], entries).kind).toBe("repeat");
  // A person's own message that quotes the marker carries the time it was sent: the request ends
  // there, at a message the branch does not hold yet.
  const quoted = { ...answers, timestamp: 7 };
  expect(nextConversion(first.conversion, [ask, reply, quoted], entries)).toEqual({
    conversion: { key: undefined, afterCut: false },
    kind: "new",
  });
});

/** A host roster that lists `sessions` (each a live session's getters beside its id), as
 *  `host.AgentRegistry` lists them. */
function roster(...sessions: readonly Record<string, unknown>[]): HostAgentRegistry {
  return {
    global: () => ({
      list: () =>
        sessions.map(({ id, ...getters }) => ({
          id: String(id),
          kind: "main" as const,
          session: { sessionManager: { getSessionId: () => String(id) }, ...getters },
          sessionFile: null,
        })),
    }),
  };
}

test("a session compacts while its live session compacts or runs a speculative compaction", () => {
  const registry = roster(
    { id: "ses_compacting", isCompacting: true },
    { id: "ses_speculating", isCompacting: false, compactionSpeculation: "running" },
    { id: "ses_armed", isCompacting: false, compactionSpeculation: "armed" },
    { id: "ses_idle", isCompacting: false }
  );
  expect(sessionCompacting(registry, "ses_compacting")).toBe(true);
  expect(sessionCompacting(registry, "ses_speculating")).toBe(true);
  expect(sessionCompacting(registry, "ses_armed")).toBe(false);
  expect(sessionCompacting(registry, "ses_idle")).toBe(false);
  // A session the roster does not list, or a host without the roster, is not compacting.
  expect(sessionCompacting(registry, "ses_other")).toBe(false);
  expect(sessionCompacting(undefined, "ses_compacting")).toBe(false);
});
