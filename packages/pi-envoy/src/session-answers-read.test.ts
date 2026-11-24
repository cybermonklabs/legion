// The rules by which `withSessionAnswers` keeps every list sent and appends a change, on reads
// through the real `readSessionAnswers`, as the extension reads: each from a Dispatch stand-in of
// its own and on a clock of its own (`now`), handed on by `carriedAnswers`, the mapping the
// extension's `sessionAnswersText` calls.
import { expect, test } from "bun:test";
import type { SessionAnswer } from "@legion/contracts";
import { bearerCredential } from "@legion/envoy-client/bearer";
import { readSessionAnswers, renderSessionAnswers } from "@legion/envoy-client/session-answers";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  ask,
  grepCall,
  grepResult,
  ITEM,
  noBranch,
  readCall,
  readResult,
  requestOf,
  skill,
} from "../test/session-answers-requests";
import {
  carriedAnswers,
  SESSION_ANSWERS_NO_ITEM,
  SESSION_ANSWERS_REPLACES,
  withSessionAnswers,
} from "./session-answers-context";

/** A state directory of the case's own, for the read's `answers.json`. */
const stateDir = temporaryDirectories("session-answers-read");

const NOW = Date.parse("2026-10-27T12:00:00.000Z");

/** A person's answer to the session's ask. */
const ANSWER: SessionAnswer = {
  ...ITEM,
  kind: "answer",
  relation: "answered_your_ask",
  selected: ["The second way"],
  ask: { id: "ask-1", question: "Which way should the fix go?" },
};

/** A person's comment in a thread the session is in. */
const COMMENT: SessionAnswer = {
  ...ITEM,
  kind: "comment",
  relation: "replied_in_a_comment_thread",
};

/** `item` with `id`, written `hours` hours before `NOW`, saying `text`. */
function writtenBefore(hours: number, id: string, text: string, item = ITEM): SessionAnswer {
  return { ...item, id, at: new Date(NOW - hours * 3_600_000).toISOString(), text };
}

/** The message a request carries the list `text` in, as `userTextMessage` adds it: the list, then
 *  the sentence saying it replaces every earlier list. */
function listMessage(text: string | undefined): unknown {
  return {
    role: "user",
    content: [
      { type: "text", text },
      { type: "text", text: SESSION_ANSWERS_REPLACES },
    ],
    timestamp: 0,
  };
}

let dispatchCount = 0;
/** A read through `readSessionAnswers` at `now()`, from a Dispatch of its own that answers
 *  `items()` whole, handed on by `carriedAnswers`. */
function readingDispatch(items: () => readonly SessionAnswer[], now: () => number) {
  dispatchCount += 1;
  const session = `ses_answers_dispatch_${dispatchCount}`;
  const url = `http://answers-dispatch-${dispatchCount}.test`;
  const env = { DISPATCH_STATE_DIR: stateDir() };
  const fetchImpl = (async (_input: string | URL | Request) => {
    const answered = items();
    return Response.json({
      session_id: session,
      since: new Date(now() - 72 * 3_600_000).toISOString(),
      total: answered.length,
      items: answered,
    });
  }) as typeof fetch;
  return async () =>
    carriedAnswers(
      await readSessionAnswers({
        session,
        dispatch: { enabled: true, url, credential: bearerCredential("token") },
        env,
        fetchImpl,
        now,
      })
    );
}

// The character cap leaves out the first item that does not fit, and every item older than it.
// What a person wrote there still counts: a list goes after the last message, and the model reads
// it with its line counting what was left out, rather than the list sent before as current. The
// list sent before stays where it went, with its bytes.
test("a new item longer than the character cap appends a list after the last message", async () => {
  const older = writtenBefore(2, "m-older", "Ship it.");
  let items = [older];
  const read = readingDispatch(
    () => items,
    () => NOW
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
  ]);
  items = [writtenBefore(1, "m-longest", "w".repeat(9_000)), older];
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
    listMessage(renderSessionAnswers(items, { now: NOW })),
  ]);
});

test("an edit that makes a shown comment too long for the character cap appends a list after the last message", async () => {
  const newer = writtenBefore(1, "m-newer", "x".repeat(3_000));
  const shipIt = writtenBefore(2, "c-1", "Ship it.", COMMENT);
  let items = [newer, shipIt];
  const read = readingDispatch(
    () => items,
    () => NOW
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
  ]);
  expect(JSON.stringify(placed)).toContain("Ship it.");
  const edited = {
    ...shipIt,
    edited_at: new Date(NOW - 1_800_000).toISOString(),
    text: `Do not ship. ${"y".repeat(5_500)}`,
  };
  items = [newer, edited];
  const appended = listMessage(renderSessionAnswers(items, { now: NOW }));
  expect(JSON.stringify(appended)).not.toContain("Do not ship.");
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
    appended,
  ]);
});

