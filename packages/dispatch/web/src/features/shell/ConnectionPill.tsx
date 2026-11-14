import type { ReactNode } from "react";

import { useLiveSocketPill } from "../../api/live-socket";
import {
  badgeBlocking,
  calloutDangerBg,
  calloutDangerBorder,
  calloutWarningBorder,
  statusConnecting,
} from "../../theme/classes";

/**
 * Whether the page's live updates are reaching it, in the shell's status corner.
 *
 * It says nothing while the socket is up, which is every second of a load that goes well. What
 * it says when the socket is down is not the page's chrome: it is the reason what the reader
 * is looking at may already be out of date, and it carries the reload that is their way out of
 * it. So it waits for no editor, where the rest of the shell's own parts are drawn after the
 * editor's first batch (dispatch://INFRA-341 R11, `features/doc/AfterTheFirstBatch.tsx`). A
 * hold over it would run longest on exactly the load whose socket is gone, and the pill stands
 * in a corner that keeps no box, so there would be nothing there for a reader to press to ask
 * for it. It costs the load one subscription to a state the shell already holds and draws
 * nothing at all while the socket is up.
 *
 * Its own component, so the state it watches re-renders this pill rather than the whole shell.
 */
export function ConnectionPill(): ReactNode {
  const connection = useLiveSocketPill();
  // A socket Dispatch refuses carries no status the page can read, so nothing here is terminal:
  // once reconnecting has gone on for two minutes it offers a reload, and the page keeps trying
  // underneath.
  if (connection === "stuck") {
    return (
      <p
        aria-live="polite"
        className={`flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${calloutDangerBorder} ${calloutDangerBg} ${badgeBlocking.text}`}
        data-testid="connection-pill"
      >
        Live updates are not reaching this page
        <button className="underline" onClick={() => window.location.reload()} type="button">
          Reload
        </button>
      </p>
    );
  }
  if (connection === "reconnecting") {
    return (
      <p
        aria-live="polite"
        className={`rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${calloutWarningBorder} ${statusConnecting.bg} ${statusConnecting.text}`}
        data-testid="connection-pill"
      >
        Reconnecting…
      </p>
    );
  }
  return null;
}
