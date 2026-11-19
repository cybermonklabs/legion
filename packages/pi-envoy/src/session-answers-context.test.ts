import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { SessionAnswer } from "@legion/contracts";
import { bearerCredential } from "@legion/envoy-client/bearer";
import {
  readSessionAnswers,
  SESSION_ANSWERS_MARKER,
  type SessionAnswersRead,
  sessionAnswersFailureLine,
  sessionAnswersWords,
} from "@legion/envoy-client/session-answers";
import {
  ask,
  branchOf,
  grepCall,
  grepResult,
  ITEM,
  LIST,
  noBranch,
  readCall,
  readResult,
  requestOf,
  skill,
} from "../test/session-answers-requests";
import { type CarriedAnswers, withSessionAnswers } from "./session-answers-context";

// Message shapes as Oh My Pi hands them to a `context` handler.
const list = { role: "user", content: [{ type: "text", text: LIST }], timestamp: 0 };
const reply = {
  role: "assistant",
  content: [{ type: "text", text: "Fixed." }],
  stopReason: "stop",
  timestamp: 4,
};

// What Oh My Pi 18.12.2 adds after the conversation for a side turn (`runEphemeralTurn`) and for
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

// A delivered Envoy message and a branch summary as the session's branch holds them, and the
// context messages Oh My Pi 18.12.2 makes of them for a compaction (`yu`, `hm`): role `custom` and
// `branchSummary`, each timestamped `new Date(entry.timestamp).getTime()`.
const DELIVERED_AT = "2026-10-27T10:00:05.123Z";
const deliveryEntry = {
  type: "custom_message",
  id: "c1",
  customType: "envoy-message",
  content: "envoy:\n  notice: CI finished",
  display: true,
  timestamp: DELIVERED_AT,
};
const delivery = {
  role: "custom",
  customType: "envoy-message",
  content: "envoy:\n  notice: CI finished",
  display: true,
  timestamp: new Date(DELIVERED_AT).getTime(),
};
const deliveryReply = {
  role: "assistant",
  content: [{ type: "text", text: "CI is green." }],
  stopReason: "stop",
  timestamp: new Date(DELIVERED_AT).getTime() + 1_000,
};
const SUMMARIZED_AT = "2026-10-27T10:00:06.456Z";
const summaryEntry = {
  type: "branch_summary",
  id: "b1",
  summary: "The other branch tried a rewrite.",
  fromId: "e0",
  timestamp: SUMMARIZED_AT,
};
const branchSummary = {
  role: "branchSummary",
  summary: "The other branch tried a rewrite.",
  fromId: "e0",
  timestamp: new Date(SUMMARIZED_AT).getTime(),
};

/** Where the fixtures' reads start their 72-hour window: every fixture item is inside it. */
const SINCE = "2026-10-24T12:00:00.000Z";

/** What `readSessionAnswers` gives with `text`: the text, what the people wrote in each item it
 *  names (its time, its head and its words, keyed as `sessionAnswersWords` keys them), and where
 *  the read's window starts. The failure line names no item. */
function carry(text: string | undefined, since = SINCE): CarriedAnswers | undefined {
  if (text === undefined) return undefined;
  const items = Array.from(
    text.matchAll(/^(\d{4}-\S+) · (.+)\n(.+)$/gm),
    (match): SessionAnswer => ({
      ...ITEM,
      at: match[1] ?? "",
      id: match[2] ?? "",
      text: match[3] ?? "",
    })
  );
  return { text, words: sessionAnswersWords(items), since };
}

/** A read that records each call and answers `text`. */
function reading(text: string | undefined): {
  readonly read: () => Promise<CarriedAnswers | undefined>;
  readonly calls: () => number;
} {
  let calls = 0;
  return {
    read: async () => {
      calls += 1;
      return carry(text);
    },
    calls: () => calls,
  };
}

test("the list goes after the request's last message, as a message of its own", async () => {
  const { read } = reading(LIST);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch))).toEqual([
    skill,
    ask,
    list,
  ]);
});

// The tool result quotes the marker: it is conversation, not a list another copy appended.
test("after a tool round the list follows the tool result, which keeps its own content", async () => {
  const { read } = reading(LIST);
  const messages = [skill, ask, readCall, readResult];
  expect(
    await withSessionAnswers(messages, requestOf(read, branchOf(ask, readCall, readResult)))
  ).toEqual([...messages, list]);
});

