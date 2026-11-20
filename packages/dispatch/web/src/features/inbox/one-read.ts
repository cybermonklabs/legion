/**
 * The Inbox's opt-in to the one read, `GET /api/v1/me/inbox` (dispatch://INFRA-1152, decision 8
 * of its plan). The page at `/` draws the reader's own rows from it when its address carries
 * `one=1` (`InboxRoute.one`), or when the browser preference `inbox.one_read` reads `on`; without
 * either, every surface reads what it read before. The surfaces it moves are the reader's own:
 * the asks that wait on them (`useInboxAsks`), their Reply and Agent rows
 * (`useInboxThreadRows`), and the two numbers the Inbox entry adds up (`useNeedsYouCount`,
 * `useUnreadThreadCount`). Another person's asks are the Inbox read's either way.
 */

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { useLocation } from "react-router-dom";

import { meInboxQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { parseInboxSearch, routeIsInbox } from "../refs/routes";
import { useUserPreference } from "../shell/userPreference";
import { meInboxAsks } from "./one-read-rows";
import { isMine } from "./sections";

/** The preference reads `on` where the reader opted in; anything else, or nothing, is off. */
function readOneRead(stored: string | null): boolean {
  return stored === "on";
}

function writeOneRead(on: boolean): string {
  return on ? "on" : "off";
}

/** Whether the reader's own rows come from the one read: the preference is on, or this is the
 *  Inbox page and its address carries `one=1`. Another page's address opts nothing in, so the
 *  drawer over it follows the preference alone. */
export function useOneRead(): boolean {
  const { pathname, search } = useLocation();
  const [preferred] = useUserPreference("inbox.one_read", readOneRead, writeOneRead);
  return preferred || (routeIsInbox(pathname) && parseInboxSearch(search).one === true);
}

/** The open asks the Inbox lists (`useInboxAsks`), and the state of the one read they are drawn
 *  from behind the opt-in. */
export interface InboxAsks {
  readonly asks: InboxRow[] | undefined;
  /** Whether the one read the reader's own asks come from has not answered yet: false without
   *  the opt-in, where no such read is made. */
  readonly pending: boolean;
  /** Whether that read failed: false without the opt-in. */
  readonly failed: boolean;
}

/**
 * The open asks the Inbox lists: `asks`, every one the Inbox read answered, as they are; and
 * behind the opt-in, those that wait on the reader taken from the one read in their place, each
 * once, with every other person's still the Inbox read's. The same array while neither read
 * moves, since the motion hold adopts what it is given by identity (`motion-hold.ts`); undefined
 * until the Inbox read has answered, as `asks` is. While the one read has not answered, or where
 * it failed, the list holds none of the reader's own asks, so it says so (`pending`, `failed`)
 * for the page to draw its loading state or its error rather than a list without them.
 */
export function useInboxAsks(asks: InboxRow[] | undefined): InboxAsks {
  const oneRead = useOneRead();
  const human = useIsHuman();
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const own = useQuery({ ...meInboxQuery(), enabled: oneRead && human });
  const answer = own.data;
  const listed = useMemo(() => {
    if (!oneRead || asks === undefined) return asks;
    const mine = meInboxAsks(answer);
    const ids = new Set(mine.map((ask) => ask.id));
    const others = asks.filter(
      (ask) => !ids.has(ask.id) && (viewer === undefined || !isMine({ ask, kind: "ask" }, viewer))
    );
    return [...others, ...mine];
  }, [asks, oneRead, answer, viewer]);
  const reading = oneRead && human;
  return { asks: listed, failed: reading && own.isError, pending: reading && own.isPending };
}
