import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRoute } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type { GoingUndoAnswer, IssueReferenceSummary } from "../../api/types";
import { Notice, showNotice } from "./Notice";

// The notice at the bottom right of the page (design v3, states 11 and 13): one line for a few
// seconds, its actions, and Undo through the run's own undo (R6). The shell's status corner draws
// it; the setup's `afterEach` lets any notice a row left go.

function CurrentRoute(): ReactNode {
  return <output data-testid="current-route">{useLocation().pathname}</output>;
}

function renderNotice(): void {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <CurrentRoute />
        <Notice />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a notice shows its line and actions, and goes once its time is up", async () => {
  jest.useFakeTimers();
  try {
    renderNotice();
    expect(screen.queryByTestId("going-notice")).toBeNull();
    act(() =>
      showNotice({
        actions: [{ label: "Open", to: "/issues/CORE-1" }],
        ms: 6_000,
        text: "Done: Watch the comet",
      })
    );
    const notice = screen.getByTestId("going-notice");
    expect(notice).toHaveAttribute("role", "status");
    expect(notice).toHaveTextContent("Done: Watch the comet · Open");

    await tick(5_900);
    expect(screen.getByTestId("going-notice")).toBeInTheDocument();
    await tick(200);
    expect(screen.queryByTestId("going-notice")).toBeNull();
  } finally {
    realClock();
  }
});

test("a newer notice takes the place of the one shown, with a time of its own", async () => {
  jest.useFakeTimers();
  try {
    renderNotice();
    act(() => showNotice({ ms: 6_000, text: "Done: Watch the comet" }));
    await tick(5_000);
    act(() => showNotice({ ms: 6_000, text: "Failed: Count the moons" }));
    expect(screen.getAllByTestId("going-notice")).toHaveLength(1);
    expect(screen.getByTestId("going-notice")).toHaveTextContent("Failed: Count the moons");
    // The first notice's time is up, and the newer one stays for its own.
    await tick(2_000);
    expect(screen.getByTestId("going-notice")).toHaveTextContent("Failed: Count the moons");
    await tick(4_000);
    expect(screen.queryByTestId("going-notice")).toBeNull();
  } finally {
    realClock();
  }
});

test("an action opens its page or runs its press, and the notice goes", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  try {
    renderNotice();
    act(() =>
      showNotice({
        actions: [{ label: "Open", to: "/issues/CORE-46" }],
        ms: 10_000,
        text: "Filed CORE-46",
      })
    );
    await user.click(screen.getByRole("link", { name: "Open" }));
    expect(screen.getByTestId("current-route")).toHaveTextContent("/issues/CORE-46");
    expect(screen.queryByTestId("going-notice")).toBeNull();

    const pressed: string[] = [];
    act(() =>
      showNotice({
        actions: [{ label: "Review", press: () => pressed.push("review") }],
        ms: 6_000,
        text: "Needs you: Change CORE-10",
      })
    );
    await user.click(screen.getByRole("button", { name: "Review" }));
    expect(pressed).toEqual(["review"]);
    expect(screen.queryByTestId("going-notice")).toBeNull();
  } finally {
    realClock();
  }
});

// The notice of a filing names what was filed (review R4, 13-filed): its line names the issue by
// its key at once, and says the issue's title after it once Dispatch answers the issue's summary,
// the read every reference to the issue shares. A notice that names no issue reads none.
test("a notice that names an issue says the issue's title after its line once Dispatch answers it", async () => {
  const answered = Promise.withResolvers<IssueReferenceSummary>();
  const summary = spyOn(api, "getIssueSummary").mockReturnValue(answered.promise);
  try {
    renderNotice();
    act(() =>
      showNotice({
        actions: [{ label: "Open", to: "/issues/CORE-46" }],
        issue: "CORE-46",
        ms: 10_000,
        text: "Filed CORE-46",
      })
    );
    expect(screen.getByTestId("going-notice-text").textContent).toBe("Filed CORE-46");
    await waitFor(() => expect(summary).toHaveBeenCalledWith("CORE-46"));
    await act(async () =>
      answered.resolve({
        documents: [],
        key: "CORE-46",
        project: "CORE",
        status: "todo",
        title: "A weather station on the roof",
      })
    );
    await waitFor(() =>
      expect(screen.getByTestId("going-notice-text").textContent).toBe(
        "Filed CORE-46: A weather station on the roof"
      )
    );
    expect(screen.getByTestId("going-notice")).toHaveTextContent(
      "Filed CORE-46: A weather station on the roof · Open"
    );

    summary.mockClear();
    act(() => showNotice({ ms: 10_000, text: "Done: Count the moons" }));
    expect(screen.getByTestId("going-notice-text").textContent).toBe("Done: Count the moons");
    expect(summary).not.toHaveBeenCalled();
  } finally {
    summary.mockRestore();
  }
});

