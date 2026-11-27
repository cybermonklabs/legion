import { beforeAll, expect, setSystemTime, spyOn, test } from "bun:test";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";

import { nextFrame } from "../../__tests__/frames";
import { api } from "../../api/client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

// When the writer unmounts while its `focus()` is waiting on an animation frame for a field to
// draw, that frame is cancelled: a box mounted afterwards does not lose the reader's focus to
// the unmounted writer.
test("unmounting the writer cancels a pending focus frame so a later box does not take the focus", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["asks", "CORE-1"], []);
  client.setQueryData(["asks", "CORE-1", "thread"], []);
  client.setQueryData(["comments", "CORE-1"], []);
  client.setQueryData(["issue-messages", "CORE-1"], []);
  const held: { writer?: OutlineWriter } = {};
  function Capture(): null {
    held.writer = useOutlineWriter();
    return null;
  }
  const first = render(
    <QueryClientProvider client={client}>
      <OutlineWriterProvider issueKey="CORE-1">
        <Capture />
      </OutlineWriterProvider>
    </QueryClientProvider>
  );
  let second: RenderResult | undefined;
  try {
    if (held.writer === undefined) throw new Error("the provider drew no writer");
    // Queue a focus frame while no composer field is in the document, then unmount the writer.
    act(() => held.writer?.focus());
    first.unmount();

    // Mount a fresh box with a field of its own, and let more frames run than the writer waits.
    second = render(
      <section data-testid="issue-composer">
        <textarea aria-label="Comment" />
      </section>
    );
    const field = second.getByLabelText("Comment");
    for (let frame = 0; frame < 12; frame += 1) {
      await act(() => nextFrame());
    }
    expect(field).not.toHaveFocus();
  } finally {
    second?.unmount();
    first.unmount();
    client.clear();
  }
});
