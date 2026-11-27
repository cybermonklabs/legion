// What the host of an editor built in batches has of the build beside the blocks it places
// (`src/progressive-build.ts`): its own node views, which every batch builds with; the thread,
// which each batch hands back to its page; and the hold it may put between two batches in that
// hand-back's place. The rows about the blocks themselves are `progressive-build.test.ts`'s.
import { describe, expect, test } from "bun:test";
import type { Window as HappyWindow } from "happy-dom";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import {
  type CreateProofEditorOptions,
  createProofEditor,
  type ProofEditorHandle,
} from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { endOfTask } from "../src/progressive-build";
import { withDomWindow } from "./dom-window";
import { type Batch, BLOCKS, blocksBuilt, paragraphs } from "./progressive-build-fixture";

// A host's node views have to be on the view before the build, not after it: `view.setProps`
// with a new `nodeViews` map re-runs ProseMirror's view update over the whole document, which
// destroys every node view the batches made and creates the host's in its place. For Dispatch's
// typed blocks over the 406,319-character document that was one task of 1,218 ms on a phone at
// 4x CPU throttling, longer than any batch of the build it followed (dispatch://INFRA-341, R11).
describe("the host's own node views", () => {
  test("are installed before the first batch, and built a batch at a time with it", async () => {
    const { parseMarkdown } = await createHeadlessProof({});
    const ydoc = new Y.Doc();
    prosemirrorToYXmlFragment(
      parseMarkdown(paragraphs(BLOCKS)),
      ydoc.getXmlFragment("prosemirror")
    );
    await withDomWindow(async (window) => {
      const root = window.document.body.appendChild(
        window.document.createElement("div")
      ) as unknown as HTMLElement;
      /** How many blocks the fragment had put in the editor when the host was called. */
      let whenCalled = -1;
      let created = 0;
      // How many of the host's node views existed at each look, one look per task, which is
      // what says they were created across the batches rather than in one task after them. A
      // real `setTimeout(0)` chain, as `watchTheBuild` in `progressive-build.test.ts` is and for
      // the same reason: what it watches for is the build's own yield between batches, which a
      // fake clock would drive rather than observe.
      const seen: number[] = [];
      let watching = true;
      const look = () => {
        if (!watching) return;
        seen.push(created);
        setTimeout(look, 0);
      };
      setTimeout(look, 0);
      const handle = await createProofEditor(root, {
        beforeBuild: (view) => {
          whenCalled = blocksBuilt(root);
          view.setProps({
            nodeViews: {
              ...view.props.nodeViews,
              paragraph: () => {
                created += 1;
                const dom = window.document.createElement("p") as unknown as HTMLElement;
                return { contentDOM: dom, dom };
              },
            },
          });
        },
        heatMapMode: "hidden",
        onMarkClick: () => {},
        onMarkHover: () => {},
        user: { color: "#0369a1", name: "alice" },
        ydoc,
      });
      watching = false;
      const { view } = handle;
      try {
        // Called over the empty document Milkdown created, before a block of the fragment was
        // in it: a node view installed here is the one each batch builds with.
        expect(whenCalled).toBe(0);
        expect(view.state.doc.childCount).toBe(BLOCKS);
        expect(created).toBeGreaterThanOrEqual(BLOCKS);
        expect(
          seen.filter((count) => count > 0 && count < BLOCKS).length,
          "every one of the host's node views was created in one task: a host that installs " +
            "them after the build pays for the whole document a second time"
        ).toBeGreaterThan(0);
      } finally {
        void handle.destroy();
        while (!view.isDestroyed) await Promise.resolve();
      }
    });
  });
});

/** Runs `act` with `value` standing in for `target`'s own `key`, and puts back whatever was
 *  there. `undefined` runs it with no `key` on `target` at all. */
async function withStandIn<T>(
  target: object,
  key: string,
  value: unknown,
  act: () => Promise<T>
): Promise<T> {
  const had = Object.getOwnPropertyDescriptor(target, key);
  if (value === undefined) {
    Reflect.deleteProperty(target, key);
  } else {
    Object.defineProperty(target, key, { configurable: true, value, writable: true });
  }
  try {
    return await act();
  } finally {
    Reflect.deleteProperty(target, key);
    if (had !== undefined) Object.defineProperty(target, key, had);
  }
}

