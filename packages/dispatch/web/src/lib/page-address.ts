import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type {
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { createElement, type ReactElement, useCallback, useMemo } from "react";
import {
  Navigate,
  type NavigationType,
  useLocation,
  useNavigate,
  useNavigationType,
  useSearchParams,
} from "react-router-dom";

/**
 * The one module that reads and writes the page address.
 *
 * Every page keeps its own parameter names - the Inbox's `?assignee=`, the project's `?label=`,
 * the Agents page's `?machine=`, the Delivery page's facets, the priorities page's `?p.sort=` -
 * but the pages this module serves do not reach for the address themselves. In the seven
 * features `components/one-definition.test.ts` names in `ADDRESS_FEATURES` - `agents`,
 * `artifacts`, `delivery`, `inbox`, `priorities`, `project` and `search` - that test refuses
 * `useSearchParams`, `useLocation`, React Router's own `<Navigate>` (a page there redirects
 * through `AddressRedirect`), a hand-built `URLSearchParams`, a `navigate` or a `<Link>` to a
 * query of the page's own, the browser's own location (`window.location`, the bare `location`
 * and its other names), and `history.pushState` or `history.replaceState`, so for those pages
 * the address is read and written here alone and there is one rule for how a parameter is read,
 * how it is written, and what happens to the browser's history when it changes.
 * `features/architecture/` is outside that list and still reads the router itself
 * (`ArchitecturePage.tsx`'s own `useSearchParams`); a page moved onto this module joins the
 * list, and the next one is held to it from its first line.
 *
 * This module is the machinery every page shares, and it imports nothing from `features/`. A
 * page's own grammar - its parameter names and the words they take - lives beside the page,
 * written with the parameter kinds below: the Inbox's route in `features/inbox/inbox-route.ts`,
 * the Agents page's filters in `features/agents/agent-search.ts`, and the project's in
 * `features/project/issue-filters.ts`.
 *
 * The rules this module holds every page to:
 *
 * - **A write replaces the history entry.** Trying filters out must not fill the Back button:
 *   a reader who picks four facets and then presses Back expects to leave the page, not to walk
 *   back through four picks, and a reader typing in a search box would otherwise push one entry
 *   per letter. Every write below passes `{ replace: true }`. The one way to push an entry is
 *   `useAddressNavigate`'s explicit `{ replace: false }`, for a move that has always pushed:
 *   picking an artifact's version, so Back returns to the version the reader came from, the
 *   move off a document the reader has just removed, taking off the List's chip of the issues a
 *   link names (`only=`), so Back gives that narrowing back, and taking off the Inbox's agent
 *   chip, so Back gives the agent's asks back. A whole page load (`loadWholePage`) leaves an
 *   entry behind as following a link does, so Back returns to the page the reader left.
 * - **A parameter is absent until the reader sets it.** A control at its default writes nothing,
 *   so a shared link carries only what was changed and an address that arrives naming nothing is
 *   the page as it opens.
 */

/** The address, as the page's live `URLSearchParams`. The object's identity changes only when
 *  the address does, so a `useMemo` keyed on it recomputes exactly as often as the address
 *  moves. */
export function useAddressParams(): URLSearchParams {
  const [params] = useSearchParams();
  return params;
}

/** The whole address as a route parser reads it: the path, the query and the fragment, each as
 *  the browser spells it. */
export interface PageAddress {
  readonly hash: string;
  readonly pathname: string;
  readonly search: string;
}

/** The address the page is on, for a caller that reads the path as well as the query: the
 *  route parsers of `features/refs/routes.ts` take the two strings, and `addressWithParam`
 *  takes the three. The router's own object is handed back as it stands, so its identity
 *  changes only when the address does. */
export function usePageAddress(): PageAddress {
  return useLocation();
}

/** What the navigation that opened this page carried with it, which travels on the router's
 *  location rather than in the address: the broadcast page's list of the recipients a send
 *  left out, which no address holds because nobody may link to it. Read here with the address
 *  so that no page binds the router for itself. */
export function useNavigationState(): unknown {
  return useLocation().state;
}

/** The history entry the page stands on, as the last render read it, and the way back off it. */
export interface HistoryEntry {
  /** One step back through the history, as the browser's Back takes it (`navigate(-1)`): below
   *  1280 px the Inbox's reading view leaves through the entry `open` pushed over the list's own,
   *  rather than writing the list a second entry the next Back would only land on again. */
  readonly back: () => void;
  /** The entry's own key, a new one for every entry the history is given: the reading view
   *  tells by it whether the back step it started has landed yet, since until it does the
   *  address still names the row the reader closed. */
  readonly key: string;
  /** How the page came onto this entry: `POP` for the browser's Back or Forward and for the
   *  first load, `PUSH` or `REPLACE` for a navigation of the page's own. A `POP` answers the
   *  browser's Back: beside the list, the Inbox's column reads one onto an entry that names no
   *  row as the reader's own way back, and takes its first row by itself. */
  readonly navigationType: NavigationType;
  /** What the entry holds besides its address, as `useNavigationState` reads it: the Inbox's
   *  marks on its open row (`features/inbox/pane-selection.ts`, `inboxEntryState`). */
  readonly state: unknown;
}

/**
 * The history entry the page stands on, for a page that keeps something of its own in the entry
 * and steps back through the history itself: the Inbox's reading selection
 * (`features/inbox/pane-selection.ts`). A page that needs only what a navigation carried reads
 * `useNavigationState`. The object's identity changes only when one of its fields does.
 */
export function useHistoryEntry(): HistoryEntry {
  const { key, state } = useLocation();
  const navigationType = useNavigationType();
  const navigate = useNavigate();
  const back = useCallback(() => navigate(-1), [navigate]);
  return useMemo(() => ({ back, key, navigationType, state }), [back, key, navigationType, state]);
}

/** The one write: `rewrite` is handed a copy of the live parameters and returns what the
 *  address should hold. It may mutate the copy and return it, or return a fresh set. */
export type AddressUpdate = (rewrite: (params: URLSearchParams) => URLSearchParams) => void;

/**
 * The one write of the page address, always `{ replace: true }` (see this module's head: a
 * filter the reader is trying out must not fill the browser's history). The pathname is left as
 * it is and only the query changes; the hash goes, since React Router's `setSearchParams`
 * navigates to the new query alone.
 */
export function useAddressUpdate(): AddressUpdate {
  const [, setSearchParams] = useSearchParams();
  return useCallback(
    (rewrite: (params: URLSearchParams) => URLSearchParams) => {
      setSearchParams((current) => rewrite(copyAddress(current)), { replace: true });
    },
    [setSearchParams]
  );
}

/** How `useAddressNavigate` writes an address: `replace`, true unless the caller pushes, and the
 *  history entry's `state`, which a page that keeps something of its own in the entry passes on
 *  (the Inbox's open row, `features/inbox/pane-selection.ts`). */
export interface AddressNavigateOptions {
  readonly replace?: boolean;
  readonly state?: unknown;
}

/**
 * Leaving this address for another whole one a route builder made - the version picker's
 * `?v=`, the Inbox's rebuilt query string. A page hands over a path rather than calling the
 * router itself, so `replace` stays this module's decision: `replace` is the default here, as
 * it is for every other write, and a caller that means to leave a history entry behind says so.
 */
export function useAddressNavigate(): (path: string, options?: AddressNavigateOptions) => void {
  const navigate = useNavigate();
  return useCallback(
    (path: string, options?: AddressNavigateOptions) => {
      navigate(path, { replace: options?.replace ?? true, state: options?.state });
    },
    [navigate]
  );
}

/**
 * Leaving the app for `href` with a whole page load (`window.location.assign`), as the browser
 * follows a plain link the router does not handle. Below 1280 px the Inbox's `o` on a thread row
 * goes to the thread's own page this way (`features/inbox/Inbox.tsx`): the row's title is a
 * plain `<a href>` whose plain click opens the row in place instead (`ThreadTitle`'s `onOpen`),
 * and a script's click is a plain one, so `o` follows the link itself, as a modified click
 * would. Held here with every other move off the address, so no page reaches for the browser's
 * location of its own.
 */
export function loadWholePage(href: string): void {
  window.location.assign(href);
}

/** Where `AddressRedirect` sends the page, and whether the page's own query goes with it. */
export interface AddressRedirectProps {
  /** Carry the page's query onto `to`, which is then a bare pathname: the bare project path
   *  leaving for its Issues with the filters a link named. Without it `to` is the whole address
   *  the page lands on, its query and hash exactly as the caller wrote them. */
  readonly keepQuery?: boolean;
  readonly to: string;
}

/**
 * Leaving this address for another as the page renders, rather than from a handler: the bare
 * project path opening on its Architecture or its Issues, a document address rewritten with the
 * whole id a short one stood for. Like every write here that does not ask to push, it replaces
 * the history entry, so Back leaves for the page the reader came from instead of returning to an
 * address that only sends them on again. It is React Router's `<Navigate>`, which no page of the
 * seven features draws for itself.
 */
export function AddressRedirect({ keepQuery = false, to }: AddressRedirectProps): ReactElement {
  const { search } = usePageAddress();
  return createElement(Navigate, {
    replace: true,
    to: keepQuery ? { pathname: to, search } : to,
  });
}

/** A `URLSearchParams` of its own over `init`: the live one the router hands out is never
 *  mutated, so every read and write here works on a copy. */
export function copyAddress(init?: string | URLSearchParams): URLSearchParams {
  return new URLSearchParams(init);
}

/** The address of `pathname` with `query` as its parameters, and `hash` after them: the one way
 *  a page joins a path to a query it built here, with no `?` while the query is empty. A link or
 *  a navigation to a built address takes this rather than joining the parts itself. */
export function addressPath(pathname: string, query: URLSearchParams, hash = ""): string {
  const search = query.toString();
  return `${pathname}${search === "" ? "" : `?${search}`}${hash}`;
}

/** `location` with `name` set to `value`, as a path `AddressRedirect` or a link can take: the
 *  pathname and the hash are kept exactly as they are, and every other parameter with them. */
export function addressWithParam(location: PageAddress, name: string, value: string): string {
  const params = copyAddress(location.search);
  params.set(name, value);
  return addressPath(location.pathname, params, location.hash);
}

/**
 * The address's cut: at most `SEARCH_QUERY_MAX` characters, ending on a whole code point. A cut
 * that lands between the halves of a surrogate pair would leave a lone high surrogate, so that
 * half is dropped rather than written. Shared rather than copied because the Inbox's own search
 * box applies the same cut in its change handler before it navigates, and two cuts worded apart
 * would drift into two rules for how long an address may be.
 */
export function capSearchQuery(next: string): string {
  const capped = next.slice(0, SEARCH_QUERY_MAX);
  return /[\uD800-\uDBFF]$/.test(capped) ? capped.slice(0, -1) : capped;
}

/**
 * One parameter's grammar: how a value is read out of the address and written back under a
 * name the page chooses. Declared as methods rather than function properties so a spec of
 * parameters of different types is still one object (`AddressSpec`).
 */
export interface AddressParam<T> {
  read(params: URLSearchParams, name: string): T;
  /** Writes `value` under `name`, deleting the parameter where `value` is the default: a
   *  control at its default says nothing in the address. */
  write(params: URLSearchParams, name: string, value: T): void;
}

/** A free-text parameter; the empty string is the default and leaves the address. */
export function textParam(): AddressParam<string> {
  return {
    read: (params, name) => params.get(name) ?? "",
    write: (params, name, value) => {
      if (value === "") params.delete(name);
      else params.set(name, value);
    },
  };
}

/**
 * A free-text parameter cut at `SEARCH_QUERY_MAX` on the way in, because the page's address is
 * where a search lives and the load balancer refuses a URL past 16 K on reload or a shared
 * link. What arrived in the address is read back as it arrived: a link carrying a longer value
 * is left alone rather than rewritten behind the reader.
 */
export function cappedTextParam(): AddressParam<string> {
  return {
    read: (params, name) => params.get(name) ?? "",
    write: (params, name, value) => {
      if (value === "") params.delete(name);
      else params.set(name, capSearchQuery(value));
    },
  };
}

/** `values` as `commaListParam` writes them into its one parameter: joined by commas, with a
 *  comma or a backslash inside a value written escaped (`\,` and `\\`). A page that bounds the
 *  list a reader picks measures this, which is what the address holds. */
export function commaListText(values: readonly string[]): string {
  return values.map((value) => value.replace(/[\\,]/g, (char) => `\\${char}`)).join(",");
}

/** Several values in one parameter, comma-separated (`?machine=relay-1,relay-2`): what a
 *  multi-select writes where one link has always named one value. A comma or a backslash inside
 *  a value is written escaped (`commaListText`), and the read splits on each comma no backslash
 *  escapes and takes only `\,` and `\\` as escapes, so every value reads back whole, while an
 *  address that holds neither reads as it always has: any other backslash is itself, and an
 *  empty item is dropped. An empty list leaves the address, so unticking the last pick does not
 *  bring the filter back on reload. Nothing is cut, at any length: a page keeps the lists its
 *  reader picks under `SEARCH_QUERY_MAX`, since the load balancer refuses a URL past 16 K on
 *  reload or a shared link, by refusing the pick that would take one past it (the Agents page's
 *  `AgentFilterBar`). */
export function commaListParam(): AddressParam<readonly string[]> {
  return {
    read: (params, name) =>
      (params.get(name)?.match(/(?:\\[\\,]|[^,])+/g) ?? []).map((value) =>
        value.replace(/\\([\\,])/g, "$1")
      ),
    write: (params, name, value) => {
      if (value.length === 0) params.delete(name);
      else params.set(name, commaListText(value));
    },
  };
}

/** Several values as a repeated parameter (`?label=a&label=b`), in the order they are written.
 *  An empty list leaves the address. */
export function repeatedParam(): AddressParam<readonly string[]> {
  return {
    read: (params, name) => params.getAll(name),
    write: (params, name, value) => {
      params.delete(name);
      for (const one of value) params.append(name, one);
    },
  };
}

/** A switch, `?<name>=1` while it is on and absent while it is off. */
export function flagParam(): AddressParam<boolean> {
  return {
    read: (params, name) => params.get(name) === "1",
    write: (params, name, value) => {
      if (value) params.set(name, "1");
      else params.delete(name);
    },
  };
}

/** One value out of a fixed set, `fallback` while the address names none. A word this build
 *  does not know reads as the fallback rather than refusing the address: an older link still
 *  opens the page. Writing the fallback leaves the address. */
export function choiceParam<T extends string>(values: readonly T[], fallback: T): AddressParam<T> {
  return {
    read: (params, name) => {
      const value = params.get(name);
      return values.find((one) => one === value) ?? fallback;
    },
    write: (params, name, value) => {
      if (value === fallback) params.delete(name);
      else params.set(name, value);
    },
  };
}

/** A whole number, `fallback` while the address names none or names something that is not one.
 *  Writing the fallback leaves the address. */
export function numberParam(fallback: number): AddressParam<number> {
  return {
    read: (params, name) => {
      const value = Number(params.get(name));
      return params.has(name) && Number.isSafeInteger(value) ? value : fallback;
    },
    write: (params, name, value) => {
      if (value === fallback) params.delete(name);
      else params.set(name, String(value));
    },
  };
}

/** A page's parameters, each under the name the page spells it with. A spec is expected to be a
 *  module-level constant: its own identity is a dependency of the memo below, so a spec built
 *  in a render would hand every render new values. */
export type AddressSpec = Readonly<Record<string, AddressParam<unknown>>>;

/** What a spec reads out of the address: one field per parameter, typed by its own grammar. */
export type AddressValues<S extends AddressSpec> = {
  [K in keyof S]: S[K] extends AddressParam<infer T> ? T : never;
};

/** Writes each field `values` holds into `params` through its own parameter's `write`, under the
 *  name `spec` gives it, and hands `params` back; a parameter whose field `values` leaves out is
 *  left as it stands. The one way a spec's fields reach a query: `useAddressFields`'s setter
 *  writes through it, and so does `fieldsAddress`, the link to a page. */
export function writeAddressFields<S extends AddressSpec>(
  params: URLSearchParams,
  spec: S,
  values: Partial<AddressValues<S>>
): URLSearchParams {
  const written = values as Record<string, unknown>;
  for (const [name, param] of Object.entries(spec)) {
    if (name in written) param.write(params, name, written[name]);
  }
  return params;
}

/** The address of `pathname` narrowed by `values` and by nothing else, written through `spec`:
 *  a link to a page is the address that page's own setter writes for the same narrowing,
 *  encoded by the same parameters. `null` where that address does not read back through `spec`
 *  as exactly `values`, field by field: a `cappedTextParam` value past `SEARCH_QUERY_MAX`, which
 *  it cuts, or an empty item in a `commaListParam`, which its read drops, would point the link
 *  at another narrowing than the one it names, so the caller draws no link rather than one that
 *  opens a different page in silence. A comma list carries every other value whole, commas and
 *  backslashes included, at any length: the bound a page holds its lists to is on the reader's
 *  own picks (`commaListParam`), not on a link to it. */
export function fieldsAddress<S extends AddressSpec>(
  pathname: string,
  spec: S,
  values: Partial<AddressValues<S>>
): string | null {
  const params = writeAddressFields(copyAddress(), spec, values);
  const given = values as Record<string, unknown>;
  for (const [name, param] of Object.entries(spec)) {
    if (name in given && !sameAddressValue(param.read(params, name), given[name])) return null;
  }
  return addressPath(pathname, params);
}

/** Whether a value a parameter read back is the one it was given: the same primitive, or lists
 *  of the same length whose items are the same, in the same order. Every parameter kind reads a
 *  primitive or a flat `readonly string[]`, so a list's items are compared as primitives. */
function sameAddressValue(read: unknown, given: unknown): boolean {
  if (Array.isArray(read) && Array.isArray(given)) {
    return read.length === given.length && read.every((one, index) => one === given[index]);
  }
  return read === given;
}

/**
 * A page's whole narrowing, read from and written to the address under the page's own names:
 * the values as they stand, and one setter that takes the parameters that changed and leaves
 * the rest of the address alone. Several parameters in one call are one write, so a control
 * that moves two of them at once replaces one history entry rather than two.
 */
export function useAddressFields<S extends AddressSpec>(
  spec: S
): readonly [AddressValues<S>, (next: Partial<AddressValues<S>>) => void] {
  const params = useAddressParams();
  const update = useAddressUpdate();
  // Memoized on the address's own identity, so two renders of one address return the same
  // arrays: a consumer that keys a query or a memo off a list of values would otherwise see
  // every render disagree with the last.
  const values = useMemo(() => {
    const read: Record<string, unknown> = {};
    for (const [name, param] of Object.entries(spec)) read[name] = param.read(params, name);
    return read as AddressValues<S>;
  }, [params, spec]);
  const set = useCallback(
    (next: Partial<AddressValues<S>>) =>
      update((current) => writeAddressFields(current, spec, next)),
    [spec, update]
  );
  return [values, set] as const;
}

/** One parameter of a page, read and written under `name`. `param` is expected to be a
 *  module-level constant, for the reason `useAddressFields` gives. */
export function useAddressParam<T>(
  name: string,
  param: AddressParam<T>
): readonly [T, (next: T) => void] {
  const params = useAddressParams();
  const update = useAddressUpdate();
  const value = useMemo(() => param.read(params, name), [name, param, params]);
  const set = useCallback(
    (next: T) =>
      update((current) => {
        param.write(current, name, next);
        return current;
      }),
    [name, param, update]
  );
  return [value, set] as const;
}

const CAPPED_TEXT = cappedTextParam();

/** A free-text `?<name>=` parameter, cut at `SEARCH_QUERY_MAX` on the way in
 *  (`cappedTextParam`): the Artifacts tab's `?q=` and the machine page's `?window=` go through
 *  this hook; a page with more than one parameter reads them through `useAddressFields`. */
export function useCappedAddressParam(name: string): readonly [string, (next: string) => void] {
  return useAddressParam(name, CAPPED_TEXT);
}

/** Every value of each repeatable `?<name>=` parameter in `keys`, memoized on the live address's
 *  identity so two renders of the same address return the exact same arrays — without this,
 *  `getAll` mints a fresh array every render, and a consumer that keys a `useMemo` or a query
 *  off the result (the Delivery page's live view) never sees two renders agree, recomputing or
 *  refetching on every render instead of only when the address changes. `keys` is expected to be
 *  a module-level constant, since its own identity is a dependency of the memo. */
export function useRepeatableAddressParams<K extends string>(
  keys: readonly K[]
): Record<K, string[]> {
  const params = useAddressParams();
  return useMemo(() => {
    const result = {} as Record<K, string[]>;
    for (const key of keys) result[key] = params.getAll(key);
    return result;
  }, [keys, params]);
}

/** A table's view as the address holds it: what it sorts, groups, filters, searches and pages
 *  by. `page` is counted from 0, as TanStack counts it; the address counts from 1. */
export interface TableAddressView {
  sorting: SortingState;
  grouping: GroupingState;
  filters: ColumnFiltersState;
  search: string;
  page: number;
  pageSize: number;
}

/** How one table's view is spelled in the address. A page with two tables gives each its own
 *  grammar, under its own prefix, so neither reads the other's parameters. A table whose view
 *  holds more than the shared fields - the project's issue table, with the issues a link names
 *  - reads and writes its own `V`, which every setter below carries through untouched. */
export interface TableAddressGrammar<V extends TableAddressView = TableAddressView> {
  read(params: URLSearchParams): V;
  write(params: URLSearchParams, view: V): URLSearchParams;
}

/** How the default grammar spells one table's parameters. */
export interface TableAddressNames {
  /** What every one of this table's parameters is named under, so two tables on one page keep
   *  their own: `sends` makes `?sends.sort=`, `?sends.q=`, `?sends.f.status=`. With no prefix
   *  the names are the bare ones a single-table page has always written. */
  prefix?: string;
  /** The page size the table opens with, which is left out of the address. */
  pageSize?: number;
}

const TABLE_SORT = textParam();
const TABLE_GROUP = textParam();
/** Written as it was read, not cut: `setSearch` below cuts what the reader types, and every
 *  write rewrites the whole view, so a cut here would shorten a longer `?q=` a link carried as
 *  soon as the reader sorted or paged. */
const TABLE_SEARCH = textParam();
const TABLE_FILTER = repeatedParam();

/**
 * The address grammar of an ordinary `DataTable`: `sort` (the sorted columns, `-` for
 * descending), `group`, `q`, `page` (counted from 1), `size`, and one repeatable `f.<columnId>`
 * per column filter - each under `prefix` where the page gives one. Every one is absent while
 * it holds its default.
 *
 * A page whose table has a vocabulary of its own writes its own grammar and hands it to
 * `useTableAddress` instead: the project's issue table does, because its `?status=`,
 * `?unclaimed=` and `?nofilter=` are the project's own words and its open statuses are a
 * default a link leaves out (`features/project/issues-table-model.ts`), and so does the
 * priorities table, which opens in its one order and leaves that sort out of a link
 * (`features/priorities/priority-table-view.ts`).
 */
export function tableAddressGrammar(names: TableAddressNames = {}): TableAddressGrammar {
  const prefix = names.prefix === undefined || names.prefix === "" ? "" : `${names.prefix}.`;
  const defaultPageSize = names.pageSize ?? 25;
  const sortName = `${prefix}sort`;
  const groupName = `${prefix}group`;
  const searchName = `${prefix}q`;
  const pageName = `${prefix}page`;
  const sizeName = `${prefix}size`;
  const filterPrefix = `${prefix}f.`;
  const page = numberParam(1);
  const size = numberParam(defaultPageSize);
  return {
    read: (params) => ({
      sorting: TABLE_SORT.read(params, sortName)
        .split(",")
        .filter((part) => part !== "" && part !== "-")
        .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") })),
      grouping: TABLE_GROUP.read(params, groupName) === "" ? [] : [params.get(groupName) ?? ""],
      filters: [...new Set(params.keys())]
        .filter((key) => key.startsWith(filterPrefix))
        .map((key) => ({ id: key.slice(filterPrefix.length), value: params.getAll(key) })),
      search: TABLE_SEARCH.read(params, searchName),
      page: Math.max(page.read(params, pageName) - 1, 0),
      pageSize: Math.max(size.read(params, sizeName), 1),
    }),
    write: (params, view) => {
      const next = copyAddress(params);
      for (const key of [...next.keys()]) {
        if (key.startsWith(filterPrefix)) next.delete(key);
      }
      TABLE_SORT.write(
        next,
        sortName,
        view.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",")
      );
      TABLE_GROUP.write(next, groupName, view.grouping[0] ?? "");
      for (const filter of view.filters) {
        const values = Array.isArray(filter.value) ? filter.value : [filter.value];
        TABLE_FILTER.write(
          next,
          `${filterPrefix}${filter.id}`,
          values.map((value) => String(value))
        );
      }
      TABLE_SEARCH.write(next, searchName, view.search);
      page.write(next, pageName, view.page + 1);
      size.write(next, sizeName, view.pageSize);
      return next;
    },
  };
}

