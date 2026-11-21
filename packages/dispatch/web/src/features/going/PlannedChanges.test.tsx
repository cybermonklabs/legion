import { afterEach, expect, spyOn, test } from "bun:test";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type { GoingPlanAnswer, GoingPlanRow, GoingRun } from "../../api/types";
import { ConversationView } from "./ConversationView";
import { resetGoingStore, setPlace, startRun } from "./going-store";
import { PlannedChanges } from "./PlannedChanges";
import { PlanCard } from "./PlanRows";
import { argumentsOf } from "./plan-rows";
import { useOpenPlan } from "./usePlan";

// Where a planned change is drawn and how the person answers it (spec R4, plan.md settlement 7):
// a page draws the rows of the open plan that change the items it shows, the plan card draws the
// rest, a plan whose conversation is not open draws on no page, and the answer carries each
// row's tick, where unticking a row unticks every row that needs it, and each edited value.

function row(number: number, extra: Partial<GoingPlanRow>): GoingPlanRow {
  return {
    arguments: {},
    changed: false,
    claim: null,
    class: "field",
    fields: [],
    issue: "",
    item: "",
    needs: [],
    now: null,
    result: null,
    row: number,
    state: "waiting",
    tool: "dispatch_issue_update",
    waits: ["ask"],
    ...extra,
  };
}

/** A split of CORE-1: a new issue under CORE-1, a move of CORE-3 under it that needs it, and a
 *  message to the agent of CORE-1, which no list draws. */
const plan: GoingPlanAnswer = {
  mode: "ask",
  plan: 1,
  rows: [
    row(0, {
      arguments: { parent: "CORE-1", project: "CORE", title: "The second half" },
      class: "create_plain",
      tool: "dispatch_issue",
    }),
    row(1, {
      arguments: { issue: "CORE-3", parent: "CORE-2" },
      fields: ["parent"],
      issue: "CORE-3",
      item: "CORE-3",
      needs: [0],
      now: { parent: "CORE-1" },
    }),
    row(2, {
      arguments: { body: "Please review the split.", issue: "CORE-1" },
      class: "message",
      issue: "CORE-1",
      item: "CORE-1",
      tool: "dispatch_message",
    }),
  ],
  run: "run-1",
  state: "waiting",
  summary: "Split CORE-1 in two.",
  version: "5be1c0",
};

const created: GoingRun = {
  context: null,
  continues: null,
  conversation: "run-1",
  created_at: new Date().toISOString(),
  finished_at: null,
  id: "run-1",
  issues: [],
  level: "standard",
  mode: "ask",
  state: "queued",
  stop_reason: null,
  used: { calls: 0, tokens: 0, working_ms: 0 },
  words: "split CORE-1 in two",
};

function Card(): ReactNode {
  return <PlanCard answering={useOpenPlan()} />;
}

/** Starts the conversation's run, whose stream carries its plan frame and then stays open while
 *  the plan waits, and shows the conversation over the page unless `place` keeps it closed. The
 *  plan is read with `readPlan`; `send` hands the stream another frame. */
async function openPlan(
  place: "over" | "closed" = "over",
  readPlan: () => Promise<GoingPlanAnswer> = async () => plan
): Promise<{ readonly restore: () => void; readonly send: (name: string, data: unknown) => void }> {
  const start = spyOn(api, "createGoingRun").mockResolvedValue(created);
  let frames: ((name: string, data: unknown) => void) | undefined;
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    frames = onFrame;
    onFrame("plan", { plan: 1, state: "waiting" });
    return Promise.withResolvers<void>().promise;
  });
  const read = spyOn(api, "goingPlan").mockImplementation(readPlan);
  await startRun("split CORE-1 in two", { path: "/projects/CORE/issues" }, "alice");
  setPlace(place);
  return {
    restore: () => {
      start.mockRestore();
      stream.mockRestore();
      read.mockRestore();
    },
    send: (name, data) => act(() => frames?.(name, data)),
  };
}

function drawnRows(within_: HTMLElement): (string | null)[] {
  return within(within_)
    .getAllByTestId("going-plan-row")
    .map((drawn) => drawn.getAttribute("data-row"));
}

