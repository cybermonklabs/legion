// The steps a page takes on its way to an open editor, which the rows of
// `large-documents-long-build.e2e.ts` wait on rather than on the clock: the page says each step
// through a binding as it takes it (`watchThePageMove`), and a wait goes on for as long as the
// page keeps saying them (`whileThePageMoves`). The document socket's three steps are said by
// `large-documents-page.ts`'s wrap of the page's WebSocket (`watchTheDocument`), the one wrap of
// it, which reaches this recorder through the window hook it declares (`__thePageMoved`), as
// `large-documents-build-probe.ts` reaches that wrap through its own (`__documentSocketMade`).
// The two selectors the recorder finds the page's draw and the editor's root by are
// `large-documents-page.ts`'s, which reads them too.
//
// The other files of this family, and what each holds, are listed once, at the head of
// `large-documents-rows.ts`.
import type { Page } from "@playwright/test";

import { documentTextSelector, editorRootSelector } from "./large-documents-page";

/** One step the page said it took, as this side heard it: `Date.now()` here, and what it was. */
interface HeardStep {
  readonly at: number;
  readonly step: string;
}

/** What this side has heard of a page opening its document and building its editor: every step
 *  it said, oldest first, and how many top-level blocks the editor held after its last batch. */
export interface ThePageMoving {
  readonly steps: HeardStep[];
  blocks: number;
}

/**
 * Has the page say each step it takes on its way to an open editor, and answers what it says.
 *
 * The steps are what a document's opening is made of, each said in the task it happened in: a
 * file of the page's own arriving - a script, a stylesheet, an image - and the document's text
 * read answering, as the browser files each `resource` entry when its response ends; anything
 * going into the page's own draw of the document, its blocks and the diagrams drawn in them;
 * the document's socket made, opened, and the first frame the server sends on it; the editor's
 * root constructed; and each batch of the editor's build, with the count it leaves. A batch
 * puts the next run of the document's top-level blocks into the editor in a task of its own
 * (`@legion/proof-editor`'s `progressive-build.ts`), so the editor's element gains children
 * once per batch and in no other way while it builds; a mutation that leaves the count where it
 * was - the adoption's redraw of the same blocks - is no batch. A read the page makes for
 * anything else - its panels, a count that refreshes - is no step, and nor is a later frame on
 * the socket, so a page whose document has stopped opening falls quiet however busy the rest of
 * it is. Each step is said through a binding, which goes out of the page in the task that says
 * it rather than waiting for the page to answer a question: from a build's first batch to its
 * last the page answers nothing the run asks it (`Reached`, in `large-documents-page.ts`, says
 * why the page announces).
 *
 * The socket's three steps are said by `watchTheDocument`'s wrap of the page's WebSocket, the
 * one wrap `large-documents-page.ts` puts there, so a row calls this beside `watchTheDocument`.
 * Both go before the navigation, and after `documentTransport` where the row holds the socket:
 * from that call Playwright's own WebSocket stands where the page's stood (`routeWebSocket`), and
 * a wrap put in front of the page's before it hears none of the socket's steps.
 */
