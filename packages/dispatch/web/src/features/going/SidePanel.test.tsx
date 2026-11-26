import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the keys of an IME composition below, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { answerRoute } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import type { GoingPlanAnswer, GoingRun } from "../../api/types";
import { goingState, openConversation, resetGoingStore, setPlace } from "./going-store";
import { SidePanel } from "./SidePanel";

// The conversation kept at the side of the page (spec R1, R11; design v3, state 10): a panel
// 420 px wide whose header moves it back over the page and whose × closes it; Esc in it puts the
// focus on the page and the panel stays; a click on the page names the item the next request is
// about, in place of the page's own issue and project, and the box's chip says it; and History,
// read again once a run starts there, lists that run. Its box is the bar's (design v3, states 2
// and 12): what Enter does now, the chip, the mode, Stop while the run works, and words sent
// during the run held until it ends.

const PAGE = "/issues/CORE-1";

const started: GoingRun = {
  context: null,
  continues: null,
  conversation: "c-9",
  created_at: new Date().toISOString(),
  finished_at: null,
  id: "r-9",
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
  words: "what does the runbook say",
};

/** The panel at the side of `PAGE`, beside `page`, alice signed in, on a client that keeps a read
 *  fresh for 30 s as the app's does (`main.tsx`). */
function renderPanel(page: ReactNode = null): void {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  window.history.pushState({}, "", PAGE);
  setPlace("side");
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
        initialEntries={[PAGE]}
      >
        {page}
        <SidePanel />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** Starts are answered with `started`, whose stream stays open. */
function answerStarts() {
  const start = spyOn(api, "createGoingRun").mockResolvedValue(started);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => Promise.withResolvers<void>().promise
  );
  return {
    restore: () => {
      start.mockRestore();
      stream.mockRestore();
    },
    start,
  };
}

afterEach(() => {
  cleanup();
  resetGoingStore();
  window.history.pushState({}, "", "/");
});

test("a click on a project's document is the next request's context, and the page's issue is not", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  try {
    renderPanel(<span data-dispatch-ref="/projects/OPS/documents/runbook">The runbook</span>);
    await user.click(screen.getByText("The runbook"));
    expect(screen.getByTestId("going-box-chip")).toHaveTextContent("the project OPS");
    await user.type(
      screen.getByRole("textbox", { name: "Ask the command bar" }),
      `${started.words}{Enter}`
    );

    await waitFor(() => expect(starts.start).toHaveBeenCalledTimes(1));
    expect(starts.start.mock.calls[0]?.[0].context).toEqual({
      block: "",
      issue: "",
      path: PAGE,
      project: "OPS",
    });
  } finally {
    starts.restore();
  }
});

test("a click on an issue on the page makes it the box's chip and the context of the next request", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  try {
    renderPanel(<span data-dispatch-ref="/issues/CORE-7">Track the comet</span>);
    const chip = screen.getByTestId("going-box-chip");
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    // The chip stands inside the box's frame, at the left of the words, as it does in the bar.
    expect(chip.parentElement).toBe(box.parentElement);
    expect(chip.nextElementSibling).toBe(box);
    expect(chip).toHaveTextContent("CORE-1");
    await user.click(screen.getByText("Track the comet"));
    expect(chip).toHaveTextContent("CORE-7");
    await user.type(
      screen.getByRole("textbox", { name: "Ask the command bar" }),
      `${started.words}{Enter}`
    );

    await waitFor(() => expect(starts.start).toHaveBeenCalledTimes(1));
    expect(starts.start.mock.calls[0]?.[0].context).toEqual({
      block: "",
      issue: "CORE-7",
      path: PAGE,
      project: "CORE",
    });
  } finally {
    starts.restore();
  }
});