// What a session's requests carry over turns with the same people's answers. A list sits after the
// request's last message only on the request that first shows an item, and stays there on the
// requests after it, so the model never reads a list with nothing new in it as the newest turn.
const LATER = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
const later = { role: "user", content: [{ type: "text", text: LATER }], timestamp: 0 };

/** A read that answers each of `reads` in turn (a text as `carry` gives it; undefined: no text
 *  and no words), then the last one again. */
function readingEach(
  ...reads: (string | CarriedAnswers | undefined)[]
): () => Promise<CarriedAnswers | undefined> {
  let call = 0;
  return async () => {
    const next = reads[Math.min(call++, reads.length - 1)];
    return typeof next === "string" ? carry(next) : next;
  };
}

test("a list with no new item stays where it first went: the request still ends with its own last message", async () => {
  const session = {};
  const read = readingEach(LIST);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
  ]);
  // Two tool rounds later, with nothing new: the list is where the first request put it.
  const turn = [skill, ask, readCall, readResult, grepCall, grepResult];
  for (let request = 0; request < 3; request += 1) {
    const carried = await withSessionAnswers([...turn], requestOf(read, noBranch, session));
    expect(carried).toEqual([skill, ask, list, readCall, readResult, grepCall, grepResult]);
    expect(carried?.at(-1)).toBe(grepResult);
  }
});

test("a new item goes after the request's last message, and stays there on the requests after it", async () => {
  const session = {};
  const read = readingEach(LIST, LATER);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    later,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([...turn, later, grepCall, grepResult]);
});

// Every byte of a request before its last cache breakpoint has to repeat, or the provider writes
// the whole conversation behind the changed byte to its cache again. The list's header names the
// read that produced it ("As read at …; this read failed: …"), which differs from one request to
// the next whenever a read crosses its 800 ms deadline, so a kept place keeps its bytes too. An
// item the 72-hour window has left behind is no change either.
const BOB = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.`;
/** A window that has left alice's answer (10:00) behind and still holds bob's message (11:00). */
const PAST_ALICE = "2026-10-27T10:30:00.000Z";

test("a list that only lost items to the window, or changed its header, keeps its place and its bytes", async () => {
  const session = {};
  const cached = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.`;
  const read = readingEach(LATER, carry(BOB, PAST_ALICE), carry(cached, PAST_ALICE));
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    later,
    readCall,
    readResult,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, later, readCall, readResult, grepCall, grepResult]);
});

// An item gone from the read while the window still holds its time was retracted, or removed by
// Dispatch: a kept list must not go on showing it as current.
test("an item gone from the read while its time is inside the window moves the list", async () => {
  const session = {};
  const read = readingEach(LATER, LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    list,
  ]);
});

// The one change that does move the list also re-renders it, so a new item is never hidden behind
// the bytes a kept place holds.
test("an item written after a kept place moves the list, with the text of that read", async () => {
  const session = {};
  const stale = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
  const read = readingEach(LIST, stale, LATER);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([...turn, grepCall, grepResult, later]);
});

// A read gives no text and no words when a server with no answers route has nothing cached, or
// when Dispatch is off. Taking the placed list out of the conversation changes the request from
// its place on, as changing its bytes would, and the next read that answers from the cache puts it
// back: the pair alternates, and every request pays for it.
test("a read with no text and no words leaves a placed list where it is", async () => {
  const session = {};
  const read = readingEach(LIST, undefined, LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list, readCall, readResult, grepCall, grepResult]);
});

// While a compaction may be converting the conversation, a list before its cut would push a
// message out of what the compaction keeps, so a read with no text keeps no list there either.
test("a read with no text while the session compacts, or right after a compaction's cut, carries no list", async () => {
  const session = {};
  const read = readingEach(LIST, undefined);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(
    await withSessionAnswers([...turn], requestOf(read, noBranch, session, true))
  ).toBeUndefined();
  // A remote compaction's cut, then its whole conversion, with the session not reading as
  // compacting: the request right after the cut keeps no list either.
  const branch = branchOf(ask, readCall, readResult, grepCall, grepResult);
  const whole = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...turn], requestOf(read, branch, session))).toBeUndefined();
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session))).toBeUndefined();
  // After that, the list is back where it went first.
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
    grepCall,
    grepResult,
  ]);
});

