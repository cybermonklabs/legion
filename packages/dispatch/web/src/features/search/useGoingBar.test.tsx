import { afterEach, beforeEach, describe, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { api } from "../../api/client";
import type { GoingConversation, GoingEvent, GoingLine, GoingRun } from "../../api/types";
import { goingState, resetGoingStore } from "../going/going-store";
import { FILED_NOTICE_MS, filingParent } from "./filing";
import {
  CONFIGURED_CAPABILITY,
  type GoingBarSpies,
  type OpenedStream,
  renderBar,
  spyOnGoingBar,
} from "./going-bar-fixture";
import { forgetAnsweredRun } from "./useGoingAnswer";
import {
  LINE_WAIT_MS,
  type LineEvent,
  type LineState,
  lineNext,
  lineShown,
  useGoingBar,
} from "./useGoingBar";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// The line on top of the command bar (spec R2, design v3, state 1): the hits show at once, and the
// line reads Ask, with a faint pulsing dot, until the decision model Jev, asked on the pause,
// names another line; an answer that comes 500 ms after the pause or later is dropped, so the
// line never changes under the person's Enter, and Enter on File as a new issue files the words.

const states: Readonly<Record<string, LineState>> = {
  typing: { kind: "typing" },
  "hits shown": { kind: "hits" },
  "Jev pending": { kind: "pending" },
  "Jev answered Do": { kind: "answered", line: "do" },
  "500 ms passed": { kind: "ask" },
  "late answer dropped": { kind: "dropped" },
};

const events: Readonly<Record<string, LineEvent>> = {
  "a keystroke": { kind: "keystroke" },
  "the hits arriving": { kind: "hits" },
  "the pause": { kind: "pause" },
  "Jev answering File": { kind: "jev", line: "file" },
  "500 ms": { kind: "wait" },
  Enter: { kind: "enter" },
};

const answeredFile: LineState = { kind: "answered", line: "file" };

type Row = readonly [keyof typeof states, keyof typeof events, LineState, GoingLine];

/** Each state under each event: the state the line moves to, and what it then reads, which is
 *  what Enter on it runs. */
const rows: readonly Row[] = [
  ["typing", "a keystroke", { kind: "typing" }, "ask"],
  ["typing", "the hits arriving", { kind: "hits" }, "ask"],
  ["typing", "the pause", { kind: "pending" }, "ask"],
  ["typing", "Jev answering File", { kind: "typing" }, "ask"],
  ["typing", "500 ms", { kind: "typing" }, "ask"],
  ["typing", "Enter", { kind: "ask" }, "ask"],
  ["hits shown", "a keystroke", { kind: "typing" }, "ask"],
  ["hits shown", "the hits arriving", { kind: "hits" }, "ask"],
  ["hits shown", "the pause", { kind: "pending" }, "ask"],
  ["hits shown", "Jev answering File", { kind: "hits" }, "ask"],
  ["hits shown", "500 ms", { kind: "hits" }, "ask"],
  ["hits shown", "Enter", { kind: "ask" }, "ask"],
  ["Jev pending", "a keystroke", { kind: "typing" }, "ask"],
  ["Jev pending", "the hits arriving", { kind: "pending" }, "ask"],
  ["Jev pending", "the pause", { kind: "pending" }, "ask"],
  ["Jev pending", "Jev answering File", answeredFile, "file"],
  ["Jev pending", "500 ms", { kind: "ask" }, "ask"],
  ["Jev pending", "Enter", { kind: "ask" }, "ask"],
  ["Jev answered Do", "a keystroke", { kind: "typing" }, "ask"],
  ["Jev answered Do", "the hits arriving", { kind: "answered", line: "do" }, "do"],
  ["Jev answered Do", "the pause", { kind: "answered", line: "do" }, "do"],
  ["Jev answered Do", "Jev answering File", { kind: "answered", line: "do" }, "do"],
  ["Jev answered Do", "500 ms", { kind: "answered", line: "do" }, "do"],
  ["Jev answered Do", "Enter", { kind: "answered", line: "do" }, "do"],
  ["500 ms passed", "a keystroke", { kind: "typing" }, "ask"],
  ["500 ms passed", "the hits arriving", { kind: "ask" }, "ask"],
  ["500 ms passed", "the pause", { kind: "ask" }, "ask"],
  ["500 ms passed", "Jev answering File", { kind: "dropped" }, "ask"],
  ["500 ms passed", "500 ms", { kind: "ask" }, "ask"],
  ["500 ms passed", "Enter", { kind: "ask" }, "ask"],
  ["late answer dropped", "a keystroke", { kind: "typing" }, "ask"],
  ["late answer dropped", "the hits arriving", { kind: "dropped" }, "ask"],
  ["late answer dropped", "the pause", { kind: "dropped" }, "ask"],
  ["late answer dropped", "Jev answering File", { kind: "dropped" }, "ask"],
  ["late answer dropped", "500 ms", { kind: "dropped" }, "ask"],
  ["late answer dropped", "Enter", { kind: "dropped" }, "ask"],
];

describe("theLineOnTopStates", () => {
  for (const [state, event, next, reads] of rows) {
    test(`the line ${state}, on ${event}, moves to ${next.kind} and reads ${reads}`, () => {
      const moved = lineNext(states[state] as LineState, events[event] as LineEvent);
      expect(moved).toEqual(next);
      expect(lineShown(moved)).toBe(reads);
    });
  }

  test("Enter runs what the line reads before it: Ask while Jev is pending, Do once he answered", () => {
    expect(lineShown(states["Jev pending"] as LineState)).toBe("ask");
    expect(lineShown(states["Jev answered Do"] as LineState)).toBe("do");
  });
});

// The same rules through the bar itself, its clock held, as AnswerPanel.test.tsx holds it: the
// pause asks the run of the words, whose decision frame a row sends down its stream.

let spies: GoingBarSpies;
let streams: OpenedStream[];
let createGoingRun: Mock<typeof api.createGoingRun>;

beforeEach(() => {
  jest.useFakeTimers();
  spies = spyOnGoingBar();
  streams = spies.streams;
  createGoingRun = spyOn(api, "createGoingRun").mockRejectedValue(new Error("no run in this test"));
});

afterEach(() => {
  spies.restore();
  createGoingRun.mockRestore();
  forgetAnsweredRun();
  resetGoingStore();
  realClock();
});

/** Types `words`, waits for the hits, and moves the clock through the pause that asks Jev. */
async function typeAndPause(user: UserEvent, words: string): Promise<void> {
  const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  await user.click(input);
  await user.keyboard(words);
  await tick(KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(1));
  expect(lineWords()).toHaveTextContent(/^Ask/);
  await tick(MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(streams).toHaveLength(1));
}

/** A row of the line on top: its first by default, or its second, Ask. */
function lineRow(id = "search-option-line"): HTMLElement {
  const line = document.getElementById(id);
  if (line === null) {
    throw new Error(`the bar lists no ${id}`);
  }
  return line;
}

/** What a row of the line on top says: its label and the words. */
function lineWords(id?: string): HTMLElement {
  return within(lineRow(id)).getByTestId("search-line-words");
}

/** The keys the foot names for the highlighted row. */
function footHints(): HTMLElement {
  return screen.getByTestId("search-keyboard-hints");
}

/** Hands the run's stream Jev's decision of `action`, which gives `line`. */
function decide(line: GoingLine, action: string): void {
  const frame: GoingEvent = { data: { action, context: "named", line }, event: "decision" };
  act(() => streams[0]?.send(frame));
}

test("while Jev may still decide, the line reads Ask with a faint pulsing dot in a place the row keeps", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "who looks after the astrolabe");
    const row = lineRow();
    const parts = row.children.length;
    expect(within(row).getByTestId("search-line-deciding")).toBeInTheDocument();
    expect(lineWords()).toHaveTextContent("Ask who looks after the astrolabe");
    expect(row).toHaveTextContent("↵");
    expect(footHints()).toHaveTextContent("↵ Ask · Esc Close");

    // Past the wait the line reads Ask for good: the dot goes, and its place stays.
    await tick(LINE_WAIT_MS);
    expect(within(row).queryByTestId("search-line-deciding")).toBeNull();
    expect(row.children).toHaveLength(parts);
    expect(lineWords()).toHaveTextContent("Ask who looks after the astrolabe");
  } finally {
    bar.unmount();
  }
});