test("the panel is 420 px wide; Open over the page moves it back, and its × closes it", async () => {
  const user = userEvent.setup();
  renderPanel();
  const panel = screen.getByTestId("going-side-panel");
  expect(panel.className).toContain("sm:w-[420px]");
  // The panel has the bar's header, conversation and box, and no search list.
  expect(within(panel).queryByRole("listbox")).toBeNull();
  // The move is an icon with its words as its tooltip, as the pin is, so the title keeps the
  // header's room.
  const move = within(panel).getByRole("button", { name: "Open over the page" });
  expect(move).toHaveAttribute("title", "Open over the page");
  expect(move.textContent).toBe("");
  expect(move.querySelector("svg")).not.toBeNull();
  await user.click(move);
  expect(goingState().place).toBe("over");
  expect(screen.queryByTestId("going-side-panel")).toBeNull();

  act(() => setPlace("side"));
  const close = within(screen.getByTestId("going-side-panel")).getByRole("button", {
    name: "Close",
  });
  expect(close).toHaveTextContent("×");
  await user.click(close);
  expect(goingState().place).toBe("closed");
  expect(screen.queryByTestId("going-side-panel")).toBeNull();
});

test("Esc in the panel puts the focus on the page and the panel stays; in the empty box while its run works, it stops the run", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  const stop = spyOn(api, "stopGoingRun").mockResolvedValue({
    ...started,
    state: "stopped",
    stop_reason: "STOPPED: its person stopped it",
  });
  try {
    renderPanel(
      <main id="main-content" tabIndex={-1}>
        The page
      </main>
    );
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    await user.type(box, "half a thought");
    await user.keyboard("{Escape}");
    expect(screen.getByRole("main")).toHaveFocus();
    expect(goingState().place).toBe("side");
    expect(screen.getByTestId("going-side-panel")).toBeInTheDocument();

    // Esc on any part of the panel does the same.
    screen.getByTestId("going-history-button").focus();
    await user.keyboard("{Escape}");
    expect(screen.getByRole("main")).toHaveFocus();

    await user.clear(box);
    await user.type(box, `${started.words}{Enter}`);
    await screen.findByTestId("going-run");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(stop).toHaveBeenCalledTimes(1));
    expect(stop.mock.calls[0]?.[0]).toBe(started.id);
    expect(box).toHaveFocus();
    expect(goingState().place).toBe("side");
  } finally {
    stop.mockRestore();
    starts.restore();
  }
});

test("the mode chosen in the panel's box is the level of the next run", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  try {
    renderPanel();
    await user.click(screen.getByTestId("going-level-button"));
    await user.click(screen.getByRole("menuitemradio", { name: /^Fast/ }));
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    expect(box).toHaveFocus();
    await user.keyboard(`${started.words}{Enter}`);

    await waitFor(() => expect(starts.start).toHaveBeenCalledTimes(1));
    expect(starts.start.mock.calls[0]?.[0]).toMatchObject({ level: "fast", words: started.words });
  } finally {
    starts.restore();
  }
});

test("History, opened again after a run starts at the side, lists that run", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  let listed = false;
  const history = spyOn(api, "myGoingRuns").mockImplementation(async () => ({
    limit: 50,
    offset: 0,
    rows: listed
      ? [{ id: started.conversation, newest_at: started.created_at, runs: [started] }]
      : [],
    total: listed ? 1 : 0,
  }));
  try {
    renderPanel();
    const toggle = screen.getByTestId("going-history-button");
    await user.click(toggle);
    expect(await screen.findByText("No conversation in the last 7 days.")).toBeInTheDocument();
    await user.click(toggle);

    listed = true;
    await user.type(
      screen.getByRole("textbox", { name: "Ask the command bar" }),
      `${started.words}{Enter}`
    );
    await screen.findByTestId("going-run");
    await user.click(toggle);

    const shown = await screen.findByTestId("going-history");
    // History names it by its title: the question with a capital.
    expect(await within(shown).findByText("What does the runbook say")).toBeInTheDocument();
  } finally {
    history.mockRestore();
    starts.restore();
  }
});

