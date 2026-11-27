import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { onFakeClock, tick } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "../../api/query-client";
import type { IssueDetails, IssueSteps, StepRecord } from "../../api/types";
import { stepRecord } from "../steps/step-record-fixture";
import { stepTasks } from "../steps/steps-model";
import { DetailsSteps } from "./DetailsSteps";
import { DetailsHistory } from "./details-more";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";

const KEY = "CORE-1";

// The runs are dated at hours of one day, built in the machine's own zone, and the section draws
// each as the instant it is, with its year only where it is not this one: each case runs at one
// fixed local midday of that day, so none reads the wall clock. `setSystemTime` holds `Date`
// alone; the timers most cases wait on (`waitFor`'s) run on the real clock, where the held `Date`
// never lets the scheduler's second between two reads of a query end, so no read of the
// catch-up's follows its first. A case that counts the catch-up's reads runs on the fake clock
// instead (`onNoonClock`), which starts from the wall clock's time, so it is set to the same
// midday before anything reads it.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

/** Runs `body` on the fake clock (`onFakeClock`), from the cases' midday. */
async function onNoonClock(body: () => Promise<void>): Promise<void> {
  await onFakeClock(async () => {
    setSystemTime(NOON);
    await body();
  });
}

/** The instant `hour`:`minute` of the cases' day, in the machine's own zone. */
function at(hour: number, minute = 0): string {
  return new Date(2026, 10, 19, hour, minute).toISOString();
}

/** What the section draws of the instant `iso`, as the Steps tab draws it: the date and the time
 *  of day in the reader's locale. */
function drawnAt(iso: string): string {
  const date = new Date(iso);
  const day = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" });
  const time = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
  return `${day.format(date)} ${time.format(date)}`;
}

/** Two tasks in the server's order, `docs` before `default`, which no sort would give: the
 *  passed run of the task `docs`; and in `default`, an implementation that passed, a review that
 *  failed and sent the task back to step 2, the implementation's second run, still running with
 *  no model yet, and a merge GitHub made, which names no type, agent, model or start. */
