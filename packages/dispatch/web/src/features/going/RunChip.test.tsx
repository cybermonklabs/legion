import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import type { GoingPlanAnswer, GoingRun } from "../../api/types";
import { borderDefault, card } from "../../theme/classes";
import {
  goingState,
  newConversation,
  openConversation,
  openRun,
  setPlace,
  startRun,
} from "./going-store";
import { Notice } from "./Notice";
import { RunChip } from "./RunChip";

// The chip at the top of the page and the notice at its bottom right (spec R11; design v3, state
// 11): while no conversation is on the screen, the chip names one conversation of the page and
// how it stands in words, the most urgent where more than one goes on, with how many more, and a
// click opens it; and a run that ends, or comes to wait on its plan, while the bar is closed shows
// a notice for 6 s, and none while the bar is open.

const CONTEXT = { path: "/projects/CORE/issues" };

const ENDED_DONE = { offers: [], reason: "", state: "done" };
const ENDED_FAILED = {
  offers: [],
  reason: "MODEL_FAILED: the model did not answer",
  state: "failed",
};

function record(id: string, conversation: string, words: string): GoingRun {
  return {
    context: null,
    continues: null,
    conversation,
    created_at: new Date().toISOString(),
    finished_at: null,
    id,
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
    words,
  };
}

/** Answers each start with the next of `records`, holds each run's stream open, handing the test
 *  the way to send it a frame, and answers each run's plan as one change of CORE-10. */
function answerRuns(...records: GoingRun[]) {
  const start = spyOn(api, "createGoingRun");
  for (const made of records) start.mockResolvedValueOnce(made);
  const frames = new Map<string, (name: string, data: unknown) => void>();
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    frames.set(id, onFrame);
    return Promise.withResolvers<void>().promise;
  });
  const plan = spyOn(api, "goingPlan").mockImplementation(
    async (run): Promise<GoingPlanAnswer> => ({
      mode: "ask",
      plan: 1,
      rows: [
        {
          arguments: { issue: "CORE-10", priority: 2 },
          changed: false,
          claim: null,
          class: "field",
          fields: ["priority"],
          issue: "CORE-10",
          item: "CORE-10",
          needs: [],
          now: null,
          result: null,
          row: 1,
          state: "waiting",
          tool: "dispatch_issue_update",
          waits: [],
        },
      ],
      run,
      state: "waiting",
      summary: "Raise CORE-10.",
      version: "c41d07",
    })
  );
  return {
    restore: () => {
      start.mockRestore();
      stream.mockRestore();
      plan.mockRestore();
    },
    send: (id: string, name: string, data: unknown) => act(() => frames.get(id)?.(name, data)),
  };
}

/** Starts a conversation of its own with `words`, its bar closed. */
async function begin(words: string): Promise<void> {
  await act(async () => {
    newConversation();
    await startRun(words, CONTEXT, "alice");
  });
}

/** The chip and the notice as the shell draws them, the notice's Open and Review inside a router. */
function renderChip(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <RunChip />
        <Notice />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function chipText(): string | null {
  return screen.getByTestId("going-chip-text").textContent;
}

test("the chip names the most urgent conversation in words, and how many more go on, and a click opens it", async () => {
  const user = userEvent.setup();
  // Dispatch started the second run after the first, as its own clock says.
  const runs = answerRuns(record("r-1", "c-1", "count the moons"), {
    ...record("r-2", "c-2", "raise CORE-10"),
    created_at: new Date(Date.now() + 1_000).toISOString(),
  });
  try {
    renderChip();
    expect(screen.queryByTestId("going-chip")).toBeNull();

    await begin("count the moons");
    expect(screen.getByTestId("going-chip")).toHaveAttribute("data-state", "working");
    expect(chipText()).toBe("Working · Count the moons");
    expect(screen.queryByTestId("going-chip-more")).toBeNull();

    // Both work: the newest is named, and one more goes on.
    await begin("raise CORE-10");
    expect(chipText()).toBe("Working · Raise CORE-10");
    expect(screen.getByTestId("going-chip-more")).toHaveTextContent("+1");

    // A failure comes before a run that works.
    runs.send("r-1", "end", ENDED_FAILED);
    expect(chipText()).toBe("Failed · Count the moons");
    expect(screen.getByTestId("going-chip")).toHaveAttribute("data-state", "failed");

    // A plan that waits comes before both, named by its card's title once it is read.
    runs.send("r-2", "plan", { plan: 1, state: "waiting" });
    expect(screen.getByTestId("going-chip")).toHaveAttribute("data-state", "waiting");
    await waitFor(() => expect(chipText()).toBe("Needs you · Change CORE-10"));
    expect(screen.getByTestId("going-chip-more")).toHaveTextContent("+1");

    await user.click(screen.getByTestId("going-chip-open"));
    expect(goingState().place).toBe("over");
    expect(goingState().conversation).toEqual(["r-2"]);
    expect(screen.queryByTestId("going-chip")).toBeNull();
  } finally {
    runs.restore();
  }
});

// A conversation's runs are ordered on Dispatch's clock (`created_at`), whether this page started
// them or History listed them: a page whose clock is behind Dispatch's still names the follow-up
// it started, which works, and the conversation by its first question.
test("the chip names the follow-up started here, though this page's clock is behind Dispatch's", async () => {
  const ahead = Date.now() + 60_000;
  const first: GoingRun = {
    ...record("r-1", "c-1", "count the moons"),
    created_at: new Date(ahead).toISOString(),
    state: "done",
  };
  const runs = answerRuns({
    ...record("r-2", "c-1", "and the comets"),
    continues: "r-1",
    created_at: new Date(ahead + 10_000).toISOString(),
  });
  try {
    renderChip();
    act(() => openConversation({ id: "c-1", newest_at: first.created_at, runs: [first] }));
    await act(async () => {
      await startRun("and the comets", CONTEXT, "alice");
    });
    act(() => setPlace("closed"));
    expect(screen.getByTestId("going-chip")).toHaveAttribute("data-state", "working");
    expect(chipText()).toBe("Working · Count the moons");
  } finally {
    runs.restore();
  }
});

test("the chip is a pill of its own, apart from the page: a background, a border and a small shadow, 24 px tall", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  try {
    renderChip();
    await begin("count the moons");
    const chip = screen.getByTestId("going-chip");
    for (const name of [...card.split(" "), ...borderDefault.split(" ")]) {
      expect(chip).toHaveClass(name);
    }
    expect(chip).toHaveClass("border", "rounded-full", "shadow-md");
    // The shell floats the chip over the top of the page (`app.tsx`), in a row of no height. The
    // chip is 24 px tall (review R4): its button, which is the whole pill with its rule
    // (`-m-px`), holds that height, and takes no 44 px floor (`data-run-chip`). It keeps room
    // above it (review n6), 4 px and 8 px from the `md` width, rather than stretching.
    expect(chip).toHaveClass("self-start", "mt-1", "md:mt-2", "leading-4");
    expect(chip).toHaveAttribute("data-run-chip");
    expect(screen.getByTestId("going-chip-open")).toHaveClass("min-h-6", "-m-px");
  } finally {
    runs.restore();
  }
});

