import { afterEach, expect, jest, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { SessionAnswer } from "@legion/contracts";
import { bearerCredential } from "@legion/envoy-client/bearer";
import {
  readSessionAnswers,
  renderSessionAnswers,
  SESSION_ANSWERS_MARKER,
  SESSION_ANSWERS_WINDOW_MS,
  sessionAnswersFailureLine,
  sessionAnswersWords,
} from "@legion/envoy-client/session-answers";
import { APPROVAL_MESSAGE_TYPE } from "@legion/pi-shared/approval-message";
import { realTimeDeadline } from "@legion/test-fixtures/deadline";
import {
  answersSentEntry,
  ask,
  branchOf,
  grepCall,
  grepResult,
  ITEM,
  LIST,
  noBranch,
  readCall,
  readResult,
  recordedFor,
  requestOf,
  skill,
} from "../test/session-answers-requests";
import { type CarriedAnswers, withSessionAnswers } from "./session-answers-context";

/** The sentence every list this copy appends carries as its second block
 *  (`SESSION_ANSWERS_REPLACES`), and the first block of the "no item" list
 *  (`SESSION_ANSWERS_NO_ITEM`), as the model reads them. Written out rather than imported: the
 *  cases pin the bytes a request carries, and the file loads against a source without those
 *  exports, where the prefix cases below then fail on bytes rather than at import. */
const REPLACES =
  "This list replaces every earlier list of people's answers in this conversation, and every earlier line saying the list could not be read: read only the newest one as current.";
const NO_ITEM = `${SESSION_ANSWERS_MARKER}\n\nNo item: every item an earlier list showed was withdrawn or is now older than 72 hours.`;

/** An answers message as a request carries it (`userTextMessage`): one user message with a text
 *  block for each of `blocks`, stamped 0. */
function sent(...blocks: string[]) {
  return { role: "user", content: blocks.map((text) => ({ type: "text", text })), timestamp: 0 };
}

/** A list as this copy appends it: the list, then the sentence saying it replaces every earlier
 *  list. */
function listOf(text: string) {
  return sent(text, REPLACES);
}

// Message shapes as Oh My Pi hands them to a `context` handler, and the answers messages a request
// carries: a list as this copy appends it, and as an older copy of this extension appended it, in
// one block with no sentence.
const list = listOf(LIST);
const olderList = sent(LIST);
const reply = {
  role: "assistant",
  content: [{ type: "text", text: "Fixed." }],
  stopReason: "stop",
  timestamp: 4,
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

// The message pi-legion places in a request while a person's approval of the spec stands, as the
// host hands it to a `context` handler: a custom message of the type both plugins read
// (`APPROVAL_MESSAGE_TYPE`), text only, attribution `agent`, stamped with the time it was placed.
// pi-legion places a new one as the request's last message; whichever plugin's handler runs
// first, the answers message goes before it.
const approval = {
  role: "custom",
  customType: APPROVAL_MESSAGE_TYPE,
  content: [
    {
      type: "text",
      text: "A person has approved version 24 of the spec of ACME-2694 (dispatch://ACME-2694/spec@v24).",
    },
  ],
  display: false,
  attribution: "agent",
  timestamp: 20,
};
const laterApproval = {
  ...approval,
  content: [
    {
      type: "text",
      text: "A person has approved version 26 of the spec of ACME-2694 (dispatch://ACME-2694/spec@v26).",
    },
  ],
  timestamp: 21,
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
const later = listOf(LATER);

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

const FAILURE = sessionAnswersFailureLine("Dispatch did not answer within 800 ms");
const failureLine = sent(FAILURE);
/** A read that failed for `reason` with no list to give: the failure line naming that reason. */
const failedRead = (reason: string) => ({ text: sessionAnswersFailureLine(reason), reason });

// The prefix cases: each request after a change begins with every byte of the request before it,
// the answers messages sent included, and ends with the change; and the first request after a
// restart carries every answers message sent at its old place, with its bytes. They run in a
// session that compacted, whose first list went after the question and sits four messages deep by
// the time the people's answers change. They match the sentence by its opening words, and use
// nothing this rule added to the source, so against the source before it they fail on the bytes
// at the list's place.
const summary = { role: "compactionSummary", summary: "Earlier work.", timestamp: 0 };
const done = {
  role: "assistant",
  content: [{ type: "text", text: "Fixed." }],
  stopReason: "stop",
  timestamp: 7,
};
const docs = { role: "user", content: [{ type: "text", text: "Now the docs." }], timestamp: 8 };
const docsCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-3", name: "read", arguments: { path: "README.md" } }],
  timestamp: 9,
};
const docsResult = {
  role: "toolResult",
  toolCallId: "call-3",
  toolName: "read",
  content: [{ type: "text", text: "# Docs" }],
  isError: false,
  timestamp: 10,
};
// The requests of the prefix cases, in turn. After a compaction the host hands the summary first,
// then the skill (`withDispatchFirst`).
const opened = [summary, skill, ask];
const deep = [...opened, readCall, readResult, grepCall, grepResult];
const asked = [...deep, done, docs];
const followed = [...asked, docsCall, docsResult];

/** What one session's requests carry for `opened`, `deep`, `asked` and `followed`, in turn: one
 *  request for each of `reads` (`readingEach`). */
async function deepRequests(
  session: object,
  ...reads: (string | CarriedAnswers | undefined)[]
): Promise<(unknown[] | undefined)[]> {
  const read = readingEach(...reads);
  const carried: (unknown[] | undefined)[] = [];
  for (const messages of [opened, deep, asked, followed].slice(0, reads.length)) {
    carried.push(await withSessionAnswers([...messages], requestOf(read, noBranch, session)));
  }
  return carried;
}

/** Checks that `next` begins with every message of `previous`, the request before it, byte for
 *  byte: only then does the provider read `previous` from its prompt cache. */
function keepsSent(
  previous: readonly unknown[] | undefined,
  next: readonly unknown[] | undefined
): void {
  expect(previous).toBeDefined();
  expect(JSON.stringify(next?.slice(0, previous?.length))).toBe(JSON.stringify(previous));
}

/** A list appended after the last message: `text`, then the sentence, matched by its opening
 *  words. */
function listWithSentence(text: string) {
  return {
    role: "user",
    content: [
      { type: "text", text },
      { type: "text", text: expect.stringContaining("This list replaces every earlier list") },
    ],
    timestamp: 0,
  };
}

/** The list `LIST` with alice's answer edited. */
const EDITED = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it after the docs.`;

test("prefix case: a new answer goes after the last message, and the list sent stays where it went", async () => {
  const [, placed, changed] = await deepRequests({}, LIST, LIST, LATER);
  keepsSent(placed, changed);
  expect(changed?.slice(placed?.length)).toEqual([done, docs, listWithSentence(LATER)]);
  expect(placed?.slice(opened.length)).toEqual([
    listWithSentence(LIST),
    readCall,
    readResult,
    grepCall,
    grepResult,
  ]);
});

test("prefix case: a retraction that leaves an item goes after the last message, and the list sent stays where it went", async () => {
  const [, placed, changed] = await deepRequests({}, LATER, LATER, LIST);
  keepsSent(placed, changed);
  expect(changed?.slice(placed?.length)).toEqual([done, docs, listWithSentence(LIST)]);
});

test("prefix case: an edit goes after the last message, and the list sent stays where it went", async () => {
  const [, placed, changed] = await deepRequests({}, LIST, LIST, EDITED);
  keepsSent(placed, changed);
  expect(changed?.slice(placed?.length)).toEqual([done, docs, listWithSentence(EDITED)]);
});

test("prefix case: a failed read with no list to give appends the failure line, and the list sent stays where it went", async () => {
  const failed = failedRead("Dispatch did not answer within 800 ms");
  const [, placed, changed] = await deepRequests({}, LIST, LIST, failed);
  keepsSent(placed, changed);
  expect(changed?.slice(placed?.length)).toEqual([done, docs, failureLine]);
});

test("prefix case: the list a read gives after a failure line goes after the last message, and the failure line stays where it went", async () => {
  const failed = failedRead("Dispatch did not answer within 800 ms");
  const [, , withFailure, recovered] = await deepRequests({}, LIST, LIST, failed, LIST);
  keepsSent(withFailure, recovered);
  expect(recovered?.slice(withFailure?.length)).toEqual([
    docsCall,
    docsResult,
    listWithSentence(LIST),
  ]);
});

// A process that resumed the session holds nothing an earlier process kept in memory for it: only
// the session's branch, with every answers message the earlier process recorded in it. The whole
// request is compared first, so a failure shows where each answers message went.
test("prefix case: after a restart the first request carries every answers message sent at its old place, with its bytes", async () => {
  const session = {};
  const [, , changed] = await deepRequests(session, LIST, LIST, LATER);
  const [first, second] = recordedFor(session).map((data, index) =>
    answersSentEntry(data, `s${index}`)
  );
  const branch = branchOf(
    ask,
    first,
    readCall,
    readResult,
    grepCall,
    grepResult,
    done,
    docs,
    second,
    docsCall,
    docsResult
  );
  // A session object of its own: nothing this process kept for `session` reaches the request.
  const resumed = await withSessionAnswers([...followed], requestOf(readingEach(LATER), branch));
  expect(resumed).toEqual([...(changed ?? []), docsCall, docsResult]);
  keepsSent(changed, resumed);
});

// A person who deletes a comment a list sent showed leaves the next read that item with
// `deleted_at`, `deleted_by` and no words (`sessionanswers.go`), and the renderer prints it with
// the delete's mark and no words line (`renderSessionAnswerItem`). The list sent keeps its place
// and its bytes, the person's words with them: an earlier request delivered it. What keeps the
// model from reading those words as current is the list appended after the last message, which
// shows the comment deleted and says in its second block that it replaces every earlier list
// (`SESSION_ANSWERS_REPLACES`). An edit is the same change, with the edited mark and the new words.
const COMMENT: SessionAnswer = {
  ...ITEM,
  kind: "comment",
  relation: "commented_on_an_issue",
  id: "c-1",
  at: "2026-10-27T10:00:00.000Z",
  author: { kind: "user", id: "alice" },
  text: "Hold the release until the docs land.",
  owner: { issue: { key: "CORE-7", title: "Release notes" } },
  ref: "dispatch://CORE-7/comment/c-1",
  href: "/issues/CORE-7#comment-c-1",
};
const COMMENTED = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · user alice · commented on an issue\ndispatch://CORE-7/comment/c-1 · CORE-7: Release notes\nHold the release until the docs land.`;
/** `COMMENT` once alice deleted it, as Dispatch sends it. */
const DELETED_COMMENT: SessionAnswer = {
  ...COMMENT,
  deleted_at: "2026-10-27T11:30:00.000Z",
  deleted_by: { kind: "user", id: "alice" },
  text: "",
};
const DELETED = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · user alice · commented on an issue · Deleted by user alice at 2026-10-27T11:30:00.000Z\ndispatch://CORE-7/comment/c-1 · CORE-7: Release notes`;
/** `COMMENT` once alice edited it, as Dispatch sends it. */
const EDITED_COMMENT: SessionAnswer = {
  ...COMMENT,
  edited_at: "2026-10-27T11:00:00.000Z",
  text: "Hold the release until the changelog lands too.",
};
const EDITED_COMMENTED = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · user alice · commented on an issue · edited 2026-10-27T11:00:00.000Z\ndispatch://CORE-7/comment/c-1 · CORE-7: Release notes\nHold the release until the changelog lands too.`;

/** What `readSessionAnswers` gives for a read whose window, from `SINCE` on, holds `item` alone:
 *  the list `renderSessionAnswers` renders at the window's end, the item's words and `SINCE`. */
function readOf(item: SessionAnswer): CarriedAnswers {
  const now = Date.parse(SINCE) + SESSION_ANSWERS_WINDOW_MS;
  return {
    text: renderSessionAnswers([item], { now }),
    words: sessionAnswersWords([item]),
    since: SINCE,
  };
}

test("a comment deleted, or edited, appends a list showing it so with the sentence that it replaces every earlier list, and the list sent stays where it went", async () => {
  for (const [changed, shown] of [
    [DELETED_COMMENT, DELETED],
    [EDITED_COMMENT, EDITED_COMMENTED],
  ] as const) {
    const session = {};
    const [placed, next] = await deepRequests(session, readOf(COMMENT), readOf(changed));
    // The list sent, alice's words in it, is re-sent with every byte where it went.
    keepsSent(placed, next);
    // After the last message: the list as the read renders it, with none of the words it
    // replaced, then exactly the sentence saying it replaces every earlier list.
    expect(next?.slice(placed?.length)).toEqual([
      readCall,
      readResult,
      grepCall,
      grepResult,
      listOf(shown),
    ]);
    expect(JSON.stringify(next?.at(-1))).not.toContain(COMMENT.text);
    expect(placed?.slice(opened.length)).toEqual([listOf(COMMENTED)]);
    // The entry recorded for it holds the read's words, which the next read is compared with.
    expect(recordedFor(session)).toStrictEqual([
      { after: "user@1", blocks: [COMMENTED, REPLACES], words: sessionAnswersWords([COMMENT]) },
      {
        after: "toolResult@6#call-2",
        blocks: [shown, REPLACES],
        words: sessionAnswersWords([changed]),
      },
    ]);
  }
});

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

test("a new item goes after the request's last message, the list before it stays where it went, and both stay on the requests after it", async () => {
  const session = {};
  const read = readingEach(LIST, LATER);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
    later,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list, readCall, readResult, later, grepCall, grepResult]);
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
// Dispatch: the list sent must not stay the newest showing it as current, so a list without it goes
// after the last message, and the list sent stays where it went.
test("an item gone from the read while its time is inside the window appends a list without it", async () => {
  const session = {};
  const read = readingEach(LATER, LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    later,
    readCall,
    readResult,
    list,
  ]);
});

// A retraction that leaves the read no item gives no text, and its words show the item gone while
// its time is inside the window. Taking the list sent out would change the bytes from its place
// on, so the "no item" list goes after the last message. It records the read's words, so the next
// read with no text compares equal and appends nothing.
test('retracting the last item appends the "no item" list, and a later read with no text appends nothing', async () => {
  const session = {};
  const read = readingEach(LIST, { text: undefined, words: [], since: SINCE });
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const noItem = sent(NO_ITEM, REPLACES);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
    noItem,
  ]);
  expect(recordedFor(session).at(-1)).toStrictEqual({
    after: "toolResult@3#call-1",
    blocks: [NO_ITEM, REPLACES],
    words: [],
  });
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list, readCall, readResult, noItem, grepCall, grepResult]);
});

