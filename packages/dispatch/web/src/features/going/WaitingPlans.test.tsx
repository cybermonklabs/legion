import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import { goingWaitingPlansQuery } from "../../api/queries";
import type { GoingPlanAnswer, GoingWaitingPlan } from "../../api/types";
import { WaitingPlans } from "./WaitingPlans";

// The plans that wait for the person at the head of the Inbox (spec R11): Approve names the version
// of the plan the list read, and Dispatch's refusal of an approval is said under the plan it
// refused, and only there, so a row whose run proposed another plan since shows none of it.

const waiting: GoingWaitingPlan = {
  conversation: "run-1",
  created_at: new Date().toISOString(),
  plan: 1,
  run: "run-1",
  summary: "Split CORE-1 in two.",
  version: "c41d07",
  waiting: 3,
  words: "split CORE-1 in two",
};

afterEach(cleanup);

test("a refusal of a run's plan does not show under the plan the run proposes in its place", async () => {
  const user = userEvent.setup();
  let listed: GoingWaitingPlan = waiting;
  const list = spyOn(api, "myGoingPlans").mockImplementation(async () => ({
    limit: 50,
    offset: 0,
    rows: [listed],
    total: 1,
  }));
  const approve = answerRoute<() => Promise<GoingPlanAnswer>>(
    "POST",
    "/api/v1/going/runs/*/plan/approve"
  );
  approve.mockImplementation(async () => {
    throw new ApiError(409, {
      code: "GOING_PLAN_NOT_WAITING",
      error: "the plan was answered meanwhile, and no longer waits",
    });
  });
  // The page's own client, so the test reads the list again as a going_plan.updated reads it.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  try {
    renderWithQueries(<WaitingPlans />, { wrapper });
    await user.click(await screen.findByTestId("inbox-going-plan-approve"));
    const row = screen.getByTestId("inbox-going-plan");
    expect(await within(row).findByRole("alert")).toHaveTextContent(
      "the plan was answered meanwhile, and no longer waits"
    );

    // The run proposes plan 2, and the list is read again: the row is the same run's, with
    // its new plan.
    listed = { ...waiting, plan: 2, summary: "Split CORE-1 in three.", version: "e9a250" };
    await act(() => client.invalidateQueries({ queryKey: goingWaitingPlansQuery().queryKey }));
    await waitFor(() =>
      expect(screen.getByTestId("inbox-going-plan")).toHaveTextContent("Split CORE-1 in three.")
    );
    expect(within(screen.getByTestId("inbox-going-plan")).queryByRole("alert")).toBeNull();
  } finally {
    list.mockRestore();
  }
});

// Approve in the Inbox names the version of the plan the list read, so Dispatch refuses it where
// the plan changed since, as another page read it again. The row says why under that plan and the
// list is read again, so the person's next Approve names the plan's version now.
test("a plan that changed since the list read it is refused under it, and the next Approve sends its version now", async () => {
  const user = userEvent.setup();
  let reads = 0;
  const list = spyOn(api, "myGoingPlans").mockImplementation(async () => {
    reads += 1;
    return {
      limit: 50,
      offset: 0,
      rows: [reads === 1 ? waiting : { ...waiting, version: "e9a250" }],
      total: 1,
    };
  });
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementationOnce(async () => {
    throw new ApiError(409, {
      code: "GOING_PLAN_CHANGED",
      error: "This plan changed after this page read it. Read it again before you approve it.",
    });
  });
  approve.mockImplementation(async () => ({
    mode: "ask",
    plan: 1,
    rows: [],
    run: "run-1",
    state: "running",
    summary: waiting.summary,
    version: "e9a250",
  }));
  try {
    renderWithQueries(<WaitingPlans />);
    await user.click(await screen.findByTestId("inbox-going-plan-approve"));
    const row = screen.getByTestId("inbox-going-plan");
    await within(row).findByRole("alert");
    expect(approve).toHaveBeenCalledWith("run-1", { plan: 1, version: "c41d07" }, keyedAttempt);

    // The list is read again, and the refusal stays under the plan, which is still plan 1.
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    expect(within(screen.getByTestId("inbox-going-plan")).getByRole("alert")).toHaveTextContent(
      "This plan changed since the list read it. Open it to read what it does now before you approve it."
    );
    await user.click(screen.getByTestId("inbox-going-plan-approve"));
    await waitFor(() =>
      expect(approve).toHaveBeenLastCalledWith(
        "run-1",
        { plan: 1, version: "e9a250" },
        keyedAttempt
      )
    );
  } finally {
    list.mockRestore();
  }
});
