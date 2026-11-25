import { jest, onTestFinished } from "bun:test";

import { createHeadlessProof, type HeadlessProofEditor } from "@legion/proof-editor/headless";

import { compiledBlockSchema } from "../doc/schema";
import { setParseWorkerFactory, warmMarkdownRenderer } from "./markdown-engine";
// The worker's own protocol, which this fake answers; a type import, so nothing of the worker's
// module runs in the test's process.
import type { DocumentJSON, ParseReply, ParseRequest } from "./markdown-parse.worker";

/**
 * The page's parse worker, faked in process, for the rows that prove what a reader sees while a
 * body longer than `markdownWorkerThreshold` is parsed off the page's own thread
 * (sjawhar/legion#2519; `markdown-engine.ts` and `MarkdownBody.test.tsx` hold those rows).
 *
 * `markdown-engine.ts` creates its worker through one module-level factory, which
 * `setParseWorkerFactory` replaces, so a row installs this fake in its place and the engine's own
 * protocol runs unchanged:
 *
 * - the worker posts `{ ready: true }` once its imports have loaded, on a later task;
 * - the engine posts each body as `{ id, markdown, softBreaks }`;
 * - the worker answers `{ id, doc }`, where `doc` is the parsed document as the plain JSON
 *   `ProseMirrorNode.toJSON()` gives, and `{ id }` with no `doc` for a body the parser refused;
 * - a worker that crashed fires `error` and answers nothing;
 * - `terminate()` stops it: it posts nothing after that.
 *
 * The engine posts bodies and nothing else: the real worker builds its engine from the manifest
 * this build compiled in, so no schema is posted to it.
 *
 * Unlike the real worker nothing here answers on its own: a row calls `answer` or `crash`, so the
 * row decides when each answer lands, and can land a newer body's answer before an older one's.
 * Installing a factory leaves the engine holding no worker and no cached answer, so each row
 * starts from a page that has created none and has parsed no long body, a rerun of its own file
 * included; `restoreParseWorker` puts the page's own factory back, emptying the cache again.
 *
 * A file that installs the fake calls `restoreParseWorker` from an `afterEach` of its own, which
 * is registered again on each `--rerun-each` run and which Bun 1.4.4 runs before the preload's
 * cleanup: that is where a wait no body ended is ended (`unfinished`).
 */

type Listener = (event: unknown) => void;

/** The read-only engine the real worker builds, here on the test's own thread, built once. */
let headless: Promise<HeadlessProofEditor> | undefined;

/** Every wait no body has ended yet, over every fake a row installed. A row that ends while one
 *  stands waited inside React's `act`, which that wait holds open, and React keeps one queue for
 *  every surface: the preload's cleanup would unmount inside that act, the next row's render
 *  paint nothing, and each row after it fail on a body that never appears. `restoreParseWorker`
 *  ends each one, which closes the act, from the file's own `afterEach`, before that cleanup. A
 *  hook this module registered itself would run on a file's first run only: a rerun keeps the
 *  module loaded and registers nothing of it again. */
const unfinished = new Set<{ giveUp: () => void }>();

/**
 * A task on. The stand-in answers and crashes a task after a row asks it to, as a worker's
 * message comes on a task of its own, and `flushTasks` steps a row over the tasks the stand-in
 * and the engine queue, as `waitForRequests` does once the engine has loaded for a first body: the
 * stand-in's `ready`, set at zero delay as the worker is made, and the body the engine posts once
 * it comes. Under a fake clock nothing else moves that clock, so the task is run here, as Testing
 * Library's own waits run theirs: `markdown-engine.test.ts`'s threshold row and
 * `PriorityDescription.test.tsx`'s stored-description row run that way.
 *
 * This one stays on the real clock (INFRA-1505) for the rows that drive the stand-in there:
 * `flushTasks`'s callers in `MarkdownBody.test.tsx`, and that file's other worker rows read
 * what one slice of a paint leaves, whose budget is read from `performance.now()`, which
 * `jest.useFakeTimers()` freezes (that file's `nextTask`); `MarkdownPreview.test.tsx`'s long-body
 * row waits on it only for the stand-in's own answer.
 */
function tick(): Promise<void> {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, 0);
    // Bun's fake clock marks the `setTimeout` it holds with its `clock`.
    if (Object.hasOwn(setTimeout, "clock")) jest.advanceTimersByTime(0);
  });
}

/** Stands in for one `Worker` the engine created: it takes each body it is posted and emits what
 *  a row tells it to, through both the `onmessage`/`onerror` properties and the listeners, so
 *  the engine is free to use either. */
export class FakeParseWorker {
  terminated = false;
  onerror: Listener | null = null;
  onmessage: Listener | null = null;
  private readonly listeners: Record<string, Listener[]> = { error: [], message: [] };
  private readonly took: (request: ParseRequest, worker: FakeParseWorker) => void;

