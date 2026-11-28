// A Legion daemon's state route, for the browser rows that need Dispatch's Legion intake (k25 and
// k28 of dispatch://INFRA-2918/artifact/plan-md, T42): a local HTTP server that answers the
// `state_url` a project's Legion setting names, as the daemon does, with a state the row sets.
//
// The state is the daemon's own wire shape: the goldens the daemon's tests write, copied under
// packages/envoy/internal/dispatch/legion/testdata (its README names the commits). The body is
// `state-signalchanges.json` with `ceiling` from `state-capchanges.json`, and the row sets what a
// whole read of Dispatch reads to start the project's state events from (`daemon.stateId`,
// `daemon.stateSeq`, `daemon.stateRun`) and the admission block's `cap`, `capChanges` and
// `signalChanges`. Each entry of the two records is built from the golden's own entry
// (`capChange`, `signalChange`), so it carries every member the daemon serves and no other.
//
// Dispatch reads `GET /legion/v1/state?view=observed` (`legion.Client.State`), and the stub answers
// that view as the daemon does (`LegionObservedStateResponse`): the full state's `daemon`,
// `settings` and `admission`, and each issue in the members the view keeps. A request with no view
// gets the full state. `answer(503)` makes every read fail until `answer(200)`, for k25's failed
// whole read; every read is recorded (`reads`).
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";

import { LegionObservedStateResponse } from "@legion/contracts/legion-api";

/** The daemon's state route (`legion.StatePath`). */
const STATE_PATH = "/legion/v1/state";

/** The view Dispatch asks the route for (`legion.stateView`). */
const OBSERVED_VIEW = "observed";

/** Who made a change of a record: the controller, with its session, or the operator. */
export interface DaemonActor {
  readonly kind: string;
  readonly session?: string;
}

/** One entry of `admission.capChanges` (the daemon's `store.CapChange`, dispatch://INFRA-5616). */
export interface CapChange {
  readonly id: number;
  readonly cap?: number;
  readonly capBefore: number;
  readonly capAfter: number;
  readonly ceiling: number;
  readonly setBy: DaemonActor;
  readonly setAt: string;
  readonly reason: string;
}

/** One entry of `admission.signalChanges` (the daemon's `store.SignalChange`,
 *  dispatch://INFRA-5899): a signal's value, its limit or its hold, or the switch. */
export interface SignalChange {
  readonly id: number;
  readonly at: string;
  readonly signal: string | null;
  readonly part: string | null;
  readonly what: "value" | "limit" | "holds" | "switch";
  readonly value: number | null;
  readonly absent: string | null;
  readonly setBy: DaemonActor | null;
  readonly reason: string | null;
}

/** What an admission block reports of the two records: the cap in force (null for none) and the
 *  entries of `capChanges` and `signalChanges`, newest first as the daemon serves them. */
export interface AdmissionRecords {
  readonly cap: number | null;
  readonly capChanges: readonly CapChange[];
  readonly signalChanges: readonly SignalChange[];
}

/** What the row sets of the daemon's store: its id, the run that serves it and its newest
 *  sequence, and what its admission block reports. */
export interface DaemonStore extends AdmissionRecords {
  readonly stateId: string;
  readonly stateRun: string;
  readonly stateSeq: number;
}

/** One read the stub answered: when, the view it named and the status it answered. */
export interface StubRead {
  readonly at: number;
  readonly view: string | null;
  readonly status: number;
}

/** A running stub: the `state_url` a project's Legion setting names, the store it serves, and its
 *  controls. */
export interface LegionDaemonStub {
  /** The daemon's base address, which `PUT /api/v1/projects/{key}/legion/daemon` takes: an http
   *  URL with a host and no path. */
  readonly url: string;
  /** The store it serves now. */
  store(): DaemonStore;
  /** Serves `changes` over the store from the next read on. */
  set(changes: Partial<DaemonStore>): void;
  /** Answers every read from the next one on with `status`: 200 with the state, or 503. */
  answer(status: 200 | 503): void;
  /** Every read answered, oldest first. */
  reads(): readonly StubRead[];
  close(): Promise<void>;
}