test("Undo reverses the run through its own undo, and the notice says what was undone or why not", async () => {
  const user = userEvent.setup();
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementationOnce(async () => ({
    lines: [
      {
        fields: [],
        item: "CORE-46",
        outcome: { code: "", message: "", ok: true, status: 200 },
        plan: 1,
        row: 0,
        tool: "dispatch_issue",
        undo: "close_created",
      },
    ],
  }));
  renderNotice();
  act(() =>
    showNotice({
      ms: 10_000,
      text: "Filed CORE-46",
      undo: { run: "r-file", undone: "Undone · CORE-46 closed" },
    })
  );
  const notice = screen.getByTestId("going-notice");
  expect(notice).toHaveTextContent("Filed CORE-46 · Undo");
  await user.click(within(notice).getByRole("button", { name: "Undo" }));
  await waitFor(() => expect(undo).toHaveBeenCalledWith("r-file", undefined, keyedAttempt));
  await waitFor(() =>
    expect(screen.getByTestId("going-notice")).toHaveTextContent("Undone · CORE-46 closed")
  );
  expect(within(screen.getByTestId("going-notice")).queryByRole("button")).toBeNull();

  // A change another writer made since stays, and the notice says so in its words.
  undo.mockImplementationOnce(async () => ({
    lines: [
      {
        fields: [],
        item: "CORE-47",
        outcome: { code: "", message: "bob reopened CORE-47 since", ok: false, status: 409 },
        plan: 1,
        row: 0,
        tool: "dispatch_issue",
        undo: "close_created",
      },
    ],
  }));
  act(() =>
    showNotice({
      ms: 10_000,
      text: "Filed CORE-47",
      undo: { run: "r-file-2", undone: "Undone · CORE-47 closed" },
    })
  );
  await user.click(screen.getByRole("button", { name: "Undo" }));
  await waitFor(() =>
    expect(screen.getByTestId("going-notice")).toHaveTextContent(
      "Not undone: bob reopened CORE-47 since"
    )
  );
});

// A refused Undo says why as a landed one says what it undid: in a notice of its own, in place of
// the one that offered it, with no Undo to press again. A newer notice takes its place in turn,
// saying nothing of it.
test("a refused Undo says why in a notice, as a landed one says what it undid, and a newer notice shows none of it", async () => {
  const user = userEvent.setup();
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementationOnce(async () => {
    throw new ApiError(409, { code: "GOING_UNDO_UNKNOWN", error: "nothing was undone" });
  });
  renderNotice();
  act(() =>
    showNotice({
      ms: 10_000,
      text: "Filed CORE-46",
      undo: { run: "r-file", undone: "Undone · CORE-46 closed" },
    })
  );
  await user.click(
    within(screen.getByTestId("going-notice")).getByRole("button", { name: "Undo" })
  );
  await waitFor(() =>
    expect(screen.getByTestId("going-notice-text").textContent).toBe(
      "Not undone: nothing was undone"
    )
  );
  expect(within(screen.getByTestId("going-notice")).queryByRole("button")).toBeNull();

  act(() => showNotice({ ms: 10_000, text: "Done: Count the moons" }));
  expect(screen.getByTestId("going-notice")).toHaveTextContent("Done: Count the moons");
  expect(screen.getByTestId("going-notice")).not.toHaveTextContent("nothing was undone");
});

// The notice stays while its Undo is out, past its time: Dispatch's answer to the Undo, a refusal
// as much as a landing, would otherwise come after the notice had gone, and show nowhere. Once it
// comes, the notice that says so takes the place of the one that offered Undo, for its own time.
test("a notice stays while its Undo is out, past its time, and the Undo's refusal then shows in its place", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answered = Promise.withResolvers<GoingUndoAnswer>();
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementationOnce(() => answered.promise);
  try {
    renderNotice();
    act(() =>
      showNotice({
        ms: 3_000,
        text: "Filed CORE-46",
        undo: { run: "r-file", undone: "Undone · CORE-46 closed" },
      })
    );
    await user.click(screen.getByRole("button", { name: "Undo" }));
    await tick(4_000);
    expect(undo).toHaveBeenCalledWith("r-file", undefined, keyedAttempt);
    const notice = screen.getByTestId("going-notice");
    expect(notice).toHaveTextContent("Filed CORE-46 · Undo");
    expect(within(notice).getByRole("button", { name: "Undo" })).toBeDisabled();

    answered.reject(new ApiError(409, { code: "GOING_UNDO_UNKNOWN", error: "nothing was undone" }));
    await tick(0);
    expect(screen.getByTestId("going-notice-text").textContent).toBe(
      "Not undone: nothing was undone"
    );
    await tick(2_900);
    expect(screen.getByTestId("going-notice")).toBeInTheDocument();
    await tick(200);
    expect(screen.queryByTestId("going-notice")).toBeNull();
  } finally {
    realClock();
  }
});