// A change appends the list as that read rendered it, so a new item is never hidden behind the
// bytes sent before; a read with the same items and another read line appends nothing.
test("an item written after a list kept with another read line appends a list with the text of that read", async () => {
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
  ).toEqual([skill, ask, list, readCall, readResult, grepCall, grepResult, later]);
});

// A read gives no text and no words when a server with no answers route has nothing cached, or
// when Dispatch is off. Such a read compares nothing: taking the list sent out of the conversation
// would change the request from its place on, as changing its bytes would, and the next read that
// answers from the cache would show it again: the pair would alternate, and every request pay.
test("a read with no text and no words keeps the list sent where it went", async () => {
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

// The same path with a read that gives text: the text goes after the last message, with no
// message sent rebuilt before it, and is not recorded. The next request with no compaction
// running rebuilds the messages sent and compares its read with the newest of them, so the change
// this path showed is appended again.
test("a read with text while the session compacts goes after the last message and is not recorded", async () => {
  const session = {};
  const read = readingEach(LIST, LATER);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session, true))).toEqual([
    ...turn,
    later,
  ]);
  expect(recordedFor(session)).toStrictEqual([
    { after: "user@1", blocks: [LIST, REPLACES], words: carry(LIST)?.words },
  ]);
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
    grepCall,
    grepResult,
    later,
  ]);
});

