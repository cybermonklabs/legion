import { afterEach, expect, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { focusManager, type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import { type MarginOwner, type MarkPlacement, useMarginItems } from "../margin/useMarginItems";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";

const OWNER: MarginOwner = { key: "CORE-1", kind: "issue" };

/** No card of the margin is placed: what is asked here is which reads go out, not where a card
 *  stands. */
const NO_PLACEMENTS: ReadonlyMap<string, MarkPlacement> = new Map();

/** The Details drawer, which the new issue page's shell hands the margin and no other page does. */
const drawer: MarginDrawer = { open: true, setOpen: () => {}, setPage: () => {} };

let restore = () => {};

afterEach(() => {
  restore();
  restore = () => {};
  setSystemTime();
  focusManager.setFocused(undefined);
});

/** Every read the margin of an issue sends on its Comments tab with no pin: the inbox, the
 *  reader's state, the issue's comments and its asks (`useAnsweredAsks`). */
interface MarginReads {
  readonly getInbox: Mock<typeof api.getInbox>;
  readonly getMyState: Mock<typeof api.getMyState>;
  readonly listComments: Mock<typeof api.listComments>;
  readonly listIssueAsks: Mock<typeof api.listIssueAsks>;
}

/** Each of the margin's reads, answered empty. */
function stubMarginReads(): MarginReads {
  const reads: MarginReads = {
    getInbox: spyOn(api, "getInbox").mockResolvedValue([]),
    getMyState: spyOn(api, "getMyState").mockResolvedValue({}),
    listComments: spyOn(api, "listComments").mockResolvedValue([]),
    listIssueAsks: spyOn(api, "listIssueAsks").mockResolvedValue([]),
  };
  restore = () => {
    for (const read of Object.values(reads)) read.mockRestore();
  };
  return reads;
}

/** The app's client with the defaults `main.tsx` gives every read: stale 30 s after it answered,
 *  and fetched again on window focus once stale. No retry, so a stub's answer is the read's. */
function appClient(): QueryClient {
  return new DispatchQueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
}

/** The margin's reads of the issue (`useMarginItems`, which reads the asks through
 *  `useAnsweredAsks`): in the Details drawer where `inDrawer` is given, as the new page's shell
 *  draws the margin, and with no drawer otherwise, as every other page draws it. */
function renderMargin(client: QueryClient, inDrawer: MarginDrawer | undefined) {
  return renderHook(
    () => useMarginItems(OWNER, "comments", undefined, NO_PLACEMENTS, NO_PLACEMENTS, undefined),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>
          <MarginSlotProvider drawer={inDrawer}>{children}</MarginSlotProvider>
        </QueryClientProvider>
      ),
    }
  );
}

/** Waits until each read has gone out once and every one has answered. */
async function firstAnswers(client: QueryClient, reads: MarginReads): Promise<void> {
  await waitFor(() => {
    for (const read of Object.values(reads)) expect(read).toHaveBeenCalledTimes(1);
    expect(client.isFetching()).toBe(0);
  });
}

/** The reader comes back to the tab 31 s after the reads answered, past the app's staleness.
 *  The client's focus listener awaits `resumePausedMutations` before it fetches again, so
 *  awaiting the same call after it finds any read the focus sends already started. */
async function focusPastStaleness(client: QueryClient): Promise<void> {
  setSystemTime(Date.now() + 31_000);
  await act(async () => {
    focusManager.setFocused(false);
    focusManager.setFocused(true);
    await client.resumePausedMutations();
  });
}

// On the new issue page the outline reads the issue's asks and comments under the margin's keys
// and keeps them fresh by events (`useIssueTree.ts`), so the margin in the Details drawer holds
// them the same way: a window focus past the app's 30 s staleness reads neither again. The inbox
// and the reader's state keep the app's defaults there too, so their second read shows the focus
// was handled.
test("in Details, a focus past 30 s sends no read of the margin's asks or comments", async () => {
  const reads = stubMarginReads();
  const client = appClient();
  const view = renderMargin(client, drawer);
  try {
    await firstAnswers(client, reads);
    await focusPastStaleness(client);
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(2);
      expect(reads.getMyState).toHaveBeenCalledTimes(2);
    });
    expect(reads.listIssueAsks).toHaveBeenCalledTimes(1);
    expect(reads.listComments).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

// Every other page has no drawer, and there the margin keeps the app's defaults: the same focus
// reads the asks and the comments again.
test("with no drawer, a focus past 30 s reads the margin's asks and comments again", async () => {
  const reads = stubMarginReads();
  const client = appClient();
  const view = renderMargin(client, undefined);
  try {
    await firstAnswers(client, reads);
    await focusPastStaleness(client);
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(2);
      expect(reads.getMyState).toHaveBeenCalledTimes(2);
      expect(reads.listIssueAsks).toHaveBeenCalledTimes(2);
      expect(reads.listComments).toHaveBeenCalledTimes(2);
    });
  } finally {
    view.unmount();
  }
});