test("the panel's box says what Enter does, and words sent while its run works go as the next turn once it ends", async () => {
  const user = userEvent.setup();
  const next: GoingRun = {
    ...started,
    continues: started.id,
    id: "r-10",
    words: "and the appendix",
  };
  const start = spyOn(api, "createGoingRun")
    .mockResolvedValueOnce(started)
    .mockResolvedValueOnce(next);
  let send: ((name: string, data: unknown) => void) | undefined;
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    if (id === started.id) send = onFrame;
    return Promise.withResolvers<void>().promise;
  });
  try {
    renderPanel();
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    expect(box).toHaveAttribute("placeholder", "Ask…");
    expect(screen.getByTestId("going-box-chip")).toHaveTextContent("CORE-1");
    await user.type(box, `${started.words}{Enter}`);
    await screen.findByTestId("going-run");
    expect(box).toHaveAttribute("placeholder", "Ask a follow-up…");
    expect(screen.getByTestId("going-stop")).toBeInTheDocument();

    await user.type(box, "and the appendix{Enter}");
    expect(start).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("going-queued")).toHaveTextContent("and the appendix");

    act(() => {
      send?.("reply", { labels: [], text: "It says to page the on-call.", tools: [] });
      send?.("end", { offers: [], reason: "", state: "done" });
    });
    await waitFor(() => expect(start).toHaveBeenCalledTimes(2));
    expect(start.mock.calls[1]?.[0]).toMatchObject({
      continues: started.id,
      words: "and the appendix",
    });
    await waitFor(() => expect(screen.getAllByTestId("going-run")).toHaveLength(2));
    expect(screen.queryByTestId("going-queued")).toBeNull();
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

/** A plan of `started`'s run, waiting for its person, with no rows. */
const waitingPlan: GoingPlanAnswer = {
  mode: "ask",
  plan: 1,
  rows: [],
  run: started.id,
  state: "waiting",
  summary: "Page the on-call.",
  version: "v1",
};

/** Opens `started`'s conversation at the side, its run ended waiting on its plan, which Dispatch
 *  answers with `read`; a read of the run's record again answers it waiting still. */
function openWaitingRun(read: () => Promise<GoingPlanAnswer>): () => void {
  const waiting: GoingRun = { ...started, state: "waiting" };
  const spies: readonly { readonly mockRestore: () => void }[] = [
    spyOn(api, "goingPlan").mockImplementation(read),
    spyOn(api, "goingRun").mockResolvedValue(waiting),
    spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
      onFrame("plan", { plan: 1, state: "waiting" });
      onFrame("end", { offers: [], reason: "", state: "waiting" });
      return Promise.withResolvers<void>().promise;
    }),
  ];
  openConversation({ id: started.conversation, newest_at: started.created_at, runs: [waiting] });
  renderPanel();
  return () => {
    for (const spy of spies) spy.mockRestore();
  };
}

// A plan of a withdrawn run waits for nobody (`planWaits`): Dispatch approves and revises it for
// nobody. Its read can come before the run's record says the run is withdrawn, on the delete that
// withdrew it. The box then offers no Approve, its hint does not say the plan is being read, as
// the card above shows it, and Enter with words keeps them in the box, where a revision of the
// plan would have taken them from the box and sent them nowhere.
test("a plan read withdrawn offers no Approve, and Enter keeps the person's words in the box", async () => {
  const user = userEvent.setup();
  const start = spyOn(api, "createGoingRun");
  const revise = answerRoute<(run: string, body: unknown) => Promise<GoingPlanAnswer>>(
    "POST",
    "/api/v1/going/runs/*/plan/revise"
  );
  const restore = openWaitingRun(async () => ({ ...waitingPlan, withdrawn: true }));
  try {
    await screen.findByTestId("going-plan");
    expect(screen.queryByTestId("going-plan-approve")).toBeNull();
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    expect(screen.queryByTestId("going-box-hint")?.textContent ?? "").not.toMatch(
      /Approve|approving|Reading the plan/u
    );
    await user.type(box, "page the night shift too{Enter}");
    expect(box).toHaveValue("page the night shift too");
    expect(screen.queryByTestId("going-box-hint")?.textContent ?? "").not.toMatch(
      /Approve|approving|Reading the plan/u
    );
    expect(revise).not.toHaveBeenCalled();
    expect(start).not.toHaveBeenCalled();
  } finally {
    restore();
    start.mockRestore();
  }
});