test("Done shows a blue dot until it is seen, a stop reads Stopped, and the chip hides once nothing goes on", async () => {
  const runs = answerRuns(
    record("r-1", "c-1", "count the moons"),
    record("r-2", "c-2", "watch the comet")
  );
  try {
    renderChip();
    await begin("count the moons");
    runs.send("r-1", "end", ENDED_DONE);
    await begin("watch the comet");
    runs.send("r-2", "end", {
      offers: [],
      reason: "STOPPED: its person stopped it",
      state: "stopped",
    });
    expect(chipText()).toBe("Done · Count the moons");
    expect(screen.getByTestId("going-chip-unread")).toBeInTheDocument();
    expect(screen.getByTestId("going-chip-more")).toHaveTextContent("+1");

    // Seen in the bar, the answer is no longer named.
    act(() => openRun("r-1"));
    act(() => setPlace("closed"));
    expect(chipText()).toBe("Stopped · Watch the comet");
    expect(screen.queryByTestId("going-chip-unread")).toBeNull();
    expect(screen.queryByTestId("going-chip-more")).toBeNull();

    act(() => openRun("r-2"));
    act(() => setPlace("closed"));
    expect(screen.queryByTestId("going-chip")).toBeNull();
  } finally {
    runs.restore();
  }
});

test("a run that ends while the bar is closed shows its notice for 6 s", async () => {
  jest.useFakeTimers();
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  try {
    renderChip();
    await begin("count the moons");
    expect(screen.queryByTestId("going-notice")).toBeNull();
    runs.send("r-1", "end", ENDED_DONE);
    expect(screen.getByTestId("going-notice")).toHaveTextContent("Done: Count the moons · Open");

    await tick(5_900);
    expect(screen.getByTestId("going-notice")).toBeInTheDocument();
    await tick(200);
    expect(screen.queryByTestId("going-notice")).toBeNull();
  } finally {
    runs.restore();
    realClock();
  }
});

test("a failure and a plan that waits each show their notice, whose action opens the conversation, and none shows while the bar is open", async () => {
  const user = userEvent.setup();
  const runs = answerRuns(
    record("r-1", "c-1", "count the moons"),
    record("r-2", "c-2", "watch the comet"),
    record("r-3", "c-3", "raise CORE-10")
  );
  try {
    renderChip();
    await begin("count the moons");
    runs.send("r-1", "end", ENDED_FAILED);
    expect(screen.getByTestId("going-notice")).toHaveTextContent("Failed: Count the moons · Open");
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.queryByTestId("going-notice")).toBeNull();
    expect(goingState().place).toBe("over");
    expect(goingState().conversation).toEqual(["r-1"]);

    // While the bar is over the page, and while the conversation is at the side, a run that
    // ends shows no notice.
    await begin("watch the comet");
    runs.send("r-2", "reply", { labels: [], text: "It is near.", tools: [] });
    act(() => setPlace("side"));
    runs.send("r-2", "end", ENDED_DONE);
    expect(screen.queryByTestId("going-notice")).toBeNull();
    act(() => setPlace("closed"));
    expect(screen.queryByTestId("going-notice")).toBeNull();

    // A plan that comes to wait is named by its card's title, and Review opens it.
    await begin("raise CORE-10");
    runs.send("r-3", "plan", { plan: 1, state: "waiting" });
    expect(await screen.findByTestId("going-notice")).toHaveTextContent(
      "Needs you: Change CORE-10 · Review"
    );
    await user.click(screen.getByRole("button", { name: "Review" }));
    expect(goingState().place).toBe("over");
    expect(goingState().conversation).toEqual(["r-3"]);
  } finally {
    runs.restore();
  }
});