// The failure line says the list could not be read. The bytes of a list sent standing in for it
// would read as a read that answered, so a failed read with no list to give is a change: the
// failure line goes after the last message, the list a later read gives goes after it, and every
// message sent stays where it went.
test("the failure line where a list is placed goes after the last message, and so does the list after it", async () => {
  const session = {};
  const read = readingEach(LIST, FAILURE, LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  const failed = [skill, ask, list, readCall, readResult, failureLine];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual(failed);
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    ...failed,
    grepCall,
    grepResult,
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

// A read with neither text nor words (Dispatch off, or no answers route and nothing cached) has
// nothing new to say, and taking the failure line sent out would change the request from its place
// on: it stays, and a failed read after it shows nothing it did not.
test("a read with no text keeps a failure line sent where it went, and a failed read after it appends nothing", async () => {
  const session = {};
  const read = readingEach(FAILURE, undefined, undefined, FAILURE);
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
  const tools = [...turn, grepCall, grepResult];
  const kept = [skill, ask, failureLine, ...tools.slice(2)];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual(kept);
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual(kept);
});

// The model reads a failure only when the read has no list to give, and once: a run of failed
// reads keeps the failure line sent as the newest message, and so does a recovery to an empty
// window, which has nothing new to say. The list a later read gives goes after the last message.
test("a failure line is appended once; a second failure and a recovery to an empty window append nothing; a list after them is appended", async () => {
  const session = {};
  const waiting = "an earlier read is still waiting on Dispatch";
  const read = readingEach(
    failedRead("Dispatch did not answer within 800 ms"),
    { text: sessionAnswersFailureLine(waiting), reason: waiting },
    { text: undefined, words: [], since: SINCE, answered: true },
    LIST
  );
  const failed = await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  expect(failed).toEqual([skill, ask, failureLine]);
  const turn = [skill, ask, readCall, readResult];
  const again = await withSessionAnswers([...turn], requestOf(read, noBranch, session));
  expect(again).toEqual([skill, ask, failureLine, readCall, readResult]);
  const tools = [...turn, grepCall, grepResult];
  const recovered = await withSessionAnswers([...tools], requestOf(read, noBranch, session));
  expect(recovered).toEqual([skill, ask, failureLine, ...tools.slice(2)]);
  const listed = await withSessionAnswers([...tools, reply], requestOf(read, noBranch, session));
  expect(listed).toEqual([skill, ask, failureLine, ...tools.slice(2), reply, list]);
});

// A recovery to an empty window compares its words with the newest list sent, not with the failure
// line after it: an item of that list withdrawn inside the window appends the "no item" list.
test('a recovery to an empty window after a failure line appends the "no item" list when the newest list sent shows an item it withdrew', async () => {
  const session = {};
  const read = readingEach(LIST, failedRead("Dispatch did not answer within 800 ms"), {
    text: undefined,
    words: [],
    since: SINCE,
    answered: true,
  });
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  const failed = [skill, ask, list, readCall, readResult, failureLine];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual(failed);
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    ...failed,
    grepCall,
    grepResult,
    sent(NO_ITEM, REPLACES),
  ]);
});

// A compaction that summarized the message an answers message went after leaves that message out
// of the request and of the comparison: the next read with text has no message sent to compare
// with, and goes after the last message.
test("a list whose place left the conversation, as a compaction leaves it, goes after the last message again", async () => {
  const session = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  // The compaction summarized the question the list followed.
  const compacted = [summary, skill, readCall, readResult];
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
  expect(recordedFor(session)).toMatchObject([
    { after: "user@1" },
    { after: "toolResult@3#call-1" },
  ]);
});

// A retried request converts the same messages again, and its read may show a change: both lists
// then follow the same message, in the order they went.
test("two lists sent after one message, as a retried request sends them, keep their order", async () => {
  const session = {};
  const read = readingEach(LIST, LATER);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    list,
  ]);
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    list,
    later,
  ]);
  expect(
    await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session))
  ).toEqual([...turn, list, later, grepCall, grepResult]);
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

