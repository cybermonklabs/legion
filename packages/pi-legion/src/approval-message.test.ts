import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import type { LegionClaimApproval } from "@legion/contracts/legion-api";
import {
  APPROVAL_MESSAGE_TYPE,
  type HostAgentRegistry,
  isApprovalMessage,
  messageKey,
} from "@legion/pi-shared/approval-message";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { withSessionAnswers } from "../../pi-envoy/src/session-answers-context";
import { APPROVAL_SENT_ENTRY, type ApprovalSent, createApprovalMessage } from "./approval-message";

// The approval message's state table (rows S1 to S23 of the plan), each row driven on the
// requests the pinned Oh My Pi made in a real root architect's pane (fixture F1,
// test/fixtures/approval-context/, captured by test/approval-context-capture.omp.test.ts) and on
// the gateway's own remote compaction response (F2, remote-compaction-response.json). A row hands
// the module the `context` event's messages, the branch, the live compaction state and the
// `message_end` messages each fixture's timeline recorded, and, for the date reminder, the wire
// body `before_provider_request` saw, with the host's per-call marks put back from the flags the
// recorder wrote beside each wire message. The daemon's route is a stand-in whose answer each row
// sets, its words the daemon's own golden (packages/contracts/fixtures/daemon-api).

const FIXTURES = path.join(import.meta.dir, "..", "test", "fixtures", "approval-context");
const DAEMON_FIXTURES = path.join(
  import.meta.dir,
  "..",
  "..",
  "contracts",
  "fixtures",
  "daemon-api"
);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function record(value: unknown, what: string): Record<string, unknown> {
  if (!isRecord(value)) throw new Error(`${what} is no object`);
  return value;
}

function list(value: unknown, what: string): unknown[] {
  if (!Array.isArray(value)) throw new Error(`${what} is no list`);
  return value;
}

/** Each fixture's timeline, every `<shared:NAME>` written back as the value `shared` holds. */
const timelines = new Map<string, readonly Record<string, unknown>[]>();

function timeline(name: string): readonly Record<string, unknown>[] {
  const known = timelines.get(name);
  if (known !== undefined) return known;
  const parsed = record(
    JSON.parse(readFileSync(path.join(FIXTURES, `${name}.json`), "utf8")),
    `${name}.json`
  );
  const shared = record(parsed.shared, `${name}.json's shared`);
  const resolved = (value: unknown): unknown => {
    if (typeof value === "string") {
      const named = /^<shared:([^>]+)>$/.exec(value);
      return named === null ? value : shared[named[1] ?? ""];
    }
    if (Array.isArray(value)) return value.map(resolved);
    if (isRecord(value)) {
      return Object.fromEntries(
        Object.entries(value).map(([key, field]) => [key, resolved(field)])
      );
    }
    return value;
  };
  const items = list(parsed.timeline, `${name}.json's timeline`).map((item, index) =>
    record(resolved(item), `${name}.json timeline[${index}]`)
  );
  timelines.set(name, items);
  return items;
}

/** A fresh copy of the recorder's `event` item at `index` of the fixture: the module edits what
 *  it is handed. */
function recorded(name: string, index: number, event: string): Record<string, unknown> {
  const item = timeline(name)[index];
  if (item?.event !== event) {
    throw new Error(`${name}.json timeline[${index}] is no ${event} (${String(item?.event)})`);
  }
  return structuredClone(item);
}

/** The description of the host's per-call Symbol (`ai/src/utils/block-symbols.ts`), which a
 *  fixture keeps as a flag beside each wire message, since JSON drops the Symbol. */
const PER_CALL = Symbol("agent.message.perCallContext");

function markedPerCall(message: unknown): boolean {
  return (
    isRecord(message) &&
    Object.getOwnPropertySymbols(message).some(
      (symbol) => symbol.description === PER_CALL.description && message[symbol as never] === true
    )
  );
}

/** The wire body the recorder saw at `index`, with the host's per-call marks put back on its
 *  messages as the host sets them (an own enumerable Symbol property). */
function wireBody(name: string, index: number): Record<string, unknown> {
  const item = recorded(name, index, "before_provider_request");
  const payload = record(item.payload, "the payload");
  const flags = list(item.perCall, "the per-call flags");
  for (const [at, message] of list(payload.messages, "the wire messages").entries()) {
    if (flags[at] === true) {
      Object.defineProperty(message, PER_CALL, {
        value: true,
        enumerable: true,
        writable: true,
        configurable: true,
      });
    }
  }
  return payload;
}

function wireMessages(body: unknown): Record<string, unknown>[] {
  return list(record(body, "the body").messages, "the wire messages").map((message, index) =>
    record(message, `wire message ${index}`)
  );
}

/** A wire message's role and content as the next request must repeat them: `cache_control` set
 *  aside, and a string the same as one text block holding it (Oh My Pi's `cache_control` mark
 *  turns string content into a block, `anthropic.ts:4396-4401`). */
function repeated(message: Record<string, unknown>): unknown {
  const content =
    typeof message.content === "string"
      ? [{ type: "text", text: message.content }]
      : list(message.content, "wire content").map((block) => {
          const { cache_control: _mark, ...rest } = record(block, "a content block");
          return rest;
        });
  return { role: message.role, content };
}

