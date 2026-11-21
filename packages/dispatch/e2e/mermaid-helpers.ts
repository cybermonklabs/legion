// The init-script and worker helpers `e2e/mermaid.e2e.ts`'s rows share: the wrappers that record,
// hold, refuse or fail the page's `Worker`, the two scroll helpers (one turns the browser's scroll
// anchoring off, one reads the offset once it settles), and the route that makes the real worker
// chunk hold a diagram's source. And the drawn-diagram helpers that file and
// `e2e/mermaid.stub.e2e.ts` share: a flowchart body, the diagram a card draws, a comment thread
// opened in the Conversation, and the check that a card draws its diagram with the source closed.
import { expect, type Locator, type Page } from "@playwright/test";

// What this file's init scripts keep on the page's window.
declare global {
  interface Window {
    /** Makes the page's workers fail from now on, as `failMermaidWorkersOnCall` describes. */
    __failMermaidWorkers?: () => void;
    /** The `source` of each request posted to a worker. */
    __mermaidSent?: string[];
    /** How many workers the page constructed. */
    __mermaidWorkers?: number;
    /** Delivers what a held worker posted, and lets later posts through. */
    __releaseMermaid?: () => void;
  }
}

/** Wraps the page's `Worker` so the page counts each worker it constructs
 *  (`window.__mermaidWorkers`) and records the `source` of each request posted to one
 *  (`window.__mermaidSent`), passing everything through to the real worker. Mermaid's worker is
 *  the app's only `new Worker` (`features/refs/mermaid-diagrams.ts`). */
export async function recordMermaidWorkers(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__mermaidWorkers = 0;
    window.__mermaidSent = [];
    const RealWorker = window.Worker;
    window.Worker = class extends RealWorker {
      constructor(url: string | URL, options?: WorkerOptions) {
        super(url, options);
        window.__mermaidWorkers = (window.__mermaidWorkers ?? 0) + 1;
      }

      postMessage(message: unknown, options?: StructuredSerializeOptions | Transferable[]): void {
        if (typeof message === "object" && message !== null && "source" in message) {
          window.__mermaidSent?.push(String(message.source));
        }
        super.postMessage(message, options as StructuredSerializeOptions);
      }
    };
  });
}

/** Wraps the page's `Worker` so nothing its workers post reaches the page until
 *  `window.__releaseMermaid()` runs: each `message` event a listener added with
 *  `addEventListener("message", …)` (as `mermaid-diagrams.ts` listens) or `onmessage` would get
 *  is held, then delivered in arrival order on release, and later ones flow straight through.
 *  The worker's `ready` is held too, and the time limit starts only at `ready`, so a held diagram
 *  is never refused; it stays undrawn until the release. */
export async function holdMermaidWorkers(page: Page): Promise<void> {
  await page.addInitScript(() => {
    let released = false;
    const held: (() => void)[] = [];
    window.__releaseMermaid = () => {
      released = true;
      for (const deliver of held.splice(0)) deliver();
    };
    const RealWorker = window.Worker;
    const HeldWorker = class extends RealWorker {
      addEventListener(
        type: string,
        listener: EventListenerOrEventListenerObject,
        options?: boolean | AddEventListenerOptions
      ): void {
        if (type !== "message") {
          super.addEventListener(type, listener, options);
          return;
        }
        super.addEventListener(
          type,
          (event) => {
            const deliver = () => {
              if (typeof listener === "function") listener.call(this, event);
              else listener.handleEvent(event);
            };
            if (released) deliver();
            else held.push(deliver);
          },
          options
        );
      }
    };
    const handlers = new WeakMap<Worker, ((event: MessageEvent) => unknown) | null>();
    Object.defineProperty(HeldWorker.prototype, "onmessage", {
      configurable: true,
      get(this: Worker) {
        return handlers.get(this) ?? null;
      },
      set(this: Worker, handler: ((event: MessageEvent) => unknown) | null) {
        if (!handlers.has(this)) {
          this.addEventListener("message", (event) => handlers.get(this)?.call(this, event));
        }
        handlers.set(this, handler);
      },
    });
    window.Worker = HeldWorker;
  });
}

/** Turns the browser's scroll anchoring off on `page`, as in Safari before 27, which has none.
 *  Every engine Playwright runs anchors the scroll on the target when a diagram above it grows,
 *  so with anchoring on a row would pass without the page's own focus re-run; with it off, only
 *  that re-run brings the target back into view. */
