import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the conversation's own scroll event below, which user-event has no form for
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { onFakeClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { api, type WriteBounds } from "../../api/client";
import type { GoingPlanAnswer, GoingPlanRow, GoingRun, GoingUndoAnswer } from "../../api/types";
import { dangerText, linkText, textMutedOnSurface } from "../../theme/classes";
import { clockWords } from "../search/blocks/block-cells";
import { ConversationView } from "./ConversationView";
import {
  goingState,
  openConversation,
  openRun,
  rereadEveryRun,
  resetGoingStore,
  startRun,
} from "./going-store";
import { useAsk } from "./useAsk";
import { useOpenPlan } from "./usePlan";

// The steps of a run as the bar draws them (design v2, state 3): one line for each call, in the
// present tense with "…" while it runs and in the past tense once it ended, or one sentence that
// says why it failed, whatever order its call, tool and reply frames came in, and no line left
// running once the run ended; no line for a call with no words, or for a change of an approved
// plan, which its row's tick shows; the model's prose beside its calls as their header, never as
// the answer; the run's timer on the newest line; and "Reconnecting…" while its stream is dropped.
// The answer (state 4): the steps folded above it into "Worked for…", each issue it names as a
// chip, the line that says results were withheld from the model, and under it "Done", "Waits for
// your answer", or, for a run of the line Do that ended done with no plan, "Nothing changed: no
// plan was proposed.", with Copy and Retry. Around them (states 2 and 3): the question as a
// bubble, "Thinking…" with its seconds until the first step or answer, from the moment of Enter,
// and the words sent while the run works, waiting at the foot with × to drop them.

function record(
  state: GoingRun["state"],
  workingMs = 1000,
  extra: Partial<GoingRun> = {}
): GoingRun {
  const at = new Date().toISOString();
  return {
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: at,
    finished_at: state === "working" ? null : at,
    id: "r-1",
    issues: [],
    level: "standard",
    line: null,
    mode: "ask",
    standing_count: 0,
    state,
    stop_reason: null,
    undoable: false,
    undoable_count: 0,
    undone_at: null,
    undone_count: 0,
    used: { calls: 1, tokens: 10, working_ms: workingMs },
    words: "where is DEMO-15",
    ...extra,
  };
}

/** The bar's conversation of one run, whose stream carries `frames` and stays open. With
 *  `dialogKeys`, it is drawn in a dialog as the bar draws it, and each key that reaches the
 *  dialog is put in `dialogKeys`. */
async function showRun(
  state: GoingRun["state"],
  frames: readonly (readonly [string, unknown])[],
  {
    dialogKeys,
    onContinue = () => {},
    onRetry = () => {},
    run = {},
    workingMs,
  }: {
    dialogKeys?: string[];
    onContinue?: (run: string, level?: GoingRun["level"]) => void;
    onRetry?: (words: string) => void;
    run?: Partial<GoingRun>;
    workingMs?: number;
  } = {}
): Promise<() => void> {
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    for (const [name, data] of frames) onFrame(name, data);
    return new Promise<void>(() => {});
  });
  openConversation({
    id: "c-1",
    newest_at: new Date().toISOString(),
    runs: [record(state, workingMs, run)],
  });
  const conversation = <Conversation onContinue={onContinue} onRetry={onRetry} />;
  renderWithQueries(
    dialogKeys === undefined ? (
      conversation
    ) : (
      <div
        aria-label="Search"
        className="relative"
        onKeyDown={(event) => dialogKeys.push(event.key)}
        role="dialog"
      >
        {conversation}
      </div>
    )
  );
  await screen.findByTestId("going-run");
  return () => stream.mockRestore();
}

function Conversation({
  onContinue = () => {},
  onRetry = () => {},
}: {
  readonly onContinue?: (run: string, level?: GoingRun["level"]) => void;
  readonly onRetry?: (words: string) => void;
}): ReactNode {
  return <ConversationView answering={useOpenPlan()} onContinue={onContinue} onRetry={onRetry} />;
}

test("a run that failed because the Dispatch has no answering model says so in one sentence, with no Failed and no code", async () => {
  const restore = await showRun("failed", [
    [
      "end",
      {
        offers: [],
        reason:
          "NO_MODEL: this server names no chat model (DISPATCH_ANSWERER), so its agent cannot work the run",
        state: "failed",
      },
    ],
  ]);
  try {
    expect((await screen.findByTestId("going-cannot-answer")).textContent).toBe(
      "Answers are off on this Dispatch: it has no answering model."
    );
    const run = screen.getByTestId("going-run");
    expect(run.textContent ?? "").not.toMatch(/Failed|NO_MODEL|DISPATCH_ANSWERER/u);
    expect(screen.queryByTestId("going-run-state")).toBeNull();
    expect(screen.queryByTestId("going-run-action")).toBeNull();
  } finally {
    restore();
  }
});

// A failure ends in the same grey card as a bound and a Stop (design v3, state 9): one sentence,
// then Try again as its one next step, a button as Continue is.
test("a failure reads one sentence in the end card and Try again, which runs the same words again", async () => {
  const user = userEvent.setup();
  const retried: string[] = [];
  const restore = await showRun(
    "failed",
    [["end", { offers: [], reason: "MODEL_FAILED: azure: 502 Bad Gateway", state: "failed" }]],
    { onRetry: (words) => retried.push(words) }
  );
  try {
    const card = await screen.findByTestId("going-run-end");
    const line = within(card).getByTestId("going-run-state");
    expect(line.textContent).toBe("Dispatch could not reach the model.");
    expect(card.textContent).not.toMatch(/MODEL_FAILED|502/u);
    expect(line.className).not.toContain(dangerText);
    await user.click(within(card).getByRole("button", { name: "Try again" }));
    expect(retried).toEqual(["where is DEMO-15"]);
  } finally {
    restore();
  }
});

// A Stop ends in the same card (design v3, state 9; review n4): what the run did before it, in
// words, and Continue, since the person stopped it on purpose. Its steps fold closed above the
// card, as an answered run's do: they are the work, never the result.
test("a Stop says what the run did and offers Continue, in the end card, its steps folded", async () => {
  const user = userEvent.setup();
  const continued: (readonly [string, GoingRun["level"] | undefined])[] = [];
  const restore = await showRun(
    "stopped",
    [
      ["tool", { items: ["DEMO-2"], kind: "read", label: "Read DEMO-2", step: 2, tool: "read" }],
      ["end", { offers: [], reason: "STOPPED: its person stopped it", state: "stopped" }],
    ],
    { onContinue: (run, level) => continued.push([run, level]) }
  );
  try {
    const card = await screen.findByTestId("going-run-end");
    expect(within(card).getByTestId("going-run-state").textContent).toBe("Stopped · read 1 item");
    expect(within(card).queryByRole("button", { name: "Try again" })).toBeNull();
    expect(within(card).queryByTestId("going-run-continue-deep")).toBeNull();
    expect(screen.getByTestId("going-steps-fold")).toHaveAttribute("aria-expanded", "false");
    expect(stepWords()).toEqual([]);
    await user.click(within(card).getByTestId("going-run-continue"));
    expect(continued).toEqual([["r-1", undefined]]);
  } finally {
    restore();
  }
});

