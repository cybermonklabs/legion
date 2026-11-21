import { type ReactNode, useEffect, useState } from "react";

import {
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { type ReconnectWait, reconnectingNotice } from "./connection";
import type { PendingState } from "./pending-sync";

/**
 * What a person sees while the document waits to reconnect: that it is reconnecting, how long
 * until the next attempt, what this browser keeps of their typing meanwhile (`reconnectingNotice`),
 * and a button that sends the waiting attempt now.
 *
 * The connection's backoff grows after every socket that closed within `STEADY_SOCKET_MS`, up to
 * a wait of 15 to 30 s (`connection.ts`, dispatch://INFRA-179). A wait that long is the reason
 * this notice exists: the page made the person wait on purpose, so it says so and offers the way
 * out. `Retry now` brings that one attempt forward and nothing more - the backoff keeps the growth
 * it had, so a person pressing it cannot become the burst the backoff exists to stop.
 *
 * The seconds tick here rather than in the page, so the countdown costs the document nothing. They
 * are hidden from a screen reader, which hears the status line instead: it says the same thing
 * without the count, so the reader is not told the time again every second.
 */
export function ReconnectNotice({
  onRetryNow,
  pending,
  wait,
}: {
  onRetryNow: () => void;
  pending: PendingState | undefined;
  wait: ReconnectWait;
}): ReactNode {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  // A wait already spent reads as the attempt that is about to go out, never as a negative count.
  const secondsLeft = Math.max(0, Math.ceil((wait.at - now) / 1000));

  return (
    <div className="flex flex-wrap items-center gap-3">
      <p aria-hidden>{reconnectingNotice(pending, secondsLeft)}</p>
      <p className="sr-only" role="status">
        {reconnectingNotice(pending)}
      </p>
      <button
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
        onClick={onRetryNow}
        type="button"
      >
        Retry now
      </button>
    </div>
  );
}
