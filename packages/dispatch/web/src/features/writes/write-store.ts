import { DISPATCH_LOGIN_CHANGED } from "@legion/contracts";
import {
  hashKey,
  type Query,
  type QueryClient,
  type QueryKey,
  useQueryClient,
} from "@tanstack/react-query";
import { useRef, useSyncExternalStore } from "react";

import {
  ApiError,
  api,
  type WriteBounds,
  type WriteMethod,
  type WriteRequest,
} from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { indexedDbFiles, type KeptFiles } from "./kept-files";
import {
  KEYED_SEND_ATTEMPT_MS,
  retriesUnderItsKey,
  SEND_DEADLINE_MS,
  SendStoppedError,
  WriteDiscardedError,
  writeUnderKey,
} from "./keyed-write";
import {
  KEPT_SEND_RESUME_MS,
  type KeptState,
  type KeptWrite,
  keptRefusal,
  keptRequest,
  type Presentation,
  refusalFrom,
  requestFrom,
  resumesOnItsOwn,
  type Shape,
  type WriteStorage,
  writeStorage,
} from "./write-storage";

/** Where a write stands: waiting its turn behind an earlier write to its target (`queued`), out
 *  within its deadline (`sending`), out past it (`late`, `stopping` once the reader pressed Stop
 *  trying), refused - by Dispatch, or stopped - and not yet retried or discarded, or made: Dispatch
 *  answered it, and its kind shows the answer until it lets it go (`WriteKind.keepsMade`). */
export type WriteState =
  | { readonly status: "queued" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly status: "refused"; readonly refusal: Error }
  | { readonly status: "made"; readonly answer: unknown };

/** A write apart from where it stands: its key, its kind and its kind's record (`detail`), the
 *  login of the person who pressed it (stamped at the press, never again), what it is called and
 *  where it shows, and the target its line is ordered on. */
export interface WriteBase<D = unknown> {
  readonly id: number;
  readonly key: string;
  readonly kind: string;
  readonly login: string | undefined;
  readonly label: string;
  readonly presentation: Presentation;
  /** Whether the surface that made it shows its refusal itself, so the store lets a refused one
   *  go: a form the reader is still in front of, holding their draft. */
  readonly inline: boolean;
  readonly target: string | undefined;
  readonly pressedAt: number;
  /** Its place in its target's line, in the order writes joined it; a Retry joins at the back. */
  readonly joined: number;
  /** Whether an attempt of it under its key has gone out. One that has not was never sent, so
   *  while it waits its turn it reads "Waiting to send" and is the reader's to discard. */
  readonly sent: boolean;
  /** How much of an upload's body its current attempt has sent, in percent; `undefined` for a
   *  write with no body going up. */
  readonly uploaded: number | undefined;
  /** Whether an attempt of it has ended without Dispatch's verdict - a connection that failed, an
   *  attempt gone silent, a 5xx - so it is being tried again, not still on its first try. */
  readonly retried: boolean;
  /** Whether its person's sign-out in another tab of this browser is out, which holds it - no
   *  attempt of it starts - until that tab says how it ended or stops saying it is out
   *  (`Writes.holdFor`). */
  readonly signingOutElsewhere: boolean;
  readonly detail: D;
}

/** One write the store holds, from its press until Dispatch answers it and its refusal is retried
 *  or discarded. */
export type Write<D = unknown> = WriteBase<D> & WriteState;

/** `write` apart from where it stands, for a new state to be put on. */
function baseOf<D>(write: Write<D>): WriteBase<D> {
  const { detail, id, inline, joined, key, kind, label, login, presentation, pressedAt } = write;
  const { retried, sent, signingOutElsewhere, target, uploaded } = write;
  return {
    detail,
    id,
    inline,
    joined,
    key,
    kind,
    label,
    login,
    presentation,
    pressedAt,
    retried,
    sent,
    signingOutElsewhere,
    target,
    uploaded,
  };
}

/** One attempt of a write, under the bounds the store gives it: its key, its login and its
 *  deadline. */
export type Attempt<T> = (bounds: WriteBounds) => Promise<T>;

/**
 * A kind of write: what its record is (`D`) and how it is kept (`K`), checked as a reload reads it
 * (`kept`), and what the store does for it beyond making it. Every kind is defined once
 * (`defineWriteKind`), so a reload finds any kept write's kind by its `name`.
 */
export interface WriteKind<D, K = D> {
  readonly name: string;
  readonly kept: Shape<K>;
  /** The record as it is kept; left out, the record itself. */
  readonly keep?: (detail: D) => K;
  /** The record a kept one names; left out, the kept one itself. */
  readonly restore?: (kept: K) => D;
  /** One attempt of a write of this kind a reload took up, from its record and the request it
   *  last went out as; left out, that request verbatim. `undefined` has nothing to make, and the
   *  write is let go. */
  readonly resume?: (detail: D, request: WriteRequest | null) => Attempt<unknown> | undefined;
  /** What a write of this kind that landed refreshes, whether or not a surface awaits it:
   *  `answer` is Dispatch's, `null` for a landing it had no answer to give for, and `awaited`
   *  says whether the surface that made it is still waiting for it. Left out, every query is read
   *  again for a write no surface awaits and for a landing without an answer. */
  readonly landed?: (
    client: QueryClient,
    write: Write<D>,
    answer: unknown,
    awaited: boolean
  ) => void;
  /** What a write of this kind undoes once it is refused, discarded or dropped. */
  readonly undone?: (client: QueryClient, write: Write<D>) => void;
  /** What a kind lets go of once whoever is signed in changes, or the person signs out: what it
   *  remembered of their answers, which the next person's writes must not build on. */
  readonly reset?: () => void;
  /** Whether a write of this kind Dispatch made and answered stays, `made`, until its surface lets
   *  it go (`letGo`); one made with no answer to give has nothing to show, and goes. */
  readonly keepsMade?: boolean;
  /** Told each time a write of this kind joins its line - its press, a Retry, a reload - with
   *  what settles once it lands or is refused, and whether it is past its deadline already. */
  readonly started?: (
    client: QueryClient,
    write: Write<D>,
    settled: Promise<unknown>,
    late: boolean
  ) => void;
}

// biome-ignore lint/suspicious/noExplicitAny: each write's record is read only by its own kind.
type AnyKind = WriteKind<any, any>;

const kinds = new Map<string, AnyKind>();

/** Defines a kind of write, once, so a reload finds it by name. */
export function defineWriteKind<D, K = D>(kind: WriteKind<D, K>): WriteKind<D, K> {
  kinds.set(kind.name, kind);
  return kind;
}

/** `write` as a write of `kind`, with its record typed, or `undefined` for another kind's. A
 *  write's record is only ever made by its own kind (`run`, `resume`). */
export function ofKind<D, K>(write: Write, kind: WriteKind<D, K>): Write<D> | undefined {
  return write.kind === kind.name ? (write as Write<D>) : undefined;
}

/** The kind of every write that has no record of its own: a status, a priority, an answer, a read
 *  mark. A reload makes it again as the request it last went out as, and a landing no surface
 *  awaits - or one without an answer - reads every query again, since the surface that made it
 *  may be gone. */
export const plainWrite = defineWriteKind<null>({
  kept: (value): value is null => value === null,
  name: "write",
});

/** The line a write is ordered on, or the reason it is on none: a write names the row it writes
 *  (`target`) or says, in its own value, why it names none (`unlined`). One of the two is
 *  required, so a write written later cannot leave the line out in silence, and `Writes.run`
 *  refuses one carrying neither (a page bug a `.js` caller could still reach). */
export type WriteLine =
  | {
      /** The row this write writes, as `<row type>:<row id>` - `issue:<KEY>`, `pin:<KEY>`,
       *  `comment:<id>`, `ask:<id>`, `project:<KEY>` for a write that adds a row to a project -
       *  or, for a read mark, `read:` and that row's own name. Writes to one target go out in
       *  press order: a later one waits while an earlier one is still being tried. A write of a
       *  kind that is made again from its request (no `resume`) is made from its `request` alone
       *  when it names a target. The sends that name no row (`broadcast`, `session:<id>`,
       *  `role:<role>`) carry their own names here. */
      readonly target: string;
      readonly unlined?: undefined;
    }
  | {
      readonly target?: undefined;
      /** Why this write is on no line, in the words of whoever wrote it: it names no row (a
       *  sign-out, a lookup, a send to a session), or the row it writes is one Dispatch names
       *  as it makes it. */
      readonly unlined: string;
    };

/**
 * The caches one change writes, and the one way a write writes them: a change names each cache
 * it shows itself on, so the store knows which answers that write must show it over again
 * (R7 of dispatch://INFRA-299). Every change is a function of the cache as it stands, never of
 * a value read before it: `change` is handed the data the cache holds now - the answer, once one
 * has landed - and answers with the data the person's change leaves. A cache holding nothing is
 * left holding nothing: there is no row there to change, and the answer that fills it is shown
 * over in its turn.
 */
export interface Shown {
  /** Shows the change on the cache at `key`. */
  on<T>(key: QueryKey, change: (data: T) => T): void;
  /** Shows it on every cache whose key starts with `key` - a project's issue lists, the Inbox's
   *  reads - including one read for the first time after the press, since the answer that fills
   *  it is shown over too. */
  under<T>(key: QueryKey, change: (data: T) => T): void;
}

/** A change a write shows: the same call at the press and over each answer that lands on a cache
 *  it wrote, until the write settles. A write that shows nothing names none. */
