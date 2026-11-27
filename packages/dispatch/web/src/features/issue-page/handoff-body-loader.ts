import { HANDOFF_BODIES_MAX_IDS } from "@legion/contracts";
import { type QueryClient, queryOptions, skipToken } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { HandoffBodies, HandoffBody } from "../../api/types";

/** How long after an answer is drawn the ids it left, and the entries past one request's
 *  `HANDOFF_BODIES_MAX_IDS`, are asked for where the reader has not scrolled (R6). */
const LEFT_WAIT_MS = 2_000;

/** The query key a handoff's body is held under for the page's life. */
export function handoffBodyKey(id: string): readonly ["handoff-body", string] {
  return ["handoff-body", id];
}

/** A handoff's body as the page holds it: an entry of the bodies answer that holds a body. */
export type HeldHandoffBody = HandoffBody & { readonly body: string };

/** The query a handoff's body is held under for the page's life: no time limit, and no read of
 *  its own, as only the loader sets its data (`HandoffBodyLoader`). */
export function handoffBodyQuery(id: string) {
  return queryOptions<HeldHandoffBody>({
    gcTime: Number.POSITIVE_INFINITY,
    queryFn: skipToken,
    queryKey: handoffBodyKey(id),
  });
}

/** What an entry reads of the read of its handoff's body. */
export interface HandoffBodyState {
  /** How many reads of the body failed since it was last read, 0 while none has: a request
   *  that failed, or an answer that held `null` for it. */
  readonly failures: number;
  /** Whether a request that asks for it is out, or is held to go once the one out answers. */
  readonly reading: boolean;
}

const IDLE: HandoffBodyState = { failures: 0, reading: false };

/** Where an id stands in the loader: to be sent as soon as no request is out, in the request
 *  out, or left to be sent at the reader's next scroll or `LEFT_WAIT_MS` after an answer is
 *  drawn. An id of none of these is held, failed, or not asked for. */
type Standing = "queued" | "out" | "waiting";

/**
 * The reads of the bodies of the handoffs the issue's history draws (the plan's D13 and its table
 * B), with no DOM in it: the history tells it which handoff entries are within one screen of
 * Details' view (`band`), that the reader scrolled (`scrolled`), that an answer was drawn
 * (`drawn`), that the history landed (`landed`) and that a Retry was pressed (`retry`), and it
 * asks for the bodies by `api.getHandoffBodies`, one request out at a time, each for at most
 * `HANDOFF_BODIES_MAX_IDS` ids.
 *
 * It sends nothing before the history's own landing, which can name the handoff an address
 * follows: that id leads the first request. An entry that comes into the band is asked for at
 * once, or as the request out answers, with every other that came in meanwhile. The ids past one
 * request's limit, and the ids an answer left to keep within `HANDOFF_BODIES_MAX_BYTES`, are asked
 * for at the reader's next scroll, or `LEFT_WAIT_MS` after an answer is drawn, and only while
 * their entries are still in the band: one that goes out of it is dropped, and asked for at once
 * when it comes back. A failed request fails each id it asked for, and an answer's `null` fails
 * its id; a Retry asks again for every id of that failure still in the band, its own among them.
 *
 * A body is kept in the query cache under `handoffBodyKey` for the page's life, since a version
 * never changes once written, so a body held is never asked for again, by this loader or by the
 * next one. `close` ends it as Details shuts: it sends nothing more and clears its timer, and an
 * answer still out is kept when it comes.
 */
export class HandoffBodyLoader {
  readonly #issueKey: string;
  readonly #client: QueryClient;
  /** The handoffs whose entries are in the band, in the order the history draws them. */
  #band: readonly string[] = [];
  #inBand: ReadonlySet<string> = new Set();
  /** Whether the band has been told once. */
  #bandTold = false;
  /** Whether the history has landed. */
  #landed = false;
  #closed = false;
  readonly #standing = new Map<string, Standing>();
  /** The ids that go first in the next request, whether or not their entries are in the band:
   *  the id the landing names, then each a Retry asks for again, in the order they came. */
  readonly #first = new Set<string>();
  /** The ids of each read that failed: how many times, and which failure, so a Retry asks again
   *  for the others of the same one. */
  readonly #failed = new Map<string, { readonly count: number; readonly failure: number }>();
  #failures = 0;
  /** Whether a request is out. */
  #out = false;
  /** Whether an answer has come that no entry has said is drawn yet. */
  #undrawn = false;
  #timer: ReturnType<typeof setTimeout> | undefined;
  readonly #states = new Map<string, HandoffBodyState>();
  readonly #listeners = new Set<() => void>();

  constructor(issueKey: string, client: QueryClient) {
    this.#issueKey = issueKey;
    this.#client = client;
  }

