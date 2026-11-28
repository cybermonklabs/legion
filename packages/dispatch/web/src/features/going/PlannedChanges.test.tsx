import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type { GoingPlanAnswer, GoingPlanRow, GoingRun } from "../../api/types";
import { issue as issueFixture } from "../margin/margin-fixture";
import { ConversationView } from "./ConversationView";
import { goingState, resetGoingStore, setPlace, startRun } from "./going-store";
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
  line: null,
  mode: "ask",
  standing_count: 0,
  state: "queued",
  stop_reason: null,
  undoable: false,
  undoable_count: 0,
  undone_at: null,
  undone_count: 0,
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

// Each change row's item is an issue chip, which reads its issue's summary: the one read every
// reference to the issue shares. A message on an issue reads the issue itself and the sessions
// that follow it, for whom Dispatch delivers it to: here an issue that routes its messages
// nowhere, that nobody claims and that nobody follows. A run's record is read again at each
// frame after which rows of its plan may have run (`rereadRun`): unless a row answers that read
// itself, Dispatch answers the record the page already holds.
let summaries: { mockRestore: () => void } | undefined;
let issues: { mockRestore: () => void } | undefined;
let followers: { mockRestore: () => void } | undefined;
let rereads: { mockRestore: () => void } | undefined;
beforeEach(() => {
  summaries = spyOn(api, "getIssueSummary").mockImplementation(async (key: string) => ({
    documents: [],
    key,
    project: "CORE",
    status: "todo",
    title: `The issue ${key}`,
  }));
  issues = spyOn(api, "getIssue").mockImplementation(async (key: string) => ({
    ...issueFixture,
    claim: null,
    key,
    route: null,
  }));
  followers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  rereads = spyOn(api, "goingRun").mockImplementation(async (id) => {
    const held = goingState().runs.get(id)?.record;
    if (held === undefined) throw new Error(`the page holds no run ${id}`);
    return held;
  });
});

