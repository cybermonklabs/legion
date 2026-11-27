import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useId,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
} from "react";

import type { DisplayTree } from "../../api/types";
import { type ReferenceNaming, useMarkdownHeadline } from "./markdown-engine";
import type { PostDeletion } from "./post-marks";
import { linkTitleMax } from "./reference-target";
import { type DispatchReferenceRoute, parseNodeReference } from "./routes";
import { referenceTargetKind } from "./target-id";
import { firstWordedBlock } from "./tree-fragment";

/**
 * The names a page has already read for the references it draws, shared with every surface the
 * shell mounts beside it.
 *
 * A link to a block shows the block's own words, and reading them one link at a time is one
 * `GET /api/v1/references` per pointer - the read `GET /api/v1/reference-targets` makes once for
 * them all (its declaration in `packages/envoy/internal/dispatch/api`). A page that has
 * made that batched read publishes what it read here, and every surface under the shell names a
 * reference from it instead of asking for it again: the plain-text projection before it mounts a
 * read of its own (`plain-text.tsx`), and the new issue page's rows. A page that publishes
 * nothing leaves every surface exactly as it was, reading its own targets.
 *
 * A page publishes how each target is named - the reading its name is projected from - and the
 * store projects each one once, by the one reader every surface reads a stored text with
 * (`useMarkdownHeadline`), so no page writes a target's words by a reader of its own. A name is a
 * title drawn over one record: the references inside it read as their short forms, as a link's
 * title reads them (`ReferenceNaming.readsNone`), since a name inside a name resolves nothing.
 * The store holds each name as a link to the target reads it (R8, `RefLink`): a target a link
 * titles by its own text - an ask, a comment, a message, a block's whole text - in the link's
 * first characters and an ellipsis, cut where the link cuts it (`linkTitleMax`), and any other by
 * its whole title. A chip names a block by its first block that holds words instead, and a whole
 * document by the page's word for it (`ReferenceNameReading.chipWord`), which the store projects
 * beside its name (`KnownReferences.lines`), and a chip cuts what it draws to its own room where
 * it draws it.
 *
 * The store sits above the router, beside the margin's slot (`issue-page/margin-slot.tsx`),
 * because the margin is mounted beside the router and its cards draw the same references as the
 * page's own column: a context inside the page would not reach them. A page publishes in the
 * layout phase of the commit that draws what it read, so the page hands its own subtree the same
 * value in the render that draws it as well (`PublishedReferenceNames`).
 *
 * More than one surface can publish at once: the page's own column and a sheet that draws the
 * outline beside it each read the tree and publish what they read. The store keeps one value per
 * publisher and hands readers the union of every value still published (`merged`), so a surface
 * that unmounts takes only its own value with it, and every surface keeps the names the surfaces
 * still mounted read. Each part readers are handed changes only where what it holds does, so a
 * publisher that publishes the same references again, in sets made anew, draws no surface
 * again.
 */

/** How a page names one target it read, which the store projects into the name readers see: the
 *  reading of the name's words - a block's own, or a title's as one text run, which nothing
 *  parses - with the page's own word for the target, which names it where those words project
 *  to none (`fallback`); or words that are no writer's text - the page's own word for a target -
 *  drawn as they stand. A whole document's reading carries the page's word for it as well
 *  (`chipWord`), which a chip draws in place of the name, since a chip never draws a document's
 *  name (decision 39). */
export type ReferenceNameReading = (
  | { readonly tree: DisplayTree; readonly fallback?: string }
  | { readonly words: string }
) & { readonly chipWord?: string };

/** What a page publishes from its one batched read, as one value per publisher, so its parts
 *  always come from one read and change together. */