export type WriteChange = (shown: Shown) => void;

/** One write to make: its kind and record, what the indicator names it, where it shows, the line
 *  it is ordered on, and how it is made - from its request alone, or by an attempt of its
 *  own. */
export type WriteSpec<T, D, K> = WriteSpecBase<D, K> &
  WriteLine &
  (
    | {
        /** The request the write makes, as its press names it: every attempt of the write is
         *  `api.send(request)`, so the request a reload makes again and the one that goes out are
         *  one value, and nothing of the write runs until its turn comes. A write of a kind that
         *  is made again from its request (no `resume`) that names a `target` is made this way,
         *  and no other: `Writes.run` refuses one that names an attempt (`write`) instead. */
        readonly request: WriteRequest<T>;
        readonly write?: undefined;
      }
    | {
        readonly request?: undefined;
        /** One attempt of the write, which may make its request afresh each time (a pin's, from
         *  the state the last answer left); the request is kept as its first attempt goes out,
         *  and every attempt must make that one (`OneRequestPerWriteError`). */
        readonly write: Attempt<T>;
      }
  );

interface WriteSpecBase<D, K> {
  readonly kind: WriteKind<D, K>;
  readonly detail: D;
  readonly label: string;
  readonly presentation: Presentation;
  readonly inline?: boolean;
  /** The key of a write the surface makes again after it ended without Dispatch's verdict (the
   *  reader stopped it): that write's own, so Dispatch answers it with what the first did. Left
   *  out, the press gets a key of its own. */
  readonly key?: string;
  /** An upload's file, kept apart so a reload can make it again (`kept-files.ts`). */
  readonly file?: File;
  /** Whether the write ends the person's session - a sign-out - so a lapse of that session has
   *  done what it was for: the write lands, with no answer, rather than being held for the next
   *  sign-in. */
  readonly endsWithSession?: boolean;
  /** Why this write's answer carries something shown once and never again - the agent-token
   *  mint's secret - in the words of whoever wrote it, for a write of that kind. It is kept for
   *  its order while the page lives, as every write is, and a reload drops it rather than sending
   *  it again, as it drops a `DELETE` on a line (`droppedOnReload`): a repeat Dispatch took would
   *  make what its answer shows with no press left to take that answer. */
  readonly answerShownOnce?: string;
  /** The change this write shows, for a write that shows one: the store shows it at the press,
   *  before anything goes out, again over each cache write that lands on a cache it wrote while
   *  the write is live (`#showAgain`), and again as a Retry puts it back in its line. A write
   *  that shows nothing - a read mark, a create that only asks for a list again - leaves it
   *  out. */
  readonly shows?: WriteChange;
  /** Whether the store puts this write's change back however it leaves its line without
   *  landing - refused, discarded or taken - while its press still waits, as it does for a write
   *  no press waits for (`#putBack`): for a surface whose press never rolls back what it showed
   *  itself - an issue field's engine, a pin. Left out, a press that still waits rolls its own
   *  change back, and the store leaves that to the surface the press's refusal reaches. */
  readonly storePutsBack?: boolean;
}

/** What became of a write, told once the store's rows say so: it landed (with Dispatch's
 *  answer, `null` for none), was refused, the reader discarded it, or the store let it go
 *  before Dispatch answered (`taken`) - a sign-out, another person's sign-in, a refusal for
 *  another login, a quiet write past its time (`Writes.#take`). Every write that leaves its line
 *  tells one, so a wait on a write ends on its outcome alone. */
export type WriteOutcome =
  | { readonly kind: "landed"; readonly write: Write; readonly answer: unknown }
  | { readonly kind: "refused"; readonly write: Write; readonly refusal: Error }
  | { readonly kind: "discarded"; readonly write: Write }
  | { readonly kind: "taken"; readonly write: Write };

/** How a joined write starts once its turn comes: within its deadline, past it, or past it and
 *  stopped. */
type From = "late" | "sending" | "stopping";

/** A promise and what settles it: a press awaiting its write, or a kind awaiting its turn. */
interface Settlement {
  readonly promise: Promise<unknown>;
  readonly resolve: (value: unknown) => void;
  readonly reject: (reason: unknown) => void;
}

/** How a write's turn starts, and what settles once it has. */
interface Turn {
  readonly from: From;
  readonly settled: Settlement;
}

/** A keep of an upload's file still in flight: the key it is being kept under, and what answers
 *  once it is there or the browser has said it kept nothing. Nothing may name that key - no
 *  record of it, no attempt under it - until this answers. */
interface Keeping {
  readonly key: string;
  readonly settled: Promise<void>;
}

/** What the store holds of a write beside it: how to make it, the request its press named or it
 *  last went out as, what stops its trying, its file, the press that awaits it, and how its turn
 *  starts. */
interface Entry {
  write: Write;
  readonly attempt: Attempt<unknown>;
  request: WriteRequest | null;
  stop: AbortController;
  readonly file: File | undefined;
  /** The press waiting for it, until it settles; none for a Retry or a reload. */
  press: Settlement | undefined;
  /** How it starts once its turn comes; none while it is out. */
  turn: Turn | undefined;
  /** Ends a quiet write left unconfirmed five minutes after its press. */
  expiry: ReturnType<typeof setTimeout> | undefined;
  /** Whether its trying stopped for a lapsed session (`signedIn(undefined)`), so the attempt loop
   *  ending puts it back in its line to wait for the sign-in rather than refusing it. */
  held: boolean;
  /** A keep of its file still in flight, which its record under that key and its attempt wait
   *  for; none for a write with no file, and none once the keep has answered. While one is in
   *  flight the write is not the reader's to retry again, and letting the write go deletes the
   *  key being kept too. */
  keeping: Keeping | undefined;
  /** Whether this browser kept nothing of its file (`KeptFiles.put` answered `false`), so no
   *  record of it is ever written: a record naming a file that is not there is one a reload
   *  drops with nothing shown and nothing sent, where no record at all leaves the upload to
   *  land on this page, as it does in a browser with no IndexedDB. */
  unkept: boolean;
  readonly endsWithSession: boolean;
  /** Why its answer is shown once (`WriteSpec.answerShownOnce`), kept with it so a reload drops
   *  it; nothing for any other write. */
  readonly answerShownOnce: string | undefined;
  /** The change this write shows, and nothing for one that shows none. A write that shows its
   *  change when its turn comes rather than at the press names it then (`Writes.shows`). */
  shows: WriteChange | undefined;
  /** The caches that change wrote by name, as it named them (`Shown.on`), hashed as TanStack
   *  hashes a key. */
  readonly exact: Set<string>;
  /** The key every cache that change wrote by prefix starts with (`Shown.under`), so a cache an
   *  answer fills for the first time is shown over too. */
  readonly under: QueryKey[];
  /** Whether the store puts that change back as the write leaves its line while its press still
   *  waits (`WriteSpec.storePutsBack`). */
  readonly storePutsBack: boolean;
}

/** Whether `entry`'s change wrote `query`'s cache, by name or by prefix, so a cache write that
 *  lands there is one its change is shown over again. */
function wrote(entry: Entry, query: Query): boolean {
  return (
    entry.exact.has(query.queryHash) || entry.under.some((head) => startsWith(query.queryKey, head))
  );
}

/** Whether the store puts `entry`'s change back as it leaves its line (`#putBack`): a live write
 *  that shows one, with no press left to put it back - a Retry's, whose press the first refusal
 *  settled, the surface's own rollback running then - or whose press never puts it back
 *  (`WriteSpec.storePutsBack`). Any other write whose press still waits leaves that to the
 *  surface, which the press's refusal reaches; one that is not live showed nothing since it last
 *  settled. */
function putBackByStore(entry: Entry): boolean {
  return (
    entry.shows !== undefined &&
    liveWrite(entry.write) &&
    (entry.press === undefined || entry.storePutsBack)
  );
}

/** What the store does for a write's change as that write leaves its line without landing:
 *  puts back Dispatch's last answer with every other live write's change over it, then reads
 *  again (`put`); only reads again, the surface's own rollback putting the change back (`read`);
 *  or nothing, for a change no longer on the screen (`none`). */
type Back = "none" | "put" | "read";

/** Whether `write` may still reach Dispatch, which is while its change is shown again: waiting
 *  its turn, out, or late - `stopping` among them, since the attempt already out may still land.
 *  A refused write is not, nor a made one, and a discarded write is held no longer. The one rule
 *  every engine that reads the store's rows asks it by, and a view of a write - a broadcast's
 *  row in the send strip (`broadcastRows`) - is decided by it where the view is made. */
export function liveWrite(write: Write): boolean {
  return write.status === "queued" || write.status === "sending" || write.status === "late";
}

/** Whether `key` starts with `head`, compared as TanStack compares two keys. */
function startsWith(key: QueryKey, head: QueryKey): boolean {
  return key.length >= head.length && hashKey(key.slice(0, head.length)) === hashKey(head);
}

/** A `Shown` that runs each change it is handed for `query`'s cache alone, through `apply`, and
 *  passes over every change named for another cache. */
function onlyOn(query: Query, apply: (change: (data: unknown) => unknown) => void): Shown {
  return {
    on: <T>(key: QueryKey, change: (data: T) => T) => {
      if (hashKey(key) === query.queryHash) apply(change as (data: unknown) => unknown);
    },
    under: <T>(key: QueryKey, change: (data: T) => T) => {
      if (startsWith(query.queryKey, key)) apply(change as (data: unknown) => unknown);
    },
  };
}