afterEach(() => {
  cleanup();
  resetGoingStore();
  summaries?.mockRestore();
  issues?.mockRestore();
  followers?.mockRestore();
  rereads?.mockRestore();
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
    // Each row's tick is named by what it does and its item, a new issue's by its title.
    await user.click(within(card).getByRole("checkbox", { name: "File The second half" }));
    const ticks = within(card)
      .getAllByTestId("going-plan-row")
      .map((drawn) => drawn.getAttribute("data-ticked"));
    expect(ticks).toEqual(["false", "false", "true"]);
    // Ticking the move again ticks the new issue it needs.
    await user.click(within(card).getByRole("checkbox", { name: "Update CORE-3" }));
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

// A move under a new issue of the plan names that issue "$0", the number of the row that creates
// it, which the server puts the new issue's key in place of when the move runs: the row shows the
// issue by its title and "(new)", the person's edit of that title included, and the person can
// replace the value with an issue's key, which the approval carries.
test("a value naming a new issue of the plan shows its title and (new), and Edit replaces it with a key", async () => {
  const user = userEvent.setup();
  const split: GoingPlanAnswer = {
    ...plan,
    rows: (plan.rows ?? []).map((planned) =>
      planned.row === 1 ? { ...planned, arguments: { issue: "CORE-3", parent: "$0" } } : planned
    ),
  };
  const { restore } = await openPlan("over", async () => split);
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementation(async () => ({ ...split, state: "running" }));
  const move = (): HTMLElement =>
    within(screen.getByTestId("going-plan-card-rows")).getAllByTestId(
      "going-plan-row"
    )[1] as HTMLElement;
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan-card-rows");
    expect(within(move()).getByTestId("going-plan-now")).toHaveTextContent("CORE-1");
    expect(within(move()).getByTestId("going-plan-next")).toHaveTextContent(
      "The second half (new)"
    );

    // The new issue's title edited, the move names it so.
    await user.click(within(card).getByRole("button", { name: "Edit title" }));
    const title = within(card).getByRole("textbox", { name: "New title" });
    await user.clear(title);
    await user.type(title, "The other half{Enter}");
    expect(within(move()).getByTestId("going-plan-next")).toHaveTextContent("The other half (new)");

    // The move's parent replaced with an issue's key: its edit box starts from the value itself.
    await user.click(within(move()).getByRole("button", { name: "Edit parent" }));
    const parent = within(move()).getByRole("textbox", { name: "New parent" });
    expect(parent).toHaveValue("$0");
    await user.clear(parent);
    await user.type(parent, "CORE-2{Enter}");
    expect(within(move()).getByTestId("going-plan-next")).toHaveTextContent("CORE-2");
    await user.click(screen.getByTestId("going-plan-approve"));

    await waitFor(() =>
      expect(approve).toHaveBeenCalledWith(
        "run-1",
        {
          plan: 1,
          rows: [
            { row: 0, tick: true, values: { title: "The other half" } },
            { row: 1, tick: true, values: { parent: "CORE-2" } },
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

// A text that reads as "$N" names no new issue: the server sends it as written, and the row shows
// it so. Only a field that takes an issue key, such as parent, shows a new issue of the plan by
// its title and "(new)".
test('titles "$100" and "$0" show as written, and only the parent "$0" shows the new issue', async () => {
  const priced: GoingPlanAnswer = {
    ...plan,
    rows: (plan.rows ?? []).map((planned) => {
      if (planned.row === 0) {
        return { ...planned, arguments: { ...argumentsOf(planned), title: "$100" } };
      }
      if (planned.row === 1) {
        return {
          ...planned,
          arguments: { issue: "CORE-3", parent: "$0", title: "$0" },
          fields: ["title", "parent"],
          now: { parent: "CORE-1", title: "The first half" },
        };
      }
      return planned;
    }),
  };
  const { restore } = await openPlan("over", async () => priced);
  const next = (row: number, field: string): string | null => {
    const drawn = within(screen.getByTestId("going-plan-card-rows")).getAllByTestId(
      "going-plan-row"
    )[row] as HTMLElement;
    const shown = within(drawn)
      .getAllByTestId("going-plan-field")
      .find((element) => element.getAttribute("data-field") === field) as HTMLElement;
    return within(shown).getByTestId("going-plan-next").textContent;
  };
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(next(0, "title")).toBe("$100");
    expect(next(1, "title")).toBe("$0");
    expect(next(1, "parent")).toBe("$100 (new)");
  } finally {
    restore();
  }
});

// An update's before and after move its issue on its project's board, and its read names them as
// the field rank: the row shows where the move puts the issue, before or after the issue it names,
// a new issue of the plan by its title and "(new)", and offers no edit of those words.
test("a board move shows rank as before or after the issue it names, a new issue by its title and (new)", async () => {
  const moves: GoingPlanAnswer = {
    ...plan,
    rows: [
      ...(plan.rows ?? []).slice(0, 1),
      row(1, {
        arguments: { before: "CORE-9", issue: "CORE-3" },
        fields: ["rank"],
        issue: "CORE-3",
        item: "CORE-3",
        now: { rank: "a0" },
      }),
      row(2, {
        arguments: { after: "$0", issue: "CORE-4" },
        fields: ["rank"],
        issue: "CORE-4",
        item: "CORE-4",
        needs: [0],
        now: { rank: "a1" },
      }),
    ],
  };
  const { restore } = await openPlan("over", async () => moves);
  const rank = (row: number): HTMLElement => {
    const drawn = within(screen.getByTestId("going-plan-card-rows")).getAllByTestId(
      "going-plan-row"
    )[row] as HTMLElement;
    return within(drawn)
      .getAllByTestId("going-plan-field")
      .find((element) => element.getAttribute("data-field") === "rank") as HTMLElement;
  };
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(within(rank(1)).getByTestId("going-plan-next").textContent).toBe("before CORE-9");
    expect(within(rank(2)).getByTestId("going-plan-next").textContent).toBe(
      "after The second half (new)"
    );
    expect(within(rank(1)).queryByRole("button", { name: "Edit rank" })).toBeNull();
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

// An upload refused because its document moved after the plan read it says so in the page's own
// words: the route's words are written for a caller of the `dispatch` command, and tell them to run
// it and send a token, which the person who approved the row cannot do. Any other refusal of an
// upload keeps the words Dispatch wrote.
test("an upload refused because its document moved says so in the page's words, never the command's", async () => {
  const uploadRow = (number: number, name: string, extra: Partial<GoingPlanRow>): GoingPlanRow =>
    row(number, {
      arguments: { content: "The split moves CORE-3.\n", issue: "CORE-1", name },
      class: "create_text",
      issue: "CORE-1",
      item: name,
      state: "failed",
      tool: "dispatch_artifact",
      ...extra,
    });
  const ran: GoingPlanAnswer = {
    ...plan,
    rows: [
      uploadRow(0, "design.md", {
        result: {
          code: "PRECONDITION_FAILED",
          delivery: null,
          message:
            'the document moved past the token the upload named; read the document with `dispatch doc-read`, which prints `Document token: sha256:…`, and upload again naming that token as the upload\'s precondition, `{"document":"sha256:…"}`',
          status: 409,
        },
      }),
      uploadRow(1, "notes.md", {
        result: {
          code: "CAP_EXCEEDED",
          delivery: null,
          message: "a markdown document is at most 1 MiB",
          status: 413,
        },
      }),
    ],
    state: "done",
  };
  const { restore } = await openPlan("over", async () => ran);
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan-card-rows");
    const [moved, tooLarge] = within(card).getAllByTestId("going-plan-row");
    expect(moved).not.toHaveTextContent("dispatch doc-read");
    expect(moved).toHaveTextContent(
      "The document under this name changed after the plan read it, so the upload wrote nothing."
    );
    expect(tooLarge).toHaveTextContent("a markdown document is at most 1 MiB");
  } finally {
    restore();
  }
});

// A refusal whose route words tell an agent to run a `dispatch` command says what happened in the
// page's words: an edit of an approval request says to change its document, a question written
// into a decision that cannot take one there says to write it in the document editor, and any
// other such refusal says Dispatch refused it and to change the item here. A refusal that names
// no command, and a row that was refused nothing, keep their words.
test("a refusal that names a dispatch command says what happened in the page's words", async () => {
  const refusedRow = (number: number, tool: string, code: string, message: string): GoingPlanRow =>
    row(number, {
      arguments: { id: `ask-${number}`, question: "Ship it on Friday?" },
      issue: "CORE-1",
      item: `ask-${number}`,
      result: { code, delivery: null, message, status: 409 },
      state: "failed",
      tool,
    });
  const ran: GoingPlanAnswer = {
    ...plan,
    rows: [
      refusedRow(
        0,
        "dispatch_edit_ask",
        "ASK_KIND_FIXED",
        "an approval ask's question and options are fixed; revise its document, then, once the human has agreed to every point in it, hand the request back with `dispatch request-approval`"
      ),
      refusedRow(
        1,
        "dispatch_edit_ask",
        "ASK_BLOCK_TEXT",
        'block: ask block "blk-7" has an empty question, and writing a question into it would delete what one of its paragraphs holds, which is no text: "![]()"; no document edit writes a paragraph into a decision, so none reaches this block - upload the document again as a new version with the question written beside that paragraph (dispatch artifact --issue <KEY>, or --project <KEY> for a project\'s document, --name <the document> --content-file - --force), or write the question beside it in the document editor; either keeps what the paragraph holds'
      ),
      refusedRow(
        2,
        "dispatch_edit_ask",
        "ASK_BLOCK_TEXT",
        'block: the edit drops option "B" of ask block "blk-9", whose list item holds more than its plain line; remove it with a document edit first (dispatch doc-edit, deleting a block by its id), then send the ask edit again'
      ),
      // The other empty-question refusal names no command, and keeps its words.
      refusedRow(
        3,
        "dispatch_edit_ask",
        "ASK_BLOCK_TEXT",
        'block: ask block "blk-8" has an empty question; send this ask edit again with a question, which writes it into the block'
      ),
      // A row that waits names the row it needs by its item's title, which can hold the words
      // "dispatch search": it was refused nothing, and keeps its words.
      row(4, {
        arguments: { issue: "CORE-1", priority: 1 },
        issue: "CORE-1",
        item: "CORE-1",
        result: {
          code: "",
          delivery: null,
          message: 'waits for the row that files "make dispatch search faster"',
          status: 0,
        },
        state: "waiting",
        tool: "dispatch_issue_update",
      }),
    ],
    state: "done",
  };
  const { restore } = await openPlan("over", async () => ran);
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan-card-rows");
    const drawn = within(card).getAllByTestId("going-plan-row");
    // No row tells the person to run a command.
    expect(
      drawn
        .slice(0, 3)
        .map((one) => one.textContent ?? "")
        .filter((text) => /\bdispatch [a-z]/u.test(text))
    ).toEqual([]);
    const [fixed, emptyQuestion, other, optionsOnly, waiting] = drawn;
    expect(fixed).toHaveTextContent(
      "An approval request's question and options cannot be edited, so nothing was changed. Change the document it asks about instead."
    );
    expect(emptyQuestion).toHaveTextContent(
      "Write the question beside that paragraph in the document editor."
    );
    expect(other).toHaveTextContent(
      "Dispatch refused this change with advice for its command-line tool"
    );
    expect(other).toHaveTextContent("Open the item to change it here.");
    expect(optionsOnly).toHaveTextContent("send this ask edit again with a question");
    expect(waiting).toHaveTextContent('waits for the row that files "make dispatch search faster"');
    expect(waiting).not.toHaveTextContent("Dispatch refused this change");
  } finally {
    restore();
  }
});

// No plan frame says that a row ran, so the plan is read again once the run ends: the rows it
// ran show as done, and the run's result offers Undo.
function Conversation(): ReactNode {
  return <ConversationView answering={useOpenPlan()} onContinue={() => {}} onRetry={() => {}} />;
}

// The card's Undo is offered once Dispatch's record of the run says Undo can take a change of it
// back: the record is read again with the plan, at each frame after which its rows may have run.
test("a run whose plan ran offers Undo once it ends, its plan and its record read again", async () => {
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
  // Dispatch's record of the run once its rows ran: two changes Undo can take back.
  const reread = spyOn(api, "goingRun").mockImplementation(async () => ({
    ...created,
    state: "done",
    undoable: true,
    undoable_count: 2,
  }));
  try {
    renderWithQueries(<Conversation />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.queryByTestId("going-undo-button")).toBeNull();
    expect(reread).not.toHaveBeenCalled();

    // Approved in another tab: the run takes its plan up again, runs its rows and ends.
    send("plan", { plan: 1, state: "running" });
    await waitFor(() => expect(reads).toBe(2));
    send("tool", { note: "issue CORE-9", step: 3, tool: "dispatch_issue" });
    send("end", { offers: [], reason: "", state: "done" });

    expect(await screen.findByTestId("going-undo-button")).toHaveTextContent(/^Undo 2 changes$/u);
    expect(reread).toHaveBeenCalledWith("run-1");
  } finally {
    reread.mockRestore();
    restore();
  }
});

// A row whose item another person changed after the plan showed waits again (design v3, state 6):
// the approval runs the other rows, and the turn that ran them ends waiting. No plan frame says a
// row ran, so the plan is read again at that end, and the card draws the changed row to be ticked
// again rather than the approved plan it read before the rows ran.
test("a turn that ran the approved rows and waits again reads its plan again, and the changed row waits to be ticked again", async () => {
  const approved: GoingPlanAnswer = {
    ...plan,
    rows: (plan.rows ?? []).map((planned) =>
      planned.row === 1
        ? { ...planned, changed: true, now: { parent: "CORE-5" } }
        : { ...planned, state: "approved" }
    ),
    state: "approved",
  };
  const waitsAgain: GoingPlanAnswer = {
    ...approved,
    rows: (approved.rows ?? []).map((planned) =>
      planned.row === 1 ? planned : { ...planned, state: "done" }
    ),
    state: "waiting",
  };
  let reads = 0;
  const { restore, send } = await openPlan("over", async () => {
    reads += 1;
    return reads === 1 ? plan : reads === 2 ? approved : waitsAgain;
  });
  try {
    renderWithQueries(<Conversation />);
    await screen.findByTestId("going-plan-card-rows");

    // Approved: the run takes its plan up again, runs the approved rows, and its turn ends
    // waiting on the row whose item changed.
    send("plan", { plan: 1, state: "approved" });
    await waitFor(() => expect(reads).toBe(2));
    send("tool", { kind: "write", step: 3, tool: "dispatch_issue" });
    send("end", { offers: [], reason: "", state: "waiting" });

    const changed = await screen.findByTestId("going-plan-changed");
    expect(reads).toBe(3);
    const changedRow = changed.closest<HTMLElement>('[data-testid="going-plan-row"]');
    expect(changedRow).toHaveAttribute("data-row", "1");
    expect(changedRow).toHaveAttribute("data-state", "waiting");
    // Its tick starts off: the person ticks it again to approve the value it shows now.
    expect(within(changedRow as HTMLElement).getByRole("checkbox")).not.toBeChecked();
    expect(screen.getByTestId("going-run")).toHaveAttribute("data-state", "waiting");
  } finally {
    restore();
  }
});

// An answer to the plan never takes the place of a newer read of it: the approval's answer is the
// plan as Dispatch took the approval, and the read at the end of the turn that ran its rows is
// newer. Nothing orders two reads of one plan, so that read stays and the plan is read again.
test("an approval's answer that lands after a newer read of the plan keeps that read, and reads the plan again", async () => {
  const user = userEvent.setup();
  const approved: GoingPlanAnswer = {
    ...plan,
    rows: (plan.rows ?? []).map((planned) =>
      planned.row === 1
        ? { ...planned, changed: true, now: { parent: "CORE-5" } }
        : { ...planned, state: "approved" }
    ),
    state: "approved",
    version: "a77e10",
  };
  const waitsAgain: GoingPlanAnswer = {
    ...approved,
    rows: (approved.rows ?? []).map((planned) =>
      planned.row === 1 ? planned : { ...planned, state: "done" }
    ),
    state: "waiting",
    version: "3a9c2f",
  };
  let reads = 0;
  const { restore, send } = await openPlan("over", async () => {
    reads += 1;
    return reads === 1 ? plan : reads === 2 ? approved : waitsAgain;
  });
  const answer = Promise.withResolvers<GoingPlanAnswer>();
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementation(() => answer.promise);
  try {
    renderWithQueries(<Conversation />);
    await screen.findByTestId("going-plan-card-rows");
    await user.click(screen.getByTestId("going-plan-approve"));
    await waitFor(() => expect(approve).toHaveBeenCalledTimes(1));

    // While the answer is out, the run takes its plan up, runs the approved rows and waits again
    // on the changed row, which the plan read at that end shows.
    send("plan", { plan: 1, state: "approved" });
    await waitFor(() => expect(reads).toBe(2));
    send("tool", { kind: "write", step: 3, tool: "dispatch_issue" });
    send("end", { offers: [], reason: "", state: "waiting" });
    await screen.findByTestId("going-plan-changed");

    // The answer lands last: the read after the rows ran stays, and the plan is read again.
    answer.resolve(approved);
    await waitFor(() => expect(reads).toBe(4));
    expect(screen.getByTestId("going-plan-changed")).toBeInTheDocument();
    expect(screen.getByTestId("going-plan")).toHaveAttribute("data-state", "waiting");
  } finally {
    restore();
  }
});
