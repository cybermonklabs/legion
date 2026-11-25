import { DISPATCH_LOGIN_CHANGED } from "@legion/contracts/dispatch-api-constants";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useRef, useSyncExternalStore } from "react";

import {
  ApiError,
  api,
  goesUnderKey,
  isPageGoing,
  type WriteBounds,
  type WriteMethod,
  type WriteRequest,
} from "../../api/client";
import { askSignInAgain } from "../../api/query-client";
import type { AuthenticatedUser } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import type { UnsentChange } from "../shell/sign-in-channel";
import { isConflict } from "./conflicts";
import { indexedDbFiles, type KeptFiles } from "./kept-files";
import {
  endedWithoutVerdict,
  KEYED_SEND_ATTEMPT_MS,
  retriesUnderItsKey,
  SEND_DEADLINE_MS,
  SendStoppedError,
  WriteDiscardedError,
  writeUnderKey,
} from "./keyed-write";
import type { Shape } from "./tab-storage";
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
  type WriteStorage,
  writeStorage,
} from "./write-storage";

/** Where a write stands: waiting its turn behind an earlier write to its target (`queued`), out
 *  within its deadline (`sending`), out past it (`late`, `stopping` once the reader pressed Stop
 *  trying), refused - by Dispatch, or stopped - and not yet retried or discarded, or made: Dispatch
 *  answered it, and its kind shows the answer until it lets it go (`WriteKind.keepsMade`). A
 *  refused one is `claimed` while a surface mounted now settles its conflict with the reader
 *  (`WriteKind.claims`, `Writes.claim`), which the shell's line leaves to that surface. */
export type WriteState =
  | { readonly status: "queued" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly status: "refused"; readonly refusal: Error; readonly claimed: boolean }
  | { readonly status: "made"; readonly answer: unknown };

/** A write apart from where it stands: its key, its kind and its kind's record (`detail`), the
 *  login of the person who pressed it (stamped at the press, never again), whether it goes under
 *  its key, what it is called and where it shows, and the target its line is ordered on. */
