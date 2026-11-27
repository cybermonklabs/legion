/** What the shell's chunk-recovery rows need of the page: the assets the document names, a
 *  deployment that never built one of them, the tab's record of the reloads it has spent, and
 *  what each load of the tab did with a chunk failure.
 *
 *  The two shell spec files share this: `shell.stub.e2e.ts` holds the rows that need nothing but
 *  the page and its own routes; `shell.e2e.ts` holds the rows whose failed asset must be the
 *  server's own 404, which the stubbed layer's file server does not give (it answers an unknown
 *  path with `index.html`, as the SPA fallback). Rows of other files take from it too, to hold,
 *  fail or wait for one of the page's chunks, or to answer the page's landing without a part.
 */
import { expect, type Page, type Request } from "@playwright/test";

/** The path of the asset the page at `origin` names in `pattern`'s first group. */
export async function pageAssetPath(
  pattern: RegExp,
  what: string,
  origin: string
): Promise<string> {
  const html = await (await fetch(new URL("/", origin))).text();
  const asset = pattern.exec(html)?.[1];
  if (asset === undefined) {
    throw new Error(`the page names no ${what}: ${html}`);
  }
  return new URL(asset, origin).pathname;
}

/** The path of the page's entry chunk: the module script Vite writes into index.html. */
export function entryScriptPath(origin: string): Promise<string> {
  return pageAssetPath(
    /<script\b[^>]*\btype="module"[^>]*\bsrc="([^"]+)"/u,
    "module script",
    origin
  );
}

/**
 * Answers the page's entry chunk 404, as a server whose deployment never built it does, whenever
 * `missing` says so for that request (numbered from 1; it may hold the request until it answers),
 * and counts the page's document loads and entry requests.
 */
export async function missEntryChunk(
  page: Page,
  missing: (request: number) => boolean | Promise<boolean>,
  origin: string
) {
  const entry = await entryScriptPath(origin);
  const counts = { documents: 0, entryRequests: 0 };
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      counts.documents += 1;
    }
  });
  await page.route(
    (url) => url.pathname === entry,
    async (route) => {
      counts.entryRequests += 1;
      if (!(await missing(counts.entryRequests))) {
        await route.continue();
        return;
      }
      await route.fulfill({
        body: JSON.stringify({ error: "not found" }),
        contentType: "application/json",
        status: 404,
      });
    }
  );
  return counts;
}

/** Records reloads `minutesAgo` by the page's clock as the tab's recovery reloads, and returns
 *  their times. */
export function recordTabReloads(page: Page, minutesAgo: number[]): Promise<number[]> {
  return page.evaluate((ages) => {
    const now = Date.now();
    const times = ages.map((age) => now - age * 60_000);
    window.sessionStorage.setItem("dispatch.chunk-reloads", JSON.stringify(times));
    return times;
  }, minutesAgo);
}

/** The tab's recorded recovery reloads. */
export function tabReloads(page: Page): Promise<number[] | null> {
  return page.evaluate(() =>
    JSON.parse(window.sessionStorage.getItem("dispatch.chunk-reloads") ?? "null")
  );
}

/** What one document load of the tab did with the chunk failures reported to it. */
interface FailureHandling {
  /** How many of this load's `vite:preloadError` dispatches have run to their end: the bundle's
   *  recovery has judged each of them. */
  preloadErrors: number;
  /** How many `error` events of this load's scripts and links every listener of the window has
   *  seen: `web/index.html`'s inline script has judged each of them. */
  assetErrors: number;
  /** Whether this load began a reload. */
  reloaded: boolean;
}

/** Where the page keeps that record: the tab's session storage, which outlives the reload a
 *  failure may begin, so the load that began it can still be read once the next one is up. */
const FAILURE_HANDLING_KEY = "e2e.failure-handling";

