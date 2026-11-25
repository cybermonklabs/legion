/**
 * The Inbox's remembered view: the filters, the assignee choice, the sort and the grouping a
 * reader last set, kept for their browser until they clear them.
 *
 * The address still carries the view, so a link opens on what it names. The browser's copy only
 * fills an address that names nothing: the sidebar's plain `/`, a reload of it, a new tab. An
 * address that names a view, an agent's asks, one row, or a search is the reader's or a link's
 * own, and the remembered view stays out of it.
 *
 * Only a change the reader makes writes the copy, so opening a shared link changes nothing kept.
 * **Clear filters** writes the copy too, so it forgets the filters it clears; the sort and the
 * grouping are forgotten by choosing the default again in **View**.
 */

import { useCallback } from "react";

import { buildInboxPath, type InboxRoute, parseInboxSearch } from "../refs/routes";
import { useUserPreference } from "../shell/userPreference";

/** The part of the Inbox's route that the browser remembers. */
export type InboxNarrowing = Pick<InboxRoute, "assignees" | "filters" | "group" | "sort">;

/** The fields of `route` the browser remembers, with every field that holds its default left out,
 *  as the address leaves it out. */
export function narrowingOf(route: InboxRoute): InboxNarrowing {
  // A round trip through the address drops the defaults and any empty list, so two routes that
  // draw the same list give the same narrowing.
  const parsed = parseInboxSearch(
    buildInboxPath({
      assignees: route.assignees,
      filters: route.filters,
      group: route.group,
      sort: route.sort,
    }).slice(1)
  );
  const narrowing: InboxNarrowing = {};
  if (parsed.assignees !== undefined) narrowing.assignees = parsed.assignees;
  if (parsed.filters !== undefined) narrowing.filters = parsed.filters;
  if (parsed.group !== undefined) narrowing.group = parsed.group;
  if (parsed.sort !== undefined) narrowing.sort = parsed.sort;
  return narrowing;
}

/** Whether `narrowing` changes nothing about the Inbox as it opens. */
function isEmpty(narrowing: InboxNarrowing): boolean {
  return Object.keys(narrowing).length === 0;
}

/**
 * Whether the remembered view may fill `route`: only when the route names no view of its own and
 * points at nothing in particular. An agent's asks (`?agent=`, `?section=`), one row (`?item=`)
 * and a search (`?q=`) are what a link or the reader asked for, and a remembered filter could
 * hide that very row.
 */
export function opensPlain(route: InboxRoute): boolean {
  return (
    route.agent === undefined &&
    route.section === undefined &&
    route.item === undefined &&
    route.q === undefined &&
    isEmpty(narrowingOf(route))
  );
}

/** `route` with the remembered view filled in where `opensPlain` allows it, and `route` itself
 *  otherwise. */
export function withRemembered(route: InboxRoute, remembered: InboxNarrowing): InboxRoute {
  if (isEmpty(remembered) || !opensPlain(route)) return route;
  return { ...route, ...remembered };
}

/** The stored form is the address's own query string, so the parser that guards a link guards
 *  the stored copy too: a value another build wrote, or a person edited, is dropped the way an
 *  unknown word in an address is. */
function readNarrowing(stored: string | null): InboxNarrowing {
  if (stored === null) return {};
  return narrowingOf(parseInboxSearch(stored));
}

function writeNarrowing(narrowing: InboxNarrowing): string {
  return buildInboxPath(narrowing).slice(1);
}

/**
 * The remembered view, and the call that remembers the view of a route the reader just made.
 * The page and the drawer each read it when they mount, so the two agree.
 */
export function useRememberedView(): [InboxNarrowing, (next: InboxRoute) => void] {
  const [remembered, setRemembered] = useUserPreference<InboxNarrowing>(
    "inbox.view",
    readNarrowing,
    writeNarrowing
  );
  const remember = useCallback(
    (next: InboxRoute) => {
      const narrowing = narrowingOf(next);
      // Typing in the search box and moving the selection change no view; skip the write.
      if (writeNarrowing(narrowing) === writeNarrowing(remembered)) return;
      setRemembered(narrowing);
    },
    [remembered, setRemembered]
  );
  return [remembered, remember];
}