const NO_WRITES: readonly Write[] = Object.freeze([]);

/** The login of the person `client`'s app is signed in as (`["whoami"]`), which every write is
 *  stamped with at its press; undefined while no one is signed in. */
export function signedInLogin(client: QueryClient): string | undefined {
  return client.getQueryData<AuthenticatedUser>(["whoami"])?.login;
}

/** Told of each store as it is made: nothing in the app, which holds no store but its own
 *  (`writesOf`), and the unit tests' preload, which stops every one after each test
 *  (`watchWriteStores`). */
let watcher: ((store: Writes) => void) | undefined;

/**
 * Every write the page makes, one per press, beside the app's `QueryClient` (`writesOf`), above
 * the router, so leaving the page that made one never drops it: a composer's send, a broadcast, a
 * pin, a read mark, a status, an answer, a form's save. Each is made under a key minted at the
 * press and tried under it until Dispatch decides it (`writeUnderKey`), whether or not the browser
 * says it is online; past `SEND_DEADLINE_MS` it is late, and Stop trying ends it. Writes to one
 * target go out in press order: a later one waits only while an earlier one is still being tried,
 * and a refused or stopped one no longer holds the line (its Retry joins at the back). Every write
 * is kept (`write-storage.ts`, an upload's file in `kept-files.ts`) from its press until Dispatch
 * answers, so a reload takes it up again (`resume`). A refusal stays, with Retry and Discard,
 * until the reader takes one - unless its surface shows refusals itself (`inline`), or it is never
 * shown (`quiet`), when it goes at once. A write belongs to the person who pressed it: a session
 * that lapses (whoami answers 401, or the write itself is answered 401) stops every write's trying
 * and starts none, but keeps them all, in memory and in storage, until someone signs in - the same
 * person's sign-in takes them up again, and anyone else's drops every write of another login
 * (`signedIn`). Dispatch refuses one that reaches it under another session first (`Dispatch-Login`,
 * `409 LOGIN_CHANGED`).
 */
export class Writes {
  /** The method of the request each of this page's send keys has already carried. A write's key
   *  is spent only where something has gone out under it (`keySpent`), and no entry can answer
   *  that for its key: a composer's Retry is a press of its own, under the refused send's key,
   *  in an entry that has sent nothing yet, and the entry it replaces is gone. So the store
   *  keeps the fact under the key it belongs to - set as each attempt goes out, written into
   *  every record it keeps (`KeptWrite.carried`) and read back from the records a reload takes
   *  up (`resume`), so a reload decides as this page does - and tells the retry loop
   *  (`writeUnderKey`'s `carried`) and every refusal a reload rebuilds (`refusalFrom`). A key is
   *  kept for as long as the page lives, since what went out under it never stops having gone
   *  out. */
  readonly #carried = new Map<string, WriteMethod>();
  readonly #client: QueryClient;
  readonly #kept: WriteStorage;
  readonly #files: KeptFiles;
  readonly #entries = new Map<number, Entry>();
  #rows: readonly Write[] = NO_WRITES;
  readonly #listeners = new Set<() => void>();
  readonly #outcomes = new Set<(outcome: WriteOutcome) => void>();
  #ids = 0;
  #joins = 0;
  #resumed = false;
  /** Whether the session of the person signed in has lapsed: no write starts until someone signs
   *  in again. */
  #lapsed = false;
  /** Who was signed in last; `undefined` before the first sign-in and once a session lapsed. */
  #login: string | undefined;
  /** The logins whose sign-out this page has out (`endsWithSession`), so none of their writes but
   *  the sign-out itself starts until Dispatch has answered it: nothing the person chose to
   *  discard goes out after they confirmed it. */
  readonly #signingOut = new Set<string>();
  /** The logins whose sign-out another tab of this browser has out (`holdFor`), held the same way
   *  until that tab says how it ended, goes, or stops saying it is out (`SignInChannel`), each
   *  under the mark its hold was taken with: a release names the hold it ends (`heldSignOut`), so
   *  one taken since - another tab's sign-out of the same person, begun while an earlier release
   *  was still deciding - is never lifted by it. */
  readonly #signingOutElsewhere = new Map<string, number>();
  /** The mark of the last hold taken, counted up for each (`holdFor`). */
  #heldMark = 0;
  /** The logins whose sign-out in another tab ended without a word - that tab went - while
   *  Dispatch still named them (`releaseUnsettled`), each with the time until which a lapse of
   *  theirs is that sign-out landing late rather than a lapse. */
  readonly #unsettledSignOuts = new Map<string, number>();
  /** Whether the store is writing a change into the caches now - at a press, at a Retry, or
   *  over a cache write that landed - so the cache writes it makes are not read as cache writes
   *  to show live writes over (`#whileShowing`, `#showAgain`). */
  #showing = false;
  /** The data of each cache a live write's change wrote, by its hash, as Dispatch last gave it:
   *  taken before the first live change is shown there (`#everywhere`), and from each cache write
   *  from outside the store that lands there, before the live changes are shown again over it
   *  (`#showAgain`), with the change of each write Dispatch made written into it (`#keepMade`).
   *  What a write leaving its line without landing is put back to (`#putBack`), so a read again
   *  that fails leaves the screen on that answer with every other live change over it, never on
   *  the change that left. A cache no live change wrote any more is let go (`#prune`). */
  readonly #confirmed = new Map<string, unknown>();

  constructor(
    client: QueryClient,
    kept: WriteStorage = writeStorage(),
    files: KeptFiles = indexedDbFiles()
  ) {
    this.#client = client;
    this.#kept = kept;
    this.#files = files;
    // Every cache write that lands, from wherever it came: an answer a fetch put there, or a
    // `setQueryData` of the page's own - a surface merging Dispatch's answer, a surface rolling
    // a refused change back, a kind's landing, the event feed's patch - each of which may replace
    // what a live write showed. The store's own writing is not one (`#whileShowing`).
    client.getQueryCache().subscribe((event) => {
      if (event.type !== "updated" || event.action.type !== "success") return;
      this.#showAgain(event.query);
    });
    watcher?.(this);
  }

  /** Every write held, in press order: one array until a write changes. */
  readonly rows = (): readonly Write[] => this.#rows;

  /** The id of the write made under `key`, as `run` has just left it in the rows: what a surface
   *  that pressed a write of its own reads to tell that press from another surface's, and to
   *  take it out again (`discard`). No write of that key is held under none. */
  readonly idOf = (key: string): number | undefined =>
    this.#rows.find((row) => row.key === key)?.id;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  };

  /** Tells `listener` what becomes of every write from now on. */
  readonly onOutcome = (listener: (outcome: WriteOutcome) => void): (() => void) => {
    this.#outcomes.add(listener);
    return () => this.#outcomes.delete(listener);
  };