const FAILURE = sessionAnswersFailureLine("Dispatch did not answer within 800 ms");
const failureLine = { role: "user", content: [{ type: "text", text: FAILURE }], timestamp: 0 };

// The failure line says the list could not be read. The bytes of a placed list standing in for it
// would read as a read that answered, so a failed read, whatever made it fail, is a change: it
// goes after the last message, and the list goes there again once a read gives it.
test("the failure line where a list is placed goes after the last message, and so does the list after it", async () => {
  const session = {};
  const read = readingEach(LIST, FAILURE, LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    failureLine,
  ]);
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    ...tools,
    list,
  ]);
});

// Two failure lines both say the list could not be read and differ only in why, as two reads'
// notes on one list do, so the first keeps its place and its bytes.
test("a failure line after a placed failure line keeps that one's place and bytes", async () => {
  const session = {};
  const read = readingEach(
    FAILURE,
    sessionAnswersFailureLine("an earlier read is still waiting on Dispatch")
  );
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    failureLine,
  ]);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    failureLine,
    readCall,
    readResult,
  ]);
});

// A read that gives no text did not fail: Dispatch answered, with nothing for the session. A placed
// failure line would go on telling the model that reads fail, so it goes, and its place with it.
test("a read with no text takes a placed failure line out, and its place with it", async () => {
  const session = {};
  const read = readingEach(FAILURE, undefined, undefined, FAILURE);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    failureLine,
  ]);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toBeUndefined();
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toBeUndefined();
  // The next failed read goes after the last message, not back where the first one sat.
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    ...tools,
    failureLine,
  ]);
});

test("a list whose place left the conversation, as a compaction leaves it, goes after the last message again", async () => {
  const session = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  // The compaction summarized the question the list followed.
  const compacted = [skill, branchSummary, readCall, readResult];
  expect(await withSessionAnswers([...compacted], requestOf(read, noBranch, session))).toEqual([
    ...compacted,
    list,
  ]);
  expect(
    await withSessionAnswers(
      [...compacted, grepCall, grepResult],
      requestOf(read, noBranch, session)
    )
  ).toEqual([...compacted, list, grepCall, grepResult]);
});

test("a second copy of the extension finds the list where the first put it, and reads nothing", async () => {
  const session = {};
  const first = reading(LIST);
  await withSessionAnswers([skill, ask], requestOf(first.read, noBranch, session));
  const carried = await withSessionAnswers(
    [skill, ask, readCall, readResult],
    requestOf(first.read, noBranch, session)
  );
  expect(carried?.at(-1)).toBe(readResult);
  // The host hands the second copy's handler the array the first returned.
  const second = reading(LIST);
  expect(
    await withSessionAnswers(carried ?? [], requestOf(second.read, noBranch, session))
  ).toBeUndefined();
  expect(second.calls()).toBe(0);
});

// A speculative compaction runs beside the main loop, so the main loop's request can come between
// the compaction's cut and its whole conversion. While the session compacts, every list goes after
// the last message, so the whole conversion's first messages are the cut's, as the compaction
// keeps them.
test("a main-loop request between a compaction's cut and its whole conversion leaves the compaction its messages", async () => {
  const session = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const branch = branchOf(ask, readCall, readResult, grepCall, grepResult);
  const cut = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...cut], requestOf(read, branch, session, true))).toBe(
    undefined
  );
  const whole = [skill, ask, readCall, readResult, grepCall, grepResult];
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session, true))).toEqual([
    ...whole,
    list,
  ]);
  const converted = await withSessionAnswers([...whole], requestOf(read, branch, session, true));
  expect(converted?.slice(0, cut.length)).toEqual(cut);
  // Once the compaction is over, the list is back where it went first.
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
    grepCall,
    grepResult,
  ]);
});