/** Every wire message's content as sent, `cache_control` marks included, in a fixed order. */
function contentsAsSent(body: unknown): string[] {
  return wireMessages(body)
    .map((message) => JSON.stringify(message.content))
    .sort();
}

/** The daemon's golden answer for a root's own spec (`TestApprovalRoute…Golden`). */
const GOLDEN: LegionClaimApproval = JSON.parse(
  readFileSync(path.join(DAEMON_FIXTURES, "claim-approval.json"), "utf8")
);
const GOLDEN_TEXT = GOLDEN.approval?.message ?? "";
const V26: LegionClaimApproval = {
  approval: {
    document: "dispatch://ACME-2694/spec",
    version: 26,
    message: GOLDEN_TEXT.replace("version 24", "version 26").replace("@v24", "@v26"),
  },
};
const NONE: LegionClaimApproval = { approval: null };
const V26_TEXT = V26.approval?.message ?? "";

/** The words the capture's stand-in placed in the date-reminder case, as its timeline notes. */
const DATE_TEXT = String(
  timeline("date-reminder-resume").find(
    (item) => item.step === "the recorder places the approval message"
  )?.text
);
const DATE_V24: LegionClaimApproval = {
  approval: { document: "dispatch://ACME-1/spec", version: 24, message: DATE_TEXT },
};

/** What the stand-in route answers next: an answer, or an error the read throws. */
interface Route {
  answer: LegionClaimApproval | Error;
}

interface Request {
  /** The request's messages as the module left them. */
  readonly messages: unknown[];
  /** The messages the fixture recorded, before the module ran. */
  readonly original: readonly unknown[];
  readonly changed: boolean;
}

/** The model a fixture's request ran on, unless a row names another. */
type Model = NonNullable<SessionContext["model"]>;

/** How a row hands the module one recorded request: the messages edited first (`edit`), on a
 *  model the context names (`model`), or with the live compaction state the roster reports in
 *  place of the one recorded (`live`). */
interface RequestOptions {
  readonly edit?: (messages: unknown[]) => unknown[];
  readonly model?: Model;
  readonly live?: Readonly<Record<string, unknown>>;
}

interface Pane {
  readonly lines: readonly string[];
  readonly reads: () => number;
  /** Every entry on the pane's branch past the fixture's: the inherited ones, then each one the
   *  module appended. */
  readonly entries: readonly Record<string, unknown>[];
  readonly records: () => ApprovalSent[];
  /** Oh My Pi's `session_start` for the pane's session. */
  readonly start: () => Promise<void>;
  /** The fixture's `context` event at `index`: its messages handed to the module at a clock just
   *  past the newest of them. */
  readonly request: (name: string, index: number, options?: RequestOptions) => Promise<Request>;
  /** The fixture's `message_end` at `index`. */
  readonly end: (name: string, index: number) => void;
  /** The fixture's `session_compact` at `index`. */
  readonly compact: (name: string, index: number) => void;
  /** The module's `before_provider_request` on `body`, as the fixture's request at `index` made
   *  it, on the model it recorded. */
  readonly wire: (name: string, index: number, body: Record<string, unknown>) => unknown;
}

/** A root architect's pane over one fixture's session: the module, the stand-in route, the
 *  entries the module appended (on the branch of every later request, after the branch the
 *  fixture recorded), the live session the host's roster lists, and the log. */
