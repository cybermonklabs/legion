import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import { goingConversationsQuery } from "../../api/queries";
import type { GoingConversation, GoingPlanRow, GoingRun, GoingUndoAnswer } from "../../api/types";
import { ConversationView } from "./ConversationView";
import { goingState, openConversation, resetGoingStore } from "./going-store";
import { History } from "./History";
import { useOpenPlan } from "./usePlan";

// History as the person reads it (spec R6, R11; design v3, state 11): the conversations of the
// last 7 days grouped Today, Yesterday and This week, each row its title, its state in words and
// its time; a box that filters them as the person types; the keys that move through them and open
// one; the Undo of a row whose newest run holds a write Undo can still take back, as a filing
// from the line on top does; and a press that shows a conversation in the bar.

const now = Date.now();
/** The reader's midnight today: a turn an hour before it was yesterday, one 3 days before it
 *  this week, whatever the time of day the test runs at. */
const midnight = new Date(new Date(now).setHours(0, 0, 0, 0)).getTime();

function run(id: string, words: string, at: number, extra: Partial<GoingRun> = {}): GoingRun {
  const iso = new Date(at).toISOString();
  return {
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: iso,
    finished_at: iso,
    id,
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
    used: { calls: 2, tokens: 10, working_ms: 1000 },
    words,
    ...extra,
  };
}

/** Three conversations, the newest first: today's split, whose second run changed two issues;
 *  yesterday's question, whose plan still waits; and this week's filing from the line on top,
 *  which filed CORE-46. */
const conversations: GoingConversation[] = [
  {
    id: "c-3",
    newest_at: new Date(now).toISOString(),
    runs: [
      run("r-4", "now move CORE-3 under the new one", now, {
        continues: "r-3",
        conversation: "c-3",
        issues: ["CORE-1", "CORE-3"],
        undoable: true,
      }),
      run("r-3", "split CORE-1 in two", now, { conversation: "c-3" }),
    ],
  },
  {
    id: "c-2",
    newest_at: new Date(midnight - 3_600_000).toISOString(),
    runs: [
      run("r-2", "close the stale duplicates", midnight - 3_600_000, {
        conversation: "c-2",
        finished_at: null,
        state: "waiting",
      }),
    ],
  },
  {
    id: "c-1",
    newest_at: new Date(midnight - 3 * 86_400_000).toISOString(),
    runs: [
      run("r-1", "file the telescope's mirror as a new issue", midnight - 3 * 86_400_000, {
        issues: ["CORE-46"],
        level: "fast",
        undoable: true,
      }),
    ],
  },
];

function listConversations(rows: GoingConversation[] = conversations) {
  return spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows,
    total: rows.length,
  });
}

function renderHistory(onOpen: () => void = () => {}): void {
  renderWithQueries(<History onOpen={onOpen} />);
}

/** The conversation as the bar draws it beside History. */
function Conversation(): ReactNode {
  return <ConversationView answering={useOpenPlan()} onContinue={() => {}} onRetry={() => {}} />;
}

/** The titles of the rows History lists, in its order. */
function titles(): (string | null)[] {
  return screen.queryAllByTestId("going-history-title").map((title) => title.textContent);
}

// History's Undo reads the run's plan as it is pressed, to name each line by its row
// (`RunUndo`). By default the run has no plan the page can read, and a line is named by its tool
// and its item.
let plans: { mockRestore: () => void } | undefined;
beforeEach(() => {
  plans = spyOn(api, "goingPlan").mockRejectedValue(
    new ApiError(404, { code: "GOING_PLAN_NOT_FOUND", error: "the run proposed no plan" })
  );
});

afterEach(() => {
  cleanup();
  resetGoingStore();
  plans?.mockRestore();
});