test("Jev answering within 500 ms of the pause makes the line Do, it stays Do, and Ask is the second row", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "mark the astrolabe started");
    decide("do", "dispatch_issue_update");
    await waitFor(() => expect(lineWords()).toHaveTextContent(/^Do/));
    await tick(LINE_WAIT_MS);
    expect(lineWords()).toHaveTextContent("Do mark the astrolabe started");
    expect(lineRow()).toHaveAttribute("data-line", "do");
    expect(lineWords("search-option-line-ask")).toHaveTextContent("Ask mark the astrolabe started");
    expect(screen.queryByTestId("search-line-deciding")).toBeNull();
    expect(footHints()).toHaveTextContent("↵ Do · Esc Close");
  } finally {
    bar.unmount();
  }
});

test("Jev answering after 500 ms is dropped, and the line reads Ask", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "mark the astrolabe started");
    await tick(LINE_WAIT_MS);
    decide("do", "dispatch_issue_update");
    await tick(0);
    expect(lineWords()).toHaveTextContent(/^Ask/);
    expect(document.getElementById("search-option-line-ask")).toBeNull();
  } finally {
    bar.unmount();
  }
});

test("with no deciding model the line reads Ask, no dot pulses, and nothing says why", async () => {
  const user = userOnFakeClock();
  spies.goingCapability.mockResolvedValue({
    ...CONFIGURED_CAPABILITY,
    decision: { configured: false, model: "", reachable: false },
  });
  const bar = renderBar();
  try {
    await typeAndPause(user, "mark the astrolabe started");
    expect(lineWords()).toHaveTextContent("Ask mark the astrolabe started");
    expect(screen.queryByTestId("search-line-deciding")).toBeNull();
    expect(screen.queryByText(/deciding model/)).toBeNull();
  } finally {
    bar.unmount();
  }
});

