import { expect, type Page, type Request } from "@playwright/test";

/** One request under `/api/`, as `watchApiRequests` counts it. */
export interface ApiRequest {
  readonly method: string;
  /** Path and query, as sent. */
  readonly path: string;
  /** The path with its ids replaced, which is what `byRoute` groups by. */
  readonly route: string;
}

/** How many requests one route took, for the per-route breakdown. */
export interface RouteCount {
  readonly count: number;
  readonly route: string;
}

/** Every request the page sends under `/api/`, whatever its method: the total and the count per
 *  route, which is what a request budget is read against. A caller measures one load by calling
 *  `reset()` at the navigation and reading the counts once the page is quiet (`quietFor`). */
export interface ApiRequests {
  /** The count per route, most sent first and then by route, so two runs print one order. */
  byRoute(): readonly RouteCount[];
  /** Whether nothing at all has been in flight for `ms` - every request the page makes, not
   *  only the ones counted here. This is how the recorded baseline measures one load
   *  ("between navigation and network idle
   *  (nothing in flight for 1500 ms)"), and why a caller polls it rather than Playwright's own
   *  `networkidle`, which calls a page idle after 500 ms and so stops counting before a read
   *  that waits on another read's answer has been sent. The event feed rides the page's live
   *  socket (`/ws/live`), which Playwright reports as a websocket and never as a request, so
   *  it holds nothing in flight. */
  quietFor(ms: number): boolean;
  /** Every request since the last `reset`, in the order the page sent them. */
  readonly requests: readonly ApiRequest[];
  reset(): void;
  /** How many requests under `/api/` were sent since the last `reset`. */
  total(): number;
}

/**
 * The route one `/api/` path belongs to: the path with every id replaced by `:id`, and the query
 * dropped, so `GET /api/v1/issues/LAY-3/references?to=…` is the one route
 * `/api/v1/issues/:id/references` whatever its query. A segment is read as an id by its shape,
 * because the path alone says nothing about which collection addresses by id: a route's own words
 * are lowercase letters and hyphens (`references`, `architecture-sources`), the API version is
 * `v<digits>`, and anything else - an issue key (`LAY-3`), a numeric id, a UUID, a session id -
 * is an id, as is a hex token of seven characters or more, which is a commit (no route word is
 * hex throughout). A name made of lowercase words - a document slug, a repository owner - is
 * therefore its own route rather than an id, which is why this is read as a grouping and never as
 * a route table.
 */
export function apiRoute(pathname: string): string {
  return pathname
    .split("/")
    .map((segment) =>
      segment === "" ||
      /^v\d+$/.test(segment) ||
      (/^[a-z][a-z-]*$/.test(segment) && !/^[0-9a-f]{7,}$/.test(segment))
        ? segment
        : ":id"
    )
    .join("/");
}

export function watchApiRequests(page: Page): ApiRequests {
  const requests: ApiRequest[] = [];
  const inFlight = new Set<Request>();
  let lastSettledAt = Date.now();
  // The request of a new document the main frame is loading, until that document commits.
  let loading: Request | undefined;
  page.on("request", (request) => {
    inFlight.add(request);
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) loading = request;
    const url = new URL(request.url());
    if (!url.pathname.startsWith("/api/")) return;
    requests.push({
      method: request.method(),
      path: `${url.pathname}${url.search}`,
      route: apiRoute(url.pathname),
    });
  });
  const settle = (request: Request) => {
    if (inFlight.delete(request)) lastSettledAt = Date.now();
  };
  page.on("requestfinished", settle);
  // A document that fails to load never takes the page's place, so it settles nothing else.
  page.on("requestfailed", (request) => {
    if (request === loading) loading = undefined;
    settle(request);
  });
  // A new document takes the page's place: a read the document it replaced still had in flight
  // can no longer answer the page, and the browser may report neither its end nor its failure,
  // so it is settled here, as Playwright's own idle wait drops it at a new document (a move to
  // a fragment of the same document loads none, and drops nothing). Kept, it would hold the
  // page from ever going quiet again.
  page.on("framenavigated", (frame) => {
    if (frame !== page.mainFrame() || loading === undefined) return;
    const kept = loading;
    loading = undefined;
    for (const request of inFlight) if (request !== kept) settle(request);
  });
  return {
    byRoute: () => {
      const counts = new Map<string, number>();
      for (const request of requests) {
        counts.set(request.route, (counts.get(request.route) ?? 0) + 1);
      }
      return [...counts]
        .map(([route, count]) => ({ count, route }))
        .sort((a, b) => b.count - a.count || a.route.localeCompare(b.route));
    },
    quietFor: (ms) => inFlight.size === 0 && Date.now() - lastSettledAt >= ms,
    requests,
    reset: () => {
      requests.length = 0;
    },
    total: () => requests.length,
  };
}

/** How long nothing may be in flight before a load is counted finished. It is the recorded
 *  baseline's own window ("between
 *  navigation and network idle (nothing in flight for 1500 ms)"). */
const LOAD_QUIET_MS = 1_500;

/** Fails unless the page has gone quiet, which is the moment a load's cost is read at. Measured
 *  on this pod, the same unchanged page counted 39, 35 and 38 requests when the count was taken
 *  at Playwright's `networkidle` instead: that window closes after 500 ms of quiet, which is
 *  less than a read waiting on another read's answer takes under load, so what it yields is
 *  whatever had arrived by then rather than what the load costs. */
export async function expectLoadSettled(traffic: ApiRequests): Promise<void> {
  await expect
    .poll(() => traffic.quietFor(LOAD_QUIET_MS), {
      message: "the page never went quiet, so its first load has no settled cost",
      timeout: 30_000,
    })
    .toBe(true);
}