// Oh My Pi keeps one `sessionManager` when it switches sessions, so a failure episode is kept by
// session id as well (`episodes`): a failed read of one session is told while another session on
// the same `sessionManager` is inside an episode of its own, and each session's episode tells its
// first failure once.
test("each session on one sessionManager has its own failure episode", async () => {
  const manager = {};
  const down = "database is down";
  const told: [string, string, boolean][] = [];
  const failingFor = (id: string) => ({
    ...requestOf(readingEach(failedRead(down)), noBranch, manager, false, id),
    readFailed: (reason: string, shown: boolean) => {
      told.push([id, reason, shown]);
    },
  });
  const downLine = sent(sessionAnswersFailureLine(down));
  expect(await withSessionAnswers([skill, ask], failingFor("ses_a"))).toEqual([
    skill,
    ask,
    downLine,
  ]);
  expect(told).toStrictEqual([["ses_a", down, true]]);
  const other = { ...ask, content: [{ type: "text", text: "Another task." }], timestamp: 50 };
  expect(await withSessionAnswers([skill, other], failingFor("ses_b"))).toEqual([
    skill,
    other,
    downLine,
  ]);
  const both: [string, string, boolean][] = [
    ["ses_a", down, true],
    ["ses_b", down, true],
  ];
  expect(told).toStrictEqual(both);
  // ses_a's episode is still open: its next failed read keeps the failure line sent where it went
  // and tells nothing.
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], failingFor("ses_a"))).toEqual([
    skill,
    ask,
    downLine,
    readCall,
    readResult,
  ]);
  expect(told).toStrictEqual(both);
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

// The entry a request records is what every later request rebuilds the message from: the key of
// the message it went after, the request's last; its blocks; and the read's words, which the next
// read is compared with. A failure line's read gives no words, and its entry holds none.
test("the entry recorded names the request's last message and holds the blocks sent and the read's words", async () => {
  const session = {};
  const read = readingEach(LIST, failedRead("database is down"));
  const turn = [skill, ask, readCall, readResult];
  await withSessionAnswers([...turn], requestOf(read, noBranch, session));
  await withSessionAnswers([...turn, grepCall, grepResult], requestOf(read, noBranch, session));
  expect(recordedFor(session)).toStrictEqual([
    {
      after: "toolResult@3#call-1",
      blocks: [LIST, REPLACES],
      words: carry(LIST)?.words,
    },
    { after: "toolResult@6#call-2", blocks: [sessionAnswersFailureLine("database is down")] },
  ]);
});

// The extension writes nothing when the host moved to another session while the read waited. The
// request still carries the list; the next request, whose branch holds no entry for it, has no
// message sent to rebuild or compare with, and appends the list after its own last message.
test("a request whose write was guarded still carries the list, and the next request appends it again", async () => {
  const session = {};
  const read = readingEach(LIST);
  const guarded = { ...requestOf(read, noBranch, session), record: () => undefined };
  expect(await withSessionAnswers([skill, ask], guarded)).toEqual([skill, ask, list]);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    ...turn,
    list,
  ]);
});