/** A real 0 ms timer, not a fake clock: what these rows read is the order the browsing
 *  context's own queue serves two tasks in, which a fake clock would drive rather than
 *  observe. Neither waits for anything. */
function thePageSetsATimer(order: string[]): void {
  setTimeout(() => order.push("the page's own timer"), 0);
}

// How the thread goes back between batches is the whole of whether anything else on the page
// runs while a build lasts. `scheduler.yield()` stood here, and the browser serves its
// continuation ahead of every posted task: for as long as a build lasted - about 35 s over one
// screenful of the 406,319-character document and minutes over a whole drawn one - no socket
// message, no fetch callback and no timer ran. `scheduler.postTask()` at its default priority
// left the socket message served in 7 ms and a timer waiting 19,945 ms, because Chromium runs
// a web-scheduling task of that priority ahead of a timer (dispatch://INFRA-341, R11).
describe("the thread a batch hands back", () => {
  test("is handed back behind a task the page is already waiting on", async () => {
    const order: string[] = [];

    await withStandIn(globalThis, "scheduler", undefined, async () => {
      thePageSetsATimer(order);
      await endOfTask();
      order.push("the build's next batch");
    });

    expect(order).toEqual(["the page's own timer", "the build's next batch"]);
  });

  test("is a timer even where the browsing context has a scheduler of its own", async () => {
    const order: string[] = [];
    const taken: string[] = [];
    const scheduler = {
      postTask: (work: () => void) => {
        taken.push("postTask");
        work();
        return Promise.resolve();
      },
      yield: () => {
        taken.push("yield");
        return Promise.resolve();
      },
    };

    await withStandIn(globalThis, "scheduler", scheduler, async () => {
      thePageSetsATimer(order);
      await endOfTask();
      order.push("the build's next batch");
    });

    expect(taken).toEqual([]);
    expect(order).toEqual(["the page's own timer", "the build's next batch"]);
  });
});

/** `paragraphs(BLOCKS)` as one Yjs update, so that two builds can each take a Y.Doc of their
 *  own holding the very same blocks: a parse gives every block an id of its own at random. */
async function theDocumentAsAnUpdate(): Promise<Uint8Array> {
  const { parseMarkdown } = await createHeadlessProof({});
  const ydoc = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(paragraphs(BLOCKS)), ydoc.getXmlFragment("prosemirror"));
  return Y.encodeStateAsUpdate(ydoc);
}

/** The ids of the blocks the editor's root holds, in document order. */
function idsBuilt(root: HTMLElement): string[] {
  return Array.from(
    root.querySelectorAll("[data-block-id]"),
    (block) => block.getAttribute("data-block-id") ?? ""
  );
}

/** A build `startTheBuild` has started and not waited for. */
interface Started {
  /** The ids each call of `onBatch` was handed, one entry per call. */
  readonly asked: (readonly string[])[];
  /** Each batch once its blocks were in the editor. */
  readonly batches: Batch[];
  /** The handle the build ends in. */
  readonly building: Promise<ProofEditorHandle>;
  readonly root: HTMLElement;
  /** Both halves of every `onBatch` call, and after the second half the page's own 0 ms timer,
   *  set in that batch's task (`thePageSetsATimer`): where that timer falls among the batches
   *  is where the build handed the thread back. */
  readonly seen: string[];
}

/** Starts the editor's build in `window` over a Y.Doc holding `update`, with `hold` among its
 *  options, and answers without waiting for the build to end. */
