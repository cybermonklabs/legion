import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import type { WriteBounds, WriteMethod } from "../api/client";
import { PendingWritesIndicator } from "../features/writes/PendingWritesIndicator";
import { plainWrite, type Writes } from "../features/writes/write-store";

/** One write the indicator shows, labelled `label`, its attempt `write`, which makes a request of
 *  `method`. */
export function indicatorWrite(
  writes: Writes,
  label: string,
  method: WriteMethod,
  write: (bounds: WriteBounds) => Promise<unknown>
): void {
  act(() => {
    void writes
      .run({ detail: null, kind: plainWrite, label, method, presentation: "indicator", write })
      .catch(() => undefined);
  });
}

/** The shell's indicator over one app's write queue, which the test drives directly: the rows are
 *  the queue's, whichever surface made them. */
export function mountIndicator(): { client: QueryClient; unmount: () => void } {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const view = render(<PendingWritesIndicator />, { wrapper });
  return { client, unmount: () => view.unmount() };
}
