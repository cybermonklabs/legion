import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { correction, DELETION, EDITED_AT } from "../__tests__/correction-feed";
import { messageCorrectionFields } from "../__tests__/message-fixture";
import { markCorrectedPost } from "./corrected-post";
import type { MessageRead, MessageWithTrees } from "./types";

// R2 and R3 of dispatch://INFRA-1523, and the architect's decision 4 of 2026-11-19 22:21Z: a read a
// correction refreshes keeps its answer when the refetch fails, so the feed writes the correction's
// own facts into every copy of the post a surface draws with no error of its own
// (`markCorrectedPost`). The new issue page draws the issue's messages from `["issue-messages",
// key]` (`useIssueTree.ts`), and shows no error when that read fails, so after a failed refetch its
// row of a deleted message gives the message's old words, and of an edited one no edited mark. The
// margin's pinned copy (`["issue-messages", key, "margin-pinned", …]`) is written, which is the
// control: the rows below fail on the issue's own list alone.

const MESSAGE = "3d4e5f6a-7b8c-4d9e-8f1a-2b3c4d5e6f7a";
const OLD_WORDS = "Ship on Friday.";

const message: MessageWithTrees = {
  author: { id: "alice", kind: "user" },
  body: OLD_WORDS,
  broadcast_id: null,
  created_at: "2026-09-10T00:00:00Z",
  deliveries: [],
  id: MESSAGE,
  in_reply_to: null,
  issue_key: "CORE-1",
  reply_block: null,
  target: null,
  ...messageCorrectionFields(),
};

const LIST = ["issue-messages", "CORE-1"];
const PINNED = ["issue-messages", "CORE-1", "margin-pinned", [MESSAGE]];

/** A page's cache holding the message in the issue's list and in the margin's pinned copy. */
function cached(): QueryClient {
  const client = new QueryClient();
  const read = { message, replies: [] } as unknown as MessageRead;
  client.setQueryData(LIST, [read]);
  client.setQueryData(PINNED, [read]);
  return client;
}

function messageIn(client: QueryClient, key: readonly unknown[]) {
  const reads = client.getQueryData<MessageRead[]>(key);
  const read = reads?.[0] as unknown as { message: MessageWithTrees } | undefined;
  const { body, deleted_at, edited_at } = read?.message ?? {};
  return { body, deleted_at, edited_at };
}

test("a message's delete writes its mark into the issue's message list, as into the pinned copy", () => {
  const client = cached();
  markCorrectedPost(client, correction("message", MESSAGE, false));
  expect({ list: messageIn(client, LIST), pinned: messageIn(client, PINNED) }).toEqual({
    list: { body: "", deleted_at: DELETION.deleted_at, edited_at: null },
    pinned: { body: "", deleted_at: DELETION.deleted_at, edited_at: null },
  });
});

test("a message's edit writes its time into the issue's message list, as into the pinned copy", () => {
  const client = cached();
  markCorrectedPost(client, correction("message", MESSAGE, true));
  expect({ list: messageIn(client, LIST), pinned: messageIn(client, PINNED) }).toEqual({
    list: { body: OLD_WORDS, deleted_at: null, edited_at: EDITED_AT },
    pinned: { body: OLD_WORDS, deleted_at: null, edited_at: EDITED_AT },
  });
});

// The Agents page's reply composer quotes the message it answers from `["agents", session,
// "messages"]` (`AgentsPage.tsx`, `replyParentOf(…, messages.data)`), and stays open with that
// quote when the conversation's refetch fails. An issue message targeted at a session is listed
// there, so its delete must mark that copy too.
const AGENT = ["agents", "019a1c01-0d7b-7a2a-8e93-000000000a01", "messages"];

test("a message's delete writes its mark into the Agents page's copy of the session's messages", () => {
  const client = cached();
  client.setQueryData(AGENT, [{ message, replies: [] } as unknown as MessageRead]);
  markCorrectedPost(client, correction("message", MESSAGE, false));
  expect({ agent: messageIn(client, AGENT), pinned: messageIn(client, PINNED) }).toEqual({
    agent: { body: "", deleted_at: DELETION.deleted_at, edited_at: null },
    pinned: { body: "", deleted_at: DELETION.deleted_at, edited_at: null },
  });
});
