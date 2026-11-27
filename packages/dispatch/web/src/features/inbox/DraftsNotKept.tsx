import type { ReactNode } from "react";

import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
} from "../../theme/shell-classes";
import {
  DRAFT_KEPT_UNITS,
  type NotKeptReason,
  STORE_KEPT_UNITS,
  useDraftsNotKept,
} from "./draft-store";

/** What the shell says for each reason the tab keeps a draft for no reload (`draft-store.ts`). */
const NOT_KEPT_WORDS: Record<NotKeptReason, string> = {
  refused:
    "This browser refused to keep a draft you have not sent (its storage for this tab is full, or turned off), so the draft is lost if the page reloads. It stays while the page is open.",
  "store full": `This tab keeps at most ${STORE_KEPT_UNITS.toLocaleString("en-US")} characters of drafts of one kind for a reload and holds that many already, so a newer draft you have not sent is lost if the page reloads. It stays while the page is open; send or discard an older draft and it is kept.`,
  "too long": `A draft you have not sent is longer than ${DRAFT_KEPT_UNITS.toLocaleString("en-US")} characters, the most this tab keeps of one draft for a reload, so it is lost if the page reloads. It stays while the page is open.`,
};

/** The shell's word on drafts the tab does not keep for a reload, in its status corner: every
 *  other draft outlives a reload of the page, so one that does not is never lost with no word.
 *  Nothing while the tab keeps every draft. */
export function DraftsNotKept(): ReactNode {
  const reasons = useDraftsNotKept();
  if (reasons.length === 0) return null;
  return (
    <div
      className={`space-y-1 rounded-lg border px-3 py-2 text-xs font-medium shadow-lg ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
      data-testid="drafts-not-kept"
      role="status"
    >
      {reasons.map((reason) => (
        <p key={reason}>{NOT_KEPT_WORDS[reason]}</p>
      ))}
    </div>
  );
}
