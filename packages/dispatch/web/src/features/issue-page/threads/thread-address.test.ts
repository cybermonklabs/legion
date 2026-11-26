import { describe, expect, test } from "bun:test";
import type { MessageThreadPage } from "@legion/contracts";

import type { MessageRead } from "../../../api/types";
import messagesBareBody from "./fixtures/messages-bare.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneBody from "./fixtures/messages-none.json";
import { focusedEntry, messageRecord, recordChoice } from "./thread-address";
import { threadRecords } from "./thread-records";

/**
 * Where an address opens a record of Threads (dispatch://INFRA-5704/spec#b-r-addresses, R12),
 * over the server's answers captured at the stack base (`fixtures/README.md`): the choice a record
 * opens under, and the record `/messages/<id>` opens in Threads.
 */

const PROJECT = "PDFD720CB";
/** The issue's conversation as the outline reads it: every root with its replies. */
const CONVERSATION = messagesBareBody as unknown as MessageRead[];
const AGENTS_PAGE = messagesNoneBody as unknown as MessageThreadPage;
const IN_PAGE = messagesInBody as unknown as MessageThreadPage;

function threadOf(test: (thread: MessageRead) => boolean, what: string): MessageRead {
  const thread = CONVERSATION.find(test);
  if (thread === undefined) throw new Error(`The captured conversation holds no ${what}.`);
  return thread;
}

/** The thread an agent began to another agent's session, which only agents wrote in. */
const BETWEEN_AGENTS = threadOf(
  ({ message }) => message.author.kind === "session" && message.target !== null,
  "thread between agents"
);
/** The agent's note: a root with no target that no person answered. */
const NOTE = threadOf(
  ({ message, replies }) =>
    message.author.kind === "session" &&
    message.target === null &&
    !message.author.id.startsWith("legion-daemon:") &&
    (replies ?? []).length === 0,
  "agent's note"
);
/** The thread the Legion daemon began, which the reader replied in. */
const DAEMON = threadOf(
  ({ message }) => message.author.id === `legion-daemon:${PROJECT}`,
  "thread the Legion daemon began"
);
/** A thread the reader began, which an agent answered. */
const PERSON = threadOf(
  ({ message, replies }) => message.author.kind === "user" && (replies ?? []).length > 0,
  "thread a person began"
);

describe("the choice a record opens under", () => {
  test("a thread between agents opens under Agents", () => {
    const row = AGENTS_PAGE.threads?.[0];
    expect(row?.message.id).toBe(BETWEEN_AGENTS.message.id);
    const records = row === undefined ? [] : threadRecords(row);
    expect(records.map((record) => recordChoice(record, PROJECT))).toEqual(["agents"]);
  });

  test("an agent's note and a thread a person wrote in open under All", () => {
    for (const row of IN_PAGE.threads ?? []) {
      for (const record of threadRecords(row)) {
        expect(recordChoice(record, PROJECT), `${record.id} opens under Agents`).toBe("all");
      }
    }
    expect(
      (IN_PAGE.threads ?? []).some((row) => row.message.id === NOTE.message.id),
      "All's page holds no agent's note"
    ).toBe(true);
  });

  test("a thread the Legion daemon began that only agents wrote in opens under All", () => {
    const agentsOnly: MessageRead = {
      ...DAEMON,
      replies: (DAEMON.replies ?? []).filter((reply) => reply.author.kind !== "user"),
    };
    expect(threadRecords(agentsOnly).map((record) => recordChoice(record, PROJECT))).toEqual([
      "all",
    ]);
  });
});

describe("the record /messages/<id> opens in Threads", () => {
  test("a reply of a thread between agents opens its thread's record", () => {
    const reply = BETWEEN_AGENTS.replies?.[0]?.id ?? "";
    expect(messageRecord(reply, CONVERSATION, PROJECT)).toBe(BETWEEN_AGENTS.message.id);
    expect(messageRecord(BETWEEN_AGENTS.message.id, CONVERSATION, PROJECT)).toBe(
      BETWEEN_AGENTS.message.id
    );
  });

  test("an agent's note opens its own record", () => {
    expect(messageRecord(NOTE.message.id, CONVERSATION, PROJECT)).toBe(NOTE.message.id);
  });

  test("a message of a thread a person wrote in opens as on main", () => {
    for (const message of [PERSON.message, ...(PERSON.replies ?? [])]) {
      expect(messageRecord(message.id, CONVERSATION, PROJECT), message.id).toBeUndefined();
    }
  });

  test("a message of a thread the Legion daemon began opens as on main, whoever wrote in it", () => {
    const agentsOnly: MessageRead = {
      ...DAEMON,
      replies: (DAEMON.replies ?? []).filter((reply) => reply.author.kind !== "user"),
    };
    for (const conversation of [CONVERSATION, [agentsOnly]]) {
      expect(messageRecord(DAEMON.message.id, conversation, PROJECT)).toBeUndefined();
    }
  });

  test("a message the conversation does not hold, or a conversation not read yet, opens nothing", () => {
    expect(messageRecord("00000000-0000-4000-8000-000000000000", CONVERSATION, PROJECT)).toBe(
      undefined
    );
    expect(messageRecord(NOTE.message.id, undefined, PROJECT)).toBeUndefined();
  });
});

test("the history state names the entry it focuses, and any other state names none", () => {
  expect(focusedEntry({ threadEntry: NOTE.message.id })).toBe(NOTE.message.id);
  for (const state of [null, undefined, "note", { threadEntry: 7 }, { entry: NOTE.message.id }]) {
    expect(focusedEntry(state)).toBeUndefined();
  }
});