function pane(options: {
  readonly session: string;
  readonly route: Route;
  /** Entries an earlier process of the same session appended: a relaunch's branch holds them. */
  readonly inherited?: readonly Record<string, unknown>[];
  /** Sessions besides `session` the claim lookup answers for. */
  readonly alsoClaimed?: readonly string[];
}): Pane {
  const entries: Record<string, unknown>[] = [...(options.inherited ?? [])];
  const lines: string[] = [];
  let clock = 0;
  let reads = 0;
  let live: Record<string, unknown> = { isCompacting: false, compactionSpeculation: "idle" };
  const liveSession = {
    sessionManager: { getSessionId: () => options.session },
    get isCompacting() {
      return live.isCompacting;
    },
    get compactionSpeculation() {
      return live.compactionSpeculation;
    },
  };
  const registry: HostAgentRegistry = {
    global: () => ({
      list: () => [{ id: "Main", kind: "main", session: liveSession, sessionFile: null }],
    }),
  };
  const claimed = new Set([options.session, ...(options.alsoClaimed ?? [])]);
  const claim = (sessionID: string): ClaimCapability | undefined =>
    claimed.has(sessionID)
      ? {
          sessionID,
          tree: "ACME-1",
          issue: "ACME-1",
          role: "architect",
          secret: fixtureSecret("claim"),
          claimToken: "legion-capture-acme-1-architect",
          generation: 1,
        }
      : undefined;
  const daemon: Pick<LegionDaemonClient, "approval"> = {
    approval: async () => {
      reads += 1;
      const { answer } = options.route;
      if (answer instanceof Error) throw answer;
      return answer;
    },
  };
  const approval = createApprovalMessage({
    claim,
    daemon: () => daemon,
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data, id: `appended-${entries.length}` });
    },
    registry,
    now: () => clock,
    log: {
      info: (message) => lines.push(message),
      warn: (message) => lines.push(message),
    },
  });
  const contextOf = (
    session: string,
    agent: unknown,
    branch: readonly unknown[],
    model?: Model
  ): SessionContext => ({
    cwd: "/workspace",
    hasUI: true,
    ...(isRecord(agent) && (agent.kind === "main" || agent.kind === "sub")
      ? { agent: { kind: agent.kind, id: String(agent.id) } }
      : {}),
    ...(model === undefined ? {} : { model }),
    sessionManager: {
      getSessionId: () => session,
      getSessionFile: () => undefined,
      ensureOnDisk: async () => undefined,
      getBranch: () => [...branch, ...entries],
      getEntries: () => [...branch, ...entries],
    },
    setInterval: () => undefined,
    setTimeout: () => undefined,
    ui: { notify: () => undefined },
  });
  const MAIN = { kind: "main", id: "Main" };
  return {
    lines,
    reads: () => reads,
    entries,
    records: () =>
      entries
        .filter((entry) => entry.customType === APPROVAL_SENT_ENTRY)
        .map((entry) => entry.data as ApprovalSent),
    start: async () => {
      clock = 1;
      await approval.sessionStart(contextOf(options.session, MAIN, []));
    },
    request: async (name, index, requestOptions = {}) => {
      const item = recorded(name, index, "context");
      live = { ...record(item.live, "the live state"), ...requestOptions.live };
      const recordedMessages = list(item.messages, "the messages");
      const messages = requestOptions.edit?.(recordedMessages) ?? recordedMessages;
      const original = structuredClone(messages);
      clock =
        Math.max(
          0,
          ...messages.map((message) =>
            isRecord(message) && typeof message.timestamp === "number" ? message.timestamp : 0
          )
        ) + 1;
      const changed = await approval.context(
        messages,
        contextOf(
          String(item.session),
          item.agent,
          list(item.branch, "the branch"),
          requestOptions.model
        )
      );
      return { messages, original, changed };
    },
    end: (name, index) => {
      const item = recorded(name, index, "message_end");
      approval.messageEnd(item.message, contextOf(String(item.session), item.agent, []));
    },
    compact: (name, index) => {
      const item = recorded(name, index, "session_compact");
      approval.sessionCompact(contextOf(String(item.session), item.agent, []));
    },
    wire: (name, index, body) => {
      const item = recorded(name, index, "before_provider_request");
      const model = record(item.model, "the model");
      return approval.beforeProviderRequest(
        body,
        contextOf(String(item.session), item.agent, [], {
          provider: String(model.provider),
          id: String(model.id),
          compat: { supportsMidConversationSystem: model.supportsMidConversationSystem === true },
        })
      );
    },
  };
}

/** The session id of the fixture's first `context` event. */
function sessionOf(name: string): string {
  return String(timeline(name).find((item) => item.event === "context")?.session);
}

function approvalsIn(messages: readonly unknown[]): Record<string, unknown>[] {
  return messages
    .filter(isApprovalMessage)
    .map((message) => record(message, "an approval message"));
}

function textOf(message: unknown): string {
  const [block] = list(record(message, "a message").content, "its content");
  return String(record(block, "its first block").text);
}

/** The answer the fixture's `message_end` at `index` recorded, by its key. */
function answerKey(name: string, index: number): string {
  return messageKey(recorded(name, index, "message_end").message);
}

/** Whether `messages` hold the approval message `copy` right before the message keyed `answer`,
 *  byte for byte. */
function copyBefore(messages: readonly unknown[], answer: string, copy: unknown): boolean {
  const at = messages.findIndex((message) => messageKey(message) === answer);
  return at > 0 && JSON.stringify(messages[at - 1]) === JSON.stringify(copy);
}

test("S1: the first request with no record has the message last, and nothing is recorded yet", async () => {
  const p = pane({ session: sessionOf("task-prompt"), route: { answer: GOLDEN } });
  await p.start();

  const first = await p.request("task-prompt", 4);

  expect(first.changed).toBe(true);
  expect(first.messages.slice(0, -1)).toEqual([...first.original]);
  expect(first.messages.at(-1)).toEqual({
    role: "custom",
    customType: APPROVAL_MESSAGE_TYPE,
    content: [{ type: "text", text: GOLDEN_TEXT }],
    display: false,
    attribution: "agent",
    timestamp: expect.any(Number),
  });
  expect(p.records()).toEqual([]);
});

test("S2: once the answer to the first request is saved, the next request has the message right before that answer, with the same bytes", async () => {
  const p = pane({ session: sessionOf("task-prompt"), route: { answer: GOLDEN } });
  await p.start();
  const first = await p.request("task-prompt", 4);
  const placed = first.messages.at(-1);

  p.end("task-prompt", 8);
  expect(p.records()).toEqual([
    {
      version: 24,
      text: GOLDEN_TEXT,
      timestamp: record(placed, "the placed message").timestamp as number,
      answer: answerKey("task-prompt", 8),
    },
  ]);

  const next = await p.request("task-prompt", 13);
  expect(copyBefore(next.messages, answerKey("task-prompt", 8), placed)).toBe(true);
  expect(approvalsIn(next.messages)).toHaveLength(1);
  expect(next.messages.at(-1)).toEqual(next.original.at(-1));
});