function startTheBuild(
  window: HappyWindow,
  update: Uint8Array,
  hold: Pick<CreateProofEditorOptions, "betweenBatches">
): Started {
  const ydoc = new Y.Doc();
  Y.applyUpdate(ydoc, update);
  const root = window.document.body.appendChild(
    window.document.createElement("div")
  ) as unknown as HTMLElement;
  const asked: (readonly string[])[] = [];
  const batches: Batch[] = [];
  const seen: string[] = [];
  const building = createProofEditor(root, {
    ...hold,
    heatMapMode: "hidden",
    onBatch: (ids) => {
      const before = blocksBuilt(root);
      asked.push(ids);
      seen.push(`building ${ids.join(" ")} over ${before} blocks`);
      return () => {
        const after = blocksBuilt(root);
        batches.push({ after, before, ids });
        seen.push(`built: ${after} blocks`);
        thePageSetsATimer(seen);
      };
    },
    onMarkClick: () => {},
    onMarkHover: () => {},
    user: { color: "#0369a1", name: "alice" },
    ydoc,
  });
  return { asked, batches, building, root, seen };
}

// A host may hold the build between two batches (`betweenBatches`), to put a write between two
// batches it names rather than wherever the machine's speed leaves it. A host that passes no
// hold leaves the build handing the thread back exactly as it did before there was a hold to
// pass (dispatch://INFRA-341, R11).
describe("a hold between two batches", () => {
  test("is the build's own hand-back where the host passes none", async () => {
    const update = await theDocumentAsAnUpdate();
    /** What one build of the document showed, with `hold` among its options. `performance.now()`
     *  stands still through it, so every batch costs nothing by the build's own measure, each is
     *  sized off the batch before it alone, and two builds of one document take the same
     *  batches, which a clock that moves with the machine's load does not promise. */
    const seenWith = (hold: Pick<CreateProofEditorOptions, "betweenBatches">) =>
      withDomWindow((window) =>
        withStandIn(
          performance,
          "now",
          () => 0,
          async () => {
            const { batches, building, seen } = startTheBuild(window, update, hold);
            const handle = await building;
            const { view } = handle;
            void handle.destroy();
            while (!view.isDestroyed) await Promise.resolve();
            return { batches: batches.length, seen };
          }
        )
      );

    const withNone = await seenWith({});
    const withEndOfTask = await seenWith({ betweenBatches: endOfTask });

    expect(withNone.batches).toBeGreaterThan(1);
    expect(withNone.seen).toEqual(withEndOfTask.seen);
  });

  test("holds the build at the batch it holds at, and the build ends once it lets go", async () => {
    const update = await theDocumentAsAnUpdate();
    await withDomWindow(async (window) => {
      /** Never the last batch: the first takes eight of the document's blocks and the second
       *  at most four times as many, which leaves blocks for a third. */
      const heldAt = 2;
      const told: number[] = [];
      const holding = Promise.withResolvers<"held">();
      const letGo = Promise.withResolvers<void>();
      const { asked, batches, building, root } = startTheBuild(window, update, {
        betweenBatches: (batchesDone) => {
          told.push(batchesDone);
          if (batchesDone !== heldAt) return endOfTask();
          holding.resolve("held");
          return letGo.promise;
        },
      });

      const first = await Promise.race([holding.promise, building.then(() => "ended" as const)]);
      expect(first, "the build ended without ever stopping at the hold").toBe("held");
      // Held: the editor holds the blocks of the batches done, and not one block more.
      const done = batches.flatMap((batch) => batch.ids);
      expect(told).toEqual(Array.from({ length: heldAt }, (_, index) => index + 1));
      expect(asked.length).toBe(heldAt);
      expect(batches.length).toBe(heldAt);
      expect(done.length).toBeLessThan(BLOCKS);
      expect(idsBuilt(root)).toEqual(done);
      // Twenty tasks, in any one of which a build nothing held would have placed its next batch.
      for (let task = 0; task < 20; task += 1) await endOfTask();
      expect(asked.length, "the build placed a batch while it was held").toBe(heldAt);
      expect(idsBuilt(root)).toEqual(done);

      letGo.resolve();
      const handle = await building;
      try {
        expect(blocksBuilt(root)).toBe(BLOCKS);
        expect(idsBuilt(root)).toEqual(asked.flat());
        expect(batches.length).toBeGreaterThan(heldAt);
        // Told after every batch but the last, the count of batches placed each time.
        expect(told).toEqual(Array.from({ length: batches.length - 1 }, (_, index) => index + 1));
      } finally {
        const { view } = handle;
        void handle.destroy();
        while (!view.isDestroyed) await Promise.resolve();
      }
    });
  });
});
