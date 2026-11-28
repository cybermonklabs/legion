import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { issueDetails } from "./decisions-filter/decisions-fixtures";
import { IssuePage } from "./IssuePage";

/**
 * `/issues/<key>/documents` is the new issue page's Documents (dispatch://INFRA-3940 R2, R4), an
 * address `refs/routes.ts` reads as the issue's route `issue-documents`. Today's page has no tab
 * there, and with the setting off it keeps drawing what it drew before the route was read: Not
 * found (R12, root R33). Its file stands here, under the new page's directory, because a new file
 * under `features/issue/` is one that part N's `c-today-unchanged` refuses.
 */

const ISSUE: IssueDetails = issueDetails();

test("today's page draws Not found at an issue's documents address with the setting off", async () => {
  // The setting off: alice is signed in and has chosen no page, which reads as today's.
  window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.page"));
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(ISSUE);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    [ISSUE.key]: { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${ISSUE.key}/documents`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={fakeDocumentRuntime().runtime}>
            <MarginProvider>
              <Routes>
                <Route element={<IssuePage />} path="/issues/:key/*" />
              </Routes>
            </MarginProvider>
          </DocumentRuntime.Provider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByRole("heading", { name: "Page not found" });
    expect(screen.getByRole("link", { name: "Back to inbox" })).toBeInTheDocument();
    expect(document.title).toBe("Not found · Dispatch");
    // Today's page, with none of its tabs and none of the new page.
    expect(screen.queryByRole("tablist", { name: "Issue detail" })).not.toBeInTheDocument();
    expect(view.container.querySelector("[data-issue-page]")).toBeNull();
    expect(getIssue).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getMyState.mockRestore();
  }
});
