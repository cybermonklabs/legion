import { expect, test } from "bun:test";
import type { AskThreadPage, CommentThreadPage, MessageThreadPage } from "@legion/contracts";
import asksThreadBody from "./fixtures/asks-thread.json";
import asksThreadAuthorBody from "./fixtures/asks-thread-author.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInAuthorBody from "./fixtures/comments-in-author.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesInAuthorBody from "./fixtures/messages-in-author.json";
import { mergeThreads, type ThreadPage } from "./thread-merge";

// The pages the server answered at the stack base (fixtures/README.md): All's three first pages,
// the comments' second page from the first page's `before`, and Mine's three pages.
const COMMENTS = commentsInBody as unknown as CommentThreadPage;
const COMMENTS_NEXT = commentsInBeforeBody as unknown as CommentThreadPage;
const MESSAGES = messagesInBody as unknown as MessageThreadPage;
const ASKS = asksThreadBody as unknown as AskThreadPage;
const MINE = [
  [commentsInAuthorBody as unknown as CommentThreadPage],
  [messagesInAuthorBody as unknown as MessageThreadPage],
  [asksThreadAuthorBody as unknown as AskThreadPage],
];

/** Each row's record id, cut to its first eight characters, as the fixtures' README names them. */
function ids(rows: readonly { readonly id: string }[]): string[] {
  return rows.map((row) => row.id.slice(0, 8));
}

/** A copy of `page` with every time of the day `from` moved to the day `to`, so the whole route
 *  sorts before or after the others. */
function movedTo<P extends ThreadPage>(page: P, from: string, to: string): P {
  return JSON.parse(JSON.stringify(page).replaceAll(`${from}T`, `${to}T`)) as P;
}

/** All's first 20 rows, read from the times of the three first pages: the newest thread first,
 *  and the two records of the reader's message that the tester quoted together, the quoting
 *  reply's first. */
const FIRST_TWENTY = [
  "b5979723",
  "267776fd",
  "0bcb3346",
  "1d7c4317",
  "b3ab4dfe",
  "e46b9c56",
  "1ce09269",
  "727f2b9d",
  "3295fd2d",
  "b788d337",
  "0d7bfef6",
  "a59f2ee7",
  "73914aae",
  "2dc5b959",
  "4b7efdcf",
  "882e100d",
  "2e0483b8",
  "6b245020",
  "b8a85377",
  "80fbc0ff",
];

/** All's second 20 rows, once the comments' second page is read. */
const SECOND_TWENTY = [
  "678608f6",
  "23a9b95f",
  "895aef15",
  "724cd9bb",
  "f49e91eb",
  "c1d04870",
  "cc32fc53",
  "59ab65ed",
  "cc70d974",
  "99945b76",
  "8bb0cec1",
  "f7ef251a",
  "e4a9b265",
  "a2dbfe5a",
  "c80f7ff7",
  "5c7bd7e1",
  "45d7dc22",
  "a3f195b2",
  "0b2be66d",
  "1d34b321",
];

test("All draws the first 20 rows of its three first pages in one order, the newest thread first", () => {
  const merged = mergeThreads({ routes: [[COMMENTS], [MESSAGES], [ASKS]], shown: 20 });
  expect(ids(merged.rows)).toEqual(FIRST_TWENTY);
  expect(merged.refill).toEqual([]);
});

test("the count left is the rows read and not drawn and the threads each route's remaining counts", () => {
  // 20 comment rows, 17 message rows and 12 ask rows are read, 20 are drawn, and the comments'
  // first page says 2 threads follow it.
  const merged = mergeThreads({ routes: [[COMMENTS], [MESSAGES], [ASKS]], shown: 20 });
  expect(merged.left).toBe(49 - 20 + 2);
});

test("before the next 20, All reads again only the route with a next page whose undrawn rows are fewer than 20", () => {
  // The first 20 rows drew 7 of the comments' 20, so 13 are left on their page and its `before`
  // names a next page. The messages and the asks ended on their first pages.
  const waiting = mergeThreads({ routes: [[COMMENTS], [MESSAGES], [ASKS]], shown: 40 });
  expect(waiting.refill).toEqual([0]);
  expect(ids(waiting.rows)).toEqual(FIRST_TWENTY);
});

test("a page whose rows were drawn stays drawn, and the next 20 draw none of them again", () => {
  const second = mergeThreads({
    routes: [[COMMENTS, COMMENTS_NEXT], [MESSAGES], [ASKS]],
    shown: 40,
  });
  expect(second.refill).toEqual([]);
  expect(ids(second.rows)).toEqual([...FIRST_TWENTY, ...SECOND_TWENTY]);
  expect(new Set(ids(second.rows)).size).toBe(40);
  expect(second.left).toBe(52 - 40);
});

test("a route that ends is never read again, and the last page draws every row left", () => {
  const last = mergeThreads({
    routes: [[COMMENTS, COMMENTS_NEXT], [MESSAGES], [ASKS]],
    shown: 60,
  });
  expect(last.refill).toEqual([]);
  expect(last.left).toBe(0);
  // The post the planner quoted gives two records at its thread's place, the quoting reply's
  // first, since its own last write is the newer.
  expect(ids(last.rows.slice(40))).toEqual([
    "4cf8626f",
    "3eb7c871",
    "be5389a3",
    "49c4362a",
    "92930886",
    "6d34bb2d",
    "d70d1491",
    "21c0f166",
    "41453914",
    "c36ac28f",
    "b6099daa",
    "18ddeb81",
  ]);
});