  /**
   * Shows again, over the cache write `query` has just taken, the change of every live write
   * that wrote that cache, in the order the store holds them in (`joined`), so the last to be
   * pressed is the last to be seen (R7 of dispatch://INFRA-299). A cache write is any the store
   * did not make itself: an answer a fetch put there, and every `setQueryData` of the page's - a
   * surface merging an earlier write's answer, a surface's rollback of a refused change - since
   * each replaces what the live writes showed there. Each change is the same call its press made,
   * read against the cache as it stands, so what reappears is the change itself and never a value
   * computed before it. That cache write, before any change is shown over it, is what the store
   * puts back once a live write leaves without landing (`#confirmed`, `#putBack`).
   *
   * Live is `queued`, `sending` and `late`, `stopping` among them, because the attempt already
   * out may still land; a write that was refused, discarded or made is not, and a write that
   * shows nothing shows nothing again.
   *
   * Nothing recurs: every cache write the store makes runs inside `#whileShowing`, and TanStack
   * tells its cache's listeners of a write in the very call that makes it, so each of those
   * returns here at once. A cache write from outside the store is shown over once, and that
   * showing is the store's own.
   */
  #showAgain(query: Query): void {
    if (this.#showing) return;
    const live = this.#liveOn(query);
    if (live.length === 0) {
      this.#confirmed.delete(query.queryHash);
      return;
    }
    this.#confirmed.set(query.queryHash, query.state.data);
    this.#whileShowing(() => {
      for (const entry of live) entry.shows?.(this.#onto(query));
    });
  }

  /** Every live write whose change wrote `query`'s cache, in the order the store holds them in
   *  (`joined`). */
  #liveOn(query: Query): Entry[] {
    return [...this.#entries.values()]
      .filter((entry) => entry.shows !== undefined && liveWrite(entry.write) && wrote(entry, query))
      .sort((a, b) => a.write.joined - b.write.joined);
  }

  /** Keeps `data`, a cache's data before the first live change is shown on it, as what the store
   *  puts back there (`#confirmed`); a cache that holds it already keeps what it holds, which no
   *  live change is in, and a cache holding nothing has nothing to put back. */
  #confirm(hash: string, data: unknown): void {
    if (data !== undefined && !this.#confirmed.has(hash)) this.#confirmed.set(hash, data);
  }

  /** Runs `show`, which writes changes into the caches, as the store's own writing: no cache
   *  write it makes is shown over again (`#showAgain`). */
  #whileShowing(show: () => void): void {
    this.#showing = true;
    try {
      show();
    } finally {
      this.#showing = false;
    }
  }

  /** Runs `write`, a surface's own cache write of a change it shows before its write joins the
   *  store, as the store's own writing: no live write's change is shown over it again
   *  (`#showAgain`). For a change made from the cache as it stands - every live write's change
   *  already in it - in the press's tick, whose write waits behind a mutation scope and so names
   *  its `shows` only when its turn comes: a board drop placed on the board an earlier,
   *  unanswered drop left, which showing that drop again over would move. */
  writeAsStore(write: () => void): void {
    this.#whileShowing(write);
  }

  /** Shows `entry`'s change, if it has one, on every cache it names (`#everywhere`): at the
   *  press, as a surface names it at its turn (`shows`), and as a Retry puts it back in its
   *  line. */
  #showEverywhere(entry: Entry): void {
    const { shows } = entry;
    if (shows !== undefined) this.#whileShowing(() => shows(this.#everywhere(entry)));
  }

  /** Shows a change on every cache it names, recording each of them, so the store knows which
   *  cache writes that write must show it over again, and keeping each cache's data as it stood
   *  before the first live change was shown there (`#confirm`). A cache holding nothing is left
   *  holding nothing - there is no row there to change - and the answer that fills it is shown
   *  over in its turn (`wrote`). */
  #everywhere(entry: Entry): Shown {
    const client = this.#client;
    return {
      on: <T>(key: QueryKey, change: (data: T) => T) => {
        const hash = hashKey(key);
        entry.exact.add(hash);
        this.#confirm(hash, client.getQueryData(key));
        client.setQueryData<T>(key, (data) => (data === undefined ? undefined : change(data)));
      },
      under: <T>(key: QueryKey, change: (data: T) => T) => {
        const hash = hashKey(key);
        if (!entry.under.some((head) => hashKey(head) === hash)) entry.under.push(key);
        for (const query of client.getQueryCache().findAll({ queryKey: key })) {
          this.#confirm(query.queryHash, query.state.data);
        }
        client.setQueriesData<T>({ queryKey: key }, (data) =>
          data === undefined ? undefined : change(data)
        );
      },
    };
  }

  /** Shows a change again on `query`'s cache alone: the answer that landed is the only cache
   *  this showing is over, and the change's other caches are left as they stand, each to be
   *  shown over when an answer of its own lands. */
  #onto(query: Query): Shown {
    const client = this.#client;
    return onlyOn(query, (change) => {
      client.setQueryData(query.queryKey, (data: unknown) =>
        data === undefined ? undefined : change(data)
      );
    });
  }

  /** Writes the change of `entry`, which Dispatch has just made, into what the store keeps of
   *  every cache that change wrote (`#confirmed`): Dispatch's answer to it may never be written
   *  there - a kind leaves that to the newest write of its row still out - so until an answer
   *  replaces it, the change is part of what Dispatch last confirmed, and a later write that
   *  leaves without landing is put back to it rather than to the state before it. */
  #keepMade(entry: Entry): void {
    const { shows } = entry;
    if (shows === undefined) return;
    const confirmed = this.#confirmed;
    const caches = this.#client.getQueryCache().findAll({ predicate: (q) => wrote(entry, q) });
    for (const query of caches) {
      const hash = query.queryHash;
      if (!confirmed.has(hash)) continue;
      shows(onlyOn(query, (change) => confirmed.set(hash, change(confirmed.get(hash)))));
    }
  }

  /** Shows `change` for the write `id`, and holds it with that write: a surface that shows its
   *  change when the write's turn comes rather than at its press - a comment action, which waits
   *  for the actions pressed before it - names it here, and the store shows it again over each
   *  answer that lands on a cache it wrote until that write settles. A write the store no longer
   *  holds shows nothing. */
  shows(id: number, change: WriteChange): void {
    const entry = this.#entries.get(id);
    if (entry === undefined) return;
    entry.shows = change;
    this.#showEverywhere(entry);
  }

  /** Makes `spec`'s write, stamped with the person signed in now, and answers with Dispatch's 2xx,
   *  `null` for a write that landed with no answer to give, or rejects with its refusal - or
   *  `SendStoppedError` once the reader stops it. A write that names its request is made from it
   *  alone (`api.send`). A write that names neither the row it writes (`target`) nor a reason it
   *  names none (`unlined`) is a page bug, refused at once with nothing sent: the types ask every
   *  write for one of the two, so a write of a row cannot reach Dispatch outside a line without
   *  someone saying why. A write on a line of a kind made again from its request that names no
   *  request is a page bug too: waiting its turn, it could not be kept for a reload without
   *  running an attempt, and the request kept could differ from the one sent. A sign-out
   *  (`endsWithSession`) holds every other write of its person until Dispatch answers it, as
   *  another tab's does (`holdFor`): refused, they go on; landed, they wait for the page to let
   *  them go (`clear`). */
  run<T, D, K>(spec: WriteSpec<T, D, K>): Promise<T | null> {
    const { label, request } = spec;
    if (spec.target === undefined && spec.unlined === undefined) {
      return Promise.reject(
        new TypeError(
          `${label}: a write names the row it writes (WriteSpec.target) or why it names none (WriteSpec.unlined)`
        )
      );
    }
    if (spec.target !== undefined && request === undefined && spec.kind.resume === undefined) {
      return Promise.reject(
        new TypeError(`${label}: a write on a line is made from its request (WriteSpec.request)`)
      );
    }
    const press = Promise.withResolvers<unknown>();
    const login = signedInLogin(this.#client);
    const key = spec.key ?? newSendKey();
    if (spec.endsWithSession === true && login !== undefined) {
      this.#signingOut.add(login);
      this.#holdWritesOf(login);
    }
    const entry: Entry = {
      answerShownOnce: spec.answerShownOnce,
      attempt: request === undefined ? spec.write : (bounds) => api.send(request, bounds),
      endsWithSession: spec.endsWithSession ?? false,
      exact: new Set<string>(),
      expiry: undefined,
      file: spec.file,
      held: false,
      keeping: undefined,
      press,
      request: request ?? null,
      shows: spec.shows,
      stop: new AbortController(),
      storePutsBack: spec.storePutsBack ?? false,
      turn: undefined,
      under: [],
      unkept: false,
      write: {
        detail: spec.detail,
        id: this.#nextId(),
        inline: spec.inline ?? false,
        joined: this.#nextJoin(),
        key,
        kind: spec.kind.name,
        label: spec.label,
        login,
        presentation: spec.presentation,
        pressedAt: Date.now(),
        retried: false,
        sent: false,
        signingOutElsewhere: login !== undefined && this.#signingOutElsewhere.has(login),
        status: "queued",
        target: spec.target,
        uploaded: undefined,
      },
    };
    // The entry joins at once, whatever its file is doing: every lifecycle method works over
    // `#entries`, so a write that joined late would outlive a sign-out, a lapse or another
    // tab's sign-in and then go out under a login nobody is signed in as. What waits for the
    // file is the record of the write and its first attempt (`#keep`, `#start`), so nothing
    // ever names a key IndexedDB holds no file under.
    if (spec.file !== undefined && login !== undefined) {
      this.#keepFile(entry, key, login, spec.file, () => this.#keep(entry));
    }
    // The change is shown here, before anything goes out and in the task the press ran in, so
    // the person sees it at once; the store shows it again over each cache write that lands on
    // one of the caches it names, until this write settles (`#showAgain`).
    this.#showEverywhere(entry);
    this.#join(entry, "sending");
    return press.promise as Promise<T | null>;
  }

  /** Stops trying the late write `id` (Stop trying): no attempt is made after the one in flight,
   *  whose answer is still the write's, and it is then refused with `SendStoppedError`, which a
   *  Retry makes again under the same key. Says whether it stopped one. */
  stop(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "late" || entry.write.stopping) return false;
    entry.stop.abort();
    this.#update(entry, { ...baseOf(entry.write), status: "late", stopping: true });
    return true;
  }

  /** Makes the refused write `id` again, at the back of its target's line: under its own key when
   *  it ended without Dispatch's verdict, under a new one when Dispatch refused it
   *  (`retriesUnderItsKey`), an upload's kept file moving to that new key with it. A Retry that
   *  moves a file waits for it: the file is kept under the new key first, and the key, the
   *  record and the attempt move together in the tick that answers, so nothing names the new
   *  key before the file is under it. Until then the write stands refused under its old key
   *  with its old file, and a reload brings it back for the reader to retry again; a second
   *  press inside that window does nothing, since the first one is still moving the file. */
  retry(id: number): void {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused" || entry.keeping !== undefined) return;
    const old = entry.write.key;
    if (retriesUnderItsKey(entry.write.refusal)) {
      this.#retryUnder(entry, old, entry.write.sent);
      return;
    }
    const key = newSendKey();
    const { file } = entry;
    const { login } = entry.write;
    if (file !== undefined && login !== undefined) {
      // The put stamps the new row with the time it was made, which is the age `keepOnly`
      // measures, so the file's day and its key's run together. The old row goes once the new
      // one is there, and goes too where the browser kept nothing: the write's key is the new
      // one by then, so no reload would read the old row, and the upload is let go as one a
      // page with no IndexedDB at all is.
      this.#keepFile(entry, key, login, file, () => {
        this.#kept.forget(old);
        void this.#files.delete(old);
        this.#retryUnder(entry, key, false);
      });
      return;
    }
    this.#kept.forget(old);
    this.#retryUnder(entry, key, false);
  }

  /** Puts the refused `entry` back in its target's line under `key`, at the back, to be made
   *  again: `sent` says whether something has already gone out under that key. It is live again,
   *  so its change is shown again as it rejoins, as at its press, rather than left off the screen
   *  until it lands. */
  #retryUnder(entry: Entry, key: string, sent: boolean): void {
    entry.stop = new AbortController();
    entry.write = {
      ...baseOf(entry.write),
      joined: this.#nextJoin(),
      key,
      pressedAt: Date.now(),
      retried: false,
      sent,
      status: "queued",
      uploaded: undefined,
    };
    this.#showEverywhere(entry);
    this.#join(entry, "sending");
  }

  /** Keeps `file` under `key` for `entry`, records whether the browser kept it (`Entry.unkept`)
   *  and tells `landed`, in the tick that answers. While it is in flight nothing may name
   *  `key`: `#keep` writes no record of a write already under it and `#start` sends no attempt,
   *  and letting the write go deletes `key` too (`#forget`). A keep whose write has gone
   *  meanwhile tells nothing. A `put` that rejects rather than answering is read as a browser
   *  that kept nothing, so a rejection leaves no write stuck. A `put` that never settles does,
   *  differently at each caller: at the press the write stays `sending` with no attempt, so no
   *  deadline starts and neither Stop trying nor Discard takes it; at a Retry it stays
   *  `refused` under its old key, where another Retry does nothing while the keep stands but
   *  Discard still drops it and deletes both keys (`#forget`). Nothing in the page can do that
   *  - `indexedDbFiles` answers every path, and its open is never blocked, the version being
   *  fixed at 1 - and an implementation of `KeptFiles` that may hang is a page bug this does
   *  not cover. */
  #keepFile(entry: Entry, key: string, login: string, file: File, landed: () => void): void {
    const answered = (kept: boolean) => {
      if (entry.keeping !== keeping) return;
      entry.keeping = undefined;
      entry.unkept = !kept;
      if (this.#entries.get(entry.write.id) === entry) landed();
    };
    const keeping: Keeping = {
      key,
      settled: this.#files.put(key, login, file).then(answered, () => answered(false)),
    };
    entry.keeping = keeping;
  }

  /** Drops the write `id` the reader discards, and says whether there was one: a refused one, or
   *  one waiting its turn that was never sent, whose press is refused (`WriteDiscardedError`). A
   *  write that is out, or that went out before and waits to go again, is not the reader's to
   *  drop: it may have reached Dispatch, so it stays until Dispatch answers or the reader stops
   *  it. A waiting one still shows its change, press or not, and its press is refused with no
   *  failure to show - its surface may not even be on screen - so the store reads again every
   *  cache that change wrote (`#readAgain`); a live write's change, a pick that replaced it among
   *  them, is shown again over that answer (`#showAgain`). Where no press puts that change back -
   *  none waits, or its surface leaves that to the store (`WriteSpec.storePutsBack`) - the store
   *  first puts back Dispatch's last answer with every other live change over it (`#putBack`),
   *  so a read that fails leaves no discarded change on the screen. A refused one put its change
   *  back as it was refused, and has nothing left to read again. */
  discard(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry === undefined) return false;
    const waiting = entry.write.status === "queued" && !entry.write.sent;
    if (entry.write.status !== "refused" && !waiting) return false;
    // Read while its press still waits, which decides whether the store puts its change back.
    const back: Back =
      !waiting || entry.shows === undefined ? "none" : putBackByStore(entry) ? "put" : "read";
    const discarded = new WriteDiscardedError();
    entry.press?.reject(discarded);
    entry.press = undefined;
    entry.turn?.settled.reject(discarded);
    entry.turn = undefined;
    this.#remove(entry);
    this.#undo(entry, entry.write, back);
    this.#prune();
    this.#emit({ kind: "discarded", write: entry.write });
    this.#advance(entry.write.target);
    return true;
  }

  /** Lets the refused write `id` go because its surface makes it again as a press of its own - a
   *  composer's Retry, with what the reader wrote over the refusal - which takes its place. It is
   *  no Discard: the reader dropped nothing, so no outcome is told, and its kind already undid
   *  what it showed at the refusal. Says whether there was one. */
  supersede(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused") return false;
    this.#remove(entry);
    return true;
  }

  /** Lets the made write `id` go, as its surface does once it is gone. */
  letGo(id: number): void {
    const entry = this.#entries.get(id);
    if (entry?.write.status === "made") this.#remove(entry);
  }

  /** Writes `change` over the record of the refused write `id`, a write of `kind`: what the
   *  reader wrote over a refusal, which its Retry sends and a reload restores. */
  amend<D, K>(id: number, kind: WriteKind<D, K>, change: (detail: D) => D): void {
    const entry = this.#entries.get(id);
    const write = entry === undefined ? undefined : ofKind(entry.write, kind);
    if (entry === undefined || write?.status !== "refused") return;
    this.#update(entry, { ...write, detail: change(write.detail) });
  }

  /**
   * Who is signed in now: `login`, or no one (`undefined`: the session lapsed, whoami answered
   * 401). A lapse keeps every write, in memory and in storage, a refused one's draft and an
   * upload's file with it: each one out stops trying after the attempt in flight, and none starts,
   * until someone signs in. The same person's sign-in takes them up again where they stood, each in
   * its line; anyone else's drops every write not theirs, its trying stopped and its press refused.
   * A sign-out still out (`endsWithSession`) has done what it was for once the session lapses, and
   * lands; so has another tab's sign-out of the person whose session lapsed, while it still holds
   * their writes here (`holdFor`): the lapse is that sign-out landing, whether it reaches this
   * page before or after that tab's hold ends, so their writes go (`dropLogin`) rather than wait
   * for a sign-in they were discarded before. A lapse with no such hold keeps them. Each kind lets
   * go of what it remembered of the person before (`reset`) whenever who is signed in changes,
   * and so does every hold this page's sign-out put on writes; another tab's hold is that tab's
   * to end (`holdFor`), or this lapse's.
   */
  signedIn(login: string | undefined): void {
    const before = this.#login;
    if (login !== before) {
      this.#resetKinds();
      this.#signingOut.clear();
    }
    this.#login = login;
    if (login === undefined) {
      this.#lapsed = true;
      const unsettledUntil = before === undefined ? undefined : this.#unsettledSignOuts.get(before);
      this.#unsettledSignOuts.clear();
      const landed =
        before !== undefined &&
        (this.#signingOutElsewhere.has(before) ||
          (unsettledUntil !== undefined && Date.now() <= unsettledUntil));
      if (landed) this.dropLogin(before);
      for (const entry of [...this.#entries.values()]) this.#hold(entry);
      return;
    }
    // Dispatch names someone, so no sign-out of this browser's is still unaccounted for.
    this.#unsettledSignOuts.clear();
    this.#lapsed = false;
    this.#take([...this.#entries.values()].filter((entry) => entry.write.login !== login));
    void this.#files.keepOnly(login);
    this.#advanceEvery();
  }

  /** Lets every write go, out or refused, and everything kept with them, every kept file
   *  included: the person who made them signed out. */
  clear(): void {
    this.#take([...this.#entries.values()]);
    this.#signingOut.clear();
    this.#resetKinds();
    void this.#files.keepOnly(undefined);
    this.#kept.clear();
  }

  /** Drops every write `login` pressed, out or refused, its trying stopped and everything kept
   *  of it, its file included: that person signed out in another tab of this browser, which
   *  discards their writes in every tab (`app.tsx`). */
  dropLogin(login: string): void {
    this.#signingOut.delete(login);
    this.#signingOutElsewhere.delete(login);
    this.#drop([...this.#entries.values()].filter((entry) => entry.write.login === login));
  }

  /** Holds every write `login` pressed while their sign-out in another tab of this browser is
   *  out, as this page's own sign-out holds them: each one out stops trying after the attempt in
   *  flight, whose answer is still its own, and waits in its line, none starts, and each says it
   *  waits for that sign-out (`Write.signingOutElsewhere`), until that tab says how it ended.
   *  Signed out, they go (`dropLogin`); still signed in, or that tab gone or silent past its
   *  lease, they go on (`release`). */
  holdFor(login: string): void {
    this.#heldMark += 1;
    this.#signingOutElsewhere.set(login, this.#heldMark);
    this.#holdWritesOf(login);
    this.#markHeld();
  }

  /** The mark of the hold another tab's sign-out of `login` has on their writes here, or nothing
   *  while none has: what a release that decides over time names, so it ends the hold it was
   *  deciding about and no later one (`release`, `releaseUnsettled`). */
  heldSignOut(login: string): number | undefined {
    return this.#signingOutElsewhere.get(login);
  }

  /** Lets `login`'s writes go on where they stood: no other tab's sign-out holds them now. With
   *  `held`, only the hold of that mark is ended: a sign-out of theirs that began since holds
   *  them still, and nothing of theirs goes out while it is in flight. */
  release(login: string, held?: number): void {
    const mark = this.#signingOutElsewhere.get(login);
    if (mark === undefined || (held !== undefined && mark !== held)) return;
    this.#signingOutElsewhere.delete(login);
    this.#markHeld();
    this.#advanceEvery();
  }

  /** Lets `login`'s writes go on, as `release` does, for a sign-out in another tab that ended
   *  without a word - its tab went - while Dispatch still named the person. That sign-out may
   *  still land: the closed tab's request can reach Dispatch after this tab's own read did. So a
   *  lapse of theirs within `KEYED_SEND_ATTEMPT_MS`, the longest the page waits on a silent
   *  request, is that sign-out landing late, and their writes go (`signedIn`); a lapse after that
   *  is a lapse, and keeps them. A hold taken since `held` ends nothing and opens no window: that
   *  sign-out is live, and its own end will decide. */
  releaseUnsettled(login: string, held?: number): void {
    const mark = this.#signingOutElsewhere.get(login);
    if (mark === undefined || (held !== undefined && mark !== held)) return;
    this.#unsettledSignOuts.set(login, Date.now() + KEYED_SEND_ATTEMPT_MS);
    this.release(login, held);
  }

  /** Stops the trying of every write `login` pressed but a sign-out (`#hold`): a sign-out of
   *  theirs is out. */
  #holdWritesOf(login: string): void {
    for (const entry of this.#entries.values()) {
      if (entry.write.login === login && !entry.endsWithSession) this.#hold(entry);
    }
  }

  /** Marks every write whose person's sign-out in another tab holds it, and unmarks every other,
   *  telling the listeners once. */
  #markHeld(): void {
    let changed = false;
    for (const entry of this.#entries.values()) {
      const { login } = entry.write;
      const held = login !== undefined && this.#signingOutElsewhere.has(login);
      if (entry.write.signingOutElsewhere === held) continue;
      entry.write = { ...entry.write, signingOutElsewhere: held };
      changed = true;
    }
    if (changed) this.#publish();
  }

  /** Starts every write whose turn has come, in every line. */
  #advanceEvery(): void {
    const targets = new Set([...this.#entries.values()].map((entry) => entry.write.target));
    for (const target of targets) this.#advance(target);
  }

  /** The person follows the sign-in link of a lapsed session: the page goes and comes back to
   *  this tab, where every write it keeps resumes, so this navigation asks nothing on leaving
   *  (`WriteStorage.letLeave`). */
  leavingToSignIn(): void {
    this.#kept.letLeave();
  }

  /** Has every kind let go of what it remembered of the person signed in until now. */
  #resetKinds(): void {
    for (const kind of kinds.values()) kind.reset?.();
  }

  /**
   * Takes up, once, the writes a reload interrupted that `login`, the person signed in, pressed
   * (`write-storage.ts`), in press order, each in its target's line; another person's are dropped
   * unmade. One still out and pressed within `KEPT_SEND_RESUME_MS` is made again under its key, as
   * the request it last was, past its deadline already if it was then, and Dispatch answers the
   * repeat with what the first did; one the reader was stopping is asked once more and lands or
   * ends stopped. One pressed longer ago is not made again on its own: it shows refused
   * (`SendInterruptedError`), for a Retry under its key or Discard. A refusal shows again. A quiet
   * write is let go rather than shown. The app calls this once signed in.
   */
  resume(login: string): void {
    if (this.#resumed) return;
    this.#resumed = true;
    const now = Date.now();
    for (const kept of this.#kept.load(login)) {
      // What the key has carried is the entry's own record of it (`KeptWrite.carried`): the
      // store wrote it from this map as the write was kept, so a write re-pressed under the key
      // of the one it replaced - kept with nothing sent and no request of its own - still says
      // what its key carried before, and `null` is the answer that nothing went out under it.
      // An entry of the shape the release before this one wrote holds no such record, and
      // `WriteStorage.load` has already read it as that release read it, so nothing here asks
      // the question a second way.
      if (kept.carried !== null) this.#carried.set(kept.key, kept.carried);
      const kind = kinds.get(kept.kind);
      if (kind === undefined || !kind.kept(kept.detail)) {
        this.#kept.forget(kept.key);
        continue;
      }
      const detail = kind.restore === undefined ? kept.detail : kind.restore(kept.detail);
      if (kept.request !== null && "form" in kept.request) {
        void this.#files.get(kept.key).then((file) => this.#resume(kept, kind, detail, now, file));
      } else {
        this.#resume(kept, kind, detail, now, undefined);
      }
    }
    void this.#files.keepOnly(login);
  }

  #resume(kept: KeptWrite, kind: AnyKind, detail: unknown, now: number, file?: File): void {
    const request = kept.request === null ? null : (requestFrom(kept.request, file) ?? null);
    let attempt: Attempt<unknown> | undefined;
    if (!droppedOnReload(kept, request)) {
      if (kind.resume !== undefined) attempt = kind.resume(detail, request);
      else if (request !== null) attempt = (bounds) => api.send(request, bounds);
    }
    const out = kept.state.status !== "refused";
    const onItsOwn = out && resumesOnItsOwn(kept.pressedAt, now);
    if (attempt === undefined || (kept.presentation === "quiet" && !onItsOwn)) {
      this.#kept.forget(kept.key);
      if (file !== undefined) void this.#files.delete(kept.key);
      return;
    }
    // A reload shows nothing again: the page that showed this write's change is gone, and the
    // change was a call of the surface that pressed it, which no record holds. The write still
    // goes out, in its line, and Dispatch's answer is what the page then reads.
    const entry: Entry = {
      answerShownOnce: kept.answerShownOnce,
      attempt,
      endsWithSession: false,
      exact: new Set<string>(),
      expiry: undefined,
      file,
      held: false,
      keeping: undefined,
      press: undefined,
      request,
      shows: undefined,
      stop: new AbortController(),
      storePutsBack: false,
      turn: undefined,
      under: [],
      unkept: false,
      write: {
        detail,
        id: this.#nextId(),
        inline: false,
        joined: this.#nextJoin(),
        key: kept.key,
        kind: kept.kind,
        label: kept.label,
        login: kept.login,
        presentation: kept.presentation,
        pressedAt: kept.pressedAt,
        retried: false,
        sent: kept.sent,
        signingOutElsewhere: this.#signingOutElsewhere.has(kept.login),
        status: "queued",
        target: kept.target ?? undefined,
        uploaded: undefined,
      },
    };
    if (onItsOwn) {
      this.#join(entry, resumedFrom(kept.state));
      return;
    }
    const refusal = refusalFrom(
      kept.state.status === "refused" ? kept.state.refusal : { kind: "interrupted" },
      kept.key,
      // What the key has carried, read where the retry loop reads it (`#start`) rather than
      // worked out a second time here: `resume` put the entry's own record of it in this map.
      // Not what the file the write needs made of it either: a missing file leaves the write
      // nothing to make again, and its key has carried what it carried all the same.
      this.#carried.get(kept.key) ?? null,
      now
    );
    this.#entries.set(entry.write.id, entry);
    this.#update(entry, { ...entry.write, refusal, status: "refused" });
  }

  /** Puts `entry` in its target's line, to start at once if nothing ahead of it is still being
   *  tried - within its deadline (`sending`), as a write already past it (`late`), or as one past
   *  it the reader has stopped (`stopping`), which makes one attempt and no other. */
  #join(entry: Entry, from: From): void {
    this.#entries.set(entry.write.id, entry);
    const settled = Promise.withResolvers<unknown>();
    // The press settles its own surface; this one only tells the kind (`started`).
    settled.promise.catch(() => {});
    entry.turn = { from, settled };
    if (entry.write.presentation === "quiet" && entry.expiry === undefined) {
      const left = entry.write.pressedAt + KEPT_SEND_RESUME_MS - Date.now();
      entry.expiry = setTimeout(() => this.#drop([entry]), Math.max(0, left));
    }
    this.#update(entry, entry.write);
    kinds
      .get(entry.write.kind)
      ?.started?.(this.#client, entry.write, settled.promise, from !== "sending");
    this.#advance(entry.write.target);
  }

  /** Starts every write of `target` whose turn has come: in join order, each queued one that no
   *  earlier one is still being tried ahead of. A write with no target starts at once. None starts
   *  while the session is lapsed, and none of a person whose sign-out is out - this page's
   *  (`#signingOut`) or another tab's (`#signingOutElsewhere`) - but that sign-out: it waits,
   *  holding its place in its line. */
  #advance(target: string | undefined): void {
    if (this.#lapsed) return;
    const line = [...this.#entries.values()]
      .filter((entry) => entry.write.target === target)
      .sort((a, b) => a.write.joined - b.write.joined);
    for (const entry of line) {
      const { status } = entry.write;
      if (status === "queued") {
        if (this.#starts(entry)) this.#start(entry);
        if (target !== undefined) return;
      } else if (target !== undefined && (status === "sending" || status === "late")) {
        return;
      }
    }
  }

  /** Whether `entry` may go out now: no lapsed session, and no sign-out of its person's out -
   *  this page's (`#signingOut`) or another tab's (`#signingOutElsewhere`) - but its own. */
  #starts(entry: Entry): boolean {
    if (this.#lapsed) return false;
    const { login } = entry.write;
    return (
      entry.endsWithSession ||
      login === undefined ||
      !(this.#signingOut.has(login) || this.#signingOutElsewhere.has(login))
    );
  }

  /** Makes the write `entry` under its key until Dispatch decides it. An upload waits here for
   *  its file: the write reads `sending` from the press, so the indicator says so at once, and
   *  its attempt and its deadline start once the file is kept. A write whose session lapsed, or
   *  whose person's sign-out went out, while its file was being kept waits in its line instead,
   *  as a queued one does. */
  #start(entry: Entry): void {
    const { turn } = entry;
    if (turn === undefined) return;
    entry.turn = undefined;
    const { stop } = entry;
    if (turn.from === "stopping") stop.abort();
    this.#update(
      entry,
      turn.from === "sending"
        ? { ...baseOf(entry.write), status: "sending" }
        : { ...baseOf(entry.write), status: "late", stopping: turn.from === "stopping" }
    );
    const current = () => this.#entries.get(entry.write.id) === entry && entry.stop === stop;
    const { keeping } = entry;
    if (keeping === undefined) {
      this.#send(entry, turn, stop, current);
      return;
    }
    void keeping.settled.then(() => {
      if (!current()) {
        turn.settled.reject(new SendStoppedError());
        return;
      }
      if (!this.#starts(entry)) {
        entry.held = false;
        turn.settled.reject(new SendStoppedError());
        this.#rejoin(entry);
        return;
      }
      this.#send(entry, turn, stop, current);
    });
  }

  /** Tries `entry` under its key until Dispatch decides it, within the deadline this starts. */
  #send(entry: Entry, turn: Turn, stop: AbortController, current: () => boolean): void {
    const deadline =
      turn.from === "sending"
        ? setTimeout(() => {
            if (current() && entry.write.status === "sending") {
              this.#update(entry, { ...baseOf(entry.write), status: "late", stopping: false });
            }
          }, SEND_DEADLINE_MS)
        : undefined;
    writeUnderKey(entry.attempt, entry.write.key, {
      carried: this.#carried.get(entry.write.key) ?? null,
      login: entry.write.login,
      planned: entry.request ?? undefined,
      sent: (request) => {
        // The request is going out under this key now, whatever becomes of this entry.
        this.#carried.set(entry.write.key, request.method);
        if (!current()) return;
        entry.request = request;
        if (entry.write.sent) this.#keep(entry);
        else this.#update(entry, { ...entry.write, sent: true });
      },
      uploading: (loaded, total) => {
        if (!current() || total <= 0) return;
        const uploaded = Math.min(100, Math.floor((loaded * 100) / total));
        if (uploaded !== entry.write.uploaded) this.#show(entry, { ...entry.write, uploaded });
      },
      failed: () => {
        if (current()) this.#show(entry, { ...entry.write, retried: true, uploaded: undefined });
      },
      signal: stop.signal,
      // The person's session may have lapsed under the write: the page reads who is signed in
      // again, and a lapse holds every write until the sign-in that follows (`signedIn`). The
      // answer says whether the check, read afresh, still names the person who pressed it, which
      // makes a repeated 401 Dispatch's refusal (`writeUnderKey`).
      unauthorized: async () => {
        await this.#client.invalidateQueries({ queryKey: ["whoami"] });
        const state = this.#client.getQueryState<AuthenticatedUser>(["whoami"]);
        return (
          state?.status === "success" &&
          !state.isInvalidated &&
          state.data?.login === entry.write.login
        );
      },
    })
      .then(
        (answer) => {
          entry.held = false;
          if (current()) this.#landed(entry, answer);
          turn.settled.resolve(answer);
        },
        (error: unknown) => {
          const refusal = error instanceof Error ? error : new Error(String(error));
          const held = entry.held;
          entry.held = false;
          if (current()) {
            // A write a lapse held goes back in its line, to be made again under its key once
            // its person signs in; one whose key Dispatch has forgotten meanwhile has no key to
            // go under, so it is refused for the reader to Retry as a new write.
            if (held && retriesUnderItsKey(refusal)) this.#rejoin(entry);
            else this.#refused(entry, refusal);
          }
          turn.settled.reject(refusal);
        }
      )
      .finally(() => clearTimeout(deadline));
  }

  /** Stops `entry`'s trying for a lapsed session, or for a sign-out of its person's that is out:
   *  one out ends after the attempt in flight, whose answer is still its own, and goes back in
   *  its line (`#rejoin`) - an attempt still waiting before its request goes out gives up at
   *  once (`WriteBounds.signal`), so nothing of it is sent while it is held; a queued one waits
   *  where it is, and one the reader was stopping ends as they asked. A sign-out has landed
   *  instead: the session it was to end has ended, so its press settles as a landing with no
   *  answer, and its surface discards what the person chose to discard rather than leaving it
   *  held for a sign-in (`app.tsx`). */
  #hold(entry: Entry): void {
    if (entry.endsWithSession) {
      entry.stop.abort();
      const { press, turn } = entry;
      entry.press = undefined;
      entry.turn = undefined;
      this.#remove(entry);
      press?.resolve(null);
      turn?.settled.resolve(null);
      this.#emit({ answer: null, kind: "landed", write: entry.write });
      return;
    }
    const { write } = entry;
    if (write.status === "sending" || (write.status === "late" && !write.stopping)) {
      entry.held = true;
      entry.stop.abort();
    }
  }

  /** Puts `entry`, whose trying a lapse stopped, back in its line where it stood - past its
   *  deadline already if it was then - to go once its person signs in again. */
  #rejoin(entry: Entry): void {
    const late = entry.write.status === "late";
    entry.stop = new AbortController();
    entry.write = { ...baseOf(entry.write), status: "queued" };
    this.#join(entry, late ? "late" : "sending");
  }

  /** The write `entry` landed with `answer` (`null` for a landing with none): it leaves, or stays
   *  made for its kind to show, its change is kept as Dispatch's (`#keepMade`), and its kind
   *  refreshes what it wrote. */
  #landed(entry: Entry, answer: unknown): void {
    const kind = kinds.get(entry.write.kind);
    const awaited = entry.press !== undefined;
    entry.press?.resolve(answer);
    entry.press = undefined;
    if (kind?.keepsMade === true && answer !== null) {
      clearTimeout(entry.expiry);
      this.#forget(entry);
      this.#update(entry, { ...baseOf(entry.write), answer, status: "made" });
    } else {
      this.#remove(entry);
    }
    this.#keepMade(entry);
    this.#prune();
    if (kind?.landed !== undefined) kind.landed(this.#client, entry.write, answer, awaited);
    else if (!awaited || answer === null) void this.#client.invalidateQueries();
    this.#emit({ answer, kind: "landed", write: entry.write });
    this.#advance(entry.write.target);
  }

  /** The write `entry` was refused: it stays for Retry or Discard - unless its surface shows the
   *  refusal itself or it is never shown - and the next write of its target goes. One Dispatch
   *  refused because someone else is signed in goes with every write of its login, and the page
   *  reads who is signed in again. A refused sign-out leaves its person signed in, so every write
   *  of theirs it held goes on. A refused Retry has no press to put its change back, nor has a
   *  write whose surface leaves that to the store (`WriteSpec.storePutsBack`), so the store puts
   *  back Dispatch's last answer and reads its caches again (`#putBack`, `#readAgain`). */
  #refused(entry: Entry, refusal: Error): void {
    const back: Back = putBackByStore(entry) ? "put" : "none";
    entry.press?.reject(refusal);
    if (
      refusal instanceof ApiError &&
      refusal.status === 409 &&
      refusal.code === DISPATCH_LOGIN_CHANGED
    ) {
      // The press stays named until the take, which reads it to leave a change the press's
      // surface rolls back to that surface (`putBackByStore`); refusing it there again settles
      // nothing.
      const login = entry.write.login;
      this.#drop([...this.#entries.values()].filter((other) => other.write.login === login));
      void this.#client.invalidateQueries({ queryKey: ["whoami"] });
      return;
    }
    entry.press = undefined;
    const write: Write = { ...baseOf(entry.write), refusal, status: "refused" };
    if (entry.write.inline || entry.write.presentation === "quiet") this.#remove(entry);
    else this.#update(entry, write);
    this.#undo(entry, write, back);
    this.#prune();
    this.#emit({ kind: "refused", refusal, write });
    this.#advance(write.target);
    if (
      entry.endsWithSession &&
      write.login !== undefined &&
      this.#signingOut.delete(write.login)
    ) {
      this.#advanceEvery();
    }
  }

  /** Drops every one of `entries` wherever it stands (`#take`), and only then starts the writes
   *  whose turn that gives, each line once: a write waiting behind a dropped one is either
   *  dropped with it or still holds its place, and is never started on the way. */
  #drop(entries: readonly Entry[]): void {
    for (const target of this.#take(entries)) this.#advance(target);
  }

  /** Takes every one of `entries` out of the store first, then lets each go wherever it stood: its
   *  trying stops, its press is refused, nothing of it is kept, and its kind undoes what it showed
   *  of it - the store putting back, and reading again, the caches of a change no press puts
   *  back (`#undo`). Once the rows no longer hold them, it tells each one's outcome (`taken`).
   *  Starts nothing, and answers the targets whose lines it took writes from. */
  #take(entries: readonly Entry[]): Set<string | undefined> {
    const taken = entries.filter((entry) => this.#entries.get(entry.write.id) === entry);
    for (const entry of taken) this.#entries.delete(entry.write.id);
    for (const entry of taken) {
      const back: Back = putBackByStore(entry) ? "put" : "none";
      clearTimeout(entry.expiry);
      entry.stop.abort();
      entry.press?.reject(new SendStoppedError());
      entry.press = undefined;
      entry.turn?.settled.reject(new SendStoppedError());
      entry.turn = undefined;
      this.#forget(entry);
      this.#undo(entry, entry.write, back);
    }
    // Only once every one of them has put its change back: each puts back what Dispatch last
    // gave the caches it wrote, which the others taken with it need too.
    this.#prune();
    if (taken.length > 0) this.#publish();
    for (const entry of taken) this.#emit({ kind: "taken", write: entry.write });
    return new Set(taken.map((entry) => entry.write.target));
  }

  /** Undoes what `entry` showed as it leaves its line - refused, discarded or taken - as `write`:
   *  its kind's `undone`, then what `back` says for a change still on the screen: Dispatch's last
   *  answer put back under every other live change (`#putBack`) and a read of every cache that
   *  change wrote (`#readAgain`), or that read alone where the surface's own rollback puts the
   *  change back. The one place the three exits pair the three. */
  #undo(entry: Entry, write: Write, back: Back): void {
    kinds.get(write.kind)?.undone?.(this.#client, write);
    if (back === "put") this.#putBack(entry);
    if (back !== "none") this.#readAgain(entry);
  }

  /** Puts back, on every cache `entry`'s change wrote, the data Dispatch last gave it
   *  (`#confirmed`), with the change of every other live write that wrote it shown over it in the
   *  store's order: what the person sees of a write that leaves its line without landing until
   *  the read again answers, and what they go on seeing where it fails - offline, a 5xx - rather
   *  than a change nothing will send (R10 of dispatch://INFRA-299). A cache nothing was kept for
   *  held nothing when the change was shown, and is left to that read. */
  #putBack(entry: Entry): void {
    const client = this.#client;
    const caches = client.getQueryCache().findAll({ predicate: (query) => wrote(entry, query) });
    this.#whileShowing(() => {
      for (const query of caches) {
        if (!this.#confirmed.has(query.queryHash)) continue;
        client.setQueryData(query.queryKey, this.#confirmed.get(query.queryHash));
        for (const other of this.#liveOn(query)) {
          if (other !== entry) other.shows?.(this.#onto(query));
        }
      }
    });
  }

  /** Lets go of the data kept for every cache no live write's change wrote any more
   *  (`#confirmed`): the next change shown there takes it afresh. */
  #prune(): void {
    const cache = this.#client.getQueryCache();
    for (const hash of [...this.#confirmed.keys()]) {
      const query = cache.get(hash);
      if (query === undefined || this.#liveOn(query).length === 0) this.#confirmed.delete(hash);
    }
  }

  /** Reads again every cache `entry`'s change wrote, once that write has left its line with its
   *  change on the screen and nothing left to put it back (`#undo`). Every kind is covered here,
   *  since a plain write has no `undone` of its own, and Dispatch's answer replaces the change
   *  (R10 of dispatch://INFRA-299). */
  #readAgain(entry: Entry): void {
    void this.#client.invalidateQueries({ predicate: (query) => wrote(entry, query) });
  }

  #remove(entry: Entry): void {
    clearTimeout(entry.expiry);
    if (this.#entries.get(entry.write.id) !== entry) return;
    this.#entries.delete(entry.write.id);
    this.#forget(entry);
    this.#publish();
  }

  /** Lets go of what is kept of `entry`: its entry, an upload's file, and the file of a keep
   *  still in flight under another key, whose row `kept-files` drops rather than leaving it for
   *  `keepOnly` to reap a day later. */
  #forget(entry: Entry): void {
    this.#kept.forget(entry.write.key);
    if (entry.file !== undefined) void this.#files.delete(entry.write.key);
    const { keeping } = entry;
    if (keeping !== undefined && keeping.key !== entry.write.key) {
      void this.#files.delete(keeping.key);
    }
  }

  /** Puts `write` in the place of `entry`'s, keeps what a reload must take up, and tells every
   *  listener. */
  #update(entry: Entry, write: Write): void {
    entry.write = write;
    if (this.#entries.get(write.id) !== entry) return;
    this.#keep(entry);
    this.#publish();
  }

  /** Keeps the write `entry` as it stands, for the person who pressed it, with what its key has
   *  carried - `null` where nothing has gone out under it, written like any other answer: the
   *  page after a reload reads that fact back rather than working it out from the entry, which
   *  a write re-pressed under another's key cannot answer for (`#carried`). An upload whose
   *  file is not under its key - the keep is still in flight, or the browser kept nothing - is
   *  kept nowhere, since a record naming a file that is not there is one a reload drops with
   *  nothing shown and nothing sent; leaving the page is still confirmed while it is out. */
  #keep(entry: Entry): void {
    const { write } = entry;
    const state = keptState(write);
    if (write.login === undefined || state === undefined) return;
    if (entry.unkept || entry.keeping?.key === write.key) {
      this.#kept.keepNothing({ key: write.key, presentation: write.presentation, state });
      return;
    }
    const kind = kinds.get(write.kind);
    this.#kept.keep({
      answerShownOnce: entry.answerShownOnce,
      carried: this.#carried.get(write.key) ?? null,
      detail: kind?.keep === undefined ? write.detail : kind.keep(write.detail),
      key: write.key,
      kind: write.kind,
      label: write.label,
      login: write.login,
      presentation: write.presentation,
      pressedAt: write.pressedAt,
      request: entry.request === null ? null : keptRequest(entry.request),
      sent: write.sent,
      state,
      target: write.target ?? null,
    });
  }

  /** Puts `write` in the place of `entry`'s and tells every listener, keeping nothing: what changed
   *  - an upload's progress, a failed attempt - is not what a reload takes up. */
  #show(entry: Entry, write: Write): void {
    entry.write = write;
    if (this.#entries.get(write.id) === entry) this.#publish();
  }

  #publish(): void {
    const rows = [...this.#entries.values()]
      .map((entry) => entry.write)
      .sort((a, b) => a.id - b.id);
    this.#rows = rows.length === 0 ? NO_WRITES : rows;
    for (const listener of this.#listeners) listener();
  }

  #emit(outcome: WriteOutcome): void {
    for (const listener of this.#outcomes) listener(outcome);
  }

  #nextId(): number {
    this.#ids += 1;
    return this.#ids;
  }

  #nextJoin(): number {
    this.#joins += 1;
    return this.#joins;
  }
}