test("S3: an answer Oh My Pi's transform drops (an error turn with thinking alone) records nothing, and the message is last again", async () => {
  const p = pane({ session: sessionOf("retried-request"), route: { answer: GOLDEN } });
  await p.start();
  await p.request("retried-request", 4);

  p.end("retried-request", 10);
  expect(p.records()).toEqual([]);

  const retried = await p.request("retried-request", 13);
  expect(isApprovalMessage(retried.messages.at(-1))).toBe(true);
  expect(textOf(retried.messages.at(-1))).toBe(GOLDEN_TEXT);
  expect(approvalsIn(retried.messages)).toHaveLength(1);
  expect(p.records()).toEqual([]);
});

test("S4: a retried request, with new messages and no answer yet, has the message last, with the bytes of the first attempt", async () => {
  const p = pane({ session: sessionOf("retried-request"), route: { answer: GOLDEN } });
  await p.start();
  const first = await p.request("retried-request", 4);

  // The agent's retry: a new `context` event without the failed turn, before any answer.
  const retried = await p.request("retried-request", 13);

  expect(retried.messages.slice(0, -1)).toEqual([...retried.original]);
  expect(JSON.stringify(retried.messages.at(-1))).toBe(JSON.stringify(first.messages.at(-1)));
  expect(p.records()).toEqual([]);
});

test("S5: a request that holds pi-envoy's new answers message when pi-legion's handler runs has the answers, then the approval last", async () => {
  const p = pane({ session: sessionOf("date-reminder-resume"), route: { answer: GOLDEN } });
  await p.start();

  // pi-envoy's handler ran first: its new people's-answers message ends the request.
  const request = await p.request("date-reminder-resume", 17);

  const answers = request.original.at(-1);
  expect(textOf(answers)).toStartWith("[Dispatch: what people wrote to this session");
  expect(request.messages.at(-2)).toEqual(answers);
  expect(isApprovalMessage(request.messages.at(-1))).toBe(true);
  expect(textOf(request.messages.at(-1))).toBe(GOLDEN_TEXT);
});

test("S6: when pi-legion's handler runs first and pi-envoy's after it, the answers go before the trailing approval, which stays last", async () => {
  const name = "date-reminder-resume";
  const session = sessionOf(name);
  const p = pane({ session, route: { answer: GOLDEN } });
  await p.start();
  const answers = recorded(name, 17, "context");
  const answersText = textOf(list(answers.messages, "the messages").at(-1));

  // The request as pi-legion sees it when it runs first: pi-envoy has added no answers yet.
  const first = await p.request(name, 17, { edit: (messages) => messages.slice(0, -1) });
  expect(isApprovalMessage(first.messages.at(-1))).toBe(true);
  const placed = first.messages.at(-1);

  // Then pi-envoy's handler, over the branch before it recorded the answers it sent.
  const branch = list(answers.branch, "the branch").filter(
    (entry) => !(isRecord(entry) && entry.customType === "envoy-answers-sent")
  );
  const withAnswers = await withSessionAnswers(first.messages, {
    branch: () => branch,
    session: {},
    id: session,
    compacting: () => false,
    read: async () => ({ text: answersText, answered: true }),
    record: () => undefined,
    readFailed: () => undefined,
  });

  expect(withAnswers).toBeDefined();
  const messages = withAnswers ?? [];
  expect(textOf(messages.at(-2))).toBe(answersText);
  expect(messages.at(-1)).toBe(placed);
  expect(approvalsIn(messages)).toHaveLength(1);
});

test("S7: after a soft compaction that summarized the answer, the message is last again, and a new record follows its answer", async () => {
  const name = "compaction-soft";
  const p = pane({ session: sessionOf(name), route: { answer: GOLDEN } });
  await p.start();
  const first = await p.request(name, 4);
  p.end(name, 8);
  const placed = first.messages.at(-1);
  expect(copyBefore((await p.request(name, 13)).messages, answerKey(name, 8), placed)).toBe(true);
  p.end(name, 17);
  expect(copyBefore((await p.request(name, 22)).messages, answerKey(name, 8), placed)).toBe(true);
  p.end(name, 26);
  p.compact(name, 33);

  const after = await p.request(name, 39);

  expect(after.original.some((message) => messageKey(message) === answerKey(name, 8))).toBe(false);
  expect(approvalsIn(after.messages)).toHaveLength(1);
  const again = after.messages.at(-1);
  expect(isApprovalMessage(again)).toBe(true);
  expect(textOf(again)).toBe(GOLDEN_TEXT);
  expect(record(again, "the new placement").timestamp).not.toBe(
    record(placed, "the first placement").timestamp
  );
  p.end(name, 43);
  expect(p.records().map((sent) => sent.answer)).toEqual([answerKey(name, 8), answerKey(name, 43)]);
});

test("S8: after a compaction that kept the answer, the copy stays before its answer and nothing new is placed", async () => {
  const name = "compaction-soft";
  const route: Route = { answer: NONE };
  const p = pane({ session: sessionOf(name), route });
  await p.start();
  await p.request(name, 4);
  p.end(name, 8);
  await p.request(name, 13);
  p.end(name, 17);
  route.answer = GOLDEN;
  const third = await p.request(name, 22);
  const placed = third.messages.at(-1);
  expect(isApprovalMessage(placed)).toBe(true);
  p.end(name, 26);
  p.compact(name, 33);

  const after = await p.request(name, 39);

  expect(copyBefore(after.messages, answerKey(name, 26), placed)).toBe(true);
  expect(approvalsIn(after.messages)).toHaveLength(1);
  expect(after.messages.at(-1)).toEqual(after.original.at(-1));
});