const TWO_TASKS: IssueSteps = {
  issue: KEY,
  tasks: [
    {
      needs: null,
      steps: [
        stepRecord({
          ended_at: at(8, 30),
          id: "docs-1",
          started_at: at(8),
          step: 1,
          task: "docs",
          type: "documentation",
        }),
      ],
      task: "docs",
    },
    {
      needs: null,
      steps: [
        stepRecord({ ended_at: at(9, 40), id: "implement-1", started_at: at(9) }),
        stepRecord({
          agent: "legion-core-core-1-reviewer",
          ended_at: at(10, 30),
          id: "review-1",
          model: "openai/gpt-6.1-sol",
          result: "failed",
          returned_to: 2,
          role: "reviewer",
          started_at: at(10),
          step: 4,
          type: "review",
        }),
        stepRecord({
          attempt: 2,
          ended_at: null,
          id: "implement-2",
          model: "",
          result: "",
          started_at: at(10, 31),
        }),
        stepRecord({
          agent: "",
          ended_at: at(11),
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
};

const NO_STEPS: IssueSteps = { issue: KEY, tasks: [] };

/** A second run of `docs`, whose record the daemon wrote after the others. */
const LATER_RUN = stepRecord({
  ended_at: at(11, 50),
  id: "docs-2",
  started_at: at(11, 20),
  step: 2,
  task: "docs",
  type: "documentation",
});

/** `TWO_TASKS` once `LATER_RUN`'s record has reached the server. */
const WITH_LATER_RUN: IssueSteps = {
  issue: KEY,
  tasks: (TWO_TASKS.tasks ?? []).map((task) =>
    task.task === "docs" ? { ...task, steps: [...(task.steps ?? []), LATER_RUN] } : task
  ),
};

/** The client of the page, with the app's own retry policy (`main.tsx`): a failed read is asked
 *  again twice, 500 ms apart. */
function appClient(): DispatchQueryClient {
  return new DispatchQueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: queryRetryDelay } },
  });
}

/** The section alone, reading the issue's steps through `read`, with Details open or shut, and
 *  opened (`open`) or shut (`shut`) after; `fetchedAt` holds the clock's time at each read. */
function renderSteps(read: () => Promise<IssueSteps>, open = true) {
  const fetchedAt: number[] = [];
  const listSteps = spyOn(api, "listSteps").mockImplementation(() => {
    fetchedAt.push(Date.now());
    return read();
  });
  const client = appClient();
  const draw = (isOpen: boolean) => (
    <QueryClientProvider client={client}>
      <DetailsSteps issueKey={KEY} open={isOpen} />
    </QueryClientProvider>
  );
  const view = render(draw(open));
  return {
    fetchedAt,
    listSteps,
    open: () => view.rerender(draw(true)),
    restore: () => {
      view.unmount();
      client.clear();
      listSteps.mockRestore();
    },
    shut: () => view.rerender(draw(false)),
  };
}

/** The section's heading. */
function heading(): HTMLElement {
  return within(screen.getByRole("region", { name: CONTROL_LABELS.steps })).getByRole("heading", {
    level: 2,
  });
}

/** Waits until the heading counts `runs`, as it does once a read has answered. Its words are
 *  read as text: happy-dom lays out no inline box, so the name the accessibility tree computes
 *  spaces the count off from its brackets where a browser does not. */
async function headedWith(runs: number): Promise<void> {
  await waitFor(() => expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (${runs})`));
}

/** One run, by its record's id. */
function run(id: string): HTMLElement {
  const found = document.querySelector<HTMLElement>(`[data-step-run="${id}"]`);
  if (found === null) throw new Error(`the section draws no run ${id}`);
  return found;
}

/** Each labelled value of a run, by its field, as the reader reads it. */
function fieldsOf(id: string): Record<string, string> {
  return Object.fromEntries(
    [...run(id).querySelectorAll("[data-step-field]")].map((field) => [
      field.getAttribute("data-step-field"),
      field.querySelector("dd")?.textContent ?? "",
    ])
  );
}

// Details lists every run of the issue's steps, headed by how many it lists: one group per task in
// the order `stepTasks` gives, each run in the read's order, each field a labelled value of a list
// rather than a cell of a table. A review that failed names the step it sent the task back to, a
// run that has not ended reads as running, and every field the record does not hold reads None.
test("the steps read as a list per task, headed by the runs' count, each run's fields labelled", async () => {
  const steps = renderSteps(async () => TWO_TASKS);
  try {
    await headedWith(5);
    expect(screen.queryByRole("table"), "the runs are drawn as a table").toBeNull();
    expect(
      within(screen.getByRole("region", { name: CONTROL_LABELS.steps }))
        .getAllByRole("heading", { level: 3 })
        .map((group) => group.textContent)
    ).toEqual(stepTasks(TWO_TASKS).map(({ task }) => `${CONTROL_LABELS.steps_task} ${task}`));
    expect(
      [...document.querySelectorAll("[data-step-run]")].map((drawn) =>
        drawn.getAttribute("data-step-run")
      )
    ).toEqual(["docs-1", "implement-1", "review-1", "implement-2", "merge-1"]);

    const fields = CONTROL_LABELS.step_fields;
    expect(
      [...run("review-1").querySelectorAll("dt")].map((label) => label.textContent),
      "the failed review's fields are not labelled in the page's words"
    ).toEqual([
      fields.step,
      fields.role,
      fields.type,
      fields.agent,
      fields.model,
      fields.attempt,
      fields.started,
      fields.ended,
      fields.result,
      fields.returned_to,
    ]);
    expect(fieldsOf("review-1")).toEqual({
      agent: "legion-core-core-1-reviewer",
      attempt: "1",
      ended: drawnAt(at(10, 30)),
      model: "openai/gpt-6.1-sol",
      result: CONTROL_LABELS.step_results.failed,
      returned_to: "step 2",
      role: "reviewer",
      started: drawnAt(at(10)),
      step: "4",
      type: "review",
    });
    // Each time is the record's own instant, in a `<time>` that names it.
    expect(
      [...run("review-1").querySelectorAll("time")].map((time) => time.getAttribute("datetime"))
    ).toEqual([at(10), at(10, 30)]);

    // The second run of the implementation: the server's count, running, and no model or end.
    expect(fieldsOf("implement-2")).toMatchObject({
      attempt: "2",
      ended: CONTROL_LABELS.none,
      model: CONTROL_LABELS.none,
      result: CONTROL_LABELS.step_results.running,
    });
    // The merge GitHub made: no type, agent, model or start, each read the one way.
    expect(fieldsOf("merge-1")).toMatchObject({
      agent: CONTROL_LABELS.none,
      model: CONTROL_LABELS.none,
      result: CONTROL_LABELS.step_results.passed,
      started: CONTROL_LABELS.none,
      step: "Merge",
      type: CONTROL_LABELS.none,
    });
    // Only a run that sent its task back names where it sent it.
    for (const id of ["docs-1", "implement-1", "implement-2", "merge-1"]) {
      expect(fieldsOf(id), `${id} names a step it went back to`).not.toHaveProperty("returned_to");
    }
    expect(steps.listSteps).toHaveBeenCalledTimes(1);
  } finally {
    steps.restore();
  }
});

test("an issue with no runs reads None under its heading", async () => {
  const steps = renderSteps(async () => NO_STEPS);
  try {
    await headedWith(0);
    const section = screen.getByRole("region", { name: CONTROL_LABELS.steps });
    expect(within(section).getByText(CONTROL_LABELS.none, { exact: true })).toBeDefined();
    expect(section.querySelectorAll("[data-step-run]")).toHaveLength(0);
  } finally {
    steps.restore();
  }
});

// Where the records of the newest run, the highest generation, name an experiment, the section
// draws its experiment and variant once, under its heading and above its first task group; the
// older generation's, which names another variant, is not drawn.
test("the section draws the experiment and variant of the newest run once, above its first task group", async () => {
  const steps = renderSteps(async () => ({
    issue: KEY,
    tasks: [
      {
        needs: null,
        steps: [
          stepRecord({ experiment: "planning", generation: 1, id: "older-1", variant: "a" }),
          stepRecord({ experiment: "planning", generation: 2, id: "newest-1", variant: "b" }),
          stepRecord({
            experiment: "planning",
            generation: 2,
            id: "newest-2",
            role: "reviewer",
            step: 4,
            type: "review",
            variant: "b",
          }),
        ],
        task: "default",
      },
    ],
  }));
  try {
    await headedWith(3);
    const section = screen.getByRole("region", { name: CONTROL_LABELS.steps });
    const line = within(section).getByText("Experiment planning: variant b");
    expect(within(section).getAllByText(/^Experiment /u)).toHaveLength(1);
    expect(line.hasAttribute("data-step-variant")).toBe(true);
    const group = section.querySelector("[data-step-task]");
    if (group === null) throw new Error("the section draws no task group");
    expect(heading().compareDocumentPosition(line) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(line.compareDocumentPosition(group) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  } finally {
    steps.restore();
  }
});

// Where no record of the newest run names an experiment, though the older generation's does, the
// section draws no experiment line.
test("the section draws no experiment line where no record of the newest run names one", async () => {
  const steps = renderSteps(async () => ({
    issue: KEY,
    tasks: [
      {
        needs: null,
        steps: [
          stepRecord({ experiment: "planning", generation: 1, id: "older-1", variant: "a" }),
          stepRecord({ generation: 2, id: "newest-1" }),
        ],
        task: "default",
      },
    ],
  }));
  try {
    await headedWith(2);
    const section = screen.getByRole("region", { name: CONTROL_LABELS.steps });
    expect(within(section).queryByText(/^Experiment /u)).toBeNull();
    expect(section.querySelector("[data-step-variant]")).toBeNull();
  } finally {
    steps.restore();
  }
});

/** `count` passed runs of the task `task`, each its own record, `<task>-1` and on. */
function runsOf(task: string, count: number): StepRecord[] {
  return Array.from({ length: count }, (_, index) =>
    stepRecord({ id: `${task}-${index + 1}`, task })
  );
}

// The runs are one list across their tasks, drawn 20 at a time with Show more under them, which
// says how many runs are left, and the heading counts every run, drawn or not: a task whose runs
// are all past the cut is not drawn until a press draws them. A keyboard press draws the rest and
// moves the focus to the first run it drew, which holds no control of its own, and the control
// goes.
test("the runs are drawn 20 at a time across their tasks, the heading counts them all, and Show more moves the focus to the first run it drew", async () => {
  const steps = renderSteps(async () => ({
    issue: KEY,
    tasks: [
      { needs: null, steps: runsOf("docs", 15), task: "docs" },
      { needs: null, steps: runsOf("default", 10), task: "default" },
      { needs: null, steps: runsOf("release", 5), task: "release" },
    ],
  }));
  const section = () => screen.getByRole("region", { name: CONTROL_LABELS.steps });
  const drawnRuns = () =>
    [...document.querySelectorAll("[data-step-run]")].map((drawn) =>
      drawn.getAttribute("data-step-run")
    );
  try {
    await headedWith(30);
    expect(drawnRuns(), "the runs drawn are not the first 20 of every task's").toEqual(
      [...runsOf("docs", 15), ...runsOf("default", 5)].map((record) => record.id)
    );
    expect(
      within(section())
        .getAllByRole("heading", { level: 3 })
        .map((group) => group.textContent),
      "a task none of whose runs is drawn is drawn"
    ).toEqual([`${CONTROL_LABELS.steps_task} docs`, `${CONTROL_LABELS.steps_task} default`]);
    const more = within(section()).getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(10));

    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(drawnRuns()).toHaveLength(30);
    await headedWith(30);
    expect(more.isConnected, "Show more stayed once every run was drawn").toBe(false);
    expect(
      document.activeElement,
      "Show more did not move the focus to the first run it drew"
    ).toBe(run("default-6"));
  } finally {
    steps.restore();
  }
});

// A read Dispatch refused says so at once with its Retry, under the heading that has no count
// yet. The section's read has no retry policy of its own, so nothing asks again before Retry or
// the catch-up's next read, where the client's policy would ask twice within the second. Retry
// reads again and draws the runs; the focus it held goes to the heading once the line goes, never
// to the page. The catch-up's next read, 2 s after the opening, is refused too, and the runs the
// read before it drew stay drawn with no failure said over them. On the fake clock each read is
// the one the case names.
test("a refused read says so with a Retry that reads again, and a later failure says nothing over the runs", async () => {
  await onNoonClock(async () => {
    const user = userOnFakeClock();
    let refusing = true;
    const steps = renderSteps(async () => {
      if (refusing) throw new ApiError(500, { code: "INTERNAL", error: "refused" });
      return TWO_TASKS;
    });
    try {
      await tick(0);
      expect(screen.getByRole("alert").textContent).toBe(ANSWER_LINES.steps_unreadable);
      expect(heading().textContent, "the heading counts runs no read has answered").toBe(
        CONTROL_LABELS.steps
      );
      expect(screen.queryByText(ANSWER_LINES.loading_steps)).toBeNull();
      await tick(1_000, 100);
      expect(
        steps.listSteps,
        "the refused read was asked again before Retry or the catch-up's next read"
      ).toHaveBeenCalledTimes(1);

      refusing = false;
      const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
      act(() => retry.focus());
      await user.keyboard("{Enter}");
      await tick(0);
      expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (5)`);
      expect(steps.listSteps).toHaveBeenCalledTimes(2);
      expect(screen.queryByRole("alert")).toBeNull();
      expect(document.activeElement, "the Retry that went left the focus on the page").toBe(
        heading()
      );

      refusing = true;
      await tick(1_000, 100);
      expect(steps.listSteps).toHaveBeenCalledTimes(3);
      expect(
        screen.queryByRole("alert"),
        "a later failure said a failure over the runs the read holds"
      ).toBeNull();
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
        "a later failure offered a Retry over the runs the read holds"
      ).toBeNull();
      expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (5)`);
      expect(
        run("docs-1"),
        "a later failure took away the runs the read holds"
      ).toBeInTheDocument();
    } finally {
      steps.restore();
    }
  });
});

// Shut, Details reads no steps; opened, the section reads them once.
test("the section reads nothing while Details is shut", async () => {
  const steps = renderSteps(async () => TWO_TASKS, false);
  try {
    expect(heading().textContent).toBe(CONTROL_LABELS.steps);
    expect(screen.getByText(ANSWER_LINES.loading_steps)).toBeDefined();
    await act(async () => {});
    expect(steps.listSteps, "the steps were read with Details shut").not.toHaveBeenCalled();

    steps.open();
    await headedWith(5);
    expect(steps.listSteps).toHaveBeenCalledTimes(1);
  } finally {
    steps.restore();
  }
});

// Opening Details sends one request for the steps, the catch-up's first read, and the next read
// is the catch-up's own, 2 s on. A query enabled while Details is open would read on its own as
// Details opens, in the commit the catch-up reads in, and the scheduler answers a read it is asked
// for only with a fetch begun after it, so the catch-up's read would follow a second later.
test("opening Details sends one request for the steps, and the next read is the catch-up's", async () => {
  await onNoonClock(async () => {
    const steps = renderSteps(async () => TWO_TASKS, false);
    try {
      await tick(0);
      expect(steps.listSteps, "the steps were read with Details shut").not.toHaveBeenCalled();

      steps.open();
      const openedAt = Date.now();
      await tick(1_900, 100);
      expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (5)`);
      expect(
        steps.fetchedAt.map((time) => time - openedAt),
        "opening Details sent more than one request for the steps"
      ).toEqual([0]);
      await tick(100, 100);
      expect(steps.fetchedAt.map((time) => time - openedAt)).toEqual([0, 2_000]);
    } finally {
      steps.restore();
    }
  });
});

