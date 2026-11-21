import { afterEach, beforeEach, describe, expect, jest, type Mock, spyOn, test } from "bun:test";
import { act, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { api } from "../../api/client";
import type { GoingEvent, GoingLine } from "../../api/types";
import { resetGoingStore } from "../going/going-store";
import {
  type GoingBarSpies,
  type OpenedStream,
  renderBar,
  spyOnGoingBar,
} from "./going-bar-fixture";
import { forgetAnsweredRun } from "./useGoingAnswer";
import { LINE_WAIT_MS, type LineEvent, type LineState, lineNext, lineShown } from "./useGoingBar";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// The line on top of the command bar (spec R2, plan.md t13): the hits show at once, and the line
// reads Ask until the decision model Jev, asked on the pause, names another line; an answer that
// comes 500 ms after the pause or later is dropped, so the line never changes under the person's
// Enter, and Enter on File as a new issue starts a new-issue run.

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
  expect(lineOnTop()).toHaveTextContent(/^Ask/);
  await tick(MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(streams).toHaveLength(1));
}

function lineOnTop(): HTMLElement {
  const line = document.getElementById("search-option-line");
  if (line === null) {
    throw new Error("the bar lists no line on top");
  }
  return line;
}

/** Hands the run's stream Jev's decision of `action`, which gives `line`. */
function decide(line: GoingLine, action: string): void {
  const frame: GoingEvent = { data: { action, context: "named", line }, event: "decision" };
  act(() => streams[0]?.send(frame));
}

test("Jev answering within 500 ms of the pause makes the line Do, and it stays Do", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "mark the astrolabe started");
    decide("do", "dispatch_issue_update");
    await waitFor(() => expect(lineOnTop()).toHaveTextContent(/^Do/));
    await tick(LINE_WAIT_MS);
    expect(lineOnTop()).toHaveTextContent(/^Do/);
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
    expect(lineOnTop()).toHaveTextContent(/^Ask/);
  } finally {
    bar.unmount();
  }
});

test("Jev answering dispatch_issue names the page's project, and Enter starts a new-issue run of the words", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "we need a dome for the astrolabe");
    decide("file", "dispatch_issue");
    await waitFor(() => expect(lineOnTop()).toHaveTextContent(/^File as a new issue in LEGION/));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(1));
    const [input] = createGoingRun.mock.calls[0] ?? [];
    expect(input).toMatchObject({ kind: "new_issue", words: "we need a dome for the astrolabe" });
    expect(input).not.toHaveProperty("level");
  } finally {
    bar.unmount();
  }
});

test("Enter before Jev answers runs Ask: a run of the words of no kind", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await typeAndPause(user, "who looks after the astrolabe");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createGoingRun).toHaveBeenCalledTimes(1));
    const [input] = createGoingRun.mock.calls[0] ?? [];
    expect(input).toMatchObject({ level: "standard", words: "who looks after the astrolabe" });
    expect(input).not.toHaveProperty("kind");
  } finally {
    bar.unmount();
  }
});
