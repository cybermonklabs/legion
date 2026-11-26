import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING, isAgentListRefilling } from "../api/client";
import { dangerText, textSecondaryOnSurface } from "../theme/classes";
import { heldPress } from "./held-press";

export interface QueryErrorProps {
  /** The small line under a control row, as the Issues tab's alerts under its view tabs are
   *  drawn: the message and a semibold Retry one space after it, in one line of text. */
  compact?: boolean;
  /** The failure itself. An agent list that is refilling (`isAgentListRefilling`) is not a
   *  failure to report: the shared refilling notice replaces `message`, as status, not alert. */
  error?: unknown;
  message: string;
  /** Omitted for a failure a retry cannot get past; no Retry is offered then. */
  onRetry?: () => void;
  /** Retry's accessible name where Retry alone does not say what it reads again: a place that
   *  holds more than one Retry, as the alert line under the Issues tab's view tabs does. */
  retryLabel?: string;
  /** Holds Retry while the retried request is in flight (`heldPress`): it keeps the focus and its
   *  Tab stop, and a press of it sends nothing then. */
  retrying?: boolean;
}

/**
 * The line every view shows while Envoy's agent list refills, in place of any claim that an
 * agent is absent. One test id on every view, so a browser test finds it the same way on each.
 */
export function AgentListRefilling({ className }: { className?: string }): ReactNode {
  return (
    <p className={className} data-testid="agent-list-refilling" role="status">
      {AGENT_LIST_REFILLING}
    </p>
  );
}

/**
 * A retryable error state for a failed query or write, matching the Conversation
 * pin-state queue's affordance. The message breaks inside a word longer than the line, so a
 * whole error with a long token in it stays in the line.
 */
export function QueryError({
  compact = false,
  error,
  message,
  onRetry,
  retryLabel,
  retrying = false,
}: QueryErrorProps): ReactNode {
  const refilling = isAgentListRefilling(error);
  // The compact line is for a failure: a refilling agent list draws its notice in the full form.
  const line = compact && !refilling;
  const retry =
    onRetry === undefined ? null : (
      <button
        aria-label={retryLabel}
        className={`${line ? "font-semibold" : "font-medium"} underline refused:cursor-not-allowed refused:opacity-50`}
        {...heldPress(retrying, onRetry)}
        type="button"
      >
        Retry
      </button>
    );
  if (line)
    return (
      <p className={`min-w-0 break-words text-xs ${dangerText}`} role="alert">
        {message}
        {retry === null ? null : <> {retry}</>}
      </p>
    );
  return (
    <div
      className={`flex flex-wrap items-center gap-3 text-sm ${refilling ? textSecondaryOnSurface : dangerText}`}
      role={refilling ? undefined : "alert"}
    >
      {refilling ? <AgentListRefilling /> : <p className="min-w-0 break-words">{message}</p>}
      {retry === null ? null : (
        <>
          {/* The space between the message and Retry: a flex container does not render a
              white-space-only child, so this changes no pixel, and without it the alert's text
              reads the message's last word and Retry as one word. */}{" "}
          {retry}
        </>
      )}
    </div>
  );
}
