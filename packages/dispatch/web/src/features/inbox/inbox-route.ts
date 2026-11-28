import { useCallback, useMemo } from "react";

import {
  addressPath,
  choiceParam,
  copyAddress,
  repeatedParam,
  textParam,
  useAddressNavigate,
  useAddressParams,
} from "../../lib/page-address";
import {
  INBOX_FILTER_FIELDS,
  INBOX_FILTER_VOCABULARY,
  INBOX_GROUP_BYS,
  INBOX_SHOWS,
  INBOX_SORTS,
  type InboxFilterField,
  type InboxGroupBy,
  type InboxSort,
} from "../refs/routes";
import { EVERYONE_FILTER_VALUE } from "./sections";

/**
 * The Inbox's address: the route the page reads out of `/` and writes back to it. The words are
 * the Inbox's own - the sorts, groupings, lists and filter vocabulary of `features/refs/routes.ts`
 * and `sections.ts`'s `everyone` - and every parameter is read and written with one of
 * `lib/page-address.ts`'s kinds. The rules live here, beside those words, so that module holds
 * only the machinery every page shares and imports nothing from `features/`.
 */

/** The Inbox at `/`, optionally narrowed to one agent's asks (`?agent=<session id>`); with
 *  `section=needs-you` only the asks waiting on the viewer are shown. Every field below is
 *  absent while it holds its default, so a plain `/` is the Inbox as it opens. */
export interface InboxRoute {
  agent?: string;
  /** The row the Inbox has selected, the one its reading pane shows: an ask's id, or a thread's
   *  `reply:<thread id>` / `agent:<message id>` (`rowId`, `list-row.ts`). The address is the
   *  whole memory of that selection, so a link carries the reader's place. */
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

/** `?agent=`, `?item=`, `?q=`, and each filter field's comma list as written. */
const TEXT = textParam();
/** `?section=needs-you`, the one section a link names; any other word reads as none. */
const SECTION = choiceParam<"" | "needs-you">(["needs-you"], "");
const SHOW = choiceParam(INBOX_SHOWS, "inbox");
const SORT = choiceParam(INBOX_SORTS, "newest");
const GROUP = choiceParam(INBOX_GROUP_BYS, "priority");
/** `?assignee=`, repeated, and every occurrence of a filter field as the address holds it. */
const REPEATED = repeatedParam();

/**
 * One filter field's chosen values. Each `?<field>=` is a comma list, and a field the address
 * names more than once is read as all of its lists. A value is trimmed, and a repeat is dropped,
 * as is a word outside the field's vocabulary (`INBOX_FILTER_VOCABULARY`).
 *
 * `commaListParam` would read only the first of those lists. So the occurrences are read with
 * `repeatedParam`, and the joined list is written with `textParam` and split back here on every
 * comma, with no escape to read: no value a field takes, a word of its vocabulary or a project's
 * key, holds a comma.
 */
function chosenValues(params: URLSearchParams, field: InboxFilterField): string[] {
  const vocabulary = INBOX_FILTER_VOCABULARY[field];
  const chosen: string[] = [];
  for (const list of REPEATED.read(params, field)) {
    for (const part of list.split(",")) {
      const value = part.trim();
      if (value === "" || chosen.includes(value)) continue;
      if (vocabulary !== undefined && !vocabulary.includes(value)) continue;
      chosen.push(value);
    }
  }
  return chosen;
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
  const route: InboxRoute = {};
  const agent = TEXT.read(params, "agent");
  if (agent !== "") {
    route.agent = agent;
  }
  const item = TEXT.read(params, "item").trim();
  if (item !== "") {
    route.item = item;
  }
  const section = SECTION.read(params, "section");
  if (section !== "") {
    route.section = section;
  }
  // Neither trimmed nor cut: a reader mid-word has a trailing space that is part of what they
  // typed, and the box rewrites no address a link arrived with.
  const q = TEXT.read(params, "q");
  if (q !== "") {
    route.q = q;
  }
  // Nothing refuses an email the page has never heard of: the filter is a client-side split of
  // one inbox response, so an unlisted or misspelled person simply matches no row.
  const assignees: string[] = [];
  let everyone = false;
  for (const raw of REPEATED.read(params, "assignee")) {
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
  const show = SHOW.read(params, "show");
  if (show !== "inbox") {
    route.show = show;
  }
  const sort = SORT.read(params, "sort");
  if (sort !== "newest") {
    route.sort = sort;
  }
  const group = GROUP.read(params, "group");
  if (group !== "priority") {
    route.group = group;
  }
  const filters: Partial<Record<InboxFilterField, readonly string[]>> = {};
  for (const field of INBOX_FILTER_FIELDS) {
    const chosen = chosenValues(params, field);
    if (chosen.length > 0) filters[field] = chosen;
  }
  if (Object.keys(filters).length > 0) {
    route.filters = filters;
  }
  return route;
}

/** The Inbox's route as a query string names it (`inboxRouteOf`), for a caller holding the
 *  address as text: this module's own tests read one. */
export function parseInboxSearch(search: string): InboxRoute {
  return inboxRouteOf(copyAddress(search));
}

/** The Inbox's address for `route`, its parameters in one fixed order and each left out while it
 *  holds its default: `/` for the Inbox as it opens. */
export function buildInboxPath(route: InboxRoute = {}): string {
  const query = copyAddress();
  TEXT.write(query, "agent", route.agent ?? "");
  TEXT.write(query, "item", route.item ?? "");
  SECTION.write(query, "section", route.section ?? "");
  TEXT.write(query, "q", route.q ?? "");
  if (route.assignees !== undefined) {
    const cleared = route.assignees.length === 0;
    REPEATED.write(query, "assignee", cleared ? [EVERYONE_FILTER_VALUE] : route.assignees);
  }
  SHOW.write(query, "show", route.show ?? "inbox");
  SORT.write(query, "sort", route.sort ?? "newest");
  GROUP.write(query, "group", route.group ?? "priority");
  for (const field of INBOX_FILTER_FIELDS) {
    TEXT.write(query, field, (route.filters?.[field] ?? []).join(","));
  }
  return addressPath("/", query);
}

/**
 * The Inbox's whole narrowing, read from the address and written back to it. The write goes
 * through the path `buildInboxPath` builds rather than through the parameters alone, because
 * the Inbox's query string is rebuilt whole from the route on every change - a parameter
 * outside `InboxRoute` would be dropped by the next selection - and because the Inbox the
 * address points at is `/`, whichever of the page's addresses the reader arrived on. The write
 * replaces the history entry, and carries `state` into it where the caller keeps something of
 * its own there (the open row's, `pane-selection.ts`).
 */
export function useInboxRoute(): readonly [
  InboxRoute,
  (next: InboxRoute, state?: unknown) => void,
] {
  const params = useAddressParams();
  const go = useAddressNavigate();
  const route = useMemo(() => inboxRouteOf(params), [params]);
  const setRoute = useCallback(
    (next: InboxRoute, state?: unknown) => go(buildInboxPath(next), { state }),
    [go]
  );
  return [route, setRoute] as const;
}