test("History groups the conversations Today, Yesterday and This week, each row its title, state and time, and Undo only where its newest run changed something", async () => {
  const read = listConversations();
  try {
    renderHistory();
    const groups = await screen.findAllByTestId("going-history-group");
    expect(groups.map((group) => group.getAttribute("data-group"))).toEqual([
      "today",
      "yesterday",
      "week",
    ]);
    expect(groups.map((group) => within(group).getByRole("heading").textContent)).toEqual([
      "Today",
      "Yesterday",
      "This week",
    ]);
    const rows = screen.getAllByTestId("going-history-conversation");
    expect(
      rows.map((row) => [
        row.getAttribute("data-conversation"),
        within(row).getByTestId("going-history-title").textContent,
        within(row).getByTestId("going-history-state").textContent,
        row.querySelector("time")?.getAttribute("datetime"),
      ])
    ).toEqual([
      // Each title is the first question with a capital, as a title starts.
      ["c-3", "Split CORE-1 in two", "Done", conversations[0]?.newest_at],
      ["c-2", "Close the stale duplicates", "Needs you", conversations[1]?.newest_at],
      ["c-1", "File the telescope's mirror as a new issue", "Done", conversations[2]?.newest_at],
    ]);
    // The split's newest run touched CORE-1 and CORE-3, and the filing filed CORE-46: each row
    // holds its run's Undo. The plan that waits changed nothing yet.
    const undos = screen.getAllByTestId("going-undo");
    expect(undos.map((undo) => undo.getAttribute("data-run"))).toEqual(["r-4", "r-1"]);
  } finally {
    read.mockRestore();
  }
});

// R6: History offers Undo only while the run holds a write Undo can take back, as Dispatch keeps
// it (`undoable`): a run undone already - on this page or another, before a reload - and a run
// whose writes Undo cannot take back offer none, whatever issues they touched.
test("History offers Undo only while the run holds a write Undo can still take back", async () => {
  const read = listConversations([
    {
      id: "c-5",
      newest_at: new Date(now).toISOString(),
      runs: [
        run("r-5", "raise CORE-5", now, {
          conversation: "c-5",
          issues: ["CORE-5"],
          undone_at: new Date(now).toISOString(),
        }),
      ],
    },
    {
      id: "c-6",
      newest_at: new Date(now - 60_000).toISOString(),
      runs: [
        run("r-6", "tell CORE-6's agent to stop", now - 60_000, {
          conversation: "c-6",
          issues: ["CORE-6"],
        }),
      ],
    },
    {
      id: "c-7",
      newest_at: new Date(now - 120_000).toISOString(),
      runs: [
        run("r-7", "raise CORE-7", now - 120_000, {
          conversation: "c-7",
          issues: ["CORE-7"],
          undoable: true,
        }),
      ],
    },
  ]);
  try {
    renderHistory();
    await waitFor(() =>
      expect(screen.getAllByTestId("going-history-conversation")).toHaveLength(3)
    );
    expect(
      screen.getAllByTestId("going-undo").map((undo) => undo.getAttribute("data-run"))
    ).toEqual(["r-7"]);
  } finally {
    read.mockRestore();
  }
});