test("a retry after an aborted tool call, whose results the host wrote, carries the list", async () => {
  const { read } = reading(LIST);
  const aborted = {
    role: "assistant",
    content: [{ type: "toolCall", id: "call-9", name: "read", arguments: { path: "y.ts" } }],
    stopReason: "aborted",
    timestamp: 5,
  };
  const synthetic = {
    role: "toolResult",
    toolCallId: "call-9",
    toolName: "read",
    content: [{ type: "text", text: "Aborted." }],
    isError: true,
    timestamp: 6,
  };
  expect(
    await withSessionAnswers([skill, ask], requestOf(read, branchOf(ask, aborted, synthetic)))
  ).toEqual([skill, ask, list]);
});

test("each session on one sessionManager keeps its own list's place", async () => {
  const manager = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, manager, false, "ses_a"));
  const other = { ...ask, content: [{ type: "text", text: "Another task." }], timestamp: 50 };
  await withSessionAnswers([skill, other], requestOf(read, noBranch, manager, false, "ses_b"));
  expect(
    await withSessionAnswers(
      [skill, ask, readCall, readResult],
      requestOf(read, noBranch, manager, false, "ses_a")
    )
  ).toEqual([skill, ask, list, readCall, readResult]);
});

test("tool results with one timestamp are told apart by their call ids", async () => {
  const session = {};
  const read = readingEach(LIST);
  const sameTime = { ...grepResult, timestamp: readResult.timestamp };
  const turn = [skill, ask, readCall, readResult];
  await withSessionAnswers([...turn], requestOf(read, noBranch, session));
  expect(
    await withSessionAnswers([...turn, grepCall, sameTime], requestOf(read, noBranch, session))
  ).toEqual([...turn, list, grepCall, sameTime]);
});

test("the failure line goes where the list would", async () => {
  const line = sessionAnswersFailureLine("Dispatch did not answer within 800 ms");
  const { read } = reading(line);
  expect(await withSessionAnswers([ask], requestOf(read, noBranch))).toEqual([
    ask,
    { role: "user", content: [{ type: "text", text: line }], timestamp: 0 },
  ]);
});

test("no text to carry adds no message", async () => {
  const { read, calls } = reading(undefined);
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(1);
});

// An older copy of the extension put the list after the last message of every request. This copy
// reads nothing for that request, adds no second list, and moves the one there where it belongs.
test("a request an older copy already gave the list gets no second one and no read; its list is placed as this copy's", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  expect(await withSessionAnswers([skill, ask, list], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
  ]);
  // The next request, with the same list after its last message, gets it after the first one's.
  expect(
    await withSessionAnswers(
      [skill, ask, readCall, readResult, list],
      requestOf(read, noBranch, session)
    )
  ).toEqual([skill, ask, list, readCall, readResult]);
  const failure = {
    role: "user",
    content: [{ type: "text", text: sessionAnswersFailureLine("database is down") }],
    timestamp: 0,
  };
  expect(await withSessionAnswers([ask, failure], requestOf(read, noBranch))).toEqual([
    ask,
    failure,
  ]);
  expect(calls()).toBe(0);
});

// A list an older copy placed carries no item fingerprints, so only its text is left to compare,
// and every difference but the line naming the read counts as something new. Here the read note's
// own length pushed an item past the reader's UTF-16 code-unit cap, so the list is re-rendered after the
// last message: a prompt-cache miss, and never a person's words held back. Reading that text more
// closely to spare the miss swallowed an item twice (see `showsNew`).
const CAPPED = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.\n\n1 older item left out to stay under 8,000 UTF-16 code units: run \`dispatch answers\` to read every one in full.`;
const cappedList = { role: "user", content: [{ type: "text", text: CAPPED }], timestamp: 0 };

// The list `LIST` as a copy renders it from its cache when a read fails, and the same list for a
// read that failed for another reason.
const CACHED = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
const cachedList = { role: "user", content: [{ type: "text", text: CACHED }], timestamp: 0 };
const WAITING = CACHED.replace(
  "Dispatch did not answer within 800 ms.",
  "an earlier read is still waiting on Dispatch."
);
const waitingList = {
  role: "user",
  content: [{ type: "text", text: WAITING }],
  timestamp: 0,
};

