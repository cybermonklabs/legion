import {
  type DefaultError,
  type InfiniteData,
  infiniteQueryOptions,
  type QueryClient,
  type QueryFunction,
  queryOptions,
  type UndefinedInitialDataInfiniteOptions,
  type UndefinedInitialDataOptions,
} from "@tanstack/react-query";

import { writesOf } from "../features/writes/write-store";

/**
 * The one factory every cache read of a row the page writes is built by (R7 of
 * dispatch://INFRA-299): it derives from the key's own head the lines that key covers and holds
 * the fetch until the store holds no write of them that may still land (`Writes.settled`), so a
 * reader written later takes the wait without naming it. A key head this file has no rule for
 * does not compile - the head is the union `RowQueryHead` below, which `COVERS` maps whole - so a
 * reader of a new row says here what its key covers rather than missing the wait in silence, and
 * `api/row-query.test.ts` names every query the app makes that is built outside this factory with
 * the reason no optimistic write touches it.
 *
 * The wait has no ceiling: a write that stays late holds the readers of its row for as long as it
 * is tried, and each of them shows the data it last had. That is the cost of never showing a
 * person their own change undone.
 */

/**
 * The lines each key head covers: a key covering one row waits for that row's lines, a list for
 * the lines of the rows it lists. Every rule reads the key it is given, because a head serves
 * every key under it.
 *
 * A read mark (`read:issue:<KEY>`) holds nothing anywhere: it is `read:` and then the row's own
 * name, so no line here is a prefix of it, and it changes no value a person chose.
 */
const COVERS = {
  /** The viewer's agent tokens: the mint that adds a row to the list (`agent-tokens:me`) and
   *  each token's own scopes and revoke (`agent-token:<id>`). */
  "agent-tokens": () => ["agent-token:", "agent-tokens:"],

  /** One project's architecture tree, which its sources are read through
   *  (`architecture-source:<project>/<owner/repo>`, `sourceLine`). */
  architecture: (key: readonly unknown[]) => [`architecture-source:${String(key[1])}/`],

  /** The architecture sources settings list: every source, or one project's
   *  (`["architecture-sources", <project>]`). */
  "architecture-sources": (key: readonly unknown[]) =>
    key.length > 1 ? [`architecture-source:${String(key[1])}/`] : ["architecture-source:"],

  /** A document's own comments list (`["artifact", <id>, "comments"]`), the margin's list under
   *  the document's key: the only `["artifact", …]` read this factory builds. */
  artifact: () => ["comment:"],

  /** One ask as a reference preview reads it (`["ask", <id>]`). */
  ask: (key: readonly unknown[]) => rowLines("ask", key[1]),

  /** One Inbox thread's ask, with its replies and its answer. */
  "ask-thread": (key: readonly unknown[]) => rowLines("ask", key[1]),

  /** One comment as a reference preview reads it (`["comment", <id>]`). */
  comment: (key: readonly unknown[]) => rowLines("comment", key[1]),

  /** The margin's and the Conversation's comments list of one owner: a comment id names no
   *  issue, so the list waits for any comment write. */
  comments: () => ["comment:"],

  /** The delivery timeline's configuration record (`settings:delivery`). */
  "delivery-settings": () => ["settings:delivery"],

  /** The Inbox's asks, every project's or one project's: an answer composed before a write of an
   *  issue or an ask would put the older row back on screen. */
  inbox: () => ["ask:", "issue:"],

  /** One issue with its details. */
  issue: (key: readonly unknown[]) => rowLines("issue", key[1]),

  /** A list of issue rows: a project's list (`["issues", "project", <PROJECT>]`), the reference
   *  picker's list of one project (`["issues", <PROJECT>]`), and the sidebar's pinned issues
   *  (`["issues", "pinned"]`), which every project's issues and every pin move. */
  issues: (key: readonly unknown[]) => {
    const project = key[1] === "project" ? key[2] : key[1];
    if (project === "pinned") return ["issue:", "pin:"];
    return typeof project === "string"
      ? [`issue:${project}-`, `project:${project}`]
      : ["issue:", "project:"];
  },

  /** The viewer's threads: the Inbox's own rows, what they have dismissed out of it, their
   *  count, and one owner's unread set - all of them ask and issue rows. */
  me: () => ["ask:", "issue:"],

  /** The repository-to-project mappings of the Settings page and the issue header's picker. */
  "repo-projects": () => ["repo-mapping:"],

  /** The viewer's per-issue read, pin and dismiss state, which the pin writes. */
  "user-state": () => ["pin:"],
} satisfies Record<string, (key: readonly unknown[]) => readonly string[]>;

