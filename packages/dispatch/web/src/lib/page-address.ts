import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type {
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { createElement, type ReactElement, useCallback, useMemo } from "react";
import { Navigate, useLocation, useNavigate, useSearchParams } from "react-router-dom";

import { EVERYONE_FILTER_VALUE } from "../features/inbox/sections";
import {
  INBOX_FILTER_FIELDS,
  INBOX_GROUP_BYS,
  INBOX_PRIORITY_VALUES,
  INBOX_SORTS,
  INBOX_TYPE_VALUES,
  INBOX_URGENCY_VALUES,
  type InboxFilterField,
  type InboxGroupBy,
  type InboxSort,
} from "../features/refs/routes";

/**
 * The one module that reads and writes the page address.
 *
 * Every page keeps its own parameter names - the Inbox's `?assignee=`, the project's `?label=`,
 * the Agents page's `?machine=`, the Delivery page's facets - but the pages this module serves
 * do not reach for the address themselves. In the six features
 * `components/one-definition.test.ts` names in `ADDRESS_FEATURES` - `agents`, `artifacts`,
 * `delivery`, `inbox`, `project` and `search` - that test refuses `useSearchParams`,
 * `useLocation`, React Router's own `<Navigate>` (a page there redirects through
 * `AddressRedirect`), a hand-built `URLSearchParams`, `window.location` and
 * `history.pushState` or `history.replaceState`, so for those pages the address is read and
 * written here alone and there is one rule for how a parameter is read, how it is written, and
 * what happens to the browser's history when it changes. `features/architecture/` is outside
 * that list and still reads the router itself (`ArchitecturePage.tsx`'s own `useSearchParams`);
 * a page moved onto this module joins the list, and the next one is held to it from its first
 * line.
 *
 * The rules this module holds every page to:
 *
 * - **A write replaces the history entry.** Trying filters out must not fill the Back button:
 *   a reader who picks four facets and then presses Back expects to leave the page, not to walk
 *   back through four picks, and a reader typing in a search box would otherwise push one entry
 *   per letter. Every write below passes `{ replace: true }`. The one way to push an entry is
 *   `useAddressNavigate`'s explicit `{ replace: false }`, for a move the reader means to walk
 *   back out of - picking an artifact's version, which has always pushed.
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

/** The one write: `rewrite` is handed a copy of the live parameters and returns what the
 *  address should hold. It may mutate the copy and return it, or return a fresh set. */
export type AddressUpdate = (rewrite: (params: URLSearchParams) => URLSearchParams) => void;

/**
 * The one write of the page address, always `{ replace: true }` (see this module's head: a
 * filter the reader is trying out must not fill the browser's history). The pathname and the
 * hash are left exactly as they are; only the query changes.
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

/**
 * Leaving this address for another whole one a route builder made - the version picker's
 * `?v=`, the Inbox's rebuilt query string. A page hands over a path rather than calling the
 * router itself, so `replace` stays this module's decision: `replace` is the default here, as
 * it is for every other write, and a caller that means to leave a history entry behind says so.
 */
export function useAddressNavigate(): (path: string, options?: { replace?: boolean }) => void {
  const navigate = useNavigate();
  return useCallback(
    (path: string, options?: { replace?: boolean }) => {
      navigate(path, { replace: options?.replace ?? true });
    },
    [navigate]
  );
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
 * whole id a short one stood for. Like every other write here it replaces the history entry, so
 * Back leaves for the page the reader came from instead of returning to an address that only
 * sends them on again. It is React Router's `<Navigate>`, which no page of the six features
 * draws for itself.
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

/** `location` with `name` set to `value`, as a path `AddressRedirect` or a link can take: the
 *  pathname and the hash are kept exactly as they are, and every other parameter with them. */
export function addressWithParam(location: PageAddress, name: string, value: string): string {
  const params = copyAddress(location.search);
  params.set(name, value);
  return `${location.pathname}?${params.toString()}${location.hash}`;
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

/** Several values in one parameter, comma-separated (`?machine=relay-1,relay-2`): what a
 *  multi-select writes where one link has always named one value. An empty list leaves the
 *  address, so unticking the last pick does not bring the filter back on reload. The written
 *  list is cut at `SEARCH_QUERY_MAX` like any other text the page puts in its address, for the
 *  same reason: the load balancer refuses a URL past 16 K on reload or a shared link. */
export function commaListParam(): AddressParam<readonly string[]> {
  return {
    read: (params, name) => (params.get(name) ?? "").split(",").filter((value) => value !== ""),
    write: (params, name, value) => {
      if (value.length === 0) params.delete(name);
      else params.set(name, capSearchQuery(value.join(",")));
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
    (next: Partial<AddressValues<S>>) => {
      const written = next as Record<string, unknown>;
      update((current) => {
        for (const [name, param] of Object.entries(spec)) {
          if (name in written) param.write(current, name, written[name]);
        }
        return current;
      });
    },
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
 *  (`cappedTextParam`): the project page's `?q=`, the Agents page's `?q=` and `?machine=`, and
 *  the machine page's `?window=` all go through this one hook. */
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

/** The Inbox at `/`, optionally narrowed to one agent's asks (`?agent=<session id>`); with
 *  `section=needs-you` only the asks waiting on the viewer are shown. Every field below is
 *  absent while it holds its default, so a plain `/` is the Inbox as it opens. */
export interface InboxRoute {
  agent?: string;
  /** The row the Inbox has selected, the one its reading pane shows: an ask's id, or a thread's
   *  `reply:<thread id>` / `agent:<message id>` (`rowId`, `features/inbox/list-row.ts`). The
   *  address is the whole memory of that selection, so a link carries the reader's place. */
  item?: string;
  section?: "needs-you";
  /** The text the Inbox's search box narrows the list by, as typed. It is a field of the route
   *  rather than a search parameter of its own because `buildInboxPath` rebuilds the whole query
   *  string from this type and `usePaneSelection` navigates with it on every selection change, so
   *  a parameter outside the type would be dropped by the next `j`. */
  q?: string;
  /** Who the listed asks are for. Absent is the page's default, the reader's own work; an empty
   *  array is the cleared filter, every question; a value is a person's lowercase email or the
   *  literal `unassigned`. */
  assignees?: readonly string[];
  /** `?show=dismissed`, the View menu's **Show** switch on **Dismissed**: what the reader has
   *  dismissed out of their Inbox, newest dismissal first. Absent is the Inbox itself, every
   *  undismissed row, read or not. */
  show?: "dismissed";
  sort?: InboxSort;
  group?: InboxGroupBy;
  /** The chosen values of each filter field, one comma list per field in the address. A field
   *  with nothing chosen is absent, never an empty array. */
  filters?: Partial<Record<InboxFilterField, readonly string[]>>;
}

/** The values a filter field accepts, or undefined for the one field whose vocabulary is the
 *  workspace's own: a project key is whatever somebody created, so only `none` is reserved. */
function filterVocabulary(field: InboxFilterField): readonly string[] | undefined {
  if (field === "priority") return INBOX_PRIORITY_VALUES;
  if (field === "urgency") return INBOX_URGENCY_VALUES;
  return field === "type" ? INBOX_TYPE_VALUES : undefined;
}

/**
 * The page's `?assignee=` deliberately spells its default the opposite way round from the
 * server's own `?assignee=` on `GET /api/v1/inbox`
 * (`packages/envoy/internal/dispatch/api/inbox.go`), where an absent parameter means everybody:
 * here absent means the reader alone and `assignee=everyone` means everybody, so the same name
 * now means different things on the two sides and neither reads the other's URL.
 *
 * Every other field is absent while it holds its default, so the Inbox as it opens is `/` and a
 * link says only what the reader changed. A value this build does not know - an older link, a
 * typo - is dropped rather than refused: the list is a client-side split of what the two reads
 * answered, so an unknown word would match no row anyway.
 */
function inboxRouteOf(params: URLSearchParams): InboxRoute {
  const agent = params.get("agent");
  const route: InboxRoute = {};
  if (agent !== null && agent !== "") {
    route.agent = agent;
  }
  const item = params.get("item")?.trim() ?? "";
  if (item !== "") {
    route.item = item;
  }
  if (params.get("section") === "needs-you") {
    route.section = "needs-you";
  }
  // Neither trimmed nor cut: a reader mid-word has a trailing space that is part of what they
  // typed, and the box rewrites no address a link arrived with.
  const q = params.get("q");
  if (q !== null && q !== "") {
    route.q = q;
  }
  // Nothing refuses an email the page has never heard of: the filter is a client-side split of
  // one inbox response, so an unlisted or misspelled person simply matches no row.
  const assignees: string[] = [];
  let everyone = false;
  for (const raw of params.getAll("assignee")) {
    const value = raw.trim().toLowerCase();
    if (value === "") continue;
    if (value === EVERYONE_FILTER_VALUE) {
      everyone = true;
      continue;
    }
    if (!assignees.includes(value)) assignees.push(value);
  }
  if (assignees.length > 0 || everyone) {
    route.assignees = assignees;
  }
  if (params.get("show") === "dismissed") {
    route.show = "dismissed";
  }
  const sort = params.get("sort");
  if (sort !== null && sort !== "newest" && (INBOX_SORTS as readonly string[]).includes(sort)) {
    route.sort = sort as InboxSort;
  }
  const group = params.get("group");
  if (
    group !== null &&
    group !== "priority" &&
    (INBOX_GROUP_BYS as readonly string[]).includes(group)
  ) {
    route.group = group as InboxGroupBy;
  }
  const filters: Partial<Record<InboxFilterField, readonly string[]>> = {};
  for (const field of INBOX_FILTER_FIELDS) {
    const vocabulary = filterVocabulary(field);
    const chosen: string[] = [];
    for (const raw of params.getAll(field)) {
      for (const part of raw.split(",")) {
        const value = part.trim();
        if (value === "" || chosen.includes(value)) continue;
        if (vocabulary !== undefined && !vocabulary.includes(value)) continue;
        chosen.push(value);
      }
    }
    if (chosen.length > 0) filters[field] = chosen;
  }
  if (Object.keys(filters).length > 0) {
    route.filters = filters;
  }
  return route;
}

/** The Inbox's route as a query string names it (`inboxRouteOf`), for a caller holding the
 *  address as text: `features/search`'s scoping reads one, and so do this module's own tests. */
export function parseInboxSearch(search: string): InboxRoute {
  return inboxRouteOf(copyAddress(search));
}

export function buildInboxPath(route: InboxRoute = {}): string {
  const query = copyAddress();
  if (route.agent !== undefined) {
    query.set("agent", route.agent);
  }
  if (route.item !== undefined) {
    query.set("item", route.item);
  }
  if (route.section !== undefined) {
    query.set("section", route.section);
  }
  if (route.q !== undefined) {
    query.set("q", route.q);
  }
  if (route.assignees !== undefined) {
    if (route.assignees.length === 0) {
      query.append("assignee", EVERYONE_FILTER_VALUE);
    }
    for (const assignee of route.assignees) {
      query.append("assignee", assignee);
    }
  }
  if (route.show !== undefined) {
    query.set("show", route.show);
  }
  if (route.sort !== undefined && route.sort !== "newest") {
    query.set("sort", route.sort);
  }
  if (route.group !== undefined && route.group !== "priority") {
    query.set("group", route.group);
  }
  for (const field of INBOX_FILTER_FIELDS) {
    const chosen = route.filters?.[field];
    if (chosen !== undefined && chosen.length > 0) query.set(field, chosen.join(","));
  }
  const search = query.toString();
  return search === "" ? "/" : `/?${search}`;
}

/**
 * The Inbox's whole narrowing, read from the address and written back to it. The write goes
 * through the path `buildInboxPath` builds rather than through the parameters alone, because
 * the Inbox's query string is rebuilt whole from the route on every change - a parameter
 * outside `InboxRoute` would be dropped by the next selection - and because the Inbox the
 * address points at is `/`, whichever of the page's addresses the reader arrived on.
 */
export function useInboxRoute(): readonly [InboxRoute, (next: InboxRoute) => void] {
  const params = useAddressParams();
  const go = useAddressNavigate();
  const route = useMemo(() => inboxRouteOf(params), [params]);
  const setRoute = useCallback((next: InboxRoute) => go(buildInboxPath(next)), [go]);
  return [route, setRoute] as const;
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
 *  grammar, under its own prefix, so neither reads the other's parameters. */
export interface TableAddressGrammar {
  read(params: URLSearchParams): TableAddressView;
  write(params: URLSearchParams, view: TableAddressView): URLSearchParams;
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
const TABLE_SEARCH = CAPPED_TEXT;
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
 * default a link leaves out (`features/project/issues-table-model.ts`).
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
export interface TableAddress {
  /** The whole address, which the table takes as its view key: a view it has to rebuild from
   *  scratch is exactly one whose address changed. */
  params: URLSearchParams;
  view: TableAddressView;
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
export function useTableAddress(grammar: TableAddressGrammar): TableAddress {
  const params = useAddressParams();
  const update = useAddressUpdate();
  const view = useMemo(() => grammar.read(params), [grammar, params]);
  const updateView = (next: (current: TableAddressView) => TableAddressView) =>
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