export interface PublishedReferences {
  /** The references the read answered ambiguous: each writes short an id two or more targets
   *  carry, so it names none of them, and a surface draws it as its author wrote it. */
  readonly ambiguous: ReadonlySet<string>;
  /** Every reference the page asks the read for, answered or not yet: a surface waits on the
   *  page's answer for one of these rather than reading it itself. */
  readonly asked: ReadonlySet<string>;
  /** The references among `readings` whose comment or message its author deleted, by the
   *  delete (`useKnownDeletedReferences`). The page names each by its deleted line, which holds
   *  the actor as no registry knows it (`actorLabelText`); the issue page's rows draw the line
   *  from the delete instead, the actor named as the page names an author (`RowText.tsx`). */
  readonly deleted: ReadonlyMap<string, PostDeletion>;
  /** The references among `readings` whose post its author edited, a comment, a message or a
   *  block of either (`useKnownEditedReferences`). A surface that draws such a reference's name
   *  cuts the name to its own length and then marks it ` · edited`, so no cut takes the mark. */
  readonly edited: ReadonlySet<string>;
  /** The references the read answered that nothing answers to now, or that the reader may not
   *  see: a surface draws each as its author wrote it and reads it no more (R8). */
  readonly gone: ReadonlySet<string>;
  /** How the read names each target that has a name, by its reference. */
  readonly readings: ReadonlyMap<string, ReferenceNameReading>;
  /** The references the read answered with no words to name them by - another issue's block it
   *  sent no reading of - which is no answer that the target has no name: a surface reads each
   *  one itself, as a link to it does (`RefLink`, R8). */
  readonly unnamed: ReadonlySet<string>;
}

/** What readers see: the published references, the name each reading projected to, and for a
 *  block and a whole document the line a chip names it by. A reference whose reading has not
 *  projected yet - the reader's engine has not answered - has no name yet, and one whose reading
 *  projected to no words is among `unnamed`. */
export interface KnownReferences {
  readonly ambiguous: ReadonlySet<string>;
  readonly asked: ReadonlySet<string>;
  readonly deleted: ReadonlyMap<string, PostDeletion>;
  readonly edited: ReadonlySet<string>;
  readonly gone: ReadonlySet<string>;
  /** The line a chip names a target by where it names it otherwise than a link does, by its
   *  reference (`chipLineReading`): a block by the first block of its words that holds any,
   *  whole, and a whole document by the page's word for it (`ReferenceNameReading.chipWord`).
   *  A chip cuts it to its room where it draws it: one drawn in place in a node's words
   *  (`issue-page/row-references.ts`), a tie's naming a block (`issue-page/NodeRow.tsx`'s
   *  `Chip`), the Plan chart's needs and serves (`issue-page/plan-filter/PlanFilterBody.tsx`'s
   *  `chipLine`), and a check's "Also under" line in the Verification view
   *  (`issue-page/verification-filter/VerificationFilterBody.tsx`'s `AlsoUnder`). None for any
   *  other target. */
  readonly lines: ReadonlyMap<string, string>;
  readonly names: ReadonlyMap<string, string>;
  readonly unnamed: ReadonlySet<string>;
}

/** No reference at all: what a page that published nothing has read. */
const NO_REFERENCES: ReadonlySet<string> = new Set();
const NO_READINGS: ReadonlyMap<string, ReferenceNameReading> = new Map();
const NO_NAMES: ReadonlyMap<string, string> = new Map();
const NO_DELETED: ReadonlyMap<string, PostDeletion> = new Map();

/** What the store holds while no page publishes, and what a surface that publishes nothing hands
 *  what it draws: no reference at all. */
export const NOTHING_PUBLISHED: PublishedReferences = {
  ambiguous: NO_REFERENCES,
  asked: NO_REFERENCES,
  deleted: NO_DELETED,
  edited: NO_REFERENCES,
  gone: NO_REFERENCES,
  readings: NO_READINGS,
  unnamed: NO_REFERENCES,
};

/** What a reader sees while no page publishes, and outside the provider. */
const NOTHING_KNOWN: KnownReferences = {
  ambiguous: NO_REFERENCES,
  asked: NO_REFERENCES,
  deleted: NO_DELETED,
  edited: NO_REFERENCES,
  gone: NO_REFERENCES,
  lines: NO_NAMES,
  names: NO_NAMES,
  unnamed: NO_REFERENCES,
};

/** One publisher's change, handed to the store: what it publishes now, under its own key
 *  (`useId`), or `undefined` as it leaves. */
interface Publication {
  readonly published: PublishedReferences | undefined;
  readonly publisher: string;
}

/** What the store holds: every value still published, by its publisher's key, in the order the
 *  publishers last published, and the one value merged from them (`merged`). */
interface Store {
  readonly publications: ReadonlyMap<string, PublishedReferences>;
  readonly value: PublishedReferences;
}

/** No publisher at all: what the store holds before any page publishes and after the last one
 *  leaves. */
const NO_PUBLICATIONS: Store = { publications: new Map(), value: NOTHING_PUBLISHED };