test("S9: after a remote compaction that summarized the answer, the message is last, newer than the compaction block F2 answered", async () => {
  const name = "compaction-remote";
  const p = pane({ session: sessionOf(name), route: { answer: GOLDEN } });
  await p.start();
  await p.request(name, 4);
  p.end(name, 8);
  await p.request(name, 13);
  p.end(name, 17);
  await p.request(name, 22);
  p.end(name, 26);
  await p.request(name, 30);
  await p.request(name, 31);
  p.compact(name, 33);

  const after = await p.request(name, 39);

  // The summary heading the request is the compaction block the gateway answered (F2).
  const response = record(
    JSON.parse(readFileSync(path.join(FIXTURES, "remote-compaction-response.json"), "utf8")),
    "F2"
  );
  const block = String(record(response.response, "F2's response").body)
    .split("\n")
    .filter((line) => line.startsWith("data: "))
    .map((line) => record(JSON.parse(line.slice("data: ".length)), "an SSE event"))
    .find((event) => event.type === "content_block_start");
  const compaction = record(record(block, "the compaction event").content_block, "its block");
  expect(compaction.type).toBe("compaction");
  const summary = record(after.original[0], "the compaction summary");
  expect(summary.role).toBe("compactionSummary");
  expect(record(summary.providerPayload, "its provider payload").content).toBe(compaction.content);

  // Oh My Pi pushes a compaction's file list before the first message newer than the compaction
  // (`anthropic.ts:6303-6321`): the message, stamped at its placement, is newer than every
  // message of the request, the summary included, so it stays after the list and last.
  expect(after.original.some((message) => messageKey(message) === answerKey(name, 8))).toBe(false);
  const placed = after.messages.at(-1);
  expect(isApprovalMessage(placed)).toBe(true);
  expect(approvalsIn(after.messages)).toHaveLength(1);
  const newest = Math.max(
    ...after.original.map((message) => Number(record(message, "a message").timestamp))
  );
  expect(Number(record(placed, "the placement").timestamp)).toBeGreaterThan(newest);
});

test("S10: a fresh session's first request, and a new process's first request in a session holding no copy, have the message last", async () => {
  const fresh = pane({ session: sessionOf("idle-card"), route: { answer: GOLDEN } });
  await fresh.start();
  const card = await fresh.request("idle-card", 4);
  expect(card.messages.slice(0, -1)).toEqual([...card.original]);
  expect(isApprovalMessage(card.messages.at(-1))).toBe(true);

  // Life 2 of the date-reminder case: a new process resumed the session, whose branch holds no
  // copy; it reads the route at its session_start.
  const resumed = pane({ session: sessionOf("date-reminder-resume"), route: { answer: GOLDEN } });
  await resumed.start();
  expect(resumed.reads()).toBe(1);
  const request = await resumed.request("date-reminder-resume", 17);
  expect(request.messages.slice(0, -1)).toEqual([...request.original]);
  expect(isApprovalMessage(request.messages.at(-1))).toBe(true);
});

test("S11: a v24 copy with its answer stays before that answer when the approval is now v26, and a v26 copy is last", async () => {
  const name = "task-prompt";
  const route: Route = { answer: GOLDEN };
  const p = pane({ session: sessionOf(name), route });
  await p.start();
  const first = await p.request(name, 4);
  const v24 = first.messages.at(-1);
  p.end(name, 8);
  route.answer = V26;

  const next = await p.request(name, 13);

  expect(copyBefore(next.messages, answerKey(name, 8), v24)).toBe(true);
  expect(isApprovalMessage(next.messages.at(-1))).toBe(true);
  expect(textOf(next.messages.at(-1))).toBe(V26_TEXT);
  expect(approvalsIn(next.messages).map(textOf)).toEqual([GOLDEN_TEXT, V26_TEXT]);
});

test("S12: a v24 copy with its answer stays after a changes request, nothing new is placed, and after a compaction that summarizes it nothing is left", async () => {
  const name = "compaction-soft";
  const route: Route = { answer: GOLDEN };
  const p = pane({ session: sessionOf(name), route });
  await p.start();
  const first = await p.request(name, 4);
  const placed = first.messages.at(-1);
  p.end(name, 8);
  route.answer = NONE;

  const next = await p.request(name, 13);
  expect(copyBefore(next.messages, answerKey(name, 8), placed)).toBe(true);
  expect(approvalsIn(next.messages)).toHaveLength(1);
  expect(next.messages.at(-1)).toEqual(next.original.at(-1));
  p.end(name, 17);
  await p.request(name, 22);
  p.end(name, 26);
  p.compact(name, 33);

  const after = await p.request(name, 39);
  expect(after.changed).toBe(false);
  expect(after.messages).toEqual([...after.original]);
});