/** How a kept state resumes once its write is made again. */
function resumedFrom(state: KeptState): From {
  if (state.status === "late") return state.stopping ? "stopping" : "late";
  return "sending";
}

/** Whether a kept write a reload takes up is dropped rather than made again. Two kinds are, each
 *  kept for its order while the page lived and ending with that page. Every `DELETE` on a line,
 *  whichever one it is - the claim release, the session unsubscribe, the ask un-snooze, the
 *  agent-token revoke, the repository-mapping removal, the architecture-source removal: such a
 *  route carries no idempotency key (`goesUnderKey` sends one on a `POST` and a `PATCH` alone),
 *  so a repeat could undo what another person did in between. A `PUT` carries no key either and
 *  is made again, as the pin is, because it names the state it wants rather than a change to it.
 *  And every write whose answer carries something shown once and never again, which says so with
 *  its reason (`WriteSpec.answerShownOnce`) - today the agent-token mint: one that never reached
 *  Dispatch would be made by the repeat with no press left to take its answer, so its secret is
 *  shown on no page and the person holds a live token they must find and revoke. */
function droppedOnReload(kept: KeptWrite, request: WriteRequest | null): boolean {
  return (
    kept.answerShownOnce !== undefined || (kept.target !== null && request?.method === "DELETE")
  );
}

