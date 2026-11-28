/**
 * Where an address opens a record of Threads (dispatch://INFRA-5704/spec#b-r-addresses, R12):
 * the choice a record opens under, the record that `/messages/<id>` opens in Threads, and the
 * entry such an address focuses as the record opens.
 *
 * Pure: it makes no request, so the view and the outline's fallback (`address-target.ts`) read
 * one rule.
 */
import type { MessageRead } from "../../../api/types";
import { isDaemon } from "../../dashboard/useWaitingItems";
import { isThreadsOnly } from "../agents-only";
import type { ThreadChoice } from "./ThreadChoices";
import { type ThreadRecord, threadRecords } from "./thread-records";

/**
 * The choice `record` opens under (R12): Agents for a thread between agents, and All for every
 * other record. A thread between agents is the one Agents lists (R5): a message thread whose root
 * targets an agent, which holds no entry a person wrote, and whose root the Legion daemon of the
 * issue's project did not write.
 */
export function recordChoice(record: ThreadRecord, project: string): ThreadChoice {
  return record.kind === "message" &&
    record.threadWriters.length === 0 &&
    record.root.target !== null &&
    !isDaemon(record.root.author, project)
    ? "agents"
    : "all";
}

/**
 * The record `/messages/<id>` opens in Threads, with the message `id` focused (R12, decision 501
 * (7)), or undefined where the message opens as on `main`. A message opens in Threads where its
 * thread is one only Threads draws (`agents-only.ts`'s `isThreadsOnly`); a message of any other
 * thread (a person wrote in it, or the Legion daemon began it) opens as on `main`.
 *
 * `messages` is the issue's conversation as the outline reads it, each root with its replies,
 * undefined while that read is out. `_project`, the issue's project, which `main`'s callers pass,
 * is read no more: `isThreadsOnly` knows the Legion daemon by its session's prefix.
 */
export function messageRecord(
  id: string,
  messages: readonly MessageRead[] | undefined,
  _project: string
): string | undefined {
  const thread = messages?.find(
    ({ message, replies }) => message.id === id || (replies ?? []).some((reply) => reply.id === id)
  );
  if (thread === undefined || !isThreadsOnly(thread)) return undefined;
  return threadRecords(thread).find(
    (record) =>
      record.root.id === id || record.replies.some((reply: { id: string }) => reply.id === id)
  )?.id;
}

/** The history state of an address that opens a record with one of its entries focused: the
 *  record `/messages/<id>` opens in Threads (`address-target.ts`). It is the history entry's own,
 *  so Back and Forward to that entry focus the entry again, and an address the reader follows
 *  later carries none. */
export interface EntryFocus {
  readonly threadEntry: string;
}

/** The entry the history state `state` focuses, or undefined where it names none. */
export function focusedEntry(state: unknown): string | undefined {
  return typeof state === "object" &&
    state !== null &&
    "threadEntry" in state &&
    typeof state.threadEntry === "string"
    ? state.threadEntry
    : undefined;
}