const ReferenceNamesContext = createContext<KnownReferences>(NOTHING_KNOWN);

/** Where a page publishes what it read, and withdraws it again. A page outside the provider
 *  publishes to nobody. */
const ReferenceNamesRegistry = createContext<(publication: Publication) => void>(() => {});

/** Whether `held` holds every reference of `references`: the one test by which the store hands a
 *  reader the set it already holds while what that set holds stays the same (`including`,
 *  `union`). */
function holdsEvery(held: ReadonlySet<string>, references: ReadonlySet<string>): boolean {
  for (const reference of references) {
    if (!held.has(reference)) return false;
  }
  return true;
}

/** `held` where it already holds every reference of `added`, so no copy is made, and the union
 *  of the two otherwise. */
function including(held: ReadonlySet<string>, added: ReadonlySet<string>): ReadonlySet<string> {
  if (holdsEvery(held, added)) return held;
  return held.size === 0 ? added : new Set([...held, ...added]);
}

/** The union of one part of every value still published, `kept` where it holds the same
 *  references, so a reader is handed the set it already holds: a publisher's own set where it
 *  alone holds any, so no copy is made, and a union of them all where two do. */
function union(parts: readonly ReadonlySet<string>[], kept: ReadonlySet<string>) {
  const held = parts.filter((part) => part.size > 0);
  const joined =
    held.length > 1 ? new Set(held.flatMap((part) => [...part])) : (held[0] ?? NO_REFERENCES);
  return joined.size === kept.size && holdsEvery(kept, joined) ? kept : joined;
}

/** The readings of every value still published, `kept` where it holds the same readings. Where
 *  two publishers name one reference, the one that published last stands: a publisher publishes
 *  again each time its read answers anew, so the last to publish holds the newest answer. */
function readingsOf(
  published: readonly PublishedReferences[],
  kept: ReadonlyMap<string, ReferenceNameReading>
): ReadonlyMap<string, ReferenceNameReading> {
  const held = published.map((value) => value.readings).filter((map) => map.size > 0);
  const joined =
    held.length > 1 ? new Map(held.flatMap((map) => [...map])) : (held[0] ?? NO_READINGS);
  const same =
    joined === kept ||
    (joined.size === kept.size &&
      [...joined].every(([reference, reading]) => kept.get(reference) === reading));
  return same ? kept : joined;
}

/** The edited references of every value still published, `kept` where it holds the same
 *  references. Whether a named reference is edited, and whether it is deleted
 *  (`deletedOfLast`), is the answer of the last publisher to name it, so a name and its marks
 *  always come from one read: a newer answer that names a post by its deleted line is never
 *  marked by an older one that read it edited. */
function editedOfLast(
  published: readonly PublishedReferences[],
  kept: ReadonlySet<string>
): ReadonlySet<string> {
  const named = published.filter((value) => value.readings.size > 0);
  let edited: ReadonlySet<string> = named[0]?.edited ?? NO_REFERENCES;
  if (named.length > 1) {
    const marked = new Set<string>();
    for (const value of named) {
      for (const reference of value.readings.keys()) {
        if (value.edited.has(reference)) marked.add(reference);
        else marked.delete(reference);
      }
    }
    edited = marked;
  }
  return edited.size === kept.size && holdsEvery(kept, edited) ? kept : edited;
}

/** The deleted references of every value still published, each with its delete as the last
 *  publisher to name it answers it (`editedOfLast`), `kept` where it holds the same deletes. */
function deletedOfLast(
  published: readonly PublishedReferences[],
  kept: ReadonlyMap<string, PostDeletion>
): ReadonlyMap<string, PostDeletion> {
  const named = published.filter((value) => value.readings.size > 0);
  let deleted: ReadonlyMap<string, PostDeletion> = named[0]?.deleted ?? NO_DELETED;
  if (named.length > 1) {
    const marked = new Map<string, PostDeletion>();
    for (const value of named) {
      for (const reference of value.readings.keys()) {
        const deletion = value.deleted.get(reference);
        if (deletion === undefined) marked.delete(reference);
        else marked.set(reference, deletion);
      }
    }
    deleted = marked;
  }
  const same =
    deleted === kept ||
    (deleted.size === kept.size &&
      [...deleted].every(([reference, deletion]) => kept.get(reference) === deletion));
  return same ? kept : deleted;
}

