import type { ReactNode } from "react";

import {
  badgeBlocking,
  badgeBlockingHoverBg,
  badgeLow,
  badgePrimary,
  badgePrimaryHoverBg,
  badgePrimaryOutline,
  borderTransparent,
  focusVisibleRing,
} from "../../theme/classes";
import type { AskRewrite } from "./ask-rewrite";

/** Every state is the same box: a 1 px border, clear unless the state draws it (Original). */
const PILL =
  "inline-flex shrink-0 items-center rounded-full border px-2 py-px text-xs leading-4 font-medium whitespace-nowrap outline-none focus-visible:ring-2";

/**
 * The one mark a rewrite leaves on the card, in the line under the question: **Rewriting…**
 * while it runs, **Rewritten** once it is in place, whoever asked for it (the reader, or
 * Dispatch's check before the reader saw the ask), and **Rewrite failed** when Dispatch did not
 * apply it. Rewritten shows the original wording in place and reads **Original** until it is
 * pressed again (`w`); Rewrite failed tries again (`r`). The rest is its tooltip.
 */
export function AskRewriteBadge({
  onRetry,
  onToggle,
  rewrite,
  running,
  showingOriginal,
}: {
  onRetry: () => void;
  onToggle: () => void;
  rewrite: AskRewrite | undefined;
  /** The reader's own rewrite is out, before the ask read says so. */
  running: boolean;
  showingOriginal: boolean;
}): ReactNode {
  if (running || rewrite?.state === "running") {
    return (
      <span
        className={`${PILL} animate-pulse ${borderTransparent} ${badgeLow.bg} ${badgeLow.text}`}
        data-ask-rewrite="running"
        role="status"
      >
        Rewriting…
      </span>
    );
  }
  if (rewrite === undefined) return null;
  if (rewrite.state === "failed") {
    return (
      <button
        className={`${PILL} ${focusVisibleRing} ${borderTransparent} ${badgeBlocking.bg} ${badgeBlocking.text} ${badgeBlockingHoverBg}`}
        data-ask-rewrite="failed"
        onClick={onRetry}
        title={`${rewrite.reason ?? "Dispatch did not apply the rewrite."} Try again (r)`}
        type="button"
      >
        Rewrite failed
      </button>
    );
  }
  const tone = showingOriginal
    ? badgePrimaryOutline
    : `${borderTransparent} ${badgePrimary.bg} ${badgePrimary.text} ${badgePrimaryHoverBg}`;
  return (
    <button
      aria-pressed={showingOriginal}
      className={`${PILL} ${focusVisibleRing} ${tone}`}
      data-ask-rewrite={showingOriginal ? "original" : "done"}
      onClick={onToggle}
      title={showingOriginal ? "Show the rewrite (w)" : "Show the original wording (w)"}
      type="button"
    >
      {showingOriginal ? "Original" : "Rewritten"}
    </button>
  );
}