test("an older copy's list the cap shortened is re-rendered, not kept", async () => {
  const session = {};
  const fresh = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
  const freshList = { role: "user", content: [{ type: "text", text: fresh }], timestamp: 0 };
  // No read of this copy's own: the older copy's list is what gets placed.
  const { read, calls } = reading(LIST);
  await withSessionAnswers([skill, ask, freshList], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(
    await withSessionAnswers([...turn, cappedList], requestOf(read, noBranch, session))
  ).toEqual([...turn, cappedList]);
  expect(calls()).toBe(0);
});

// The one difference an older copy's list may show and keep its place and bytes: the line naming
// the read (`withoutReadLine`).
test("an older copy's list that differs only in the line naming its read keeps its place and bytes", async () => {
  const session = {};
  const { read, calls } = reading(LIST);
  await withSessionAnswers([skill, ask, cappedList], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  const sameItems = {
    role: "user",
    content: [
      {
        type: "text",
        text: CAPPED.replace("2026-10-27T11:00:00.000Z;", "2026-10-27T11:05:00.000Z;"),
      },
    ],
    timestamp: 0,
  };
  expect(
    await withSessionAnswers([...turn, sameItems], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, cappedList, readCall, readResult]);
  expect(calls()).toBe(0);
});

// A list an older copy placed has no words, so a read whose window holds no item cannot tell its
// aged-out items from a retracted one. Kept, a retracted only answer would stay in every request
// while an older copy runs beside this one, so such a read takes that list out (`keptWithoutText`).
test("a read with no text that gives words takes out a list an older copy placed", async () => {
  const session = {};
  const read = readingEach({ text: undefined, words: [], since: SINCE });
  // No read of this copy's own: the older copy's list is what gets placed.
  expect(await withSessionAnswers([skill, ask, list], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
  ]);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toBeUndefined();
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toBeUndefined();
});

// An older copy's list rendered from its cache names the read that failed. Kept behind placed
// bytes that differ, that line leaves the request, and the failure it names is told to
// `unshownFailure` instead, once, as this copy's own failed reads are. An older copy's list with
// no read line names no failure and tells nothing. An older copy's failure line comes from a read
// that failed, so unlike a list with no read line it lets no failure be told again.
test("an older copy's list naming a failed read, kept behind the placed bytes, tells that failure once", async () => {
  const session = {};
  const told: string[] = [];
  const request = {
    ...requestOf(readingEach(LIST), noBranch, session),
    unshownFailure: (reason: string) => {
      told.push(reason);
    },
  };
  // The request carries the placed list's own failure, so nothing is told.
  expect(await withSessionAnswers([skill, ask, waitingList], request)).toEqual([
    skill,
    ask,
    waitingList,
  ]);
  const turn = [skill, ask, readCall, readResult];
  const kept = [skill, ask, waitingList, readCall, readResult];
  // Kept, as it differs from the placed bytes only in their read line, and names no failure.
  expect(await withSessionAnswers([...turn, list], request)).toEqual(kept);
  expect(told).toStrictEqual([]);
  expect(await withSessionAnswers([...turn, cachedList], request)).toEqual(kept);
  expect(told).toStrictEqual(["Dispatch did not answer within 800 ms."]);
  // The failure line moves, the cached list after it moves, and a failure kept behind that list
  // is not told: no read has answered since the first was told.
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools, failureLine], request)).toEqual([
    ...tools,
    failureLine,
  ]);
  const tail = [...tools, reply];
  expect(await withSessionAnswers([...tail, cachedList], request)).toEqual([...tail, cachedList]);
  expect(await withSessionAnswers([...tail, waitingList], request)).toEqual([...tail, cachedList]);
  expect(told).toStrictEqual(["Dispatch did not answer within 800 ms."]);
});

// Every renderer writes a list with no read line only for a read Dispatch answered, so an older
// copy's list with no read line is an answered read: when an older copy decides every request of
// a session, the next failure after it is told too.
test("an older copy's list with no read line lets the next failure it names be told again", async () => {
  const session = {};
  const told: string[] = [];
  const request = {
    ...requestOf(readingEach(LIST), noBranch, session),
    unshownFailure: (reason: string) => {
      told.push(reason);
    },
  };
  expect(await withSessionAnswers([skill, ask, list], request)).toEqual([skill, ask, list]);
  const turn = [skill, ask, readCall, readResult];
  const tools = [...turn, grepCall, grepResult];
  for (const older of [cachedList, list, waitingList]) {
    await withSessionAnswers([...turn, older], request);
  }
  expect(await withSessionAnswers([...tools, list], request)).toEqual([
    skill,
    ask,
    list,
    ...tools.slice(2),
  ]);
  expect(told).toEqual([
    "Dispatch did not answer within 800 ms.",
    "an earlier read is still waiting on Dispatch.",
  ]);
});