  constructor(took: (request: ParseRequest, worker: FakeParseWorker) => void) {
    this.took = took;
    // The stand-in's `ready`, on a task of its own as a worker's first message comes. This one
    // stays on the real clock (INFRA-1505) for the rows `tick` above stays there for: they read
    // what one slice of a paint leaves, whose budget is read from `performance.now()`, which
    // `jest.useFakeTimers()` freezes, and they step over this task with `flushTasks`. A row on the
    // fake clock runs it with that clock, as any timer.
    setTimeout(() => {
      this.emit("message", { data: { ready: true } });
    }, 0);
  }

  addEventListener(type: string, listener: Listener): void {
    this.listeners[type]?.push(listener);
  }

  removeEventListener(type: string, listener: Listener): void {
    const listeners = this.listeners[type];
    const at = listeners?.indexOf(listener) ?? -1;
    if (listeners !== undefined && at !== -1) {
      listeners.splice(at, 1);
    }
  }

  postMessage(request: ParseRequest): void {
    this.took(request, this);
  }

  terminate(): void {
    this.terminated = true;
  }

  emit(type: "error" | "message", event: unknown): void {
    if (this.terminated) {
      return;
    }
    const own = type === "message" ? this.onmessage : this.onerror;
    own?.(event);
    for (const listener of [...(this.listeners[type] ?? [])]) {
      listener(event);
    }
  }
}

/** What a row drives the faked worker with. */
export interface InstalledParseWorker {
  /** How many times the engine asked the factory for a worker, a refused creation included. */
  readonly created: number;
  /** Every body the engine has sent to be parsed, oldest first, across workers. */
  readonly requests: readonly ParseRequest[];
  /** Every worker the factory made, oldest first. */
  readonly workers: readonly FakeParseWorker[];
  /** Answers the `index`th body the engine sent, once it has sent it: the document the read-only
   *  engine parses in process, or a refusal where that parser throws, delivered on a later task.
   *  Resolves once the answer has been delivered. */
  answer(index: number): Promise<void>;
  /** Fires `error` on the worker holding the `index`th body, as a worker that crashed mid-parse
   *  does, and answers nothing for it. */
  crash(index: number): Promise<void>;
  /** Resolves on the post that brings the bodies the engine has sent to `count`, however long the
   *  engine takes to load and send them: it waits for the post itself, not for a number of tasks,
   *  so a lazy load that a slow or loaded machine, or the cold-load lane, holds back still ends in
   *  the body. A wait for the first body also ends on the engine's load: with the load's own error
   *  where the load fails, and once the worker's ready task has run where the engine loaded and
   *  posted nothing. Any other wait ends at the row's own deadline, and the row then fails naming
   *  the bodies it waited for and the bodies the engine sent. */
  waitForRequests(count: number): Promise<void>;
}

/** Installs the fake in place of the page's `new Worker(…)`. `refuseToCreate` makes the factory
 *  throw instead, as a page that forbids a worker does. */
