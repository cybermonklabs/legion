import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE } from "@legion/contracts";
import { act, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  BAR_HIT,
  CONFIGURED_CAPABILITY,
  type GoingBarSpies,
  renderBar,
  spyOnGoingBar,
} from "./going-bar-fixture";
import { searchAnswer } from "./search-fixture";
import { forgetAnsweredRun } from "./useGoingAnswer";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// What the bar cannot do on this Dispatch, as the person meets it: said once, at the bar's foot,
// from the moment the bar opens, and never under an answer, so a second answer in the same bar
// brings no second copy and pushes no answer down; and nothing at all where every part of the
// bar is configured and meaning search runs. The clock is held, as in AnswerPanel.test.tsx.

let spies: GoingBarSpies;

beforeEach(() => {
  jest.useFakeTimers();
  spies = spyOnGoingBar();
});

afterEach(() => {
  spies.restore();
  forgetAnsweredRun();
  realClock();
});

/** Types `words`, lets the pause ask them, and has the run answer `text` and end. */
async function askAndAnswer(user: UserEvent, words: string, text: string): Promise<void> {
  const asked = spies.streams.length;
  await user.click(screen.getByRole("combobox", { name: "Search" }));
  await user.keyboard(words);
  await tick(KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
  await tick(MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(spies.streams).toHaveLength(asked + 1));
  act(() => {
    spies.streams[asked]?.send({ data: { text }, event: "text" });
    spies.streams[asked]?.send({ data: { items: 0, words: 2 }, event: "end" });
  });
  await waitFor(() => expect(screen.getByTestId("going-answer-text")).toHaveTextContent(text));
}

/** Whether `later` comes after `earlier` in the page, as the bar lays them out top to bottom. */
function follows(earlier: Element, later: Element): boolean {
  return (earlier.compareDocumentPosition(later) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
}

const NOTES = ["search-degraded", "going-no-session", "going-no-label"] as const;

test("what the bar cannot do is said once at its foot from the moment it opens, never under an answer, and a second answer repeats none of it", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  spies.goingCapability.mockResolvedValue({
    ...CONFIGURED_CAPABILITY,
    label: { configured: false },
    session: { configured: false },
  });
  spies.search.mockResolvedValue({
    ...searchAnswer([BAR_HIT]),
    degraded: SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  });
  const bar = renderBar();

  try {
    // The bar opens with an empty box, and already says the two settings it lacks.
    const opened = await screen.findByTestId("going-capability");
    expect(within(opened).getByTestId("going-no-session")).toBeInTheDocument();
    expect(within(opened).getByTestId("going-no-label")).toBeInTheDocument();

    await askAndAnswer(user, "astrolabe", "The first answer");
    await askAndAnswer(user, "s", "The second answer");

    // After the second answer each note is on the screen once, in the bar's foot, below the
    // hits and the answer, and none of them is the answer's.
    const foot = screen.getByTestId("going-capability");
    const panel = screen.getByTestId("going-answer");
    for (const id of NOTES) {
      expect(screen.getAllByTestId(id)).toHaveLength(1);
      expect(within(foot).getByTestId(id)).toBeInTheDocument();
      expect(within(panel).queryByTestId(id)).not.toBeInTheDocument();
    }
    expect(follows(screen.getByRole("listbox"), foot)).toBe(true);
    expect(follows(panel, foot)).toBe(true);
  } finally {
    bar.unmount();
  }
});

test("a Dispatch whose every part is configured and whose meaning search runs says nothing of what the bar cannot do, across its answers", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await askAndAnswer(user, "astrolabe", "The first answer");
    await askAndAnswer(user, "s", "The second answer");

    expect(screen.queryByTestId("going-capability")).not.toBeInTheDocument();
    for (const id of NOTES) {
      expect(screen.queryByTestId(id)).not.toBeInTheDocument();
    }
  } finally {
    bar.unmount();
  }
});
