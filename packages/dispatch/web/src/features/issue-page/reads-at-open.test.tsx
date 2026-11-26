import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { AppProviders, commentNodes, stubReads, treeOf } from "./slots/stage-fixtures";

/**
 * The reads the page starts as it mounts (`IssueOutlinePage.tsx`): the tree read of the view the
 * address opens, the six reads the tree is built from beside it and the reader's login, each
 * under the key and with the function its hook reads it by, so none of them waits for the issue
 * read, which the page's slots are mounted after, and none is sent a second time once they are.
 */

/** The api client's reads the page sends with a load of Conversation, each by the name the case
 *  says it by: the tree read, the six reads and the reader's login, as the request log names
 *  them (`requests`). */
const READS_AT_OPEN = {
  asks: "listIssueAskThreads",
  comments: "listComments",
  coverage: "getIssueCoverage",
  decisions: "getIssueDecisions",
  messages: "listIssueMessages",
  results: "getIssueResults",
  tree: "getIssueTree",
  whoami: "whoAmI",
} as const satisfies Record<string, keyof typeof api>;

type ReadAtOpen = keyof typeof READS_AT_OPEN;

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
// Every read the page's slots reach, answered (`stubReads`): the comments with the issue's two
// threads, and the tree read with them, each view's read (`?view=`) with an empty view, and the
// header's reads of the reader's state and the project's architecture sources. Drawn at 1440 px
// (`DESKTOP_WIDTH`), where the Contents pane stands beside the stage.
beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
    ),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
  ];
  restoreViewport = stubViewport(DESKTOP_WIDTH);
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The request log: each read of `READS_AT_OPEN` the api client was asked for, in the order it was
 *  asked, once per request, by the case's name for it; a view's read (`?view=`) is no tree read
 *  of the outline and is left out. */
function requests(): ReadAtOpen[] {
  const sent: { readonly at: number; readonly read: ReadAtOpen }[] = [];
  for (const read of Object.keys(READS_AT_OPEN) as ReadAtOpen[]) {
    const spy = api[READS_AT_OPEN[read]] as unknown as Mock<(...args: unknown[]) => unknown>;
    spy.mock.calls.forEach((call, index) => {
      const query = call[1] as { readonly view?: string } | undefined;
      if (read === "tree" && query?.view !== undefined) return;
      sent.push({ at: spy.mock.invocationCallOrder[index] ?? 0, read });
    });
  }
  return sent.sort((one, other) => one.at - other.at).map(({ read }) => read);
}

// The page mounted no slot until the issue read answered, and each slot sent the reads it draws
// from as it mounted: on a cold load the tree read, the six reads and the reader's login waited
// for the lazy chunk and then for the issue read, one after the other. They are sent as the page
// mounts, beside the issue read, which is held here: all of them go out while it is still out.
// Once it answers the slots read each of them under the key it was started under, and send none
// again.
test("the tree read and the six reads are sent before the issue read answers", async () => {
  const issueRead = Promise.withResolvers<IssueDetails>();
  const issue = spyOn(api, "getIssue").mockReturnValue(issueRead.promise);
  // The app's client (`main.tsx`): no read tried again here, and a read 30 s old read again by a
  // new observer that sets no freshness of its own, as the view row's of the reader's login.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const opened = (Object.keys(READS_AT_OPEN) as ReadAtOpen[]).sort();
  try {
    const view = render(
      <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
        <AppProviders client={client}>
          <IssueOutlinePage />
        </AppProviders>
      </MemoryRouter>
    );
    await waitFor(() => {
      expect(issue, "the page never read the issue").toHaveBeenCalledTimes(1);
      expect(
        [...requests()].sort(),
        "a read the tree is built from waited for the issue read"
      ).toEqual(opened);
    });
    expect(
      view.container.querySelector("[data-issue-body]"),
      "the page drew its slots with the issue read out"
    ).toBeNull();

    await act(async () => issueRead.resolve(issueDetails()));
    await waitFor(() => {
      expect(client.isFetching(), "a read the page sent is still out").toBe(0);
      expect(
        view.container.querySelector('[data-issue-body] [data-node-row][data-node-id="head-a"]'),
        "the outline drew no row of the first thread once the issue read answered"
      ).not.toBeNull();
    });
    expect(
      [...requests()].sort(),
      "a read started as the page mounted was sent again once its slots mounted"
    ).toEqual(opened);
    view.unmount();
  } finally {
    issue.mockRestore();
    client.clear();
  }
});
