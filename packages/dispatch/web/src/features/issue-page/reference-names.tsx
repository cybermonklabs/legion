import { createContext, type ReactNode, useContext, useEffect, useId, useReducer } from "react";

/**
 * The names a page has already read for the references it draws, shared with every surface the
 * shell mounts beside it.
 *
 * A link to a block shows the block's own words, and reading them one link at a time is one
 * `GET /api/v1/references` per pointer - the read `GET /api/v1/reference-targets` makes once for
 * them all (its entry in `packages/envoy/internal/dispatch/api/routes_table.go`). A page that has
 * made that batched read publishes its answer here, and every link under the shell reads its own
 * name from it instead of asking for it again. A page that publishes nothing leaves every link
 * exactly as it was, reading its own target.
 *
 * The store sits above the router, beside the margin's slot (`margin-slot.tsx`),
 * because the margin is mounted beside the router and its cards draw the same references as the
 * page's own column: a context inside the page would not reach them.
 *
 * More than one surface can publish at once: the page's own column and a sheet that draws the
 * outline beside it each read the tree and publish what they read. The store keeps one value per
 * publisher and hands readers the union of every value still published (`merged`), so a surface
 * that unmounts takes only its own value with it, and every link keeps the names the surfaces
 * still mounted read.
 */

/** What a page publishes from its one batched read: the name of each target it read, by
 *  reference, and the references it read as ambiguous (`useKnownAmbiguousReferences`). The store
 *  holds the two as one value per publisher, so a publisher's two always come from one read and
 *  change together. */
interface PublishedReferences {
  readonly ambiguous: ReadonlySet<string>;
  readonly names: ReadonlyMap<string, string>;
}

/** No name at all: what a page that published nothing has read. */
const NO_NAMES: ReadonlyMap<string, string> = new Map();

/** No ambiguous reference: what a page that published none has read. */
const NO_AMBIGUOUS: ReadonlySet<string> = new Set();

/** What a reader sees while no page publishes, and outside the provider: no name and no
 *  ambiguous reference. */
const NOTHING_PUBLISHED: PublishedReferences = { ambiguous: NO_AMBIGUOUS, names: NO_NAMES };

/** One publisher's change, handed to the store: what it publishes now, under its own key
 *  (`useId`), or `undefined` as it leaves. */
interface Publication {
  readonly published: PublishedReferences | undefined;
  readonly publisher: string;
}

/** What the store holds: every value still published, by its publisher's key, in the order the
 *  publishers last published, and the one value readers see, merged from them (`merged`). The
 *  merged value is made anew only when a publisher publishes or leaves, so a reader is handed
 *  the same map and set on every render in between, and re-renders only then. */
interface Store {
  readonly publications: ReadonlyMap<string, PublishedReferences>;
  readonly value: PublishedReferences;
}

/** No publisher at all: what the store holds before any page publishes and after the last one
 *  leaves. */
const NO_PUBLICATIONS: Store = { publications: new Map(), value: NOTHING_PUBLISHED };

const ReferenceNamesContext = createContext<PublishedReferences>(NOTHING_PUBLISHED);

/** Where a page publishes what it read, and withdraws it again. A page outside the provider
 *  publishes to nobody. */
const ReferenceNamesRegistry = createContext<(publication: Publication) => void>(() => {});

/**
 * Every value still published as the one value readers see. The names are merged, and where two
 * publishers name one reference, the name of the one that published last stands: a publisher
 * publishes again each time its read answers anew, so the last to publish holds the newest
 * answer. The ambiguous references are the union of every publisher's. A publisher whose map or
 * set is empty adds nothing to it, so where one publisher alone has names, or ambiguous
 * references, readers get its own map or set as it published it, and no copy is made.
 */
function merged(publications: ReadonlyMap<string, PublishedReferences>): PublishedReferences {
  if (publications.size === 0) return NOTHING_PUBLISHED;
  const published = [...publications.values()];
  const names = published.map((value) => value.names).filter((map) => map.size > 0);
  const ambiguous = published.map((value) => value.ambiguous).filter((set) => set.size > 0);
  return {
    ambiguous:
      ambiguous.length > 1
        ? new Set(ambiguous.flatMap((set) => [...set]))
        : (ambiguous[0] ?? NO_AMBIGUOUS),
    names: names.length > 1 ? new Map(names.flatMap((map) => [...map])) : (names[0] ?? NO_NAMES),
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
  return { publications, value: merged(publications) };
}

export function ReferenceNamesProvider({ children }: { children: ReactNode }): ReactNode {
  const [{ value }, publish] = useReducer(withPublication, NO_PUBLICATIONS);
  return (
    <ReferenceNamesRegistry.Provider value={publish}>
      <ReferenceNamesContext.Provider value={value}>{children}</ReferenceNamesContext.Provider>
    </ReferenceNamesRegistry.Provider>
  );
}

/**
 * Publishes `names`, and `ambiguous` beside them, for as long as the calling page is mounted,
 * and withdraws them again when the reader leaves, leaving every other publisher's value as it
 * was. Both must be stable across renders (`useMemo`): the store holds them in state, so a fresh
 * map or set every render would register on every commit.
 */
export function usePublishReferenceNames(
  names: ReadonlyMap<string, string> | undefined,
  ambiguous: ReadonlySet<string> = NO_AMBIGUOUS
): void {
  const register = useContext(ReferenceNamesRegistry);
  // The key this publisher's value is held under, the same for as long as it is mounted, so its
  // cleanup withdraws its own value and no other.
  const publisher = useId();
  useEffect(() => {
    register({ published: { ambiguous, names: names ?? NO_NAMES }, publisher });
    return () => register({ published: undefined, publisher });
  }, [ambiguous, names, publisher, register]);
}

/** The name the page read for `reference`, or `undefined` where it read none and the caller
 *  must read its own. */
export function useKnownReferenceName(reference: string | undefined): string | undefined {
  const { names } = useContext(ReferenceNamesContext);
  return reference === undefined ? undefined : names.get(reference);
}

/** Every name the page read, by reference, for a surface that names what its own words cite
 *  from the page's one batched read rather than asking for each (the issue page's rows,
 *  `row-references.ts`). Empty where the page published none. */
export function useKnownReferenceNames(): ReadonlyMap<string, string> {
  return useContext(ReferenceNamesContext).names;
}

/** Every reference the page read as ambiguous, empty where the page published none. Such a
 *  reference writes short an id two or more targets carry, so it names none of them, and a
 *  surface draws it as its author wrote it and links nothing. A reference with no name
 *  (`useKnownReferenceName`) and not in this set reads its own: a gone one, or one the page did
 *  not read. */
export function useKnownAmbiguousReferences(): ReadonlySet<string> {
  return useContext(ReferenceNamesContext).ambiguous;
}