/** A table's view in the address, in the shape `useDataTable` takes it: the state it renders
 *  and the `on…Change` handlers it calls. */
export interface TableAddress<V extends TableAddressView = TableAddressView> {
  /** The whole address, which the table takes as its view key: a view it has to rebuild from
   *  scratch is exactly one whose address changed. */
  params: URLSearchParams;
  view: V;
  setSorting: (update: SortingState | ((current: SortingState) => SortingState)) => void;
  setGrouping: (update: GroupingState | ((current: GroupingState) => GroupingState)) => void;
  setFilters: (
    update: ColumnFiltersState | ((current: ColumnFiltersState) => ColumnFiltersState)
  ) => void;
  setSearch: (search: string) => void;
  setPagination: (
    update: PaginationState | ((current: PaginationState) => PaginationState)
  ) => void;
}

/**
 * The table's view — what it sorts, groups, filters, searches and pages by — read from and
 * written to the address through `grammar`, so a shared link opens the same table and two
 * tables under two prefixes keep their own state. Every setter has the shape TanStack's
 * `on…Change` handlers expect, and every change but the page itself returns to the first page:
 * a filter that leaves fewer rows than the page the reader was on would otherwise show nothing.
 *
 * The table core (`components/DataTable.tsx`) reads no address at all: this hook is the page's,
 * and what it returns is handed to `useDataTable` as plain state and setters.
 */