afterEach(() => {
  cleanup();
  resetGoingStore();
});

test("a plan with rows on the page and in the card shows each in its place, once", async () => {
  const { restore } = await openPlan();
  try {
    renderWithQueries(
      <>
        <PlannedChanges
          surface={{ issues: ["CORE-1", "CORE-3"], kind: "issues", project: "CORE" }}
        />
        <Card />
      </>
    );
    const page = await screen.findByTestId("going-plan-on-page");
    await waitFor(() =>
      expect(drawnRows(screen.getByTestId("going-plan-card-rows"))).toEqual(["2"])
    );
    expect(drawnRows(page)).toEqual(["0", "1"]);
  } finally {
    restore();
  }
});

test("a plan waiting in the Inbox draws on no page until its conversation is opened", async () => {
  // The run's plan waits, and its conversation is on no part of the screen: the bar was closed.
  const { restore } = await openPlan("closed");
  try {
    renderWithQueries(
      <PlannedChanges surface={{ issues: ["CORE-1", "CORE-3"], kind: "issues", project: "CORE" }} />
    );
    const pause = Promise.withResolvers<void>();
    setTimeout(pause.resolve, 50);
    await pause.promise;
    expect(screen.queryByTestId("going-plan-on-page")).toBeNull();
    // Opened again, its plan draws on the page.
    act(() => setPlace("over"));
    expect(await screen.findByTestId("going-plan-on-page")).toBeInTheDocument();
  } finally {
    restore();
  }
});

test("unticking a row unticks the row that needs it, and the approval carries each tick and each edited value", async () => {
  const user = userEvent.setup();
  const { restore } = await openPlan();
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementation(async () => ({ ...plan, state: "running" }));
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan-card-rows");
    // Each row's tick is named by its change and its item, a new issue's by its title.
    await user.click(within(card).getByRole("checkbox", { name: "Run issue The second half" }));
    const ticks = within(card)
      .getAllByTestId("going-plan-row")
      .map((drawn) => drawn.getAttribute("data-ticked"));
    expect(ticks).toEqual(["false", "false", "true"]);
    // Ticking the move again ticks the new issue it needs.
    await user.click(within(card).getByRole("checkbox", { name: "Run issue update CORE-3" }));
    await user.click(within(card).getByRole("button", { name: "Edit title" }));
    const title = within(card).getByRole("textbox", { name: "New title" });
    await user.clear(title);
    await user.type(title, "The other half{Enter}");
    await user.click(screen.getByTestId("going-plan-approve"));

    await waitFor(() =>
      expect(approve).toHaveBeenCalledWith(
        "run-1",
        {
          plan: 1,
          rows: [
            { row: 0, tick: true, values: { title: "The other half" } },
            { row: 1, tick: true },
            { row: 2, tick: true },
          ],
          version: "5be1c0",
        },
        keyedAttempt
      )
    );
  } finally {
    restore();
  }
});

// A plan that could not be read is said to be, with its Retry, in the card that would show it:
// the conversation's plan waits, and the card is the one place the person can answer it.
test("a plan that cannot be read says so in the card, and Retry reads it again", async () => {
  const user = userEvent.setup();
  let reads = 0;
  const { restore } = await openPlan("over", async () => {
    reads += 1;
    if (reads === 1) throw new Error("the connection was reset");
    return plan;
  });
  try {
    renderWithQueries(<Card />);
    const unreadable = await screen.findByTestId("going-plan-unreadable");
    expect(within(unreadable).getByRole("alert")).toHaveTextContent(
      "The run's plan could not be read."
    );
    await user.click(within(unreadable).getByRole("button", { name: "Retry" }));
    expect(drawnRows(await screen.findByTestId("going-plan-card-rows"))).toEqual(["0", "1", "2"]);
  } finally {
    restore();
  }
});

