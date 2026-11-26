import type { ReactNode } from "react";

import { useListReadBound } from "../../api/read-bound";
import type { MyReplyRow } from "../../api/types";
import { ThreadRowCard } from "./ThreadRowCard";

// The thread cards `read-state.test.tsx` and `ThreadBody.test.tsx` both render.

/** Thread cards drawn as the Inbox's list draws them, under the list's one hold through the read
 *  a Mark read makes (`useListReadBound`). */
export function ListedCards({ threads }: { readonly threads: readonly MyReplyRow[] }): ReactNode {
  const [unsettledOf, holdThrough] = useListReadBound();
  return threads.map((thread) => (
    <ThreadRowCard
      holdThrough={holdThrough}
      key={thread.thread_id}
      thread={thread}
      unsettled={unsettledOf(thread.thread_id)}
    />
  ));
}