export async function withoutScrollAnchoring(page: Page): Promise<void> {
  await page.addStyleTag({ content: "* { overflow-anchor: none !important; }" });
}

/** The document's scroll offset once it has stopped moving: unchanged over 10 frames in a row, so
 *  a smooth scroll a link or a wheel started has ended before the offset is read. */
export function settledScrollTop(page: Page): Promise<number> {
  return page.evaluate(
    () =>
      new Promise<number>((resolve) => {
        let last = -1;
        let still = 0;
        const sample = () => {
          const top = document.scrollingElement?.scrollTop ?? 0;
          still = top === last ? still + 1 : 0;
          last = top;
          if (still >= 10) resolve(top);
          else requestAnimationFrame(sample);
        };
        sample();
      })
  );
}

/** Makes the page's `new Worker(...)` throw, as a browser with no worker to give does. */
export async function refuseWorkers(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.Worker = class {
      constructor() {
        throw new Error("This test's page has no Worker");
      }
    } as unknown as typeof Worker;
  });
}

/** Wraps the page's `Worker` so its workers pass everything through until
 *  `window.__failMermaidWorkers()` runs, and from then on fail as a worker whose chunk can no
 *  longer load does, as across a redeploy: each worker's next `postMessage` posts nothing and
 *  instead fires `error` on that worker (on a later task, as the browser reports a load failure),
 *  and every `new Worker(...)` throws. */
export async function failMermaidWorkersOnCall(page: Page): Promise<void> {
  await page.addInitScript(() => {
    let failing = false;
    window.__failMermaidWorkers = () => {
      failing = true;
    };
    const RealWorker = window.Worker;
    window.Worker = class extends RealWorker {
      constructor(url: string | URL, options?: WorkerOptions) {
        if (failing) throw new Error("This test's worker chunk no longer loads");
        super(url, options);
      }

      postMessage(message: unknown, options?: StructuredSerializeOptions | Transferable[]): void {
        if (failing) {
          setTimeout(() => this.dispatchEvent(new Event("error")), 0);
          return;
        }
        super.postMessage(message, options as StructuredSerializeOptions);
      }
    };
  });
}

/** The built Mermaid render worker's chunk, as Vite emits it under `/assets`. */
const workerChunk = /\/assets\/mermaid-render\.worker-[^/]+\.js$/;

/** The same-origin path the held worker requests once it has taken a diagram's source. */
const workerHeldPath = "/e2e/mermaid-worker-held";

/** Prepended to the worker chunk, so it registers before the worker's own listener: it stops that
 *  listener, reports through `workerHeldPath` that it holds the source, and never returns. */
const holdingListener = [
  'self.addEventListener("message", async (event) => {',
  "  event.stopImmediatePropagation();",
  `  await fetch("${workerHeldPath}");`,
  "  for (;;) {}",
  "});",
].join("\n");

/** Serves the real worker chunk to `page` with `holdingListener` in front of the worker's own, so
 *  the worker takes a diagram's source and never answers, and the page's shipped time limit
 *  refuses it whatever the machine's speed. Returns how many times a worker reported that hold. */
export async function holdWorkerSource(page: Page): Promise<() => number> {
  let holds = 0;
  await page.route(workerChunk, async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, body: `${holdingListener}\n${await response.text()}` });
  });
  await page.route(
    (url) => url.pathname === workerHeldPath,
    async (route) => {
      holds += 1;
      await route.fulfill({ status: 204 });
    }
  );
  return () => holds;
}

/** Waits for a worker to report that it holds the source, then shows that five 100 ms timers on
 *  the page's thread each fire on time, give or take a scheduling hiccup, while the worker is
 *  busy: a page that drew on its own thread would never hand the source over.
 *
 *  The 1,000 ms bound is five times the largest sample read under load, and a page that draws the
 *  diagram on its own thread holds that thread for seconds, so the bound fails that page. The
 *  load: the row that calls it, "a diagram too large to draw in time shows the reason, and the
 *  page keeps responding" (`e2e/mermaid.e2e.ts`), ran 50 times with no retries at 4357585e of
 *  main, on two CPUs beside the full lane's shard that holds the row running with three workers.
 *  0 of the 50 failed, and the largest of the five samples over all 50 runs was 200 ms. */
