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
import type { GoingPlanAnswer, GoingPlanRow, GoingRun, GoingWaitingPlan } from "../../api/types";
import { goingState } from "./going-store";
import { WaitingPlans } from "./WaitingPlans";

// The plans that wait for the person at the head of the Inbox (spec R11; design v3, state 11):
// each row reads "Plan · Change CORE-1 · from ‹title› · 2 hours ago", the plan named as its card
// names it and the conversation by its first question; Review opens the bar on that conversation;
// Approve names the version of the plan the list read, and Dispatch's refusal of an approval is
// said under the plan it refused, and only there, so a row whose run proposed another plan since
// shows none of it.

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

/** A change of `key`'s priority, as a plan's row. */
function change(row: number, key: string): GoingPlanRow {
  return {
    arguments: { issue: key, priority: 2 },
    changed: false,
    claim: null,
    class: "field",
    fields: ["priority"],
    issue: key,
    item: key,
    needs: [],
    now: null,
    result: null,
    row,
    state: "waiting",
    tool: "dispatch_issue_update",
    waits: [],
  };
}

/** Plan `plan` of `run`, waiting, with `rows`. */
function planOf(run: string, plan: number, rows: GoingPlanRow[]): GoingPlanAnswer {
  return { mode: "ask", plan, rows, run, state: "waiting", summary: "", version: "c41d07" };
}

afterEach(cleanup);

test("a row names the plan as its card does and its conversation by its first question, and Review opens the bar on it", async () => {
  const user = userEvent.setup();
  const first: GoingRun = {
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: new Date(Date.now() - 7_200_000).toISOString(),
    finished_at: null,
    id: "run-1",
    issues: [],
    level: "standard",
    line: null,
    mode: "ask",
    standing_count: 0,
    state: "done",
    stop_reason: null,
    undoable: false,
    undoable_count: 0,
    undone_at: null,
    undone_count: 0,
    used: { calls: 1, tokens: 10, working_ms: 1000 },
    words: "what blocks the release",
  };
  const followUp: GoingRun = {
    ...first,
    continues: "run-1",
    id: "run-2",
    state: "waiting",
    words: "then raise CORE-10",
  };
  const list = spyOn(api, "myGoingPlans").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [
      {
        ...waiting,
        conversation: "c-1",
        created_at: new Date(Date.now() - 7_200_000).toISOString(),
        run: "run-2",
        words: followUp.words,
      },
    ],
    total: 1,
  });
  const history = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [{ id: "c-1", newest_at: followUp.created_at, runs: [followUp, first] }],
    total: 1,
  });
  const plan = spyOn(api, "goingPlan").mockResolvedValue(
    planOf("run-2", 1, [change(1, "CORE-10")])
  );
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => new Promise<void>(() => {})
  );
  try {
    renderWithQueries(<WaitingPlans />);
    const row = await screen.findByTestId("inbox-going-plan");
    await waitFor(() =>
      expect(within(row).getByTestId("inbox-going-plan-title")).toHaveTextContent("Change CORE-10")
    );
    await waitFor(() =>
      expect(within(row).getByTestId("inbox-going-plan-from")).toHaveTextContent(
        "from what blocks the release"
      )
    );
    expect(row.querySelector("p")?.textContent).toBe(
      "Plan · Change CORE-10 · from what blocks the release · 2 hours ago"
    );

    await user.click(within(row).getByRole("button", { name: "Review" }));
    await waitFor(() => expect(goingState().place).toBe("over"));
    expect(goingState().conversation).toEqual(["run-1", "run-2"]);
  } finally {
    list.mockRestore();
    history.mockRestore();
    plan.mockRestore();
    stream.mockRestore();
  }
});

// Until its plan is read, a row names the plan by the summary the agent wrote, and a time the
// agent wrote there as a link to its instant draws as that time, the full date on hover.
test("until its plan is read a row names it by its summary, each time in it drawn as its time", async () => {
  const list = spyOn(api, "myGoingPlans").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [
      {
        ...waiting,
        summary: "Close CORE-5, idle since [5 days ago](dispatch-time:2026-11-18T11:35:00Z).",
      },
    ],
    total: 1,
  });
  const history = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [],
    total: 0,
  });
  const plan = spyOn(api, "goingPlan").mockImplementation(() => new Promise(() => {}));
  try {
    renderWithQueries(<WaitingPlans />);
    const title = await screen.findByTestId("inbox-going-plan-title");
    expect(title.textContent).toBe("Close CORE-5, idle since 5 days ago.");
    const time = title.querySelector("time");
    expect(time?.getAttribute("datetime")).toBe("2026-11-18T11:35:00Z");
    expect(time?.getAttribute("title")).toBe(new Date("2026-11-18T11:35:00Z").toLocaleString());
  } finally {
    list.mockRestore();
    history.mockRestore();
    plan.mockRestore();
  }
});

