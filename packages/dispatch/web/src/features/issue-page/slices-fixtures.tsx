import { spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render } from "@testing-library/react";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter } from "react-router-dom";

import { MarginProvider } from "../margin/margin-context";
import { ReferenceNamesProvider, usePublishReferenceNames } from "../refs/reference-names";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { KEY, node, READER } from "./decisions-filter/decisions-fixtures";
import { FocusedNodeProvider } from "./focused-node";
import { IssueTree } from "./IssueTree";
import { IDENTITY_HEIGHT_PROPERTY } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import { OutlineRequestProvider, useAskOutline, useOutlineRequest } from "./outline-request";
import { publishedTree } from "./projection-fixture";

/**
 * What the cases of the rows drawn in slices share (`drawn-in-slices.test.tsx`,
 * `first-drawing-scroll-slices.test.tsx` and each view's `.slices.test.tsx`): a page laid out by
 * hand, since happy-dom lays nothing out and every box it answers is empty, so no row would ever
 * fill a screen and every row would be drawn at once; the browser's `scheduler.postTask` by hand,
 * so a case runs each slice when it chooses and inside `act`; and a clock by hand, which a row's
 * drawing moves on, so a slice's length is read as the hook reads it.
 */

/** The height of the window the cases lay the page out in, in CSS pixels. */
export const SCREEN_PX = 600;

/** The height every row is laid out at unless a case says otherwise. */
export const ROW_PX = 100;

/** The height of the band that stays at the top of the window in the cases that set one: the
 *  page's header, as `identity-block.ts` publishes it. */
export const BAND_PX = 60;

/** The layout of `layoutByHand`: what the window has scrolled, and each row's top in it. */
export interface LayoutByHand {
  /** Every scroll asked for since the layout was made, in px down the page each. */
  readonly scrolls: number[];
  /** How far the window is scrolled down the document. */
  readonly scrolled: () => number;
  /** Scrolls the window to `y` px down the document, as the reader's own scroll does. */
  readonly scrollTo: (y: number) => void;
  /** The top of `element` in the window, as the browser would give it. */
  readonly topOf: (element: Element | null | undefined) => number | undefined;
  readonly restore: () => void;
}

/** How `layoutByHand` lays the page out. */
export interface LayoutOptions {
  readonly band?: boolean;
  readonly height?: (element: Element) => number;
  readonly rows?: string;
  readonly top?: number;
}

/**
 * Lays out by hand every element `rows` matches, one under another in the document's order, each
 * as tall as `height` says, the first `top` px down the document; every other element is given
 * the empty box happy-dom gives it. The window is `SCREEN_PX` tall and scrolls only by
 * `scrollBy`, `scrollTo` and `scrollIntoView` (the last two move nothing), each of which is
 * counted. `band` publishes the header's band as the page does, so a row brought on screen is
 * brought under it. `restore` puts every stand-in back.
 */
export function layoutByHand({
  band = false,
  height = () => ROW_PX,
  rows = "[data-node-row]",
  top = 0,
}: LayoutOptions = {}): LayoutByHand {
  let scrolled = 0;
  const scrolls: number[] = [];
  const descriptors = {
    innerHeight: Object.getOwnPropertyDescriptor(window, "innerHeight"),
    scrollY: Object.getOwnPropertyDescriptor(window, "scrollY"),
  };
  Object.defineProperty(window, "innerHeight", { configurable: true, get: () => SCREEN_PX });
  Object.defineProperty(window, "scrollY", { configurable: true, get: () => scrolled });
  if (band) document.documentElement.style.setProperty(IDENTITY_HEIGHT_PROPERTY, `${BAND_PX}px`);
  const documentTop = (element: Element): number | undefined => {
    let y = top;
    for (const one of document.querySelectorAll(rows)) {
      if (one === element) return y;
      y += height(one);
    }
    return undefined;
  };
  const spies = [
    spyOn(window, "scrollBy").mockImplementation(((
      options?: ScrollToOptions | number,
      y?: number
    ) => {
      const by = typeof options === "number" ? (y ?? 0) : (options?.top ?? 0);
      scrolled += by;
      scrolls.push(by);
    }) as typeof window.scrollBy),
    spyOn(window, "scrollTo").mockImplementation(() => {
      scrolls.push(0);
    }),
    spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {
      scrolls.push(0);
    }),
    spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement
    ) {
      const y = this.matches(rows) ? documentTop(this) : undefined;
      return y === undefined ? new DOMRect() : new DOMRect(0, y - scrolled, 800, height(this));
    }),
  ];
  return {
    restore: () => {
      for (const spy of spies) spy.mockRestore();
      for (const [name, descriptor] of Object.entries(descriptors)) {
        if (descriptor !== undefined) Object.defineProperty(window, name, descriptor);
      }
      document.documentElement.style.removeProperty(IDENTITY_HEIGHT_PROPERTY);
    },
    scrollTo: (y: number) => {
      scrolled = y;
    },
    scrolled: () => scrolled,
    scrolls,
    topOf: (element) => (element == null ? undefined : element.getBoundingClientRect().top),
  };
}