  /** The handoffs whose entries are within one screen of Details' view, in the order drawn. */
  band(ids: readonly string[]): void {
    this.#band = ids;
    this.#inBand = new Set(ids);
    this.#bandTold = true;
    let changed = false;
    for (const [id, standing] of this.#standing) {
      if (standing !== "out" && !this.#inBand.has(id) && !this.#first.has(id)) {
        this.#standing.delete(id);
        changed = true;
      }
    }
    for (const id of ids) {
      if (this.#standing.has(id) || this.#failed.has(id) || this.#held(id)) continue;
      this.#standing.set(id, "queued");
      changed = true;
    }
    if (changed) this.#changed();
    this.#send();
  }

  /** The reader scrolled Details: the ids left are asked for where their entries are in the
   *  band. */
  scrolled(): void {
    this.#askLeft();
  }

  /** An entry drew what an answer brought: the first such after an answer sets the wait after
   *  which the ids left are asked for. */
  drawn(): void {
    if (!this.#undrawn || this.#closed) return;
    this.#undrawn = false;
    if (this.#timer !== undefined || ![...this.#standing.values()].includes("waiting")) return;
    this.#timer = setTimeout(() => {
      this.#timer = undefined;
      this.#askLeft();
    }, LEFT_WAIT_MS);
  }

  /** The history has landed, on the handoff `first` where an address names one: the loader
   *  starts, with that id first, once the band has been told. */
  landed(first?: string): void {
    if (this.#landed) return;
    this.#landed = true;
    if (first !== undefined && !this.#held(first)) {
      this.#first.add(first);
      this.#standing.set(first, "queued");
      this.#changed();
    }
    this.#send();
  }

  /** A Retry of the entry of `id`, whose read failed: asks again for it and for each other id of
   *  the same failure still in the band, in the order drawn, unless a read of it is out or held
   *  to go. */
  retry(id: string): void {
    const failed = this.#failed.get(id);
    if (this.#closed || failed === undefined || this.#standing.has(id)) return;
    const again = this.#band.filter(
      (other) =>
        other === id ||
        (this.#failed.get(other)?.failure === failed.failure && !this.#standing.has(other))
    );
    for (const each of this.#inBand.has(id) ? again : [id, ...again]) {
      this.#first.add(each);
      this.#standing.set(each, "queued");
    }
    this.#changed();
    this.#send();
  }

  /** Details shut: nothing more is sent, and an answer still out is kept when it comes. */
  close(): void {
    this.#closed = true;
    clearTimeout(this.#timer);
    this.#timer = undefined;
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** What the entry of `id` draws of its body's read; the same object until it changes. */
  state(id: string): HandoffBodyState {
    return this.#states.get(id) ?? IDLE;
  }

  #held(id: string): boolean {
    return this.#client.getQueryData(handoffBodyKey(id)) !== undefined;
  }

  /** Each id left, in the band, is asked for. */
  #askLeft(): void {
    let changed = false;
    for (const [id, standing] of this.#standing) {
      if (standing === "waiting") {
        this.#standing.set(id, "queued");
        changed = true;
      }
    }
    if (changed) this.#changed();
    this.#send();
  }

  /** Sends the next request where none is out: the ids that go first, then the band's new ids
   *  in the order drawn, at most `HANDOFF_BODIES_MAX_IDS`; the band's ids past that wait. */
  #send(): void {
    if (this.#closed || this.#out || !this.#landed || !this.#bandTold) return;
    const ids: string[] = [];
    for (const id of this.#first) {
      if (ids.length < HANDOFF_BODIES_MAX_IDS) ids.push(id);
    }
    const sent = new Set(ids);
    for (const id of this.#band) {
      if (this.#standing.get(id) !== "queued" || sent.has(id)) continue;
      if (ids.length < HANDOFF_BODIES_MAX_IDS) {
        ids.push(id);
        sent.add(id);
      } else {
        this.#standing.set(id, "waiting");
      }
    }
    if (ids.length === 0) return;
    for (const id of ids) {
      this.#first.delete(id);
      this.#standing.set(id, "out");
    }
    this.#out = true;
    this.#changed();
    api.getHandoffBodies(this.#issueKey, ids).then(
      (answer) => this.#answered(ids, answer),
      () => this.#refused(ids)
    );
  }

  #answered(ids: readonly string[], answer: HandoffBodies): void {
    this.#out = false;
    const answeredIds = new Set<string>();
    let failure: number | undefined;
    for (const entry of answer.bodies ?? []) {
      answeredIds.add(entry.id);
      this.#standing.delete(entry.id);
      if (entry.body === null) {
        failure ??= ++this.#failures;
        this.#fail(entry.id, failure);
      } else {
        this.#keep({ ...entry, body: entry.body });
        this.#failed.delete(entry.id);
      }
    }
    // Each id the answer did not hold is left: `left` names them, in the order asked.
    for (const id of ids) {
      if (answeredIds.has(id)) continue;
      if (this.#inBand.has(id) && !this.#closed) this.#standing.set(id, "waiting");
      else this.#standing.delete(id);
    }
    this.#undrawn = true;
    this.#changed();
    this.#send();
  }

  #refused(ids: readonly string[]): void {
    this.#out = false;
    const failure = ++this.#failures;
    for (const id of ids) {
      this.#standing.delete(id);
      this.#fail(id, failure);
    }
    this.#undrawn = true;
    this.#changed();
    this.#send();
  }

  #fail(id: string, failure: number): void {
    this.#failed.set(id, { count: (this.#failed.get(id)?.count ?? 0) + 1, failure });
  }

  /** Holds `body` under its key for the page's life: the query is made with no time limit and no
   *  read of its own before the data is set, since `setQueryData` alone would make it with the
   *  default 5 minutes, after which a body no entry draws would go. */
  #keep(body: HeldHandoffBody): void {
    const query = handoffBodyQuery(body.id);
    this.#client.getQueryCache().build<HeldHandoffBody>(this.#client, query);
    this.#client.setQueryData(query.queryKey, body);
  }

  /** Each entry's state, made again where it changed, and every listener told. */
  #changed(): void {
    const ids = new Set([...this.#states.keys(), ...this.#standing.keys(), ...this.#failed.keys()]);
    for (const id of ids) {
      const standing = this.#standing.get(id);
      const failures = this.#failed.get(id)?.count ?? 0;
      const reading = standing === "out" || (standing === "queued" && failures > 0);
      const held = this.#states.get(id) ?? IDLE;
      if (held.failures === failures && held.reading === reading) continue;
      if (failures === 0 && !reading) this.#states.delete(id);
      else this.#states.set(id, { failures, reading });
    }
    for (const listener of [...this.#listeners]) listener();
  }
}
