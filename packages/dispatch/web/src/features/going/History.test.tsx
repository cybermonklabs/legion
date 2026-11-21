import { afterEach, expect, spyOn, test } from "bun:test";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { api, type WriteBounds } from "../../api/client";
import type { GoingConversation, GoingRun, GoingUndoAnswer } from "../../api/types";
import { ConversationView } from "./ConversationView";
import { goingState, resetGoingStore } from "./going-store";
import { History } from "./History";
import { useOpenPlan } from "./usePlan";

// History as the person reads it (spec R6, R11): the conversations of the last 7 days, each with
// its runs, the newest first, as Dispatch lists them; the Undo of a run that changed something,
// for as long as its conversation is listed; and Open, which shows a conversation in the bar.

const now = Date.now();

function run(
  id: string,
  words: string,
  minutesAgo: number,
  extra: Partial<GoingRun> = {}
): GoingRun {
  const at = new Date(now - minutesAgo * 60_000).toISOString();
  return {
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: at,
    finished_at: at,
    id,
    issues: [],
    level: "standard",
    mode: "ask",
    state: "done",
    stop_reason: null,
    used: { calls: 2, tokens: 10, working_ms: 1000 },
    words,
    ...extra,
  };
}

/** Two conversations, the newest first: the split, whose second run changed two issues, and an
 *  older question whose plan still waits. */
const conversations: GoingConversation[] = [
  {
    id: "c-2",
    newest_at: new Date(now - 5 * 60_000).toISOString(),
    runs: [
      run("r-3", "now move CORE-3 under the new one", 5, {
        continues: "r-2",
        conversation: "c-2",
        issues: ["CORE-1", "CORE-3"],
      }),
      run("r-2", "split CORE-1 in two", 20, { conversation: "c-2" }),
    ],
  },
  {
    id: "c-1",
    newest_at: new Date(now - 90 * 60_000).toISOString(),
    runs: [run("r-1", "close the stale duplicates", 90, { finished_at: null, state: "waiting" })],
  },
];

function renderHistory(onOpen: () => void = () => {}): void {
  renderWithQueries(<History onOpen={onOpen} />);
}

/** The conversation as the bar draws it beside History. */
function Conversation(): ReactNode {
  return <ConversationView answering={useOpenPlan()} onContinue={() => {}} />;
}

afterEach(() => {
  cleanup();
  resetGoingStore();
});

test("History lists each conversation with its runs, the newest first, and offers Undo only on a run that changed something", async () => {
  const read = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: conversations,
    total: 2,
  });
  try {
    renderHistory();
    const listed = await screen.findAllByTestId("going-history-conversation");
    expect(listed.map((row) => row.getAttribute("data-conversation"))).toEqual(["c-2", "c-1"]);
    const first = listed[0] as HTMLElement;
    expect(
      within(first)
        .getAllByTestId("going-history-words")
        .map((words) => words.textContent)
    ).toEqual(["now move CORE-3 under the new one", "split CORE-1 in two"]);
    // The run that touched CORE-1 and CORE-3 has Undo; the one that changed nothing and the one
    // whose plan waits have none.
    const undos = screen.getAllByTestId("going-undo");
    expect(undos.map((undo) => undo.getAttribute("data-run"))).toEqual(["r-3"]);
  } finally {
    read.mockRestore();
  }
});

test("Undo in History undoes that run and names a value another person changed, and a message sent, not undone", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: conversations,
    total: 2,
  });
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
    await user.click(await screen.findByTestId("going-undo-button"));

    await waitFor(() => expect(undo).toHaveBeenCalledWith("r-3", undefined, keyedAttempt));
    const lines = await screen.findAllByTestId("going-undo-line");
    expect(lines.map((line) => line.textContent)).toEqual([
      "issue update CORE-3: parent kept, bob set it to CORE-9 since",
      "message CORE-1: sent, not undone",
    ]);
  } finally {
    read.mockRestore();
  }
});

test("Open shows the conversation in the bar, its runs in the order they were asked", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: conversations,
    total: 2,
  });
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
    expect(goingState().conversation).toEqual(["r-2", "r-3"]);
    expect(goingState().place).toBe("over");
    expect(stream.mock.calls.map(([id]) => id)).toEqual(["r-2", "r-3"]);
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
  const read = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: conversations,
    total: 2,
  });
  // Each run's stream carries a reply that calls a tool, the call's own frame, and the end.
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(async (id, onFrame) => {
    onFrame("reply", { text: `Reading for ${id}.`, tools: ["call_tool"] });
    onFrame("tool", { note: `read for ${id}`, step: 2, tool: "call_tool" });
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
    const steps = (run: string) =>
      screen
        .getAllByTestId("going-run")
        .filter((shown) => shown.getAttribute("data-run") === run)
        .flatMap((shown) => within(shown).queryAllByTestId("going-step"))
        .map((step) => step.textContent);
    await waitFor(() => expect(steps("r-2")).toEqual(["read for r-2"]));

    await user.click(first as HTMLElement);

    expect(steps("r-2")).toEqual(["read for r-2"]);
    expect(steps("r-3")).toEqual(["read for r-3"]);
    expect(stream.mock.calls.map(([id]) => id)).toEqual(["r-2", "r-3"]);
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

// A run its level's bound stopped offers Continue; a run its person or its grant stopped - the
// session ended, the token was revoked - offers none, and History says so before its frames.
test("History names a stop a bound's only where its reason is one of the level's budgets", async () => {
  const stopped = (id: string, reason: string) =>
    run(id, `run ${id}`, 10, { conversation: "c-3", state: "stopped", stop_reason: reason });
  const read = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [
      {
        id: "c-3",
        newest_at: new Date(now - 10 * 60_000).toISOString(),
        runs: [
          stopped("r-calls", "CALL_BUDGET: this run made the 6 model calls a Fast run may"),
          stopped("r-tokens", "OUTPUT_TOKEN_BUDGET: a model call wrote the tokens it may"),
          stopped("r-person", "STOPPED: its person stopped it"),
          stopped("r-session", "SESSION_ENDED: the session that asked ended"),
          stopped("r-token", "TOKEN_REVOKED: the grant's token was revoked"),
        ],
      },
    ],
    total: 1,
  });
  try {
    renderHistory();
    const runs = await screen.findAllByTestId("going-history-run");
    expect(
      runs.map((shown) => [
        shown.getAttribute("data-run"),
        within(shown).queryByText("stopped at a bound") === null ? "stopped" : "bound",
      ])
    ).toEqual([
      ["r-calls", "bound"],
      ["r-tokens", "bound"],
      ["r-person", "stopped"],
      ["r-session", "stopped"],
      ["r-token", "stopped"],
    ]);
  } finally {
    read.mockRestore();
  }
});
