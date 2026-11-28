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
 * Whether `thread` is on one of root R40's routes to Threads: a thread between agents, which
 * Agents' `person=none` lists (a root that targets an agent, and no person wrote in the thread),
 * or an agent's note to the issue's people, which All lists (a root with no target that no person
 * wrote, whoever replied). It does not read who the agent is: the caller sets the Legion daemon's
 * thread aside first.
 */
function isAgentThread({ message, replies }: MessageRead): boolean {
  if (message.target === null) return message.author.kind !== "user";
  return [message, ...(replies ?? [])].every((entry) => entry.author.kind !== "user");
}

/**
 * The record `/messages/<id>` opens in Threads, with the message `id` focused (R12, decision 501
 * (7)), or undefined where the message opens as on `main`. A message opens in Threads where its
 * thread is on one of root R40's routes (`isAgentThread`): Agents' `person=none`, a root that
 * targets an agent and that no person wrote in, and All's agent's note, a root with no target
 * that an agent other than the Legion daemon wrote, whoever replied. A message of a thread the
 * Legion daemon began, and one of any other thread (a thread a person began, or a thread to an
 * agent that a person wrote in), opens as on `main`, so none of them opens here. The history's
 * rule (`agents-only.ts`'s `agentsOnlyIds`, a thread the daemon began and no other) is a separate
 * one, and this does not read it.
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
  if (!isAgentThread(thread)) return undefined;
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
