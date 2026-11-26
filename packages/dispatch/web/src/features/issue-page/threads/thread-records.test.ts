import { expect, test } from "bun:test";
import type {
  AskThreadPage,
  AskThreadRow,
  CommentThreadPage,
  CommentThreadRow,
  MessageThreadPage,
  MessageThreadRow,
} from "@legion/contracts";

import type { AskRead, CommentRead } from "../../../api/types";
import askBody from "./fixtures/ask.json";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentReplyToBody from "./fixtures/comment-reply-to.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneBody from "./fixtures/messages-none.json";
import { type ThreadRecord, threadRecords } from "./thread-records";

// The pages the server answered at the stack base (fixtures/README.md).
const COMMENT_ROWS = [
  ...((commentsInBody as unknown as CommentThreadPage).threads ?? []),
  ...((commentsInBeforeBody as unknown as CommentThreadPage).threads ?? []),
];
const MESSAGE_ROWS = [
  ...((messagesInBody as unknown as MessageThreadPage).threads ?? []),
  ...((messagesNoneBody as unknown as MessageThreadPage).threads ?? []),
];
const ASK_ROWS = (asksThreadBody as unknown as AskThreadPage).threads ?? [];

function commentRow(prefix: string): CommentThreadRow {
  const row = COMMENT_ROWS.find((one) => one.comment.id.startsWith(prefix));
  if (row === undefined) throw new Error(`No comment page of the fixtures holds ${prefix}.`);
  return row;
}

function messageRow(prefix: string): MessageThreadRow {
  const row = MESSAGE_ROWS.find((one) => one.message.id.startsWith(prefix));
  if (row === undefined) throw new Error(`No message page of the fixtures holds ${prefix}.`);
  return row;
}

function askRow(prefix: string): AskThreadRow {
  const row = ASK_ROWS.find((one) => one.id.startsWith(prefix));
  if (row === undefined) throw new Error(`No ask page of the fixtures holds ${prefix}.`);
  return row;
}

/** Each record's id, with the ids of its replies, each cut to its first eight characters. */
function shape(records: readonly ThreadRecord[]) {
  return records.map((record) => ({
    id: record.id.slice(0, 8),
    replies: record.replies.map((reply) => reply.id.slice(0, 8)),
  }));
}

test("a reply with a quote on a post begins a record of its own, on text, in the post's thread", () => {
  const [quoting, post] = threadRecords(commentRow("18ddeb81"));
  expect(quoting?.id.slice(0, 8)).toBe("b6099daa");
  expect(quoting?.onText).toBe(true);
  expect(quoting?.quote).toBe("twenty threads");
  expect(post?.id.slice(0, 8)).toBe("18ddeb81");
  expect(post?.onText).toBe(false);
  expect(quoting?.thread).toBe(post?.thread ?? "");
  expect(quoting?.thread.slice(0, 8)).toBe("18ddeb81");
});

test("the comment that answers a quoting reply is in the quoting reply's record and not in the post's", () => {
  expect(shape(threadRecords(commentRow("18ddeb81")))).toEqual([
    { id: "b6099daa", replies: ["fa70efe5"] },
    { id: "18ddeb81", replies: ["718e3e52"] },
  ]);
});

test("each record of a thread that holds two draws its own last activity, the newest record first", () => {
  const [quoting, post] = threadRecords(commentRow("18ddeb81"));
  // The reviewer's answer to the quoting reply is the thread's last write, and the colleague's
  // reply is the post's.
  expect(quoting?.lastActivity).toBe(commentRow("18ddeb81").last_activity_at);
  expect(post?.lastActivity).toBe(post?.replies[0]?.created_at ?? "");
});

test("a reply with a quote to a record on text already joins that record", () => {
  const records = threadRecords(commentRow("c1d04870"));
  expect(shape(records)).toEqual([{ id: "c1d04870", replies: ["39c39145", "b213f6e9"] }]);
  expect(records[0]?.onText).toBe(true);
  expect(records[0]?.quote).toBe(commentRow("c1d04870").comment.anchor?.quote);
});

test("a reply with a quote to a record that a quoting reply began joins that record", () => {
  // A copy of the post's thread whose reviewer's answer to the planner's quoting reply quotes a
  // block of it, with the planner's own reply block.
  const row = commentRow("18ddeb81");
  const block = row.replies?.find((reply) => reply.id.startsWith("b6099daa"))?.reply_block;
  const quotes = {
    ...row,
    replies: (row.replies ?? []).map((reply) =>
      reply.id.startsWith("fa70efe5") ? { ...reply, reply_block: block ?? null } : reply
    ),
  };
  expect(shape(threadRecords(quotes))).toEqual([
    { id: "b6099daa", replies: ["fa70efe5"] },
    { id: "18ddeb81", replies: ["718e3e52"] },
  ]);
});

test("answers in a chain stand in one record of a message thread, at every depth", () => {
  const records = threadRecords(messageRow("2dc5b959"));
  expect(shape(records)).toEqual([
    { id: "2dc5b959", replies: ["077c7fbb", "5dde05a0", "5d027993", "ada59c63"] },
  ]);
});

test("answers in a chain that leads to a quoting reply stand in that reply's record", () => {
  // A copy of the thread whose planner's first reply quotes a block of the reader's message,
  // with the tester's reply block of the other thread: the reader's answer to it and the
  // planner's two answers to the reader lead to it.
  const row = messageRow("2dc5b959");
  const block = messageRow("0d7bfef6").replies?.[0]?.reply_block ?? null;
  const quoted = {
    ...row,
    replies: (row.replies ?? []).map((reply) =>
      reply.id.startsWith("077c7fbb") ? { ...reply, reply_block: block } : reply
    ),
  };
  expect(shape(threadRecords(quoted))).toEqual([
    { id: "077c7fbb", replies: ["5dde05a0", "5d027993", "ada59c63"] },
    { id: "2dc5b959", replies: [] },
  ]);
});