/** Every value still published as the one value readers see, each part the one `previous`
 *  holds where it holds the same, so a part readers are handed changes only where what it holds
 *  does (`ReferenceNamesProvider`). */
function merged(
  publications: ReadonlyMap<string, PublishedReferences>,
  previous: PublishedReferences
): PublishedReferences {
  if (publications.size === 0) return NOTHING_PUBLISHED;
  const published = [...publications.values()];
  return {
    ambiguous: union(
      published.map((value) => value.ambiguous),
      previous.ambiguous
    ),
    asked: union(
      published.map((value) => value.asked),
      previous.asked
    ),
    deleted: deletedOfLast(published, previous.deleted),
    edited: editedOfLast(published, previous.edited),
    gone: union(
      published.map((value) => value.gone),
      previous.gone
    ),
    readings: readingsOf(published, previous.readings),
    unnamed: union(
      published.map((value) => value.unnamed),
      previous.unnamed
    ),
  };
}

/** The store after one publisher's change. A publisher's value is removed before it is set
 *  again, so one that publishes again moves to the end of the order `merged` reads them by; one
 *  that leaves takes its own value with it and no other. */
function withPublication(store: Store, { published, publisher }: Publication): Store {
  if (published === undefined && !store.publications.has(publisher)) return store;
  const publications = new Map(store.publications);
  publications.delete(publisher);
  if (published !== undefined) publications.set(publisher, published);
  return { publications, value: merged(publications, store.value) };
}

/** How a name is read: a title drawn over one record, inside which a reference reads as its
 *  short form and nothing is read (`ReferenceNaming.readsNone`). */
const NAME_NAMING: ReferenceNaming = { names: new Map(), readsNone: true };

/**
 * The name `reading` projects to, by the one reader every surface reads a stored text with
 * (`useMarkdownHeadline`), as a title over one record, so a name inside it resolves nothing
 * (`NAME_NAMING`), and cut to `max` characters: a published name, and a chip's line beside it
 * (`chipLineReading`). Words that are no writer's text are the name as they stand. `undefined`
 * for no reading and until the engine has answered, and `""` for a reading that projects to no
 * words and names no `fallback`.
 */
function useReadingName(
  reading: ReferenceNameReading | undefined,
  max: number
): string | undefined {
  const headline = useMarkdownHeadline(
    reading !== undefined && "tree" in reading ? reading.tree : undefined,
    max,
    undefined,
    NAME_NAMING
  );
  if (reading === undefined) return undefined;
  if ("words" in reading) return reading.words;
  return headline === undefined ? undefined : headline.text || (reading.fallback ?? "");
}

/**
 * Whether a chip names the target `route` by a block's line (issue page decisions 66 and 88):
 * `route` names a block, and the reading the page published for it is the block's whole text,
 * whose first block that holds words the store projects beside its name (`chipLineReading`,
 * `KnownReferences.lines`). A tie's chip (`issue-page/NodeRow.tsx`'s `Chip`) waits on that line
 * where this holds, and reads its address otherwise.
 */
export function namesBlockByLine(
  route: DispatchReferenceRoute | undefined,
  reading: ReferenceNameReading | undefined
): boolean {
  return (
    route !== undefined &&
    referenceTargetKind(route) === "block" &&
    reading !== undefined &&
    "tree" in reading
  );
}

/**
 * The reading a chip names a target by where it names it otherwise than a link does, from the
 * reading the page published for it (`reading`): a whole document by the page's word for it
 * (`ReferenceNameReading.chipWord`); and a block (`namesBlockByLine`), whose reading is its whole
 * text, by its first block that holds any words (`firstWordedBlock`), so a decision is named by
 * its own heading, with the page's word for the block's kind (`fallback`). The page publishes a
 * block's whole text only where a block of it holds words (`issue-page/tree-drafts.ts`'s
 * `treeContext`). A link to the target, and so a plain line, names it by the name instead.
 * Undefined for every target a chip names by that name.
 */
function chipLineReading(
  reference: string,
  reading: ReferenceNameReading
): ReferenceNameReading | undefined {
  if (reading.chipWord !== undefined) return { words: reading.chipWord };
  if (!("tree" in reading) || !namesBlockByLine(parseNodeReference(reference), reading)) {
    return undefined;
  }
  const tree = firstWordedBlock(reading.tree) ?? reading.tree;
  return reading.fallback === undefined ? { tree } : { fallback: reading.fallback, tree };
}

