/**
 * The room's first sync in parts (decision 547): `splitSync` cuts the Yjs V1 update of the
 * server's first SyncStep2 into updates of at most `maxStructs` structs each, which the
 * connection applies one to a task (`firstSyncInParts`, `doc/connection.ts`). The cut reads no
 * document: `Y.decodeUpdate` lists the update's structs, client by client, and each part writes
 * its own run of each client again with `Y.UpdateEncoderV1`, its first clock stated, so a part
 * boundary may fall inside one client's run. The delete set is written in the last part alone,
 * where every struct it names has been applied; every earlier part holds an empty one. Applied in
 * order, the parts give the document the whole update gives (`sync-parts.test.ts`, on the
 * captured shapes).
 *
 * `splitSyncInWorker` runs the cut in `sync-parts.worker.ts`, off the page's thread: on the largest
 * seed it took 24 ms at 1440 px and 29 ms at 390 px, each the fourth smallest of five loads
 * (dispatch://INFRA-5939/comment/6dc882f3-5a0d-44d6-9ff8-591f2ace4f2b). Where no worker can be
 * made or the worker fails, the page cuts the update itself, in a task of its own.
 */
import { writeVarUint } from "lib0/encoding";
import * as Y from "yjs";

/** The structs of one part, the probe's size: 53 parts on the largest seed, each applied in a
 *  task the Long Tasks API never reported (none over 50 ms at 390 px and a slowdown of 4). */
export const PART_STRUCTS = 150;

type DecodedUpdate = ReturnType<typeof Y.decodeUpdate>;
type Struct = DecodedUpdate["structs"][number];

/** One part: the runs of its structs, each of one client, in the update's order. */
function encodePart(structs: readonly Struct[], ds: DecodedUpdate["ds"] | undefined): Uint8Array {
  const runs: Struct[][] = [];
  for (const struct of structs) {
    const run = runs.at(-1);
    if (run !== undefined && run[0].id.client === struct.id.client) run.push(struct);
    else runs.push([struct]);
  }
  const encoder = new Y.UpdateEncoderV1();
  writeVarUint(encoder.restEncoder, runs.length);
  for (const run of runs) {
    writeVarUint(encoder.restEncoder, run.length);
    encoder.writeClient(run[0].id.client);
    writeVarUint(encoder.restEncoder, run[0].id.clock);
    for (const struct of run) struct.write(encoder, 0);
  }
  // The delete set as Yjs writes one, in the order the update listed its clients.
  const clients = ds === undefined ? [] : [...ds.clients];
  writeVarUint(encoder.restEncoder, clients.length);
  for (const [client, deletes] of clients) {
    encoder.resetDsCurVal();
    writeVarUint(encoder.restEncoder, client);
    writeVarUint(encoder.restEncoder, deletes.length);
    for (const deleted of deletes) {
      encoder.writeDsClock(deleted.clock);
      encoder.writeDsLen(deleted.len);
    }
  }
  return encoder.toUint8Array();
}

/** Cuts a Yjs V1 update into parts of at most `maxStructs` structs, which applied in order give
 *  the whole update's document. An update with no struct is one part, holding its delete set. */
export function splitSync(update: Uint8Array, maxStructs = PART_STRUCTS): Uint8Array[] {
  if (!Number.isInteger(maxStructs) || maxStructs < 1) {
    throw new RangeError(`a part holds at least one struct, not ${maxStructs}`);
  }
  const { ds, structs } = Y.decodeUpdate(update);
  const parts: Uint8Array[] = [];
  for (let start = 0; start < structs.length || parts.length === 0; start += maxStructs) {
    const end = start + maxStructs;
    parts.push(encodePart(structs.slice(start, end), end >= structs.length ? ds : undefined));
  }
  return parts;
}

/** What the page posts the worker: the update to cut and the size of a part. */
export interface SplitRequest {
  readonly maxStructs: number;
  readonly update: Uint8Array;
}

/** What the worker answers: the parts, in order. */
export interface SplitAnswer {
  readonly parts: Uint8Array[];
}

/**
 * The page's own worker, which Vite bundles as a chunk of its own (`sync-parts.worker-<hash>.js`).
 * The `new Worker(new URL(…), { type: "module" })` below is one expression because that is the
 * shape a bundler recognises; split in two, the worker's module is copied out as an asset and
 * never bundled (as `newParseWorker` of `refs/markdown-engine.ts`).
 */
function newSplitWorker(): Worker {
  return new Worker(new URL("./sync-parts.worker.ts", import.meta.url), { type: "module" });
}

export interface SplitInWorkerOptions {
  /** The size of a part; `PART_STRUCTS` unless given. */
  readonly maxStructs?: number;
  /** Makes the worker; the unit cases replace it. */
  readonly createWorker?: () => Worker;
  /** Runs the page's own cut in a task of its own; `setTimeout(0)` unless given. */
  readonly schedule?: (run: () => void) => void;
}

/** The worker `createWorker` makes, or none where it throws (no `Worker`, or a refused URL). */
function workerOf(createWorker: () => Worker): Worker | undefined {
  try {
    return createWorker();
  } catch (error) {
    console.warn("Splitting the document's first sync on the page: no worker could be made", error);
    return undefined;
  }
}

/**
 * The parts of `update`, cut in a worker of their own, which ends once it has answered. Where no
 * worker can be made, or it fails, the page cuts the update in a task of its own. The promise
 * rejects only where the cut itself throws (an update Yjs cannot read).
 */
export function splitSyncInWorker(
  update: Uint8Array,
  {
    createWorker = newSplitWorker,
    maxStructs = PART_STRUCTS,
    schedule = (run) => setTimeout(run, 0),
  }: SplitInWorkerOptions = {}
): Promise<Uint8Array[]> {
  const answer = Promise.withResolvers<Uint8Array[]>();
  const onThePage = () =>
    schedule(() => {
      try {
        answer.resolve(splitSync(update, maxStructs));
      } catch (error) {
        answer.reject(error);
      }
    });
  const worker = workerOf(createWorker);
  if (worker === undefined) {
    onThePage();
    return answer.promise;
  }
  // The worker's first word settles the cut: its answer, or an `error` or `messageerror` after
  // which the page cuts the update itself. The worker then ends, so no later word arrives.
  let settled = false;
  const settle = (parts: Uint8Array[] | undefined, failure?: Event) => {
    if (settled) return;
    settled = true;
    worker.terminate();
    if (parts !== undefined) {
      answer.resolve(parts);
      return;
    }
    console.warn("Splitting the document's first sync on the page: the worker failed", failure);
    onThePage();
  };
  worker.addEventListener("error", (event) => settle(undefined, event));
  worker.addEventListener("messageerror", (event) => settle(undefined, event));
  worker.addEventListener("message", (event: MessageEvent<SplitAnswer>) =>
    settle(event.data.parts)
  );
  const request: SplitRequest = { maxStructs, update };
  worker.postMessage(request);
  return answer.promise;
}
