import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { GoingRun } from "../../api/types";
import { resetGoingStore, setPlace } from "./going-store";
import { SidePanel } from "./SidePanel";

// The conversation kept at the side of the page (spec R1, R11): a click on the page names the item
// the next request is about, in place of the page's own issue and project, and History, read again
// once a run starts there, lists that run.

const PAGE = "/issues/CORE-1";

const started: GoingRun = {
  context: null,
  continues: null,
  conversation: "c-9",
  created_at: new Date().toISOString(),
  finished_at: null,
  id: "r-9",
  issues: [],
  level: "standard",
  mode: "ask",
  state: "queued",
  stop_reason: null,
  used: { calls: 0, tokens: 0, working_ms: 0 },
  words: "what does the runbook say",
};

/** The panel at the side of `PAGE`, beside `page`, alice signed in, on a client that keeps a read
 *  fresh for 30 s as the app's does (`main.tsx`). */
function renderPanel(page: ReactNode = null): void {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  window.history.pushState({}, "", PAGE);
  setPlace("side");
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
        initialEntries={[PAGE]}
      >
        {page}
        <SidePanel />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** Starts are answered with `started`, whose stream stays open. */
function answerStarts() {
  const start = spyOn(api, "createGoingRun").mockResolvedValue(started);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => Promise.withResolvers<void>().promise
  );
  return {
    restore: () => {
      start.mockRestore();
      stream.mockRestore();
    },
    start,
  };
}

afterEach(() => {
  cleanup();
  resetGoingStore();
  window.history.pushState({}, "", "/");
});

test("a click on a project's document is the next request's context, and the page's issue is not", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  try {
    renderPanel(<span data-dispatch-ref="/projects/OPS/documents/runbook">The runbook</span>);
    await user.click(screen.getByText("The runbook"));
    await user.type(
      screen.getByRole("textbox", { name: "Ask the command bar" }),
      `${started.words}{Enter}`
    );

    await waitFor(() => expect(starts.start).toHaveBeenCalledTimes(1));
    expect(starts.start.mock.calls[0]?.[0].context).toEqual({
      block: "",
      issue: "",
      path: PAGE,
      project: "OPS",
    });
  } finally {
    starts.restore();
  }
});

test("History, opened again after a run starts at the side, lists that run", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  let listed = false;
  const history = spyOn(api, "myGoingRuns").mockImplementation(async () => ({
    limit: 50,
    offset: 0,
    rows: listed
      ? [{ id: started.conversation, newest_at: started.created_at, runs: [started] }]
      : [],
    total: listed ? 1 : 0,
  }));
  try {
    renderPanel();
    const toggle = screen.getByTestId("going-history-button");
    await user.click(toggle);
    expect(await screen.findByText("No conversation in the last 7 days.")).toBeInTheDocument();
    await user.click(toggle);

    listed = true;
    await user.type(
      screen.getByRole("textbox", { name: "Ask the command bar" }),
      `${started.words}{Enter}`
    );
    await screen.findByTestId("going-run");
    await user.click(toggle);

    const shown = await screen.findByTestId("going-history");
    expect(await within(shown).findByText(started.words)).toBeInTheDocument();
  } finally {
    history.mockRestore();
    starts.restore();
  }
});
