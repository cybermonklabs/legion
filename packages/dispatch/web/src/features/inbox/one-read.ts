/**
 * The open asks the Inbox lists. The reader's own come from the one read, `GET /api/v1/me/inbox`,
 * as their Reply and Agent rows do (`useInboxThreadRows`) and as the two numbers the Inbox entry
 * adds up (`useNeedsYouCount`, `useUnreadThreadCount`); every other person's come from the read of
 * the open asks, `GET /api/v1/asks/open`, which the page makes only while its filter shows them.
 */

import { meInboxAsks } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { meInboxQuery, openAsksQuery } from "../../api/queries";
import type { OpenAskRow } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { isMine, isOwnList } from "./sections";

/** The open asks the Inbox lists (`useInboxAsks`), and the state of the reads they are drawn
 *  from. */
export interface InboxAsks {
  /** Undefined until every read the list needs has answered. */
  readonly asks: OpenAskRow[] | undefined;
  /** Whether a read the list needs has not answered yet: the one read always, and the read of
   *  the open asks while the selection shows anyone's asks but the reader's own. */
  readonly pending: boolean;
  /** Whether one of those reads failed. */
  readonly failed: boolean;
  /** When the read `askId` is listed from answered: the one read's time for an ask drawn from it,
   *  the open asks' for every other. A card seeds its thread from its row (`AskCard`'s
   *  `initialThreadUpdatedAt`), and that seed is as old as the read the row came from, not as new
   *  as the other read. */
  readonly updatedAtOf: (askId: string) => number;
}

/**
 * The open asks the Inbox lists for `selection`, the assignees its filter names (empty for
 * nobody): the reader's own from the one read, and - while the selection shows anyone's asks but
 * the reader's own - every other person's from the read of the open asks, each once and none of
 * them the reader's. The read of the open asks is every project's, once, whoever the selection
 * names: the page narrows its rows itself, so a pick of another person asks for nothing new. A
 * write that has just made an ask wait on the reader leaves it in the open asks until that read
 * answers again, and the one read lists it (`patchMeInboxAsks`). The same array while neither
 * read moves, since the motion hold adopts what it is given by identity (`motion-hold.ts`).
 */
export function useInboxAsks(selection: readonly string[], viewer: string | undefined): InboxAsks {
  const human = useIsHuman();
  const own = useQuery({ ...meInboxQuery(), enabled: human });
  // Nobody is "anyone but the reader" until the reader's login resolves.
  const othersShown = viewer !== undefined && !isOwnList(selection, viewer);
  const others = useQuery({ ...openAsksQuery(), enabled: othersShown });
  const answer = own.data;
  const rows = others.data;
  const { ids, listed } = useMemo(() => {
    if (answer === undefined) return { ids: new Set<string>(), listed: undefined };
    const mine = meInboxAsks(answer);
    const ids = new Set(mine.map((ask) => ask.id));
    if (!othersShown) return { ids, listed: mine };
    if (rows === undefined) return { ids, listed: undefined };
    const rest = rows.filter(
      (ask) => !ids.has(ask.id) && (viewer === undefined || !isMine({ ask, kind: "ask" }, viewer))
    );
    return { ids, listed: [...rest, ...mine] };
  }, [answer, othersShown, rows, viewer]);
  const ownAt = own.dataUpdatedAt;
  const othersAt = others.dataUpdatedAt;
  const updatedAtOf = useCallback(
    (askId: string) => (ids.has(askId) ? ownAt : othersAt),
    [ids, othersAt, ownAt]
  );
  return {
    asks: listed,
    failed: (human && own.isError) || (othersShown && others.isError),
    pending: (human && own.isPending) || (othersShown && others.isPending),
    updatedAtOf,
  };
}
