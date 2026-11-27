import type { ReactNode } from "react";

import { usePageWords, useTooltips } from "../../components/tooltips";
import {
  connectionDotConnected,
  connectionDotConnecting,
  connectionDotFailed,
  connectionDotOffline,
  pageWaitingBg,
} from "../../theme/classes";
import { pageQuietText, pageSegmentDoneBg, textMutedOnSurface } from "../../theme/shell-classes";
import { type ConnectionState, connectionLabel, pendingNotice } from "./connection";
import type { PendingState } from "./pending-sync";

const DOT_CLASS_BY_STATE: Record<ConnectionState, string> = {
  connected: connectionDotConnected,
  connecting: connectionDotConnecting,
  failed: connectionDotFailed,
  offline: connectionDotOffline,
};

/** Each dot inside `NoTooltips`, in the new issue page's colours: connected is the passed
 *  green (`pageSegmentDoneBg` is that fill), connecting the waiting orange, failed the failed
 *  red and offline the not-run grey. The dot is decoration; its name says the state. */
const PAGE_DOT_CLASS_BY_STATE: Record<ConnectionState, string> = {
  connected: pageSegmentDoneBg,
  connecting: pageWaitingBg,
  failed: "bg-page-failed",
  offline: "bg-page-not-run",
};

/** The document's live-connection indicator. A pending-edit notice expands it only while the
 * reader needs one; otherwise it stays a compact dot in the document header. */
export function ConnectionDot({
  connection,
  pending,
}: {
  connection: ConnectionState;
  pending: PendingState | undefined;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const notice = pendingNotice(connection, pending);
  const label = connectionLabel(connection, notice);
  // Inside `NoTooltips` the dot is named by `aria-label`, not by a `title`: that surface draws
  // no tooltip (`components/tooltips.tsx`, the new issue page), and the indicator still has to
  // say which state it is in. The socket's state and the count of unsent edits are a reading of
  // the document, not a word of the page.
  return (
    <span
      aria-label={tooltips ? undefined : label}
      className={
        notice === undefined
          ? "inline-flex shrink-0 items-center"
          : "inline-flex items-center gap-1"
      }
      role="status"
      title={tooltips ? label : undefined}
    >
      <span
        aria-hidden
        className={`h-2.5 w-2.5 rounded-full ${(tooltips ? DOT_CLASS_BY_STATE : PAGE_DOT_CLASS_BY_STATE)[connection]}`}
      />
      {notice === undefined ? null : (
        // Inside `NoTooltips` the notice is a reading of the document, a value, so it keeps
        // `max-w-full` and breaks anywhere.
        <span
          aria-hidden
          className={`text-xs ${tooltips ? textMutedOnSurface : `max-w-full [overflow-wrap:anywhere] ${pageQuietText}`}`}
          {...page?.authorText}
        >
          {notice}
        </span>
      )}
      <span className="sr-only" {...page?.authorText}>
        {label}
      </span>
    </span>
  );
}