// Before the plan is read, the hint under the box says it is being read; once that read failed,
// it says why, as the card does above its Retry, and no longer that the plan is being read.
test("once the plan's read failed, the hint under the box says why rather than Reading the plan…", async () => {
  const restore = openWaitingRun(async () => {
    throw new Error("the network dropped");
  });
  try {
    const card = await screen.findByTestId("going-plan-unreadable");
    expect(within(card).getByRole("alert")).toHaveTextContent("The run's plan could not be read.");
    expect(screen.getByTestId("going-box-hint")).toHaveTextContent(
      "The run's plan could not be read."
    );
  } finally {
    restore();
  }
});

// An Enter that confirms an input method's conversion is the input method's: Chromium and Firefox
// mark it `isComposing`, and Safari sends it after the composition's end, unmarked, with keyCode
// 229. It sends nothing and leaves the words in the box; a plain Enter after it sends them.
test("an Enter that confirms an input method's conversion in the panel's box sends nothing, and a plain Enter sends the words", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  try {
    renderPanel();
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    await user.type(box, started.words);
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(box, { isComposing: true, key: "Enter" });
    fireEvent.keyDown(box, { isComposing: false, key: "Enter", keyCode: 229 });
    expect(box).toHaveValue(started.words);
    expect(goingState().starting).toBeUndefined();
    expect(starts.start).not.toHaveBeenCalled();

    await user.keyboard("{Enter}");
    await waitFor(() => expect(starts.start).toHaveBeenCalledTimes(1));
    expect(starts.start.mock.calls[0]?.[0].words).toBe(started.words);
    expect(box).toHaveValue("");
  } finally {
    starts.restore();
  }
});

/** The page's own address, as the router holds it. */
function Here(): ReactNode {
  return <p data-testid="here">{useLocation().pathname}</p>;
}

// Design v3, state 10 (R1): a chip in the panel opens its item in the page with the router, and
// the panel stays; a load of the whole page would drop the panel and the conversation in it.
test("an item's chip in the panel's answer opens the item in the page with the router, and the panel stays", async () => {
  const user = userEvent.setup();
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    onFrame("reply", { labels: [], text: "The comet is [CORE-7](dispatch://CORE-7).", tools: [] });
    onFrame("end", { offers: [], reason: "", state: "done" });
    return Promise.withResolvers<void>().promise;
  });
  const summary = spyOn(api, "getIssueSummary").mockImplementation(async (key: string) => ({
    documents: [],
    key,
    project: "CORE",
    status: "todo",
    title: key === "CORE-7" ? "Track the comet" : "The page's issue",
  }));
  try {
    openConversation({
      id: started.conversation,
      newest_at: started.created_at,
      runs: [{ ...started, finished_at: started.created_at, state: "done" }],
    });
    renderPanel(<Here />);
    const chip = await within(screen.getByTestId("going-run-answer")).findByTestId("item-chip");
    await waitFor(() => expect(chip).toHaveTextContent("Track the comet"));
    await user.click(chip);
    expect(screen.getByTestId("here")).toHaveTextContent("/issues/CORE-7");
    expect(window.location.pathname).toBe(PAGE);
    expect(goingState().place).toBe("side");
    expect(screen.getByTestId("going-side-panel")).toBeInTheDocument();
    // The box's chip follows the page the panel now sits beside.
    expect(screen.getByTestId("going-box-chip")).toHaveTextContent("CORE-7");
  } finally {
    stream.mockRestore();
    summary.mockRestore();
  }
});

// The 420 px panel cut "Ask a follow-up…" to "Ask a follow-u" beside a long chip and Stop: the box
// keeps room for what it says whole, and the context chip gives way first.
test("the panel's box keeps room for what it says whole, and its chip gives way first", async () => {
  const user = userEvent.setup();
  const starts = answerStarts();
  try {
    renderPanel();
    const box = screen.getByRole("textbox", { name: "Ask the command bar" });
    await user.type(box, `${started.words}{Enter}`);
    await screen.findByTestId("going-run");
    expect(box).toHaveAttribute("placeholder", "Ask a follow-up…");
    expect(box.style.minWidth).toBe("calc(16ch + 0.75rem)");
    const chip = screen.getByTestId("going-box-chip").className.split(" ");
    expect(chip).toContain("min-w-0");
    expect(chip).toContain("shrink!");
  } finally {
    starts.restore();
  }
});