// Dispatch's refusal of an answer is said under the plan it refused, and only there: the bar's card
// stays on the screen as the person starts another run, whose plan shows none of it.
test("a refusal of one run's plan does not show under the next run's plan", async () => {
  const user = userEvent.setup();
  const next: GoingRun = { ...created, conversation: "run-2", id: "run-2", words: "close CORE-5" };
  const nextPlan: GoingPlanAnswer = { ...plan, run: "run-2", summary: "Close CORE-5." };
  const start = spyOn(api, "createGoingRun")
    .mockResolvedValueOnce(created)
    .mockResolvedValueOnce(next);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    onFrame("plan", { plan: 1, state: "waiting" });
    return Promise.withResolvers<void>().promise;
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async (run) =>
    run === next.id ? nextPlan : plan
  );
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
  try {
    await startRun("split CORE-1 in two", { path: "/projects/CORE/issues" }, "alice");
    setPlace("over");
    renderWithQueries(<Card />);
    await user.click(await screen.findByTestId("going-plan-approve"));
    expect(await screen.findByTestId("going-plan-refused")).toHaveTextContent(
      "the plan was answered meanwhile, and no longer waits"
    );

    // The person asks for something else while the refused plan still waits: the bar's
    // conversation is now run-2's, and its card draws run-2's plan.
    await act(() => startRun("close CORE-5", { path: "/projects/CORE/issues" }, "alice"));
    await waitFor(() =>
      expect(screen.getByTestId("going-plan")).toHaveTextContent("Close CORE-5.")
    );
    expect(screen.queryByTestId("going-plan-refused")).toBeNull();
  } finally {
    start.mockRestore();
    stream.mockRestore();
    read.mockRestore();
  }
});