// A list placed from a cached read names that read's failure. A later read that fails the same
// way renders the same text, and the request then carries this read's failure itself, so nothing
// is told; a read that fails for another reason is told once.
test("a cached list placed with this read's own text tells nothing; another failure after it is told once", async () => {
  const session = {};
  const told: string[] = [];
  // `CACHED` and `WAITING` show the items of `LIST`, so their reads give its words.
  const words = carry(LIST)?.words;
  const timedOut = {
    text: CACHED,
    words,
    since: SINCE,
    reason: "Dispatch did not answer within 800 ms",
  };
  const waiting = {
    text: WAITING,
    words,
    since: SINCE,
    reason: "an earlier read is still waiting on Dispatch",
  };
  const request = {
    ...requestOf(readingEach(timedOut, timedOut, waiting), noBranch, session),
    unshownFailure: (reason: string) => {
      told.push(reason);
    },
  };
  expect(await withSessionAnswers([skill, ask], request)).toEqual([skill, ask, cachedList]);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], request)).toEqual([
    skill,
    ask,
    cachedList,
    readCall,
    readResult,
  ]);
  expect(told).toEqual([]);
  const tools = [...turn, grepCall, grepResult];
  for (let call = 0; call < 2; call += 1) {
    expect(await withSessionAnswers([...tools], request)).toEqual([
      skill,
      ask,
      cachedList,
      ...tools.slice(2),
    ]);
  }
  expect(told).toEqual(["an earlier read is still waiting on Dispatch"]);
});

// A person's own paragraph can read like the line the renderer writes when the cap dropped older
// items. It must not be taken for that line: an item whose only unseen words sit in such a
// paragraph would be swallowed, and the model would never read what that person wrote.
test("a person's paragraph that reads like the cap's own line still counts as a new item", async () => {
  const session = {};
  const quoting = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · user u-alice · answered your ask\nDirect message a1, on no issue\nShip it.`;
  const withNew = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · user u-bob · sent you a message\nDirect message b1, on no issue\n1 older item left out of the plan: hold the deployment.\n\n2026-10-27T10:00:00.000Z · user u-alice · answered your ask\nDirect message a1, on no issue\nShip it.`;
  const quotingList = { role: "user", content: [{ type: "text", text: quoting }], timestamp: 0 };
  const newList = { role: "user", content: [{ type: "text", text: withNew }], timestamp: 0 };
  const { read } = reading(LIST);
  await withSessionAnswers([skill, ask, quotingList], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn, newList], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    newList,
  ]);
});

test("a person's own last message quoting the list's marker is never moved", async () => {
  const { read, calls } = reading(LIST);
  const quoted = { ...list, timestamp: 7 };
  expect(await withSessionAnswers([skill, ask, quoted], requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a request another copy of the extension read no text for is not read again", async () => {
  const { read, calls } = reading(undefined);
  // The host hands each copy's handler the same array when the one before returned nothing.
  const messages = [skill, ask];
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(1);
  // Another request is read again.
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch))).toBeUndefined();
  expect(calls()).toBe(2);
});