// A list the cap left with no item, only its header and its left-out line, shows nobody's words.
// Each case below changes an item that list leaves out and retracts nothing, so only the words of
// every item Dispatch sent, the left-out ones included, tell the two reads apart.
test("a second item over the character cap appends a list after one the cap had already emptied", async () => {
  const first = writtenBefore(2, "m-first", "w".repeat(9_000));
  let items = [first];
  const read = readingDispatch(
    () => items,
    () => NOW
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
  ]);
  expect(JSON.stringify(placed)).not.toContain(first.text);
  items = [writtenBefore(1, "m-second", "v".repeat(9_000)), first];
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
    listMessage(renderSessionAnswers(items, { now: NOW })),
  ]);
});

test("an edit to the comment a list left out whole appends a list, though its text is the same", async () => {
  const first = writtenBefore(2, "c-1", "w".repeat(9_000), COMMENT);
  let items = [first];
  const read = readingDispatch(
    () => items,
    () => NOW
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  const edited = {
    ...first,
    edited_at: new Date(NOW - 1_800_000).toISOString(),
    text: "v".repeat(9_000),
  };
  items = [edited];
  expect(listMessage(renderSessionAnswers(items, { now: NOW }))).toEqual(placed);
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
    placed,
  ]);
});

// An answer a person retracts (sets back to none) leaves the read while its time is still inside
// the 72 hours, so the list sent would go on showing it as current: a list without it goes after
// the last message, and the list sent stays where it went. An item the window leaves behind is no
// change, and appends nothing.
test("a retracted answer appends a list after the last message", async () => {
  const message = writtenBefore(2, "m-1", "Also check the docs.");
  let items = [writtenBefore(1, "ask-1", "The second way, please.", ANSWER), message];
  let clock = NOW;
  const read = readingDispatch(
    () => items,
    () => clock
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  clock = NOW + 60_000;
  items = [message];
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
    listMessage(renderSessionAnswers(items, { now: clock })),
  ]);
});

// With the only answer in the window retracted, the read has no list to give, and no text; its
// words still show that answer gone while its time is inside the window. Taking the list sent out
// would change the request from its place on, so the "no item" list goes after the last message,
// rather than leave the withdrawn words the newest list; its words are the read's, so the next
// request appends nothing.
test('retracting the only answer in the window appends the "no item" list, and the next request appends nothing', async () => {
  let items = [writtenBefore(1, "ask-1", "The second way, please.", ANSWER)];
  let clock = NOW;
  const read = readingDispatch(
    () => items,
    () => clock
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  expect(await withSessionAnswers([skill, ask], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
  ]);
  clock = NOW + 60_000;
  items = [];
  const turn = [skill, ask, readCall, readResult];
  const emptied = [skill, ask, placed, readCall, readResult, listMessage(SESSION_ANSWERS_NO_ITEM)];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual(emptied);
  const tools = [...turn, grepCall, grepResult];
  expect(await withSessionAnswers([...tools], requestOf(read, noBranch, session))).toEqual([
    ...emptied,
    grepCall,
    grepResult,
  ]);
});

test("an item the 72-hour window left behind keeps the list's place and bytes", async () => {
  const newest = writtenBefore(1, "m-new", "Use the second way.");
  let items = [newest, writtenBefore(71, "m-old", "Ship it.")];
  let clock = NOW;
  const read = readingDispatch(
    () => items,
    () => clock
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  // Two hours on, the window starts after the older item, and Dispatch no longer sends it.
  clock = NOW + 2 * 3_600_000;
  items = [newest];
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
  ]);
});

// The window can leave a list's every item behind: the read then gives no text, and its words
// show only items the window left. Nothing a person wrote changed, so the list stays.
test("a window the clock emptied keeps the list's place and bytes", async () => {
  let items = [writtenBefore(71, "m-old", "Ship it.")];
  let clock = NOW;
  const read = readingDispatch(
    () => items,
    () => clock
  );
  const session = {};
  const placed = listMessage(renderSessionAnswers(items, { now: NOW }));
  await withSessionAnswers([skill, ask], requestOf(read, noBranch, session));
  clock = NOW + 2 * 3_600_000;
  items = [];
  const turn = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers([...turn], requestOf(read, noBranch, session))).toEqual([
    skill,
    ask,
    placed,
    readCall,
    readResult,
  ]);
});