type Json = { [key: string]: unknown };

/** The two goldens, read once. */
let goldens: { signals: Json; caps: Json } | undefined;

function goldenOf(name: string): Json {
  const path = new URL(`../../envoy/internal/dispatch/legion/testdata/${name}`, import.meta.url);
  return JSON.parse(readFileSync(path, "utf8")) as Json;
}

function readGoldens(): { signals: Json; caps: Json } {
  goldens ??= {
    caps: goldenOf("state-capchanges.json"),
    signals: goldenOf("state-signalchanges.json"),
  };
  return goldens;
}

/** `value` as an object, refusing any other JSON: the goldens are the daemon's, so a member this
 *  file reads that is not an object is a golden moved under it. */
function objectOf(value: unknown, what: string): Json {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`the daemon golden's ${what} is no object`);
  }
  return value as Json;
}

/** The first entry of a golden's admission record `record`, the shape every entry of it is built
 *  in. */
function goldenEntry(golden: Json, record: string): Json {
  const entries = objectOf(golden.admission, "admission")[record];
  if (!Array.isArray(entries) || entries.length === 0) {
    throw new Error(`the daemon golden's admission.${record} holds no entry`);
  }
  return objectOf(entries[0], `admission.${record}[0]`);
}

/** `fields` laid over `template`, a golden's entry, each a member the golden's entry carries. */
function entryLike<T extends object>(template: Json, fields: T, record: string): T {
  const unknown = Object.keys(fields).filter((key) => !(key in template));
  if (unknown.length > 0) {
    throw new Error(
      `admission.${record} entries carry no ${unknown.join(", ")} in the daemon's golden`
    );
  }
  return { ...template, ...fields } as T;
}

/** A `capChanges` entry in the daemon's shape: `state-capchanges.json`'s first entry with
 *  `fields` over it. `cap` is the setting after the change, which a clear leaves out. */
export function capChange(fields: Omit<CapChange, "cap"> & { cap: number | null }): CapChange {
  const { cap, ...rest } = fields;
  const entry = entryLike(goldenEntry(readGoldens().caps, "capChanges"), rest, "capChanges");
  const { cap: _template, ...cleared } = entry as CapChange;
  return cap === null ? cleared : { ...cleared, cap };
}

/** A `signalChanges` entry in the daemon's shape: `state-signalchanges.json`'s first entry with
 *  `fields` over it. */
export function signalChange(fields: SignalChange): SignalChange {
  return entryLike(goldenEntry(readGoldens().signals, "signalChanges"), fields, "signalChanges");
}

/** The admission block of the daemon's state reporting `records`: the signals golden's block with
 *  the cap and the two records given, and the cap golden's ceiling. */
export function admissionBlock(records: AdmissionRecords): Json {
  const { caps, signals } = readGoldens();
  const {
    cap: _cap,
    capChanges: _capChanges,
    signalChanges: _signalChanges,
    ...admission
  } = objectOf(signals.admission, "admission");
  return {
    ...admission,
    ceiling: objectOf(caps.admission, "admission").ceiling,
    ...(records.cap === null ? {} : { cap: records.cap }),
    capChanges: records.capChanges,
    signalChanges: records.signalChanges,
  };
}

/** The full state the store gives: the signals golden with the store's daemon members and its
 *  admission block (`admissionBlock`). */
function fullState(project: string, store: DaemonStore): Json {
  const { signals } = readGoldens();
  return {
    ...signals,
    admission: admissionBlock(store),
    daemon: {
      ...objectOf(signals.daemon, "daemon"),
      project,
      stateId: store.stateId,
      stateRun: store.stateRun,
      stateSeq: store.stateSeq,
    },
  };
}

/** A claim as the observed view shows it: its state alone. */
function observedClaim(claim: unknown): Json | undefined {
  const state = typeof claim === "object" && claim !== null ? Reflect.get(claim, "state") : null;
  return typeof state === "string" ? { state } : undefined;
}