// A row whose newest run the model's provider stopped says so under it, by its reason's code,
// whether or not the run offered Continue: the record holds no offers, and no row offers
// Continue; the conversation it opens does, from the run's end frame. It says what the provider
// refused to read as the reason says it: part of what the run found, or, where the refused call
// held nothing the run found, what the person asked.
test("a row whose run the model's provider refused says what it refused to read, and offers no Continue", async () => {
  const read = listConversations([
    {
      id: "c-8",
      newest_at: new Date(now).toISOString(),
      runs: [
        run("r-8", "read every alert and sum them up", now, {
          conversation: "c-8",
          state: "stopped",
          stop_reason:
            "MODEL_CONTENT_FILTER: the chat model's provider refused to read part of what this run found, also with the 4 results it could not vouch for withheld (jailbreak); Continue starts a follow-up with a new budget, or ask in other words",
        }),
      ],
    },
    {
      id: "c-9",
      newest_at: new Date(now - 60_000).toISOString(),
      runs: [
        run("r-9", "watch the comet", now - 60_000, {
          conversation: "c-9",
          state: "stopped",
          stop_reason: "STOPPED: its person stopped it",
        }),
      ],
    },
    {
      id: "c-10",
      newest_at: new Date(now - 120_000).toISOString(),
      runs: [
        run("r-10", "say something rude about the comet", now - 120_000, {
          conversation: "c-10",
          state: "stopped",
          stop_reason:
            "MODEL_CONTENT_FILTER: the chat model's provider refused to read this run's model call, which held nothing the run found, only the person's words, the page and the agent's rules (jailbreak); ask in other words",
        }),
      ],
    },
  ]);
  try {
    renderHistory();
    await waitFor(() =>
      expect(screen.getAllByTestId("going-history-conversation")).toHaveLength(3)
    );
    const [refused, stopped, wordsAlone] = screen.getAllByTestId("going-history-conversation");
    expect(within(refused as HTMLElement).getByTestId("going-history-state").textContent).toBe(
      "Stopped"
    );
    expect(within(refused as HTMLElement).getByTestId("going-history-refused").textContent).toBe(
      "The model's provider refused to read part of what this run found"
    );
    expect(within(stopped as HTMLElement).queryByTestId("going-history-refused")).toBeNull();
    expect(within(wordsAlone as HTMLElement).getByTestId("going-history-refused").textContent).toBe(
      "The model's provider refused to read what you asked"
    );
    expect(screen.queryByText(/Continue/u)).toBeNull();
    expect(screen.getByTestId("going-history").textContent ?? "").not.toMatch(/MODEL_/u);
  } finally {
    read.mockRestore();
  }
});

// A row whose newest run is withdrawn - it read a comment or an issue message deleted since, so
// Dispatch reads it stopped with ANSWER_WITHDRAWN - says so in one line under its title, which
// keeps the person's words, and names no code; a row of a run stopped otherwise says nothing of it.
test("a row whose run read a post deleted since says the answer is withdrawn, under its own words", async () => {
  const read = listConversations([
    {
      id: "c-11",
      newest_at: new Date(now).toISOString(),
      runs: [
        run("r-11", "what does the harbour comment say", now, {
          conversation: "c-11",
          state: "stopped",
          stop_reason:
            "ANSWER_WITHDRAWN: a comment or an issue message this run read was deleted, so nothing the run wrote is shown",
        }),
      ],
    },
    {
      id: "c-12",
      newest_at: new Date(now - 60_000).toISOString(),
      runs: [
        run("r-12", "watch the comet", now - 60_000, {
          conversation: "c-12",
          state: "stopped",
          stop_reason: "STOPPED: its person stopped it",
        }),
      ],
    },
  ]);
  try {
    renderHistory();
    await waitFor(() =>
      expect(screen.getAllByTestId("going-history-conversation")).toHaveLength(2)
    );
    const [withdrawn, stopped] = screen.getAllByTestId("going-history-conversation");
    expect(within(withdrawn as HTMLElement).getByTestId("going-history-title").textContent).toBe(
      "What does the harbour comment say"
    );
    expect(
      within(withdrawn as HTMLElement).getByTestId("going-history-withdrawn").textContent
    ).toBe("Dispatch withdrew this answer because a comment or message it read was deleted.");
    expect(within(stopped as HTMLElement).queryByTestId("going-history-withdrawn")).toBeNull();
    expect(screen.getByTestId("going-history").textContent ?? "").not.toMatch(/ANSWER_WITHDRAWN/u);
  } finally {
    read.mockRestore();
  }
});

