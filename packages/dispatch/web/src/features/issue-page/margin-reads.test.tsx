import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { specArtifact } from "../margin/margin-fixture";
import { type MarginOwner, type MarkPlacement, useMarginItems } from "../margin/useMarginItems";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { appClient, focusPastStaleness, restoreWindowFocus } from "./window-focus-fixture";

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
  restoreWindowFocus();
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

/** The decisions beside the issue's spec, answered empty: the margin reads them only while the
 *  spec is shown (`useMarginItems`' `showsDecisions`). */
function stubDecisionsRead(): Mock<typeof api.getIssueDecisions> {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue({
    decisions: [],
    document: null,
    open_questions: [],
  });
  const before = restore;
  restore = () => {
    before();
    read.mockRestore();
  };
  return read;
}

/** The margin's reads of the issue (`useMarginItems`, which reads the asks through
 *  `useAnsweredAsks`): in the Details drawer where `inDrawer` is given, as the new page's shell
 *  draws the margin, and with no drawer otherwise, as every other page draws it. Where `spec` is
 *  given, the margin is beside that document, shown, and so reads its decisions too. */
function renderMargin(client: QueryClient, inDrawer: MarginDrawer | undefined, spec?: Artifact) {
  return renderHook(
    () =>
      useMarginItems(OWNER, "comments", spec, NO_PLACEMENTS, NO_PLACEMENTS, undefined, {
        documentShown: spec !== undefined,
      }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>
          <MarginSlotProvider drawer={inDrawer}>{children}</MarginSlotProvider>
        </QueryClientProvider>
      ),
    }
  );
}

/** Waits until each read, and the decisions' read where it is given, has gone out once and every
 *  one has answered. */
async function firstAnswers(
  client: QueryClient,
  reads: MarginReads,
  decisions?: Mock<typeof api.getIssueDecisions>
): Promise<void> {
  await waitFor(() => {
    for (const read of Object.values(reads)) expect(read).toHaveBeenCalledTimes(1);
    if (decisions !== undefined) expect(decisions).toHaveBeenCalledTimes(1);
    expect(client.isFetching()).toBe(0);
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

// Beside the shown spec the margin reads its decisions too, which the outline also reads under
// this key and keeps fresh by events (`useIssueTree.ts`): in Details the same focus reads them
// again no more than it reads the asks and the comments.
test("in Details, a focus past 30 s sends no read of the decisions beside the shown spec", async () => {
  const reads = stubMarginReads();
  const decisions = stubDecisionsRead();
  const client = appClient();
  const view = renderMargin(client, drawer, specArtifact);
  try {
    await firstAnswers(client, reads, decisions);
    await focusPastStaleness(client);
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(2);
      expect(reads.getMyState).toHaveBeenCalledTimes(2);
    });
    expect(decisions).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

// With no drawer the decisions keep the app's defaults, as the asks and the comments do: the same
// focus reads them again.
test("with no drawer, a focus past 30 s reads the decisions beside the shown spec again", async () => {
  const reads = stubMarginReads();
  const decisions = stubDecisionsRead();
  const client = appClient();
  const view = renderMargin(client, undefined, specArtifact);
  try {
    await firstAnswers(client, reads, decisions);
    await focusPastStaleness(client);
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(2);
      expect(reads.getMyState).toHaveBeenCalledTimes(2);
      expect(decisions).toHaveBeenCalledTimes(2);
    });
  } finally {
    view.unmount();
  }
});
