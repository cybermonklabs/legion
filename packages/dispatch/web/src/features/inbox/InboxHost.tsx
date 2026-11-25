import { type ReactNode, type RefObject, Suspense, useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

import { landingReadOf } from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { afterNextPaint } from "../shell/after-next-paint";
import { askedSurface } from "../shell/asked-surface";
import { chunk } from "../shell/chunk-component";
import { lazyChunk, prefetchChunk } from "../shell/chunk-loads";
import type { InboxProps } from "./Inbox";
import type * as InboxChunk from "./inbox-chunk";

/**
 * Where the shell places the Inbox, and where the Inbox's code is fetched: with the navigation
 * at the addresses whose page is the Inbox, and at the reader's own press of the peek
 * everywhere else, so no page that merely offers the peek waits for it or fetches it before its
 * first paint.
 *
 * The shell mounted the Inbox itself until dispatch://INFRA-1109: the page at `/` rendered it,
 * and the peek drawer rendered it over every other page, so the list, the ask cards, the thread
 * bodies and the Markdown surfaces under them were all in the entry chunk. That chunk is what
 * every page parses before it draws anything, and a browser context with a cold cache has none
 * of it, so an issue, a board or a document paid for the Inbox to be shown none of it. It was
 * the largest such part of the entry; what the entry cost then and costs now is measured in one
 * place, `MAX_ENTRY_BYTES` in `web/src/asset-sizes-in-build.ts`.
 *
 * Both hosts below load the one module (`inbox-chunk.ts`), so the page and the drawer share one
 * download: a reader who peeks at the Inbox from an issue and then opens `/` fetches nothing the
 * second time.
 */
// Through `lazyChunk`, like every chunk a `Suspense` waits on: a reader who lands on the Inbox's
// own page is in front of a page that cannot draw until this arrives, so a download that fails
// must spend one of the tab's recovery reloads rather than be read as a prefetch's
// (`features/shell/DeploymentResilience.tsx`).
const InboxList = lazyChunk(() => loadInboxChunk().then((module) => ({ default: module.Inbox })));

// The peek opens on a key over a page whose own shortcuts take every key typed until it mounts,
// so it is drawn through `askedSurface` (`features/shell/asked-surface.tsx`) and not behind
// `lazy`, whose first render waits at least 300 ms also with the chunk in; `InboxDrawerHost`
// says what the reader is shown until then.
const inboxPeekChunk = chunk(() => loadInboxChunk().then((module) => module.InboxDrawer));

const InboxPeek = askedSurface(inboxPeekChunk, "Inbox");

/** The one import of the Inbox's chunk: the hosts below render it, and the entry starts it at
 *  the addresses whose page is the Inbox (`app.tsx`, `preloadRouteChunk`, which asks
 *  `routeIsInbox`). The download then overlaps the landing read's round trip, so the Inbox's own
 *  reads still go out in the wave that follows the landing's answer and the list draws as soon
 *  as both are in. Named here rather than in the shell, so the chunk the entry starts cannot be
 *  another than the one these hosts render. */
export function loadInboxChunk(): Promise<typeof InboxChunk> {
  return import("./inbox-chunk");
}

/**
 * Starts the peek's chunk one frame after the page has painted, on the addresses the landing
 * read takes for the Inbox and on no others.
 *
 * What the press costs where nothing starts it. The peek is a drawer over the page, so no part
 * of the page waits for this chunk, and the press paid for the whole download: 222,154 bytes
 * over 18 chunks on the agents page, 223,252 over 16 on a project page, and 91,312 over 5 on an
 * issue page, whose own chunks hold the ask card and the surfaces under it already (`bun run
 * build:web`, 2026-11-12). Started here, the download is over before the press and the drawer
 * opens on code the page holds.
 *
 * Why after the paint. The shell renders under the landing read's own answer, so this download
 * cannot delay that read, and one frame more keeps it off the first paint as well - the frame
 * the reader is waiting for. That is what the entry's own start of this chunk could not do: it
 * runs before React renders anything, so at those addresses it put 136,988 bytes over 11
 * requests in front of the landing (measured on a cold `/agents`, dispatch://INFRA-1109).
 *
 * Why these addresses and no others. Their own page chunk is small - the agents page is 31,723
 * bytes and the settings page 30,708 - so the peek is the largest thing a reader there can still
 * ask for. An issue, a board and a document carry page chunks of 99,209 to 244,489 bytes with
 * their own reads behind them, and that is what their reader is waiting for. Measured on 107
 * rows of this suite that use the Inbox, the peek and the ask threads, the two arms built and
 * run turn and turn about: starting the chunk on every page costs a row 121 ms, and starting it
 * on these addresses alone saves a row 125 ms.
 */
function usePeekChunkAfterFirstPaint(asked: RefObject<boolean>): void {
  // One parse of the address decides this, the landing read's own (`api/queries.ts`), so the
  // page this chunk is started on cannot be another than the page that reads the Inbox.
  const peekIsAPressAway = landingReadOf(useLocation().pathname).page === "inbox";
  useEffect(() => {
    // A chunk that is in has nothing left to fetch. Each return to one of these addresses runs
    // this effect again, and without this test each return asked for the chunk again. The test
    // is the peek's own chunk and not the Inbox's module: a module the Inbox's page brought in
    // costs this start no request, and it is this start that tells the peek its chunk is in, so
    // that the press draws the drawer in its own commit rather than the box that waits for it.
    if (!peekIsAPressAway || inboxPeekChunk.loaded() !== undefined) {
      return;
    }
    return afterNextPaint(() => {
      // A reader who opened the peek inside that first frame is waiting on this chunk under
      // `renderChunk` already, and this start would add nothing to the download they wait for.
      if (!asked.current) prefetchChunk(inboxPeekChunk.load);
    });
  }, [asked, peekIsAPressAway]);
}

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
 * The Inbox as a peek over whatever page is open. No part of the page waits for the peek's chunk,
 * which is what makes this a boundary rather than a rename: the drawer is mounted on every page
 * of the shell and closed on almost all of them (`InboxDrawer.tsx` draws nothing while closed,
 * and this host renders no chunk while closed either). On the addresses the landing read takes
 * for the Inbox the chunk is fetched all the same, once the page has painted, so that the
 * reader's press opens the drawer rather than starting its download
 * (`usePeekChunkAfterFirstPaint`); on an issue, a board or a document the press is what fetches
 * it. The Inbox's own page offers no peek at all - neither the rail's button nor its shortcut is
 * there (`routeIsInbox`) - and the entry has started the same chunk at that address anyway.
 *
 * While the press waits for that download, a small box stands in the drawer's place and takes
 * every key typed (`askedSurface`): the page under it would otherwise take them, and on an issue
 * page a digit writes the issue's priority. The drawer drops them as it lands rather than taking
 * them as `typed`: its focus lands on its Close, and its letters and digits are shortcuts that
 * walk and act on its rows (`Inbox.tsx`), not words for its search box, so a key replayed then
 * would act on a row the reader has not seen. Escape, a press that takes the focus out of the
 * box, or a move to another address calls the peek off, and a chunk that lands after that opens
 * nothing; Tab and Shift+Tab stay in the box.
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
  usePeekChunkAfterFirstPaint(asked);
  if (!asked.current) {
    return null;
  }
  return <InboxPeek onClose={onClose} open={open} />;
}