/** What one target's projection reported: its name, `""` for a reading that projected to no
 *  words, and for a block or a whole document the line a chip names it by (`chipLineReading`),
 *  each `undefined` while its projection has not answered or where the reader's schema refuses
 *  its reading. */
interface Named {
  readonly line: string | undefined;
  readonly name: string | undefined;
}

/** How one projection tells the store what its reading projected to, and `undefined` while
 *  nothing of it has answered and as it leaves. */
type NameReport = (reference: string, named: Named | undefined) => void;

/** One target's name, projected from the reading the page published for it as a link to the
 *  target reads it: the whole of it, cut as the link cuts its title (`linkTitleMax`). For a block
 *  or a whole document, the line a chip names it by beside it (`chipLineReading`). It draws
 *  nothing. */
function NameProjection({
  onName,
  reading,
  reference,
}: {
  onName: NameReport;
  reading: ReferenceNameReading;
  reference: string;
}): ReactNode {
  const max = useMemo(() => linkTitleMax(parseNodeReference(reference)), [reference]);
  const name = useReadingName(reading, max);
  const lineReading = useMemo(() => chipLineReading(reference, reading), [reading, reference]);
  const line = useReadingName(lineReading, Number.POSITIVE_INFINITY);
  // In the layout phase, so a name lands in the commit its projection answered in, before the
  // page is painted with the reference still unnamed.
  useLayoutEffect(() => {
    onName(reference, name === undefined && line === undefined ? undefined : { line, name });
  }, [line, name, onName, reference]);
  useLayoutEffect(() => () => onName(reference, undefined), [onName, reference]);
  return null;
}

/** The names the projections reported, the references whose reading projected to no words, and
 *  the line a chip names each block and each whole document by. */
interface Projected {
  readonly blank: ReadonlySet<string>;
  readonly lines: ReadonlyMap<string, string>;
  readonly names: ReadonlyMap<string, string>;
}