export function useTableAddress<V extends TableAddressView>(
  grammar: TableAddressGrammar<V>
): TableAddress<V> {
  const params = useAddressParams();
  const update = useAddressUpdate();
  const view = useMemo(() => grammar.read(params), [grammar, params]);
  const updateView = (next: (current: V) => V) =>
    update((current) => grammar.write(current, next(grammar.read(current))));
  return {
    params,
    view,
    setSorting: (update) =>
      updateView((current) => ({
        ...current,
        sorting: typeof update === "function" ? update(current.sorting) : update,
        page: 0,
      })),
    setGrouping: (update) =>
      updateView((current) => ({
        ...current,
        grouping: typeof update === "function" ? update(current.grouping) : update,
        page: 0,
      })),
    setFilters: (update) =>
      updateView((current) => ({
        ...current,
        filters: typeof update === "function" ? update(current.filters) : update,
        page: 0,
      })),
    setSearch: (search) =>
      updateView((current) => ({ ...current, search: capSearchQuery(search), page: 0 })),
    setPagination: (update) =>
      updateView((current) => {
        const next =
          typeof update === "function"
            ? update({ pageIndex: current.page, pageSize: current.pageSize })
            : update;
        return { ...current, page: next.pageIndex, pageSize: next.pageSize };
      }),
  };
}