test("Enter before Jev answers runs Ask: a run of the words of no kind, which records the line ask", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "who looks after the astrolabe");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(1));
    const [input] = createGoingRun.mock.calls[0] ?? [];
    expect(input).toMatchObject({
      level: "standard",
      line: "ask",
      words: "who looks after the astrolabe",
    });
    expect(input).not.toHaveProperty("kind");
  } finally {
    bar.unmount();
  }
});

test("Enter on the line Do runs the words recording the line do, the line on the screen at the moment of the key", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "mark the astrolabe started");
    decide("do", "dispatch_issue_update");
    await waitFor(() => expect(lineWords()).toHaveTextContent(/^Do/));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(1));
    const [input] = createGoingRun.mock.calls[0] ?? [];
    expect(input).toMatchObject({ line: "do", words: "mark the astrolabe started" });
    expect(input).not.toHaveProperty("kind");
  } finally {
    bar.unmount();
  }
});

test("Enter on the second row, Ask, under Do runs the words recording the line ask", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "mark the astrolabe started");
    decide("do", "dispatch_issue_update");
    await waitFor(() => expect(lineWords()).toHaveTextContent(/^Do/));
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("combobox", { name: "Search" })).toHaveAttribute(
      "aria-activedescendant",
      "search-option-line-ask"
    );
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(1));
    expect(createGoingRun.mock.calls[0]?.[0]).toMatchObject({
      line: "ask",
      words: "mark the astrolabe started",
    });
  } finally {
    bar.unmount();
  }
});

