import { type Dispatch, type SetStateAction, useCallback, useSyncExternalStore } from "react";

import type { AcceptedMention } from "../conversation/composer-model";
import { survivingMentions } from "../conversation/send-request";
import {
  entryNames,
  pageStorage,
  readEntry,
  removeEntry,
  type Shape,
  shape,
} from "../writes/tab-storage";

/** One unsent draft: what the reader typed, and the mentions picked in it, whose offsets are into
 *  that exact text (`features/conversation/MentionField.tsx`). The two travel together because
 *  only the records reach the wire and only the text reaches the reader, so a store that kept the
 *  text alone would hand a reopened box prose where a mention stood. */
export interface Draft {
  readonly mentions: readonly AcceptedMention[];
  readonly text: string;
}

/** The empty draft: no text and no mention (`draftEmpty`). A store of plain drafts reads it for a
 *  key it holds nothing under, and forgets a key by it (`createMentionDraftStore`'s `empty`). */
export const EMPTY_DRAFT: Draft = { mentions: [], text: "" };

/** Whether a draft holds nothing: no text and no picked mention. */
export function draftEmpty(draft: Draft): boolean {
  return draft.text === "" && draft.mentions.length === 0;
}

/** Why Ask back and Reply refuse an empty box: each sends what the box holds. */
export const EMPTY_DRAFT_REFUSAL = "Write something first.";

/** Why a send that posts what the box holds refuses `draft`, or `null` where it does not: the
 *  one rule, because the open card's Ask back and the answered ask's Reply are the same send. */
export function emptyDraftRefusal(draft: Draft): string | null {
  return draft.text.trim() === "" ? EMPTY_DRAFT_REFUSAL : null;
}

/** The mentions a draft would put on the wire: those whose span the reader has not edited since
 *  the picker wrote it (`survivingMentions`). A box's text is what the reader sees, so a mention
 *  they typed over is their own prose and reaches nobody. */
export function draftTargets(draft: Draft): readonly AcceptedMention[] {
  return survivingMentions(draft.text, draft.mentions);
}

/** Whether two drafts say the same thing, so a write that changes nothing tells nobody. */
function sameDraft(left: Draft, right: Draft): boolean {
  return (
    left.text === right.text &&
    left.mentions.length === right.mentions.length &&
    left.mentions.every((mention, index) => {
      const other = right.mentions[index];
      return (
        other !== undefined &&
        mention.target === other.target &&
        mention.text === other.text &&
        mention.start === other.start &&
        mention.end === other.end
      );
    })
  );
}

/** The mentions picked in a draft, as the tab keeps them (`Draft.mentions`). */
export const keptMentions = shape.array(
  shape.record({
    end: shape.number,
    start: shape.number,
    target: shape.string,
    text: shape.string,
  })
) satisfies Shape<readonly AcceptedMention[]>;

/** A draft as the tab keeps it (`Draft`): the text and the mentions picked in it. */
export const keptMentionDraft = shape.record({
  mentions: keptMentions,
  text: shape.string,
}) satisfies Shape<Draft>;

/** One keyed store of unsent drafts, and the ways its callers reach it. Its drafts are `D`: text
 *  (`createDraftStore`), or a `Draft` and whatever else the store's own drafts carry with it
 *  (`createMentionDraftStore`). */