/** The head of a key this factory has a rule for. A reader whose key starts with anything else
 *  does not compile. */
export type RowQueryHead = keyof typeof COVERS;

/** A cache key of a row the page writes: its head names the lines it covers. */
export type RowQueryKey = readonly [RowQueryHead, ...unknown[]];

/** The line of one row, or no line at all for a key whose row id is not there: such a read is
 *  held by its own `enabled` and its fetch throws, so there is nothing for it to wait for. */
function rowLines(type: string, id: unknown): readonly string[] {
  return typeof id === "string" && id !== "" ? [`${type}:${id}`] : [];
}

/** The lines `key` covers, by its head alone. */
export function linesOf(key: RowQueryKey): readonly string[] {
  return COVERS[key[0]](key);
}

/** What a held fetch reads from its context: the client whose store holds the writes, the key
 *  whose head names the lines, and the signal the wait ends on. */
interface HeldContext<TKey extends RowQueryKey> {
  readonly client: QueryClient;
  readonly queryKey: TKey;
  readonly signal: AbortSignal;
}

/** `fetch`, held until the store holds no write of the lines `context.queryKey` covers. A fetch
 *  nothing holds is made in the very task that started it, as a reader that names no wait is:
 *  one made a microtask later would reach the server after its caller moved on. */
function held<TQueryFnData, TKey extends RowQueryKey, TContext extends HeldContext<TKey>>(
  context: TContext,
  fetch: (context: TContext) => TQueryFnData | Promise<TQueryFnData>
): TQueryFnData | Promise<TQueryFnData> {
  const writes = writesOf(context.client);
  const lines = linesOf(context.queryKey);
  if (!writes.holding(lines)) return fetch(context);
  return writes.settled(lines, context).then(() => fetch(context));
}

/** One row read, waiting: `queryOptions` with the key's own wait around the fetch. */
export function rowQuery<
  TQueryFnData,
  TError = DefaultError,
  TData = TQueryFnData,
  TKey extends RowQueryKey = RowQueryKey,
>(
  options: Omit<UndefinedInitialDataOptions<TQueryFnData, TError, TData, TKey>, "queryFn"> & {
    readonly queryFn: QueryFunction<TQueryFnData, TKey>;
  }
) {
  const fetch = options.queryFn;
  return queryOptions<TQueryFnData, TError, TData, TKey>({
    ...options,
    queryFn: (context) => held(context, fetch),
  });
}

/** One paged row read, waiting: `infiniteQueryOptions` with the same wait. */
export function rowInfiniteQuery<
  TQueryFnData,
  TError = DefaultError,
  TPageParam = unknown,
  TKey extends RowQueryKey = RowQueryKey,
  TData = InfiniteData<TQueryFnData, TPageParam>,
>(
  options: Omit<
    UndefinedInitialDataInfiniteOptions<TQueryFnData, TError, TData, TKey, TPageParam>,
    "queryFn"
  > & {
    readonly queryFn: QueryFunction<TQueryFnData, TKey, TPageParam>;
  }
) {
  const fetch = options.queryFn;
  return infiniteQueryOptions<TQueryFnData, TError, TData, TKey, TPageParam>({
    ...options,
    queryFn: (context) => held(context, fetch),
  });
}