export interface WriteBase<D = unknown> {
  readonly id: number;
  readonly key: string;
  readonly kind: string;
  readonly login: string | undefined;
  /** Whether the request it makes goes under its key (`goesUnderKey`): a `POST` or a `PATCH`,
   *  whose Retry after it ended without Dispatch's verdict asks again under that key, and not a
   *  `PUT` or a `DELETE`, which sends none, nor a write whose spec says it goes under none
   *  (`WriteSpec.keyed`). Read from what its spec names at the press (`WriteSpec`), never from
   *  what goes out later, since a write can end stopped before any attempt of it sends a
   *  request; kept with it (`KeptWrite.keyed`), so a reload reads it back. */
  readonly keyed: boolean;
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

/** Whether `write` may still reach Dispatch: waiting its turn, or out, within its deadline or
 *  past it and still being tried. A refused or discarded write is not. */
export function isLive(write: Write): boolean {
  return write.status === "queued" || write.status === "sending" || write.status === "late";
}

/** `write` apart from where it stands, for a new state to be put on. */
function baseOf<D>(write: Write<D>): WriteBase<D> {
  const { detail, id, inline, joined, key, keyed, kind, label, login, presentation } = write;
  const { pressedAt, retried, sent, signingOutElsewhere, target, uploaded } = write;
  return {
    detail,
    id,
    inline,
    joined,
    key,
    keyed,
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
  /** Whether a surface takes `refusal`, a conflict only the reader can settle (`isConflict`), to
   *  settle with them: a page that shows what the write was of, which keeps the reader's text
   *  beside it. A claimed write stays, kept for a reload as any refusal is, but `claimed`: the
   *  shell's line leaves it to that surface, which lets it go once the reader settles it there
   *  (`Writes.settle`) and hands it back to the line as it goes (`Writes.unclaim`). Its kind
   *  still undoes it, and its outcome is still told. A reload's kept one is asked again: no
   *  claim outlives the page. Left out, none is. */
  readonly claims?: (client: QueryClient, write: Write<D>, refusal: ApiError) => boolean;
  /** The reader's own text the write carried, which the shell's line offers to copy while its
   *  refusal is a conflict; left out, or `undefined`, it carried none. */
  readonly draft?: (write: Write<D>) => string | undefined;
  /** Whether `refused`, a write of this kind that went out and a conflict refused, carries what
   *  `unsent` does: a write of this kind claimed with none of it sent (`Writes.claimUnsent`). The
   *  store then lets `unsent` go and `refused` stands for it alone, so the shell's line never names
   *  the reader's text twice: claimed or not as such a refusal lands or a reload takes the writes
   *  up, and else once `unsent` is handed back to the line (`Writes.unclaim`). Left out, no write
   *  takes another's place. */
  readonly supersedes?: (refused: Write<D>, unsent: Write<D>) => boolean;
  /** Whether `carrier`, a write of this kind that landed or that such a landing carries, carries
   *  `earlier`, another write of this kind that stands refused: the newer write was made on the
   *  older one's change, so a Retry of the older would only go back over it. Which of the two is
   *  the newer is the kind's to say: the store's own order (`id`) follows the line, which a Retry
   *  and a reload reorder. The store makes `earlier` needless where it ended with no verdict of
   *  Dispatch's (`endedWithoutVerdict`) - stopped, its key spent, or refused after an attempt
   *  that went unanswered: it keeps it no longer, so a reload does not take it up, and its Retry
   *  lets it go and sends nothing (`Writes.retry`). Either way it asks the same of `earlier`, as
   *  `carrier`, so a run of writes, each made on the one before, goes with the landing that
   *  carries the last of them, through any a verdict refused. Left out, no landing makes another
   *  write needless. */
  readonly outdates?: (carrier: Write<D>, earlier: Write<D>) => boolean;
  /** What a kind lets go of once whoever is signed in changes, or the person signs out: what it
   *  remembered of their answers, which the next person's writes must not build on. The writes
   *  that change drops are undone first (`undone`), from what the kind still remembers. */
  readonly reset?: () => void;
  /** Told once of each store, with the client it lives with - as the store is made, or as the
   *  kind is defined where the store came first (`defineWriteKind`): what a kind keeps of that
   *  client for its whole life, from before the first write of the kind. */
  readonly opened?: (client: QueryClient, writes: Writes) => void;
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

/** Every store made so far, held weakly so a store its app let go of can still be collected: a
 *  kind defined after a store was made is opened on it here (`defineWriteKind`). */
const madeStores = new Set<WeakRef<Writes>>();

/** Defines a kind of write, once, so a reload finds it by name, and opens it (`opened`) on every
 *  store already made, as each store opens the kinds defined before it. The app's kinds come in a
 *  chunk of their own, after its store was made (`kinds.ts`), and a kind opened only on the
 *  stores made after it would miss the app's one store: the issue-field kind's watch, which keeps
 *  what Dispatch last confirmed from every read since the store was made, would begin only at the
 *  first pick (`issue-field-writes.ts`). */
export function defineWriteKind<D, K = D>(kind: WriteKind<D, K>): WriteKind<D, K> {
  const known = kinds.has(kind.name);
  kinds.set(kind.name, kind);
  if (known || kind.opened === undefined) return kind;
  for (const made of madeStores) {
    const store = made.deref();
    if (store === undefined) madeStores.delete(made);
    else store.open(kind);
  }
  return kind;
}

/** `write` as a write of `kind`, with its record typed, or `undefined` for another kind's. A
 *  write's record is only ever made by its own kind (`run`, `resume`). */
export function ofKind<D, K>(write: Write, kind: WriteKind<D, K>): Write<D> | undefined {
  return write.kind === kind.name ? (write as Write<D>) : undefined;
}

/** The reader's own text `write` carried, as its kind reads it (`WriteKind.draft`). */
export function writeDraft(write: Write): string | undefined {
  return kinds.get(write.kind)?.draft?.(write);
}

/** The kind of every write that has no record of its own: a status, a priority, an answer, a read
 *  mark. A reload makes it again as the request it last went out as, and a landing no surface
 *  awaits - or one without an answer - reads every query again, since the surface that made it
 *  may be gone. */
export const plainWrite = defineWriteKind<null>({
  kept: (value): value is null => value === null,
  name: "write",
});

/** One write to make: its kind and record, what the indicator names it, where it shows, the
 *  target it is ordered on, and how it is made - from its request alone, or by an attempt of its
 *  own. */
export type WriteSpec<T, D, K> = WriteSpecBase<D, K> &
  (
    | {
        /** The request the write makes, as its press names it: every attempt of the write is
         *  `api.send(request)`, so the request a reload makes again and the one that goes out are
         *  one value, and nothing of the write runs until its turn comes. A write of a kind that
         *  is made again from its request (no `resume`) that names a `target` is made this way,
         *  and no other: `Writes.run` refuses one that names an attempt (`write`) instead. */
        readonly request: WriteRequest<T>;
        readonly write?: undefined;
        /** The request's own (`request.method`). */
        readonly method?: undefined;
      }
    | {
        readonly request?: undefined;
        /** One attempt of the write, which may make its request afresh each time (a pin's, from
         *  the state the last answer left); the request is kept as its first attempt goes out,
         *  and every attempt must make that one (`OneRequestPerWriteError`). */
        readonly write: Attempt<T>;
        /** The method of the request every attempt of `write` makes, which no attempt says
         *  until it goes out: what tells whether the write goes under its key (`Write.keyed`). */
        readonly method: WriteMethod;
      }
  );

interface WriteSpecBase<D, K> {
  readonly kind: WriteKind<D, K>;
  readonly detail: D;
  readonly label: string;
  readonly presentation: Presentation;
  readonly inline?: boolean;
  /** Writes to one target go out in press order: a later one waits while an earlier one is still
   *  being tried. A write of a kind that is made again from its request (no `resume`) is made
   *  from its `request` alone when it names a target. */
  readonly target?: string;
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
  /** `false` for a write that goes under no key whatever its method: the sign-out, a `POST` to
   *  `/auth/logout`, which sits outside Dispatch's keyed layer (`api.logout`). */
  readonly keyed?: false;
}

/** The reader's own text a surface claims as a write a conflict refused, though no write of it
 *  went out (`Writes.claimUnsent`): its kind and record, what the shell's line names it, the
 *  target it is ordered on, the conflict that stands for it (`isConflict`), and the request a
 *  write of it would make, which a reload makes it again from as it makes any refused write. */
export interface UnsentClaimSpec<D, K>
  extends Pick<WriteSpecBase<D, K>, "detail" | "kind" | "label" | "target"> {
  readonly refusal: ApiError;
  readonly request: WriteRequest;
}

/** What became of a write: it landed (with Dispatch's answer, `null` for none), was refused, or
 *  the reader discarded its refusal. */
export type WriteOutcome =
  | { readonly kind: "landed"; readonly write: Write; readonly answer: unknown }
  | { readonly kind: "refused"; readonly write: Write; readonly refusal: Error }
  | { readonly kind: "discarded"; readonly write: Write };

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
  /** Whether a newer write that landed carries it, made needless while it stood refused
   *  (`WriteKind.outdates`): it is kept nowhere since, and its Retry lets it go and sends nothing.
   *  It stays for the reader to press Retry or Discard, and loses nothing of theirs: the newer
   *  write landed. */
  outdated: boolean;
  readonly endsWithSession: boolean;
}

/** A take-up of the writes a reload kept that waits for the kinds of write to be in (`resume`):
 *  the person signed in as it began, what asks for the kinds, and every write pressed meanwhile
 *  still waiting on it, in press order, none of them in its line until the take-up is done or
 *  the page goes (`#letInAsPageGoes`). Only that sending waits: a drop of the person's writes
 *  meanwhile forgets what the reload kept of theirs at once and ends the take-up (`#dropKept`). */
interface TakeUp {
  readonly login: string;
  readonly ask: () => void;
  readonly pressedMeanwhile: Entry[];
}

/** What ends each take-up still waiting for the kinds of write (`kindsRegistered`). */
const takeUpsWaiting = new Set<() => void>();

/**
 * Ends every take-up of what a reload kept still waiting for the kinds of write (`Writes.resume`),
 * since they are in: `kinds.ts` calls this once its imports have defined every kind, whichever
 * load evaluated it. So a take-up whose own ask failed with no reload to follow - the page was
 * being left, the reader chose to stay at the leave prompt that the writes the reload kept
 * (`WriteStorage.load`) or a write pressed meanwhile open, or the tab's recovery reloads are
 * spent - still ends once another load brings the kinds in, such as the writes host's
 * (`write-hosts.tsx`), and the kept writes go out, every write pressed meanwhile still waiting
 * behind them, with no reload by hand. A take-up that begins after the kinds are in ends through
 * its own ask, answered from what is in. A writes host calls this too whenever its own load of
 * the kinds answers (`prefetchWriteKinds`), which in the app ends nothing those two have not; it
 * ends a take-up only where that take-up's own ask is not the app's load and fails after the
 * kinds are in, as the loader a test hands `resume` does (`write-hosts.test.tsx`). Letting every
 * write pressed meanwhile into its line while the take-up waits would not do: a kept write taken
 * up after a newer press on its line would land after it.
 */
export function kindsRegistered(): void {
  for (const end of takeUpsWaiting) end();
  takeUpsWaiting.clear();
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
 * shown (`quiet`), when it goes at once. One a surface takes to settle with the reader
 * (`WriteKind.claims`) stays too, kept as any refusal is, and is that surface's until the reader
 * settles it there (`settle`) or the surface goes (`unclaim`); so does a text of the reader's a
 * surface claims though no write of it was sent (`claimUnsent`), until a refused write that went
 * out carries it too (`WriteKind.supersedes`). A refused write that ended with no verdict, and
 * that a newer write made on its change carries as that one lands (`WriteKind.outdates`), is
 * kept no longer, and its Retry sends nothing. A conflict (`isConflict`) refuses the same body
 * every time, so no Retry makes it again. A write belongs to the person who pressed it: a session
 * that lapses (whoami answers 401, or the write itself is answered 401) stops every write's trying
 * and starts none, but keeps them all, in memory and in storage, until someone signs in - the same
 * person's sign-in takes them up again, and anyone else's drops every write of another login
 * (`signedIn`). Dispatch refuses one that reaches it under another session first
 * (`Dispatch-Login`, `409 LOGIN_CHANGED`).
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
  readonly #signedOut = new Set<(login: string) => void>();
  #ids = 0;
  #joins = 0;
  #resumed = false;
  /** The take-up of the writes a reload kept while it waits for the kinds of write (`resume`);
   *  none before the app asks for one, none once it is done, and none once a drop of its person's
   *  writes left it nothing to take up (`#dropKept`). */
  #takingUp: TakeUp | undefined;
  /** What the reload kept that the store has not taken up yet, each write's record as the tab
   *  keeps it (`WriteStorage.peek`): every person's from the store's making, only the person's
   *  once their take-up begins (`resume`), and none once it is done. Each record names its
   *  person, its label, its kind's name and where it stood, so these writes are known with no
   *  kind of write in: a sign-out asks about them (`unsent`), a drop forgets them (`#dropKept`),
   *  and the sign-in page lists them (`KeptWrites.tsx`), a refused one with the Discard that
   *  drops it (`discardKept`), while the kinds are still on their way. No write the store holds
   *  is among them in the app: the take-up begins before any page can press one (`app.tsx`), and
   *  a write it takes up leaves them as it joins the store. A store with no app around it can
   *  hold a write pressed before its take-up began, which the take-up and `unsent` leave out as
   *  the store's own. Each change after the store's making goes through `#setNotTakenUp`, which
   *  tells every listener. */
  #notTakenUp: readonly KeptWrite[];
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

  constructor(
    client: QueryClient,
    kept: WriteStorage = writeStorage(),
    files: KeptFiles = indexedDbFiles()
  ) {
    this.#client = client;
    this.#kept = kept;
    this.#files = files;
    this.#notTakenUp = kept.peek();
    watcher?.(this);
    madeStores.add(new WeakRef(this));
    for (const kind of kinds.values()) this.open(kind);
  }

  /** Tells `kind` of this store and its client (`WriteKind.opened`): as the store is made, for
   *  every kind defined before it, and as a kind is defined after it (`defineWriteKind`). */
  open(kind: AnyKind): void {
    kind.opened?.(this.#client, this);
  }

  /** Every write held, in press order: one array until a write changes. */
  readonly rows = (): readonly Write[] => this.#rows;

  /** What the reload kept that the store has not taken up yet (`#notTakenUp`), one array until
   *  that changes: for the sign-in page's list, which shows these writes beside the store's own
   *  before the kinds of write are in (`KeptWrites.tsx`). */
  readonly notTakenUp = (): readonly KeptWrite[] => this.#notTakenUp;

  /** What of `login`'s a sign-out would discard here: every write of theirs they can see that
   *  Dispatch has not made - out, waiting its turn, or refused and holding their draft - by its
   *  key and name. That is every such write the store holds, and every one of theirs the reload
   *  kept that the store has not taken up (`#notTakenUp`): before the take-up begins (`resume`) -
   *  while the sign-in check the page loaded with is still out, or on a page that loaded signed
   *  out, which calls `resume` only once someone signs in (`app.tsx`) - and while the take-up
   *  waits for the kinds of write. Those are in the tab's storage alone until they are taken up,
   *  and a sign-out that lands meanwhile discards them with the rest (`signedOut`, `dropLogin`).
   *  A quiet write - a read mark - is never shown, and is no change of theirs to lose. What this
   *  tab's sign-out asks the person about, and what this tab answers another tab's (`app.tsx`). */
  readonly unsent = (login: string): UnsentChange[] => {
    // A kept write the store holds is this page's own, kept from its press: it is named once, as
    // the store's.
    const held = new Set(this.#rows.map((row) => row.key));
    return [
      ...this.#rows.filter((row) => row.login === login && row.status !== "made"),
      ...this.#notTakenUp.filter((write) => write.login === login && !held.has(write.key)),
    ]
      .filter((write) => write.presentation !== "quiet")
      .map(({ key, label }) => ({ key, label }));
  };

  /** The request the write `id` makes - the one its press named, or the one it last went out as
   *  - or undefined where the store holds no such write or it names none yet (one made by an
   *  attempt of its own that has not gone out). What a reader of a line asks of the writes on it:
   *  which change each one makes (`issue-field-writes.ts`). */
  readonly requestOf = (id: number): WriteRequest | undefined =>
    this.#entries.get(id)?.request ?? undefined;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  };

  /** Tells `listener` what becomes of every write from now on. */
  readonly onOutcome = (listener: (outcome: WriteOutcome) => void): (() => void) => {
    this.#outcomes.add(listener);
    return () => this.#outcomes.delete(listener);
  };

  /** Tells `listener` whom each sign-out that lands from now on signed out, once their writes are
   *  gone: this page's (`signedOut`), another tab's (`dropLogin`), and one a lapse lands
   *  (`signedIn`). What else the page holds of theirs goes on the same word: every draft they
   *  typed and did not send (`app.tsx`). A lapse that lands none tells nothing. */
  readonly onSignedOut = (listener: (login: string) => void): (() => void) => {
    this.#signedOut.add(listener);
    return () => this.#signedOut.delete(listener);
  };

  /** Makes `spec`'s write, stamped with the person signed in now, and answers with Dispatch's 2xx,
   *  `null` for a write that landed with no answer to give, or rejects with its refusal - or
   *  `SendStoppedError` once the reader stops it. A write that names its request is made from it
   *  alone (`api.send`). A write on a line of a kind made again from its request that names none
   *  is a page bug, refused at once: waiting its turn, it could not be kept for a reload without
   *  running an attempt, and the request kept could differ from the one sent. A sign-out
   *  (`endsWithSession`) holds every other write of its person until Dispatch answers it, as
   *  another tab's does (`holdFor`): refused, they go on; landed, they wait for the page to let
   *  them go (`signedOut`). A write pressed meanwhile - while the take-up of what a reload kept
   *  waits for the kinds of write (`resume`) - is kept and shown from its press, and joins its
   *  line once the take-up is done, so it never goes out ahead of a kept write on its line; a
   *  quiet one is let go five minutes after its press all the same (`#expire`). Two presses join
   *  their line at once instead: a sign-out (`endsWithSession`), since ending the session needs
   *  no kind of write and every other write of its person waits for its answer in any case, and
   *  a write pressed while the page is going (`isPageGoing`) whose line holds no kept write the
   *  take-up will send on its own (`#keptAhead`), so a tab closing then sends it with
   *  `keepalive`, as it sends any write pressed as the page goes; one whose line holds such a
   *  kept write stays out of its line, kept in the tab behind that write for the next load. */
  run<T, D, K>(spec: WriteSpec<T, D, K>): Promise<T | null> {
    const { request } = spec;
    if (spec.target !== undefined && request === undefined && spec.kind.resume === undefined) {
      return Promise.reject(
        new TypeError(
          `${spec.label}: a write on a line is made from its request (WriteSpec.request)`
        )
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
      attempt: request === undefined ? spec.write : (bounds) => api.send(request, bounds),
      endsWithSession: spec.endsWithSession ?? false,
      expiry: undefined,
      file: spec.file,
      held: false,
      keeping: undefined,
      outdated: false,
      press,
      request: request ?? null,
      stop: new AbortController(),
      turn: undefined,
      unkept: false,
      write: {
        detail: spec.detail,
        id: this.#nextId(),
        inline: spec.inline ?? false,
        joined: this.#nextJoin(),
        key,
        keyed: spec.keyed ?? goesUnderKey(request === undefined ? spec.method : request.method),
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
    // The entry is in the store at once, whatever its file is doing - in its line, or pressed
    // meanwhile while the take-up waits: every lifecycle method works over `#entries`, so a write
    // that came in late would outlive a sign-out, a lapse or another tab's sign-in and then go
    // out under a login nobody is signed in as. What waits for the file is the record of the
    // write and its first attempt (`#keep`, `#start`), so nothing ever names a key IndexedDB
    // holds no file under.
    if (spec.file !== undefined && login !== undefined) {
      this.#keepFile(entry, key, login, spec.file, () => this.#keep(entry));
    }
    const takingUp = this.#takingUp;
    // A write let into its line here needs nothing from the kinds chunk (`kinds.ts`). Its
    // attempt is `spec.write` or `api.send(request)`, and neither reads a kind. `#join`,
    // `#keep`, `#landed` and `#refused` read its kind through `kinds.get(...)?.`, which leaves a
    // missing kind to the store's defaults. And every caller passes `spec.kind` as the constant
    // `defineWriteKind` made as its own module loaded, so the kind is registered before that
    // chunk is in.
    if (
      takingUp === undefined ||
      entry.endsWithSession ||
      (isPageGoing() && !this.#keptAhead(entry.write.target, Date.now()))
    ) {
      this.#join(entry, "sending");
    } else {
      // Kept from its press, so a reload before the take-up is done takes it up as a kept write.
      // A reader now waits on the kinds, so the press asks for them too: a load that failed is
      // asked for again, under the chunk-failure recovery (`resume`).
      this.#entries.set(entry.write.id, entry);
      this.#update(entry, entry.write);
      this.#expire(entry);
      takingUp.pressedMeanwhile.push(entry);
      takingUp.ask();
    }
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
   *  press inside that window does nothing, since the first one is still moving the file. A
   *  conflict (`isConflict`) is not made again: Dispatch would refuse its body the same way. One a
   *  newer write that landed made needless (`Entry.outdated`) is let go instead, and nothing is
   *  sent. */
  retry(id: number): void {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused" || entry.keeping !== undefined) return;
    if (isConflict(entry.write.refusal)) return;
    if (entry.outdated) {
      this.#remove(entry);
      return;
    }
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
   *  again: `sent` says whether something has already gone out under that key. */
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
   *  it. A sign-out discarded while it waited its turn signed no one out, so every write of its
   *  person it held goes on (`#signOutEnded`). */
  discard(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry === undefined) return false;
    const waiting = entry.write.status === "queued" && !entry.write.sent;
    if (entry.write.status !== "refused" && !waiting) return false;
    const discarded = new WriteDiscardedError();
    entry.press?.reject(discarded);
    entry.press = undefined;
    entry.turn?.settled.reject(discarded);
    entry.turn = undefined;
    this.#remove(entry);
    kinds.get(entry.write.kind)?.undone?.(this.#client, entry.write);
    this.#emit({ kind: "discarded", write: entry.write });
    this.#advance(entry.write.target);
    this.#signOutEnded(entry);
    return true;
  }

  /** Drops the write the reload kept under `key` that the store has not taken up
   *  (`#notTakenUp`), and says whether there was one: a refused one, which the sign-in page
   *  offers the reader to discard from its record alone (`KeptWrites.tsx`). It goes from the
   *  tab's storage, from these records, and with its file for an upload, as `discard` drops a
   *  write the store holds. It needs no kind of write, so it is done at once, however long the
   *  kinds take; and this page showed nothing of it, so nothing is undone. A take-up still
   *  waiting holds only its person's records (`resume`), so one left with none ends: it has
   *  nothing to take up (`#endTakeUp`). */
  discardKept(key: string): boolean {
    const kept = this.#notTakenUp.find((write) => write.key === key);
    if (kept?.state.status !== "refused") return false;
    this.#forgetKept(kept);
    this.#setNotTakenUp(this.#notTakenUp.filter((write) => write !== kept));
    if (this.#notTakenUp.length === 0) this.#endTakeUp();
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

  /** Takes the refused write `id` for a surface that settles its conflict with the reader after
   *  the fact: a page drawn after the refusal arrived, which keeps the reader's text beside what
   *  the write was of, as one drawn then would have (`WriteKind.claims`). The write stays, kept as
   *  any refusal is, until the reader settles it there (`settle`) or the surface goes
   *  (`unclaim`). Says whether it took it: a conflict no surface holds. */
  claim(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused" || entry.write.claimed) return false;
    if (!isConflict(entry.write.refusal)) return false;
    this.#show(entry, { ...entry.write, claimed: true });
    return true;
  }

  /** Claims the reader's own text for a surface that kept it on its own, as another person's text
   *  arrived over it, where no write of it was refused: made as a write the conflict `spec.refusal`
   *  refused already, which the surface has claimed (`claim`). It is never sent, and no Retry
   *  sends it, since its refusal is a conflict (`isConflict`). From there it is any claimed
   *  write: kept for a reload, which takes it up refused, the surface's until the reader settles
   *  it there (`settle`) or the surface goes (`unclaim`), and the shell's line's in between, until
   *  a refused write that went out carries it too (`WriteKind.supersedes`). No outcome is told,
   *  since nothing was pressed. Answers its id. A refusal that is no conflict is a page bug,
   *  refused at once: a Retry of it would send a write the reader never pressed. */
  claimUnsent<D, K>(spec: UnsentClaimSpec<D, K>): number {
    const { refusal, request } = spec;
    if (!isConflict(refusal)) {
      throw new TypeError(
        `${spec.label}: an unsent claim stands for a conflict (UnsentClaimSpec.refusal)`
      );
    }
    const login = signedInLogin(this.#client);
    const entry: Entry = {
      attempt: (bounds) => api.send(request, bounds),
      endsWithSession: false,
      expiry: undefined,
      file: undefined,
      held: false,
      keeping: undefined,
      outdated: false,
      press: undefined,
      request,
      stop: new AbortController(),
      turn: undefined,
      unkept: false,
      write: {
        claimed: true,
        detail: spec.detail,
        id: this.#nextId(),
        inline: false,
        joined: this.#nextJoin(),
        key: newSendKey(),
        keyed: goesUnderKey(request.method),
        kind: spec.kind.name,
        label: spec.label,
        login,
        presentation: "indicator",
        pressedAt: Date.now(),
        refusal,
        retried: false,
        sent: false,
        signingOutElsewhere: login !== undefined && this.#signingOutElsewhere.has(login),
        status: "refused",
        target: spec.target,
        uploaded: undefined,
      },
    };
    this.#entries.set(entry.write.id, entry);
    this.#update(entry, entry.write);
    return entry.write.id;
  }

  /** Hands the claimed write `id` back to the shell's line, its draft to copy and Discard beside
   *  it: the surface that took it is gone, and the reader's text beside what the write was of
   *  with it. A surface drawn later that shows what it was of takes it again (`claim`). An unsent
   *  claim on the line that a refused write which went out carries too goes, whichever of the two
   *  came back first (`#supersedeUnsent`). */
  unclaim(id: number): void {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused" || !entry.write.claimed) return;
    this.#show(entry, { ...entry.write, claimed: false });
    this.#supersedeUnsent("unclaimed");
  }

  /** Lets the claimed write `id` go: the reader settled its conflict on the surface that took it,
   *  keeping the other person's text, or writing theirs again as a press of its own. No outcome
   *  is told: its refusal was told already, and the reader dropped nothing the surface did not
   *  show them. Says whether there was one. */
  settle(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused" || !entry.write.claimed) return false;
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
   * its line; anyone else's drops every write not theirs, its trying stopped and its press refused,
   * and at once every write the reload kept of the person a take-up still waiting began for,
   * which ends that take-up (`#dropKept`). A sign-out still out (`endsWithSession`) has done
   * what it was for once the session lapses, and lands; so has another tab's sign-out of the
   * person whose session lapsed, while it still holds their writes here (`holdFor`): the lapse is
   * that sign-out landing, whether it reaches this page before or after that tab's hold ends, so
   * their writes go (`dropLogin`) rather than wait for a sign-in they were discarded before. A
   * lapse with no such hold keeps them. Whenever who is signed in changes, every hold this page's
   * sign-out put on writes ends, and each kind lets go of what it remembered of the person before
   * (`reset`) - once the writes the change drops are gone, so each of those is undone (`undone`)
   * from what its kind still remembers of that person's answers. Another tab's hold is that tab's
   * to end (`holdFor`), or this lapse's.
   */
  signedIn(login: string | undefined): void {
    const before = this.#login;
    const changed = login !== before;
    if (changed) this.#signingOut.clear();
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
      if (changed) this.#resetKinds();
      for (const entry of [...this.#entries.values()]) this.#hold(entry);
      return;
    }
    // Dispatch names someone, so no sign-out of this browser's is still unaccounted for.
    this.#unsettledSignOuts.clear();
    this.#lapsed = false;
    const takingUp = this.#takingUp;
    this.#take([...this.#entries.values()].filter((entry) => entry.write.login !== login));
    if (changed) this.#resetKinds();
    // What the reload kept of the person the take-up began for is theirs alone, and anyone else's
    // sign-in drops it now, with no kind of write: the tab keeps none of it from here on, and the
    // take-up, left nothing to take up, ends, so whatever was pressed meanwhile joins its line.
    if (takingUp !== undefined && takingUp.login !== login) this.#dropKept(takingUp.login);
    void this.#files.keepOnly(login);
    this.#advanceEvery();
  }

  /** Lets every write go, out or refused, and everything kept with them, every kept file
   *  included, and every write the reload kept that the store has not taken up, which ends a
   *  take-up still waiting (`#endTakeUp`): what a sign-out of this page's that landed does
   *  (`signedOut`), and the unit-test preload after each test. */
  clear(): void {
    this.#take([...this.#entries.values()]);
    this.#signingOut.clear();
    this.#resetKinds();
    void this.#files.keepOnly(undefined);
    this.#kept.clear();
    this.#setNotTakenUp([]);
    this.#endTakeUp();
  }

  /** This page's sign-out of `login` landed: every write goes (`clear`), what the reload kept of
   *  theirs too, and every listener is told (`onSignedOut`). */
  signedOut(login: string): void {
    this.clear();
    for (const listener of this.#signedOut) listener(login);
  }

  /** Drops every write `login` pressed, out or refused, its trying stopped and everything kept
   *  of it, its file included, and every write the reload kept of theirs that the store has not
   *  taken up, at once, whether or not their take-up has begun (`#dropKept`), and tells every
   *  listener (`onSignedOut`): that person's sign-out landed in another tab of this browser,
   *  which discards their writes in every tab (`app.tsx`), or under a lapse here (`signedIn`).
   *  So a sign-in of theirs takes up and sends none of what the reload kept. */
  dropLogin(login: string): void {
    this.#signingOut.delete(login);
    this.#signingOutElsewhere.delete(login);
    this.#drop([...this.#entries.values()].filter((entry) => entry.write.login === login));
    this.#dropKept(login);
    for (const listener of this.#signedOut) listener(login);
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
   * (`SendInterruptedError`), for a Retry under its key or Discard. A refusal shows again; a
   * conflict a surface takes to settle with the reader (`WriteKind.claims`) comes back as that
   * surface's, claimed, and an unsent claim a refused write carries too goes
   * (`#supersedeUnsent`). A quiet write is let go rather than shown. The app calls this as the
   * person signs in, before it draws any page (`app.tsx`).
   *
   * Only sending a kept write waits for the kinds of write. A kept write is made again by its
   * kind, found by the name it was kept under, and one whose kind no module has registered is
   * dropped. The app's kinds are a chunk of their own (`kinds.ts`), so the app passes what loads
   * them (`loadKinds`), and the take-up waits until they are in, whichever load brought them
   * (`kindsRegistered`). Every write pressed meanwhile (`run`) but a sign-out stays out of its
   * line while the page stays, and joins it once the take-up is done, behind every write the
   * take-up put there, in press order, whatever order the page's effects pressed it in. When the
   * page goes (`pagehide`), each write waiting in `pressedMeanwhile` - and any pressed during
   * `pagehide` itself (`run`) - whose line holds no kept write this take-up will send on its own
   * (`#keptAhead`) joins its line at once and goes out as the page goes (`keepalive`,
   * `api/client.ts`), in press order, so a tab closing then loses none of them; one whose line
   * holds such a kept write stays out of its line, kept in the tab behind that write for the next
   * load (`WriteStorage.load` sorts by `pressedAt`). A `pageshow` changes nothing already
   * released, and the take-up goes on. A load that fails ends nothing here: the next press asks
   * again, and the failure of a load a reader waits on reloads the page while the tab's reload
   * budget lasts (`loadWriteKinds`), whose own take-up finds every write pressed meanwhile here
   * kept in the tab. Without `loadKinds` - every unit test's store - the take-up runs at once.
   *
   * Nothing else waits for the kinds. With nothing kept for the person there is nothing to take
   * up: the take-up ends at once and loads no kinds, and every press goes straight to its line.
   * What the reload kept is known from its records alone (`#notTakenUp`), each with its person,
   * its label and its kind's name: a sign-out asks about them (`unsent`), the sign-in page lists
   * them (`KeptWrites.tsx`), and leaving the page asks for them (`WriteStorage.load`). A drop of
   * the person's writes - another person's sign-in (`signedIn`), or their sign-out landing here
   * (`signedOut`), in another tab or under a lapse (`dropLogin`) - forgets them at once, unsent,
   * and ends the take-up, so the writes pressed meanwhile join their lines then (`#dropKept`,
   * `clear`); so does one while the take-up reads an upload's file (`#takeUp`).
   *
   * Another person's kept writes, and their files, go at once, before any wait: the take-up is
   * only ever of `login`'s. Chosen once the kinds are in instead, a person signed in from another
   * tab during the download would have what this tab kept of theirs from before the reload taken
   * up and sent, where the page's own sign-in had already dropped it - the rule that another
   * person's sign-in drops every write of another login (`signedIn`) - and a sweep of every file
   * but one person's, made then, would delete the files of the uploads someone signed in since
   * pressed meanwhile.
   */
  resume(login: string, loadKinds?: () => Promise<unknown>): void {
    if (this.#resumed) return;
    this.#resumed = true;
    // Read for what it drops - every entry of another person's - for leaving the page to ask for
    // the person's own, and for what the take-up takes up once the kinds are in (`#takeUp`).
    this.#setNotTakenUp(this.#kept.load(login));
    void this.#files.keepOnly(login);
    if (this.#notTakenUp.length === 0) return;
    if (loadKinds === undefined) {
      void this.#takeUp();
      return;
    }
    const registered = Promise.withResolvers<void>();
    takeUpsWaiting.add(registered.resolve);
    // One load out at a time: a press asks again only once the load before it has failed, so a
    // reader pressing while the kinds come down - or while a load waits offline for the network
    // (`importWhenOnline`) - starts no second load beside it.
    let asking = false;
    const takingUp: TakeUp = {
      ask: () => {
        if (asking) return;
        asking = true;
        void loadKinds().then(
          // Answered, the kinds are in: this load registered them, which has ended the take-up
          // already, or a load before this take-up began did, which told no take-up of this one.
          () => registered.resolve(),
          () => {
            asking = false;
          }
        );
      },
      login,
      pressedMeanwhile: [],
    };
    this.#takingUp = takingUp;
    if (typeof window !== "undefined") window.addEventListener("pagehide", this.#letInAsPageGoes);
    takingUp.ask();
    void registered.promise.then(async () => {
      takeUpsWaiting.delete(registered.resolve);
      // A take-up ended meanwhile has nothing left to take up: a drop of the person's writes, or
      // a Discard of the last of them, forgot every one (`#dropKept`, `clear`, `discardKept`). So
      // it does nothing as the kinds come. Run, it would let go an unsent claim a surface still
      // draws (`#supersedeUnsent`), at a moment only the chunk's download sets.
      if (this.#takingUp !== takingUp) return;
      // The person's writes are taken up whoever is signed in by now - they themselves, or no one
      // after a lapse, to wait for the sign-in that follows (`signedIn`). A drop while the take-up
      // reads an upload's file ends it there (`#dropKept`, `clear`).
      await this.#takeUp();
      if (this.#takingUp === takingUp) this.#endTakeUp();
    });
  }

  /** Ends the take-up still waiting for the kinds of write, if one is: it is done, or a drop of
   *  its person's writes left it nothing to take up (`#dropKept`, `clear`, `discardKept`). Every
   *  write pressed meanwhile still waiting that the store still holds joins its line now, in
   *  press order, as the newest in it. One ended before the kinds come stays in `takeUpsWaiting`
   *  until they do, and does nothing then (`resume`): it takes nothing up and lets no unsent
   *  claim go. Its list of the writes pressed meanwhile is emptied here, so such a take-up holds
   *  none of those writes until then, nor their requests and files. */
  #endTakeUp(): void {
    const takingUp = this.#takingUp;
    if (takingUp === undefined) return;
    this.#takingUp = undefined;
    if (typeof window !== "undefined") {
      window.removeEventListener("pagehide", this.#letInAsPageGoes);
    }
    this.#letIn(takingUp.pressedMeanwhile.splice(0));
  }

  /** Puts each of `entries` the store still holds into its line, in press order, as the newest in
   *  it: every write waiting when the take-up ends (`#endTakeUp`), and every unblocked one when
   *  the page goes (`#letInAsPageGoes`). */
  #letIn(entries: readonly Entry[]): void {
    for (const entry of entries) {
      if (this.#entries.get(entry.write.id) !== entry) continue;
      entry.write = { ...entry.write, joined: this.#nextJoin() };
      this.#join(entry, "sending");
    }
  }

  /** Whether `target`'s line holds a write the reload kept that the store has not taken up yet
   *  (`#notTakenUp`) and will send on its own once it does (`goesOnItsOwn`). A write with no
   *  target has no line. A kept write that is refused, or pressed longer than
   *  `KEPT_SEND_RESUME_MS` ago, is taken up refused (`#resume`) and holds no line (`#advance`), so
   *  it never lands on its own and the order rule has nothing to protect ahead of a newer press. */
  #keptAhead(target: string | undefined, now: number): boolean {
    if (target === undefined) return false;
    return this.#notTakenUp.some((kept) => kept.target === target && goesOnItsOwn(kept, now));
  }

  /** The page is going (`pagehide`) while the take-up still waits: every write in
   *  `pressedMeanwhile` whose line holds no kept write the take-up will send on its own joins its
   *  line now, in press order, so `api/client.ts` sends it with `keepalive` (`client.ts` listens
   *  as it loads, before `resume`, so `pageGoing` is already true). Any whose line holds such a
   *  kept write stays in `pressedMeanwhile`, kept in the tab behind that write for the next load.
   *  The take-up itself keeps waiting: a `pageshow` changes nothing already released. */
  readonly #letInAsPageGoes = (): void => {
    const takingUp = this.#takingUp;
    if (takingUp === undefined) return;
    const now = Date.now();
    const released: Entry[] = [];
    const waiting: Entry[] = [];
    for (const entry of takingUp.pressedMeanwhile) {
      if (this.#keptAhead(entry.write.target, now)) waiting.push(entry);
      else released.push(entry);
    }
    takingUp.pressedMeanwhile.splice(0, takingUp.pressedMeanwhile.length, ...waiting);
    this.#letIn(released);
  };

  /** The take-up `resume` describes, with every kind in, of what the reload kept that the store
   *  has not taken up (`#notTakenUp`): answers once each write it takes up has joined its line,
   *  an upload once its file has been read. */
  #takeUp(): Promise<unknown> {
    const now = Date.now();
    // A write pressed in this page before this take-up began is kept in the tab from its press,
    // so the reload's records read as the take-up began hold it too: it is already in the store,
    // in its line, and is not taken up beside itself.
    const held = new Set([...this.#entries.values()].map((entry) => entry.write.key));
    const files: Promise<void>[] = [];
    // The uploads whose files are still being read: not taken up yet, so a drop meanwhile
    // forgets them, and the sign-in page lists them still.
    const reading: KeptWrite[] = [];
    for (const kept of this.#notTakenUp) {
      if (held.has(kept.key)) continue;
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
        reading.push(kept);
        files.push(
          this.#files.get(kept.key).then((file) => {
            // A drop of the person's writes while the file was read - another person's sign-in,
            // or their sign-out landing here or in another tab - forgot this one already
            // (`#dropKept`, `clear`): it is not taken up.
            if (!this.#notTakenUp.includes(kept)) return;
            this.#setNotTakenUp(this.#notTakenUp.filter((write) => write !== kept));
            this.#resume(kept, kind, detail, now, file);
          })
        );
      } else {
        this.#resume(kept, kind, detail, now, undefined);
      }
    }
    // The kinds are in here, so an unsent claim that a write taken up carries too goes now
    // (`#supersedeUnsent`).
    this.#supersedeUnsent("all");
    this.#setNotTakenUp(reading);
    // Settled rather than all: a write pressed meanwhile goes on whatever a read made.
    return Promise.allSettled(files);
  }

  /** Forgets every write the reload kept of `login` that the store has not taken up, unsent, its
   *  file with it, and ends a take-up of theirs still waiting, which has nothing left to take up
   *  (`#endTakeUp`): their sign-out landed in another tab or under a lapse (`dropLogin`), or
   *  another person signed in (`signedIn`). It needs no kind of write, so it is done at once,
   *  however long the kinds take: the tab keeps none of those writes from then on, and leaving
   *  the page asks for them no more. Nothing else of the tab's goes. A write the store holds is
   *  no write of the reload's left to take up - one taken up already, or this page's own, kept
   *  in the tab from its press - and goes on as its press asked; and a sweep of every file but
   *  `login`'s would delete the files of the uploads someone signed in since pressed meanwhile. */
  #dropKept(login: string): void {
    this.#setNotTakenUp(
      this.#notTakenUp.filter((kept) => {
        if (kept.login !== login) return true;
        this.#forgetKept(kept);
        return false;
      })
    );
    if (this.#takingUp?.login === login) this.#endTakeUp();
  }

  /** Lets go of what the tab keeps of `kept`, a write the reload kept: its entry, and an
   *  upload's file. */
  #forgetKept(kept: KeptWrite): void {
    this.#kept.forget(kept.key);
    if (kept.request !== null && "form" in kept.request) void this.#files.delete(kept.key);
  }

  #resume(kept: KeptWrite, kind: AnyKind, detail: unknown, now: number, file?: File): void {
    const request = kept.request === null ? null : (requestFrom(kept.request, file) ?? null);
    let attempt: Attempt<unknown> | undefined;
    if (kind.resume !== undefined) attempt = kind.resume(detail, request);
    else if (request !== null) attempt = (bounds) => api.send(request, bounds);
    const onItsOwn = goesOnItsOwn(kept, now);
    if (attempt === undefined || (kept.presentation === "quiet" && !onItsOwn)) {
      this.#kept.forget(kept.key);
      if (file !== undefined) void this.#files.delete(kept.key);
      return;
    }
    const entry: Entry = {
      attempt,
      endsWithSession: false,
      expiry: undefined,
      file,
      held: false,
      keeping: undefined,
      outdated: false,
      press: undefined,
      request,
      stop: new AbortController(),
      turn: undefined,
      unkept: false,
      write: {
        detail,
        id: this.#nextId(),
        inline: false,
        joined: this.#nextJoin(),
        key: kept.key,
        keyed: kept.keyed,
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
    const write: Write = { ...entry.write, claimed: false, refusal, status: "refused" };
    this.#entries.set(entry.write.id, entry);
    this.#update(entry, { ...write, claimed: this.#claimed(write, refusal) });
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
    this.#expire(entry);
    this.#update(entry, entry.write);
    kinds
      .get(entry.write.kind)
      ?.started?.(this.#client, entry.write, settled.promise, from !== "sending");
    this.#advance(entry.write.target);
  }

  /** Lets the quiet `entry` go once five minutes have passed since its press without Dispatch
   *  confirming it, as a reload lets one that old go unmade (`#resume`): armed once, as it joins
   *  its line, or at its press while the take-up waits (`run`), so a take-up that ends later than
   *  that sends nothing of it. */
  #expire(entry: Entry): void {
    if (entry.write.presentation !== "quiet" || entry.expiry !== undefined) return;
    const left = entry.write.pressedAt + KEPT_SEND_RESUME_MS - Date.now();
    entry.expiry = setTimeout(() => this.#drop([entry]), Math.max(0, left));
  }

  /** Starts every write of `target` whose turn has come: in join order, each queued one that no
   *  earlier one is still being tried ahead of. A write with no target starts at once. None starts
   *  while the session is lapsed, and none of a person whose sign-out is out - this page's
   *  (`#signingOut`) or another tab's (`#signingOutElsewhere`) - but that sign-out: it waits,
   *  holding its place in its line. A write still waiting in `pressedMeanwhile` while the take-up
   *  waits is in no line yet (`resume`, `#letInAsPageGoes`). */
  #advance(target: string | undefined): void {
    if (this.#lapsed) return;
    const pressedMeanwhile = this.#takingUp?.pressedMeanwhile;
    const line = [...this.#entries.values()]
      .filter((entry) => entry.write.target === target && !pressedMeanwhile?.includes(entry))
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
      // makes a repeated 401 Dispatch's refusal (`writeUnderKey`). The answer read is the newest
      // check's, once it is in (`askSignInAgain`): another ask made meanwhile starts a check of
      // its own, and a check paused offline has not answered yet.
      unauthorized: async () => {
        await askSignInAgain(this.#client);
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

  /** Stops `entry`'s trying for a lapsed session: one out ends after the attempt in flight, whose
   *  answer is still its own, and goes back in its line (`#rejoin`); a queued one waits where it
   *  is, and one the reader was stopping ends as they asked. A sign-out has landed instead: the
   *  session it was to end has ended, so its press settles as a landing with no answer, and its
   *  surface discards what the person chose to discard rather than leaving it held for a sign-in
   *  (`app.tsx`). */
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
   *  made for its kind to show, its kind refreshes what it wrote, and each refused write it
   *  carries that ended with no verdict is made needless and kept no longer (`#outdate`). The
   *  landing is told before the store reads anything again for it, so every read it starts was
   *  sent after it: a listener that orders Dispatch's answers by when each read went out
   *  (`issue-field-writes.ts`) counts those reads as newer than the landing. */
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
    this.#outdate(entry.write);
    if (kind?.landed !== undefined) kind.landed(this.#client, entry.write, answer, awaited);
    this.#emit({ answer, kind: "landed", write: entry.write });
    if (kind?.landed === undefined && (!awaited || answer === null)) {
      void this.#client.invalidateQueries();
    }
    this.#advance(entry.write.target);
  }

  /** The write `entry` was refused: it stays for Retry or Discard - unless its surface shows the
   *  refusal itself or it is never shown, when it goes, and claimed where a surface takes it to
   *  settle with the reader (`#claimed`) - and the next write of its target goes. A conflict that
   *  stays takes the place of each unsent claim it carries too (`#supersedeUnsent`). One Dispatch
   *  refused because someone else is signed in goes with every write of its login, and the page
   *  reads who is signed in again. A refused sign-out leaves its person signed in, so every write
   *  of theirs it held goes on (`#signOutEnded`). */
  #refused(entry: Entry, refusal: Error): void {
    entry.press?.reject(refusal);
    entry.press = undefined;
    if (
      refusal instanceof ApiError &&
      refusal.status === 409 &&
      refusal.code === DISPATCH_LOGIN_CHANGED
    ) {
      const login = entry.write.login;
      this.#drop([...this.#entries.values()].filter((other) => other.write.login === login));
      void askSignInAgain(this.#client);
      return;
    }
    const stays = !entry.write.inline && entry.write.presentation !== "quiet";
    const refused: Write = { ...baseOf(entry.write), claimed: false, refusal, status: "refused" };
    const write: Write = stays ? { ...refused, claimed: this.#claimed(refused, refusal) } : refused;
    if (stays) this.#update(entry, write);
    else this.#remove(entry);
    if (stays && isConflict(refusal)) this.#supersedeUnsent("all");
    kinds.get(write.kind)?.undone?.(this.#client, write);
    this.#emit({ kind: "refused", refusal, write });
    this.#advance(write.target);
    this.#signOutEnded(entry);
  }

  /** A sign-out of this page's that ended with its person still signed in - refused, or
   *  discarded while it waited its turn - lets every write of theirs it held go on: none would
   *  start again until a reload or someone else's sign-in otherwise. */
  #signOutEnded({ endsWithSession, write }: Entry): void {
    if (endsWithSession && write.login !== undefined && this.#signingOut.delete(write.login)) {
      this.#advanceEvery();
    }
  }

  /** Whether a surface takes `write`'s refusal to settle with the reader: a conflict
   *  (`isConflict`) its kind's surface claims (`WriteKind.claims`). */
  #claimed(write: Write, refusal: Error): boolean {
    if (!isConflict(refusal)) return false;
    return kinds.get(write.kind)?.claims?.(this.#client, write, refusal) === true;
  }

  /** Lets go of each write claimed with none of it sent (`claimUnsent`) that a refused write which
   *  went out carries too (`WriteKind.supersedes`): the refused write stands for the reader's text
   *  alone, so the shell's line never names it twice. `all` lets such a claim go whether a surface
   *  holds it still or not, at two moments only, in whatever order the two writes came: as a
   *  conflict that stays lands, and as a reload's take-up takes every write up (`#takeUp`), which
   *  a take-up ended before the kinds came never does (`resume`). `unclaimed` lets go only one on
   *  the shell's line, as a surface hands a write back (`unclaim`): a surface that still draws its
   *  claim keeps it until it goes too. */
  #supersedeUnsent(which: "all" | "unclaimed"): void {
    const conflicts = [...this.#entries.values()].filter(
      ({ write }) => write.status === "refused" && isConflict(write.refusal)
    );
    for (const entry of conflicts) {
      const unsent = entry.write;
      const supersedes = kinds.get(unsent.kind)?.supersedes;
      if (unsent.status !== "refused" || unsent.sent || supersedes === undefined) continue;
      if (which === "unclaimed" && unsent.claimed) continue;
      const carried = conflicts.some(
        ({ write }) => write.sent && write.kind === unsent.kind && supersedes(write, unsent)
      );
      if (carried) this.#remove(entry);
    }
  }

  /** Makes needless each write the landed `landed` carries (`WriteKind.outdates`), and keeps it no
   *  longer: one of its kind that stands refused with no verdict of Dispatch's
   *  (`endedWithoutVerdict`), for a Retry - no conflict, which no Retry makes again. The run goes
   *  on through every refused write of the kind that a write it reached carries, whatever refused
   *  it, each once: a write Dispatch refused with a verdict, a conflict among them, is no write to
   *  let go, but what it was made on is carried all the same. A write the store no longer holds -
   *  one discarded - is no link, so the run stops there. A Retry of a write made needless lets it
   *  go (`retry`), and a reload does not take it up. */
  #outdate(landed: Write): void {
    const outdates = kinds.get(landed.kind)?.outdates;
    if (outdates === undefined) return;
    const reached = new Set<number>();
    const carriers = [landed];
    for (let carrier = carriers.pop(); carrier !== undefined; carrier = carriers.pop()) {
      for (const entry of this.#entries.values()) {
        const { write } = entry;
        if (reached.has(write.id) || write.kind !== carrier.kind) continue;
        if (write.status !== "refused" || !outdates(carrier, write)) continue;
        reached.add(write.id);
        carriers.push(write);
        if (entry.outdated || isConflict(write.refusal)) continue;
        if (!endedWithoutVerdict(write.refusal)) continue;
        entry.outdated = true;
        this.#forget(entry);
      }
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
   *  of it. Starts nothing, and answers the targets whose lines it took writes from. */
  #take(entries: readonly Entry[]): Set<string | undefined> {
    const taken = entries.filter((entry) => this.#entries.get(entry.write.id) === entry);
    for (const entry of taken) this.#entries.delete(entry.write.id);
    for (const entry of taken) {
      clearTimeout(entry.expiry);
      entry.stop.abort();
      entry.press?.reject(new SendStoppedError());
      entry.press = undefined;
      entry.turn?.settled.reject(new SendStoppedError());
      entry.turn = undefined;
      this.#forget(entry);
      kinds.get(entry.write.kind)?.undone?.(this.#client, entry.write);
    }
    if (taken.length > 0) this.#publish();
    return new Set(taken.map((entry) => entry.write.target));
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
   *  nothing shown and nothing sent; leaving the page is still confirmed while it is out. Nor is
   *  a write a newer landing made needless (`Entry.outdated`). */
  #keep(entry: Entry): void {
    const { write } = entry;
    const state = keptState(write);
    if (write.login === undefined || state === undefined || entry.outdated) return;
    if (entry.unkept || entry.keeping?.key === write.key) {
      this.#kept.keepNothing({ key: write.key, presentation: write.presentation, state });
      return;
    }
    const kind = kinds.get(write.kind);
    this.#kept.keep({
      carried: this.#carried.get(write.key) ?? null,
      detail: kind?.keep === undefined ? write.detail : kind.keep(write.detail),
      key: write.key,
      keyed: write.keyed,
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

  /** Builds the rows again from the writes held (`rows`), and tells every listener. */
  #publish(): void {
    const rows = [...this.#entries.values()]
      .map((entry) => entry.write)
      .sort((a, b) => a.id - b.id);
    this.#rows = rows.length === 0 ? NO_WRITES : rows;
    this.#notify();
  }

  /** Puts `next` in the place of what the reload kept that the store has not taken up
   *  (`#notTakenUp`), and tells every listener. A list with the same records in the same order is
   *  no change: the store keeps the array it has and tells no one, so a reader of `notTakenUp`
   *  draws again only for a change (`useNotTakenUp`). */
  #setNotTakenUp(next: readonly KeptWrite[]): void {
    const was = this.#notTakenUp;
    if (next.length === was.length && next.every((write, index) => write === was[index])) return;
    this.#notTakenUp = next;
    this.#notify();
  }

  /** Tells every listener of a change: of the rows (`#publish`), or of what the reload kept that
   *  the store has not taken up alone (`#setNotTakenUp`), which leaves the rows one array. */
  #notify(): void {
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

/** Whether `kept`, a write a reload kept, will be made again on its own when the take-up runs at
 *  `now`: still out, and pressed within `KEPT_SEND_RESUME_MS`. Shared by `#resume` and
 *  `#keptAhead`, so a kept write the take-up will not send for its state or its age holds no line
 *  when the page goes. One the take-up later drops for want of its kind still holds one, since
 *  the kinds are not in yet. */
function goesOnItsOwn(kept: KeptWrite, now: number): boolean {
  return kept.state.status !== "refused" && resumesOnItsOwn(kept.pressedAt, now);
}

/** How a kept state resumes once its write is made again. */
function resumedFrom(state: KeptState): From {
  if (state.status === "late") return state.stopping ? "stopping" : "late";
  return "sending";
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

/** What the reload kept that the store has not taken up yet (`Writes.notTakenUp`), re-rendering
 *  only when that changes. */
export function useNotTakenUp(): readonly KeptWrite[] {
  const store = useWrites();
  return useSyncExternalStore(store.subscribe, store.notTakenUp);
}
