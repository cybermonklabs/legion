import { lazy, type ReactNode, Suspense, useRef } from "react";

import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { renderChunk } from "../shell/DeploymentResilience";
import type { InboxProps } from "./Inbox";

/**
 * Where the shell places the Inbox, and where the Inbox's code is fetched: after the first paint
 * of whatever page the reader landed on, never before it.
 *
 * The shell mounted the Inbox itself until dispatch://INFRA-1109: the page at `/` rendered it,
 * and the peek drawer rendered it over every other page, so the list, the ask cards, the thread
 * bodies and the Markdown surfaces under them were all in the entry chunk. That chunk is what
 * every page parses before it draws anything, and a browser context with a cold cache has none
 * of it, so an issue, a board or a document paid for the Inbox to be shown none of it. It was
 * the largest such part of the entry: of the 1,028,690 bytes the entry held at this branch's
 * head (`bun run build:web`, 2026-11-12) the Inbox's graph was about a third.
 *
 * Both hosts below load the one module (`inbox-chunk.ts`), so the page and the drawer share one
 * download: a reader who peeks at the Inbox from an issue and then opens `/` fetches nothing the
 * second time.
 */
// Under `renderChunk`, like every chunk a render waits on: a reader who lands on `/` or opens
// the peek is in front of a page that cannot draw until this arrives, so a download that fails
// must spend one of the tab's recovery reloads rather than be read as the entry's prefetch
// (`features/shell/DeploymentResilience.tsx`).
const InboxList = lazy(() =>
  renderChunk(() => import("./inbox-chunk").then((module) => ({ default: module.Inbox })))
);

const InboxPeek = lazy(() =>
  renderChunk(() => import("./inbox-chunk").then((module) => ({ default: module.InboxDrawer })))
);

/** The same import the hosts below render, for the entry to start on a landing at `/`
 *  (`app.tsx`, `preloadRouteChunk`): the download then overlaps the landing read's round trip,
 *  so the Inbox's own reads still go out in the wave that follows the landing's answer and the
 *  list draws as soon as both are in. Named here rather than in the shell, so the chunk the
 *  entry starts cannot be another than the one these hosts render. */
export const loadInboxChunk = (): Promise<unknown> => import("./inbox-chunk");

/**
 * The Inbox as the page at `/` draws it. What a reader loses while the chunk is on its way is
 * the list itself: they are shown the page's heading over the skeleton the Inbox draws for its
 * own unanswered read, which is the frame they were shown anyway until the Inbox's rows arrived.
 */
export function InboxHost(props: InboxProps): ReactNode {
  return (
    <Suspense fallback={<LoadingSkeleton label="Loading your inbox" />}>
      <InboxList {...props} />
    </Suspense>
  );
}

/**
 * The Inbox as a peek over whatever page is open. Nothing is fetched until the reader opens it,
 * which is what makes this a boundary rather than a rename: the drawer is mounted on every page
 * of the shell and closed on almost all of them (`InboxDrawer.tsx` draws nothing while closed,
 * and this host asks for no chunk while closed either).
 *
 * The host keeps the drawer mounted once it has been opened, as the shell always did: the drawer
 * holds the filters, the sort and the search text of this peek, and a reader who closes it and
 * opens it again is given the peek they left rather than a new one.
 */
export function InboxDrawerHost({
  onClose,
  open,
}: {
  onClose: () => void;
  open: boolean;
}): ReactNode {
  // A ref, not state: it only ever goes from false to true, and the render that sets it is the
  // render the open prop already brought. Once set it stays set, so the drawer below is mounted
  // from the first open until the page is left - the shell mounted it from the first paint
  // before this split, and a drawer unmounted on close would hand the reader a new peek, with
  // its filters, its sort and its search box empty, each time they opened it again.
  const asked = useRef(false);
  if (open) {
    asked.current = true;
  }
  if (!asked.current) {
    return null;
  }
  // `fallback={null}` is the drawer's own closed frame: the page under it is untouched until the
  // chunk lands, which is what a reader opening the peek sees for those frames.
  return (
    <Suspense fallback={null}>
      <InboxPeek onClose={onClose} open={open} />
    </Suspense>
  );
}