test("S13: a failed read leaves the request decided on the last answer, and the next request reads again", async () => {
  const name = "retried-request";
  const route: Route = { answer: GOLDEN };
  const p = pane({ session: sessionOf(name), route });
  await p.start();
  expect(p.reads()).toBe(1);
  route.answer = new Error("the daemon did not answer within 800 ms");

  // The task prompt entered after the session_start read, so this request reads, and fails.
  const first = await p.request(name, 4);
  expect(p.reads()).toBe(2);
  expect(isApprovalMessage(first.messages.at(-1))).toBe(true);
  expect(textOf(first.messages.at(-1))).toBe(GOLDEN_TEXT);
  p.end(name, 10);

  // The agent's retry holds no entry newer than that read: it reads again only because the read
  // failed.
  route.answer = NONE;
  const retried = await p.request(name, 13);
  expect(p.reads()).toBe(3);
  expect(retried.changed).toBe(false);
  expect(approvalsIn(retried.messages)).toEqual([]);
});

test("S14: a placed message is the route's text byte for byte", async () => {
  const name = "task-prompt";
  const p = pane({ session: sessionOf(name), route: { answer: GOLDEN } });
  await p.start();

  const first = await p.request(name, 4);
  p.end(name, 8);
  const next = await p.request(name, 13);

  expect(textOf(first.messages.at(-1))).toBe(GOLDEN_TEXT);
  expect(approvalsIn(next.messages).map(textOf)).toEqual([GOLDEN_TEXT]);
});

test("S15: a side turn, the handoff prompt and a compaction's own requests rebuild each copy whose answer they hold, and place nothing new", async () => {
  // A side turn (`/btw`), with v26 newly standing.
  {
    const name = "side-turn";
    const route: Route = { answer: GOLDEN };
    const p = pane({ session: sessionOf(name), route });
    await p.start();
    const placed = (await p.request(name, 4)).messages.at(-1);
    p.end(name, 8);
    route.answer = V26;
    const reads = p.reads();
    const side = await p.request(name, 13);
    expect(copyBefore(side.messages, answerKey(name, 8), placed)).toBe(true);
    expect(approvalsIn(side.messages)).toHaveLength(1);
    expect(side.messages.at(-1)).toEqual(side.original.at(-1));
    // A side turn waits on no read of the route.
    expect(p.reads()).toBe(reads);
  }
  // The handoff prompt, with v26 newly standing.
  {
    const name = "handoff-prompt";
    const route: Route = { answer: GOLDEN };
    const p = pane({ session: sessionOf(name), route });
    await p.start();
    const placed = (await p.request(name, 4)).messages.at(-1);
    p.end(name, 8);
    await p.request(name, 13);
    p.end(name, 17);
    route.answer = V26;
    const handoff = await p.request(name, 20);
    expect(copyBefore(handoff.messages, answerKey(name, 8), placed)).toBe(true);
    expect(approvalsIn(handoff.messages)).toHaveLength(1);
    expect(handoff.messages.at(-1)).toEqual(handoff.original.at(-1));
  }
  // A remote compaction's two conversions: up to its cut, then the whole conversation.
  {
    const name = "compaction-remote";
    const route: Route = { answer: GOLDEN };
    const p = pane({ session: sessionOf(name), route });
    await p.start();
    const placed = (await p.request(name, 4)).messages.at(-1);
    p.end(name, 8);
    await p.request(name, 13);
    p.end(name, 17);
    await p.request(name, 22);
    p.end(name, 26);
    route.answer = V26;
    const cut = await p.request(name, 30);
    const whole = await p.request(name, 31);
    for (const conversion of [cut, whole]) {
      expect(copyBefore(conversion.messages, answerKey(name, 8), placed)).toBe(true);
      expect(approvalsIn(conversion.messages).map(textOf)).toEqual([GOLDEN_TEXT]);
      expect(conversion.messages.at(-1)).toEqual(conversion.original.at(-1));
    }
    // The compaction keeps as many messages of the whole conversion as the cut's holds
    // (`agent-session.ts:3556-3567`): exactly the cut's messages, the rebuilt copy among them.
    expect(whole.messages.slice(0, cut.messages.length).map(messageKey)).toEqual(
      cut.messages.map(messageKey)
    );
  }
});

// A compaction's own request is told apart by what the request itself shows (the handoff prompt,
// or a conversion that ends before the branch's last message), not by the session's compaction
// state alone, so a request of the loop made while a speculative compaction runs in the
// background is placed as any other.
test("a request of the loop made while a speculative compaction runs still gets its placement", async () => {
  const name = "compaction-remote";
  const route: Route = { answer: GOLDEN };
  const p = pane({ session: sessionOf(name), route });
  await p.start();
  const placed = (await p.request(name, 4)).messages.at(-1);
  p.end(name, 8);
  await p.request(name, 13);
  p.end(name, 17);
  route.answer = V26;

  const third = await p.request(name, 22, { live: { compactionSpeculation: "running" } });

  expect(copyBefore(third.messages, answerKey(name, 8), placed)).toBe(true);
  expect(textOf(third.messages.at(-1))).toBe(V26_TEXT);
});

