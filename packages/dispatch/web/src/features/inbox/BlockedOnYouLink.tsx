import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { CredentialPendingRow, InboxRow } from "../../api/types";
import {
  surfaceMutedStrongBg,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurfaceMuted,
} from "../../theme/classes";
import { borderDefault, surfaceMutedBg, textSecondaryOnSurface } from "../../theme/shell-classes";
import { formatAskAge } from "./ask-age";
import { needsYou } from "./BlockedOnYou";

/**
 * The Inbox's `Blocked on you` strip and the project page's pill, which count what waits for the
 * reader by `needsYou`'s one rule (`BlockedOnYou.tsx`). A file of its own, apart from that rule,
 * because the shell's `Needs you N` imports it (`useNeedsYouCount`, through
 * `useInboxCount.ts`), which puts that module in the entry chunk, and a module's every function
 * goes where the module goes. Only the Inbox's list (`Inbox.tsx`) and the project page
 * (`ProjectPage.tsx`) draw this, and each is a chunk of its own, so here the build puts it with
 * them rather than in the entry every page parses before it draws (dispatch://INFRA-1109, R13).
 */
export function BlockedOnYou({
  asks,
  className,
  credentialRequests = [],
  variant = "banner",
  viewer,
}: {
  asks: readonly InboxRow[];
  className?: string;
  /** The credential requests the Inbox lists above its asks; the project pill has none. */
  credentialRequests?: readonly CredentialPendingRow[];
  variant?: "banner" | "pill";
  /** The signed-in person's lowercase login, whose questions this counts; nothing is counted
   *  until the caller's `/auth/whoami` read resolves. */
  viewer: string | undefined;
}): ReactNode {
  const { count, oldest } = needsYou(asks, credentialRequests, viewer);
  if (count === 0) return null;

  if (variant === "pill") {
    return (
      <Link
        className={`${className ?? ""} inline-flex min-h-11 items-center rounded-full px-3 text-xs font-medium md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
        to="/"
      >
        Blocked on you · {count}
      </Link>
    );
  }

  // A count of every waiting ask, not one urgency, so the strip stays structural: the spec
  // reserves the urgency hues for urgency and priority. `w-full` is load-bearing - below
  // 1280 px an unlayered `a:not(.prose a) { display: inline-flex }` in styles.css beats
  // Tailwind's `block`, and without it the strip sits beside the controls above the list.
  return (
    <Link
      className={`block w-full rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
      to="/"
    >
      Blocked on you: {count} {count === 1 ? "item" : "items"}
      {oldest === undefined ? null : `, oldest ${formatAskAge(oldest)}`}
    </Link>
  );
}