// A bound stops a run: what the agent found so far - the answer it wrote - stands above the card,
// and its steps fold into one closed line under the question, as an answered run's do (design
// v3, rule 3: fold the work, never the result).
test("a bound shows the answer found so far above its card, its steps folded", async () => {
  const restore = await showRun("stopped", [
    ["tool", { items: ["DEMO-2"], kind: "read", label: "Read DEMO-2", step: 2, tool: "read" }],
    ["reply", { labels: [], text: "DEMO-2 waits on its mount.", tools: [] }],
    [
      "end",
      {
        offers: ["continue"],
        reason: "CALL_BUDGET: the run made its 6 model calls",
        state: "stopped",
      },
    ],
  ]);
  try {
    const bound = await screen.findByTestId("going-run-bound");
    await waitFor(() =>
      expect(screen.getByTestId("going-run-answer").textContent).toBe("DEMO-2 waits on its mount.")
    );
    const fold = screen.getByTestId("going-steps-fold");
    expect(fold).toHaveAttribute("aria-expanded", "false");
    expect(stepWords()).toEqual([]);
    const answer = screen.getByTestId("going-run-answer");
    expect(fold.compareDocumentPosition(answer)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(answer.compareDocumentPosition(bound)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  } finally {
    restore();
  }
});

/** The read of the runs below. */
const READ_DEMO_2: readonly [string, unknown] = [
  "tool",
  { items: ["DEMO-2"], kind: "read", label: "Read DEMO-2", step: 2, tool: "read" },
];

/** The end of a run its reply length limit stopped: the bound that cuts the reply of the run's
 *  latest model call at its output tokens (`OUTPUT_TOKEN_BUDGET`, `Level.cut`). */
const STOPPED_AT_REPLY_LENGTH: readonly [string, unknown] = [
  "end",
  {
    offers: ["continue"],
    reason:
      "OUTPUT_TOKEN_BUDGET: a model call of this run wrote the 4000 tokens a call of a fast run may; Continue starts a follow-up with a new budget",
    state: "stopped",
  },
];

// A run stopped at its reply length limit keeps the text its last model call wrote, which ends
// where the cut fell: that text ends with "…" where it is drawn, so it never reads as whole
// (review M3).
test("an answer the reply length limit cut ends with an ellipsis", async () => {
  const restore = await showRun("stopped", [
    READ_DEMO_2,
    ["reply", { labels: [], text: "DEMO-2 is ready. The next one waits on \n", tools: [] }],
    STOPPED_AT_REPLY_LENGTH,
  ]);
  try {
    await screen.findByTestId("going-run-bound");
    const answer = screen.getByTestId("going-run-answer");
    await waitFor(() => expect(answer.textContent).toBe("DEMO-2 is ready. The next one waits on…"));
    expect(answer).toHaveAttribute("data-cut", "true");
  } finally {
    restore();
  }
});

// The call the bound cut wrote no text: the answer an earlier call wrote is whole, and keeps no
// mark.
test("an answer an earlier call wrote keeps no ellipsis when the reply length limit cut a later call", async () => {
  const restore = await showRun("stopped", [
    READ_DEMO_2,
    ["reply", { labels: [], text: "DEMO-2 waits on its mount.", tools: [] }],
    ["reply", { labels: [], text: "", tools: [] }],
    STOPPED_AT_REPLY_LENGTH,
  ]);
  try {
    await screen.findByTestId("going-run-bound");
    const answer = screen.getByTestId("going-run-answer");
    await waitFor(() => expect(answer.textContent).toBe("DEMO-2 waits on its mount."));
    expect(answer).not.toHaveAttribute("data-cut");
  } finally {
    restore();
  }
});

// A bound stops a run with no answer: its steps fold into one line as an answered run's do, and
// that line and the card count what the run read from the same steps, so they never disagree.
test("a bound is a grey card with what the run did so far, Continue and Continue in Deep", async () => {
  const user = userEvent.setup();
  const read = (step: number): readonly [string, unknown] => [
    "tool",
    { items: [`DEMO-${step}`], kind: "read", label: `Read DEMO-${step}`, step, tool: "read" },
  ];
  const restore = await showRun("stopped", [
    ...[2, 3, 4, 5, 6].map(read),
    [
      "end",
      {
        offers: ["continue"],
        reason: "TIME_BUDGET: this run worked the 5m0s a standard run may",
        state: "stopped",
      },
    ],
  ]);
  try {
    const bound = await screen.findByTestId("going-run-bound");
    expect(within(bound).getByTestId("going-run-state").textContent).toBe(
      "Stopped at Standard's time limit · read 5 items so far"
    );
    // One line above the card, closed; a click opens the steps.
    const fold = screen.getByTestId("going-steps-fold");
    expect(fold).toHaveAccessibleName("Worked for 1 s · read 5 items");
    expect(fold).toHaveAttribute("aria-expanded", "false");
    expect(stepWords()).toEqual([]);
    await user.click(fold);
    expect(fold).toHaveAttribute("aria-expanded", "true");
    expect(screen.queryByTestId("going-steps-older")).toBeNull();
    expect(stepWords()).toEqual([
      "Read DEMO-2",
      "Read DEMO-3",
      "Read DEMO-4",
      "Read DEMO-5",
      "Read DEMO-6",
    ]);
    // Continue, with the key that does it, and Continue in Deep beside it at the same height.
    expect(within(bound).getByTestId("going-run-continue")).toHaveTextContent("Continue↵");
    expect(within(bound).getByTestId("going-run-continue-deep").className).toContain("py-[7px]");
  } finally {
    restore();
  }
});

// Continue starts one follow-up through the store (`continueRun`), at the stopped run's own level.
// While its start is out, the end card's buttons are off, and a second Continue starts nothing: a
// second follow-up would take the first start's place on the screen, and the run Dispatch answers
// the first with would join no conversation shown. Once Dispatch answers, that run joins this one.
test("Continue starts one follow-up: while it starts the end card's buttons are off, and the follow-up joins the conversation", async () => {
  const user = userEvent.setup();
  const { promise: created, resolve: create } = Promise.withResolvers<GoingRun>();
  const start = spyOn(api, "createGoingRun").mockReturnValue(created);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    if (id === "r-1") {
      onFrame("end", {
        offers: ["continue"],
        reason: "TIME_BUDGET: this run worked the 5m0s a standard run may",
        state: "stopped",
      });
    }
    return new Promise<void>(() => {});
  });
  openConversation({
    id: "c-1",
    newest_at: new Date().toISOString(),
    runs: [record("stopped")],
  });
  try {
    renderWithQueries(<AskedConversation />);
    const bound = await screen.findByTestId("going-run-bound");
    await user.click(within(bound).getByTestId("going-run-continue"));
    expect(screen.getByTestId("going-run-starting")).toBeInTheDocument();
    expect(within(bound).getByTestId("going-run-continue")).toBeDisabled();
    expect(within(bound).getByTestId("going-run-continue-deep")).toBeDisabled();
    await user.click(within(bound).getByTestId("going-run-continue-deep"));
    expect(start).toHaveBeenCalledTimes(1);
    expect(start.mock.calls[0]?.[0]).toMatchObject({
      continues: "r-1",
      level: "standard",
      words: "Continue",
    });

    await act(async () => {
      create(record("working", 1000, { continues: "r-1", id: "r-2", words: "Continue" }));
      await created;
    });
    await waitFor(() => expect(screen.getAllByTestId("going-run")).toHaveLength(2));
    expect(screen.queryByTestId("going-run-starting")).toBeNull();
    expect(goingState().conversation).toEqual(["r-1", "r-2"]);
    expect(start).toHaveBeenCalledTimes(1);
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

// A run the model's provider stopped, refusing part of what it found (`filterStopped`,
// `going/agent/run.go`): its end says so in words, never as a level's limit and never as its
// code, with Continue where the end frame offers it - and no Continue in Deep, which Deep's
// larger bounds do not help with - else "Ask in other words" and no button.
test("a stop the model's provider refused says so, with Continue where its end offers it and Ask in other words where not", async () => {
  const user = userEvent.setup();
  const continued: (readonly [string, GoingRun["level"] | undefined])[] = [];
  const reason =
    "MODEL_CONTENT_FILTER: the chat model's provider refused to read part of what this run found, also with the 2 results it could not vouch for withheld (jailbreak); Continue starts a follow-up with a new budget, or ask in other words";
  const read: readonly [string, unknown] = [
    "tool",
    { items: ["DEMO-2"], kind: "read", label: "Read DEMO-2", step: 2, tool: "read" },
  ];
  const restore = await showRun(
    "stopped",
    [read, ["end", { offers: ["continue"], reason, state: "stopped" }]],
    {
      onContinue: (run, level) => continued.push([run, level]),
      run: { stop_reason: reason },
    }
  );
  try {
    const card = await screen.findByTestId("going-run-bound");
    expect(within(card).getByTestId("going-run-state").textContent).toBe(
      "The model's provider refused to read part of what this run found. Continue, or ask in other words."
    );
    expect(screen.getByTestId("going-run").textContent ?? "").not.toMatch(/limit|MODEL_/u);
    expect(within(card).queryByTestId("going-run-continue-deep")).toBeNull();
    await user.click(within(card).getByTestId("going-run-continue"));
    expect(continued).toEqual([["r-1", undefined]]);
  } finally {
    restore();
  }
});

// A refused call that held nothing the run found - only the person's words, the page and the
// agent's rules - offers no Continue, and says it was what the person asked that the provider
// refused, never part of what the run found, which it had none of.
test("a refused stop that offers no Continue says what was refused and Ask in other words, as one read from its record alone does", async () => {
  const reason =
    "MODEL_CONTENT_FILTER: the chat model's provider refused to read this run's model call, which held nothing the run found, only the person's words, the page and the agent's rules (jailbreak); ask in other words";
  const restore = await showRun("stopped", [["end", { offers: [], reason, state: "stopped" }]], {
    run: { stop_reason: reason },
  });
  try {
    const line = await screen.findByTestId("going-run-state");
    expect(line.textContent).toBe(
      "The model's provider refused to read what you asked. Ask in other words."
    );
    expect(screen.queryByTestId("going-run-bound")).toBeNull();
    expect(screen.queryByTestId("going-run-continue")).toBeNull();
  } finally {
    restore();
    cleanup();
    resetGoingStore();
  }
  // The record holds no offers: a refused run read from its record, before any frame of it came,
  // draws the same, until its end frame says whether it offers Continue.
  const unread = await showRun("stopped", [], { run: { stop_reason: reason } });
  try {
    expect((await screen.findByTestId("going-run-state")).textContent).toBe(
      "The model's provider refused to read what you asked. Ask in other words."
    );
    expect(screen.queryByTestId("going-run-continue")).toBeNull();
  } finally {
    unread();
  }
});

// Design v2, state 5: a block is a card with a caption that says what it counts and when, and a
// table past 8 rows opens whole, over the whole bar that draws the conversation, with a way back.
test("a table's caption says what it counts and when, and Show all opens it over the whole bar until Esc or Back", async () => {
  const user = userEvent.setup();
  const finished = "2026-11-22T16:40:00Z";
  const dialogKeys: string[] = [];
  const restore = await showRun(
    "done",
    [
      [
        "block",
        {
          kind: "table",
          rows: Array.from({ length: 12 }, (_, index) => ({
            issues: index + 1,
            status: index % 2 === 0 ? "in_progress" : "todo",
          })),
          spec: { columns: ["status", "issues"], params: {}, source: "run", table: "chain" },
          title: "DEMO issues by status",
        },
      ],
      ["reply", { labels: [], text: "Here they are.", tools: [] }],
      ["end", { offers: [], reason: "", state: "done" }],
    ],
    { dialogKeys, run: { finished_at: finished } }
  );
  try {
    // The block was read before the run finished, and was read again here after it: the caption
    // says the time the run finished.
    expect((await screen.findByTestId("going-block-caption")).textContent).toBe(
      `DEMO issues by status · as of ${clockWords(Date.parse(finished))}`
    );
    const table = screen.getByTestId("going-block-table");
    expect(within(table).getAllByRole("row")).toHaveLength(9);
    expect(within(table).getAllByRole("cell")[0]).toHaveTextContent("In progress");
    await user.click(screen.getByTestId("going-block-show-all"));
    const whole = screen.getByTestId("going-block-whole");
    // It covers the whole bar: it is drawn in the bar's dialog itself, over the conversation and
    // the box, and not in the conversation's own scrolled area.
    expect(whole.parentElement).toBe(screen.getByRole("dialog", { name: "Search" }));
    expect(whole.className).toContain("absolute inset-0");
    expect(within(whole).getAllByRole("row")).toHaveLength(13);
    expect(within(whole).getByTestId("going-block-whole-close")).toHaveFocus();
    // Esc closes the table first, and the bar never hears it.
    await user.keyboard("{Escape}");
    expect(screen.queryByTestId("going-block-whole")).toBeNull();
    expect(dialogKeys).not.toContain("Escape");

    await user.click(screen.getByTestId("going-block-show-all"));
    await user.click(screen.getByTestId("going-block-whole-close"));
    expect(screen.queryByTestId("going-block-whole")).toBeNull();
  } finally {
    restore();
  }
});

// The table opened whole is one run's: another conversation opened under the same view - the
// side panel while the Inbox's Review opens one - takes it away, and it does not come back when
// its own conversation is opened again.
test("a table opened whole goes once another conversation opens, and does not come back with its own", async () => {
  const user = userEvent.setup();
  const restore = await showRun("done", [
    [
      "block",
      {
        kind: "table",
        rows: Array.from({ length: 12 }, (_, index) => ({ issues: index + 1, status: "todo" })),
        spec: { columns: ["status", "issues"], params: {}, source: "run", table: "chain" },
        title: "DEMO issues by status",
      },
    ],
    ["reply", { labels: [], text: "Here they are.", tools: [] }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    await user.click(await screen.findByTestId("going-block-show-all"));
    expect(screen.getByTestId("going-block-whole")).toBeInTheDocument();
    act(() =>
      openConversation({
        id: "c-2",
        newest_at: new Date().toISOString(),
        runs: [record("done", 1000, { conversation: "c-2", id: "r-2", words: "count the moons" })],
      })
    );
    expect(screen.queryByTestId("going-block-whole")).toBeNull();
    act(() => openRun("r-1"));
    expect(await screen.findByTestId("going-block-show-all")).toBeInTheDocument();
    expect(screen.queryByTestId("going-block-whole")).toBeNull();
  } finally {
    restore();
  }
});

// Design v2, state 8: the view follows new output until the person scrolls up; then "↓ New reply"
// shows at the next output and takes them back to the foot.
test("the conversation follows new output until the person scrolls up, and New reply brings it back", async () => {
  const user = userEvent.setup();
  let send: ((name: string, data: unknown) => void) | undefined;
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    send = onFrame;
    return new Promise<void>(() => {});
  });
  try {
    openConversation({ id: "c-1", newest_at: new Date().toISOString(), runs: [record("working")] });
    renderWithQueries(<Conversation />);
    const view = screen.getByTestId("going-conversation-view");
    Object.defineProperty(view, "scrollHeight", { configurable: true, value: 1000 });
    Object.defineProperty(view, "clientHeight", { configurable: true, value: 200 });
    act(() => send?.("call", { kind: "read", label: "Reading DEMO-15", step: 2, tool: "read" }));
    // At the foot it follows: the new line scrolled the view down, and no pill shows.
    expect(view.scrollTop).toBe(1000);
    expect(screen.queryByTestId("going-new-reply")).toBeNull();

    view.scrollTop = 100;
    fireEvent.scroll(view);
    act(() =>
      send?.("tool", {
        items: ["DEMO-15"],
        kind: "read",
        label: "Read DEMO-15",
        step: 2,
        tool: "read",
      })
    );
    expect(view.scrollTop).toBe(100);
    const pill = screen.getByTestId("going-new-reply");
    expect(pill).toHaveTextContent("↓ New reply");
    await user.click(pill);
    expect(view.scrollTop).toBe(1000);
    expect(screen.queryByTestId("going-new-reply")).toBeNull();
  } finally {
    stream.mockRestore();
  }
});

// How far the reader follows is of one conversation (class search, entry 71). The side panel keeps
// one view while the Inbox's Review opens another conversation in it, and that conversation
// opened at the first one's scroll offset, under "↓ New reply" for output it never had.
test("another conversation opened under the same view starts followed at its foot, with no New reply", async () => {
  let send: ((name: string, data: unknown) => void) | undefined;
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    if (id === "r-1") {
      send = onFrame;
    } else {
      onFrame("reply", { labels: [], text: "Nothing blocks the release.", tools: [] });
      onFrame("end", { offers: [], reason: "", state: "done" });
    }
    return new Promise<void>(() => {});
  });
  try {
    openConversation({ id: "c-1", newest_at: new Date().toISOString(), runs: [record("working")] });
    renderWithQueries(<Conversation />);
    const view = screen.getByTestId("going-conversation-view");
    Object.defineProperty(view, "scrollHeight", { configurable: true, value: 1000 });
    Object.defineProperty(view, "clientHeight", { configurable: true, value: 200 });
    act(() => send?.("call", { kind: "read", label: "Reading DEMO-15", step: 2, tool: "read" }));
    expect(view.scrollTop).toBe(1000);
    // The reader scrolls up, and the first conversation's next output leaves them behind.
    view.scrollTop = 100;
    fireEvent.scroll(view);
    act(() =>
      send?.("tool", {
        items: ["DEMO-15"],
        kind: "read",
        label: "Read DEMO-15",
        step: 2,
        tool: "read",
      })
    );
    expect(screen.getByTestId("going-new-reply")).toBeInTheDocument();

    act(() =>
      openConversation({
        id: "c-2",
        newest_at: new Date().toISOString(),
        runs: [
          record("done", 1000, {
            conversation: "c-2",
            id: "r-9",
            words: "what blocks the release",
          }),
        ],
      })
    );
    await waitFor(() => expect(screen.getByTestId("going-run")).toHaveAttribute("data-run", "r-9"));
    expect(view.scrollTop).toBe(1000);
    expect(screen.queryByTestId("going-new-reply")).toBeNull();
  } finally {
    stream.mockRestore();
  }
});

// Design v2, state 8: an old turn keeps its folded line, and its applied plan folds to one line,
// "Applied 2 changes · Undo 2 changes", which opens the card. The Undo is offered, and counts,
// as Dispatch's record of the run says.
test("an old turn's applied plan folds to one line with Undo, which opens the card", async () => {
  const user = userEvent.setup();
  const applied: GoingPlanAnswer = {
    mode: "ask",
    plan: 1,
    rows: [0, 1].map(
      (number): GoingPlanRow => ({
        arguments: { issue: `DEMO-${number + 1}`, priority: 1 },
        changed: false,
        claim: null,
        class: "field",
        fields: ["priority"],
        issue: `DEMO-${number + 1}`,
        item: `DEMO-${number + 1}`,
        needs: [],
        now: { priority: 3 },
        result: { code: "", delivery: null, message: "", status: 200 },
        row: number,
        state: "done",
        tool: "dispatch_issue_update",
        waits: [],
      })
    ),
    run: "r-1",
    state: "done",
    summary: "",
    version: "v1",
  };
  const read = spyOn(api, "goingPlan").mockImplementation(async () => applied);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    if (id === "r-1") {
      onFrame("plan", { plan: 1, state: "waiting" });
      onFrame("plan", { plan: 1, state: "done" });
      onFrame("end", { offers: [], reason: "", state: "done" });
    } else {
      onFrame("reply", { labels: [], text: "Both are P1 now.", tools: [] });
      onFrame("end", { offers: [], reason: "", state: "done" });
    }
    return new Promise<void>(() => {});
  });
  try {
    openConversation({
      id: "c-1",
      newest_at: new Date().toISOString(),
      runs: [
        record("done", 1000, { continues: "r-1", id: "r-2", words: "and now?" }),
        record("done", 1000, {
          undoable: true,
          undoable_count: 2,
          words: "raise DEMO-1 and DEMO-2",
        }),
      ],
    });
    renderWithQueries(<Conversation />);
    const [old] = await screen.findAllByTestId("going-run");
    const folded = await within(old as HTMLElement).findByTestId("going-plan");
    expect(folded).toHaveAttribute("data-folded", "true");
    expect(within(folded).getByTestId("going-plan-title")).toHaveTextContent("Applied 2 changes");
    expect(within(folded).getByTestId("going-undo-button")).toHaveTextContent(/^Undo 2 changes$/u);
    expect(within(folded).queryByTestId("going-plan-row")).toBeNull();
    await user.click(within(folded).getByTestId("going-plan-title"));
    const card = within(old as HTMLElement).getByTestId("going-plan");
    expect(within(card).getAllByTestId("going-plan-row")).toHaveLength(2);
    expect(
      within(card)
        .getAllByTestId("going-plan-row-mark")
        .map((mark) => mark.getAttribute("data-mark"))
    ).toEqual(["done", "done"]);
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

// Design v3, rule 3 and state 7: once the plan showed, the run's steps stay folded into one line
// while the rows of its approved plan run, which the card shows as its rows' ticks: no step line
// and no timer opens above the card as the run works again.
test("while an approved plan's rows run, the run's steps stay folded above the card, with no timer", async () => {
  let send: ((name: string, data: unknown) => void) | undefined;
  const change: GoingPlanRow = {
    arguments: { issue: "DEMO-10", priority: 2 },
    changed: false,
    claim: null,
    class: "field",
    fields: ["priority"],
    issue: "DEMO-10",
    item: "DEMO-10",
    needs: [],
    now: { priority: null },
    result: null,
    row: 0,
    state: "waiting",
    tool: "dispatch_issue_update",
    waits: [],
  };
  const planOf = (state: GoingPlanAnswer["state"], rowState: GoingPlanRow["state"]) => ({
    mode: "ask" as const,
    plan: 1,
    rows: [{ ...change, state: rowState }],
    run: "r-1",
    state,
    summary: "",
    version: "v1",
  });
  let answer: GoingPlanAnswer = planOf("waiting", "waiting");
  const read = spyOn(api, "goingPlan").mockImplementation(async () => answer);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    send = onFrame;
    onFrame("tool", {
      items: ["DEMO-10"],
      kind: "read",
      label: "Read DEMO-10",
      step: 1,
      tool: "read",
    });
    onFrame("plan", { plan: 1, state: "waiting" });
    return new Promise<void>(() => {});
  });
  try {
    openConversation({
      id: "c-1",
      newest_at: new Date().toISOString(),
      runs: [record("waiting", 4000, { words: "raise DEMO-10" })],
    });
    renderWithQueries(<Conversation />);
    const title = await screen.findByTestId("going-plan-title");
    expect(screen.getByTestId("going-steps-fold")).toHaveTextContent("read 1 item");

    // Approved, the run works again and its rows run: the card ticks them, and the steps stay
    // folded, with no line of their own and no timer.
    answer = planOf("running", "running");
    act(() => send?.("plan", { plan: 1, state: "running" }));
    act(() =>
      send?.("call", {
        kind: "write",
        label: "Changing DEMO-10…",
        step: 2,
        tool: "dispatch_issue_update",
      })
    );
    await waitFor(() => expect(title).toHaveTextContent("Applying 1 of 1…"));
    expect(screen.getByTestId("going-run")).toHaveAttribute("data-state", "working");
    expect(screen.getByTestId("going-steps-fold")).toHaveTextContent("read 1 item");
    expect(screen.queryAllByTestId("going-step")).toHaveLength(0);
    expect(screen.queryByTestId("going-step-timer")).toBeNull();

    // Its rows ran, and the run works on to write its answer: still folded.
    answer = planOf("done", "done");
    act(() => send?.("plan", { plan: 1, state: "done" }));
    await waitFor(() => expect(title).toHaveTextContent(/^Applied 1 change/u));
    expect(screen.getByTestId("going-run")).toHaveAttribute("data-state", "working");
    expect(screen.getByTestId("going-steps-fold")).toBeInTheDocument();
    expect(screen.queryAllByTestId("going-step")).toHaveLength(0);
  } finally {
    read.mockRestore();
    stream.mockRestore();
  }
});

