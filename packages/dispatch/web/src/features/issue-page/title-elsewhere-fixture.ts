import type { QueryClient } from "@tanstack/react-query";
import { act } from "@testing-library/react";

import type { IssueDetails } from "../../api/types";

/** Another reader's write of the title of CORE-1, the issue the page's cases open, which reaches
 *  the page as an event's refresh does. */
export function titleWrittenElsewhere(queryClient: QueryClient, title: string): void {
  act(() => {
    queryClient.setQueryData<IssueDetails>(
      ["issue", "CORE-1"],
      (current) => current && { ...current, title }
    );
  });
}