test("S16: on a model whose flag is off the message is placed, and one log line names the session's model, with no second line", async () => {
  const name = "task-prompt";
  const p = pane({ session: sessionOf(name), route: { answer: GOLDEN } });
  await p.start();
  const sonnet: Model = {
    provider: "anthropic",
    id: "claude-sonnet-5",
    compat: { supportsMidConversationSystem: false },
  };

  const first = await p.request(name, 4, { model: sonnet });
  expect(isApprovalMessage(first.messages.at(-1))).toBe(true);
  p.end(name, 8);
  const next = await p.request(name, 13, { model: sonnet });
  expect(approvalsIn(next.messages)).toHaveLength(1);

  const named = p.lines.filter((line) => line.includes("anthropic/claude-sonnet-5"));
  expect(named).toHaveLength(1);
  expect(named[0]).toContain(sessionOf(name));
});

test("S17: a task subagent's request is returned unchanged", async () => {
  const name = "task-subagent";
  const sub = String(recorded(name, 32, "context").session);
  // Even where a claim answered for the subagent's session, its `agent.kind` is `sub`.
  const p = pane({ session: sessionOf(name), route: { answer: GOLDEN }, alsoClaimed: [sub] });
  await p.start();
  const reads = p.reads();

  const request = await p.request(name, 32);

  expect(request.changed).toBe(false);
  expect(request.messages).toEqual([...request.original]);
  expect(p.reads()).toBe(reads);
});

/** A compaction case's run: the route stands from the request at `from` on, every request and
 *  saved answer of the fixture before the compaction, the compaction, and the first request after
 *  it, with the copy placed at `from`. */
async function compactionRun(
  name: string,
  steps: readonly { readonly request: number; readonly answer?: number }[],
  from: number,
  compaction: number | undefined,
  after: number
): Promise<{ readonly placed: unknown; readonly answer: string; readonly after: Request }> {
  const route: Route = { answer: NONE };
  const p = pane({ session: sessionOf(name), route });
  await p.start();
  let placed: unknown;
  let answer = "";
  for (const step of steps) {
    if (step.request === from) route.answer = GOLDEN;
    const request = await p.request(name, step.request);
    if (step.request === from) {
      placed = request.messages.at(-1);
      expect(isApprovalMessage(placed)).toBe(true);
    }
    if (step.answer !== undefined) {
      if (step.request === from) answer = answerKey(name, step.answer);
      p.end(name, step.answer);
    }
  }
  if (compaction !== undefined) p.compact(name, compaction);
  return { placed, answer, after: await p.request(name, after) };
}

test("S18: after a snapcompact, the message is last again where it summarized the answer, and the copy stays where it kept it", async () => {
  const name = "compaction-snapcompact";
  const steps = [
    { request: 4, answer: 8 },
    { request: 13, answer: 17 },
    { request: 22, answer: 26 },
    { request: 31, answer: 35 },
  ];
  const summarized = await compactionRun(name, steps, 4, 39, 45);
  expect(
    summarized.after.original.some((message) => messageKey(message) === summarized.answer)
  ).toBe(false);
  expect(isApprovalMessage(summarized.after.messages.at(-1))).toBe(true);
  expect(approvalsIn(summarized.after.messages)).toHaveLength(1);

  const kept = await compactionRun(name, steps, 31, 39, 45);
  expect(copyBefore(kept.after.messages, kept.answer, kept.placed)).toBe(true);
  expect(approvalsIn(kept.after.messages)).toHaveLength(1);
});

test("S18: after a handoff compaction, the message is last again where it summarized the answer, and the copy stays where it kept it", async () => {
  const name = "compaction-handoff";
  // The handoff's own request (19) rebuilds the copies and places nothing.
  const steps = [{ request: 4, answer: 8 }, { request: 13, answer: 17 }, { request: 19 }];
  const summarized = await compactionRun(name, steps, 4, 22, 28);
  expect(
    summarized.after.original.some((message) => messageKey(message) === summarized.answer)
  ).toBe(false);
  expect(isApprovalMessage(summarized.after.messages.at(-1))).toBe(true);
  expect(approvalsIn(summarized.after.messages)).toHaveLength(1);

  const kept = await compactionRun(name, steps, 13, 22, 28);
  expect(copyBefore(kept.after.messages, kept.answer, kept.placed)).toBe(true);
  expect(approvalsIn(kept.after.messages)).toHaveLength(1);
});

test("S18: after a shake, which keeps every message, the copy stays before its answer", async () => {
  const name = "compaction-shake";
  // Shake commits no compaction entry (`session-maintenance.ts:1036-1110`), so no
  // session_compact: the request after it reads the route for its new prompt alone.
  const steps = [
    { request: 4, answer: 8 },
    { request: 10, answer: 13 },
    { request: 15, answer: 18 },
  ];
  const kept = await compactionRun(name, steps, 4, undefined, 27);
  expect(copyBefore(kept.after.messages, kept.answer, kept.placed)).toBe(true);
  expect(approvalsIn(kept.after.messages)).toHaveLength(1);
  expect(kept.after.messages.at(-1)).toEqual(kept.after.original.at(-1));
});

/** Life 2 of the date-reminder case, the card's request placed: the pane and the wire body the
 *  host built for that request, its reminder of today's date last. */
async function cardRequest(): Promise<{
  readonly p: Pane;
  readonly body: Record<string, unknown>;
}> {
  const name = "date-reminder-resume";
  const p = pane({ session: sessionOf(name), route: { answer: DATE_V24 } });
  await p.start();
  const request = await p.request(name, 17);
  expect(textOf(request.messages.at(-1))).toBe(DATE_TEXT);
  return { p, body: wireBody(name, 18) };
}

