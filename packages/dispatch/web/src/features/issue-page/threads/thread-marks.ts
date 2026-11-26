/**
 * R7's count (dispatch://INFRA-5704/spec#b-r-row): the mark a row draws, the number of writes in
 * its record that the reader has not read. Which events are writes, and who wrote in a thread,
 * follow the server (plan choice 8, and the event table of `fixtures/README.md`).
 *
 * The server keeps a read mark only of a thread a person wrote in (`user_threads`, migration
 * `0095_user_threads`), and `me/replies` gives it as a row's `read_through`. For every other
 * thread the one read of what is new is the reader's last read of the issue, `last_read_seq`,
 * against the issue's newest 200 events (the spec's Design, "Which reads give the mark").
 */
import type { EventPages } from "../../../api/event-feed";
import type { DispatchEvent, MyRepliesResponse, UserState } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import { stateForIssue } from "../../issue/pins";
import { isViewer } from "../../refs/actor";
import { EVENTS_PAGE } from "./thread-reads";
import type { ThreadRecord } from "./thread-records";

/** What the count of one record reads of it. */
export type MarkedRecord = Pick<ThreadRecord, "thread" | "threadWriters" | "waitsOn" | "writes">;

/** The reads the count is read from. */
export interface MarkReads {
  readonly issueKey: string;
  /** Whether the issue is open: an open ask of a closed issue is a thread of `me/replies` again
   *  (`myRepliesOwner` in `api/replies.go`). */
  readonly issueOpen: boolean;
  /** The reader's login. */
  readonly reader: string;
  /** The reader's unread threads of the issue, every one (`dashboardRepliesQuery`, R6). */
  readonly replies: MyRepliesResponse;
  /** The reader's state of each issue (`userStateQuery`). */
  readonly state: UserState;
  /** The events read (`threadEventsQuery`). */
  readonly events: EventPages;
}

/** The entry a write event names, by the id its payload gives: a comment's, a message's, or an
 *  ask's for the ask, its answer and its resolution. An edit, a delete, a delivery and an
 *  acceptance name the same ids and are no writes. No other event names an entry. */
function writtenEntry(event: DispatchEvent): string | undefined {
  switch (event.type) {
    case "ask.answered":
    case "ask.opened":
    case "ask.resolved":
    case "comment.answered":
    case "comment.created":
    case "message.answered":
    case "message.created":
      return event.payload.id;
    default:
      return undefined;
  }
}

/**
 * The mark the row of `record` draws, or undefined where it counts no write (R7): the number,
 * and `+` after it where the count may miss older writes, such as `3+`. A write of the reader is
 * never counted, and an edit is no write. The writer of a write event is its actor, whom the
 * server appends it under: the author of a comment or a message, and the person who answered.
 *
 * A record whose thread on the server holds a write of the reader's counts the writes of others
 * past the `read_through` of that thread's `me/replies` row, and none where the read lists no row:
 * a record that a reply with a quote begins reads its post's row, and counts only its own writes
 * (plan choice 11). Every other record, and the open ask on an open issue that waits on the
 * reader, which `me/replies` leaves out, counts its write events past the reader's
 * `last_read_seq` of the issue, 0 where `me/state` holds no state for it, among the events read's
 * first page. Where that page holds 200 events and the oldest of them is past `last_read_seq`, a
 * write can stand below it, and the count carries `+`. The issue's events are numbered from 1
 * with no gap (`events/broker.go`), so a page whose oldest event is the issue's first holds the
 * whole log, however many events the feed's prepends put above it, and draws no `+`.
 */
export function recordMark(record: MarkedRecord, reads: MarkReads): string | undefined {
  const { reader } = reads;
  const waitsOnReader = reads.issueOpen && record.waitsOn?.toLowerCase() === reader.toLowerCase();
  if (!waitsOnReader && record.threadWriters.some((writer) => isViewer(writer, reader))) {
    const row = reads.replies.rows?.find((one) => one.thread_id === record.thread);
    const through = row?.read_through;
    if (row === undefined || !row.unread || through === undefined) return undefined;
    const count = record.writes.filter(
      (write) => !isViewer(write.author, reader) && compareTimestamps(write.at, through) > 0
    ).length;
    return count === 0 ? undefined : String(count);
  }
  const lastRead = stateForIssue(reads.state, reads.issueKey).last_read_seq;
  const first = reads.events.pages[0] ?? [];
  const entries = new Set(record.writes.map((write) => write.id));
  const count = first.filter((event) => {
    const entry = writtenEntry(event);
    return (
      event.seq > lastRead &&
      entry !== undefined &&
      entries.has(entry) &&
      !isViewer(event.actor, reader)
    );
  }).length;
  if (count === 0) return undefined;
  const oldest = first.at(-1)?.seq ?? 0;
  const more = first.length >= EVENTS_PAGE && oldest > lastRead && oldest > 1;
  return more ? `${count}+` : String(count);
}