// A step record leaves the daemon on a lane of its own and sends no event, so a run whose record
// reaches the server only after the opening's read is drawn by the catch-up's next read, with no
// event and no reload.
test("with Details open, a run the server holds only after the first read shows at the catch-up's next read", async () => {
  await onNoonClock(async () => {
    let answer = TWO_TASKS;
    const steps = renderSteps(async () => answer);
    try {
      await tick(0);
      expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (5)`);

      answer = WITH_LATER_RUN;
      await tick(1_900, 100);
      expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (5)`);
      expect(document.querySelector(`[data-step-run="${LATER_RUN.id}"]`)).toBeNull();
      await tick(100, 100);
      expect(heading().textContent).toBe(`${CONTROL_LABELS.steps} (6)`);
      expect(run(LATER_RUN.id).closest("[data-step-task]")?.getAttribute("data-step-task")).toBe(
        "docs"
      );
      expect(fieldsOf(LATER_RUN.id)).toMatchObject({
        ended: drawnAt(at(11, 50)),
        started: drawnAt(at(11, 20)),
        step: "2",
      });
      expect(steps.listSteps).toHaveBeenCalledTimes(2);
    } finally {
      steps.restore();
    }
  });
});

// Shutting Details stops its catch-up: nothing reads the steps after it, and Details opened again
// reads on its new catch-up alone. A read asked while Details is shut is dropped, since nothing
// holds the query then, so a timer of the first catch-up left running would show only once
// Details holds the query again: Details opens again at 4 s, before that catch-up's 5 s read,
// which would then land a second after the opening.
test("shutting Details stops the catch-up: nothing reads after it, and Details opened again reads on its own", async () => {
  await onNoonClock(async () => {
    const openedAt = Date.now();
    const steps = renderSteps(async () => TWO_TASKS);
    try {
      await tick(1_000, 100);
      steps.shut();
      await tick(3_000, 100);
      expect(
        steps.fetchedAt.map((time) => time - openedAt),
        "the steps were read after Details shut"
      ).toEqual([0]);

      const openedAgainAt = Date.now();
      steps.open();
      await tick(2_000, 100);
      expect(steps.fetchedAt.slice(1).map((time) => time - openedAgainAt)).toEqual([0, 2_000]);

      steps.shut();
      await tick(600_000, 1_000);
      expect(steps.listSteps, "the steps were read after Details shut").toHaveBeenCalledTimes(3);
    } finally {
      steps.restore();
    }
  });
});