/** One issue of the full state in the members the observed view keeps (`api.ObservedIssue`): its
 *  key, phase and tree, its slot's index, what it waits on, its architect's claim state, and each
 *  worker's claim state, leaving out what the issue does not hold. */
function observedIssue(issue: Json): Json {
  const slot = issue.slot === undefined ? undefined : objectOf(issue.slot, "issue's slot");
  const workers = Object.entries(objectOf(issue.workers ?? {}, "issue's workers")).flatMap(
    ([role, worker]) => {
      const claim = observedClaim(objectOf(worker, `worker ${role}`).claim);
      return claim === undefined ? [] : [[role, { claim }] as const];
    }
  );
  const architect = observedClaim(issue.architect);
  return {
    key: issue.key,
    phase: issue.phase,
    ...(issue.tree === undefined ? {} : { tree: issue.tree }),
    ...(slot === undefined ? {} : { slot: { index: slot.index } }),
    ...(issue.waitingOn === undefined ? {} : { waitingOn: issue.waitingOn }),
    ...(architect === undefined ? {} : { architect }),
    ...(workers.length === 0 ? {} : { workers: Object.fromEntries(workers) }),
  };
}

/** The observed view of `state` (`api.ObservedState`), checked against the contract's reader: a
 *  stub whose view the contract refuses is no daemon's, and fails the row that started it. */
function observedState(state: Json): Json {
  const issues = Object.fromEntries(
    Object.entries(objectOf(state.issues, "issues")).map(([key, issue]) => [
      key,
      observedIssue(objectOf(issue, `issue ${key}`)),
    ])
  );
  const view = {
    admission: state.admission,
    daemon: state.daemon,
    issues,
    settings: state.settings,
  };
  const checked = LegionObservedStateResponse.safeParse(view);
  if (!checked.success) {
    throw new Error(`the stub's observed view is not the daemon's: ${checked.error.message}`);
  }
  return view;
}

/** Starts a stub daemon of `project` on 127.0.0.1 at a port the system picks, serving `store`. */
export async function startLegionDaemonStub(
  project: string,
  store: DaemonStore
): Promise<LegionDaemonStub> {
  let served = store;
  let status: 200 | 503 = 200;
  const reads: StubRead[] = [];
  // The first view is built before the port opens, so a store the contract refuses fails here.
  observedState(fullState(project, served));
  const server = createServer((request, response) => {
    const address = new URL(request.url ?? "/", "http://stub.invalid");
    if (request.method !== "GET" || address.pathname !== STATE_PATH) {
      response.writeHead(404, { "Content-Type": "text/plain" });
      response.end("not found");
      return;
    }
    const view = address.searchParams.get("view");
    reads.push({ at: Date.now(), status, view });
    if (status !== 200) {
      response.writeHead(status, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ code: "UNAVAILABLE", error: "the daemon is stopping" }));
      return;
    }
    const state = fullState(project, served);
    const body = JSON.stringify(view === OBSERVED_VIEW ? observedState(state) : state);
    response.writeHead(200, {
      "Content-Length": Buffer.byteLength(body),
      "Content-Type": "application/json",
    });
    response.end(body);
  });
  const listening = Promise.withResolvers<void>();
  server.once("error", listening.reject);
  server.listen(0, "127.0.0.1", () => listening.resolve());
  await listening.promise;
  const { port } = server.address() as AddressInfo;
  return {
    answer: (next) => {
      status = next;
    },
    close: () => {
      const closed = Promise.withResolvers<void>();
      server.close((error) => (error === undefined ? closed.resolve() : closed.reject(error)));
      server.closeAllConnections();
      return closed.promise;
    },
    reads: () => reads,
    set: (changes) => {
      served = { ...served, ...changes };
      observedState(fullState(project, served));
    },
    store: () => served,
    url: `http://127.0.0.1:${port}`,
  };
}