// Each change row of a plan names its issue as a chip, and each issue an answer names is one: a
// chip reads its issue's summary, the one read every reference to the issue shares. A run's
// record is read again at each frame after which rows of its plan may have run (`rereadRun`);
// unless a row answers that read itself, Dispatch answers the record the page already holds.
let summaries: { mockRestore: () => void } | undefined;
let rereads: { mockRestore: () => void } | undefined;
beforeEach(() => {
  summaries = spyOn(api, "getIssueSummary").mockImplementation(async (key: string) => ({
    documents: [],
    key,
    project: "DEMO",
    status: "todo",
    title: `The issue ${key}`,
  }));
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
  rereads?.mockRestore();
});

/** The words of each step line the bar draws, without the run's timer. */
function stepWords(): readonly (string | null)[] {
  return screen
    .queryAllByTestId("going-step")
    .map((step) => within(step).getByTestId("going-step-label").textContent);
}

/** The step lines the bar draws, each with whether it runs. */
function stepLines(): readonly (readonly [string | null, string | null])[] {
  return screen
    .getAllByTestId("going-step")
    .map((step) => [
      within(step).getByTestId("going-step-label").textContent,
      step.getAttribute("data-running"),
    ]);
}

test("a running call shows its present words with …, an ended one its past words, and neither a title", async () => {
  const restore = await showRun("working", [
    [
      "reply",
      {
        labels: ["Reading DEMO-15", "Reading the messages of DEMO-7", ""],
        text: "Checking the issue and its messages.",
        tools: ["read", "call_tool", "made_up_tool"],
      },
    ],
    ["tool", { items: ["DEMO-15"], kind: "read", label: "Read DEMO-15", step: 2, tool: "read" }],
    // A row an approved plan's runner made, which the plan's card shows as its tick.
    [
      "tool",
      {
        items: ["DEMO-7"],
        kind: "write",
        label: "Commented on DEMO-7",
        step: 5,
        tool: "dispatch_comment",
      },
    ],
  ]);
  try {
    await waitFor(() =>
      expect(stepWords()).toEqual(["Read DEMO-15", "Reading the messages of DEMO-7…"])
    );
    const steps = screen.getAllByTestId("going-step");
    expect(steps.map((step) => step.getAttribute("data-running"))).toEqual(["false", "true"]);
    expect(steps.every((step) => !step.hasAttribute("title"))).toBe(true);
    expect(screen.getByTestId("going-step-words").textContent).toBe(
      "Checking the issue and its messages."
    );
    // The key the ended call read is a plain link, and the newest line carries the run's timer.
    expect(within(steps[0] as HTMLElement).getByRole("link", { name: "DEMO-15" })).toHaveAttribute(
      "href",
      "/issues/DEMO-15"
    );
    expect(within(steps[1] as HTMLElement).getByTestId("going-step-timer")).toBeInTheDocument();
    // The steps and the timer say the run works.
    expect(screen.queryByTestId("going-run-state")).toBeNull();
    expect(screen.queryByTestId("going-run-answer")).toBeNull();
  } finally {
    restore();
  }
});

