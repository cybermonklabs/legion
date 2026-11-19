import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING, isAgentListRefilling } from "../../api/client";
import { pageFailedText, pageHeaderControl, pageQuietText } from "../../theme/classes";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/** What a row of Details is handed of a read or a write that failed. */
export interface DetailsErrorLineProps {
  /** The failure itself. An agent list that is refilling (`isAgentListRefilling`) is not a
   *  failure to report: the refilling line replaces `message`, as status, not alert. */
  readonly error?: unknown;
  readonly message: string;
  /** Omitted for a failure a retry cannot get past; no Retry is offered then. */
  readonly onRetry?: () => void;
  /** Disables Retry while the retried request is in flight. */
  readonly retrying?: boolean;
}

/**
 * A read or a write of a row of Details that failed, said in that row with its Retry
 * (dispatch://INFRA-1196 R13): the line the shared `QueryError` draws, in the page's colours and
 * with the page's word for its control. An agent list that is refilling is said as the line
 * every view says it in (`AgentListRefilling`'s test id), as status rather than alert.
 */
export function DetailsErrorLine({
  error,
  message,
  onRetry,
  retrying = false,
}: DetailsErrorLineProps): ReactNode {
  return (
    <div className="flex flex-wrap items-center gap-3 text-sm">
      {isAgentListRefilling(error) ? (
        <p className={pageQuietText} data-testid="agent-list-refilling" role="status">
          {AGENT_LIST_REFILLING}
        </p>
      ) : (
        <p className={pageFailedText} role="alert">
          {message}
        </p>
      )}
      {onRetry === undefined ? null : (
        <button
          className={`${pageHeaderControl} disabled:cursor-not-allowed disabled:opacity-50`}
          disabled={retrying}
          onClick={onRetry}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.retry}
        </button>
      )}
    </div>
  );
}