export function ReferenceNamesProvider({ children }: { children: ReactNode }): ReactNode {
  const [{ value }, publish] = useReducer(withPublication, NO_PUBLICATIONS);
  // Every projected name, kept in place as each projection reports, and handed to readers as
  // one map per change (`version`): a page's projections report in one commit, and a map copied
  // for each report would be copied once per target.
  const reported = useRef(new Map<string, Named>());
  const [version, setVersion] = useState(0);
  const onName = useCallback<NameReport>((reference, named) => {
    const all = reported.current;
    const now = all.get(reference);
    if (now?.name === named?.name && now?.line === named?.line) return;
    if (named === undefined) all.delete(reference);
    else all.set(reference, named);
    setVersion((count) => count + 1);
  }, []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: `version` counts each change of the map the ref holds.
  const projected = useMemo<Projected>(() => {
    const names = new Map<string, string>();
    const lines = new Map<string, string>();
    const blank = new Set<string>();
    for (const [reference, { line, name }] of reported.current) {
      if (name === "") blank.add(reference);
      else if (name !== undefined) names.set(reference, name);
      if (line !== undefined && line !== "") lines.set(reference, line);
    }
    return {
      blank: blank.size === 0 ? NO_REFERENCES : blank,
      lines: lines.size === 0 ? NO_NAMES : lines,
      names: names.size === 0 ? NO_NAMES : names,
    };
  }, [version]);
  const { ambiguous, asked, deleted, edited, gone, unnamed } = value;
  const known = useMemo<KnownReferences>(
    () => ({
      ambiguous,
      asked,
      deleted,
      edited,
      gone,
      lines: projected.lines,
      names: projected.names,
      unnamed: including(unnamed, projected.blank),
    }),
    [ambiguous, asked, deleted, edited, gone, projected, unnamed]
  );
  // Drawn again only where the readings do, so a publisher that publishes the same readings
  // again projects none of them again.
  const projections = useMemo(
    () =>
      [...value.readings].map(([reference, reading]) => (
        <NameProjection key={reference} onName={onName} reading={reading} reference={reference} />
      )),
    [onName, value.readings]
  );
  return (
    <ReferenceNamesRegistry.Provider value={publish}>
      <ReferenceNamesContext.Provider value={known}>
        {children}
        {projections}
      </ReferenceNamesContext.Provider>
    </ReferenceNamesRegistry.Provider>
  );
}

/**
 * Publishes `published` for as long as the calling page is mounted, and withdraws it again when
 * the page leaves, leaving every other publisher's value as it was. It must be stable across
 * renders (`useMemo`): the store holds it in state, so a fresh value every render would register
 * on every commit. `undefined` publishes nothing. It publishes in the layout phase of the commit
 * that draws what the page read, so a surface beside the page drawn in that commit is named in
 * the commit after, before the page is painted; the page's own subtree reads the value in the
 * commit that draws it (`PublishedReferenceNames`).
 */
export function usePublishReferenceNames(published: PublishedReferences | undefined): void {
  const register = useContext(ReferenceNamesRegistry);
  // The key this publisher's value is held under, the same for as long as it is mounted, so its
  // cleanup withdraws its own value and no other.
  const publisher = useId();
  // A new value takes the place of the one before it, and the store keeps every part of it that
  // holds what it held (`merged`); a value withdrawn first would leave the store empty between
  // the two, and every part of the next would be new to it.
  useLayoutEffect(() => {
    register({ published, publisher });
  }, [published, publisher, register]);
  useLayoutEffect(() => () => register({ published: undefined, publisher }), [publisher, register]);
}

/**
 * Hands the subtree of a page that publishes `published` (`usePublishReferenceNames`) the page's
 * own value in the very render that draws it, over what the store holds: the store takes the
 * value in the layout phase of that commit, and a surface the subtree draws in it would read a
 * reference the page is asking for before the page's answer reached it. So a row drawn with the
 * page's read waits on that read for every reference the page asks it for, from its first
 * render, and mounts no read of its own for one (`plain-text.tsx`). A name is the store's alone,
 * since only its projection names one, and so are its marks (`edited`, `deleted`), which come from
 * the read that named it. What the subtree reads changes only where a part of it does, so a page
 * that publishes the same references again, in sets made anew, draws no row again: the one value
 * this hands on is the store's own wherever the store already holds every reference the page's
 * holds.
 */
export function PublishedReferenceNames({
  children,
  published,
}: {
  children: ReactNode;
  published: PublishedReferences;
}): ReactNode {
  const known = useContext(ReferenceNamesContext);
  const { ambiguous, asked, gone, unnamed } = published;
  const own = useMemo<KnownReferences>(() => {
    const value = {
      ambiguous: including(known.ambiguous, ambiguous),
      asked: including(known.asked, asked),
      deleted: known.deleted,
      edited: known.edited,
      gone: including(known.gone, gone),
      lines: known.lines,
      names: known.names,
      unnamed: including(known.unnamed, unnamed),
    };
    const kept =
      value.ambiguous === known.ambiguous &&
      value.asked === known.asked &&
      value.gone === known.gone &&
      value.unnamed === known.unnamed;
    return kept ? known : value;
  }, [ambiguous, asked, gone, known, unnamed]);
  return <ReferenceNamesContext.Provider value={own}>{children}</ReferenceNamesContext.Provider>;
}

/** Everything the page published, with the names its readings projected to: what the
 *  plain-text projection reads before it mounts a read of its own (`plain-text.tsx`), and what a
 *  row of the new issue page draws its references by (`issue-page/RowText.tsx`). */
export function useKnownReferences(): KnownReferences {
  return useContext(ReferenceNamesContext);
}

/** Every reference among the page's names whose post its author edited, a comment, a message or
 *  a block of either, empty where the page published none. A surface that draws such a
 *  reference's name cuts the name to its own length and then marks it ` · edited`, so no cut
 *  takes the mark: in one string by `editedName`, as a row's link does (`row-references.ts`), or
 *  as an element of its own by `EditedMark`, as the row's chip does with its own name
 *  (`NodeRow.tsx`'s `Chip`). */
export function useKnownEditedReferences(): ReadonlySet<string> {
  return useContext(ReferenceNamesContext).edited;
}

/** Every reference among the page's names whose comment or message its author deleted, by the
 *  delete, empty where the page published none. The page names each by its deleted line, which
 *  holds the actor as no registry knows it (`actorLabelText`); the issue page's rows draw the
 *  line from the delete instead, the actor named as the page names an author (`RowText.tsx`). */
export function useKnownDeletedReferences(): ReadonlyMap<string, PostDeletion> {
  return useContext(ReferenceNamesContext).deleted;
}