test("S19: the card's request, which ends with the answers message, the new placement and Oh My Pi's date reminder, leaves with the next request's messages before its answer", async () => {
  const name = "date-reminder-resume";
  const { p, body } = await cardRequest();
  const sentBefore = wireMessages(body);
  expect(sentBefore.at(-1)?.role).toBe("system");
  expect(String(sentBefore.at(-1)?.content)).toStartWith("<system-reminder>\nToday: ");
  expect(sentBefore.at(-2)?.role).toBe("user");

  const sent = p.wire(name, 18, body);

  // The next request: the tool round, whose answer to the card's request is its 9th message.
  const next = wireMessages(wireBody(name, 24));
  expect(next[8]?.role).toBe("assistant");
  const messages = wireMessages(sent);
  expect(messages.map(repeated)).toEqual(next.slice(0, 8).map(repeated));
  // The reminder went right after the last message that is not per-call, as a user message, and
  // the placement is last, as `system`.
  expect(messages.at(-3)?.content).toBe(sentBefore.at(-1)?.content);
  expect(messages.at(-3)?.role).toBe("user");
  expect(messages.at(-1)?.role).toBe("system");
  expect(messages.at(-1)?.content).toBe(DATE_TEXT);
  // No `cache_control` changed, and every message kept its per-call mark.
  expect(contentsAsSent(sent)).toEqual(contentsAsSent(body));
  expect(messages.map(markedPerCall)).toEqual([true, false, false, true, false, true, true, true]);
});

test("S20: the same body again, as a retry sends it, gives the same body", async () => {
  const name = "date-reminder-resume";
  const { p, body } = await cardRequest();
  const sent = p.wire(name, 18, body);

  const again = p.wire(name, 18, wireBody(name, 18));

  expect(JSON.stringify(again)).toBe(JSON.stringify(sent));
  expect(wireMessages(again).map(markedPerCall)).toEqual(wireMessages(sent).map(markedPerCall));
  // A body the handler already gave is left as it is.
  expect(p.wire(name, 18, record(sent, "the body sent"))).toBeUndefined();
});

test("S21: a body whose last two messages are not a new placement and the reminder is unchanged", async () => {
  const name = "date-reminder-resume";
  // A later request: the tool round after the card's.
  {
    const { p } = await cardRequest();
    expect(p.wire(name, 24, wireBody(name, 24))).toBeUndefined();
  }
  // A request with no reminder: the card's request without its trailing reminder.
  {
    const { p, body } = await cardRequest();
    const messages = wireMessages(body);
    const withoutReminder = { ...body, messages: messages.slice(0, -1) };
    expect(p.wire(name, 18, withoutReminder)).toBeUndefined();
  }
  // A side turn while a placement waits for its answer.
  {
    const side = "side-turn";
    const p = pane({ session: sessionOf(side), route: { answer: GOLDEN } });
    await p.start();
    await p.request(side, 4);
    expect(p.wire(side, 14, wireBody(side, 14))).toBeUndefined();
  }
});

test("S22: a relaunch after a changes request, and after a later approval, rebuilds each placed copy from its record with the same bytes", async () => {
  const name = "date-reminder-resume";
  const session = sessionOf(name);
  // Life 1 placed v24 and saved its answer.
  const life1 = pane({ session, route: { answer: GOLDEN } });
  await life1.start();
  const placed = (await life1.request(name, 4)).messages.at(-1);
  life1.end(name, 8);
  expect(life1.records()).toHaveLength(1);
  const answer = answerKey(name, 8);

  // A relaunch after a changes request.
  const changed = pane({ session, route: { answer: NONE }, inherited: life1.entries });
  await changed.start();
  const afterChange = await changed.request(name, 17);
  expect(copyBefore(afterChange.messages, answer, placed)).toBe(true);
  expect(approvalsIn(afterChange.messages)).toHaveLength(1);
  expect(afterChange.messages.at(-1)).toEqual(afterChange.original.at(-1));

  // A relaunch after a later approval.
  const approved = pane({ session, route: { answer: V26 }, inherited: life1.entries });
  await approved.start();
  const afterApproval = await approved.request(name, 17);
  expect(copyBefore(afterApproval.messages, answer, placed)).toBe(true);
  expect(approvalsIn(afterApproval.messages).map(textOf)).toEqual([GOLDEN_TEXT, V26_TEXT]);
  expect(textOf(afterApproval.messages.at(-1))).toBe(V26_TEXT);
});

test("S23: a relaunch into a session that holds a copy of the standing version with its answer rebuilds it at its place and places nothing new", async () => {
  const name = "date-reminder-resume";
  const session = sessionOf(name);
  const life1 = pane({ session, route: { answer: GOLDEN } });
  await life1.start();
  const placed = (await life1.request(name, 4)).messages.at(-1);
  life1.end(name, 8);

  const relaunched = pane({ session, route: { answer: GOLDEN }, inherited: life1.entries });
  await relaunched.start();
  const request = await relaunched.request(name, 17);

  expect(copyBefore(request.messages, answerKey(name, 8), placed)).toBe(true);
  expect(approvalsIn(request.messages)).toHaveLength(1);
  expect(request.messages.at(-1)).toEqual(request.original.at(-1));
  expect(relaunched.records()).toEqual(life1.records());
});