/** One task the page asked `scheduler.postTask` for. */
interface AskedTask {
  readonly callback: () => unknown;
  readonly signal: AbortSignal | undefined;
}

/** The tasks `tasksByHand` holds. */
export interface TasksByHand {
  /** How many tasks are held and not aborted. */
  readonly pending: () => number;
  readonly restore: () => void;
  /** Runs the first task still held, and answers whether there was one. */
  readonly runNext: () => Promise<boolean>;
  /** Runs every task held, and each one those ask for, until none is held. */
  readonly runAll: () => Promise<void>;
}

/**
 * The browser's `scheduler.postTask`, by hand: every task asked for is held until the case runs
 * it, inside `act`, and a task whose signal was aborted first is never run, as the browser drops
 * it. `restore` takes the scheduler away again.
 */
export function tasksByHand(): TasksByHand {
  const queue: AskedTask[] = [];
  const had = Object.getOwnPropertyDescriptor(globalThis, "scheduler");
  Object.defineProperty(globalThis, "scheduler", {
    configurable: true,
    value: {
      postTask: (callback: () => unknown, options?: { signal?: AbortSignal }) => {
        const answered = Promise.withResolvers<unknown>();
        queue.push({
          callback: () => answered.resolve(callback()),
          signal: options?.signal,
        });
        options?.signal?.addEventListener("abort", () => answered.reject(options.signal?.reason));
        return answered.promise;
      },
    },
  });
  const runNext = async (): Promise<boolean> => {
    let task = queue.shift();
    while (task !== undefined && task.signal?.aborted === true) task = queue.shift();
    if (task === undefined) return false;
    const run = task.callback;
    await act(async () => {
      run();
    });
    return true;
  };
  return {
    pending: () => queue.filter((task) => task.signal?.aborted !== true).length,
    restore: () => {
      if (had === undefined) Reflect.deleteProperty(globalThis, "scheduler");
      else Object.defineProperty(globalThis, "scheduler", had);
    },
    runAll: async () => {
      // Every slice asks for the next at once, so the queue empties only once every row is drawn.
      for (let ran = 0; ran < 1_000 && (await runNext()); ran += 1) {
        // Each pass ran one slice.
      }
    },
    runNext,
  };
}

/** The clock `clockByHand` stands in with. */
export interface ClockByHand {
  readonly advance: (ms: number) => void;
  readonly now: () => number;
  readonly restore: () => void;
}

/** A clock by hand in place of `performance.now()`, which only `advance` moves: a row's drawing
 *  moves it by what that row costs, so a slice reads the time its rows took. */
export function clockByHand(): ClockByHand {
  let now = 0;
  const spy = spyOn(performance, "now").mockImplementation(() => now);
  return {
    advance: (ms: number) => {
      now += ms;
    },
    now: () => now,
    restore: () => spy.mockRestore(),
  };
}

