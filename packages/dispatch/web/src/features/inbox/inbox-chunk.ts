/**
 * The Inbox's own chunk: the list, its cards, its bar and the peek drawer, in one module both
 * hosts of `InboxHost.tsx` import dynamically, so the two of them share one download instead of
 * each starting its own.
 *
 * Why the Inbox is not in the entry any more. The entry is what every page of the dashboard
 * parses before it draws anything, and no cache spares a new browser context that parse, so each
 * of the browser suite's rows pays it as well as each person opening a page. Of the 1,028,690
 * bytes the entry held at this branch's head (`bun run build:web`, 2026-11-12), the Inbox's own
 * graph was the largest part no page but the Inbox itself draws: the card list, the ask cards,
 * the thread bodies and the Markdown surfaces under them. A reader on an issue, a board or a
 * document parsed all of it to be shown none of it.
 *
 * What a reader loses by its being late, and when it is fetched. On `/` and `/credentials/*` the
 * list is the page, so the download starts with the navigation rather than after it
 * (`app.tsx`, `preloadRouteChunk`) and the landing read runs beside it: the reads are the reads
 * of the head before this split, and the frames before the chunk lands show the page's heading
 * over the list's skeleton. Anywhere else the chunk is fetched the moment the reader opens the
 * peek drawer, and nothing on the page waits for it until then.
 */
export { Inbox } from "./Inbox";
export { InboxDrawer } from "./InboxDrawer";