// Oh My Pi does not wait for the `context` handler of a run that was stopped: it races the handler
// and drops what it returns, and the handler goes on to its read and its record. The next run's
// request can start while that read is still open, so both requests' reads can come back with the
// same change. The stopped request decides first and appends the change after its own last
// message; the request after it rebuilds that message where it went and appends nothing, so the
// request after both begins with the bytes of the one the host sent.
test("two overlapping requests on one shared read: the next request carries the stopped request's list where it went", async () => {
  const session = {};
  await withSessionAnswers([skill, ask], requestOf(readingEach(LIST), noBranch, session));
  const stopped = [skill, ask, readCall, readResult];
  const next = [...stopped, grepCall];
  const shared = Promise.withResolvers<CarriedAnswers | undefined>();
  const read = () => shared.promise;
  const deciding = [
    withSessionAnswers([...stopped], requestOf(read, noBranch, session)),
    withSessionAnswers([...next], requestOf(read, noBranch, session)),
  ];
  shared.resolve(carry(LATER));
  const [, carried] = await Promise.all(deciding);
  const after = await withSessionAnswers(
    [...next, grepResult],
    requestOf(readingEach(LATER), noBranch, session)
  );
  keepsSent(carried, after);
  expect(recordedFor(session)).toStrictEqual([
    { after: "user@1", blocks: [LIST, REPLACES], words: carry(LIST)?.words },
    { after: "toolResult@3#call-1", blocks: [LATER, REPLACES], words: carry(LATER)?.words },
  ]);
});

// The same two requests, each on a read of its own, the later request's coming back first: the
// earlier request still decides first, and the later one decides from the branch as the earlier
// one left it, whichever read settles first.
test("two overlapping requests whose reads settle out of order: the earlier request decides first", async () => {
  const session = {};
  await withSessionAnswers([skill, ask], requestOf(readingEach(LIST), noBranch, session));
  const stopped = [skill, ask, readCall, readResult];
  const next = [...stopped, grepCall];
  const stoppedRead = Promise.withResolvers<CarriedAnswers | undefined>();
  const nextRead = Promise.withResolvers<CarriedAnswers | undefined>();
  const first = withSessionAnswers(
    [...stopped],
    requestOf(() => stoppedRead.promise, noBranch, session)
  );
  const second = withSessionAnswers(
    [...next],
    requestOf(() => nextRead.promise, noBranch, session)
  );
  nextRead.resolve(carry(LATER));
  // Every callback queued so far runs, so a request free to decide on its read alone has decided.
  const drained = Promise.withResolvers<void>();
  setImmediate(drained.resolve);
  await drained.promise;
  stoppedRead.resolve(carry(LATER));
  const [, carried] = await Promise.all([first, second]);
  const after = await withSessionAnswers(
    [...next, grepResult],
    requestOf(readingEach(LATER), noBranch, session)
  );
  keepsSent(carried, after);
  expect(recordedFor(session)).toStrictEqual([
    { after: "user@1", blocks: [LIST, REPLACES], words: carry(LIST)?.words },
    { after: "toolResult@3#call-1", blocks: [LATER, REPLACES], words: carry(LATER)?.words },
  ]);
  expect(carried).toEqual([skill, ask, list, readCall, readResult, later, grepCall]);
});

// The host can move to another session while a request's read waits, and the branch it lists then
// is that session's. The request reads none of it (`branch` gives undefined) and decides from the
// branch as it began, so it still rebuilds the list sent where it went.
test("a request whose host moved to another session while its read waited decides from the branch as it began", async () => {
  const session = {};
  await withSessionAnswers([skill, ask], requestOf(readingEach(LIST), noBranch, session));
  const request = requestOf(readingEach(LIST), noBranch, session);
  let reads = 0;
  const moved = { ...request, branch: () => (reads++ === 0 ? request.branch() : undefined) };
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], moved)).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
  // Read before the read, and again after it.
  expect(reads).toBe(2);
});

// A message is rebuilt from the blocks its entry recorded, never from a constant read again, so a
// release that rewords the sentence still rebuilds the bytes an earlier release sent.
test("an entry an earlier release recorded with another sentence is rebuilt with exactly its blocks", async () => {
  const earlier = "Read only this list as current.";
  const entry = { after: "user@1", blocks: [LIST, earlier], words: carry(LIST)?.words };
  const branch = branchOf(ask, answersSentEntry(entry, "s0"), readCall, readResult);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(readingEach(LIST), branch))).toEqual([
    skill,
    ask,
    sent(LIST, earlier),
    readCall,
    readResult,
  ]);
});

// A reader passes over an entry it cannot read: no message is rebuilt from it, and the newest
// message sent is the newest entry it can read.
test("an entry whose data does not read as an answers message sent is passed over", async () => {
  const after = "toolResult@3#call-1";
  const words = carry(LATER)?.words;
  const branch = branchOf(
    ask,
    answersSentEntry({ after: "user@1", blocks: [LIST, REPLACES] }, "s0"),
    readCall,
    readResult,
    answersSentEntry(null, "s1"),
    answersSentEntry({ blocks: [LATER, REPLACES], words }, "s2"),
    answersSentEntry({ after, blocks: [], words }, "s3"),
    answersSentEntry({ after, blocks: ["Ship it.", REPLACES], words }, "s4"),
    answersSentEntry({ after, blocks: [LATER, REPLACES], words: [1] }, "s5")
  );
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(readingEach(LIST), branch))).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
});