export async function expectHeldAndResponsive(page: Page, holds: () => number): Promise<void> {
  await expect
    .poll(holds, { message: "the Mermaid worker took the diagram's source and holds it" })
    .toBeGreaterThanOrEqual(1);
  for (let sample = 0; sample < 5; sample += 1) {
    const elapsed = await page.evaluate(
      () =>
        new Promise<number>((resolve) => {
          const start = performance.now();
          setTimeout(() => resolve(performance.now() - start), 100);
        })
    );
    expect(elapsed).toBeLessThan(1_000);
  }
}

const diagramName = "Rendered Mermaid diagram";

/** A body of `lead` and then one flowchart from `from` to `to`, whose source's first line is
 *  `graph TD`. */
export function flowchart(lead: string, from: string, to: string): string {
  return `${lead}\n\n\`\`\`mermaid\ngraph TD\n  ${from} --> ${to}\n\`\`\`\n`;
}

/** The drawn diagram inside `card`. */
export function diagramIn(card: Locator): Locator {
  return card.getByRole("img", { exact: true, name: diagramName });
}

function conversationTurns(page: Page): Locator {
  return page.getByRole("list", { name: "Conversation turns" });
}

/** The comment thread `rootId` opened in the Conversation, where its bodies render in full
 *  rather than as the collapsed card's preview. The turn shows `lead`, the root comment's first
 *  text, before it is expanded, so the click lands on a turn that has rendered. */
export async function openCommentThread(
  page: Page,
  rootId: string,
  lead: string
): Promise<Locator> {
  const turn = conversationTurns(page).locator(`:scope > li[data-turn="comment:${rootId}"]`);
  await expect(turn).toContainText(lead);
  const expand = turn.getByRole("button", { exact: true, name: "Expand thread" });
  const opened = turn.getByRole("button", { exact: true, name: "Collapse thread" });
  // Retried for the reason the reader's click on `Mermaid source` is below: the page moves
  // while the turn's diagrams draw, so a click can land on whatever slid under the pointer and
  // the thread never opens. Measured here on firefox, where the expand click was lost and the
  // row waited out its budget for a `Collapse thread` that never appeared. The expand button is
  // gone once the thread is open, so the retry clicks only while it is still there.
  await expect(async () => {
    if (await expand.isVisible()) await expand.click();
    await expect(opened).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 5_000 });
  return turn;
}

/** Both halves of the control: `card` shows the drawn diagram holding `labels`, and its source
 *  (whose first line is `graph TD`) stays out of sight until `Mermaid source` is opened. */
export async function expectDrawnDiagram(card: Locator, labels: readonly string[]): Promise<void> {
  const diagram = diagramIn(card);
  await expect(diagram).toBeVisible();
  for (const label of labels) {
    await expect(diagram).toContainText(label);
  }
  const source = card.getByText("graph TD");
  // Mounted, closed inside the disclosure: toBeHidden alone also passes on no match at all.
  await expect(source).toHaveCount(1);
  await expect(source).toBeHidden();
  const disclosure = card.getByText("Mermaid source", { exact: true });
  // The reader's click is retried until the disclosure opens, because a click can be lost. An
  // instrumented run of these rows caught it: a document-level `click` listener recorded
  // `click ARTICLE connected=true`, so the click landed on the `<article>` behind the summary -
  // the page moved between Playwright's actionability check on the summary and the click's own
  // dispatch - and no `toggle` fired and no figure was redrawn. Nothing had opened the
  // disclosure, so the row waited out its whole expect timeout; it failed that way twice in 100
  // repeats on firefox in a two-worker run of this pod. A click already taken leaves the source
  // visible, so the retry clicks only while it is still hidden rather than closing what the
  // first click opened.
  //
  // A retried click is no proof of the product's own rule, that a source the reader opened is
  // drawn open again after a repaint: this helper only gets the reader's click to land. That
  // rule is proven by the row `a mermaid source a reader opened is open again when the comment
  // is edited` and by the module's own test file,
  // `web/src/features/refs/mermaid-diagrams.test.tsx`.
  await expect(async () => {
    if (await source.isHidden()) await disclosure.click();
    await expect(source).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 5_000 });
}