// The architect's ruling of 15:53Z: a run only its own plan's deletes withdrew
// (ANSWER_WITHDRAWN_BY_RUN) says that this run deleted the post, and one another writer's delete
// withdrew says the post was deleted: one code is the start of the other, and neither row reads as
// the other's or names a code.
test("a row whose run its own delete withdrew says this run deleted the post, apart from another writer's delete", async () => {
  const read = listConversations([
    {
      id: "c-13",
      newest_at: new Date(now).toISOString(),
      runs: [
        run("r-13", "delete my harbour comment", now, {
          conversation: "c-13",
          state: "stopped",
          stop_reason:
            "ANSWER_WITHDRAWN_BY_RUN: every comment or issue message this run read that is gone, this run deleted",
        }),
      ],
    },
    {
      id: "c-14",
      newest_at: new Date(now - 60_000).toISOString(),
      runs: [
        run("r-14", "what does the harbour comment say", now - 60_000, {
          conversation: "c-14",
          state: "stopped",
          stop_reason:
            "ANSWER_WITHDRAWN: a comment or an issue message this run read was deleted, so nothing the run wrote is shown",
        }),
      ],
    },
  ]);
  try {
    renderHistory();
    await waitFor(() =>
      expect(screen.getAllByTestId("going-history-conversation")).toHaveLength(2)
    );
    const [byRun, byOther] = screen.getAllByTestId("going-history-conversation");
    expect(within(byRun as HTMLElement).getByTestId("going-history-withdrawn").textContent).toBe(
      "Dispatch withdrew this answer because this run deleted a comment or message it read."
    );
    expect(within(byOther as HTMLElement).getByTestId("going-history-withdrawn").textContent).toBe(
      "Dispatch withdrew this answer because a comment or message it read was deleted."
    );
    expect(screen.getByTestId("going-history").textContent ?? "").not.toMatch(/ANSWER_WITHDRAWN/u);
  } finally {
    read.mockRestore();
  }
});

test("Undo in History undoes that run and names a value another person changed, and a message sent, not undone", async () => {
  const user = userEvent.setup();
  const read = listConversations();
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementation(async () => ({
    lines: [
      {
        fields: [{ field: "parent", message: "bob set it to CORE-9 since", ok: false }],
        item: "CORE-3",
        outcome: { code: "", message: "", ok: true, status: 200 },
        plan: 1,
        row: 2,
        tool: "dispatch_issue_update",
        undo: "write_back",
      },
      {
        fields: [],
        item: "CORE-1",
        outcome: { code: "", message: "", ok: true, status: 200 },
        plan: 1,
        row: 1,
        tool: "dispatch_message",
        undo: "not_undone",
      },
    ],
  }));
  try {
    renderHistory();
    const [first] = await screen.findAllByTestId("going-undo-button");
    await user.click(first as HTMLElement);

    await waitFor(() => expect(undo).toHaveBeenCalledWith("r-4", undefined, keyedAttempt));
    const lines = await screen.findAllByTestId("going-undo-line");
    // Each line names its change and its field as the plan card named them, never by a tool.
    expect(lines.map((line) => line.textContent)).toEqual([
      "Update CORE-3: Parent kept: bob set it to CORE-9 since",
      "Send a message to CORE-1: Sent, not undone",
    ]);
  } finally {
    read.mockRestore();
  }
});

