import { onTestFinished } from "bun:test";

import { createHeadlessProof, type HeadlessProofEditor } from "@legion/proof-editor/headless";

import { compiledBlockSchema } from "../doc/schema";
import { setParseWorkerFactory } from "./markdown-engine";
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
 */

type Listener = (event: unknown) => void;

/** The read-only engine the real worker builds, here on the test's own thread, built once. */
let headless: Promise<HeadlessProofEditor> | undefined;

function tick(): Promise<void> {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, 0);
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
   *  the body. Nothing here bounds it: a body that never comes is ended by the row's own deadline,
   *  and the row then fails naming the bodies it waited for and the bodies the engine sent. */
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
  /** The rows' waits for a count of bodies still to come, each resolved by the post that reaches
   *  its count. */
  const waits: { count: number; resolve: () => void }[] = [];
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
        wait.resolve();
      }
    });
    workers.push(worker);
    return worker as unknown as Worker;
  });

  function waitForRequests(count: number): Promise<void> {
    if (taken.length >= count) {
      return Promise.resolve();
    }
    const { promise, resolve } = Promise.withResolvers<void>();
    let resolved = false;
    waits.push({
      count,
      resolve: () => {
        resolved = true;
        resolve();
      },
    });
    // Bun 1.4.4 runs a row's `onTestFinished` after the row's `afterEach` hooks, the preload's
    // included, for a row its deadline ended too and on every rerun, and reports what it throws
    // against that row; a hook registered mid-row, after awaits, attaches to the row running. So a
    // wait still outstanding when the row ends fails it naming what it waited for, beside Bun's own
    // "timed out" line, with no bound of the fixture's own, and the throw skips none of the row's
    // other hooks, which have already run.
    onTestFinished(() => {
      if (!resolved) {
        throw new Error(
          `The parse worker had ${taken.length} ${taken.length === 1 ? "body" : "bodies"} when ` +
            `the row ended, and the row waited for ${count}: a body longer than ` +
            "markdownWorkerThreshold is still parsed on the page's own thread, or the Markdown " +
            "engine never loaded."
        );
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

/** Puts the page's own worker factory back, so the next page creates a real one. */
export function restoreParseWorker(): void {
  setParseWorkerFactory(undefined);
}

/** Lets the tasks already queued run, so a body the engine was about to send has been sent by the
 *  time a row counts what it sent. A row calls it to prove a body was not sent, which no post can
 *  resolve, and only once the engine has loaded: a loaded engine sends a body in the call that asks
 *  for it, or on the one zero-delay task a new worker's ready handshake takes (`FakeParseWorker`),
 *  and zero-delay tasks run in the order they were queued, so the five here run after it however
 *  slow the machine. It counts tasks and races no load; a wait for a body is `waitForRequests`. */
export async function flushTasks(): Promise<void> {
  for (let tries = 0; tries < 5; tries += 1) {
    await tick();
  }
}