/** How often the waits below look again. Each waits on a fact that turns true within a frame or a
 *  round trip of the page. Playwright's default intervals are `[100, 250, 500, 1000]`
 *  (playwright/lib/matchers/expect.js:13404), so the default looks again at 100 ms, then at
 *  350 ms and 850 ms: a fact that turns true just after the look at 100 ms is read 250 ms past
 *  it, and one just after the look at 350 ms half a second past it. The suite's short ladder, as
 *  in `block-references.e2e.ts`, looks again at 50, 100, 200 and 400 ms. */
const PROBE = { intervals: [50, 50, 100, 200, 500] };

/**
 * Records what each document load of the tab does with a chunk failure, from before any script
 * of the page runs, and returns a wait for the `load`th load (from 1) to have judged one - both
 * judges have, or one of them began a reload - which answers whether each load up to then began
 * a reload.
 *
 * A chunk the bundle imports fails twice. Vite asks for it through a
 * `<link rel="modulepreload">` beside the import, so its download fails as that link's `error`
 * event, which the inline script judges, and as the import's `vite:preloadError`, which the
 * bundle's recovery judges (`reloadForChunkFailure`,
 * `web/src/features/shell/DeploymentResilience.tsx`). Each judge decides inside the event it is
 * given: it returns, or it calls `window.location.reload()` there. The two events come in either
 * order - on the stubbed layer the link's came first for some loads and second for others - so
 * the wait is for both: once both have passed their judges, a reload either judge was going to
 * begin has begun, and a row about a failure that must reload nothing reads that at that moment
 * rather than after a margin of the wall clock.
 *
 * The wait ends at a reload too. A load whose first judge began one can lose the other event
 * with its document, and so never count both; a row whose failure must reload nothing would
 * then, the moment the failure did reload, wait out the poll's timeout and name the judges. Ended
 * at the reload, the wait answers `true` for that load at once.
 */
export async function recordFailureHandling(
  page: Page
): Promise<(load: number) => Promise<boolean[]>> {
  await page.addInitScript((key) => {
    const navigation = window.navigation;
    // A frame inside the page shares the tab's session storage and is no load of the tab. An
    // engine without the Navigation API keeps no record at all, so the wait below fails rather
    // than reading a reload it could not see as none.
    if (window !== window.top || navigation === undefined) return;
    const loads: FailureHandling[] = JSON.parse(window.sessionStorage.getItem(key) ?? "[]");
    const load: FailureHandling = { assetErrors: 0, preloadErrors: 0, reloaded: false };
    loads.push(load);
    const keep = () => window.sessionStorage.setItem(key, JSON.stringify(loads));
    keep();
    // `location.reload()` fires `navigate` before it returns, so a reload either judge begins is
    // on the record inside the event that began it. The app declares only the part of the event
    // it reads (`web/src/features/shell/DeploymentResilience.tsx`), so the type is read here.
    navigation.addEventListener("navigate", (event) => {
      if (!("navigationType" in event) || event.navigationType !== "reload") return;
      load.reloaded = true;
      keep();
    });
    // Vite dispatches this event from script, so a microtask queued here runs once every listener
    // of that dispatch has returned, the bundle's included.
    window.addEventListener("vite:preloadError", () => {
      queueMicrotask(() => {
        load.preloadErrors += 1;
        keep();
      });
    });
    // An element's `error` event reaches the document's capturing listeners after every
    // capturing listener of the window, which is where the inline script listens.
    document.addEventListener(
      "error",
      (event) => {
        if (event.target instanceof HTMLLinkElement || event.target instanceof HTMLScriptElement) {
          load.assetErrors += 1;
          keep();
        }
      },
      true
    );
  }, FAILURE_HANDLING_KEY);
  return async (load) => {
    let loads: FailureHandling[] = [];
    await expect
      .poll(
        async () => {
          // A read made while a reload takes the document away has no page to run in; the last
          // record read stands, and the next poll reads the new one.
          loads = await page
            .evaluate(
              (key): FailureHandling[] => JSON.parse(window.sessionStorage.getItem(key) ?? "[]"),
              FAILURE_HANDLING_KEY
            )
            .catch(() => loads);
          const record = loads[load - 1];
          return (
            record !== undefined &&
            (record.reloaded || (record.assetErrors > 0 && record.preloadErrors > 0))
          );
        },
        {
          ...PROBE,
          message: `in load ${load} of the tab, both judges judged a chunk failure or one reloaded`,
        }
      )
      .toBe(true);
    return loads.map((one) => one.reloaded);
  };
}