/** How `write`'s state is kept, or `undefined` for one that is made and so kept no longer. */
function keptState(write: Write): KeptState | undefined {
  switch (write.status) {
    case "queued":
    case "sending":
      return { status: write.status };
    case "late":
      return { status: "late", stopping: write.stopping };
    case "refused":
      return { refusal: keptRefusal(write.refusal), status: "refused" };
    case "made":
      return undefined;
  }
}

const stores = new WeakMap<QueryClient, Writes>();

/** The writes that live with `client`: one store per app, as there is one client. */
export function writesOf(client: QueryClient): Writes {
  let store = stores.get(client);
  if (store === undefined) {
    store = new Writes(client);
    stores.set(client, store);
  }
  return store;
}

/** Has `watch` told of every store made from now on (none when `undefined`), for the unit tests'
 *  preload, which stops each one's trying after every test (`Writes.clear`). A store outlives the
 *  test that made it, and a write it left out goes on retrying on its backoff once the test's spies
 *  are restored, so against the real client: into the tests after it, and into the next file's when
 *  `bun test` runs every file in one process, where the network guard fails whichever test is
 *  running when the retry reaches the network. */
export function watchWriteStores(watch: ((store: Writes) => void) | undefined): void {
  watcher = watch;
}

export function useWrites(): Writes {
  return writesOf(useQueryClient());
}

/**
 * What `select` picks out of every write the store holds, re-rendering only when that changes:
 * the pick is made again only when a write changes or `key` - what the pick is of, by name - does,
 * and handed back as the same value while it holds the same items (an array) or is the same value.
 */
export function useWriteSelection<R>(key: string, select: (rows: readonly Write[]) => R): R {
  const store = useWrites();
  const last = useRef<{
    readonly key: string;
    readonly rows: readonly Write[];
    readonly picked: R;
  }>(undefined);
  const pick = useRef(select);
  pick.current = select;
  return useSyncExternalStore(store.subscribe, () => {
    const rows = store.rows();
    const before = last.current;
    if (before?.rows === rows && before.key === key) return before.picked;
    const picked = pick.current(rows);
    const same =
      before !== undefined &&
      (Object.is(before.picked, picked) ||
        (Array.isArray(before.picked) &&
          Array.isArray(picked) &&
          before.picked.length === picked.length &&
          before.picked.every((item, index) => Object.is(item, picked[index]))));
    const kept = same ? before.picked : picked;
    last.current = { key, picked: kept, rows };
    return kept;
  });
}
