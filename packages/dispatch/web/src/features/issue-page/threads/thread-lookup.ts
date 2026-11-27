/**
 * R12's lookups (dispatch://INFRA-5704/spec#b-r-addresses): the record that
 * `/threads/<id>` names where no page the view holds has it. The id is read through the three
 * routes that read one thread, `GET /api/v1/asks/{id}`, `GET /api/v1/comments/{id}` and
 * `GET /api/v1/messages/{id}`, at once, each once. A read the server refuses with a `4xx` is not
 * sent again, and one that failed past its retries is sent again only by the reader's Retry. Only
 * an answer whose `issue_key` is this issue's gives a record of this page.
 */
import {
  type QueryStatus,
  queryOptions,
  type UseQueryResult,
  useQuery,
} from "@tanstack/react-query";

import { ApiError, api } from "../../../api/client";
import { queryRetry } from "../../../api/query-client";
import { askQuery, commentQuery, EVENT_FRESH } from "../../refs/reference-target";
import { quoteOf, type ThreadRecord, type ThreadSource, threadRecords } from "./thread-records";

/** What the main column draws for an id no page holds. */
export type ThreadLookup =
  /** A page the view holds has the record, so nothing is read. */
  | { readonly kind: "held" }
  /** A lookup that can still give the record is out, or is not sent yet while a page that may
   *  hold it is out. */
  | { readonly kind: "reading" }
  /** The record, with every record of its thread on the server, which a record that a reply with
   *  a quote begins reads its thread's target from. */
  | {
      readonly kind: "found";
      readonly record: ThreadRecord;
      readonly thread: readonly ThreadRecord[];
    }
  /** The id names no record of this issue: the page's line for an address it cannot find. */
  | { readonly kind: "missing" }
  /** A read failed past its retries and no other gave the record: the page's failed-read
   *  sentence, whose Retry reads the failed routes again and keeps the others' answers. Nothing
   *  else reads a failed route again (`settledRead`). A read that Retry sent is out with no
   *  answer, so the lookup is reading again meanwhile. */
  | { readonly kind: "failed"; readonly retry: () => void };

/** A record's id as the server stores it and every address writes it: a uuid. */
const RECORD_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Whether `error` is the server's refusal of the read, which a read again would answer alike. */
function refused(error: unknown): boolean {
  return error instanceof ApiError && error.status >= 400 && error.status < 500;
}

/** A lookup is retried as every read of the page is (`queryRetry`), but on no `4xx`. */
function lookupRetry(failureCount: number, error: unknown): boolean {
  return !refused(error) && queryRetry(failureCount, error);
}

/** What a lookup's rules read of its query: its state. */
interface LookupQuery {
  readonly state: {
    readonly data: unknown;
    readonly error: unknown;
    readonly status: QueryStatus;
  };
}

/** Whether a lookup stands settled with no answer to keep fresh: refused with a `4xx`, or failed
 *  past its retries with no answer before it. Such a read is no longer enabled (R12), so window
 *  focus, an event's refresh, the stream's whole-cache refresh, and the lookups enabled again once
 *  a page the view waited on has answered, send it no more. A refusal would be answered alike, and
 *  a failure, holding no answer, would be pending while it is out, which takes the page's line
 *  away until it settles. Only the failed-read line's Retry reads a failure again (`answerOf`):
 *  TanStack Query 5 sends a `refetch` whatever `enabled` says. A read is pending, not in error,
 *  through its first try and its own retries, so it stays enabled for them. One in error that
 *  holds an earlier answer stays enabled unless refused, so an event's refresh keeps that answer
 *  fresh; it is not pending while it is out. */
function settledRead(query: LookupQuery): boolean {
  const { data, error, status } = query.state;
  return status === "error" && (refused(error) || data === undefined);
}

/** The whole conversation of the message `id`, read by any message id in it, under the issue's
 *  message key, which every message event of the issue refreshes (`api/event-refresh.ts`). */
const messageThreadQuery = (key: string, id: string | undefined) =>
  queryOptions({
    queryFn: () => api.getMessageThread(id ?? ""),
    queryKey: ["issue-message", key, "thread", id],
  });