test("a refused call reads its sentence in the danger colour", async () => {
  const restore = await showRun("working", [
    ["reply", { labels: ["Reading DEMO-12"], text: "", tools: ["read"] }],
    [
      "tool",
      {
        error: true,
        items: ["DEMO-12"],
        kind: "read",
        label: "Could not read DEMO-12: you have no access",
        step: 2,
        tool: "read",
      },
    ],
  ]);
  try {
    await waitFor(() =>
      expect(stepWords()).toEqual(["Could not read DEMO-12: you have no access"])
    );
    expect(screen.getByTestId("going-step").className).toContain(dangerText);
  } finally {
    restore();
  }
});

test("a run recorded before calls had words shows no step line", async () => {
  const restore = await showRun("done", [
    ["reply", { text: "", tools: ["call_tool"] }],
    ["tool", { note: "call_tool dispatch_read: CORE-1", step: 2, tool: "call_tool" }],
    ["reply", { text: "CORE-1 is done.", tools: [] }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    await screen.findByTestId("going-run-answer");
    expect(screen.queryByTestId("going-steps")).toBeNull();
    expect(screen.queryByTestId("going-steps-fold")).toBeNull();
    expect(screen.queryByText(/call_tool/)).toBeNull();
  } finally {
    restore();
  }
});

test("the answer is the reply that called no tool, and withheld results read as one line under it", async () => {
  const restore = await showRun("done", [
    [
      "reply",
      { labels: ["Reading DEMO-1"], text: "Checking the children of DEMO-1.", tools: ["read"] },
    ],
    ["tool", { items: ["DEMO-1"], kind: "read", label: "Read DEMO-1", step: 2, tool: "read" }],
    ["reply", { labels: [], text: "DEMO-1 has two children.", tools: [], withheld: 2 }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    // The answer's markdown renders after its frame arrives.
    await waitFor(() =>
      expect(screen.getByTestId("going-run-answer").textContent).toBe("DEMO-1 has two children.")
    );
    expect(screen.queryByTestId("going-step-words")).toBeNull();
    expect(screen.getByTestId("going-run-withheld").textContent).toBe(
      "The model's content filter held back 2 results, so this answer may miss them."
    );
  } finally {
    restore();
  }
});

test("one withheld result reads in the singular", async () => {
  const restore = await showRun("done", [
    ["reply", { labels: [], text: "No session is named the architect.", tools: [], withheld: 1 }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    expect((await screen.findByTestId("going-run-withheld")).textContent).toBe(
      "The model's content filter held back one result, so this answer may miss it."
    );
  } finally {
    restore();
  }
});

test("a call frame draws its call running before the call ends, and its tool frame ends that line", async () => {
  const restore = await showRun("working", [
    [
      "call",
      { kind: "read", label: "Reading DEMO-15", step: 2, text: "Checking DEMO-15.", tool: "read" },
    ],
    ["call", { kind: "read", label: "Reading the messages of DEMO-7", step: 3, tool: "call_tool" }],
    ["tool", { items: ["DEMO-15"], kind: "read", label: "Read DEMO-15", step: 2, tool: "read" }],
  ]);
  try {
    await waitFor(() =>
      expect(stepLines()).toEqual([
        ["Read DEMO-15", "false"],
        ["Reading the messages of DEMO-7…", "true"],
      ])
    );
    expect(screen.getByTestId("going-step-words").textContent).toBe("Checking DEMO-15.");
  } finally {
    restore();
  }
});

test("a tool frame after its call frame, or before its reply frame, draws one line per call", async () => {
  const user = userEvent.setup();
  const restore = await showRun("done", [
    // The first call, as a run sends it now: its call frame, its tool frame, then its reply.
    ["call", { kind: "read", label: "Reading DEMO-15", step: 2, text: "", tool: "read" }],
    ["tool", { items: ["DEMO-15"], kind: "read", label: "Read DEMO-15", step: 2, tool: "read" }],
    ["reply", { labels: ["Reading DEMO-15"], step: 1, text: "", tools: ["read"] }],
    // The second, as a run recorded before call frames: its tool frame, then its reply.
    [
      "tool",
      {
        items: ["DEMO-7"],
        kind: "read",
        label: "Read the comments of DEMO-7",
        step: 4,
        tool: "read",
      },
    ],
    ["reply", { labels: ["Reading the comments of DEMO-7"], step: 3, text: "", tools: ["read"] }],
    ["reply", { labels: [], step: 5, text: "DEMO-15 waits on DEMO-7.", tools: [] }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    await waitFor(() =>
      expect(screen.getByTestId("going-run-answer").textContent).toBe("DEMO-15 waits on DEMO-7.")
    );
    // The answer came, so the steps fold above it; the fold opens them.
    await user.click(screen.getByTestId("going-steps-fold"));
    expect(stepLines()).toEqual([
      ["Read DEMO-15", "false"],
      ["Read the comments of DEMO-7", "false"],
    ]);
  } finally {
    restore();
  }
});

test("no running line stays once the run ended: a call its end cut draws no line", async () => {
  const user = userEvent.setup();
  const restore = await showRun("stopped", [
    ["call", { kind: "read", label: "Reading DEMO-15", step: 2, text: "Checking.", tool: "read" }],
    ["tool", { items: ["DEMO-15"], kind: "read", label: "Read DEMO-15", step: 2, tool: "read" }],
    ["call", { kind: "read", label: "Reading the messages of DEMO-7", step: 3, tool: "call_tool" }],
    ["end", { offers: [], reason: "STOPPED: its person stopped it", state: "stopped" }],
  ]);
  try {
    await waitFor(() =>
      expect(screen.getByTestId("going-run-state").textContent).toContain("Stopped")
    );
    // The stopped run's steps fold into one closed line, which counts only the call that ended,
    // and opens on a click.
    const fold = screen.getByTestId("going-steps-fold");
    expect(fold).toHaveAccessibleName("Worked for 1 s · read 1 item");
    expect(fold).toHaveAttribute("aria-expanded", "false");
    await user.click(fold);
    expect(stepLines()).toEqual([["Read DEMO-15", "false"]]);
    // The prose stays as the header of the call it heads, in the past tense now that the call
    // ended; the stopped run has no timer.
    expect(screen.getByTestId("going-step-words").textContent).toBe("Checked.");
    expect(screen.queryByTestId("going-step-timer")).toBeNull();
  } finally {
    restore();
  }
});

test("a run that has sent nothing yet shows its question and Thinking with its seconds, until its first step", async () => {
  await onFakeClock(async () => {
    let send: ((name: string, data: unknown) => void) | undefined;
    const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
      send = onFrame;
      return new Promise<void>(() => {});
    });
    try {
      openConversation({
        id: "c-1",
        newest_at: new Date().toISOString(),
        runs: [record("working")],
      });
      renderWithQueries(<Conversation />);
      expect(screen.getByTestId("going-run-words")).toHaveTextContent("where is DEMO-15");
      expect(screen.getByTestId("going-thinking")).toHaveTextContent("Thinking… 0 s");
      // "Working…" would say the same thing twice.
      expect(screen.queryByTestId("going-run-state")).toBeNull();
      await tick(1000);
      expect(screen.getByTestId("going-thinking")).toHaveTextContent("Thinking… 1 s");
      await tick(1000);
      expect(screen.getByTestId("going-thinking")).toHaveTextContent("Thinking… 2 s");

      act(() => send?.("call", { kind: "read", label: "Reading DEMO-15", step: 2, tool: "read" }));
      expect(screen.queryByTestId("going-thinking")).toBeNull();
      expect(stepLines()).toEqual([["Reading DEMO-15…", "true"]]);
      // The step's line carries the run's timer on from the question; it says the run works.
      expect(screen.getByTestId("going-step-timer")).toHaveTextContent("2 s");
      expect(screen.queryByTestId("going-run-state")).toBeNull();
    } finally {
      stream.mockRestore();
    }
  });
});

test("the question shows with Thinking from the moment of Enter, before Dispatch answers the start", async () => {
  const { promise: created, resolve: create } = Promise.withResolvers<GoingRun>();
  const start = spyOn(api, "createGoingRun").mockReturnValue(created);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => new Promise<void>(() => {})
  );
  try {
    renderWithQueries(<Conversation />);
    let started: Promise<boolean> | undefined;
    act(() => {
      started = startRun("where is DEMO-15", { path: "/" }, "alice");
    });
    const asked = screen.getByTestId("going-run-starting");
    expect(within(asked).getByTestId("going-run-words")).toHaveTextContent("where is DEMO-15");
    expect(within(asked).getByTestId("going-thinking")).toHaveTextContent("Thinking… 0 s");

    await act(async () => {
      create(record("working"));
      await started;
    });
    expect(screen.queryByTestId("going-run-starting")).toBeNull();
    const run = screen.getByTestId("going-run");
    expect(within(run).getByTestId("going-run-words")).toHaveTextContent("where is DEMO-15");
    expect(within(run).getByTestId("going-thinking")).toBeInTheDocument();
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

test("words sent while the run works wait at the foot, sending when it ends, and × drops them", async () => {
  const user = userEvent.setup();
  const start = spyOn(api, "createGoingRun");
  const restore = await showRun("working", []);
  try {
    await act(() => startRun("and its comments", { path: "/" }, "alice"));
    expect(start).not.toHaveBeenCalled();
    const queued = screen.getByTestId("going-queued");
    expect(within(queued).getByTestId("going-queued-words")).toHaveTextContent("and its comments");
    expect(queued).toHaveTextContent("Sends when this run ends");

    await user.click(within(queued).getByRole("button", { name: "Drop these words" }));
    expect(screen.queryByTestId("going-queued")).toBeNull();
    expect(goingState().queued).toBeUndefined();
  } finally {
    start.mockRestore();
    restore();
  }
});

/** A search of DEMO that answered three hits, then a read of DEMO-1, then the answer. */
const SEARCHED_AND_READ: readonly (readonly [string, unknown])[] = [
  ["call", { kind: "search", label: "Searching DEMO", step: 2, text: "", tool: "search" }],
  [
    "tool",
    {
      count: 3,
      items: ["DEMO-1", "DEMO-2", "DEMO-3"],
      kind: "search",
      label: "Searched DEMO · 3 hits",
      step: 2,
      tool: "search",
    },
  ],
  ["call", { kind: "read", label: "Reading DEMO-1", step: 4, text: "", tool: "read" }],
  ["tool", { items: ["DEMO-1"], kind: "read", label: "Read DEMO-1", step: 4, tool: "read" }],
  ["reply", { labels: [], step: 5, text: "DEMO-1 is the oldest.", tools: [] }],
  ["end", { offers: [], reason: "", state: "done" }],
];

test("once the answer came, the steps fold into one line above it, Worked for and what they did, which a click opens", async () => {
  const user = userEvent.setup();
  const restore = await showRun("done", SEARCHED_AND_READ, { workingMs: 108_000 });
  try {
    const fold = await screen.findByTestId("going-steps-fold");
    expect(fold).toHaveAccessibleName("Worked for 1 min 48 s · read 1 item, searched once");
    expect(fold.compareDocumentPosition(screen.getByTestId("going-run-answer"))).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING
    );
    expect(stepWords()).toEqual([]);
    await user.click(fold);
    expect(stepWords()).toEqual(["Searched DEMO · 3 hits", "Read DEMO-1"]);
  } finally {
    restore();
  }
});

test("Worked for counts from the question to the frame that ended the run, for a run started here", async () => {
  await onFakeClock(async () => {
    let send: ((name: string, data: unknown) => void) | undefined;
    const start = spyOn(api, "createGoingRun").mockResolvedValue(record("working"));
    const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
      send = onFrame;
      return new Promise<void>(() => {});
    });
    try {
      renderWithQueries(<Conversation />);
      await act(() => startRun("where is DEMO-15", { path: "/" }, "alice"));
      await tick(3000);
      act(() => {
        for (const [name, data] of SEARCHED_AND_READ) send?.(name, data);
      });
      expect(screen.getByTestId("going-steps-fold")).toHaveAccessibleName(
        "Worked for 3 s · read 1 item, searched once"
      );
    } finally {
      start.mockRestore();
      stream.mockRestore();
    }
  });
});

test("under the answer, Done in grey, with Copy of the answer's text and Retry of the same words", async () => {
  const user = userEvent.setup();
  const retried: string[] = [];
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  const clipboard = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const restore = await showRun("done", SEARCHED_AND_READ, {
    onRetry: (words) => retried.push(words),
  });
  try {
    const foot = await screen.findByTestId("going-run-foot");
    const state = within(foot).getByTestId("going-run-state");
    expect(state.textContent).toBe("Done");
    // The bar's one grey for hints and status, never the colour of a link.
    expect(state.className).toContain(textMutedOnSurface);
    expect(state.className).not.toContain(linkText);
    await user.click(within(foot).getByRole("button", { name: "Copy the answer" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("DEMO-1 is the oldest."));
    await user.click(within(foot).getByRole("button", { name: "Retry" }));
    expect(retried).toEqual(["where is DEMO-15"]);
  } finally {
    restore();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
  }
});

/** The conversation as the bar draws it, with the bar's own box behind its Retry and Continue. */
function AskedConversation(): ReactNode {
  const asking = useAsk({ path: "/projects/DEMO/issues" });
  return (
    <ConversationView
      answering={asking.answering}
      onContinue={asking.onContinue}
      onRetry={asking.retry}
    />
  );
}

/** `AskedConversation` with a box under it whose Enter is the bar's: `asking.enter` of what it holds. */
function BoxedConversation(): ReactNode {
  const asking = useAsk({ path: "/projects/DEMO/issues" });
  return (
    <>
      <ConversationView
        answering={asking.answering}
        onContinue={asking.onContinue}
        onRetry={asking.retry}
      />
      <input
        aria-label="Ask the command bar"
        onKeyDown={(event) => {
          if (event.key === "Enter") asking.enter(event.currentTarget.value);
        }}
        type="text"
      />
    </>
  );
}

// Try again shows the key that does it, as Continue and Approve do (design v3, rule 7; review a9):
// Enter in the empty box runs the failed run's words again, and the button says so. A failure that
// asks for a new conversation has no such key.
test("Try again shows ↵, and Enter in the empty box runs the failed run's words again", async () => {
  const user = userEvent.setup();
  const start = spyOn(api, "createGoingRun").mockResolvedValue(
    record("working", 1000, { id: "r-2", words: "where is DEMO-15" })
  );
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    onFrame("end", { offers: [], reason: "MODEL_FAILED: azure: 502 Bad Gateway", state: "failed" });
    return new Promise<void>(() => {});
  });
  openConversation({
    id: "c-1",
    newest_at: new Date().toISOString(),
    runs: [record("failed")],
  });
  try {
    renderWithQueries(<BoxedConversation />);
    const button = await screen.findByRole("button", { name: "Try again" });
    expect(button).toHaveTextContent("Try again↵");
    await user.click(screen.getByRole("textbox", { name: "Ask the command bar" }));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(start).toHaveBeenCalledTimes(1));
    expect(start.mock.calls[0]?.[0]).toMatchObject({ continues: "r-1", words: "where is DEMO-15" });
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

// Retry under an older answer takes the words as Enter in the box does: while the newest run's
// plan waits, they say what to change of it. It starts no conversation of its own, which would
// hide the one open, the waiting plan's card with it, and drop the person's answer to it so far.
test("Retry under an older answer while the newest run's plan waits revises that plan, and starts no run", async () => {
  const user = userEvent.setup();
  const waiting: GoingPlanAnswer = {
    ...appliedPlan,
    rows: (appliedPlan.rows ?? []).map((row) => ({ ...row, result: null, state: "waiting" })),
    run: "r-2",
    state: "waiting",
  };
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    if (id === "r-1") {
      onFrame("reply", { labels: [], text: "DEMO-15 is in the release.", tools: [] });
      onFrame("end", { offers: [], reason: "", state: "done" });
    } else {
      onFrame("plan", { plan: 1, state: "waiting" });
      onFrame("end", { offers: [], reason: "", state: "waiting" });
    }
    return new Promise<void>(() => {});
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async () => waiting);
  const start = spyOn(api, "createGoingRun");
  const revise = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/revise");
  revise.mockImplementation(async () => ({ ...waiting, plan: 2 }));
  openConversation({
    id: "c-1",
    newest_at: new Date().toISOString(),
    runs: [
      record("waiting", 1000, { continues: "r-1", id: "r-2", words: "raise DEMO-10" }),
      record("done"),
    ],
  });
  try {
    renderWithQueries(<AskedConversation />);
    await screen.findByTestId("going-plan-approve");
    const [older] = screen.getAllByTestId("going-run");
    await user.click(within(older as HTMLElement).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(revise).toHaveBeenCalledWith(
        "r-2",
        { plan: 1, words: "where is DEMO-15" },
        keyedAttempt
      )
    );
    expect(start).not.toHaveBeenCalled();
    expect(goingState().starting).toBeUndefined();
    expect(screen.getAllByTestId("going-run")).toHaveLength(2);
  } finally {
    stream.mockRestore();
    read.mockRestore();
    start.mockRestore();
  }
});

/** Lays the conversation out as a browser does: the view stands 100 px down the window and the
 *  turn of `run` 700 px down the conversation, moving up as it scrolls; every other box stands at
 *  the window's top. Answers the spy on `getBoundingClientRect`, which the test restores. */
function layOutConversation(run: string): { readonly mockRestore: () => void } {
  const box = (top: number): DOMRect => ({
    bottom: top + 100,
    height: 100,
    left: 0,
    right: 700,
    toJSON: () => ({}),
    top,
    width: 700,
    x: 0,
    y: top,
  });
  return spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    if (this.dataset.testid === "going-conversation-view") return box(100);
    const scrolled =
      document.querySelector<HTMLElement>('[data-testid="going-conversation-view"]')?.scrollTop ??
      0;
    return box(this.dataset.run === run ? 700 - scrolled : 0);
  });
}

// Design v3, state 6: while the newest run's plan waits, the conversation follows no further than
// that run's question, so its card reads from the top, with Approve and Discard pinned at the foot
// of the view - in the bar and the panel alike - rather than from its middle, the line its steps
// fold into cut in half under the header (06-plan-panel). Nothing above the question shows then,
// so nothing fades out under the header.
test("while the newest run's plan waits, the conversation follows no further than its question, and the card's actions are pinned", async () => {
  const waiting: GoingPlanAnswer = {
    ...appliedPlan,
    rows: (appliedPlan.rows ?? []).map((row) => ({ ...row, result: null, state: "waiting" })),
    run: "r-2",
    state: "waiting",
  };
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    if (id === "r-1") {
      onFrame("reply", { labels: [], text: "DEMO-15 is in the release.", tools: [] });
      onFrame("end", { offers: [], reason: "", state: "done" });
    } else {
      onFrame("plan", { plan: 1, state: "waiting" });
      onFrame("end", { offers: [], reason: "", state: "waiting" });
    }
    return new Promise<void>(() => {});
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async () => waiting);
  const boxes = layOutConversation("r-2");
  openConversation({
    id: "c-1",
    newest_at: new Date().toISOString(),
    runs: [
      record("waiting", 1000, { continues: "r-1", id: "r-2", words: "raise DEMO-10" }),
      record("done"),
    ],
  });
  try {
    renderWithQueries(<AskedConversation />);
    const view = screen.getByTestId("going-conversation-view");
    Object.defineProperty(view, "scrollHeight", { configurable: true, value: 2000 });
    Object.defineProperty(view, "clientHeight", { configurable: true, value: 400 });
    const actions = await screen.findByTestId("going-plan-actions");
    expect(view.scrollTop).toBe(600);
    expect(view).not.toHaveAttribute("data-cut");
    expect(actions.className.split(" ")).toEqual(
      expect.arrayContaining(["sticky", "bottom-0", "border-t"])
    );
    // The scroll keeps no padding of its own at its foot, where a browser pins the bar to the
    // padding's edge and the next row showed under it (review R1): what the scroll holds keeps
    // that space, and the bar is its card's last part.
    expect(view.className.split(" ")).toEqual(expect.arrayContaining(["px-3", "pt-3"]));
    expect(view.className).not.toMatch(/(?:^|\s)(?:p|py|pb)-\d/u);
    expect(screen.getByTestId("going-conversation-turns")).toHaveClass("pb-3");
    expect(view.firstElementChild).toBe(screen.getByTestId("going-conversation-turns"));
    expect(screen.getByTestId("going-plan").lastElementChild).toBe(actions);
    expect(within(actions).getByTestId("going-plan-approve")).toBeInTheDocument();
    expect(within(actions).getByTestId("going-plan-reject")).toBeInTheDocument();
  } finally {
    boxes.mockRestore();
    stream.mockRestore();
    read.mockRestore();
  }
});

// A plan of a withdrawn run waits for nobody (`planWaits`): Dispatch approves and revises it for
// nobody. Its read can come before the run's record says the run is withdrawn, on the delete that
// withdrew it; the conversation's scroll then stops at no question for it, and follows on to its
// foot as it does with no plan waiting, with no Approve pinned there.
test("a plan read withdrawn holds the conversation's scroll at no question: it follows on to the foot", async () => {
  const withdrawn: GoingPlanAnswer = {
    ...appliedPlan,
    rows: (appliedPlan.rows ?? []).map((row) => ({ ...row, result: null, state: "waiting" })),
    state: "waiting",
    withdrawn: true,
  };
  let send: ((name: string, data: unknown) => void) | undefined;
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    send = onFrame;
    return new Promise<void>(() => {});
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async () => withdrawn);
  const boxes = layOutConversation("r-1");
  openConversation({
    id: "c-1",
    newest_at: new Date().toISOString(),
    runs: [record("waiting", 1000, { words: "raise DEMO-10" })],
  });
  try {
    renderWithQueries(<AskedConversation />);
    const view = screen.getByTestId("going-conversation-view");
    Object.defineProperty(view, "scrollHeight", { configurable: true, value: 2000 });
    Object.defineProperty(view, "clientHeight", { configurable: true, value: 400 });
    // The plan frame has the plan read; the run's end comes once the read has landed, and the
    // conversation follows it to its foot.
    act(() => send?.("plan", { plan: 1, state: "waiting" }));
    await screen.findByTestId("going-plan");
    expect(read).toHaveBeenCalledWith("r-1");
    expect(screen.queryByTestId("going-plan-approve")).toBeNull();
    expect(view.scrollTop).toBe(2000);
    act(() => send?.("end", { offers: [], reason: "", state: "waiting" }));
    expect(view.scrollTop).toBe(2000);
  } finally {
    boxes.mockRestore();
    stream.mockRestore();
    read.mockRestore();
  }
});

// Copy gives the words a reader sees: a time the agent wrote as a link to its instant is copied
// as the words it shows, never as its address.
test("Copy of an answer gives each time in it as the words it shows, never its link", async () => {
  const user = userEvent.setup();
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  const clipboard = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const restore = await showRun("done", [
    [
      "reply",
      {
        labels: [],
        text: "Bob wrote [5 days ago](dispatch-time:2026-11-18T11:35:00Z) that it is stuck.",
        tools: [],
      },
    ],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    const foot = await screen.findByTestId("going-run-foot");
    await user.click(within(foot).getByRole("button", { name: "Copy the answer" }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith("Bob wrote 5 days ago that it is stuck.")
    );
  } finally {
    restore();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
  }
});

/** A plan of one change of DEMO-10's priority, approved and run. */
const appliedPlan: GoingPlanAnswer = {
  mode: "ask",
  plan: 1,
  rows: [
    {
      arguments: { issue: "DEMO-10", priority: 2 },
      changed: false,
      claim: null,
      class: "field",
      fields: ["priority"],
      issue: "DEMO-10",
      item: "DEMO-10",
      needs: [],
      now: { priority: null },
      result: { code: "", delivery: null, message: "", status: 200 },
      row: 0,
      state: "done",
      tool: "dispatch_issue_update",
      waits: [],
    },
  ],
  run: "r-1",
  state: "done",
  summary: "",
  version: "v1",
};

/** The answer of the run that made `appliedPlan`, once its change ran. */
const appliedAnswer = "DEMO-10 is P2 now.";

/** The frames of that run: a read, the plan proposed and run, then the answer. */
const appliedFrames: readonly (readonly [string, unknown])[] = [
  ["tool", { items: ["DEMO-10"], kind: "read", label: "Read DEMO-10", step: 2, tool: "read" }],
  ["plan", { plan: 1, state: "waiting" }],
  ["plan", { plan: 1, state: "done" }],
  ["reply", { labels: [], text: appliedAnswer, tools: [] }],
  ["end", { offers: [], reason: "", state: "done" }],
];

// Design v3, state 7: an applied plan draws its steps' fold line, its answer, then its card, in
// that order, and its fold line says what the run did last, "applied 1 change", not "proposed a
// plan". The card's Undo says how many changes Dispatch's record says it can take back. Once
// pressed, the run's record is read again, and where it says an undo landed and no change stays,
// the answer, which speaks of the changes, turns grey and the line under it reads "Undone"; the
// card's title says what was taken back, so no second line under the answer says it again. The
// fold line is the run's work, and says what the run applied after the Undo as before it
// (review R3).
test("an applied plan's run says what it applied, and after Undo greys its answer and reads Undone", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "goingPlan").mockImplementation(async () => appliedPlan);
  // Dispatch's record of the run: one change Undo can take back, until the Undo lands.
  let undone = false;
  const reread = spyOn(api, "goingRun").mockImplementation(async () =>
    undone
      ? record("done", 1000, { undone_at: new Date().toISOString(), undone_count: 1 })
      : record("done", 1000, { undoable: true, undoable_count: 1 })
  );
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementation(async () => {
    undone = true;
    return {
      lines: [
        {
          fields: [{ field: "priority", message: "written back", ok: true }],
          item: "DEMO-10",
          outcome: { code: "", message: "written back", ok: true, status: 200 },
          plan: 1,
          row: 0,
          tool: "dispatch_issue_update",
          undo: "write_back",
        },
      ],
    };
  });
  const proposed: readonly [string, unknown] = [
    "tool",
    { items: [], kind: "plan", label: "Proposed a plan", step: 3, tool: "propose_plan" },
  ];
  const [readFrame, ...rest] = appliedFrames;
  const restore = await showRun(
    "done",
    readFrame === undefined ? rest : [readFrame, proposed, ...rest],
    { run: { undoable: true, undoable_count: 1 } }
  );
  try {
    const card = await screen.findByTestId("going-plan");
    const undoButton = await within(card).findByTestId("going-undo-button");
    expect(undoButton).toHaveTextContent(/^Undo 1 change$/u);
    const fold = screen.getByTestId("going-steps-fold");
    await waitFor(() => expect(fold).toHaveAccessibleName(/· read 1 item, applied 1 change$/u));
    const answered = screen.getByTestId("going-run-answer");
    expect(fold.compareDocumentPosition(answered)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(answered.compareDocumentPosition(card)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(screen.getByTestId("going-run-state")).toHaveTextContent(/^Done$/u);

    await user.click(undoButton);
    await waitFor(() =>
      expect(within(card).getByTestId("going-plan-title")).toHaveTextContent(
        "Undone · 1 change reverted"
      )
    );
    await waitFor(() =>
      expect(screen.getByTestId("going-run-state")).toHaveTextContent(/^Undone$/u)
    );
    expect(fold).toHaveAccessibleName(/· read 1 item, applied 1 change$/u);
    // The answer keeps its words, in grey, and no line under it says when.
    expect(answered.textContent).toBe(appliedAnswer);
    expect(answered).toHaveAttribute("data-undone", "true");
    expect(answered.className).toContain(textMutedOnSurface);
    expect(screen.getByTestId("going-run").textContent ?? "").not.toContain("Undone at");
    expect(reread).toHaveBeenCalledWith("r-1");
  } finally {
    read.mockRestore();
    reread.mockRestore();
    restore();
  }
});

// Round 4 #1: whether an undo landed is read from what committed, as Dispatch's record says
// (`undone_at`), never from the Undo's answer: an Undo whose every change another writer kept
// put nothing back, so the run reads as before and its answer keeps its colour, as it would
// after a reload; the card says what became of the change.
test("an Undo that put nothing back leaves the run reading Done, as Dispatch's record says after it", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "goingPlan").mockImplementation(async () => appliedPlan);
  const reread = spyOn(api, "goingRun").mockImplementation(async () =>
    record("done", 1000, { undoable: true, undoable_count: 1 })
  );
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementation(async () => ({
    lines: [
      {
        fields: [
          {
            by: { id: "bob", kind: "user" },
            field: "priority",
            message: "holds P1 now, which user bob wrote after this run, so it is kept",
            now: 1,
            ok: false,
          },
        ],
        item: "DEMO-10",
        outcome: {
          code: "GOING_UNDO_CHANGED",
          message: "priority: holds …",
          ok: false,
          status: 409,
        },
        plan: 1,
        row: 0,
        tool: "dispatch_issue_update",
        undo: "write_back",
      },
    ],
  }));
  const restore = await showRun("done", appliedFrames);
  try {
    const card = await screen.findByTestId("going-plan");
    await user.click(await within(card).findByTestId("going-undo-button"));
    await waitFor(() => expect(within(card).getByTestId("going-undo-line")).toBeInTheDocument());
    await waitFor(() => expect(reread).toHaveBeenCalledWith("r-1"));
    await waitFor(() => expect(goingState().runs.get("r-1")?.record.undoable).toBe(true));
    expect(screen.getByTestId("going-run-state")).toHaveTextContent(/^Done$/u);
    expect(screen.getByTestId("going-run-answer")).not.toHaveAttribute("data-undone");
  } finally {
    read.mockRestore();
    reread.mockRestore();
    restore();
  }
});

// A run undone before this page was opened - in another tab, or before a reload - says so as
// Dispatch keeps it (`undone_at`, and no change of it that stays): its card's title reads "Undone"
// at that time, its answer is grey under "Undone", and its card offers no Undo, which has nothing
// left to take back.
test("a run undone before this page was opened says so on its card, as Dispatch keeps it, and offers no Undo", async () => {
  const read = spyOn(api, "goingPlan").mockImplementation(async () => appliedPlan);
  const undoneAt = "2026-11-18T11:35:00Z";
  const restore = await showRun("done", appliedFrames, {
    run: { undone_at: undoneAt, undone_count: 1 },
  });
  try {
    const card = await screen.findByTestId("going-plan");
    await waitFor(() =>
      expect(within(card).getByTestId("going-plan-title")).toHaveTextContent(
        `Undone · ${clockWords(Date.parse(undoneAt))}`
      )
    );
    expect(screen.getByTestId("going-run-state")).toHaveTextContent(/^Undone$/u);
    expect(screen.getByTestId("going-run-answer")).toHaveAttribute("data-undone", "true");
    expect(within(card).queryByTestId("going-undo-button")).toBeNull();
  } finally {
    read.mockRestore();
    restore();
  }
});

// Round 6 B: `undone_at` is when the undo of any one change landed, so a run whose record still
// counts a change Undo can take back (`undoable_count`) was undone in part: it reads "Partly
// undone" with how many changes stay, its answer keeps its colour, and its card says when and
// offers Undo of what Dispatch says it can take back.
test("a run undone in part before this page was opened says what stays, and its card offers Undo of what it can take back", async () => {
  const read = spyOn(api, "goingPlan").mockImplementation(async () => appliedPlan);
  const undoneAt = "2026-11-18T11:35:00Z";
  const restore = await showRun("done", appliedFrames, {
    run: { undoable: true, undoable_count: 1, undone_at: undoneAt, undone_count: 1 },
  });
  try {
    const card = await screen.findByTestId("going-plan");
    await waitFor(() =>
      expect(within(card).getByTestId("going-plan-title")).toHaveTextContent(
        `Partly undone · ${clockWords(Date.parse(undoneAt))}`
      )
    );
    expect(within(card).getByTestId("going-undo-button")).toHaveTextContent(/^Undo 1 change$/u);
    expect(screen.getByTestId("going-run-state")).toHaveTextContent(
      /^Partly undone · 1 change stays$/u
    );
    expect(screen.getByTestId("going-run-answer")).not.toHaveAttribute("data-undone");
  } finally {
    read.mockRestore();
    restore();
  }
});

// The class search's entry 36: a run can hold a change an Undo tried and could not take back - its
// undo is refused for good - beside one whose undo landed. Undo has nothing left to take back of
// such a run, and the page read that as "Undone" and greyed its answer, though the change
// stands. Dispatch's record counts it (`standing_count`), and the page says only that.
test("a run whose undo left a change standing reads Partly undone, never Undone, and offers no Undo", async () => {
  const read = spyOn(api, "goingPlan").mockImplementation(async () => appliedPlan);
  const undoneAt = "2026-11-18T11:35:00Z";
  const restore = await showRun("done", appliedFrames, {
    run: { standing_count: 1, undone_at: undoneAt, undone_count: 1 },
  });
  try {
    const card = await screen.findByTestId("going-plan");
    await waitFor(() =>
      expect(within(card).getByTestId("going-plan-title")).toHaveTextContent(
        `Partly undone · ${clockWords(Date.parse(undoneAt))}`
      )
    );
    expect(screen.getByTestId("going-run-state")).toHaveTextContent(
      /^Partly undone · 1 change stays$/u
    );
    expect(screen.getByTestId("going-run-answer")).not.toHaveAttribute("data-undone");
    expect(within(card).queryByTestId("going-undo-button")).toBeNull();
  } finally {
    read.mockRestore();
    restore();
  }
});

test("an answer whose frame says it waits for the person ends with Waits for your answer, not Done", async () => {
  const restore = await showRun("done", [
    [
      "reply",
      {
        labels: [],
        text: "Which changes hand it over: a label or a status",
        tools: [],
        waits_for_person: true,
      },
    ],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    expect((await screen.findByTestId("going-run-state")).textContent).toBe(
      "Waits for your answer"
    );
  } finally {
    restore();
  }
});

const NOTHING_CHANGED = "Nothing changed: no plan was proposed.";

/** An answer, and the end of a run done with it. */
const ANSWERED_DONE: readonly (readonly [string, unknown])[] = [
  ["reply", { labels: [], text: "DEMO-15 is marked as started.", tools: [] }],
  ["end", { offers: [], reason: "", state: "done" }],
];

/** What the line under the run's answer says. */
async function footWords(): Promise<string | null> {
  const foot = await screen.findByTestId("going-run-foot");
  return within(foot).getByTestId("going-run-state").textContent;
}

test("a run of the line Do that ended done with no plan says nothing changed in place of Done, read from its stored record", async () => {
  const restore = await showRun("done", ANSWERED_DONE, { run: { line: "do" } });
  try {
    expect(await footWords()).toBe(NOTHING_CHANGED);
    // The answer is a Markdown body, which a cold engine draws a moment later.
    await waitFor(() =>
      expect(screen.getByTestId("going-run-answer")).toHaveTextContent(
        "DEMO-15 is marked as started."
      )
    );
    expect(within(screen.getByTestId("going-run")).queryByText("Done")).toBeNull();
  } finally {
    restore();
  }
});

test("a run started here from the line Do sends the line, and says nothing changed once its frames end it done with no plan", async () => {
  let send: ((name: string, data: unknown) => void) | undefined;
  const start = spyOn(api, "createGoingRun").mockResolvedValue(
    record("working", 1000, { line: "do", words: "mark DEMO-15 started" })
  );
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    send = onFrame;
    return new Promise<void>(() => {});
  });
  try {
    renderWithQueries(<Conversation />);
    await act(() => startRun("mark DEMO-15 started", { path: "/" }, "alice", { line: "do" }));
    expect(start.mock.calls[0]?.[0]).toMatchObject({ line: "do", words: "mark DEMO-15 started" });
    act(() => send?.("reply", { labels: [], text: "DEMO-15 is marked as started.", tools: [] }));
    expect(screen.queryByText(NOTHING_CHANGED)).toBeNull();
    act(() => send?.("end", { offers: [], reason: "", state: "done" }));
    expect(await footWords()).toBe(NOTHING_CHANGED);
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

test("a run of the line Ask that ended done with no plan ends Done, as before", async () => {
  const restore = await showRun("done", ANSWERED_DONE, { run: { line: "ask" } });
  try {
    expect(await footWords()).toBe("Done");
  } finally {
    restore();
  }
});

test("a run started from no line that ended done with no plan ends Done", async () => {
  const restore = await showRun("done", ANSWERED_DONE);
  try {
    expect(await footWords()).toBe("Done");
  } finally {
    restore();
  }
});

test("a run of the line Do whose answer waits for the person ends Waits for your answer, not that nothing changed", async () => {
  const restore = await showRun(
    "done",
    [
      [
        "reply",
        {
          labels: [],
          text: "Which changes hand it over: a label or a status",
          tools: [],
          waits_for_person: true,
        },
      ],
      ["end", { offers: [], reason: "", state: "done" }],
    ],
    { run: { line: "do" } }
  );
  try {
    expect(await footWords()).toBe("Waits for your answer");
  } finally {
    restore();
  }
});

test("a run of the line Do that proposed a plan ends Done, not that nothing changed", async () => {
  const read = spyOn(api, "goingPlan").mockImplementation(async () => ({
    mode: "ask",
    plan: 1,
    rows: [],
    run: "r-1",
    state: "rejected",
    summary: "Mark DEMO-15 started.",
    version: "v1",
  }));
  const restore = await showRun(
    "done",
    [
      ["plan", { plan: 1, state: "waiting" }],
      ["plan", { plan: 1, state: "rejected" }],
      ...ANSWERED_DONE,
    ],
    { run: { line: "do" } }
  );
  try {
    expect(await footWords()).toBe("Done");
    expect(screen.queryByText(NOTHING_CHANGED)).toBeNull();
  } finally {
    restore();
    read.mockRestore();
  }
});

test("each issue the answer links is a chip: its key, a status dot and its title", async () => {
  const summary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "DEMO-1",
    project: "DEMO",
    status: "in_progress",
    title: "Map the lunar craters",
  });
  const restore = await showRun("done", [
    ["reply", { labels: [], text: "The oldest is [DEMO-1](dispatch://DEMO-1).", tools: [] }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    const answer = await screen.findByTestId("going-run-answer");
    const chip = await within(answer).findByTestId("item-chip");
    await waitFor(() => expect(chip).toHaveTextContent("DEMO-1In progressMap the lunar craters"));
    expect(within(chip).getByTestId("item-chip-status")).toHaveAttribute("title", "In progress");
    expect(within(answer).getByRole("link")).toHaveAttribute("href", "/issues/DEMO-1");
  } finally {
    restore();
    summary.mockRestore();
  }
});

// A chip that leads a line of a list marks it as a bullet would, so the answer's list of chips
// draws no bullet beside them (design v3, state 4); a line of words keeps its bullet. The body
// marks each line a chip leads (`data-chip-leads`), which the answer draws with no list mark. The
// lines are the rows of going-agent-answer-list.e2e.ts, where the browser reads each line's mark,
// and a line that is a chip alone. A chip with punctuation after it shares a span with that
// punctuation (`holdPunctuation`), and its line is still the chip's; a line whose words come
// first is not, even where a chip with no punctuation after it follows them. A line of the loose
// list holds a second paragraph, so its words stay in paragraphs and its chip begins the first.
test("a list of chips in the answer draws no bullet before each chip, and a list of words keeps its bullets", async () => {
  const chip = (key: string) => `[${key}](/issues/${key})`;
  const text = [
    "Four are open:",
    "",
    `- ${chip("DEMO-1")}: the mount is aligned again tonight`,
    `- ${chip("DEMO-2")}, with nobody on it yet`,
    `- ${chip("DEMO-3")} waits on clear weather`,
    `- ${chip("DEMO-4")}`,
    "",
    "Three more, at length:",
    "",
    `- ${chip("DEMO-5")}: the mount is aligned again tonight`,
    "",
    "  It drifts by morning.",
    "",
    `- ${chip("DEMO-6")}. Nobody is on it yet`,
    "",
    "  The frost holds it.",
    "",
    `- ${chip("DEMO-7")} waits on clear weather`,
    "",
    "  The sky is clouded.",
    "",
    "By state:",
    "",
    `- Aligning: ${chip("DEMO-8")} is in hand tonight`,
    `- Stalled: ${chip("DEMO-9")}, since the frost`,
    "- Nothing else is open there",
  ].join("\n");
  const restore = await showRun("done", [
    ["reply", { labels: [], text, tools: [] }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  try {
    const answer = await screen.findByTestId("going-run-answer");
    await waitFor(() => expect(within(answer).getAllByTestId("item-chip")).toHaveLength(9));
    const lines = [...answer.querySelectorAll("li")];
    // The first list's four lines, then the loose list's three, then the three lines of words.
    expect(lines.map((line) => line.firstElementChild?.tagName === "P")).toEqual([
      false,
      false,
      false,
      false,
      true,
      true,
      true,
      false,
      false,
      false,
    ]);
    expect(lines.map((line) => line.hasAttribute("data-chip-leads"))).toEqual([
      true,
      true,
      true,
      true,
      true,
      true,
      true,
      false,
      false,
      false,
    ]);
    expect(answer.querySelectorAll("[data-chip-line]")).toHaveLength(5);
  } finally {
    restore();
  }
});

test("a run whose stream dropped reads Reconnecting… until the stream comes back, and its steps carry on", async () => {
  let send: ((name: string, data: unknown) => void) | undefined;
  let dropped: ((dropped: boolean) => void) | undefined;
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    (_id, onFrame, _signal, onDropped) => {
      send = onFrame;
      dropped = onDropped;
      return new Promise<void>(() => {});
    }
  );
  try {
    openConversation({ id: "c-1", newest_at: new Date().toISOString(), runs: [record("working")] });
    renderWithQueries(<Conversation />);
    act(() => send?.("call", { kind: "read", label: "Reading DEMO-15", step: 2, tool: "read" }));
    expect(screen.queryByTestId("going-run-reconnecting")).toBeNull();

    act(() => dropped?.(true));
    expect(screen.getByTestId("going-run-reconnecting").textContent).toBe("Reconnecting…");
    expect(stepWords()).toEqual(["Reading DEMO-15…"]);

    act(() =>
      send?.("tool", {
        items: ["DEMO-15"],
        kind: "read",
        label: "Read DEMO-15",
        step: 2,
        tool: "read",
      })
    );
    expect(screen.queryByTestId("going-run-reconnecting")).toBeNull();
    expect(stepWords()).toEqual(["Read DEMO-15"]);
  } finally {
    stream.mockRestore();
  }
});

/** The stop reason of a withdrawn run, as Dispatch writes it (`goingRunWithdrawnReason`). */
const WITHDRAWN_REASON =
  "ANSWER_WITHDRAWN: a comment or an issue message this run read was deleted, so nothing the run wrote is shown";

/** The frames of a run that read a comment and answered with its words. */
const QUOTING_FRAMES: readonly (readonly [string, unknown])[] = [
  ["tool", { items: ["DEMO-15"], kind: "read", label: "Read the comment", step: 2, tool: "read" }],
  ["reply", { labels: [], text: "The comment says the harbour comet came in.", tools: [] }],
];

/** Whether the run's turn draws its words and the withdrawal's one line, and nothing of what the
 *  run wrote: no step, no answer, no block and no end card. */
function drawsTheWithdrawalAlone(): void {
  const run = screen.getByTestId("going-run");
  expect(within(run).getByTestId("going-run-words").textContent).toBe("where is DEMO-15");
  expect(within(run).getByTestId("going-run-withdrawn").textContent).toBe(
    "Dispatch withdrew this answer because a comment or message it read was deleted."
  );
  expect(within(run).queryByTestId("going-run-answer")).toBeNull();
  expect(within(run).queryByTestId("going-step-words")).toBeNull();
  expect(within(run).queryByTestId("going-run-end")).toBeNull();
  expect(run.textContent ?? "").not.toMatch(/harbour comet|ANSWER_WITHDRAWN|Read the comment/u);
}

// A run still streaming when the post it read is deleted is written the withdrawal's end frame:
// what it drew before, a delivery already sent, goes, and the one line stands in its place.
test("a run whose stream ends on the withdrawal draws one line in place of its steps and its answer", async () => {
  const restore = await showRun("working", [
    ...QUOTING_FRAMES,
    ["end", { offers: [], reason: WITHDRAWN_REASON, state: "stopped" }],
  ]);
  try {
    await waitFor(() => expect(screen.getByTestId("going-run-withdrawn")).toBeTruthy());
    drawsTheWithdrawalAlone();
  } finally {
    restore();
  }
});

// A run read to its end before the delete holds its frames; its record, read again on the delete,
// says it is withdrawn, and the turn draws the one line in place of all it holds.
test("a run whose record reads withdrawn draws one line though its frames ended it done", async () => {
  const restore = await showRun(
    "stopped",
    [...QUOTING_FRAMES, ["end", { offers: [], reason: "", state: "done" }]],
    { run: { stop_reason: WITHDRAWN_REASON } }
  );
  try {
    await waitFor(() => expect(screen.getByTestId("going-run-withdrawn")).toBeTruthy());
    drawsTheWithdrawalAlone();
    expect(goingState().runs.get("r-1")?.frames.replies).toHaveLength(1);
  } finally {
    restore();
  }
});

/** The stop reason of a run only its own plan's deletes withdrew, as Dispatch writes it. */
const WITHDRAWN_BY_RUN_REASON =
  "ANSWER_WITHDRAWN_BY_RUN: every comment or issue message this run read that is gone, this run deleted";

/** The plan of a run that deleted the comment it read, and changed DEMO-10's priority, once both
 *  ran: the deleted comment's row shows its author and the delete's mark, and no word of it. */
const deletingPlan: GoingPlanAnswer = {
  mode: "ask",
  plan: 1,
  rows: [
    {
      arguments: { comment: "c-1" },
      changed: false,
      claim: null,
      class: "text",
      deleted: true,
      fields: ["body"],
      issue: "DEMO-15",
      item: "c-1",
      needs: [],
      now: {
        author: { id: "alice", kind: "user" },
        deleted_at: "2026-11-24T15:00:00Z",
        deleted_by: { id: "alice", kind: "user" },
      },
      result: { code: "", delivery: null, message: "", status: 204 },
      row: 0,
      state: "done",
      tool: "dispatch_delete_comment",
      waits: [],
    },
    {
      arguments: { issue: "DEMO-10", priority: 2 },
      changed: false,
      claim: null,
      class: "field",
      fields: ["priority"],
      issue: "DEMO-10",
      item: "DEMO-10",
      needs: [],
      now: { priority: null },
      result: { code: "", delivery: null, message: "", status: 200 },
      row: 1,
      state: "done",
      tool: "dispatch_issue_update",
      waits: [],
    },
  ],
  run: "r-1",
  state: "done",
  summary: "",
  version: "v1",
};

/** The frames of a run that read a comment, quoted it, and ran a plan that deleted it, then ended
 *  in `state` with `reason`. */
function deletingFrames(
  reason: string,
  state: "done" | "stopped" = "stopped"
): readonly (readonly [string, unknown])[] {
  const [read, reply] = QUOTING_FRAMES;
  if (read === undefined || reply === undefined) throw new Error("a quoting run reads and replies");
  return [
    read,
    ["plan", { plan: 1, state: "waiting" }],
    ["plan", { plan: 1, state: "done" }],
    reply,
    ["end", { offers: [], reason, state }],
  ];
}

// The architect's ruling of 15:53Z: a run withdrawn only by a delete its own approved plan made
// keeps its plan's card, its rows and their results - the deleted comment by its author and the
// delete's mark, "Deleted, not undone" - and gives way to the withdrawal's line only in its answer
// and its steps, which may quote the comment; the line says that this run deleted it.
test("a run its own plan's delete withdrew keeps its plan's rows, and says so in one line in place of its answer and steps", async () => {
  const read = spyOn(api, "goingPlan").mockImplementation(async () => deletingPlan);
  const restore = await showRun("stopped", deletingFrames(WITHDRAWN_BY_RUN_REASON), {
    run: { stop_reason: WITHDRAWN_BY_RUN_REASON },
  });
  try {
    const card = await screen.findByTestId("going-plan");
    const run = screen.getByTestId("going-run");
    const line = within(run).getByTestId("going-run-withdrawn");
    expect(line.textContent).toBe(
      "Dispatch withdrew this answer because this run deleted a comment or message it read."
    );
    expect(line.compareDocumentPosition(card)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    const rows = within(card).getAllByTestId("going-plan-row");
    expect(rows).toHaveLength(2);
    const [deleted] = rows;
    await waitFor(() =>
      expect(within(deleted as HTMLElement).getByTestId("going-plan-post")).toHaveTextContent(
        /^Delete the comment by alice on DEMO-15/u
      )
    );
    expect(within(deleted as HTMLElement).getByTestId("going-plan-row-state")).toHaveTextContent(
      /^Deleted, not undone$/u
    );
    expect(within(deleted as HTMLElement).queryByTestId("going-plan-quote")).toBeNull();
    expect(within(run).queryByTestId("going-run-answer")).toBeNull();
    expect(within(run).queryByTestId("going-step-words")).toBeNull();
    expect(within(run).queryByTestId("going-steps-fold")).toBeNull();
    expect(within(run).queryByTestId("going-run-end")).toBeNull();
    expect(run.textContent ?? "").not.toMatch(/harbour comet|ANSWER_WITHDRAWN|Read the comment/u);
  } finally {
    restore();
    read.mockRestore();
  }
});

// A run another writer's delete withdrew draws the one line alone, though its plan's card was
// drawn before the delete: the plan, made from the post's words, may quote it.
test("a run another writer's delete withdrew takes its plan's card away with its answer", async () => {
  const read = spyOn(api, "goingPlan").mockImplementation(async () => deletingPlan);
  const restore = await showRun("done", deletingFrames("", "done"));
  let withdrawn: { mockRestore: () => void } | undefined;
  try {
    await screen.findByTestId("going-plan");
    expect(screen.getByTestId("going-run-answer")).toHaveTextContent("harbour comet");
    // Another writer deletes the comment the run read: the page reads every run's record again,
    // which now reads the run withdrawn.
    withdrawn = spyOn(api, "goingRun").mockImplementation(async () =>
      record("stopped", 1000, { stop_reason: WITHDRAWN_REASON })
    );
    act(() => rereadEveryRun());
    await waitFor(() => expect(screen.getByTestId("going-run-withdrawn")).toBeTruthy());
    drawsTheWithdrawalAlone();
    expect(screen.queryByTestId("going-plan")).toBeNull();
  } finally {
    withdrawn?.mockRestore();
    restore();
    read.mockRestore();
  }
});