export interface DraftStore<D = string> {
  /** The key's draft, read without subscribing to it. */
  read(key: string): D;
  /** What a reload of the page does to the store, for a test that stands for one (the draft
   *  matrix's `endAsAReload`): a store a module holds outlives the tree the test ends, where the
   *  next page load's would start empty. It forgets every draft in the page and whose they were,
   *  and every word that one is not kept, and leaves what the tab keeps, which the next sign-in
   *  takes up (`signedIn`). */
  reload(): void;
  /** Forgets every draft, and whose they were, in the page and in the tab. The unit-test preload
   *  calls it after each test, since the store outlives any one render. */
  reset(): void;
  /** Writes the key's draft - the draft, or an updater, which reads the draft as it is now - and
   *  tells every field holding that key; an empty draft (`""`, or `draftEmpty`) forgets it. A
   *  draft that says what the key already holds is no write, whatever else it carries. */
  set(key: string, next: SetStateAction<D>): void;
  /** Who is signed in now: `login`, or no one (`undefined`: the session lapsed). A draft is the
   *  person's who typed it, and the store outlives the shell, which another person signed in
   *  from another tab arrives in: their sign-in forgets every draft, the ones the tab keeps with
   *  them, as it drops every write not theirs (`Writes.signedIn`). A lapse keeps the drafts for
   *  the sign-in that follows, and the same person's keeps them on; a sign-out is no lapse
   *  (`signedOut`). The first sign-in the store is told of takes up the drafts the tab kept for
   *  that person before a reload. */
  signedIn(login: string | undefined): void;
  /** `login` signed out, and the sign-out landed - in this tab or another of the browser, or
   *  under a lapse that landed it (`Writes.onSignedOut`) - so whoever signs in next at this
   *  browser must find nothing they typed: every draft of theirs is forgotten in the page, with
   *  every entry the tab keeps for them, and the store takes no draft until someone signs in,
   *  since what a field hands it meanwhile - the shell going, its fields with it - is theirs too.
   *  Another person's drafts, and what the tab keeps for them, stay. */
  signedOut(login: string): void;
  /** The key's draft and its setter; the component re-renders whenever any field for that key
   *  changes it. An updater reads the draft as it is now, so a text added when an upload lands
   *  never overwrites what another upload or a keystroke put there since the render that
   *  started it. */
  useDraft(key: string): [D, Dispatch<SetStateAction<D>>];
}

/** What a store's drafts are, to the store: the draft it reads for a key it holds nothing under,
 *  which is also what it forgets a key by, whether a draft holds nothing, whether two say the same
 *  thing, and the text the tab keeps one as and reads it back from. */
interface DraftKind<D> {
  /** One value for every empty key, so a box whose draft was dropped reads the same object each
   *  time and re-renders for none of it. */
  readonly empty: D;
  readonly isEmpty: (draft: D) => boolean;
  /** The text the tab keeps a draft as, which the bounds below count. */
  readonly kept: (draft: D) => string;
  /** The draft that kept text holds, or undefined for text that holds none, which a load drops. */
  readonly parse: (text: string) => D | undefined;
  readonly same: (left: D, right: D) => boolean;
}

/** Drafts that are their text, kept in the tab as that text. */
const TEXT_DRAFTS: DraftKind<string> = {
  empty: "",
  isEmpty: (text) => text === "",
  kept: (text) => text,
  parse: (text) => text,
  same: (left, right) => left === right,
};

/**
 * Where every draft store keeps its drafts for a reload: the tab's `sessionStorage`, which a
 * reload keeps and closing the tab drops, as the held sends are kept (`write-storage.ts`). Each
 * draft is one entry, `dispatch.draft:<store>:<key>`, in one versioned format: the text the store
 * keeps the draft as (`DraftKind.kept`), and the login of the person who typed it. A store reads
 * its entries back only for that person, at the first sign-in it is told of, and drops each entry
 * of another person's, of another format, or whose text holds no draft; a sign-out drops every
 * entry that names the person who signed out, of whatever format. No store reads the entries of a
 * store name that is none of `STORE_NAMES`, so the first store to load over a tab's storage drops
 * those.
 */
const ENTRY_PREFIX = "dispatch.draft:";

/** The names of the app's draft stores: the composers' drafts and the ones each keeps by
 *  address (`unsent-drafts.ts`), and the asks' answers and replies (`answer-drafts.ts`,
 *  `reply-drafts.ts`). */
const STORE_NAMES = ["unsent", "away", "answer", "reply"] as const;

export type StoreName = (typeof STORE_NAMES)[number];

/** The storages a store has loaded over, whose entries of no store's name are dropped. */
const swept = new WeakSet<Storage>();

/** The format's version: an entry of any other is dropped on load, never read. */
const FORMAT_VERSION = 1;

const keptDraftShape = shape.record({
  login: shape.string,
  text: shape.string,
  version: shape.oneOf(FORMAT_VERSION),
}) satisfies Shape<{ readonly login: string; readonly text: string; readonly version: 1 }>;

/**
 * The longest draft a store keeps for a reload, in UTF-16 code units of the text it keeps for a
 * key (`DraftKind.kept`): a composer's draft, and an answer or a reply with the mentions picked
 * in it, as its JSON. It is twice the longest block Dispatch takes in a body (32,000 units). A
 * longer draft stays in the page while it is open, and the page says it is not kept
 * (`DraftsNotKept`).
 */
