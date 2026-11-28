import { type ReactNode, useEffect } from "react";

import { useInboxWrites } from "./ask-writes";
import { AskSnoozeContext, AskSnoozeWritesProvider } from "./useAskSnooze";

// The snooze writes `Inbox.test.tsx`, `ReadingPane.test.tsx` and `useAskSnooze.test.tsx` draw
// their controls under.

/** The Inbox's snooze writes over `children`, as the Inbox provides them to every Snooze it draws
 *  (`AskSnoozeWritesProvider`), with each set of asks whose write is out handed to `onOut` as it
 *  changes. */
export function SnoozeWrites({
  children,
  onOut,
}: {
  children: ReactNode;
  onOut?: (out: ReadonlySet<string>) => void;
}): ReactNode {
  return (
    <AskSnoozeWritesProvider>
      {children}
      {onOut === undefined ? null : <ReportOut onOut={onOut} />}
    </AskSnoozeWritesProvider>
  );
}

/** Hands `onOut` each set of asks whose snooze write is out, as it changes. */
function ReportOut({ onOut }: { onOut: (out: ReadonlySet<string>) => void }): ReactNode {
  const { out } = useInboxWrites(AskSnoozeContext);
  useEffect(() => {
    onOut(out);
  }, [onOut, out]);
  return null;
}