// An approval names the version of the plan the person was shown, and Dispatch refuses one over a
// plan that changed since it was read: the card says why under that plan and reads it again, so the
// next approval names the version the person sees now.
test("an approval of a plan that changed since it was read is refused, and the next names the plan as it is now", async () => {
  const user = userEvent.setup();
  const changed: GoingPlanAnswer = {
    ...plan,
    summary: "Split CORE-1 in three.",
    version: "9d02aa",
  };
  let reads = 0;
  const { restore } = await openPlan("over", async () => {
    reads += 1;
    return reads === 1 ? plan : changed;
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
  approve.mockImplementation(async () => ({ ...changed, state: "running" }));
  try {
    renderWithQueries(<Card />);
    await user.click(await screen.findByTestId("going-plan-approve"));
    expect(await screen.findByTestId("going-plan-refused")).toHaveTextContent(
      "This plan changed after this page read it. Read it again before you approve it."
    );
    expect(approve).toHaveBeenCalledWith(
      "run-1",
      expect.objectContaining({ plan: 1, version: "5be1c0" }),
      keyedAttempt
    );

    // The plan is read again and shows as it is now, the refusal still under it.
    await waitFor(() =>
      expect(screen.getByTestId("going-plan")).toHaveTextContent("Split CORE-1 in three.")
    );
    expect(screen.getByTestId("going-plan-refused")).toBeInTheDocument();
    await user.click(screen.getByTestId("going-plan-approve"));
    await waitFor(() =>
      expect(approve).toHaveBeenLastCalledWith(
        "run-1",
        expect.objectContaining({ plan: 1, version: "9d02aa" }),
        keyedAttempt
      )
    );
    expect(approve).toHaveBeenCalledTimes(2);
  } finally {
    restore();
  }
});

// A row's edit box starts from the value of the plan it is drawn for: after the person revises a
// plan, its rows keep their numbers in the plan that takes its place, and Edit opens with that
// plan's value, not the earlier plan's or what the person typed into it and did not keep.
test("after a revise, a row's Edit opens with the new plan's value", async () => {
  const user = userEvent.setup();
  const revised: GoingPlanAnswer = {
    ...plan,
    plan: 2,
    rows: (plan.rows ?? []).map((planned) =>
      planned.row === 0
        ? { ...planned, arguments: { ...argumentsOf(planned), title: "The first half" } }
        : planned
    ),
    summary: "Split CORE-1 in two, the new issue first.",
    version: "77c3e1",
  };
  let reads = 0;
  const { restore, send } = await openPlan("over", async () => {
    reads += 1;
    return reads === 1 ? plan : revised;
  });
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan-card-rows");
    // The person starts an edit of plan 1's title and leaves it without keeping it.
    await user.click(within(card).getByRole("button", { name: "Edit title" }));
    await user.type(within(card).getByRole("textbox", { name: "New title" }), " again{Escape}");

    // They revise the plan in words, and the run proposes plan 2 in its place.
    send("plan", { plan: 2, state: "waiting" });
    await waitFor(() => expect(screen.getByTestId("going-plan")).toHaveAttribute("data-plan", "2"));
    await user.click(
      within(screen.getByTestId("going-plan-card-rows")).getByRole("button", {
        name: "Edit title",
      })
    );
    expect(screen.getByRole("textbox", { name: "New title" })).toHaveValue("The first half");
  } finally {
    restore();
  }
});

// An upload over a document the owner already holds writes the document's next version: its row's
// diff takes the document's text now out and puts the uploaded text in, word by word, so the person
// sees what the upload replaces and not only what it writes.
test("an upload row over a document shows that document's text taken out and the new text put in", async () => {
  const user = userEvent.setup();
  const upload: GoingPlanAnswer = {
    ...plan,
    rows: [
      row(0, {
        arguments: {
          content: "The split moves CORE-3 under the new issue.\n",
          issue: "CORE-1",
          name: "design.md",
          summary: "Say where CORE-3 goes.",
        },
        class: "create_text",
        issue: "CORE-1",
        item: "8c1f0a52-4d1e-4f7b-9a43-0d6c1b2e7f10",
        now: {
          content: "The split keeps CORE-3 where it is.\n",
          id: "8c1f0a52-4d1e-4f7b-9a43-0d6c1b2e7f10",
          name: "design.md",
          version: 3,
        },
        tool: "dispatch_artifact",
      }),
    ],
    summary: "Rewrite the design of the split.",
  };
  const { restore } = await openPlan("over", async () => upload);
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan-card-rows");
    expect(within(card).getByTestId("going-plan-sentences")).toHaveTextContent(
      "Say where CORE-3 goes."
    );
    await user.click(within(card).getByText("The diff"));
    const piece = within(card).getByTestId("going-plan-diff-piece");
    // The words both texts share are left unmarked; only what differs is taken out or put in.
    expect([...piece.querySelectorAll("del")].map((part) => part.textContent).join(" ")).toBe(
      "keeps where it is"
    );
    expect([...piece.querySelectorAll("ins")].map((part) => part.textContent).join(" ")).toBe(
      "moves under the new issue"
    );
  } finally {
    restore();
  }
});

// No plan frame says that a row ran, so the plan is read again once the run ends: the rows it
// ran show as done, and the run's result offers Undo.
function Conversation(): ReactNode {
  return <ConversationView answering={useOpenPlan()} onContinue={() => {}} />;
}

test("a run whose plan ran offers Undo once it ends, its plan read again", async () => {
  const approved: GoingPlanAnswer = {
    ...plan,
    rows: (plan.rows ?? []).map((planned) => ({ ...planned, state: "approved" })),
    state: "running",
  };
  const ran: GoingPlanAnswer = {
    ...plan,
    rows: (plan.rows ?? []).map((planned) => ({ ...planned, state: "done" })),
    state: "done",
  };
  let reads = 0;
  const { restore, send } = await openPlan("over", async () => {
    reads += 1;
    return reads === 1 ? plan : reads === 2 ? approved : ran;
  });
  try {
    renderWithQueries(<Conversation />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.queryByTestId("going-undo-button")).toBeNull();

    // Approved in another tab: the run takes its plan up again, runs its rows and ends.
    send("plan", { plan: 1, state: "running" });
    await waitFor(() => expect(reads).toBe(2));
    send("tool", { note: "issue CORE-9", step: 3, tool: "dispatch_issue" });
    send("end", { offers: [], reason: "", state: "done" });

    expect(await screen.findByTestId("going-undo-button")).toBeInTheDocument();
  } finally {
    restore();
  }
});