test("three routes, one of which holds the next 20 rows alone: All reads its next page before it draws them", () => {
  // The messages moved a day later and the asks a day earlier: the messages' 17 rows and the
  // three newest comments are the first 20, and every row after them up to the asks is a
  // comment, the rest of the comments' first page and all of their second.
  const messages = movedTo(MESSAGES, "2026-11-26", "2026-11-27");
  const asks = movedTo(ASKS, "2026-11-26", "2026-11-25");
  const first = mergeThreads({ routes: [[COMMENTS], [messages], [asks]], shown: 20 });
  expect(ids(first.rows.slice(17))).toEqual(["b5979723", "0bcb3346", "b3ab4dfe"]);

  const before = mergeThreads({ routes: [[COMMENTS], [messages], [asks]], shown: 40 });
  expect(before.refill).toEqual([0]);
  expect(before.rows).toHaveLength(20);

  const after = mergeThreads({
    routes: [[COMMENTS, COMMENTS_NEXT], [messages], [asks]],
    shown: 40,
  });
  const comments = new Set(
    [...(COMMENTS.threads ?? []), ...(COMMENTS_NEXT.threads ?? [])].map((row) => row.comment.id)
  );
  const next = after.rows.slice(20);
  expect(next).toHaveLength(20);
  expect(next.every((record) => comments.has(record.thread))).toBe(true);
  expect(ids(next.slice(-3))).toEqual(["c36ac28f", "b6099daa", "18ddeb81"]);
});

test("two threads on the same last activity are drawn the larger id first", () => {
  // The planner's first question is given the reader's newest comment's time, and the newest
  // message the time of the reader's comment before it.
  const comments = COMMENTS.threads ?? [];
  const [newest, second] = comments;
  const asks = (ASKS.threads ?? []).map((row) =>
    row.id.startsWith("267776fd")
      ? { ...row, last_activity_at: newest?.last_activity_at ?? "" }
      : row
  );
  const messages = (MESSAGES.threads ?? []).map((row) =>
    row.message.id.startsWith("1d7c4317")
      ? { ...row, last_activity_at: second?.last_activity_at ?? "" }
      : row
  );
  const merged = mergeThreads({
    routes: [[COMMENTS], [{ ...MESSAGES, threads: messages }], [{ ...ASKS, threads: asks }]],
    shown: 20,
  });
  // On each tie the larger id comes first: `b5979723` before `267776fd`, and `1d7c4317` before
  // `0bcb3346`, which is the newer of the two without the tie.
  expect(ids(merged.rows.slice(0, 4))).toEqual(["b5979723", "267776fd", "1d7c4317", "0bcb3346"]);
});

test("Mine leaves out the records the reader did not write in before it takes its 20 rows", () => {
  const merged = mergeThreads({ mine: "alice", routes: MINE, shown: 20 });
  // The tester's quoting reply to the reader's message and the planner's quoting reply to the
  // reader's post begin records the reader wrote nothing in.
  expect(ids(merged.rows)).toEqual([
    "b5979723",
    "267776fd",
    "0bcb3346",
    "1d7c4317",
    "e46b9c56",
    "1ce09269",
    "727f2b9d",
    "3295fd2d",
    "0d7bfef6",
    "a59f2ee7",
    "73914aae",
    "2dc5b959",
    "4b7efdcf",
    "2e0483b8",
    "6b245020",
    "b8a85377",
    "80fbc0ff",
    "678608f6",
    "895aef15",
    "724cd9bb",
  ]);
  // 17 comment records, 14 message records and 12 ask records hold a write of the reader's.
  expect(merged.left).toBe(17 + 14 + 12 - 20);
  const last = mergeThreads({ mine: "alice", routes: MINE, shown: 60 });
  expect(ids(last.rows)).not.toContain("b788d337");
  expect(ids(last.rows)).not.toContain("b6099daa");
  expect(last.rows).toHaveLength(43);
});

test("a thread that stands on two pages of its route is drawn once, at its newest place", () => {
  // A copy of the comments' second page that holds the reader's newest comment again, a day
  // older, as a page read before a write moved that thread up would.
  const [newest] = COMMENTS.threads ?? [];
  if (newest === undefined) throw new Error("The comments' first page is empty.");
  const stale = movedTo({ ...COMMENTS_NEXT, threads: [newest] }, "2026-11-26", "2026-11-25");
  const next = {
    ...COMMENTS_NEXT,
    threads: [...(COMMENTS_NEXT.threads ?? []), ...(stale.threads ?? [])],
  };
  const merged = mergeThreads({ routes: [[COMMENTS, next], [MESSAGES], [ASKS]], shown: 60 });
  expect(ids(merged.rows).filter((id) => id === "b5979723")).toHaveLength(1);
  expect(ids(merged.rows)[0]).toBe("b5979723");
  expect(merged.rows).toHaveLength(52);
});

test("All draws no row while one of its three first pages has not answered", () => {
  const merged = mergeThreads({ routes: [[COMMENTS], [], [ASKS]], shown: 20 });
  expect(merged.rows).toEqual([]);
  expect(merged.refill).toEqual([]);
});