export async function watchThePageMove(page: Page): Promise<ThePageMoving> {
  const moving: ThePageMoving = { blocks: 0, steps: [] };
  await page.exposeFunction("__thePageMoved", (step: string, blocks: number) => {
    moving.steps.push({ at: Date.now(), step });
    if (step === "batch") {
      moving.blocks = blocks;
    }
  });
  await page.addInitScript(
    ({ drawn, root }: { drawn: string; root: string }) => {
      const say = (step: string, blocks = 0): void => {
        window.__thePageMoved?.(step, blocks);
      };
      const theTextRead = /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/u;
      const reads = ["beacon", "fetch", "xmlhttprequest"];
      new PerformanceObserver((entries) => {
        for (const entry of entries.getEntries() as PerformanceResourceTiming[]) {
          const path = new URL(entry.name).pathname;
          if (!reads.includes(entry.initiatorType)) {
            say(`${entry.initiatorType} ${path.slice(path.lastIndexOf("/") + 1)}`);
          } else if (theTextRead.test(path)) {
            say("text read");
          }
        }
      }).observe({ buffered: true, type: "resource" });
      let built = 0;
      let rooted = false;
      new MutationObserver((records) => {
        let drew = false;
        for (const { addedNodes, target } of records) {
          if (addedNodes.length === 0 || !(target instanceof HTMLElement)) {
            continue;
          }
          if (target.classList.contains("ProseMirror") && target.closest(root) !== null) {
            if (target.childElementCount !== built) {
              built = target.childElementCount;
              say("batch", built);
            }
          } else if (target.closest(drawn) !== null) {
            drew = true;
          } else if (
            !rooted &&
            [...addedNodes].some(
              (node) =>
                node instanceof HTMLElement &&
                (node.matches(root) || node.querySelector(root) !== null)
            )
          ) {
            rooted = true;
            say("editor root");
          }
        }
        if (drew) {
          say("draw");
        }
      }).observe(document, { childList: true, subtree: true });
    },
    { drawn: documentTextSelector, root: editorRootSelector }
  );
  return moving;
}

/**
 * Waits for `reaching` for as long as the page keeps taking steps `counts` accepts, and answers
 * what it reached, with the longest this side went without such a step and the step that gap
 * came after.
 *
 * The bound is on the page's progress, not on the clock alone: the row fails by the stall's own
 * name once `stalledAfterMs` has passed since the last such step - or since the wait began,
 * before any. `reaching` carries the most the row's leg leaves the wait however steadily the
 * page moves, with its own message for running out, which this adds the page's last step to.
 * `givingUp` is the row's own reason to stop waiting, which ends the wait with whatever it
 * rejects with, as it is.
 */
export async function whileThePageMoves<T>(
  reaching: Promise<T>,
  moving: ThePageMoving,
  counts: (step: string) => boolean,
  { stalledAfterMs, stopped, what }: { stalledAfterMs: number; stopped: string; what: string },
  givingUp?: Promise<never>
): Promise<{ longestQuiet: string; reached: T }> {
  const began = Date.now();
  const heard = (): HeardStep[] =>
    moving.steps.filter(({ at, step }) => at >= began && counts(step));
  const theLast = (): string => {
    const last = heard().at(-1);
    return last === undefined
      ? `no ${what} since the wait began`
      : `the last ${last.step}, ${Date.now() - last.at} ms before`;
  };
  const stalled = Promise.withResolvers<never>();
  const look = (): void => {
    const quietMs = Date.now() - (heard().at(-1)?.at ?? began);
    if (quietMs < stalledAfterMs) {
      timer = setTimeout(look, stalledAfterMs - quietMs);
      return;
    }
    stalled.reject(
      new Error(
        `${stopped}: no ${what} for ${quietMs} ms (the page is stalled at ${stalledAfterMs} ` +
          `ms), ${Date.now() - began} ms into the wait, ${theLast()}, with ${moving.blocks} ` +
          "top-level blocks in the editor"
      )
    );
  };
  let timer = setTimeout(look, stalledAfterMs);
  try {
    const reached = await Promise.race([
      reaching.catch((error: unknown) => {
        throw new Error(
          `${error instanceof Error ? error.message : String(error)}, while the page was still ` +
            `moving: ${theLast()}, with ${moving.blocks} top-level blocks in the editor`
        );
      }),
      stalled.promise,
      ...(givingUp === undefined ? [] : [givingUp]),
    ]);
    const steps = [{ at: began, step: "the wait's start" }, ...heard()];
    const gaps = steps.map((step, index) => ({
      after: step.step,
      ms: (steps[index + 1]?.at ?? Date.now()) - step.at,
    }));
    const longest = gaps.reduce((most, gap) => (gap.ms > most.ms ? gap : most));
    return { longestQuiet: `${longest.ms} ms after ${longest.after}`, reached };
  } finally {
    clearTimeout(timer);
  }
}