// An older copy of the extension put the list after the last message of every request. This copy
// reads nothing for that request and adds no second list: the request without it goes through
// this copy's rule with the older copy's list as its read, so that list is appended, and recorded
// as this copy's with the sentence, only when it shows something the newest message sent did not.
test("a request an older copy already gave the list gets no second one and no read; its list is recorded as this copy's", async () => {
  const { read, calls } = reading(LIST);
  const session = {};
  expect(
    await withSessionAnswers([skill, ask, olderList], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list]);
  // The next request, with the same list after its last message, carries the list sent where it
  // went and appends nothing.
  expect(
    await withSessionAnswers(
      [skill, ask, readCall, readResult, olderList],
      requestOf(read, noBranch, session)
    )
  ).toEqual([skill, ask, list, readCall, readResult]);
  expect(recordedFor(session)).toStrictEqual([{ after: "user@1", blocks: [LIST, REPLACES] }]);
  const failure = sent(sessionAnswersFailureLine("database is down"));
  expect(await withSessionAnswers([ask, failure], requestOf(read, noBranch))).toEqual([
    ask,
    failure,
  ]);
  expect(calls()).toBe(0);
});

// A list an older copy placed carries no item fingerprints, so only its text is left to compare,
// and every difference but the line naming the read counts as something new. Here the read note's
// own length pushed an item past the reader's character cap, so a list is appended after the last
// message: a prompt-cache miss, and never a person's words held back. Reading that text more
// closely to spare the miss swallowed an item twice (see `showsNew`).
const CAPPED = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.\n\n1 older item left out to stay under 8,000 characters: run \`dispatch answers\` to read every one in full.`;
const cappedList = sent(CAPPED);

