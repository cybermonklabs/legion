import { expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the table's own scroll events below, which user-event has no form for
import { fireEvent, screen, within } from "@testing-library/react";

import { onFakeClock, tick } from "../../__tests__/fake-clock";
import { renderWithQueries } from "../../__tests__/query-provider";
import { installResizeObserver, layOutScrollBox } from "../../__tests__/scroll-box";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import type { IssueSteps } from "../../api/types";
import { scrollFadeTrailing } from "../../theme/classes";
import { StepsTab } from "./StepsTab";
import { defaultRuns, stepRecord as record } from "./step-record-fixture";

function renderTab(read: () => Promise<IssueSteps>) {
  const listSteps = spyOn(api, "listSteps").mockImplementation(read);
  const view = renderWithQueries(<StepsTab active issueKey="CORE-1" />);
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

// One table per task, in the server's order, each run one row with nine columns: step, role,
// type, agent, model, attempt, start, end and result. A review that failed says where it sent the
// task back to, a run that has not ended says it runs, and a field the record does not hold reads
// as a dash rather than as an empty cell.
test("the Steps tab draws each task's runs as a table, a running run as running", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          defaultRuns.implement1,
          defaultRuns.review1,
          defaultRuns.implement2,
          defaultRuns.merge1,
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
    // That merge: no type, no agent, no model and no start.
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

// Two tasks are two tables under two headings, in the server's order, and each table holds its own
// task's runs and none of the other's.
test("the Steps tab draws each of two tasks as its own table, with its own runs", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          record({ id: "docs-implement-1", task: "docs" }),
          record({
            attempt: 2,
            id: "docs-review-1",
            result: "stopped",
            role: "reviewer",
            step: 3,
            task: "docs",
            type: "review",
          }),
        ],
        task: "docs",
      },
      {
        needs: null,
        steps: [record({ id: "default-plan-1", role: "planner", step: 1, type: "plan" })],
        task: "default",
      },
    ],
  }));

  try {
    const tables = await screen.findAllByRole("table");
    expect(tables.map((table) => table.getAttribute("aria-label"))).toEqual([
      "Steps of task docs",
      "Steps of task default",
    ]);
    expect(screen.getAllByRole("heading").map((heading) => heading.textContent)).toEqual([
      "Task docs",
      "Task default",
    ]);
    // Each table's step, role, type, attempt and result, the table of docs first.
    expect(
      tables.map((table) =>
        bodyRows(table).map((cells) => [cells[0], cells[1], cells[2], cells[5], cells[8]])
      )
    ).toEqual([
      [
        ["2", "implementer", "implementation", "1", "passed"],
        ["3", "reviewer", "review", "2", "stopped"],
      ],
      [["1", "planner", "plan", "1", "passed"]],
    ]);
  } finally {
    tab.restore();
  }
});

// An issue a draw put in an experiment shows the experiment and the variant its records name, once,
// above its tasks; an issue whose records name none shows no such line.
test("the Steps tab shows the variant of the issue above its tasks, and none where none is named", async () => {
  const drawn = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          record({ experiment: "planning", id: "implement-1", variant: "b" }),
          record({
            experiment: "planning",
            id: "review-1",
            role: "reviewer",
            step: 3,
            variant: "b",
          }),
        ],
        task: "default",
      },
    ],
  }));
  try {
    const line = await screen.findByText("Experiment planning: variant b");
    const table = screen.getByRole("table");
    expect(line.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getAllByText(/^Experiment /)).toHaveLength(1);
  } finally {
    drawn.restore();
  }

  const plain = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [{ needs: null, steps: [record({ id: "implement-1" })], task: "default" }],
  }));
  try {
    await screen.findByRole("table");
    expect(screen.queryByText(/^Experiment /)).toBeNull();
  } finally {
    plain.restore();
  }
});

