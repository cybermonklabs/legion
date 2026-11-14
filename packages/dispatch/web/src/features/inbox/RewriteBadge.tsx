import type { ReactNode } from "react";

import {
  badgeBlocking,
  badgeBlockingHoverBg,
  badgeLow,
  badgePrimary,
  badgePrimaryEdge,
  badgePrimaryHoverBg,
  badgePrimaryOutline,
  borderTransparent,
  groupFocusVisibleRing,
} from "../../theme/classes";
import type { Rewrite } from "./ask-rewrite";

/** Every state is the same box: a 1 px border, clear unless the state draws it (Original). */
const PILL =
  "inline-flex shrink-0 items-center rounded-full border px-2 py-px text-xs leading-4 font-medium whitespace-nowrap";
/** A pressable state's button is the pill's hit area: below 1280 px every button is a 44 px tap
 *  target (`styles.css`), and the pill inside keeps its own size; the focus ring is the pill's. */
const HIT = "group inline-flex shrink-0 items-center justify-center outline-none";

/**
 * The one mark a rewrite leaves on an agent's text - an ask's card, a comment, an issue's title -
 * beside who wrote it and when: **Rewriting…** while it runs, **Rewritten** once it is in place,
 * whoever asked for it (the reader, or Dispatch's check before the reader saw the text), and
 * **Rewrite failed** when Dispatch did not apply it. Rewritten shows the original wording in
 * place and reads **Original** until it is pressed again (`w`); Rewrite failed tries again
 * (`r`), and why it failed is its tooltip alone. `onRetry` is absent where nothing can be
 * rewritten again from the page.
 */
export function RewriteBadge({
  onRetry,
  onToggle,
  rewrite,
  running = false,
  showingOriginal,
}: {
  onRetry?: () => void;
  onToggle: () => void;
  rewrite: Pick<Rewrite<unknown>, "state" | "reason"> | undefined;
  /** The reader's own rewrite is out, before the read says so. */
  running?: boolean;
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
        className={HIT}
        data-ask-rewrite="failed"
        onClick={onRetry}
        title={`${rewrite.reason ?? "Dispatch did not apply the rewrite."} Try again (r)`}
        type="button"
      >
        <span
          className={`${PILL} ${groupFocusVisibleRing} ${borderTransparent} ${badgeBlocking.bg} ${badgeBlocking.text} ${badgeBlockingHoverBg}`}
        >
          Rewrite failed
        </span>
      </button>
    );
  }
  const tone = showingOriginal
    ? badgePrimaryOutline
    : `${badgePrimaryEdge} ${badgePrimary.bg} ${badgePrimary.text} ${badgePrimaryHoverBg}`;
  return (
    <button
      aria-pressed={showingOriginal}
      className={HIT}
      data-ask-rewrite={showingOriginal ? "original" : "done"}
      onClick={onToggle}
      title={showingOriginal ? "Show the rewrite (w)" : "Show the original wording (w)"}
      type="button"
    >
      <span className={`${PILL} ${groupFocusVisibleRing} ${tone}`}>
        {showingOriginal ? "Original" : "Rewritten"}
      </span>
    </button>
  );
}