// Enter on File as a new issue (R9, design v3, state 13): the words are filed at once by a
// new-issue run, while the line's first row reads "Filing in LEGION…" with a spinner and nothing
// else changes; once the run's end names the issue, the bar closes and the notice reads "Filed
// LEGION-46: Instruments · Open · Undo" for 10 s, the issue named by its key and its title; a
// filing that files nothing stays in the bar, in words, with Try again.

const FILE_WORDS = "we need a dome for the astrolabe";

/** The record `POST /api/v1/going/runs` answers for the new-issue run `id`. */
function newIssueRun(id: string): GoingRun {
  return {
    context: null,
    continues: null,
    conversation: id,
    created_at: new Date().toISOString(),
    finished_at: null,
    id,
    issues: [],
    level: "fast",
    line: "file",
    mode: "ask",
    standing_count: 0,
    state: "queued",
    stop_reason: null,
    undoable: false,
    undoable_count: 0,
    undone_at: null,
    undone_count: 0,
    used: { calls: 0, tokens: 0, working_ms: 0 },
    words: FILE_WORDS,
  };
}

/** Each new-issue run's stream, held open until a row ends it with its end frame. */
function holdRunStreams() {
  const ends: ((end: Record<string, unknown>) => void)[] = [];
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    const { promise, resolve } = Promise.withResolvers<void>();
    ends.push((end) => {
      onFrame("end", end);
      resolve();
    });
    return promise;
  });
  return { ends, stream };
}

/** Types the words, has Jev answer File, and presses Enter on the line. */
async function enterOnFile(user: UserEvent): Promise<void> {
  await typeAndPause(user, FILE_WORDS);
  decide("file", "dispatch_issue");
  await waitFor(() => expect(lineWords()).toHaveTextContent(/^File as a new issue in LEGION/));
  expect(lineWords()).toHaveTextContent(`File as a new issue in LEGION: ${FILE_WORDS}`);
  expect(lineWords("search-option-line-ask")).toHaveTextContent(`Ask ${FILE_WORDS}`);
  expect(footHints()).toHaveTextContent("↵ File · Esc Close");
  await user.keyboard("{Enter}");
}

test("Enter on File as a new issue files the words: the line reads Filing, then the bar closes and the notice offers Open and Undo for 10 s", async () => {
  const user = userOnFakeClock();
  createGoingRun.mockResolvedValue(newIssueRun("run-filing"));
  const { ends, stream } = holdRunStreams();
  let closed = 0;
  const bar = renderBar(undefined, () => {
    closed += 1;
  });
  try {
    await enterOnFile(user);
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(1));
    const [input] = createGoingRun.mock.calls[0] ?? [];
    expect(input).toEqual({
      context: { block: "", issue: "", path: "/projects/LEGION/issues", project: "LEGION" },
      kind: "new_issue",
      line: "file",
      words: FILE_WORDS,
    });

    // The first row turns into the filing, and nothing else changes: the second row, the words
    // in the box and the bar itself stay as they were.
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "filing"));
    expect(lineWords()).toHaveTextContent("Filing in LEGION…");
    expect(lineRow()).not.toHaveTextContent("↵");
    expect(lineWords("search-option-line-ask")).toHaveTextContent(`Ask ${FILE_WORDS}`);
    expect(screen.getByRole("combobox", { name: "Search" })).toHaveValue(FILE_WORDS);
    expect(screen.getByRole("dialog", { name: "Search" })).toHaveAttribute("data-state", "search");
    expect(closed).toBe(0);

    await waitFor(() => expect(ends).toHaveLength(1));
    await act(async () => {
      ends[0]?.({ issue: "LEGION-46", offers: ["undo"], reason: "", state: "done" });
    });
    await waitFor(() => expect(closed).toBe(1));
    // The notice names what was filed: the key at once, then the issue's title, read from the
    // issue's summary, which the run's end does not carry (review R4).
    const notice = screen.getByTestId("going-notice");
    await waitFor(() =>
      expect(notice).toHaveTextContent("Filed LEGION-46: Instruments · Open · Undo")
    );
    expect(within(notice).getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      "/issues/LEGION-46"
    );
    expect(within(notice).getByRole("button", { name: "Undo" })).toBeInTheDocument();

    await tick(FILED_NOTICE_MS);
    expect(screen.queryByTestId("going-notice")).toBeNull();
  } finally {
    stream.mockRestore();
    bar.unmount();
  }
});

