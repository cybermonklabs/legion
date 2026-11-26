/**
 * R12's lookups (dispatch://INFRA-5704/spec#b-r-addresses, plan choice 9): the record that
 * `/threads/<id>` names where no page the view holds has it. The id is read through the three
 * routes that read one thread, `GET /api/v1/asks/{id}`, `GET /api/v1/comments/{id}` and
 * `GET /api/v1/messages/{id}`, at once, each once, and a read the server refuses with a `4xx` is
 * not sent again. Only an answer whose `issue_key` is this issue's gives a record of this page.
 */
import { queryOptions, type UseQueryResult, useQuery } from "@tanstack/react-query";

import { ApiError, api } from "../../../api/client";
import { queryRetry } from "../../../api/query-client";
import { askQuery, commentQuery, EVENT_FRESH } from "../../refs/reference-target";
import { type ThreadRecord, type ThreadSource, threadRecords } from "./thread-records";

/** What the main column draws for an id no page holds. */
export type ThreadLookup =
  /** A page the view holds has the record, so nothing is read. */
  | { readonly kind: "held" }
  /** A read that can still give the record is out. */
  | { readonly kind: "reading" }
  | { readonly kind: "found"; readonly record: ThreadRecord }
  /** The id names no record of this issue: the page's line for an address it cannot find. */
  | { readonly kind: "missing" }
  /** A read failed past its retries and no other gave the record: the page's failed-read
   *  sentence, whose Retry reads the failed routes again and keeps the others' answers. */
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

/** The whole conversation of the message `id`, read by any message id in it, under the issue's
 *  message key, which every message event of the issue refreshes (`api/event-refresh.ts`). */
const messageThreadQuery = (key: string, id: string | undefined) =>
  queryOptions({
    queryFn: () => api.getMessageThread(id ?? ""),
    queryKey: ["issue-message", key, "thread", id],
  });

/**
 * The record `id` names on issue `issueKey`, where `held` says no page the view holds has it. An
 * id that is no uuid is refused before any read. Where the comment route answers a reply with a
 * quote, which may begin a record (R8) and whose read holds only the comments whose `reply_to`
 * names it, its thread is read once more by that `reply_to`, whose records give the reply's.
 */
export function useThreadLookup(
  issueKey: string,
  id: string | undefined,
  held: boolean
): ThreadLookup {
  const named = id !== undefined && RECORD_ID.test(id) ? id.toLowerCase() : undefined;
  const asked = named !== undefined && !held;
  const lookup = { ...EVENT_FRESH, enabled: asked, retry: lookupRetry };
  const ask = useQuery({ ...askQuery(named), ...lookup });
  const comment = useQuery({ ...commentQuery(named), ...lookup });
  const message = useQuery({ ...messageThreadQuery(issueKey, named), ...lookup });
  const reply = comment.data?.comment;
  const head =
    reply !== undefined &&
    reply.issue_key === issueKey &&
    reply.reply_to !== null &&
    (reply.reply_block?.quote ?? "") !== ""
      ? reply.reply_to
      : undefined;
  const thread = useQuery({
    ...commentQuery(head),
    ...lookup,
    enabled: asked && head !== undefined,
  });
  if (held) return { kind: "held" };
  if (named === undefined) return { kind: "missing" };

  const answers: readonly Answer[] = [
    answerOf(ask, ask.data?.ask.issue_key),
    // A reply's own read names it alone, so only a thread's head gives its records here.
    answerOf(comment, reply?.reply_to === null && reply.ask_id === null ? reply.issue_key : null),
    answerOf(message, message.data?.message.issue_key),
    ...(head === undefined ? [] : [answerOf(thread, thread.data?.comment.issue_key)]),
  ];
  for (const answer of answers) {
    if (answer.source === undefined || answer.issue !== issueKey) continue;
    const record = threadRecords(answer.source).find((one) => one.id === named);
    if (record !== undefined) return { kind: "found", record };
  }
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
  /** Whether the read is still out with no answer: its first try, a retry of its own, or one
   *  that Retry started. */
  readonly out: boolean;
  /** Whether the read failed past its retries with no refusal of the server's. */
  readonly failed: boolean;
  readonly retry: () => void;
}

function answerOf(read: UseQueryResult<ThreadSource>, issue: string | null | undefined): Answer {
  return {
    failed: read.isError && !read.isFetching && !refused(read.error),
    issue,
    out: read.data === undefined && (read.isPending || read.isFetching),
    retry: () => void read.refetch(),
    source: read.data,
  };
}
