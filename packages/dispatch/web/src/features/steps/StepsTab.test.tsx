import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ApiError, api } from "../../api/client";
import type { IssueSteps, StepRecord } from "../../api/types";
import { StepsTab } from "./StepsTab";

function record(fields: Partial<StepRecord> & Pick<StepRecord, "id">): StepRecord {
  return {
    agent: "legion-leg-leg-1-implementer",
    attempt: 1,
    ended_at: "2026-11-16T09:40:00Z",
    generation: 1,
    issue: "CORE-1",
    kind: "step",
    model: "anthropic/claude-opus-5",
    rating: null,
    rating_reason: null,
    report: null,
    result: "passed",
    returned_to: null,
    role: "implementer",
    session: "",
    started_at: "2026-11-16T09:00:00Z",
    step: 2,
    task: "default",
    type: "implementation",
    ...fields,
  };
}

function renderTab(read: () => Promise<IssueSteps>) {
  const listSteps = spyOn(api, "listSteps").mockImplementation(read);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <StepsTab issueKey="CORE-1" />
    </QueryClientProvider>
  );
  return {
    listSteps,
    restore: () => {
      view.unmount();
      listSteps.mockRestore();
    },
  };
}

/** Each body row of `table` as the text of its cells, in order. */
function bodyRows(table: HTMLElement): string[][] {
  return within(table)
    .getAllByRole("row")
    .slice(1)
    .map((row) =>
      within(row)
        .getAllByRole("cell")
        .map((cell) => cell.textContent ?? "")
    );
}

// One table per task, in the server's order, each run one row with the nine columns the plan
// names. A review that failed says where it sent the task back to, a run that has not ended says
// it runs, and a field the record does not hold reads as a dash rather than as an empty cell.
test("the Steps tab draws each task's runs as a table, a running run as running", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        steps: [
          record({ id: "implement-1" }),
          record({
            agent: "legion-leg-leg-1-reviewer",
            ended_at: "2026-11-16T10:30:00Z",
            id: "review-1",
            model: "openai/gpt-6.1-sol",
            result: "failed",
            returned_to: 2,
            role: "reviewer",
            started_at: "2026-11-16T10:00:00Z",
            step: 4,
            type: "review",
          }),
          record({
            attempt: 2,
            ended_at: null,
            id: "implement-2",
            model: "",
            result: "",
            started_at: "2026-11-16T10:31:00Z",
          }),
          record({
            agent: "",
            id: "merge-1",
            kind: "merge",
            model: "",
            role: "merger",
            started_at: null,
            step: 0,
            type: "",
          }),
        ],
        task: "default",
      },
    ],
  }));

  try {
    const table = await screen.findByRole("table", { name: "Steps of task default" });
    expect(screen.getByRole("heading", { name: "Task default" })).toBeDefined();
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((header) => header.textContent)
    ).toEqual(["Step", "Role", "Type", "Agent", "Model", "Attempt", "Started", "Ended", "Result"]);
    const rows = bodyRows(table);
    expect(rows.map((cells) => [cells[0], cells[1], cells[5], cells[8]])).toEqual([
      ["2", "implementer", "1", "passed"],
      ["4", "reviewer", "1", "failed, returned to step 2"],
      ["2", "implementer", "2", "running"],
      ["Merge", "merger", "1", "passed"],
    ]);
    // The running run: no model yet, and no end.
    expect(rows[2]?.[4]).toBe("—");
    expect(rows[2]?.[7]).toBe("—");
    // The merge GitHub made: no type, no agent, no model and no start.
    expect([rows[3]?.[2], rows[3]?.[3], rows[3]?.[4], rows[3]?.[6]]).toEqual(["—", "—", "—", "—"]);
    // Each time the record holds is the record's own instant, whatever the relative words say.
    expect(
      [...table.querySelectorAll("tbody tr:first-child time")].map((time) =>
        time.getAttribute("datetime")
      )
    ).toEqual(["2026-11-16T09:00:00Z", "2026-11-16T09:40:00Z"]);
    expect(tab.listSteps).toHaveBeenCalledWith("CORE-1");
  } finally {
    tab.restore();
  }
});

// "step 1" is one phrase. A narrow Result column (a phone) wraps "stopped, returned to step 1"
// between its words, and the last break must not fall inside the phrase: at 390 px it left the
// number alone on the last line.
test("the Steps tab keeps the step a run returned to whole, with its number", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        steps: [record({ id: "docs-1", result: "stopped", returned_to: 1, step: 1 })],
        task: "docs",
      },
    ],
  }));
  try {
    const table = await screen.findByRole("table", { name: "Steps of task docs" });
    expect(bodyRows(table)[0]?.[8]).toBe("stopped, returned to step 1");
    expect(within(table).getByText("step 1").className).toContain("whitespace-nowrap");
  } finally {
    tab.restore();
  }
});

// An agent token breaks at its hyphens, and a model id after its slash too, but a number or a
// single letter stays with the part before it: at 1440 px with the margin open, the Agent column
// put "2-" of `legion-core-core-2-implementer` on a line alone and tore the key `core-2` in two.
test("the Steps tab never leaves a number or a single letter of an id on a line alone", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        steps: [
          record({
            agent: "legion-core-core-2-implementer-with-a-long-name",
            id: "implement-1",
            model: "openai/gpt-6.1-sol",
          }),
        ],
        task: "default",
      },
    ],
  }));
  try {
    const table = await screen.findByRole("table", { name: "Steps of task default" });
    const cells = bodyRows(table)[0];
    expect([cells?.[3], cells?.[4]]).toEqual([
      "legion-core-core-2-implementer-with-a-long-name",
      "openai/gpt-6.1-sol",
    ]);
    for (const whole of ["core-2", "with-a", "gpt-6.1"]) {
      expect(within(table).getByText(whole).className).toContain("whitespace-nowrap");
    }
  } finally {
    tab.restore();
  }
});

// An issue Legion never ran - or ran before the release that records steps - has no task.
test("the Steps tab says so when no step of the issue has run in Legion", async () => {
  const tab = renderTab(async () => ({ issue: "CORE-1", tasks: [] }));
  try {
    const empty = await screen.findByRole("region", { name: "Steps empty state" });
    expect(empty.textContent).toBe("No step of this issue has run in Legion.");
    expect(screen.queryByRole("table")).toBeNull();
  } finally {
    tab.restore();
  }
});

// A failed read names what it could not read and why, in the server's words, and offers the
// read again; it never reads as an issue with no steps.
test("the Steps tab says what failed when the read fails, and reads again on Retry", async () => {
  const user = userEvent.setup();
  let answer: () => Promise<IssueSteps> = async () => {
    throw new ApiError(503, { code: "DATABASE_BUSY", error: "the database is busy" });
  };
  const tab = renderTab(() => answer());
  try {
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't load the steps of CORE-1: the database is busy");
    expect(screen.queryByRole("region", { name: "Steps empty state" })).toBeNull();

    answer = async () => ({
      issue: "CORE-1",
      tasks: [{ steps: [record({ id: "implement-1" })], task: "default" }],
    });
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await screen.findByRole("table", { name: "Steps of task default" });
    await waitFor(() => expect(tab.listSteps).toHaveBeenCalledTimes(2));
  } finally {
    tab.restore();
  }
});