// A read of the plans that wait that failed says so at the head of the Inbox, with Retry, rather
// than draw nothing, which says no plan waits; Retry reads them again, and the plans it lists take
// the failure's place.
test("a read of the plans that wait that failed says so with Retry, which reads them again", async () => {
  const user = userEvent.setup();
  const list = spyOn(api, "myGoingPlans")
    .mockRejectedValueOnce(new Error("the network dropped"))
    .mockResolvedValueOnce({ limit: 50, offset: 0, rows: [waiting], total: 1 });
  const history = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [],
    total: 0,
  });
  const plan = spyOn(api, "goingPlan").mockImplementation(() => new Promise(() => {}));
  try {
    renderWithQueries(<WaitingPlans />);
    const section = await screen.findByTestId("inbox-going-plans");
    expect(within(section).getByRole("heading")).toHaveTextContent("Plans that wait for you");
    expect(within(section).getByRole("alert")).toHaveTextContent(
      "The plans that wait for you could not be read."
    );
    expect(within(section).queryByTestId("inbox-going-plan")).toBeNull();

    await user.click(within(section).getByRole("button", { name: "Retry" }));
    const row = await screen.findByTestId("inbox-going-plan");
    expect(within(row).getByTestId("inbox-going-plan-title")).toHaveTextContent(
      "Split CORE-1 in two."
    );
    expect(screen.queryByRole("alert")).toBeNull();
    expect(list).toHaveBeenCalledTimes(2);
  } finally {
    list.mockRestore();
    history.mockRestore();
    plan.mockRestore();
  }
});

test("a refusal of a run's plan does not show under the plan the run proposes in its place", async () => {
  const user = userEvent.setup();
  let listed: GoingWaitingPlan = waiting;
  let proposed = planOf("run-1", 1, [change(1, "CORE-1")]);
  const list = spyOn(api, "myGoingPlans").mockImplementation(async () => ({
    limit: 50,
    offset: 0,
    rows: [listed],
    total: 1,
  }));
  const plan = spyOn(api, "goingPlan").mockImplementation(async () => proposed);
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
    // Approve says how many changes it runs, as the plan card's Approve does.
    expect(await screen.findByTestId("inbox-going-plan-approve")).toHaveTextContent(
      "Approve 3 changes"
    );
    await waitFor(() =>
      expect(screen.getByTestId("inbox-going-plan-title")).toHaveTextContent("Change CORE-1")
    );
    // Where History does not list the conversation, the row names it by its run's words.
    expect(screen.getByTestId("inbox-going-plan-from")).toHaveTextContent(
      "from split CORE-1 in two"
    );
    await user.click(screen.getByTestId("inbox-going-plan-approve"));
    const row = screen.getByTestId("inbox-going-plan");
    expect(await within(row).findByRole("alert")).toHaveTextContent(
      "the plan was answered meanwhile, and no longer waits"
    );

    // The run proposes plan 2, and the list is read again: the row is the same run's, with
    // its new plan, which it reads again to name it.
    proposed = planOf("run-1", 2, [change(1, "CORE-1"), change(2, "CORE-2")]);
    listed = { ...waiting, plan: 2, summary: "Split CORE-1 in three.", version: "e9a250" };
    await act(() => client.invalidateQueries({ queryKey: goingWaitingPlansQuery().queryKey }));
    await waitFor(() =>
      expect(screen.getByTestId("inbox-going-plan-title")).toHaveTextContent("Change 2 issues")
    );
    expect(within(screen.getByTestId("inbox-going-plan")).queryByRole("alert")).toBeNull();
  } finally {
    list.mockRestore();
    plan.mockRestore();
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
  const plan = spyOn(api, "goingPlan").mockResolvedValue(planOf("run-1", 1, [change(1, "CORE-1")]));
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
      "This plan changed since the list read it. Review it to read what it does now before you approve it."
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
    plan.mockRestore();
  }
});