/** Adds a script of this build that the server never had, as a replaced deployment's would fail. */
export async function failBuildScript(page: Page): Promise<void> {
  await page.evaluate(() => {
    const script = document.createElement("script");
    script.src = "/assets/index-missing.js";
    document.head.append(script);
  });
}

/** The pattern of the chunk the build names `<name>-<hash>.js`. */
export function chunkFile(name: string): RegExp {
  return new RegExp(`/assets/${name}-[\\w-]{8}\\.js$`, "u");
}

/** The path of the margin's own chunk (`web/src/features/margin/MarginColumn.tsx`), which the
 *  shell asks for once its hold on a landing has ended (`web/src/features/shell/
 *  content-drawn.ts`). */
export const MARGIN_CHUNK = /\/assets\/Margin-[^/]+\.js$/u;

/** Holds the margin's chunk on the wire until the row calls the release this answers: the moment
 *  between the page's first draw and the margin, which a row measures the page in. */
export async function holdMarginChunk(page: Page): Promise<() => void> {
  return (await holdAssetUntilReleased(page, MARGIN_CHUNK)).release;
}

/** The path of the reference-preview layer's chunk (`web/src/features/refs/RefPreviewLayer.tsx`),
 *  which the shell asks for at the same moment. */
export const REF_PREVIEW_CHUNK = /\/assets\/RefPreview-[^/]+\.js$/u;

/** The path of the pending-writes indicator's chunk, which the writes host fetches once the page
 *  has drawn and draws only while it holds a write (`web/src/features/writes/write-hosts.tsx`).
 *  The sign-in page's list of kept writes has no chunk: it is in the entry. */
export const PENDING_WRITES_CHUNK = /\/assets\/PendingWritesIndicator-[\w-]{8}\.js$/u;

/** The path of every kind of write, which the store's take-up of what a reload kept waits for and
 *  nothing draws from (`web/src/features/writes/kinds.ts`). */
export const WRITE_KINDS_CHUNK = /\/assets\/kinds-[\w-]{8}\.js$/u;

/** Every request `page` makes for the kinds of write from here on, and each of them that has
 *  ended, answered or failed. */
export function kindsRequests(page: Page): { asked: Set<Request>; ended: Set<Request> } {
  const asked = new Set<Request>();
  const ended = new Set<Request>();
  page.on("request", (request) => {
    if (WRITE_KINDS_CHUNK.test(new URL(request.url()).pathname)) asked.add(request);
  });
  const end = (request: Request) => {
    if (asked.has(request)) ended.add(request);
  };
  page.on("requestfinished", end);
  page.on("requestfailed", end);
  return { asked, ended };
}

/** The two chunks of writes every signed-in page fetches, neither of which a page without a write
 *  draws from, so no landmark shows they came down (`fetchedChunk`). */
export const writesChunks = [PENDING_WRITES_CHUNK, WRITE_KINDS_CHUNK] as const;

/** The new issue page's frame shell (`web/src/features/issue-page/frame-shell.tsx`, loaded
 *  through `frame-chunk.ts`): the route chunk of both landings of a reader who turned that page
 *  on (`e2e/landing-reads.e2e.ts`), and the chunk Settings starts as it draws the switch that
 *  turns it on (`e2e/issue-page-switch.e2e.ts`). */
export const FRAME_SHELL_CHUNK = /\/assets\/frame-shell-[\w-]{8}\.js$/u;