/**
 * The record `id` names on issue `issueKey`. `held` says whether a page the view holds has it:
 * true reads nothing, false sends the lookups, and undefined, while a page that may hold it is
 * still out, sends none of them and waits for that page, so a record the first pages hold is never
 * looked up (R15). An answer the lookups already gave stands meanwhile: a record found, the line
 * for an id that names no record, or the failed read, each stays drawn while a page is read again.
 * An id that is no uuid is refused before any read, whatever the pages hold. Where the comment
 * route answers a reply with a quote, which may begin a record (R8) and whose read holds only the
 * comments whose `reply_to` names it, its thread is read once more by its `reply_to` (for a
 * comment thread) or its `ask_id` (for an ask thread), whose records give the reply's.
 */
export function useThreadLookup(
  issueKey: string,
  id: string | undefined,
  held: boolean | undefined
): ThreadLookup {
  const named = id !== undefined && RECORD_ID.test(id) ? id.toLowerCase() : undefined;
  const asked = named !== undefined && held === false;
  const lookup = {
    ...EVENT_FRESH,
    enabled: (query: LookupQuery) => asked && !settledRead(query),
    retry: lookupRetry,
  };
  const ask = useQuery({ ...askQuery(named), ...lookup });
  const comment = useQuery({ ...commentQuery(named), ...lookup });
  const message = useQuery({ ...messageThreadQuery(issueKey, named), ...lookup });
  const reply = comment.data?.comment;
  const quotedReply =
    reply !== undefined && reply.issue_key === issueKey && quoteOf(reply) !== undefined;
  const head =
    quotedReply && reply.reply_to !== null && reply.ask_id === null ? reply.reply_to : undefined;
  const replyAsk =
    quotedReply && reply.ask_id !== null && reply.reply_to === null ? reply.ask_id : undefined;
  const thread = useQuery({
    ...commentQuery(head),
    ...lookup,
    enabled: (query: LookupQuery) => asked && head !== undefined && !settledRead(query),
  });
  const askThread = useQuery({
    ...askQuery(replyAsk),
    ...lookup,
    enabled: (query: LookupQuery) => asked && replyAsk !== undefined && !settledRead(query),
  });
  if (named === undefined) return { kind: "missing" };
  if (held === true) return { kind: "held" };

  const answers: readonly Answer[] = [
    answerOf(ask, ask.data?.ask.issue_key),
    // A reply's own read names it alone, so only a thread's head gives its records here.
    answerOf(comment, reply?.reply_to === null && reply.ask_id === null ? reply.issue_key : null),
    answerOf(message, message.data?.message.issue_key),
    ...(head === undefined ? [] : [answerOf(thread, thread.data?.comment.issue_key)]),
    ...(replyAsk === undefined ? [] : [answerOf(askThread, askThread.data?.ask.issue_key)]),
  ];
  // A record found stays found while a page that may hold it is out again, from the answers the
  // client keeps.
  for (const answer of answers) {
    if (answer.source === undefined || answer.issue !== issueKey) continue;
    const records = threadRecords(answer.source);
    const record = records.find((one) => one.id === named);
    if (record !== undefined) return { kind: "found", record, thread: records };
  }
  // A lookup not sent yet, while a page that may hold the record is out, is pending as well. One
  // that has answered is not enabled while such a page is out again, so it keeps the state it
  // settled in, and the line it draws stays drawn while each event of the issue reads the list's
  // pages again.
  if (answers.some((answer) => answer.out)) return { kind: "reading" };
  const failed = answers.filter((answer) => answer.failed);
  if (failed.length === 0) return { kind: "missing" };
  return {
    kind: "failed",
    retry: () => {
      for (const answer of failed) answer.retry();
    },
  };
}

/** What one lookup's read has answered. */
interface Answer {
  readonly source: ThreadSource | undefined;
  /** The issue the answer is of, where it gives records at all. */
  readonly issue: string | null | undefined;
  /** Whether the read has no answer and no failure yet: not sent yet, while a page that may hold
   *  the record is out, or out on its first try, a retry of its own, or a read that Retry sent. */
  readonly out: boolean;
  /** Whether the read failed past its retries with no refusal of the server's. */
  readonly failed: boolean;
  readonly retry: () => void;
}

function answerOf(read: UseQueryResult<ThreadSource>, issue: string | null | undefined): Answer {
  return {
    failed: read.isError && !refused(read.error),
    issue,
    // Pending holds neither an answer nor a failure: TanStack Query 5 makes a read with no
    // answer pending again as Retry sends it.
    out: read.isPending,
    retry: () => void read.refetch(),
    source: read.data,
  };
}