// An Undo line carries its tool and its item alone (class search, entries 33 and 53), and a label
// filled from those two dropped a setting's project and said a value its write never set: "Set
// the default repository of to none: Sent, not undone" for a row that set acme/infra. The Undo
// reads the run's plan, and names each line by the row it undid.
test("Undo in History names each line by its plan row, a setting by its project and the value it set", async () => {
  const user = userEvent.setup();
  const read = listConversations();
  const ran = (number: number, extra: Partial<GoingPlanRow>): GoingPlanRow => ({
    arguments: {},
    changed: false,
    claim: null,
    class: "field",
    fields: [],
    issue: "",
    item: "",
    needs: [],
    now: null,
    result: { code: "", delivery: null, message: "", status: 200 },
    row: number,
    state: "done",
    tool: "dispatch_issue_update",
    waits: [],
    ...extra,
  });
  const plan = spyOn(api, "goingPlan").mockResolvedValue({
    mode: "ask",
    plan: 1,
    rows: [
      ran(0, {
        arguments: { issue: "CORE-3", parent: "CORE-9" },
        fields: ["parent"],
        issue: "CORE-3",
        item: "CORE-3",
      }),
      ran(1, {
        arguments: { project: "CORE", repo: "acme/infra" },
        class: "setting",
        item: "CORE",
        tool: "dispatch_set_default_repository",
      }),
      ran(2, {
        arguments: { assignee: "bob@example.com", project: "CORE" },
        class: "setting",
        item: "CORE",
        tool: "dispatch_set_default_assignee",
      }),
    ],
    run: "r-4",
    state: "done",
    summary: "",
    version: "v1",
  });
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  const line = (row: number, tool: string, item: string, kind: "not_undone" | "write_back") => ({
    fields: [],
    item,
    outcome: { code: "", message: "", ok: true, status: 200 },
    plan: 1,
    row,
    tool,
    undo: kind,
  });
  undo.mockImplementation(async () => ({
    lines: [
      line(2, "dispatch_set_default_assignee", "CORE", "not_undone"),
      line(1, "dispatch_set_default_repository", "CORE", "not_undone"),
      line(0, "dispatch_issue_update", "CORE-3", "write_back"),
    ],
  }));
  try {
    renderHistory();
    const [first] = await screen.findAllByTestId("going-undo-button");
    await user.click(first as HTMLElement);
    await waitFor(() => expect(plan).toHaveBeenCalledWith("r-4"));
    await waitFor(() =>
      expect(screen.getAllByTestId("going-undo-line").map((drawn) => drawn.textContent)).toEqual([
        "Set the default assignee of CORE to bob@example.com: Sent, not undone",
        "Set the default repository of CORE to acme/infra: Sent, not undone",
        "Update CORE-3: Reverted",
      ])
    );
  } finally {
    read.mockRestore();
    plan.mockRestore();
  }
});

/** History over `client`, which the row holds, so it reads History again as Dispatch's list
 *  changes. */
