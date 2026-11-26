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
 * (7)), or undefined where the message opens as on `main`. A message opens in the view that draws
 * it, and in Threads only where no view draws it. On this part's base the outline leaves out every
 * thread that only agents wrote in (`agents-only.ts`), and no other view draws a message, so a
 * message of a thread between agents and one of an agent's note open in Threads. A message of a
 * thread a person wrote in is the outline's, and one of a thread the Legion daemon began opens
 * where `main` opens it, so neither opens here.
 *
 * `messages` is the issue's conversation as the outline reads it, each root with its replies,
 * undefined while that read is out.
 */
export function messageRecord(
  id: string,
  messages: readonly MessageRead[] | undefined,
  project: string
): string | undefined {
  const thread = messages?.find(
    ({ message, replies }) => message.id === id || (replies ?? []).some((reply) => reply.id === id)
  );
  if (thread === undefined || isDaemon(thread.message.author, project)) return undefined;
  if ([thread.message, ...(thread.replies ?? [])].some((turn) => turn.author.kind === "user")) {
    return undefined;
  }
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
