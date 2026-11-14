import { type Mock, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { GoingCapability, GoingEvent, IssueReferenceSummary } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SearchPalette } from "./SearchPalette";
import { searchAnswer } from "./search-fixture";

/**
 * The command bar on a Dispatch that answers, as the answer's and the operations' rows drive it:
 * every part of the bar configured, one hit for whatever is typed, and each run's stream handed
 * to the row to send frames down, the bar's abort rejecting it as a browser's does.
 */

/** A Dispatch with an answering model that draws blocks, a deciding model it reaches, and a
 *  session and a label for a new issue. */
export const CONFIGURED_CAPABILITY: GoingCapability = {
  answer: { blocks: true, configured: true, model: "e2e-chat" },
  decision: { configured: true, model: "e2e-decide", reachable: true },
  label: { configured: true },
  session: { configured: true },
};

/** The one hit the bar's search answers. */
export const BAR_HIT = {
  href: "/issues/LEGION-3",
  id: "LEGION-3",
  kind: "issue",
  owner: { key: "LEGION-3", kind: "issue", status: "todo", title: "Instruments" },
  rank: 1,
  snippet: "The <mark>astrolabe</mark>.",
} as const;

/** One stream the bar opened: the frames a row hands it, and whether the bar let it go. */
export interface OpenedStream {
  readonly id: string;
  readonly send: (frame: GoingEvent) => void;
  readonly signal: AbortSignal;
}

/** The api the bar reaches, spied: each run is `run-<n>`, numbered from 1 as it is asked. */
export interface GoingBarSpies {
  readonly streams: OpenedStream[];
  readonly createGoingRequest: Mock<typeof api.createGoingRequest>;
  readonly cancelGoingRequest: Mock<typeof api.cancelGoingRequest>;
  readonly goingCapability: Mock<typeof api.goingCapability>;
  readonly restore: () => void;
}

export function spyOnGoingBar(): GoingBarSpies {
  const streams: OpenedStream[] = [];
  let runs = 0;
  const createGoingRequest = spyOn(api, "createGoingRequest").mockImplementation(async (input) => {
    runs += 1;
    return {
      author: { id: "alice", kind: "user" },
      created_at: new Date().toISOString(),
      ends_at: new Date(Date.now() + 30_000).toISOString(),
      id: `run-${runs}`,
      project: input.project ?? "",
      words: input.words,
    };
  });
  const cancelGoingRequest = spyOn(api, "cancelGoingRequest").mockImplementation(async (id) => ({
    cancelled_at: new Date().toISOString(),
    id,
  }));
  const goingCapability = spyOn(api, "goingCapability").mockResolvedValue(CONFIGURED_CAPABILITY);
  const spies = [
    createGoingRequest,
    cancelGoingRequest,
    goingCapability,
    spyOn(api, "search").mockResolvedValue(searchAnswer([BAR_HIT])),
    spyOn(api, "getIssueSummary").mockImplementation((key) =>
      Promise.resolve({
        documents: [],
        key,
        project: "LEGION",
        status: "todo",
        title: "Instruments",
      } satisfies IssueReferenceSummary)
    ),
    spyOn(api, "streamGoingAnswer").mockImplementation((id, onFrame, signal) => {
      streams.push({ id, send: onFrame, signal });
      const { promise, reject } = Promise.withResolvers<void>();
      signal.addEventListener("abort", () => reject(signal.reason), { once: true });
      return promise;
    }),
  ];
  return {
    cancelGoingRequest,
    createGoingRequest,
    goingCapability,
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
    streams,
  };
}

/** The bar, open over a project's issue list. */
export function renderBar(): { unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/projects/LEGION/issues"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SearchPalette onClose={() => {}} open />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    unmount: () => {
      view.unmount();
      queryClient.clear();
    },
  };
}
