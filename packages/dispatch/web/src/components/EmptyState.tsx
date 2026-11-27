import type { ReactNode } from "react";

import {
  borderStrong,
  focusVisibleRing,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
} from "../theme/classes";

export function EmptyState({
  action,
  label,
  marks,
  message,
  tabIndex,
}: {
  action?: ReactNode;
  label: string;
  /** The marks a page finds the message by: the Inbox's `data-inbox-empty`. */
  marks?: Readonly<Record<`data-${string}`, string>>;
  message: string;
  /** `-1` where a page hands the keyboard's focus to the message: the Inbox does, when the
   *  reader's action leaves no item in the list (R8). */
  tabIndex?: -1;
}): ReactNode {
  const focusRing =
    tabIndex === undefined ? "" : ` outline-none focus-visible:ring-2 ${focusVisibleRing}`;
  return (
    <section
      {...marks}
      aria-label={label}
      className={`rounded-xl border border-dashed px-4 py-6 text-center text-sm ${borderStrong} ${surfaceMutedBg} ${textMutedOnSurfaceMuted}${focusRing}`}
      tabIndex={tabIndex}
    >
      <p>{message}</p>
      {action === undefined ? null : <div className="mt-3">{action}</div>}
    </section>
  );
}
