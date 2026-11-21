import { expect, setSystemTime, spyOn, test } from "bun:test";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import { OutlineWriterProvider } from "./outline-writer";

// R7: the writer adds no request. It reads the issue's asks, comments and messages under the keys
// the tree reads them by (`useIssueTree.ts`), and keeps them fresh as the tree does, by events
// and never by their age (decision 180). Staleness is per observer, so under the app's 30 s
// `staleTime` and its window-focus refetch (`main.tsx`) a reader who comes back to the window
// more than 30 s after the load would read all three again for the writer alone.
test("a window focus more than 30 s after the load reads the writer's asks, comments and messages no more", async () => {
  const reads = [
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  // The app's client's defaults (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
  try {
    const view = render(
      <QueryClientProvider client={client}>
        <OutlineWriterProvider issueKey="CORE-1">{null}</OutlineWriterProvider>
      </QueryClientProvider>
    );
    try {
      await waitFor(() => expect(reads.map((read) => read.mock.calls.length)).toEqual([1, 1, 1]));
      await waitFor(() => expect(client.isFetching()).toBe(0));
      // The reader leaves the window and comes back to it more than 30 s after the load.
      setSystemTime(Date.now() + 31_000);
      act(() => {
        focusManager.setFocused(false);
        focusManager.setFocused(true);
      });
      // Where a read the focus started would show: the client's focus listener awaits the paused
      // mutations it resumes, of which there are none, and then refetches each query whose
      // observer refetches on focus, which calls the query's function in the same turn, so a
      // started read shows in the counts below.
      await act(async () => {});
      expect(
        reads.map((read) => read.mock.calls.length),
        "the window's focus read the asks, the comments or the messages again"
      ).toEqual([1, 1, 1]);
    } finally {
      view.unmount();
    }
  } finally {
    focusManager.setFocused(undefined);
    setSystemTime();
    for (const read of reads) read.mockRestore();
    client.clear();
  }
});