/** The path of the editor library's own chunk (`@legion/proof-editor`, which
 *  `web/src/features/doc/document-chunks.ts`'s `loadEditorLibrary` imports), and not
 *  `lib-headless`, whose name begins the same way. */
export const EDITOR_LIBRARY_CHUNK = /\/assets\/lib-[\w-]{8}\.js$/u;

/** Waits until the page has fetched a script whose path `chunk` matches: for a chunk the page
 *  starts of its own accord and draws nothing from until a reader needs it, so that no landmark
 *  can show that it came down. It reads the page's own resource timing, which keeps the
 *  document's fetches, so a fetch that ended before the wait began is found as well as one still
 *  to come. That buffer holds 250 entries unless the page sets another size, which this one does
 *  not, and records no fetch past them, so an absent entry can also mean a full buffer; every
 *  caller reads it soon after a load or reload of its own, early in that document. How that fetch
 *  ended is the caller's to hold it to. */
export async function fetchedChunk(page: Page, chunk: RegExp): Promise<void> {
  await expect
    .poll(
      () =>
        page.evaluate(
          ({ flags, source }) => {
            const pattern = new RegExp(source, flags);
            return performance
              .getEntriesByType("resource")
              .some((entry) => pattern.test(new URL(entry.name).pathname));
          },
          { flags: chunk.flags, source: chunk.source }
        ),
      `the page never fetched ${chunk}`
    )
    .toBe(true);
}

/** Waits until the page itself has fetched the chunk at `pattern` and run it: its resource timing
 *  names the fetch (`fetchedChunk`), and an import of that same address settles once the module
 *  has run - with no request of its own, since the page holds the module - by which time the
 *  page's own import of it, out since before that fetch, has settled too. Not for a page whose
 *  clock the row installed: Playwright's clock replaces `performance`, whose resource entries
 *  then read empty, so such a row imports the address its hold saw (`HeldAsset.url`), as
 *  `issue.e2e.ts`'s held tab does. */
export async function ranChunk(page: Page, pattern: RegExp): Promise<void> {
  await fetchedChunk(page, pattern);
  await page.evaluate(
    async ({ flags, source }) => {
      const chunk = new RegExp(source, flags);
      const fetched = performance
        .getEntriesByType("resource")
        .find((entry) => chunk.test(new URL(entry.name).pathname));
      if (fetched === undefined) throw new Error(`the page fetched no ${source}`);
      await import(fetched.name);
    },
    { flags: pattern.flags, source: pattern.source }
  );
}

/** One node of a tree React commits, as far as `chunkComponentsRendered` reads it. */
interface Fiber {
  readonly child: Fiber | null;
  readonly sibling: Fiber | null;
  /** The component React rendered: for one behind `lazy`, the component its chunk resolved to,
   *  and for one drawn through `chunkComponent` (`web/src/features/shell/chunk-component.tsx`),
   *  the component its chunk carries, which that component draws once the chunk is in. */
  readonly type: unknown;
}

declare global {
  interface Window {
    /** Every root React has committed, kept by the DevTools hook `shellLoads` installs. */
    __shellReactRoots?: Set<{ readonly current: Fiber }>;
  }
}

/** Whether, for each chunk at `urls`, a component it carries is in a tree React has committed -
 *  the margin's behind `lazy`, the reference-preview layer's through `chunkComponent` - so the
 *  import that loaded it settled. */