const issue = {
  artifacts: [],
  key: KEY,
  status: "in_progress",
  title: "Review the spec",
} as unknown as IssueDetails;

/** Details' history and what follows it, as the page draws them inside the drawer, which starts
 *  shut; `hold.open` opens and shuts it. */
function HistoryInDrawer({ hold }: { hold: { open?: (open: boolean) => void } }): ReactNode {
  const [open, setOpen] = useState(false);
  hold.open = setOpen;
  const drawer: MarginDrawer = { open, setOpen, setPage: () => undefined };
  return (
    <MarginSlotProvider drawer={drawer}>
      <DetailsHistory issue={issue} state={undefined} />
    </MarginSlotProvider>
  );
}

// The section stands under the history in Details: a load that leaves Details shut draws neither
// and reads no steps, and an open of Details draws it under the history and reads them.
test("Details draws the steps under the history only once it opens, and reads them only then", async () => {
  const listSteps = spyOn(api, "listSteps").mockResolvedValue(TWO_TASKS);
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const client = appClient();
  // The outline's own reads, which the history draws from, held as the outline left them.
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  client.setQueryData(["issue-messages", KEY], []);
  client.setDefaultOptions({
    queries: { retry: queryRetry, retryDelay: queryRetryDelay, staleTime: 30_000 },
  });
  const hold: { open?: (open: boolean) => void } = {};
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${KEY}`]}>
      <QueryClientProvider client={client}>
        <HistoryInDrawer hold={hold} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await act(async () => {});
    expect(screen.queryByTestId("issue-steps")).toBeNull();
    expect(
      listSteps,
      "the steps were read on a load that left Details shut"
    ).not.toHaveBeenCalled();

    act(() => hold.open?.(true));
    await headedWith(5);
    expect(listSteps).toHaveBeenCalledTimes(1);
    const history = screen.getByTestId("issue-history");
    expect(
      history.compareDocumentPosition(screen.getByTestId("issue-steps")) &
        Node.DOCUMENT_POSITION_FOLLOWING,
      "the steps are not drawn under the history"
    ).toBeTruthy();
  } finally {
    view.unmount();
    client.clear();
    listSteps.mockRestore();
    getIssueEvents.mockRestore();
  }
});