// The list `LIST` as a copy renders it from its cache when a read fails, and the same list for a
// read that failed for another reason.
const CACHED = `${SESSION_ANSWERS_MARKER}\nAs read at 2026-10-27T11:00:00.000Z; this read failed: Dispatch did not answer within 800 ms.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
const cachedList = sent(CACHED);
const WAITING = CACHED.replace(
  "Dispatch did not answer within 800 ms.",
  "an earlier read is still waiting on Dispatch."
);
const waitingList = sent(WAITING);

test("an older copy's list the cap shortened is appended after the last message, and the list sent stays", async () => {
  const session = {};
  const fresh = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · bob sent you a message\nAlso check the docs.\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
  // No read of this copy's own: the older copy's list is what gets placed.
  const { read, calls } = reading(LIST);
  await withSessionAnswers([skill, ask, sent(fresh)], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(
    await withSessionAnswers([...turn, cappedList], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, listOf(fresh), readCall, readResult, listOf(CAPPED)]);
  expect(calls()).toBe(0);
});

// The one difference an older copy's list may show and keep its place and bytes: the line naming
// the read (`withoutReadLine`).
test("an older copy's list that differs only in the line naming its read keeps its place and bytes", async () => {
  const session = {};
  const { read, calls } = reading(LIST);
  await withSessionAnswers([skill, ask, cappedList], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  const sameItems = sent(CAPPED.replace("2026-10-27T11:00:00.000Z;", "2026-10-27T11:05:00.000Z;"));
  expect(
    await withSessionAnswers([...turn, sameItems], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, listOf(CAPPED), readCall, readResult]);
  expect(calls()).toBe(0);
});

// A list an older copy placed has no words, so a read whose window holds no item cannot tell its
// aged-out items from a retracted one. Left the newest list, a retracted only answer would stay
// current in every request while an older copy runs beside this one, so such a read appends the
// "no item" list after it. That list records the read's words, so the next such read appends
// nothing.
test('a read with no text that gives words appends the "no item" list after a list an older copy placed', async () => {
  const session = {};
  const read = readingEach({ text: undefined, words: [], since: SINCE });
  // No read of this copy's own: the older copy's list is what gets placed.
  expect(
    await withSessionAnswers([skill, ask, olderList], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list]);
  const turn = [skill, ask, readCall, readResult];
  const noItem = sent(NO_ITEM, REPLACES);
  const emptied = [skill, ask, list, readCall, readResult, noItem];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual(emptied);
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    ...emptied,
    grepCall,
    grepResult,
  ]);
});

// An older copy's list rendered from its cache names the read that failed, and stands in for this
// copy's read: the first failure of an episode is told to `readFailed`, shown when the list is
// appended and its read line names it, not shown when the request keeps the messages sent, and a
// later failure of the same episode is not told. An older copy's list with no read line names no
// failure and ends the episode. An older copy's failure line comes from a read that failed, so
// unlike a list with no read line it ends no episode.
test("an older copy's list naming a failed read tells that failure once per episode, shown or kept", async () => {
  const session = {};
  const told: [string, boolean][] = [];
  const request = {
    ...requestOf(readingEach(LIST), noBranch, session),
    readFailed: (reason: string, shown: boolean) => {
      told.push([reason, shown]);
    },
  };
  const waiting = listOf(WAITING);
  // Appended, with the read line that names its failure.
  expect(await withSessionAnswers([skill, ask, waitingList], request)).toEqual([
    skill,
    ask,
    waiting,
  ]);
  expect(told).toStrictEqual([["an earlier read is still waiting on Dispatch.", true]]);
  const turn = [skill, ask, readCall, readResult];
  const kept = [skill, ask, waiting, readCall, readResult];
  // Kept, as it differs from the list sent only in its read line; it names no failure, so the
  // episode ends, and the cached list after it begins another, kept and not shown.
  expect(await withSessionAnswers([...turn, olderList], request)).toEqual(kept);
  expect(await withSessionAnswers([...turn, cachedList], request)).toEqual(kept);
  const toldTwice: [string, boolean][] = [
    ["an earlier read is still waiting on Dispatch.", true],
    ["Dispatch did not answer within 800 ms.", false],
  ];
  expect(told).toStrictEqual(toldTwice);
  // The failure line is appended, the cached list after it is appended, and a failure kept behind
  // that list is not told: no read has answered since the episode began.
  const tools = [...turn, grepCall, grepResult];
  const failed = [...kept, grepCall, grepResult, failureLine];
  expect(await withSessionAnswers([...tools, failureLine], request)).toEqual(failed);
  const tail = [...tools, reply];
  const cached = [...failed, reply, listOf(CACHED)];
  expect(await withSessionAnswers([...tail, cachedList], request)).toEqual(cached);
  expect(await withSessionAnswers([...tail, waitingList], request)).toEqual(cached);
  expect(told).toStrictEqual(toldTwice);
});

// Every renderer writes a list with no read line only for a read Dispatch answered, so an older
// copy's list with no read line is an answered read: when an older copy decides every request of
// a session, the next failure after it is told too.
test("an older copy's list with no read line lets the next failure it names be told again", async () => {
  const session = {};
  const told: [string, boolean][] = [];
  const request = {
    ...requestOf(readingEach(LIST), noBranch, session),
    readFailed: (reason: string, shown: boolean) => {
      told.push([reason, shown]);
    },
  };
  expect(await withSessionAnswers([skill, ask, olderList], request)).toEqual([skill, ask, list]);
  const turn = [skill, ask, readCall, readResult];
  const tools = [...turn, grepCall, grepResult];
  for (const older of [cachedList, olderList, waitingList]) {
    await withSessionAnswers([...turn, older], request);
  }
  expect(await withSessionAnswers([...tools, olderList], request)).toEqual([
    skill,
    ask,
    list,
    ...tools.slice(2),
  ]);
  expect(told).toEqual([
    ["Dispatch did not answer within 800 ms.", false],
    ["an earlier read is still waiting on Dispatch.", false],
  ]);
});

// A cached list a failed read gives names that read's failure on its read line. Appended, the
// model reads it, so the episode's first failure is told as shown; the later reads of the same
// episode keep the list sent and tell nothing, whatever failed them.
test("a cached list appended for a failed read tells that failure once, shown; later failures of the episode tell nothing", async () => {
  const session = {};
  const told: [string, boolean][] = [];
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
    readFailed: (reason: string, shown: boolean) => {
      told.push([reason, shown]);
    },
  };
  const cached = listOf(CACHED);
  expect(await withSessionAnswers([skill, ask], request)).toEqual([skill, ask, cached]);
  expect(told).toEqual([["Dispatch did not answer within 800 ms", true]]);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], request)).toEqual([
    skill,
    ask,
    cached,
    readCall,
    readResult,
  ]);
  const tools = [...turn, grepCall, grepResult];
  for (let call = 0; call < 2; call += 1) {
    expect(await withSessionAnswers([...tools], request)).toEqual([
      skill,
      ask,
      cached,
      ...tools.slice(2),
    ]);
  }
  expect(told).toEqual([["Dispatch did not answer within 800 ms", true]]);
});

// `readFailed` is told on the first failed read of an episode, whether the request shows the model
// a message for it or not, and not again until a read answers: a cached list with the items sent
// is kept and not shown, a failure line appended is shown.
test("readFailed is told once per failure episode, shown or not, and again after a read that answered", async () => {
  const session = {};
  const told: [string, boolean][] = [];
  const words = carry(LIST)?.words;
  const answered = { text: LIST, words, since: SINCE, answered: true };
  const timedOut = "Dispatch did not answer within 800 ms";
  const down = failedRead("database is down");
  const downLine = sent(sessionAnswersFailureLine("database is down"));
  const reads = readingEach(
    answered,
    { text: CACHED, words, since: SINCE, reason: timedOut },
    down,
    answered,
    down
  );
  const request = {
    ...requestOf(reads, noBranch, session),
    readFailed: (reason: string, shown: boolean) => {
      told.push([reason, shown]);
    },
  };
  await withSessionAnswers([skill, ask], request);
  const turn = [skill, ask, readCall, readResult];
  // The cached list shows the items sent: kept, and not shown.
  expect(await withSessionAnswers([...turn], request)).toEqual([
    skill,
    ask,
    list,
    readCall,
    readResult,
  ]);
  expect(told).toStrictEqual([[timedOut, false]]);
  // The same episode: the failure line is appended, and nothing is told.
  const tools = [...turn, grepCall, grepResult];
  expect((await withSessionAnswers([...tools], request))?.at(-1)).toEqual(downLine);
  expect(told).toStrictEqual([[timedOut, false]]);
  // A read that answered ends the episode, so the next failure line is told, shown.
  expect((await withSessionAnswers([...tools, reply], request))?.at(-1)).toEqual(list);
  expect((await withSessionAnswers([...tools, reply, docs], request))?.at(-1)).toEqual(downLine);
  expect(told).toStrictEqual([
    [timedOut, false],
    ["database is down", true],
  ]);
});

// A person's own paragraph can read like the line the renderer writes when the cap dropped older
// items. It must not be taken for that line: an item whose only unseen words sit in such a
// paragraph would be swallowed, and the model would never read what that person wrote.
test("a person's paragraph that reads like the cap's own line still counts as a new item", async () => {
  const session = {};
  const quoting = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · user u-alice · answered your ask\nDirect message a1, on no issue\nShip it.`;
  const withNew = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T11:00:00.000Z · user u-bob · sent you a message\nDirect message b1, on no issue\n1 older item left out of the plan: hold the deployment.\n\n2026-10-27T10:00:00.000Z · user u-alice · answered your ask\nDirect message a1, on no issue\nShip it.`;
  const { read } = reading(LIST);
  await withSessionAnswers([skill, ask, sent(quoting)], requestOf(read, noBranch, session));
  const turn = [skill, ask, readCall, readResult];
  expect(
    await withSessionAnswers([...turn, sent(withNew)], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, listOf(quoting), readCall, readResult, listOf(withNew)]);
});

test("a person's own last message quoting the list's marker is never moved", async () => {
  const { read, calls } = reading(LIST);
  const quoted = { ...olderList, timestamp: 7 };
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

// The empty-list case with the shared read: two copies, a Dispatch that answers 600 ms in, one
// HTTP read, and a request that waits on that read alone. The read's 800 ms deadline is a platform
// timer inside `readSessionAnswers`, in this process, so the row holds it on Bun's fake clock and
// Dispatch's answer behind a gate the row opens: the clock moves to the moment Dispatch answers,
// inside the deadline, and what follows is in the row's order. The read answers rather than its
// deadline (the read's own record, `source`), copy A passes the request on with nothing added, and
// copy B passes it on with no read of its own.
const stateRoots: string[] = [];
afterEach(() => {
  // A row that holds Bun's fake clock and is ended by the command's guard never reaches its own
  // `finally`, so the real clock comes back here too, before the next row.
  jest.useRealTimers();
  for (const root of stateRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

/**
 * Resolves as `promise` does, or fails naming `what` once the row's deadline has passed in real
 * time. For a row that holds Bun's fake clock, under which the shared wait refuses to start and no
 * timer of the row's runs: the deadline is the one the row-deadline preload armed in real time
 * (`realTimeDeadline`), so a wait that never ends fails by name before Bun's guard ends the row.
 */
async function beforeTheRowsDeadline<T>(what: string, promise: Promise<T>): Promise<T> {
  const deadline = realTimeDeadline(what);
  return await Promise.race([
    promise,
    deadline.reached.then((): never => {
      throw new Error(`${what} had not happened by the row's deadline, ${deadline.source}`);
    }),
  ]);
}