function chunkComponentsRendered(page: Page, urls: readonly string[]): Promise<boolean> {
  return page.evaluate(async (chunks) => {
    const roots = window.__shellReactRoots;
    // Two things leave no root here: a hook that no longer works, and a poll whose evaluate lands
    // in a document a navigation has just made, which reads the empty set the init script below
    // made there before React commits. No row polls across a navigation today, for two reasons.
    // `failAssetWhen` aborts only once `ready()` has resolved, so no load it fails reloads while
    // its own poll runs; and the two rows that call `settled()` outside it await that poll before
    // they fail a download: `keyed-writes.e2e.ts`'s write pressed while the kinds of write fail to
    // download, before each download it aborts, the second of which reloads the page, and
    // `keyboard-palette.e2e.ts`'s bar whose chunk failed offline, whose failure reloads nothing.
    // A poll begins only once the reference-preview chunk has come down, which the render an
    // effect of a committed tree begins asks for (`web/src/features/refs/RefPreviewLayer.tsx`,
    // `setWanted`), so React has committed a root in that document by then. Read as "not rendered
    // yet", a hook React never found would hold the poll to its timeout; a throw ends it at once,
    // since `expect.poll` does not catch an error of the function it polls
    // (playwright/lib/matchers/expect.js:13397).
    if (roots === undefined || roots.size === 0) {
      throw new Error("React committed no root to the DevTools hook `shellLoads` installs");
    }
    const rendered = new Set<unknown>();
    for (const root of roots) {
      const stack = [root.current];
      for (let fiber = stack.pop(); fiber !== undefined; fiber = stack.pop()) {
        rendered.add(fiber.type);
        if (fiber.child !== null) stack.push(fiber.child);
        if (fiber.sibling !== null) stack.push(fiber.sibling);
      }
    }
    for (const chunk of chunks) {
      // The page's own module of that chunk, whose exports are the components it carries: an
      // import of a URL the page has already loaded is answered from its module map, with no
      // request.
      const module: Record<string, unknown> = await import(chunk);
      if (!Object.values(module).some((value) => rendered.has(value))) return false;
    }
    return true;
  }, urls);
}

/** One document load of a page, as the rows about a prefetched chunk read it. */
export interface ShellLoad {
  /** Settles once the two components the shell renders on every page from a chunk of their own -
   *  the margin and the reference-preview layer - have rendered. A chunk failure while either
   *  chunk is out is a failure a reader is waiting behind, whatever chunk it is
   *  (`web/src/features/shell/DeploymentResilience.tsx`), so a row about a chunk nobody waits on
   *  fails its chunk only after this. */
  readonly settled: () => Promise<void>;
  /** Settles once the Inbox peek's prefetch has gone out (`web/src/features/inbox/InboxHost.tsx`,
   *  `usePeekChunkAfterFirstPaint`), which a page the landing reads as the Inbox starts after its
   *  first paint. */
  readonly peekAsked: Promise<void>;
}

function shellLoad(page: Page) {
  const margin = Promise.withResolvers<string>();
  const preview = Promise.withResolvers<string>();
  const peek = Promise.withResolvers<void>();
  const settled = async () => {
    const chunks = await Promise.all([margin.promise, preview.promise]);
    // A chunk's response is in before the import it answers has settled - the margin's chunk
    // imports chunks of its own - and before `renderChunk`'s count of it has dropped. React
    // renders the margin, behind `lazy`, only once its import has settled, and the preview
    // layer, drawn through `chunkComponent`, only once that component's own `then` on the import
    // has set its state. The count drops in a `.finally` chained after a `.catch` of that import
    // (`renderChunk` in `web/src/features/shell/chunk-loads.ts`), which runs after React's own
    // `then` on it, or `chunkComponent`'s; but neither `then` renders anything. A settled import
    // wakes its boundary through `pingSuspendedRoot` or `retryTimedOutBoundary`, a state set
    // outside React schedules its update, and each ends in `ensureRootIsScheduled`, which
    // schedules the render - as a task of React's own scheduler, or at the earliest in a microtask
    // queued there, behind the `.finally` the `.catch` queued before it
    // (`react-dom-client.production.js`, `ensureRootIsScheduled` and
    // `scheduleImmediateRootScheduleTask`). So by the time either
    // component is in a committed tree the count has dropped. Neither draws anything on most of
    // the pages these rows open - the margin is null on a route that has none (the issue page
    // `keyed-writes.e2e.ts` opens has one), and the layer is null until a card opens - so the
    // tree React committed is where they are read.
    await expect
      .poll(() => chunkComponentsRendered(page, chunks), {
        ...PROBE,
        message: "the margin and the reference-preview layer rendered",
      })
      .toBe(true);
  };
  return { load: { peekAsked: peek.promise, settled }, margin, peek, preview };
}