// The arrows the reader pressed are of the list they pressed them on (class search, entry 57;
// INFRA-2952's class). A conversation takes that list's place, and the search New brings back is
// a new list, where Continue takes the empty box's highlight as on any open. A filing of this
// open that is still out is that list's head and takes no press: a highlight the old list's arrow
// left to the head stood on it, and Enter did nothing instead of continuing.
test("New after an arrowed Enter lets Continue take the empty box's highlight again, above a filing still out", async () => {
  const user = userOnFakeClock();
  const asked = new Date(Date.now() - 600_000).toISOString();
  const earlier: GoingConversation = {
    id: "c-earlier",
    newest_at: asked,
    runs: [
      {
        ...newIssueRun("run-earlier"),
        conversation: "c-earlier",
        created_at: asked,
        finished_at: asked,
        level: "standard",
        line: "ask",
        state: "done",
        words: "who looks after the comet",
      },
    ],
  };
  const history = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [earlier],
    total: 1,
  });
  createGoingRun.mockResolvedValueOnce(newIssueRun("run-filing")).mockResolvedValueOnce({
    ...newIssueRun("run-asked"),
    level: "standard",
    line: "ask",
    words: "who looks after the astrolabe",
  });
  const { stream } = holdRunStreams();
  const bar = renderBar();
  try {
    await enterOnFile(user);
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "filing"));

    // Other words, an arrow to Ask under the Filing… row, which holds the head, and Enter.
    const box = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(box);
    await user.keyboard("who looks after the astrolabe");
    await tick(KEYSTROKE_DEBOUNCE_MS);
    await waitFor(() =>
      expect(lineWords("search-option-line-ask")).toHaveTextContent(
        "Ask who looks after the astrolabe"
      )
    );
    await user.keyboard("{ArrowDown}");
    expect(box).toHaveAttribute("aria-activedescendant", "search-option-line-ask");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.getByRole("dialog", { name: "Search" })).toHaveAttribute(
        "data-state",
        "conversation"
      )
    );

    // New, while the filing is still out: the empty box lists it first, then Continue, which
    // takes the highlight, so Enter continues.
    await user.click(screen.getByTestId("going-new"));
    const again = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await waitFor(() =>
      expect(again).toHaveAttribute("aria-activedescendant", "search-option-continue")
    );
    expect(again).toHaveValue("");
    expect(lineRow()).toHaveAttribute("data-line", "filing");
    expect(footHints()).toHaveTextContent("↵ Continue · Esc Close");
    await user.click(again);
    await user.keyboard("{Enter}");
    expect(goingState().conversation).toEqual(["run-earlier"]);
  } finally {
    history.mockRestore();
    stream.mockRestore();
    bar.unmount();
  }
});

test("a filing that files nothing stays in the bar in words, naming a near copy, with Try again, which files the words again", async () => {
  const user = userOnFakeClock();
  createGoingRun.mockResolvedValue(newIssueRun("run-refused"));
  const { ends, stream } = holdRunStreams();
  let closed = 0;
  const bar = renderBar(undefined, () => {
    closed += 1;
  });
  try {
    await enterOnFile(user);
    await waitFor(() => expect(ends).toHaveLength(1));
    await act(async () => {
      ends[0]?.({
        offers: [],
        reason:
          "NEW_ISSUE_NOT_FILED: the issue was not filed: POSSIBLE_DUPLICATE: possible duplicate of LEGION-7: The dome turns by hand",
        state: "failed",
      });
    });
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "failed"));
    // The reason in words, never a code, nor "not filed" twice, and the near copy it may repeat
    // by its key and title; then one next step.
    expect(lineWords()).toHaveTextContent(
      /^Not filed: a near copy is filed already: LEGION-7, The dome turns by hand$/u
    );
    expect(lineRow()).toHaveTextContent("Try again");
    expect(footHints()).toHaveTextContent("↵ Try again · Esc Close");
    expect(screen.queryByTestId("going-notice")).toBeNull();
    expect(closed).toBe(0);

    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(2));
    expect(createGoingRun.mock.calls[1]?.[0]).toMatchObject({
      kind: "new_issue",
      line: "file",
      words: FILE_WORDS,
    });
  } finally {
    stream.mockRestore();
    bar.unmount();
  }
});