test("a message thread is one record of its root and every message under it", () => {
  const row = messageRow("2dc5b959");
  const [only] = threadRecords(row);
  expect(only?.kind).toBe("message");
  expect(only?.thread).toBe(row.message.id);
  expect(only?.lastActivity).toBe(row.last_activity_at);
  expect(only?.writes.map((write) => write.id)).toEqual([
    row.message.id,
    ...(row.replies ?? []).map((reply) => reply.id),
  ]);
});

test("a message reply with a quote begins a record of its own in its root's thread", () => {
  const row = messageRow("0d7bfef6");
  const records = threadRecords(row);
  expect(shape(records)).toEqual([
    { id: "b788d337", replies: [] },
    { id: "0d7bfef6", replies: [] },
  ]);
  const [quoting, root] = records;
  expect(quoting?.kind).toBe("message");
  expect(quoting?.onText).toBe(true);
  expect(quoting?.quote).toBe("cut at a word");
  expect(quoting?.thread).toBe(row.message.id);
  expect(root?.lastActivity).toBe(row.message.created_at);
});

test("a deleted entry stays in its record, and its time counts toward the record's last activity", () => {
  const row = commentRow("c36ac28f");
  const deleted = (row.replies ?? []).find((reply) => reply.deleted_at !== null);
  expect(deleted).toBeDefined();
  const [record] = threadRecords(row);
  expect(record?.replies.map((reply) => reply.id)).toContain(deleted?.id ?? "");
  expect(record?.lastActivity).toBe(deleted?.created_at ?? "");
  // Read as one thread by its id, which carries no `last_activity_at`, the record finds the same
  // time from its own writes.
  const { last_activity_at: _, ...read } = row;
  expect(threadRecords(read)[0]?.lastActivity).toBe(deleted?.created_at ?? "");
});

test("an ask's record is the ask, its comments and its answer, each a write in time order", () => {
  const row = askRow("59ab65ed");
  const [record] = threadRecords(row);
  expect(record?.kind).toBe("ask");
  expect(record?.thread).toBe(row.id);
  expect(record?.lastActivity).toBe(row.last_activity_at);
  expect(record?.replies.map((reply) => reply.id.slice(0, 8))).toEqual(["1fd5ce6a", "06a1f394"]);
  expect(record?.writes.map((write) => [write.id.slice(0, 8), write.author.id])).toEqual([
    ["59ab65ed", "threads-planner"],
    ["59ab65ed", "alice"],
    ["1fd5ce6a", "threads-planner"],
    ["06a1f394", "alice"],
  ]);
  expect(record?.waitsOn).toBeUndefined();
});

test("each record of an open ask's thread names the person the ask waits on", () => {
  expect(threadRecords(askRow("4b7efdcf"))[0]?.waitsOn).toBe("alice");
});

test("the read of one ask by its id gives the record its page row gives", () => {
  const fromRead = threadRecords(askBody as unknown as AskRead);
  const fromPage = threadRecords(askRow("4b7efdcf"));
  expect(shape(fromRead)).toEqual(shape(fromPage));
  expect(fromRead[0]?.writes).toEqual(fromPage[0]?.writes ?? []);
  expect(fromRead[0]?.lastActivity).toBe(fromPage[0]?.lastActivity ?? "");
});

test("the read of a post by its id gives the records its page row gives", () => {
  const fromRead = threadRecords(commentReplyToBody as unknown as CommentRead);
  expect(shape(fromRead)).toEqual(shape(threadRecords(commentRow("18ddeb81"))));
});

test("the people who wrote in a thread are those the server keeps a read mark for, the same on each of its records", () => {
  // The server's triggers keep a mark for a person alone: the agents who replied hold none.
  const [quoting, post] = threadRecords(commentRow("18ddeb81"));
  const writers = (record: ThreadRecord | undefined) =>
    record?.threadWriters.map((actor) => actor.id) ?? [];
  expect(writers(quoting)).toEqual(["alice", "bob"]);
  expect(writers(post)).toEqual(writers(quoting));
  // The planner asked, and the reader replied and answered: an answer is a write in the thread.
  expect(writers(threadRecords(askRow("59ab65ed"))[0])).toEqual(["alice"]);
  // The colleague began the thread the reader replied in.
  expect(writers(threadRecords(messageRow("cc32fc53"))[0])).toEqual(["bob", "alice"]);
});

test("no entry of any page is in two records, and every entry is in one", () => {
  const sources = [...COMMENT_ROWS, ...MESSAGE_ROWS, ...ASK_ROWS];
  for (const source of sources) {
    const records = threadRecords(source);
    const held = records.flatMap((record) => [
      ...(record.kind === "ask" ? [] : [record.root.id]),
      ...record.replies.map((reply) => reply.id),
    ]);
    const entries =
      "comment" in source
        ? [source.comment.id, ...(source.replies ?? []).map((reply) => reply.id)]
        : "message" in source
          ? [source.message.id, ...(source.replies ?? []).map((reply) => reply.id)]
          : (source.thread?.replies ?? []).map((reply) => reply.id);
    expect([...held].sort()).toEqual([...entries].sort());
  }
});