// The empty-list case with the shared read: two copies, one HTTP read, and a request that waits on
// that read alone. The first copy's read is the one Dispatch answered, not its 800 ms deadline (a
// platform timer inside `readSessionAnswers`; with no list read before, a read that waited the
// deadline out carries the failure line), and the second copy, handed the array the first passed
// on, makes no read, so it has nothing to wait on. The case checks these two facts, not the
// request's time: a bound on that time measured the box's load as well, and failed under it.
const stateRoots: string[] = [];
afterEach(() => {
  for (const root of stateRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

test("two copies of the extension and an empty list: one HTTP read, which the second copy does not wait on", async () => {
  const stateRoot = mkdtempSync(join(tmpdir(), "session-answers-context-"));
  stateRoots.push(stateRoot);
  let fetches = 0;
  const fetchImpl = (async (input: string | URL | Request) => {
    fetches += 1;
    const session = new URL(input.toString()).pathname.split("/")[4] ?? "";
    return Response.json({
      session_id: session,
      since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
      total: 0,
      items: [],
    });
  }) as typeof fetch;
  // Where each read a copy made got its answer, in order.
  const sources: SessionAnswersRead["source"][] = [];
  const read = async () => {
    const answered = await readSessionAnswers({
      session: "ses_two_copies_empty",
      dispatch: {
        enabled: true,
        url: "http://dispatch.test",
        credential: bearerCredential("token"),
      },
      env: { DISPATCH_STATE_DIR: stateRoot },
      fetchImpl,
    });
    sources.push(answered.source);
    return carry(answered.text);
  };
  const messages = [skill, ask];

  // Copy A's handler, then copy B's on the array A passed on.
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toBeUndefined();

  // One read, the first copy's, which Dispatch answered.
  expect(sources).toEqual(["fresh"]);
  // And that read sent one HTTP request.
  expect(fetches).toBe(1);
});

test("a list quoted earlier in the conversation does not switch the list off", async () => {
  const { read } = reading(LIST);
  const messages = [list, ask, readCall, readResult];
  expect(await withSessionAnswers(messages, requestOf(read, noBranch))).toEqual([
    ...messages,
    list,
  ]);
});

test("a side turn keeps its prompt last and is not read for", async () => {
  const { read, calls } = reading(LIST);
  const conversation = [skill, ask, reply];
  const branch = branchOf(ask, reply);
  expect(
    await withSessionAnswers([...conversation, sideTurnNotice, sidePrompt], requestOf(read, branch))
  ).toBeUndefined();
  // A /btw follow-up: the earlier question and answer sit between the notice and the prompt.
  const earlier = { ...sidePrompt, timestamp: 8 };
  const earlierAnswer = { ...reply, content: [{ type: "text", text: "Not much." }], timestamp: 9 };
  expect(
    await withSessionAnswers(
      [...conversation, sideTurnNotice, earlier, earlierAnswer, sidePrompt],
      requestOf(read, branch)
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a host reminder before a host prompt in the main loop is no side turn", async () => {
  const { read } = reading(LIST);
  const reminder = {
    role: "developer",
    content: [{ type: "text", text: "<system-reminder>\nFinish the todos.\n</system-reminder>" }],
    attribution: "agent",
    timestamp: 5,
  };
  const messages = [skill, ask, reply, reminder, { ...sidePrompt, timestamp: 6 }];
  expect(await withSessionAnswers(messages, requestOf(read, branchOf(ask, reply)))).toEqual([
    ...messages,
    list,
  ]);
});

test("the handoff prompt stays last and is not read for", async () => {
  const { read, calls } = reading(LIST);
  expect(
    await withSessionAnswers(
      [skill, ask, reply, handoffPrompt],
      requestOf(read, branchOf(ask, reply))
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
  // The same words from the person are their prompt.
  const typed = { ...handoffPrompt, attribution: "user" };
  expect(
    await withSessionAnswers([skill, ask, reply, typed], requestOf(read, branchOf(ask, reply)))
  ).toEqual([skill, ask, reply, typed, list]);
});

// Anthropic remote compaction converts the conversation up to its cut, counts it, and keeps that
// many messages of the whole conversation's conversion.
test("a conversion that ends before the branch's last message is not read for; the whole conversation is", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(ask, readCall, readResult, reply);
  expect(
    await withSessionAnswers([skill, ask, readCall, readResult], requestOf(read, branch))
  ).toBeUndefined();
  expect(calls()).toBe(0);
  const whole = [skill, ask, readCall, readResult, reply];
  expect(await withSessionAnswers(whole, requestOf(read, branch))).toEqual([...whole, list]);
  expect(calls()).toBe(1);
});

test("a retry after a failed reply still carries the list", async () => {
  const { read } = reading(LIST);
  const failed = { ...reply, content: [], stopReason: "error", timestamp: 5 };
  const aborted = { ...reply, content: [], stopReason: "aborted", timestamp: 6 };
  expect(
    await withSessionAnswers([skill, ask], requestOf(read, branchOf(ask, failed, aborted)))
  ).toEqual([skill, ask, list]);
});

// The compaction's messages come from every branch entry, a delivered message and a branch summary
// among them, and its cut may fall right after either. Every Envoy delivery with `triggerTurn` is
// a delivered message the reply follows.
test("a conversion that ends at a delivered message with a reply after it is not read for", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(ask, reply, deliveryEntry, deliveryReply);
  expect(
    await withSessionAnswers([skill, ask, reply, delivery], requestOf(read, branch))
  ).toBeUndefined();
  expect(calls()).toBe(0);
  const whole = [skill, ask, reply, delivery, deliveryReply];
  expect(await withSessionAnswers(whole, requestOf(read, branch))).toEqual([...whole, list]);
});

test("a conversion that ends at a branch summary with a message after it is not read for", async () => {
  const { read, calls } = reading(LIST);
  const branch = branchOf(summaryEntry, ask, reply);
  expect(await withSessionAnswers([skill, branchSummary], requestOf(read, branch))).toBe(undefined);
  expect(calls()).toBe(0);
});

test("a delivered message after the request's last message is a later one", async () => {
  const { read, calls } = reading(LIST);
  expect(
    await withSessionAnswers(
      [skill, ask, reply],
      requestOf(read, branchOf(ask, reply, deliveryEntry))
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a main-loop request that ends at a delivered message, the branch's last, carries the list", async () => {
  const { read } = reading(LIST);
  const messages = [skill, ask, reply, delivery];
  expect(
    await withSessionAnswers(messages, requestOf(read, branchOf(ask, reply, deliveryEntry)))
  ).toEqual([...messages, list]);
});

// A remote compaction that keeps no tail converts the same messages twice, each ending at the
// branch's last message. With no list on the second, it keeps no list whatever the first carried.
test("a compaction that keeps no tail: the repeat of the conversion before it is not read for", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  const branch = branchOf(ask, reply, deliveryEntry);
  const conversation = [skill, ask, reply, delivery];
  expect(
    await withSessionAnswers([...conversation], requestOf(read, branch, session, true))
  ).toEqual([...conversation, list]);
  expect(
    await withSessionAnswers([...conversation], requestOf(read, branch, session, true))
  ).toBeUndefined();
  expect(calls()).toBe(1);
});

test("a repeat carries the list when the session is not compacting, the branch grew, or the session is another", async () => {
  const { read, calls } = reading(LIST);
  const conversation = [skill, ask, reply, delivery];
  const branch = branchOf(ask, reply, deliveryEntry);
  const idle = {};
  await withSessionAnswers([...conversation], requestOf(read, branch, idle));
  expect(await withSessionAnswers([...conversation], requestOf(read, branch, idle))).toEqual([
    ...conversation,
    list,
  ]);
  // A failed reply joined the branch: a retry, which carries the list.
  const failed = { ...reply, content: [], stopReason: "error", timestamp: delivery.timestamp + 1 };
  const retried = {};
  await withSessionAnswers([...conversation], requestOf(read, branch, retried, true));
  expect(
    await withSessionAnswers(
      [...conversation],
      requestOf(read, branchOf(ask, reply, deliveryEntry, failed), retried, true)
    )
  ).toEqual([...conversation, list]);
  // Another session's conversion is not the one before this session's.
  await withSessionAnswers([...conversation], requestOf(read, branch, {}, true));
  expect(await withSessionAnswers([...conversation], requestOf(read, branch, {}, true))).toEqual([
    ...conversation,
    list,
  ]);
  expect(calls()).toBe(6);
});

// A speculative compaction runs beside the main loop: its cut, then its whole conversion, which
// the main loop's next request matches message for message.
test("the main loop's request after a speculative compaction's cut and whole conversion keeps the list", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  const branch = branchOf(ask, readCall, readResult);
  const whole = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([skill, ask], requestOf(read, branch, session, true))).toBe(
    undefined
  );
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session, true))).toEqual([
    ...whole,
    list,
  ]);
  expect(await withSessionAnswers([...whole], requestOf(read, branch, session, true))).toEqual([
    ...whole,
    list,
  ]);
  expect(calls()).toBe(2);
});
