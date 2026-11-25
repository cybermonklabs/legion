import { meInboxThreads } from "@legion/contracts";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { type RefObject, useMemo } from "react";

import { meInboxDismissedQuery, meInboxQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";
import type { InboxShow } from "../refs/routes";
import type { InboxListRow } from "./list-row";
import { useMotionHold } from "./motion-hold";

/** The thread side of the Inbox: the rows it lists, the mark a write over that list carries, and
 *  the page behind **Show more** where the list is paged. */
export interface InboxThreadRows {
  /** Whether `rows` are an answer of the read they are listed from, so a count taken over them
   *  counts what that read holds: false while that read is out, was refused with no answer before
   *  it, or is not asked (until the sign-in read says the reader is a person). A count over them is
   *  drawn only once that read has answered. The Inbox waits for the one read's `inbox` state
   *  before it draws its bar, but not for its `dismissed` state, which the Dismissed view lists.
   *  In the Dismissed view the read is the pages read so far, which are the rows the list holds
   *  and a filter narrows: it has answered once its first page is in, and a count counts those
   *  pages while **Show more** has more to read. Always where the list is narrowed to asks, which
   *  lists no thread whatever the read says. A read that answered once keeps its answer through a
   *  later refresh that fails. */
  readonly answered: boolean;
  /** The read mark of the list read these rows came from, which **Mark all read** and a
   *  **Select all** dismiss both carry. */
  readonly asOf: string | undefined;
  readonly rows: readonly InboxListRow[];
  /** Reads the next page of the Dismissed view, or undefined where there is none to read: the
   *  Inbox's own list is unpaged, and the last dismissed page names no next cursor. */
  readonly showMore: (() => void) | undefined;
  /** Whether the page **Show more** asks for is already on its way. */
  readonly showingMore: boolean;
}

/**
 * The reader's own threads as Inbox rows: the one read's `inbox` state, unpaged, its `dismissed`
 * state, paged, and the pointer hold over whichever is showing.
 *
 * A thread stays in the Inbox, read or not, until it is dismissed - by **Dismiss**, or by the
 * reader's own answer or reply in it - so the `inbox` state answers for every row the list draws
 * and reading one takes nothing away. The `dismissed` state answers newest dismissal first,
 * `INBOX_DISMISSED_PAGE` entries at a time, and **Show more** follows its cursor.
 */
export function useInboxThreadRows({
  asksOnly,
  listRef,
  show,
}: {
  /** Whether this list is narrowed to asks (an agent filter, or `?section=needs-you`), which
   *  lists no thread row at all. */
  asksOnly: boolean;
  /** The list the pointer travels across, which the motion hold watches. */
  listRef: RefObject<HTMLElement | null>;
  /** Which of the reader's two lists is showing. */
  show: InboxShow;
}): InboxThreadRows {
  // The reader's own threads. The route needs the `person` scope, which a signed-in browser
  // holds, so the list only asks once the sign-in read says the principal is a user; no read is
  // made for the view that is not showing.
  const human = useIsHuman();
  const listing = human && !asksOnly;
  const inbox = useQuery({ ...meInboxQuery(), enabled: listing && show === "inbox" });
  const dismissed = useInfiniteQuery({
    ...meInboxDismissedQuery(),
    enabled: listing && show === "dismissed",
  });
  const pages = dismissed.data?.pages;
  const answer = inbox.data;
  // One array per set of pages or per answer, and the same array until they change: the motion
  // hold adopts what it is given by identity (`motion-hold.ts`), so a fresh `flatMap` on every
  // render would have it adopt - and so re-render - for ever. Each is undefined only while its
  // read has no answer: an answer that holds no thread gives an empty array.
  const paged = useMemo(() => pages?.flatMap((page) => meInboxThreads(page)), [pages]);
  const unpaged = useMemo(
    () => (answer === undefined ? undefined : meInboxThreads(answer)),
    [answer]
  );
  const read = show === "dismissed" ? paged : unpaged;
  // A refetch can land while a pointer travels between rows; adopt it once that pointer settles.
  const adopted = useMotionHold(read, listRef);
  const listed = asksOnly ? [] : (adopted ?? []);
  return {
    answered: asksOnly || adopted !== undefined,
    // The mark of the read these rows came from: the Dismissed view's is its first page's, the
    // one snapshot its cursor walks on from.
    asOf: show === "dismissed" ? pages?.[0]?.as_of : answer?.as_of,
    rows: listed.map((thread): InboxListRow => ({ kind: "thread", thread })),
    showMore:
      show === "dismissed" && dismissed.hasNextPage
        ? () => void dismissed.fetchNextPage()
        : undefined,
    showingMore: dismissed.isFetchingNextPage,
  };
}