/** The issue `KEY` holding `children`, each a comment, by its id, with words of its own. */
export function commentsUnder(
  ids: readonly string[],
  replies: Readonly<Record<string, number>> = {}
): TreeNode {
  return node({
    children: ids.map((id) =>
      node({
        children: Array.from({ length: replies[id] ?? 0 }, (_unused, at) =>
          node({
            id: `${id}-r${String(at).padStart(3, "0")}`,
            text: `A reply ${at}.`,
            type: "comment",
          })
        ),
        id,
        text: `The comment ${id}.`,
        type: "comment",
      })
    ),
    id: KEY,
    type: "issue",
  });
}

/** `count` ids, `prefix` and a number of two digits each, from `c00`: one comment each. */
export function idsOf(count: number, prefix = "c"): string[] {
  return Array.from({ length: count }, (_unused, at) => `${prefix}${String(at).padStart(2, "0")}`);
}

/** The node ids of the outline's rows in `container`, in the order they are drawn. */
export function outlineRows(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>('[role="tree"] [data-node-row]')].map(
    (row) => row.dataset.nodeId ?? ""
  );
}

/** The outline's row of node `id` in `container`, where it is drawn. */
export function outlineRow(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(id)}"]`);
}

/** What the outline of these cases is drawn with: the chain down to the node an address names and
 *  the following it came with, whether the reads that place the rows and those the rows come from
 *  have answered, and whether the outline is drawn with no filter on. */
export interface OutlineDrawn {
  readonly addressKey?: string;
  readonly placesSettled?: boolean;
  readonly readsSettled?: boolean;
  readonly reveal?: readonly TreeNode[];
  readonly root: TreeNode;
  readonly rootRow?: boolean;
}

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
export function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** The outline as `slots/body.tsx` draws it, from the last ask of the Contents list
 *  (`OutlineRequestProvider`), with what the page publishes for its rows' chips published to the
 *  names store and handed to it; `held.ask` is the Contents list's ask. */
function OutlineAt({
  drawn,
  held,
}: {
  drawn: OutlineDrawn;
  held: { ask?: (id: string | undefined, focusBranch: boolean) => void };
}): ReactNode {
  held.ask = useAskOutline();
  const request = useOutlineRequest();
  const published = useMemo(() => publishedTree(drawn.root), [drawn.root]);
  usePublishReferenceNames(published);
  return (
    <IssueTree
      {...(drawn.addressKey === undefined ? {} : { addressKey: drawn.addressKey })}
      artifacts={new Map()}
      context={new Set()}
      focusReveal
      holdsEscape={false}
      isClosed={false}
      onEditing={() => {}}
      placesSettled={drawn.placesSettled ?? true}
      published={published}
      readsSettled={drawn.readsSettled ?? true}
      {...(request === undefined ? {} : { request })}
      reveal={drawn.reveal}
      root={drawn.root}
      rootRow={drawn.rootRow ?? true}
    />
  );
}

/** The outline drawn over `first`, under the page's keymap, the Contents list's asks and the
 *  names store, with the reader signed in: `ask` asks the Contents list for node `id`, and
 *  `redraw` draws the outline again with what changed, as a read answering or a link followed
 *  does. */
export function drawOutline(first: OutlineDrawn): {
  readonly ask: (id: string) => Promise<void>;
  readonly redraw: (next: Partial<OutlineDrawn>) => void;
  readonly view: RenderResult;
} {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: READER });
  const held: { ask?: (id: string | undefined, focusBranch: boolean) => void } = {};
  let drawn = first;
  const draw = () => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <FocusedNodeProvider>
              <OutlineRequestProvider>
                <IssueScope />
                <OutlineAt drawn={drawn} held={held} />
              </OutlineRequestProvider>
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  // Under the names store, as the shell draws every page (`app.tsx`).
  const view = render(draw(), { wrapper: ReferenceNamesProvider });
  return {
    ask: (id: string) => act(async () => held.ask?.(id, false)),
    redraw: (next) => {
      drawn = { ...drawn, ...next };
      view.rerender(draw());
    },
    view,
  };
}