export const DRAFT_KEPT_UNITS = 65_536;

/**
 * The most text one store keeps for a reload across all its drafts, in the same units. The four
 * stores at this bound hold about 2 MB, which leaves room for the held sends inside the 5 MB a
 * tab's storage holds in the major browsers. A draft that would take the store past it stays in
 * the page, the page says it is not kept, and it is kept as soon as there is room again.
 */
export const STORE_KEPT_UNITS = 262_144;

/** Why the tab does not keep a draft for a reload, which the page says (`DraftsNotKept`): it is
 *  longer than `DRAFT_KEPT_UNITS`, its store holds `STORE_KEPT_UNITS` already, or the browser
 *  refused the write (its storage for the tab is full, or the page has none). */
export type NotKeptReason = "too long" | "store full" | "refused";

/** The order the page names the reasons in. */
const REASONS: readonly NotKeptReason[] = ["too long", "store full", "refused"];

/** Every draft of every store the tab does not keep, by its entry's name, with the reason. */
const notKept = new Map<string, NotKeptReason>();
const notKeptListeners = new Set<() => void>();
let notKeptReasons: readonly NotKeptReason[] = [];

function noteNotKept(entry: string, reason: NotKeptReason | undefined): void {
  if (notKept.get(entry) === reason) return;
  if (reason === undefined) notKept.delete(entry);
  else notKept.set(entry, reason);
  const reasons = new Set(notKept.values());
  notKeptReasons = REASONS.filter((each) => reasons.has(each));
  for (const listener of notKeptListeners) listener();
}

const subscribeNotKept = (listener: () => void): (() => void) => {
  notKeptListeners.add(listener);
  return () => notKeptListeners.delete(listener);
};

/** Why the tab keeps some draft the reader has not sent for no reload, each reason once, in a
 *  fixed order; empty while it keeps every one. */
export function useDraftsNotKept(): readonly NotKeptReason[] {
  return useSyncExternalStore(subscribeNotKept, () => notKeptReasons);
}

/** Drops, once for each storage, every draft entry of a store name none of `STORE_NAMES` is,
 *  whoever kept it: an entry of a store the app has is that store's own load to judge. */
function dropUnknownStores(storage: Storage): void {
  if (swept.has(storage)) return;
  swept.add(storage);
  const known = STORE_NAMES.map((name) => `${ENTRY_PREFIX}${name}:`);
  for (const entry of entryNames(storage, ENTRY_PREFIX)) {
    if (!known.some((prefix) => entry.startsWith(prefix))) removeEntry(storage, entry);
  }
}

/**
 * A store of unsent text, keyed by what the text belongs to (a composer's send name): the draft
 * belongs to that thing rather than to the field showing it, so every field for one key reads and
 * writes the same draft, a field that remounts picks it back up, and the draft outlives every
 * render. Once the store knows who is signed in, each draft is kept in the tab under the store's
 * `name`, one of `STORE_NAMES`, as well (`ENTRY_PREFIX`), so it outlives a reload of the page
 * too, within the bounds above.
 *
 * Each of the app's draft stores is one of these or a store of mention drafts
 * (`createMentionDraftStore`) - the two the composers' unsent drafts keep for each query client
 * (`unsent-drafts.ts`), the answer drafts (`answer-drafts.ts`) and the reply drafts
 * (`reply-drafts.ts`) - and all of them are `draftStore`, so a fix to the subscription, the
 * listener cleanup, the updater, the sign-in, the sign-out or the keeping reaches every one.
 * `storage` is the tab's own unless a test names another; undefined, as where the browser gives
 * the page none, it keeps nothing for a reload and says so.
 */
export function createDraftStore(
  name: StoreName,
  storage: Storage | undefined = pageStorage()
): DraftStore {
  return draftStore(name, TEXT_DRAFTS, storage);
}

/**
 * A store of the drafts of a box with the `@` picker in it, keyed by what the draft belongs to (an
 * ask id), as `createDraftStore` keeps text: a field that remounts picks the draft back up - its
 * mentions with its text - and a reload does too, the tab keeping each as its JSON.
 *
 * A store's drafts may carry more than the text and its mentions, written with them: a reply
 * draft records whether its ask was settled when the reader wrote it (`reply-drafts.ts`). Only
 * the text and the mentions are compared, so a write that changes neither is no write, and what
 * the draft carries stays what the last write that changed them recorded. `empty` is what the
 * store reads for a key it holds nothing under, and the draft it forgets a key by, and `fits` is
 * the shape a kept draft is read back by: text an older release kept in another shape, or that
 * does not parse, is no draft, and the load drops it.
 */