// Every failure of a filing says why in words a person reads, as the short answer's failure does:
// a create Dispatch answered with a bare status, a create the run ended before, and the agent's
// own failure each read as a sentence, never as the server's or the model service's words.
const NOT_FILED_IN_WORDS: readonly (readonly [string, string, string])[] = [
  [
    "a create answered 503",
    "NEW_ISSUE_NOT_FILED: the issue was not filed: its create answered 503",
    "Not filed: Dispatch could not file it just now.",
  ],
  [
    "a create the run never ran",
    "NEW_ISSUE_NOT_FILED: the issue was not filed: its create is skipped and did not run",
    "Not filed: Dispatch stopped before it filed the issue.",
  ],
  [
    "a model service that failed",
    "MODEL_FAILED: azure: 502 Bad Gateway",
    "Not filed: Dispatch could not reach the model.",
  ],
];

for (const [what, reason, words] of NOT_FILED_IN_WORDS) {
  test(`a filing that ends with ${what} says why in words`, async () => {
    const user = userOnFakeClock();
    createGoingRun.mockResolvedValue(newIssueRun("run-not-filed"));
    const { ends, stream } = holdRunStreams();
    const bar = renderBar();
    try {
      await enterOnFile(user);
      await waitFor(() => expect(ends).toHaveLength(1));
      await act(async () => {
        ends[0]?.({ offers: [], reason, state: "failed" });
      });
      await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "failed"));
      expect(lineWords().textContent).toBe(words);
      expect(lineRow()).toHaveTextContent("Try again");
    } finally {
      stream.mockRestore();
      bar.unmount();
    }
  });
}

test("a filing Dispatch refuses to start says so in the bar, and a word typed after lets it go", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await enterOnFile(user);
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "failed"));
    expect(lineWords()).toHaveTextContent("Not filed: Dispatch could not be reached.");

    await user.keyboard("s");
    await waitFor(() => expect(lineRow()).not.toHaveAttribute("data-line", "failed"));
  } finally {
    bar.unmount();
  }
});

/** A conversation of the last hour, which an open of the bar with an empty box offers Continue
 *  on. */
function recentConversation(): GoingConversation {
  const at = new Date().toISOString();
  return {
    id: "c-1",
    newest_at: at,
    runs: [
      {
        ...newIssueRun("run-asked"),
        conversation: "c-1",
        created_at: at,
        line: "ask",
        state: "done",
        words: "who looks after the comet",
      },
    ],
  };
}

// A filing belongs to the open of the bar it was made in. Closed while the filing is out, the bar
// opened again lists Continue first and highlights it, and lists no filing; the filing's end,
// which no open of the bar draws now, says how it ended in the notice.
test("a filing of an earlier open is no row of the next, where Continue leads, and its failure says so in the notice", async () => {
  const user = userOnFakeClock();
  createGoingRun.mockResolvedValue(newIssueRun("run-filing"));
  const { ends, stream } = holdRunStreams();
  const first = renderBar();
  let second: { unmount: () => void } | undefined;
  try {
    await enterOnFile(user);
    await waitFor(() => expect(ends).toHaveLength(1));
    first.unmount();

    (api.myGoingRuns as Mock<typeof api.myGoingRuns>).mockResolvedValue({
      limit: 50,
      offset: 0,
      rows: [recentConversation()],
      total: 1,
    });
    second = renderBar();
    const box = screen.getByRole("combobox", { name: "Search" });
    await waitFor(() =>
      expect(box).toHaveAttribute("aria-activedescendant", "search-option-continue")
    );
    expect(document.getElementById("search-option-line")).toBeNull();

    await act(async () => {
      ends[0]?.({
        offers: [],
        reason: "NEW_ISSUE_NOT_FILED: the run proposed no issue to file",
        state: "failed",
      });
    });
    await waitFor(() =>
      expect(screen.getByTestId("going-notice")).toHaveTextContent(
        "Not filed: the run proposed no issue to file"
      )
    );
    expect(document.getElementById("search-option-line")).toBeNull();
  } finally {
    stream.mockRestore();
    second?.unmount();
  }
});

