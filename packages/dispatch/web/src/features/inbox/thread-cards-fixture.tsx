import type { ReactNode } from "react";

import type { MyReplyRow } from "../../api/types";
import { ThreadRowCard } from "./ThreadRowCard";

// The thread cards `read-state.test.tsx` and `ThreadBody.test.tsx` both render.

/** Thread cards drawn as the Inbox's list draws them, each taking the list's one hold through the
 *  read a Mark read makes (`ThreadRowCard`). */
export function ListedCards({ threads }: { readonly threads: readonly MyReplyRow[] }): ReactNode {
  return threads.map((thread) => <ThreadRowCard key={thread.thread_id} thread={thread} />);
}
