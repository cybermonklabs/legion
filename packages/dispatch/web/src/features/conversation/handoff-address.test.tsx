/**
 * A handoff's page address on today's issue page, the page a reader with the new issue page off
 * reads: `/issues/<KEY>/handoffs/<id>` opens the Conversation tab with that handoff's card marked,
 * its fields open and brought into view. The tab's own landing marks and scrolls the card a link
 * names (`focusItemId`), as it does for every other card; the card opens its fields itself once it
 * is the one the link names (`HandoffTurn`).
 */
import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { http } from "msw";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import { api } from "../../api/client";
import type { Handoff, HandoffSummary } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { IssuePage } from "../issue-page/IssuePage";
import { MarginProvider } from "../margin/margin-context";
import { DESKTOP_WIDTH, issue, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";

// The card draws its fields through the shared renderer, whose Markdown engine loads lazily, and
// the rows run in random order, so the file loads the engine before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The handoff the address names: the older of the issue's two, so it stands below the newer. */
const ADDRESSED = "0195f3a4-7b2c-7d10-9a3e-5c1d2e3f4a5b";
const NEWER = "0195f3a4-7b2c-7d10-9a3e-5c1d2e3f4a5c";

/** A handoff of the issue, written by the implementer at `at`, whose summary is `summary`. */
function handoff(id: string, version: number, summary: string, at: string): Handoff {
  return {
    author: { id: "fedcba9876543210", kind: "session", origin: { session_title: "Implementer" } },
    body: JSON.stringify({ schemaVersion: 1, summary }),
    created_at: at,
    id,
    issue_key: issue.key,
    phase: "implement",
    version,
  };
}

test("the address of a handoff opens the Conversation tab with that handoff's card open", async () => {
  const handoffs = [
    handoff(NEWER, 2, "Second pass", "2026-09-10T09:05:00Z"),
    handoff(ADDRESSED, 1, "First pass", "2026-09-10T09:00:00Z"),
  ];
  const listed: HandoffSummary[] = handoffs.map(({ body: _body, ...summary }) => summary);
  const state = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getMyState").mockResolvedValue({ [issue.key]: state }),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listHandoffs").mockResolvedValue(listed),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
  ];
  const getHandoff = spyOn(api, "getHandoff").mockImplementation(async (id) => {
    const found = handoffs.find((each) => each.id === id);
    if (found === undefined) throw new Error(`no handoff ${id}`);
    return found;
  });
  // The header's own reads, each answered with the API's empty shape.
  answerRequests(
    http.get("/api/v1/projects/:project/architecture-sources", () => Response.json([]))
  );
  // Each element the page brings into view, as the browser would scroll to it.
  const landed: Element[] = [];
  const intoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    landed.push(this);
  });
  const restoreViewport = stubViewport(DESKTOP_WIDTH);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const runtime = fakeDocumentRuntime();
  const view = render(
    <MemoryRouter
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      initialEntries={[buildIssuePath({ handoffId: ADDRESSED, key: issue.key, kind: "handoff" })]}
    >
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={runtime.runtime}>
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
    await screen.findByRole("tab", { name: "Conversation", selected: true });
    const card = await waitFor(() => {
      const found = document.querySelector<HTMLElement>(`[data-turn="handoff:${ADDRESSED}"]`);
      expect(found, "the Conversation tab draws no card of the handoff the address names").not.toBe(
        null
      );
      return found as HTMLElement;
    });
    expect(card.getAttribute("aria-current"), "the handoff's card is not the one marked").toBe(
      "true"
    );
    await waitFor(() =>
      expect(
        within(card).getByRole("button", { name: "Hide fields" }),
        "the address left the handoff's fields shut"
      ).toHaveAttribute("aria-expanded", "true")
    );
    await within(card).findByText("First pass");
    await waitFor(() =>
      expect(landed, "the address did not bring the handoff's card into view").toContain(card)
    );
    // The other handoff's card is no part of the address, and stays as a reader finds it.
    const other = document.querySelector<HTMLElement>(`[data-turn="handoff:${NEWER}"]`);
    if (other === null) throw new Error("the Conversation tab draws no card of the newer handoff");
    expect(other.getAttribute("aria-current")).toBe(null);
    expect(within(other).getByRole("button", { name: "Show fields" })).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    expect(getHandoff.mock.calls, "the page read a body the address did not open").toEqual([
      [ADDRESSED],
    ]);
  } finally {
    view.unmount();
    restoreViewport();
    intoView.mockRestore();
    getHandoff.mockRestore();
    for (const read of reads) read.mockRestore();
  }
});