/** The current document load of `page`, a new one from each navigation of its main frame. */
export async function shellLoads(page: Page): Promise<() => ShellLoad> {
  // React looks for its DevTools hook once, as its bundle runs, so this one is in the page before
  // any of the page's scripts. It keeps each root React commits, which is what DevTools reads a
  // tree from, and nothing else.
  await page.addInitScript(() => {
    const roots = new Set<{ readonly current: Fiber }>();
    window.__shellReactRoots = roots;
    Object.assign(window, {
      __REACT_DEVTOOLS_GLOBAL_HOOK__: {
        inject: () => 1,
        onCommitFiberRoot: (_renderer: number, root: { readonly current: Fiber }) => {
          roots.add(root);
        },
        supportsFiber: true,
      },
    });
  });
  let current = shellLoad(page);
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      current = shellLoad(page);
    } else if (/\/assets\/inbox-chunk-[^/]+\.js$/u.test(new URL(request.url()).pathname)) {
      current.peek.resolve();
    }
  });
  page.on("requestfinished", (request) => {
    const url = request.url();
    const path = new URL(url).pathname;
    if (MARGIN_CHUNK.test(path)) current.margin.resolve(url);
    if (REF_PREVIEW_CHUNK.test(path)) current.preview.resolve(url);
  });
  return () => current.load;
}

/** Holds every request for the asset at `pattern` on the wire for the rest of the row: a
 *  download that is out and never answers. */
export async function holdAsset(page: Page, pattern: RegExp): Promise<void> {
  await page.route(pattern, () => undefined);
}

/** A download a row holds on the wire until it lets it land (`holdAssetUntilReleased`). */
export interface HeldAsset {
  /** Lets every request held so far, and every later one, through to the server. */
  readonly release: () => void;
  /** The address of the first request held, once the page has asked for the asset. */
  readonly url: Promise<string>;
}

/** Holds every request for the asset at `pattern` on the wire until the row calls `release`: a
 *  download that is still out when the reader acts, which the row then lets land. */
export async function holdAssetUntilReleased(page: Page, pattern: RegExp): Promise<HeldAsset> {
  const held = Promise.withResolvers<void>();
  const url = Promise.withResolvers<string>();
  await page.route(pattern, async (route) => {
    url.resolve(route.request().url());
    await held.promise;
    await route.continue();
  });
  return { release: () => held.resolve(), url: url.promise };
}

/** Fails every request for the asset at `pattern`, each once `ready` has settled for the load it
 *  belongs to, and returns how many it failed. */
export async function failAssetWhen(
  page: Page,
  pattern: RegExp,
  ready: () => Promise<unknown>
): Promise<() => number> {
  let failed = 0;
  await page.route(pattern, async (route) => {
    await ready();
    await route.abort();
    failed += 1;
  });
  return () => failed;
}

/**
 * Answers this page's landing with its shell's `part` null - the agent list or the Inbox list - so
 * the page reads that list on its own route. A landing carries each list and seeds it under the
 * key the page reads it by (`web/src/api/queries.ts`, `seedShell`), and a page given it asks that
 * route not at all on its first load. Null is what a Dispatch that could not read the part
 * answers, as one with no Envoy listener answers for the agents, and the seed leaves a null part
 * for its own route on purpose, so this is the page's own fallback and no behaviour invented here.
 */
export async function landingWithout(page: Page, part: "agents" | "inbox"): Promise<void> {
  await page.route("**/api/v1/landing*", async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as { shell: Record<string, unknown> };
    await route.fulfill({ json: { ...body, shell: { ...body.shell, [part]: null } }, response });
  });
}