// A filing's end closes the open of the bar it was made in, and no other: a filing of an earlier
// open that files while the bar is open again says so in the notice and leaves the bar open.
test("a filing of an earlier open that files leaves the next open of the bar open, and its notice comes", async () => {
  const user = userOnFakeClock();
  createGoingRun.mockResolvedValue(newIssueRun("run-filing"));
  const { ends, stream } = holdRunStreams();
  const first = renderBar();
  let second: { unmount: () => void } | undefined;
  try {
    await enterOnFile(user);
    await waitFor(() => expect(ends).toHaveLength(1));
    first.unmount();
    let closed = 0;
    second = renderBar(undefined, () => {
      closed += 1;
    });
    await act(async () => {
      ends[0]?.({ issue: "LEGION-46", offers: ["undo"], reason: "", state: "done" });
    });
    await waitFor(() =>
      expect(screen.getByTestId("going-notice")).toHaveTextContent("Filed LEGION-46")
    );
    expect(closed).toBe(0);
  } finally {
    stream.mockRestore();
    second?.unmount();
  }
});

// Try again files the words again with the chip's context they were filed with: a chip moved
// since - Tab narrows it to the block the writing cursor was in as the bar opened - is no part of
// that filing.
test("Try again files the failed words with the context they were filed with, whatever the chip says since", async () => {
  const user = userOnFakeClock();
  createGoingRun.mockResolvedValue(newIssueRun("run-refused"));
  const { ends, stream } = holdRunStreams();
  // The page's writing cursor, in a block of the issue's document, as the bar opens over it.
  const page = document.createElement("div");
  page.contentEditable = "true";
  page.innerHTML = '<p data-block-id="b-7">The dome turns by hand.</p>';
  document.body.append(page);
  page.focus();
  window.getSelection()?.collapse(page.querySelector("p")?.firstChild ?? page, 3);
  const bar = renderBar(undefined, undefined, "/issues/LEGION-7");
  try {
    await typeAndPause(user, FILE_WORDS);
    decide("file", "dispatch_issue");
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "file"));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(ends).toHaveLength(1));
    const filed = createGoingRun.mock.calls[0]?.[0];
    expect(filed?.context).toEqual({
      block: "",
      issue: "LEGION-7",
      path: "/issues/LEGION-7",
      project: "LEGION",
    });
    await act(async () => {
      ends[0]?.({
        offers: [],
        reason: "NEW_ISSUE_NOT_FILED: the run proposed no issue to file",
        state: "failed",
      });
    });
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "failed"));

    await user.keyboard("{Tab}");
    await waitFor(() => expect(screen.getByTestId("going-box-chip")).toHaveTextContent("a block"));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(2));
    expect(createGoingRun.mock.calls[1]?.[0]).toEqual(filed);
  } finally {
    stream.mockRestore();
    bar.unmount();
    page.remove();
  }
});