function renderHistoryOver(client: QueryClient): void {
  renderWithQueries(<History onOpen={() => {}} />, {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
}

/** Run `id` of the split conversation, which moved CORE-3 and which Undo can take back. */
function moved(id: string): GoingRun {
  return run(id, "now move CORE-3 under the new one", now, {
    conversation: "c-3",
    issues: ["CORE-3"],
    undoable: true,
  });
}

// One run's Undo stays that run's: a History row names its conversation's newest run, and where
// a newer one comes while an Undo is out, the newer run's Undo is its own - not pressed, with no
// refusal - and the answer that lands is held for the run pressed.
test("an Undo out when its conversation lists a newer run keeps its press, its answer and its refusal that run's", async () => {
  const user = userEvent.setup();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const listedAs = (...runs: GoingRun[]): GoingConversation[] => [
    { id: "c-3", newest_at: new Date(now).toISOString(), runs },
  ];
  let rows = listedAs(moved("r-4"));
  const read = spyOn(api, "myGoingRuns").mockImplementation(async () => ({
    limit: 50,
    offset: 0,
    rows,
    total: rows.length,
  }));
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  const answered = Promise.withResolvers<GoingUndoAnswer>();
  undo.mockImplementationOnce(() => answered.promise);
  undo.mockImplementationOnce(async () => {
    throw new ApiError(409, { code: "GOING_UNDO_UNKNOWN", error: "nothing was undone" });
  });
  const relist = async (...runs: GoingRun[]) => {
    rows = listedAs(...runs);
    await act(() => client.invalidateQueries({ queryKey: goingConversationsQuery().queryKey }));
  };
  try {
    renderHistoryOver(client);
    await user.click(await screen.findByTestId("going-undo-button"));
    await waitFor(() => expect(undo).toHaveBeenCalledWith("r-4", undefined, keyedAttempt));
    expect(screen.getByTestId("going-undo-button")).toBeDisabled();

    await relist(moved("r-5"), moved("r-4"));
    await waitFor(() =>
      expect(screen.getByTestId("going-undo")).toHaveAttribute("data-run", "r-5")
    );
    expect(screen.getByTestId("going-undo-button")).toBeEnabled();
    answered.resolve({ lines: [] });
    await waitFor(() => expect(goingState().undone.has("r-4")).toBe(true));
    expect(goingState().undone.has("r-5")).toBe(false);
    expect(screen.getByTestId("going-undo-button")).toBeEnabled();

    await user.click(screen.getByTestId("going-undo-button"));
    expect(await screen.findByRole("alert")).toHaveTextContent("nothing was undone");
    await relist(moved("r-6"), moved("r-5"), moved("r-4"));
    await waitFor(() =>
      expect(screen.getByTestId("going-undo")).toHaveAttribute("data-run", "r-6")
    );
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    read.mockRestore();
  }
});

test("typing filters History by every question of a conversation, and the arrows and Enter open one", async () => {
  const user = userEvent.setup();
  const read = listConversations();
  // Each opened run's frames are read again from its stream; the stream answers nothing here.
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => new Promise<void>(() => {})
  );
  let opened = 0;
  try {
    renderHistory(() => {
      opened += 1;
    });
    await screen.findAllByTestId("going-history-conversation");
    // The box takes the focus as History opens.
    const filter = screen.getByRole("searchbox", { name: "Filter history" });
    expect(filter).toHaveFocus();

    // A later question of a conversation finds it too.
    await user.keyboard("move core-3");
    expect(titles()).toEqual(["Split CORE-1 in two"]);
    await user.clear(filter);
    await user.keyboard("comet");
    expect(titles()).toEqual([]);
    expect(screen.getByText("No conversation matches “comet”.")).toBeInTheDocument();
    await user.clear(filter);
    expect(titles()).toHaveLength(3);

    // While the focus is in the box, the first row is drawn selected, as Enter there opens it.
    const rows = screen.getAllByTestId("going-history-open");
    expect(rows.map((row) => row.getAttribute("data-selected"))).toEqual(["true", null, null]);
    // Down moves the focus through the rows, and Up from the first goes back to the box.
    await user.keyboard("{ArrowDown}");
    expect(rows[0]).toHaveFocus();
    expect(rows[0]).not.toHaveAttribute("data-selected");
    await user.keyboard("{ArrowDown}");
    expect(rows[1]).toHaveFocus();
    await user.keyboard("{ArrowUp}{ArrowUp}");
    expect(filter).toHaveFocus();
    expect(rows[0]).toHaveAttribute("data-selected", "true");

    // Typing on a row goes on in the box.
    await user.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}");
    expect(rows[2]).toHaveFocus();
    await user.keyboard("tel");
    expect(filter).toHaveFocus();
    expect(filter).toHaveValue("tel");
    expect(titles()).toEqual(["File the telescope's mirror as a new issue"]);

    // Enter in the box opens the first row; Enter on a row opens that row.
    await user.keyboard("{Enter}");
    expect(opened).toBe(1);
    expect(goingState().conversation).toEqual(["r-1"]);
    await user.clear(filter);
    await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    expect(opened).toBe(2);
    expect(goingState().conversation).toEqual(["r-2"]);
    expect(goingState().place).toBe("over");
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

test("a press on a row shows the conversation in the bar, its runs in the order they were asked", async () => {
  const user = userEvent.setup();
  const read = listConversations();
  // Each opened run's frames are read again from its stream; the stream answers nothing here.
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => new Promise<void>(() => {})
  );
  let opened = 0;
  try {
    renderHistory(() => {
      opened += 1;
    });
    const [first] = await screen.findAllByTestId("going-history-open");
    await user.click(first as HTMLElement);

    expect(opened).toBe(1);
    expect(goingState().conversation).toEqual(["r-3", "r-4"]);
    expect(goingState().place).toBe("over");
    expect(stream.mock.calls.map(([id]) => id)).toEqual(["r-3", "r-4"]);
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

test("a row says how its newest run stands as this page follows it", async () => {
  const working = run("r-9", "count the moons", now, { conversation: "c-9", state: "working" });
  const listed: GoingConversation = {
    id: "c-9",
    newest_at: working.created_at,
    runs: [working],
  };
  const read = listConversations([listed]);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(async (_id, onFrame) => {
    onFrame("reply", { labels: [], text: "Many.", tools: [] });
    onFrame("end", { offers: [], reason: "", state: "done" });
  });
  try {
    renderHistory();
    const state = await screen.findByTestId("going-history-state");
    expect(state).toHaveTextContent("Working");
    // The page reads the run's stream to its end: the list still says it works, and the row says
    // it is done.
    await act(async () => openConversation(listed));
    await waitFor(() =>
      expect(screen.getByTestId("going-history-state")).toHaveTextContent("Done")
    );
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

// A run's frames are read from its stream's first frame. Opening its conversation again, from
// History or from the Inbox, shows each step once: a run read to its end is not read again, and
// one read again starts its frames afresh.
test("opening a conversation again shows each step of its runs once", async () => {
  const user = userEvent.setup();
  const read = listConversations();
  // Each run's stream carries a reply that calls a tool, the call's own frame, and the end.
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(async (id, onFrame) => {
    onFrame("reply", { labels: [`Reading the notes of ${id}`], text: "", tools: ["call_tool"] });
    onFrame("tool", {
      kind: "read",
      label: `Read the notes of ${id}`,
      note: `read for ${id}`,
      step: 2,
      tool: "call_tool",
    });
    onFrame("end", { offers: [], reason: "", state: "done" });
  });
  try {
    renderWithQueries(
      <>
        <History onOpen={() => {}} />
        <Conversation />
      </>
    );
    const [first] = await screen.findAllByTestId("going-history-open");
    await user.click(first as HTMLElement);
    // Each run ended, so its steps fold into one line, which counts each step it holds: a step
    // shown twice would read "2 items".
    const folds = (run: string) =>
      screen
        .getAllByTestId("going-run")
        .filter((shown) => shown.getAttribute("data-run") === run)
        .flatMap((shown) => within(shown).queryAllByTestId("going-steps-fold"))
        .map((fold) => fold.textContent);
    await waitFor(() => expect(folds("r-3")).toEqual(["Worked for 1 s · read 1 item›"]));

    await user.click(first as HTMLElement);

    expect(folds("r-3")).toEqual(["Worked for 1 s · read 1 item›"]);
    expect(folds("r-4")).toEqual(["Worked for 1 s · read 1 item›"]);
    expect(stream.mock.calls.map(([id]) => id)).toEqual(["r-3", "r-4"]);
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

// A run its level's bound stopped offers Continue; a run its person or its grant stopped - the
// session ended, the token was revoked - offers none. History says "Stopped" of each, and holds
// which it was before the run's frames are read.
test("History names a stop a bound's only where its reason is one of the level's budgets", async () => {
  const stopped = (id: string, reason: string): GoingConversation => {
    const ran = run(id, `run ${id}`, now, {
      conversation: `c-${id}`,
      state: "stopped",
      stop_reason: reason,
    });
    return { id: `c-${id}`, newest_at: ran.created_at, runs: [ran] };
  };
  const read = listConversations([
    stopped("calls", "CALL_BUDGET: this run made the 6 model calls a Fast run may"),
    stopped("tokens", "OUTPUT_TOKEN_BUDGET: a model call wrote the tokens it may"),
    stopped("person", "STOPPED: its person stopped it"),
    stopped("session", "SESSION_ENDED: the session that asked ended"),
    stopped("token", "TOKEN_REVOKED: the grant's token was revoked"),
  ]);
  try {
    renderHistory();
    const rows = await screen.findAllByTestId("going-history-conversation");
    expect(
      rows.map((row) => [
        row.getAttribute("data-conversation"),
        row.getAttribute("data-state"),
        within(row).getByTestId("going-history-state").textContent,
      ])
    ).toEqual([
      ["c-calls", "stopped_bound", "Stopped"],
      ["c-tokens", "stopped_bound", "Stopped"],
      ["c-person", "stopped_person", "Stopped"],
      ["c-session", "stopped_person", "Stopped"],
      ["c-token", "stopped_person", "Stopped"],
    ]);
  } finally {
    read.mockRestore();
  }
});
