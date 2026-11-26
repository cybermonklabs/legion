import { expect, type Page, type Request } from "@playwright/test";

/** What one page sends: its GETs under `/api/` since the last `reset`, every read of one issue's
 *  detail since the page opened, and whether any request is still in flight. The event feed rides
 *  the page's live socket (`/ws/live`). Playwright can report the socket's upgrade as a request
 *  that stays in flight for as long as the socket is open, so a request whose path starts with
 *  `/ws/` is left out of the requests in flight (`isLiveSocket`), and no count here holds it. */
export interface Traffic {
  /** Path and query of each GET under `/api/` since the last `reset`. */
  readonly gets: string[];
  /** How many times `GET /api/v1/issues/<key>` (the full issue, not its summary) was sent. */
  issueReads(key: string): number;
  /** Whether no request has been in flight for `ms`. */
  quietFor(ms: number): boolean;
  reset(): void;
}

/** Whether a request is the page's live socket: its path starts with `/ws/`. The socket stays open
 *  for the page's whole life and is no read of the page, so no watcher of a page's requests holds
 *  it in flight. In a CI trace the upgrade of a load's `GET /ws/live` (status 101) was reported as
 *  a request that stayed open 29,998.5 ms, though every read had answered within 0.5 s. */
export function isLiveSocket(url: URL): boolean {
  return url.pathname.startsWith("/ws/");
}

/** Whether `request` loads a new document into the page's main frame. Such a load ends every
 *  request of the document it replaces, and Playwright reports no end for some of them: three
 *  reads Settings still had open stayed in flight after a load of an issue, so a watch started on
 *  Settings never went quiet. So each watcher drops what is in flight when such a load starts. */
function loadsDocument(page: Page, request: Request): boolean {
  return request.isNavigationRequest() && request.frame() === page.mainFrame();
}

export function watchTraffic(page: Page): Traffic {
  const gets: string[] = [];
  const issueReads = new Map<string, number>();
  const inFlight = new Set<Request>();
  let lastSettledAt = Date.now();
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (loadsDocument(page, request)) inFlight.clear();
    if (!isLiveSocket(url)) inFlight.add(request);
    if (request.method() !== "GET" || !url.pathname.startsWith("/api/")) return;
    gets.push(`${url.pathname}${url.search}`);
    const issue = /^\/api\/v1\/issues\/([^/]+)$/.exec(url.pathname)?.[1];
    if (issue !== undefined) {
      const key = decodeURIComponent(issue);
      issueReads.set(key, (issueReads.get(key) ?? 0) + 1);
    }
  });
  const settle = (request: Request) => {
    if (inFlight.delete(request)) lastSettledAt = Date.now();
  };
  page.on("requestfinished", settle);
  page.on("requestfailed", settle);
  return {
    gets,
    issueReads: (key) => issueReads.get(key) ?? 0,
    quietFor: (ms) => inFlight.size === 0 && Date.now() - lastSettledAt >= ms,
    reset: () => {
      gets.length = 0;
    },
  };
}

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
  /** Whether nothing has been in flight for `ms` - every request the page makes but the live
   *  socket's upgrade (`isLiveSocket`), not only the ones counted here. This is how the recorded
   *  baseline measures one load
   *  ("between navigation and network idle
   *  (nothing in flight for 1500 ms)"), and why a caller polls it rather than Playwright's own
   *  `networkidle`, which calls a page idle after 500 ms and so stops counting before a read
   *  that waits on another read's answer has been sent. */
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
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (loadsDocument(page, request)) inFlight.clear();
    if (!isLiveSocket(url)) inFlight.add(request);
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
  page.on("requestfailed", settle);
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