// The filing line says what it will do (R9, design v3, state 13): over an issue's page the new
// issue goes under that issue, as the new-issue run files it, and the line names that parent
// before Enter. A filing line draws no short answer - Enter files the words and answers nothing -
// and the box's clear button keeps a gap from the words it follows.
test("over an issue's page the filing line names the issue it files under, and draws no short answer", async () => {
  const user = userOnFakeClock();
  const bar = renderBar(undefined, undefined, "/issues/LEGION-7");
  try {
    await typeAndPause(user, FILE_WORDS);
    act(() => streams[0]?.send({ data: { text: "No answer to that\\." }, event: "text" }));
    decide("file", "dispatch_issue");
    await waitFor(() => expect(lineRow()).toHaveAttribute("data-line", "file"));
    expect(lineWords()).toHaveTextContent(`File as a new issue under LEGION-7: ${FILE_WORDS}`);
    act(() => streams[0]?.send({ data: { items: 0, words: 4 }, event: "end" }));
    await tick(0);
    expect(screen.queryByTestId("going-answer")).toBeNull();
    const box = screen.getByRole("combobox", { name: "Search" });
    expect(box.className.split(" ")).toContain("[&::-webkit-search-cancel-button]:ms-2");
  } finally {
    bar.unmount();
  }
});

test("the page's issue is the filing's parent unless the words say top-level or name another issue", () => {
  expect(filingParent("we need a dome", "DEMO-6")).toBe("DEMO-6");
  expect(filingParent("we need a dome for demo-6", "DEMO-6")).toBe("DEMO-6");
  expect(filingParent("we need a dome", "")).toBeUndefined();
  expect(filingParent("a top-level issue for a dome", "DEMO-6")).toBeUndefined();
  expect(filingParent("a dome, top level", "DEMO-6")).toBeUndefined();
  expect(filingParent("a dome with no parent", "DEMO-6")).toBeUndefined();
  expect(filingParent("a dome under DEMO-12", "DEMO-6")).toBeUndefined();
});

// The line on top is of one question: the words, and the project the filter chip asks them of. A
// chip moved to another project asks the same words again, a run of its own, and the line starts
// over for it: the line Jev named for the run asked of the project before is not this run's, and
// the new run's decision names the line.
test("a project change asks the same words again, and the line on top starts over for that run", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const bar = renderHook(
    ({ project }: { readonly project: string }) =>
      useGoingBar({
        context: { path: "/projects/LEGION/issues" },
        enabled: true,
        login: "alice",
        project,
        showHits: true,
        words: "mark the astrolabe started",
      }),
    {
      initialProps: { project: "LEGION" },
      wrapper: ({ children }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );
  try {
    await waitFor(() => expect(bar.result.current.answers).toBe(true));
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(1));
    decide("do", "dispatch_issue_update");
    await waitFor(() => expect(bar.result.current.line).toBe("do"));

    bar.rerender({ project: "CORE" });
    expect(bar.result.current.line).toBe("ask");
    expect(bar.result.current.deciding).toBe(true);
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    expect(spies.createGoingRequest.mock.calls[1]?.[0]).toMatchObject({ project: "CORE" });
    act(() =>
      streams[1]?.send({
        data: { action: "dispatch_issue", context: "named", line: "file" },
        event: "decision",
      })
    );
    await waitFor(() => expect(bar.result.current.line).toBe("file"));
  } finally {
    bar.unmount();
    client.clear();
  }
});

// The line on top is also of whether the words are asked of a model at all. Words the bar stops
// asking about, as words become a hit's exact title once the hits arrive, keep no line Jev named
// while they were asked, though the words and the project stay as they were.
test("words the bar stops asking about drop the line Jev named for them, the words and the project unchanged", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const bar = renderHook(
    ({ enabled }: { readonly enabled: boolean }) =>
      useGoingBar({
        context: { path: "/projects/LEGION/issues" },
        enabled,
        login: "alice",
        project: "LEGION",
        showHits: true,
        words: "mark the astrolabe started",
      }),
    {
      initialProps: { enabled: true },
      wrapper: ({ children }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );
  try {
    await waitFor(() => expect(bar.result.current.answers).toBe(true));
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(1));
    decide("do", "dispatch_issue_update");
    await waitFor(() => expect(bar.result.current.line).toBe("do"));

    bar.rerender({ enabled: false });
    expect(bar.result.current.line).toBe("ask");
  } finally {
    bar.unmount();
    client.clear();
  }
});