export function createMentionDraftStore<D extends Draft>(
  name: StoreName,
  empty: D,
  fits: Shape<D>,
  storage: Storage | undefined = pageStorage()
): DraftStore<D> {
  return draftStore(
    name,
    {
      empty,
      isEmpty: draftEmpty,
      kept: (draft) => JSON.stringify(draft),
      parse: (text) => {
        try {
          const draft: unknown = JSON.parse(text);
          return fits(draft) ? draft : undefined;
        } catch {
          return undefined;
        }
      },
      same: sameDraft,
    },
    storage
  );
}

/** The store both constructors above make, over drafts of `kind`. */
function draftStore<D>(
  name: StoreName,
  kind: DraftKind<D>,
  storage: Storage | undefined
): DraftStore<D> {
  const drafts = new Map<string, D>();
  const listeners = new Map<string, Set<() => void>>();
  /** The person signed in when the drafts were typed: the last sign-in that named someone. */
  let owner: string | undefined;
  /** Whether that person signed out, with nobody signed in since (`signedOut`): until someone
   *  does, the store takes no draft. */
  let ended = false;
  const prefix = `${ENTRY_PREFIX}${name}:`;
  /** The units of each draft the tab keeps, by key, and their sum: what the store's bound
   *  counts. */
  const keptUnits = new Map<string, number>();
  let keptTotal = 0;
  /** The drafts the tab does not keep for want of room or for a refused write, in the order they
   *  were refused: each is tried again once a draft leaves the tab. */
  const waiting = new Set<string>();

  const read = (key: string): D => drafts.get(key) ?? kind.empty;

  /** The text the tab keeps for the key's draft, `""` for none. */
  const keptText = (draft: D): string => (kind.isEmpty(draft) ? "" : kind.kept(draft));

  const tell = (key: string): void => {
    for (const listener of listeners.get(key) ?? []) listener();
  };

  /** Writes the key's entry, or says why the tab does not keep it. */
  const write = (key: string, text: string): NotKeptReason | undefined => {
    const before = keptUnits.get(key) ?? 0;
    if (text.length > DRAFT_KEPT_UNITS) return "too long";
    if (keptTotal - before + text.length > STORE_KEPT_UNITS) return "store full";
    if (storage === undefined) return "refused";
    try {
      storage.setItem(
        `${prefix}${key}`,
        JSON.stringify({ login: owner, text, version: FORMAT_VERSION })
      );
    } catch {
      return "refused";
    }
    keptUnits.set(key, text.length);
    keptTotal += text.length - before;
    return undefined;
  };

  /** Drops what the tab keeps for `key`, which stands for nothing once the draft changed. */
  const unkeep = (key: string): void => {
    const units = keptUnits.get(key);
    removeEntry(storage, `${prefix}${key}`);
    if (units === undefined) return;
    keptUnits.delete(key);
    keptTotal -= units;
  };

  /** Keeps the key's draft in the tab for the person signed in, or drops what was kept for it
   *  and says why it is not kept; with nobody signed in yet, nothing is anyone's to keep. A draft
   *  that leaves the tab, or takes less of it, tries each that waits for room again. */
  const keep = (key: string, draft: D): void => {
    if (owner === undefined) return;
    const before = keptUnits.get(key) ?? 0;
    const text = keptText(draft);
    const reason = text === "" ? undefined : write(key, text);
    if (text === "" || reason !== undefined) unkeep(key);
    noteNotKept(`${prefix}${key}`, reason);
    if (reason === "store full" || reason === "refused") waiting.add(key);
    else waiting.delete(key);
    if ((keptUnits.get(key) ?? 0) >= before) return;
    for (const other of [...waiting]) {
      if (other === key || write(other, keptText(read(other))) !== undefined) continue;
      waiting.delete(other);
      noteNotKept(`${prefix}${other}`, undefined);
    }
  };

  const set = (key: string, next: SetStateAction<D>): void => {
    if (ended) return;
    const current = read(key);
    // A draft is text or a `Draft`, never a function, so a function is the updater.
    const update = next as (previous: D) => D;
    const draft = typeof next === "function" ? update(current) : next;
    if (kind.same(current, draft)) return;
    if (kind.isEmpty(draft)) drafts.delete(key);
    else drafts.set(key, draft);
    keep(key, draft);
    tell(key);
  };

  /** Takes up the drafts the tab kept for `login` before the page loaded, and drops every entry
   *  of this store's that another person kept, that is of another format or whose text holds no
   *  draft; the first load over a storage drops every entry of no store's name too
   *  (`dropUnknownStores`). A draft already in the page wins over the one the tab kept for its
   *  key, and is kept in its place. */
  const load = (login: string): void => {
    const loaded = new Set<string>();
    if (storage !== undefined) {
      dropUnknownStores(storage);
      for (const entry of entryNames(storage, prefix)) {
        const key = entry.slice(prefix.length);
        const kept = readEntry(storage, entry);
        if (!keptDraftShape(kept) || kept.login !== login) {
          removeEntry(storage, entry);
          continue;
        }
        const draft = kind.parse(kept.text);
        if (draft === undefined || kind.isEmpty(draft)) {
          removeEntry(storage, entry);
          continue;
        }
        keptUnits.set(key, kept.text.length);
        keptTotal += kept.text.length;
        if (drafts.has(key)) continue;
        loaded.add(key);
        drafts.set(key, draft);
        tell(key);
      }
    }
    for (const [key, draft] of drafts) {
      if (!loaded.has(key)) keep(key, draft);
    }
  };

  function useDraft(key: string): [D, Dispatch<SetStateAction<D>>] {
    const subscribe = useCallback(
      (listener: () => void) => {
        const keyListeners = listeners.get(key) ?? new Set<() => void>();
        listeners.set(key, keyListeners);
        keyListeners.add(listener);
        return () => {
          keyListeners.delete(listener);
          if (keyListeners.size === 0) listeners.delete(key);
        };
      },
      [key]
    );
    const draft = useSyncExternalStore(subscribe, () => read(key));
    const setDraft = useCallback((next: SetStateAction<D>) => set(key, next), [key]);
    return [draft, setDraft];
  }

  /** Forgets every draft in the page and whose they were, telling each field that holds one, and
   *  every word that one is not kept; what the tab keeps for a reload is the caller's to drop. */
  const forget = (): void => {
    // Nobody's first, so forgetting each draft writes nothing to the tab on the way.
    owner = undefined;
    for (const key of [...drafts.keys()]) set(key, kind.empty);
    for (const entry of [...notKept.keys()]) {
      if (entry.startsWith(prefix)) noteNotKept(entry, undefined);
    }
    keptUnits.clear();
    keptTotal = 0;
    waiting.clear();
  };

  const reset = (): void => {
    ended = false;
    forget();
    if (storage !== undefined) {
      for (const entry of entryNames(storage, prefix)) removeEntry(storage, entry);
    }
  };

  return {
    read,
    reload: () => {
      ended = false;
      forget();
    },
    reset,
    set,
    signedIn: (login) => {
      if (login === undefined || login === owner) return;
      if (owner !== undefined) reset();
      ended = false;
      owner = login;
      load(login);
    },
    signedOut: (login) => {
      // The drafts in the page are the owner's alone, and an entry may be anyone's: one kept
      // before this page was told who is signed in, or one of another format, still names them.
      if (login === owner) {
        forget();
        ended = true;
      }
      if (storage === undefined) return;
      for (const entry of entryNames(storage, prefix)) {
        const kept = readEntry(storage, entry);
        if (typeof kept === "object" && kept !== null && Reflect.get(kept, "login") === login) {
          removeEntry(storage, entry);
        }
      }
    },
    useDraft,
  };
}

/** Drops every draft every store keeps in the tab, and every word that one is not kept. The
 *  unit-test preload calls it after each test, since the tab's storage outlives the test while
 *  the stores that wrote it do not, and the next test's sign-in would take up what they kept. */
export function resetDraftStorage(): void {
  const storage = pageStorage();
  if (storage !== undefined) {
    for (const entry of entryNames(storage, ENTRY_PREFIX)) removeEntry(storage, entry);
  }
  for (const entry of [...notKept.keys()]) noteNotKept(entry, undefined);
}