// A run's start and end are what a person reads off the row, so the cells show the clock time
// and day of each instant, not only carry it in `datetime`. The instants are built from local
// components, so the hour and minute each cell must show hold in any time zone, and differ in day
// and time, so a cell that shows one fixed instant, or the other cell's, cannot pass.
test("the Steps tab shows each run's own start and end, as a clock and calendar read them", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          record({
            ended_at: new Date(2026, 10, 17, 14, 40).toISOString(),
            id: "implement-1",
            started_at: new Date(2026, 10, 16, 9, 5).toISOString(),
          }),
        ],
        task: "default",
      },
    ],
  }));
  try {
    const table = await screen.findByRole("table", { name: "Steps of task default" });
    const [cells] = bodyRows(table);
    expect(cells?.[6]).toMatch(/\b16\b.*\b0?9:05\b/u);
    expect(cells?.[7]).toMatch(/\b17\b.*\b(?:14|0?2):40\b/u);
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
        needs: null,
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
        needs: null,
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

// Each task of the plan shows the tasks it needs and each step the plan gives it, with where the
// step stands, before its runs; a task none of whose steps ran yet says so in place of a table. A
// task that needs none shows no needs, and one whose plan node recorded no needs says so. The
// fault is a task of the plan, or a step or the needs of one, that a person cannot see; the control
// is the tab that draws the runs alone, under which task b has no section and step 2 of a no line.
test("the Steps tab draws each step of the plan with its state, and each task's needs", async () => {
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [
      {
        needs: [],
        planned: [
          { name: "implementer", state: "running", step: 1 },
          { name: "reviewer", state: "not started", step: 2 },
        ],
        steps: [record({ ended_at: null, id: "a-1", result: "", step: 1, task: "a" })],
        task: "a",
      },
      {
        needs: ["a"],
        planned: [{ name: "tester", state: "not started", step: 1 }],
        steps: [],
        task: "b",
      },
      {
        needs: null,
        planned: [{ name: "tester", state: "not started", step: 1 }],
        steps: [],
        task: "c",
      },
    ],
  }));
  try {
    const planned = await screen.findByRole("list", { name: "Planned steps of task a" });
    expect(
      within(planned)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["1 implementer running", "2 reviewer not started"]);
    expect(
      bodyRows(screen.getByRole("table", { name: "Steps of task a" })).map((row) => row[0])
    ).toEqual(["1"]);

    const b = screen.getByRole("region", { name: "Task b" });
    expect(within(b).getByText("Needs a")).toBeDefined();
    expect(
      within(within(b).getByRole("list", { name: "Planned steps of task b" }))
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["1 tester not started"]);
    expect(within(b).queryByRole("table")).toBeNull();
    expect(within(b).getByText("No step of task b has run yet.")).toBeDefined();
    expect(within(screen.getByRole("region", { name: "Task a" })).queryByText(/^Needs/)).toBeNull();
    expect(
      within(screen.getByRole("region", { name: "Task c" })).getByText("Needs not recorded")
    ).toBeDefined();
  } finally {
    tab.restore();
  }
});

// At 390 px the table is twice as wide as its card and scrolls sideways inside it, and the column
// cut at the card's edge read as clipped text. The table wears a trailing fade while columns lie
// past the edge, not where the table fits, and not at the end of the scroll.
test("a Steps table wider than its box wears the trailing fade, and one that fits or is scrolled to its end does not", async () => {
  const sizes = installResizeObserver();
  const tab = renderTab(async () => ({
    issue: "CORE-1",
    tasks: [{ needs: null, steps: [record({ id: "implement-1" })], task: "default" }],
  }));
  try {
    const table = await screen.findByRole("table", { name: "Steps of task default" });
    const scroller = table.parentElement;
    if (scroller === null) {
      throw new Error("the Steps table has no box around it");
    }
    // A desktop width: the table fits its 688 px box.
    const box = { client: 688, scroll: 688 };
    layOutScrollBox(scroller, box);
    sizes.resize();
    expect(scroller).not.toHaveClass(scrollFadeTrailing);

    // A phone: the table's 683 px of columns in a 340 px box.
    box.client = 340;
    box.scroll = 683;
    sizes.resize();
    expect(scroller).toHaveClass(scrollFadeTrailing);

    // At the end of the scroll the last column is all there is to see; a fade there would hide it.
    // The reader's scroll is the box's own scroll event, which user-event has no form for.
    scroller.scrollLeft = 343;
    fireEvent.scroll(scroller);
    expect(scroller).not.toHaveClass(scrollFadeTrailing);

    // The reader scrolls back to the start, and the columns past the edge are cued again: a
    // scroll event, which user-event has no form for.
    scroller.scrollLeft = 0;
    fireEvent.scroll(scroller);
    expect(scroller).toHaveClass(scrollFadeTrailing);
  } finally {
    tab.restore();
    sizes.restore();
  }
});

// An issue with no plan and no run - or one that ran before the release that records steps - has
// no task.
test("the Steps tab says so when the issue has no plan and no step of it has run in Legion", async () => {
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
// read again; it never reads as an issue with no steps. The clock stands still, so the read after
// the failed one is Retry's and not a later read of the tab's catch-up.
test("the Steps tab says what failed when the read fails, and reads again on Retry", async () => {
  await onFakeClock(async () => {
    const user = userOnFakeClock();
    let answer: () => Promise<IssueSteps> = async () => {
      throw new ApiError(503, { code: "DATABASE_BUSY", error: "the database is busy" });
    };
    const tab = renderTab(() => answer());
    try {
      await tick(0);
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toContain(
        "Couldn't load the steps of CORE-1: the database is busy"
      );
      expect(screen.queryByRole("region", { name: "Steps empty state" })).toBeNull();

      answer = async () => ({
        issue: "CORE-1",
        tasks: [{ needs: null, steps: [record({ id: "implement-1" })], task: "default" }],
      });
      await user.click(within(alert).getByRole("button", { name: "Retry" }));
      await tick(0);
      expect(screen.getByRole("table", { name: "Steps of task default" })).toBeDefined();
      expect(tab.listSteps).toHaveBeenCalledTimes(2);
    } finally {
      tab.restore();
    }
  });
});
