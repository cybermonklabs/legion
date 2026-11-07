import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { userAgentStateQuery } from "../../api/queries";
import { railHoverBg, railNeedsYouBadgeBg, railNeedsYouBadgeText } from "../../theme/classes";
import { totalUnreadReplies, unreadRepliesLabel } from "../agents/unread";
import { AfterTheFirstBatch } from "../doc/AfterTheFirstBatch";
import { useInboxCount } from "../inbox/useInboxCount";
import { routeIsInbox } from "../refs/routes";

/** The shape every count in the compact header takes, so the box one of them stands in is the
 *  box the next one takes. */
const countBadge = "whitespace-nowrap rounded-full px-2 py-1 text-xs font-semibold";

/**
 * The inbox the compact header offers, and what it is holding for the reader.
 *
 * One count on one control: the work waiting on the reader and the threads they have not read,
 * which the Inbox lists together. On the Inbox page it stays plain text - the page is the Inbox
 * already, so there is nowhere for it to go - and everywhere else the peek that opens the
 * drawer stands beside it, carrying no count of its own.
 */
function InboxPeek({
  count,
  onInboxPage,
  onOpen,
}: {
  count: number;
  onInboxPage: boolean;
  onOpen: () => void;
}): ReactNode {
  const label = count === 0 ? "Inbox" : `Inbox ${count}`;
  const badge = `${countBadge} ${
    count === 0 ? railHoverBg : `${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`
  }`;
  if (onInboxPage) {
    return <span className={badge}>{label}</span>;
  }
  return (
    <>
      <button
        aria-label="Peek at the inbox without leaving this page"
        className={`${countBadge} ${railHoverBg}`}
        onClick={onOpen}
        type="button"
      >
        Peek
      </button>
      <Link className={badge} to="/">
        {label}
      </Link>
    </>
  );
}

/**
 * What the compact header says beside the title: the inbox peek, the one Inbox count, and how
 * many agent replies the reader has not read.
 *
 * The reads behind those numbers are not what a reader who opened a document is waiting for, so
 * on a document address they are made - and these counts drawn - after the editor's first batch
 * (dispatch://INFRA-341 R11, `features/doc/AfterTheFirstBatch.tsx`). The space they keep is the
 * peek at a count of nothing, which is exactly what the header draws until those reads answer,
 * so the box is the box it already was and the header's height does not change as they land.
 *
 * Its own component, so a count arriving re-renders the counts rather than the whole shell.
 */
export function ShellCounts({
  onOpenInboxDrawer,
  reached,
}: {
  onOpenInboxDrawer: () => void;
  reached: boolean;
}): ReactNode {
  const { pathname } = useLocation();
  const onInboxPage = routeIsInbox(pathname);
  return (
    <AfterTheFirstBatch
      reached={reached}
      space={<InboxPeek count={0} onInboxPage={onInboxPage} onOpen={onOpenInboxDrawer} />}
    >
      <TheCounts onInboxPage={onInboxPage} onOpen={onOpenInboxDrawer} />
    </AfterTheFirstBatch>
  );
}

function TheCounts({
  onInboxPage,
  onOpen,
}: {
  onInboxPage: boolean;
  onOpen: () => void;
}): ReactNode {
  const inboxCount = useInboxCount();
  const unreadReplies = totalUnreadReplies(useQuery(userAgentStateQuery()).data);
  return (
    <>
      <InboxPeek count={inboxCount} onInboxPage={onInboxPage} onOpen={onOpen} />
      {unreadReplies === 0 ? null : (
        <Link
          className={`${countBadge} ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
          to="/agents"
        >
          {unreadRepliesLabel(unreadReplies)}
        </Link>
      )}
    </>
  );
}
