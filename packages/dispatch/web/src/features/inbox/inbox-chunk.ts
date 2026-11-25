/**
 * The Inbox's own chunk: the list, its cards, its bar and the peek drawer, in one module both
 * hosts of `InboxHost.tsx` import dynamically, so the two of them share one download instead of
 * each starting its own.
 *
 * Why the Inbox stays out of the entry. The entry is what every page of the dashboard parses
 * before it draws anything, and no cache spares a new browser context that parse, so each of the
 * browser suite's rows pays it as well as each person opening a page. The Inbox's graph holds the
 * card list, the ask cards, the thread bodies and the Markdown surfaces under them, none of which
 * an issue, a board or a document draws. Keeping it in its own chunk spares every other route from
 * parsing it. The entry's measured sizes live in one place, `MAX_ENTRY_BYTES` in
 * `web/src/asset-sizes-in-build.ts`.
 *
 * What a reader loses by its being late, and when it is fetched. Where the Inbox is the page,
 * the list is what the reader came for, so the download starts with the navigation rather than
 * after it (`app.tsx`, `preloadRouteChunk`) and the landing read runs beside it, and the frames
 * before the chunk lands show the page's heading over the list's skeleton. Where the Inbox is
 * the peek drawer instead, no landing asks for this chunk and nothing on the page waits for it;
 * the shell starts it once that page has painted, so the drawer opens on code the page holds
 * rather than on a download the reader's press begins (`InboxHost.tsx`,
 * `usePeekChunkAfterFirstPaint`).
 */
export { Inbox } from "./Inbox";
export { InboxDrawer } from "./InboxDrawer";