test("two copies of the extension and an empty list: one HTTP read, answered inside its deadline", async () => {
  const stateRoot = mkdtempSync(join(tmpdir(), "session-answers-context-"));
  stateRoots.push(stateRoot);
  let fetches = 0;
  const asked = Promise.withResolvers<void>();
  const answer = Promise.withResolvers<void>();
  const fetchImpl = (async (input: string | URL | Request) => {
    fetches += 1;
    asked.resolve();
    await answer.promise;
    const session = new URL(input.toString()).pathname.split("/")[4] ?? "";
    return Response.json({
      session_id: session,
      since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
      total: 0,
      items: [],
    });
  }) as typeof fetch;
  /** How each read the copies made ended, as the read reports it. */
  const sources: string[] = [];
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

  jest.useFakeTimers();
  try {
    // Copy A's handler, its read in flight to Dispatch and its deadline armed.
    const copyA = withSessionAnswers(messages, requestOf(read, noBranch));
    await beforeTheRowsDeadline(
      "copy A's handler asking Dispatch",
      Promise.race([
        asked.promise,
        copyA.then((): never => {
          throw new Error("copy A's handler returned without asking Dispatch");
        }),
      ])
    );
    // Dispatch answers 600 ms in: the deadline has not run out by then.
    jest.advanceTimersByTime(600);
    answer.resolve();
    expect(await beforeTheRowsDeadline("copy A's handler returning", copyA)).toBeUndefined();
    expect(sources).toEqual(["fresh"]);
    // Copy B's, on the array A passed on, with the clock where Dispatch answered.
    expect(
      await beforeTheRowsDeadline(
        "copy B's handler returning",
        withSessionAnswers(messages, requestOf(read, noBranch))
      )
    ).toBeUndefined();
  } finally {
    jest.useRealTimers();
  }

  expect(sources).toEqual(["fresh"]);
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

// pi-legion's handler ran first and placed the approval message last, then this handler ran. The
// answers message goes before the trailing run of approval messages, so the approval message
// stays the request's last message.
test("an answers list placed in a request that ends with an approval message goes before it", async () => {
  const { read } = reading(LIST);
  expect(await withSessionAnswers([skill, ask, approval], requestOf(read, noBranch))).toEqual([
    skill,
    ask,
    list,
    approval,
  ]);
  expect(
    await withSessionAnswers([skill, ask, approval, laterApproval], requestOf(read, noBranch))
  ).toEqual([skill, ask, list, approval, laterApproval]);
});

// The approval message is no session message, so a later request holds it only where pi-legion
// rebuilds it, and not at all when this handler runs first. The answers message is recorded after
// the message before it, which every later request holds.
test("an answers list anchors on the message before a trailing approval message", async () => {
  const session = {};
  const read = readingEach(LIST);
  await withSessionAnswers([skill, ask, approval], requestOf(read, noBranch, session));
  expect(recordedFor(session)).toStrictEqual([
    { after: "user@1", blocks: [LIST, REPLACES], words: carry(LIST)?.words },
  ]);
  const answer = { ...reply, timestamp: 22 };
  // This handler first: the request does not hold the approval message yet.
  expect(
    await withSessionAnswers([skill, ask, answer], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list, answer]);
  // pi-legion's handler first: it rebuilt the approval message right before its answer.
  expect(
    await withSessionAnswers([skill, ask, approval, answer], requestOf(read, noBranch, session))
  ).toEqual([skill, ask, list, approval, answer]);
});

// The approval message is no entry of the branch: where the request ends on the branch is where
// the message before the trailing approval messages sits. A remote compaction's conversion up to
// its cut ends before the branch's last message; a repeat of a conversion that ends at the
// branch's last message, while the session compacts, is not read for.
test("a request that ends with an approval message reads as the branch's position", async () => {
  const { read, calls } = reading(LIST);
  expect(
    await withSessionAnswers(
      [skill, ask, readCall, readResult, approval],
      requestOf(read, branchOf(ask, readCall, readResult, reply))
    )
  ).toBeUndefined();
  expect(calls()).toBe(0);
  const session = {};
  const branch = branchOf(ask, reply, deliveryEntry);
  const conversation = [skill, ask, reply, delivery, approval];
  expect(
    await withSessionAnswers([...conversation], requestOf(read, branch, session, true))
  ).toEqual([skill, ask, reply, delivery, list, approval]);
  expect(
    await withSessionAnswers([...conversation], requestOf(read, branch, session, true))
  ).toBeUndefined();
  expect(calls()).toBe(1);
});