export function installFakeParseWorker({
  refuseToCreate = false,
}: {
  refuseToCreate?: boolean;
} = {}): InstalledParseWorker {
  const workers: FakeParseWorker[] = [];
  const taken: { request: ParseRequest; worker: FakeParseWorker }[] = [];
  /** The rows' waits for a count of bodies still to come, each ended by the post that reaches its
   *  count, or by the end of the row that made it (`unfinished`). */
  const waits: { count: number; bodyCame: () => void; giveUp: () => void }[] = [];
  let asked = 0;

  setParseWorkerFactory(() => {
    asked += 1;
    if (refuseToCreate) {
      throw new Error("This page cannot create a parse worker.");
    }
    const worker = new FakeParseWorker((request, from) => {
      taken.push({ request, worker: from });
      for (const wait of waits.filter(({ count }) => count <= taken.length)) {
        waits.splice(waits.indexOf(wait), 1);
        wait.bodyCame();
      }
    });
    workers.push(worker);
    return worker as unknown as Worker;
  });

  function waitForRequests(count: number): Promise<void> {
    if (taken.length >= count) {
      return Promise.resolve();
    }
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    /** What the row waited for and what the worker had, read when the wait ends. */
    const failure = (when: string) =>
      new Error(
        `The parse worker had ${taken.length} ${taken.length === 1 ? "body" : "bodies"} ${when}, ` +
          `and the row waited for ${count}: a body longer than markdownWorkerThreshold is still ` +
          "parsed on the page's own thread, or the Markdown engine never loaded."
      );
    /** How the wait ended: a body came, the engine's load failed, no body came once the engine
     *  had loaded, or the row ended first. */
    let ended: "came" | "load failed" | "no body" | "row ended" | undefined;
    const wait = {
      count,
      /** The post that brings the bodies to `count`. */
      bodyCame: () => {
        ended ??= "came";
        unfinished.delete(wait);
        resolve();
      },
      /** The end of the row that made the wait, from `restoreParseWorker` (`unfinished`). */
      giveUp: () => {
        ended ??= "row ended";
        unfinished.delete(wait);
        reject(failure("when the row ended"));
      },
    };
    waits.push(wait);
    unfinished.add(wait);
    if (taken.length === 0) {
      // The first body waits on one event, the engine's load, however long a loaded machine or the
      // cold-load lane holds it. Once it has loaded the body is posted, if it ever will be, by the
      // loaded engine's render, which ran on that same load before this, and the ready handshake
      // of the worker that render created: `flushTasks` runs after that handshake's task. So a
      // wait no body has ended by then never will be, and it fails at once rather than at the
      // row's deadline. A load that fails posts nothing either, and the wait fails with the load's
      // own error, which is the cause. Later bodies follow what the row itself does next, so only
      // the first ends on the load.
      void (async () => {
        let loadFailure: { error: unknown } | undefined;
        try {
          await warmMarkdownRenderer();
        } catch (error) {
          loadFailure = { error };
        }
        if (loadFailure === undefined) {
          await flushTasks();
        }
        if (taken.length > 0 || !unfinished.has(wait)) {
          return;
        }
        unfinished.delete(wait);
        waits.splice(waits.indexOf(wait), 1);
        if (loadFailure !== undefined) {
          ended ??= "load failed";
          reject(loadFailure.error);
        } else {
          ended ??= "no body";
          reject(failure("once the Markdown engine had loaded"));
        }
      })();
    }
    // Bun 1.4.4 runs a row's `onTestFinished` after the row's `afterEach` hooks, the preload's
    // included, for a row its deadline ended too and on every rerun, and reports what it throws
    // against that row; a hook registered mid-row, after awaits, attaches to the row running. So a
    // wait the row's end gave up fails its row naming what it waited for, beside Bun's own "timed
    // out" line, with no bound of the fixture's own, and the throw skips none of the row's other
    // hooks, which have already run. `restoreParseWorker`, from the file's `afterEach`, ended the
    // wait itself first; Bun reports that rejection once more as an unhandled error between tests,
    // with this same text, since the row it belonged to has ended. A wait that ended on the load
    // has already failed the row from its own `await`.
    onTestFinished(() => {
      if (ended === undefined || ended === "row ended") {
        throw failure("when the row ended");
      }
    });
    return promise;
  }

  async function sent(index: number): Promise<{ request: ParseRequest; worker: FakeParseWorker }> {
    await waitForRequests(index + 1);
    return taken[index];
  }

  return {
    get created() {
      return asked;
    },
    get requests() {
      return taken.map(({ request }) => request);
    },
    get workers() {
      return workers;
    },
    async answer(index) {
      const { request, worker } = await sent(index);
      headless ??= createHeadlessProof({ blockSchema: compiledBlockSchema, readOnly: true });
      const engine = await headless;
      let doc: DocumentJSON | undefined;
      try {
        doc = engine.parseMarkdown(request.markdown, request.softBreaks).toJSON() as DocumentJSON;
      } catch {
        doc = undefined;
      }
      const reply: ParseReply = { doc, id: request.id };
      await tick();
      worker.emit("message", { data: reply });
    },
    async crash(index) {
      const { worker } = await sent(index);
      await tick();
      worker.emit("error", new Event("error"));
    },
    waitForRequests,
  };
}

/** Ends every wait no body ended (`unfinished`), then puts the page's own worker factory back, so
 *  the next page creates a real one. A file that installs the fake calls it from an `afterEach` of
 *  its own. */
export function restoreParseWorker(): void {
  for (const wait of [...unfinished]) {
    wait.giveUp();
  }
  setParseWorkerFactory(undefined);
}

/** Lets the tasks already queued run, so a body the engine was about to send has been sent by the
 *  time a row counts what it sent. A row calls it to prove a body was not sent, which no post can
 *  resolve, and only once the engine has loaded: a loaded engine sends a body in the call that asks
 *  for it, or on the one zero-delay task a new worker's ready handshake takes (`FakeParseWorker`),
 *  and zero-delay tasks run in the order they were queued, so the five here run after it however
 *  slow the machine. It counts tasks and races no load; a wait for a body is `waitForRequests`.
 *  Its callers are `MarkdownBody.test.tsx`'s worker rows, on the real clock,
 *  `PriorityDescription.test.tsx`'s stored-description row, on the fake one (`tick`), and
 *  `waitForRequests`'s wait for a first body. */
export async function flushTasks(): Promise<void> {
  for (let tries = 0; tries < 5; tries += 1) {
    await tick();
  }
}
