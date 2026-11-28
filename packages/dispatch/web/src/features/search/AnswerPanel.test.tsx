import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_ANSWER_WITHDRAWN,
  GOING_ERROR_NO_MODEL_CONFIGURED,
} from "@legion/contracts";
import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { UNCONFIGURED_CAPABILITY } from "../../__tests__/network";
import { userOnFakeClock } from "../../__tests__/user";
import { api } from "../../api/client";
import type { GoingEvent } from "../../api/types";
import { goingState } from "../going/going-store";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { TABLE_ROWS } from "./blocks/TableBlock";
import {
  type GoingBarSpies,
  type OpenedStream,
  renderBar,
  spyOnGoingBar,
} from "./going-bar-fixture";
import { forgetAnsweredRun } from "./useGoingAnswer";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// The bar's answer as the person meets it: asked on the pause and never on a keystroke, one run
// out at a time, cancelled by the next keystroke and by closing the bar, each frame of its stream
// drawn as its line, the answer the person typed past kept, marked as the earlier words', until
// the next answer's first words take its place, and the last answer drawn again as the bar
// reopens. The clock is held, as in SearchPalette.test.tsx, so which of the bar's timers has
// fired is the row's to decide.

let streams: OpenedStream[];
let createGoingRequest: Mock<typeof api.createGoingRequest>;
let cancelGoingRequest: Mock<typeof api.cancelGoingRequest>;
let goingCapability: Mock<typeof api.goingCapability>;
let spies: GoingBarSpies;

beforeEach(() => {
  jest.useFakeTimers();
  spies = spyOnGoingBar();
  ({ cancelGoingRequest, createGoingRequest, goingCapability, streams } = spies);
});

afterEach(() => {
  spies.restore();
  forgetAnsweredRun();
  realClock();
});

/** Types `words` and moves the clock past the keystroke's search, short of the pause. */
async function type(user: UserEvent, words: string): Promise<void> {
  const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  await user.click(input);
  await user.keyboard(words);
  await tick(KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
}

/** Hands `stream` `frames`, in order, as the server writes them. */
function send(stream: OpenedStream | undefined, ...frames: GoingEvent[]): void {
  act(() => {
    for (const frame of frames) stream?.send(frame);
  });
}

const pauseRest = MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS;

/** Waits for the panel's mark on an answer kept from words the person typed past, and reads that
 *  it is a status naming exactly `words`, the words that answer answers. */
async function expectKeptFor(words: string): Promise<void> {
  const mark = await screen.findByTestId("going-answer-stale");
  expect(mark).toHaveAttribute("role", "status");
  expect(mark).toHaveTextContent(new RegExp(`\\b${words}\\b`));
}

test("the pause asks once with the chip's context, and the next keystroke cancels the run out", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    // Every keystroke and the keystroke's search have happened: still no question asked.
    expect(createGoingRequest).not.toHaveBeenCalled();

    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    expect(createGoingRequest.mock.calls.map(([input]) => input)).toEqual([
      {
        context: { block: "", issue: "", path: "/projects/LEGION/issues", project: "LEGION" },
        project: "",
        words: "astrolabe",
      },
    ]);
    const first = streams[0];
    send(first, { data: { text: "The first answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The first answer")
    );

    // One more letter: the run is cancelled on Dispatch and its stream let go.
    await user.keyboard("s");
    await waitFor(() => expect(cancelGoingRequest.mock.calls.map(([id]) => id)).toEqual(["run-1"]));
    expect(first?.signal.aborted).toBe(true);

    // And the pause after it asks the new words, once.
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    expect(createGoingRequest.mock.calls.map(([input]) => input.words)).toEqual([
      "astrolabe",
      "astrolabes",
    ]);
    send(streams[1], { data: { text: "The second answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The second answer")
    );
    // A frame the let-go stream still delivers reaches no panel, the new run's included.
    send(first, { data: { text: " and more" }, event: "text" });
    await tick(0);
    expect(screen.getByTestId("going-answer-text").textContent).toBe("The second answer");
  } finally {
    bar.unmount();
  }
});

test("one more letter keeps the answer on the screen as the earlier words', with its link and lines, until the next answer's first words replace it", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    const first = streams[0];
    send(
      first,
      { data: { text: "The first answer" }, event: "text" },
      {
        data: { href: "/issues/LEGION-3", id: "LEGION-3", kind: "issue", title: "Instruments" },
        event: "item",
      },
      { data: { age_ms: 4200, source: "live" }, event: "roster" }
    );
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-item")).toHaveAttribute("href", "/issues/LEGION-3")
    );
    // The text does not write the issue's key, so its chip stands beneath the text, in place of
    // a plain link of its title.
    await waitFor(() =>
      expect(
        within(screen.getByTestId("going-answer-item")).getByTestId("item-chip")
      ).toHaveTextContent("LEGION-3TodoInstruments")
    );

    // One more letter: the run is cancelled on Dispatch and its stream let go, as ever, and the
    // panel keeps what it drew of it under a status naming the words it answers.
    await user.keyboard("s");
    await waitFor(() => expect(cancelGoingRequest.mock.calls.map(([id]) => id)).toEqual(["run-1"]));
    expect(first?.signal.aborted).toBe(true);
    const panel = screen.getByTestId("going-answer");
    expect(panel).toHaveAttribute("data-state", "stale");
    expect(panel).toHaveAttribute("aria-busy", "false");
    await expectKeptFor("astrolabe");
    expect(within(panel).getByTestId("going-answer-stale")).toBeInTheDocument();
    expect(within(panel).getByTestId("going-answer-text").textContent).toBe("The first answer");
    expect([...panel.querySelectorAll("a")].map((link) => link.getAttribute("href"))).toEqual([
      "/issues/LEGION-3",
    ]);
    expect(within(panel).getByTestId("going-answer-roster")).toHaveTextContent("roster 4 s old");
    // A frame the let-go stream still delivers changes nothing of what the panel keeps.
    send(first, { data: { text: " and more" }, event: "text" });
    await tick(0);
    expect(screen.getByTestId("going-answer-text").textContent).toBe("The first answer");

    // Through the pause, the next question and its first frame, which is no text, it stays.
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(streams[1], { data: { action: "none", context: "none", line: "ask" }, event: "decision" });
    await tick(0);
    expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "stale");
    expect(screen.getByTestId("going-answer-text").textContent).toBe("The first answer");

    // The next answer's first words take its place, and nothing of the kept answer stays.
    send(streams[1], { data: { text: "The second answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text").textContent).toBe("The second answer")
    );
    expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "streaming");
    expect(screen.queryByTestId("going-answer-stale")).not.toBeInTheDocument();
    expect(screen.queryByTestId("going-answer-item")).not.toBeInTheDocument();
    expect(screen.queryByTestId("going-answer-roster")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

// What the reader did with an answer's block is that answer's (class search, entry 71). A table
// past 8 rows shows its first rows and "Show all", which opens it in place here. The next
// answer's table is another block in the same place of the panel, and it starts at its own first
// rows rather than opened by a press made on the table before it.
test("Show all on an answer's table is not carried over to the next answer's table", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();
  const table = (count: number): GoingEvent => ({
    data: {
      kind: "table",
      rows: Array.from({ length: count }, (_, index) => ({ issues: index + 1, status: "todo" })),
      spec: { columns: ["status", "issues"], params: {}, source: "run", table: "chain" },
      title: "LEGION issues by status",
    },
    event: "block",
  });
  /** The rows the panel's table draws under its header. */
  const rows = (): number =>
    within(screen.getByTestId("going-block-table")).getAllByRole("row").length - 1;

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(streams[0], { data: { text: "The first answer" }, event: "text" }, table(10));
    await waitFor(() => expect(rows()).toBe(TABLE_ROWS));
    await user.click(screen.getByTestId("going-block-show-all"));
    expect(rows()).toBe(10);
    expect(screen.queryByTestId("going-block-show-all")).toBeNull();

    // One more letter, and its answer with a table of its own.
    await user.click(screen.getByRole("combobox", { name: "Search" }));
    await user.keyboard("s");
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(streams[1], { data: { text: "The second answer" }, event: "text" }, table(12));
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text").textContent).toBe("The second answer")
    );
    expect(rows()).toBe(TABLE_ROWS);
    expect(screen.getByTestId("going-block-show-all")).toHaveTextContent("Show all 12 ›");
  } finally {
    bar.unmount();
  }
});

test("the render the next letter makes already draws the answer typed past, marked as the earlier words', before any useEffect of that render runs", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  // What each render of the bar showed as it reached the page, before any of its useEffects ran:
  // the words in the box, and the panel's state, text and mark.
  const commits: { readonly box: string; readonly panel: string }[] = [];
  const bar = renderBar(() => {
    const box = document.querySelector<HTMLInputElement>('input[role="combobox"]');
    const panel = document.querySelector('[data-testid="going-answer"]');
    const text = panel?.querySelector('[data-testid="going-answer-text"]')?.textContent ?? "";
    const mark = panel?.querySelector('[data-testid="going-answer-stale"]') ?? null;
    commits.push({
      box: box?.value ?? "",
      panel:
        panel === null
          ? "no panel"
          : `${panel.getAttribute("data-state")}: ${text}${mark === null ? "" : " (marked)"}`,
    });
  });

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(streams[0], { data: { text: "The first answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The first answer")
    );

    await user.keyboard("s");
    await expectKeptFor("astrolabe");

    // From the first render with the new words in the box on, the panel drew the answer to the
    // words before, marked as theirs: no render left it out, emptied it or drew it unmarked.
    const typedPast = commits.filter((commit) => commit.box === "astrolabes");
    expect(typedPast.length).toBeGreaterThan(0);
    expect([...new Set(typedPast.map((commit) => commit.panel))]).toEqual([
      "stale: The first answer (marked)",
    ]);
  } finally {
    bar.unmount();
  }
});

test("a kept answer cut off before it finished says so beside the words it answers, and one that finished says nothing of the kind", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();
  const stopped = /stopped before it finished/;

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(
      streams[0],
      { data: { text: "The first answer" }, event: "text" },
      { data: { items: 0, words: 3 }, event: "end" }
    );
    await waitFor(() =>
      expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "ended")
    );

    // Typed past once it ended: the whole answer is kept, and its line says no more than whose.
    await user.keyboard("s");
    await expectKeptFor("astrolabe");
    expect(screen.getByTestId("going-answer-stale")).not.toHaveTextContent(stopped);

    // The next answer, typed past while it was still being written: what it drew is kept, and
    // its line says it stopped there.
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(streams[1], { data: { text: "The second" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The second")
    );
    await user.keyboard("e");
    await expectKeptFor("astrolabes");
    expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The second");
    expect(screen.getByTestId("going-answer-stale")).toHaveTextContent(stopped);
  } finally {
    bar.unmount();
  }
});

test("a next run that finds no answer or fails takes the kept answer's place, and one with no words of its own leaves nothing to keep", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(
      streams[0],
      { data: { text: "The first answer" }, event: "text" },
      { data: { items: 0, words: 3 }, event: "end" }
    );
    await waitFor(() =>
      expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "ended")
    );
    await user.keyboard("s");
    await expectKeptFor("astrolabe");

    // The model writes no answer to the new words: the panel says so beside the hits, and the
    // earlier words' answer is gone.
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(streams[1], { data: {}, event: "none" });
    await screen.findByTestId("going-answer-none");
    expect(screen.queryByTestId("going-answer-stale")).not.toBeInTheDocument();
    expect(screen.queryByTestId("going-answer-text")).not.toBeInTheDocument();
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0);
    send(streams[1], { data: { items: 0, words: 0 }, event: "end" });

    // Typed past an answer that had no words, the panel has nothing to keep.
    await type(user, "e");
    expect(screen.queryByTestId("going-answer")).not.toBeInTheDocument();

    // And a failure takes the kept answer's place as a none does.
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(3));
    send(streams[2], { data: { text: "The third answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The third answer")
    );
    await user.keyboard("r");
    await expectKeptFor("astrolabese");
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(4));
    send(streams[3], { data: { code: GOING_ERROR_ANSWER_FAILED }, event: "error" });
    expect(await screen.findByTestId("going-answer-error")).toHaveAttribute("role", "alert");
    expect(screen.queryByTestId("going-answer-stale")).not.toBeInTheDocument();
    expect(screen.queryByTestId("going-answer-text")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

test("emptying the box lets the kept answer go for good, and so does closing the bar", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();
  const input = () => screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(streams[0], { data: { text: "The first answer" }, event: "text" });
    await user.keyboard("s");
    await expectKeptFor("astrolabe");

    // Emptied: the panel is gone, and typing anew brings nothing of the kept answer back.
    await user.clear(input());
    expect(screen.queryByTestId("going-answer")).not.toBeInTheDocument();
    await type(user, "sextant");
    expect(screen.queryByTestId("going-answer")).not.toBeInTheDocument();

    // An answer to these words, typed past and kept, then the bar closed.
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(
      streams[1],
      { data: { text: "The sextant answer" }, event: "text" },
      { data: { items: 0, words: 3 }, event: "end" }
    );
    await waitFor(() =>
      expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "ended")
    );
    await user.keyboard("s");
    await expectKeptFor("sextant");
  } finally {
    bar.unmount();
  }

  // Opened again with an empty box: the bar draws the last answered run again, as ever, and
  // nothing marks it as typed past.
  const reopened = renderBar();
  try {
    await waitFor(() => expect(streams).toHaveLength(3));
    expect(await screen.findByTestId("going-answer-replayed")).toHaveTextContent("sextant");
    expect(screen.queryByTestId("going-answer-stale")).not.toBeInTheDocument();
  } finally {
    reopened.unmount();
  }
});

/** Asks `astrolabe`, draws its first words, and types one more letter: the panel keeps that
 *  answer as the earlier words' while the next run has nothing of its own. */
async function keepAnAnswer(user: UserEvent): Promise<void> {
  await type(user, "astrolabe");
  await tick(pauseRest);
  await waitFor(() => expect(streams).toHaveLength(1));
  send(streams[0], { data: { text: "The first answer" }, event: "text" });
  await waitFor(() =>
    expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The first answer")
  );
  await user.keyboard("s");
  await expectKeptFor("astrolabe");
}

test("the project chip asking the same words of another project lets the kept answer go", async () => {
  await warmMarkdownRenderer();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-10T00:00:00Z",
      gated_steps: [],
      key: "DEMO",
      name: "Demo",
      open_asks: 0,
      proven_sessions: false,
    },
  ]);
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await keepAnAnswer(user);

    await user.click(screen.getByRole("button", { name: "Filters" }));
    await user.click(screen.getByRole("button", { name: "Project" }));
    await user.click(await screen.findByRole("button", { name: "DEMO" }));
    await screen.findByRole("button", { name: "Remove Project DEMO" });
    expect(screen.queryByTestId("going-answer-stale")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
    listProjects.mockRestore();
  }
});

test("the bar no longer answering, with the same words in the box, lets the kept answer go", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await keepAnAnswer(user);

    // This Dispatch stops answering, and the bar reads so as the tab comes back, a minute on.
    goingCapability.mockResolvedValue(UNCONFIGURED_CAPABILITY);
    jest.setSystemTime(Date.now() + 60_000);
    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await screen.findByTestId("going-cannot-answer");
    expect(screen.queryByTestId("going-answer-stale")).not.toBeInTheDocument();
  } finally {
    focusManager.setFocused(undefined);
    bar.unmount();
  }
});

test("closing the bar cancels the run out, and a run that ended is never cancelled", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  await type(user, "astrolabe");
  await tick(pauseRest);
  await waitFor(() => expect(streams).toHaveLength(1));
  bar.unmount();
  await waitFor(() => expect(cancelGoingRequest.mock.calls.map(([id]) => id)).toEqual(["run-1"]));

  const again = renderBar();
  try {
    await type(user, "sextant");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(streams[1], { data: { items: 0, words: 2 }, event: "end" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "ended")
    );
  } finally {
    again.unmount();
  }
  expect(cancelGoingRequest.mock.calls.map(([id]) => id)).toEqual(["run-1"]);
});

test("each frame of the stream is drawn as its own line, and the item frames are the only links, an issue's key in the text its chip", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(
      streams[0],
      {
        // As the server sends it: every punctuation byte escaped, and no word a renderer would
        // link on its own (the server drops those).
        data: {
          text: "See LEGION\\-3, \\[x\\]\\(\\/issues\\/LEGION\\-9\\), \\!\\[pic\\]\\(\\/p\\.png\\) and \\<b\\>bold\\<\\/b\\>",
        },
        event: "text",
      },
      {
        data: { href: "/issues/LEGION-3", id: "LEGION-3", kind: "issue", title: "Instruments" },
        event: "item",
      },
      { data: { words: 120 }, event: "cut" },
      { data: { dropped: 1 }, event: "changed" },
      { data: { age_ms: 4200, source: "live" }, event: "roster" },
      { data: { items: 1, words: 120 }, event: "end" }
    );
    // The item frame's key makes the text read that issue's summary, whose answer draws the text
    // again with the key as the issue's chip: it is let answer before anything is read.
    await tick(0);
    const text = await screen.findByTestId("going-answer-text");
    // The key an item frame names is that issue's chip, in the text where the text writes it, as
    // a run's answer draws an issue it links (design v3, state 4); the panel lists it no more.
    await waitFor(() =>
      expect(within(text).getByTestId("item-chip")).toHaveTextContent("LEGION-3TodoInstruments")
    );
    const chip = within(text).getByTestId("item-chip");
    expect(text).toHaveTextContent(
      "See LEGION-3TodoInstruments, [x](/issues/LEGION-9), ![pic](/p.png) and <b>bold</b>"
    );
    expect(chip.closest("a")).toHaveAttribute("href", "/issues/LEGION-3");
    expect(screen.queryByTestId("going-answer-items")).toBeNull();
    // The answer carries the ✦ the line on top marks the agent's words with (design v3, state 1),
    // hidden from a screen reader, which hears the answer's words alone.
    const mark = screen.getByTestId("going-answer-mark");
    expect(mark).toHaveTextContent("✦");
    expect(mark).toHaveAttribute("aria-hidden", "true");
    expect(mark.nextElementSibling).toBe(text);
    // Nothing the model wrote is a link, an image or a tag: the one link is the item frame's.
    expect(text.querySelector("img, b")).toBeNull();
    const links = screen.getByTestId("going-answer").querySelectorAll("a");
    expect([...links].map((link) => link.getAttribute("href"))).toEqual(["/issues/LEGION-3"]);
    expect(screen.getByTestId("going-answer-cut")).toHaveTextContent("answer cut to 120 words");
    expect(screen.getByTestId("going-answer-changed")).toHaveTextContent(
      "the answer changed: a link was dropped"
    );
    expect(screen.getByTestId("going-answer-roster")).toHaveTextContent("roster 4 s old");
    expect(screen.queryByTestId("going-answer-no-source")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

// The real model's short answer named LEGION-3 twice - the issue, then "the spec of LEGION-3" -
// and cited the issue and its spec, which the server names by the issue's title: the panel drew
// the chip twice and a stray link of the issue's title beneath. The chip is drawn once, the
// second mention stays the key, and the spec of an issue the text already draws is no link of
// its own.
test("an issue the short answer names twice is its chip once, and its spec draws no second link of its title", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(
      streams[0],
      { data: { text: "LEGION\\-3 is mapped; the spec of LEGION\\-3 says why\\." }, event: "text" },
      {
        data: { href: "/issues/LEGION-3", id: "LEGION-3", kind: "issue", title: "Instruments" },
        event: "item",
      },
      {
        data: {
          href: "/issues/LEGION-3/spec",
          id: "spec-3",
          kind: "document",
          title: "Instruments",
        },
        event: "item",
      },
      { data: { items: 2, words: 9 }, event: "end" }
    );
    await tick(0);
    const text = await screen.findByTestId("going-answer-text");
    await waitFor(() =>
      expect(within(text).getByTestId("item-chip")).toHaveTextContent("LEGION-3TodoInstruments")
    );
    expect(within(text).getAllByTestId("item-chip")).toHaveLength(1);
    expect(text).toHaveTextContent(
      "LEGION-3TodoInstruments is mapped; the spec of LEGION-3 says why."
    );
    expect(screen.queryByTestId("going-answer-items")).toBeNull();
    const links = screen.getByTestId("going-answer").querySelectorAll("a");
    expect([...links].map((link) => link.getAttribute("href"))).toEqual([
      "/issues/LEGION-3",
      "/issues/LEGION-3",
    ]);
  } finally {
    bar.unmount();
  }
});

// The server's filter passes on a time the model wrote as a link to its instant, its words
// escaped as every word is: the short answer draws it as that time, the full date on hover, and
// no link.
test("a time the short answer writes as a link to its instant draws as that time, with no link", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(
      streams[0],
      {
        data: { text: "It moved [5 days ago](dispatch-time:2026-11-18T11:35:00Z)\\." },
        event: "text",
      },
      { data: { items: 0, words: 4 }, event: "end" }
    );
    await tick(0);
    const text = await screen.findByTestId("going-answer-text");
    await waitFor(() => expect(text.querySelector("time")).not.toBeNull());
    const time = text.querySelector("time");
    expect(time?.textContent).toBe("5 days ago");
    expect(time?.getAttribute("datetime")).toBe("2026-11-18T11:35:00Z");
    expect(time?.getAttribute("title")).toBe(new Date("2026-11-18T11:35:00Z").toLocaleString());
    expect(text.querySelector("a")).toBeNull();
    expect(text).toHaveTextContent("It moved 5 days ago.");
  } finally {
    bar.unmount();
  }
});

test("no answer text says none was found, and an answer citing nothing says it rests on no source", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(streams[0], { data: {}, event: "none" }, { data: { items: 0, words: 0 }, event: "end" });
    await screen.findByTestId("going-answer-none");
    expect(screen.queryByTestId("going-answer-no-source")).not.toBeInTheDocument();
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0);

    await user.keyboard("s");
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(
      streams[1],
      { data: { text: "Plain words" }, event: "text" },
      { data: { age_ms: 0, source: "stored" }, event: "roster" },
      { data: { items: 0, words: 2 }, event: "end" }
    );
    expect(await screen.findByTestId("going-answer-no-source")).toHaveTextContent(
      /^This answer rests on no source\.$/u
    );
    expect(screen.getByTestId("going-answer-roster")).toHaveTextContent("live roster unavailable");
    expect(screen.queryByTestId("going-answer-none")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

test("reopening the bar draws the last answered run again from its stream's first frame", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  await type(user, "astrolabe");
  await tick(pauseRest);
  await waitFor(() => expect(streams).toHaveLength(1));
  send(streams[0], { data: { items: 0, words: 1 }, event: "end" });
  await waitFor(() =>
    expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "ended")
  );
  bar.unmount();

  const reopened = renderBar();
  try {
    await waitFor(() => expect(streams).toHaveLength(2));
    expect(streams[1]?.id).toBe("run-1");
    expect(createGoingRequest).toHaveBeenCalledTimes(1);
    expect(await screen.findByTestId("going-answer-replayed")).toHaveTextContent("astrolabe");

    // Typing lets the replay go: it is no answer to the new words, and it is not cancelled,
    // since its run ended long ago.
    await type(user, "sextant");
    expect(screen.queryByTestId("going-answer-replayed")).not.toBeInTheDocument();
    expect(streams[1]?.signal.aborted).toBe(true);
    expect(cancelGoingRequest).not.toHaveBeenCalled();
  } finally {
    reopened.unmount();
  }
});

// On a Dispatch with no answering model (design v3, state 13), the one sentence that says so
// stands in the line on top's place, the hits are as usual, and nothing else is said of how the
// Dispatch is set up (rule 8).
test("a Dispatch that does not answer asks nothing, lists no line, and says so once in the line's place", async () => {
  const user = userOnFakeClock();
  goingCapability.mockResolvedValue(UNCONFIGURED_CAPABILITY);
  const bar = renderBar();
  try {
    await type(user, "astrolabe");
    await tick(MEANING_PAUSE_MS);
    const sentence = await screen.findByTestId("going-cannot-answer");
    expect(sentence).toHaveTextContent(
      "Answers are off on this Dispatch: it has no answering model."
    );
    expect(screen.getAllByTestId("going-cannot-answer")).toHaveLength(1);
    expect(document.getElementById("search-option-line")).toBeNull();
    expect(screen.getAllByRole("option")).toHaveLength(1);
    for (const id of [
      "going-capability",
      "going-cannot-decide",
      "going-no-session",
      "going-no-label",
      "going-no-blocks",
    ]) {
      expect(screen.queryByTestId(id)).toBeNull();
    }
    expect(createGoingRequest).not.toHaveBeenCalled();
  } finally {
    bar.unmount();
  }
});

// The short answer (R3; C1 of the answer-surface mockups) is a compact card above the hits, never
// in the list: its text shows at most 3 lines, whatever its length, and its one button, Open in
// panel, is a button of its own outside the listbox and every option of it, so no option holds a
// control. Nothing about the answer's length picks this layout or the button.
test("the short answer is a card above the hits, its text at most 3 lines, with an Open in panel button outside the list", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(
      streams[0],
      { data: { text: "The first answer" }, event: "text" },
      { data: { items: 0, words: 3 }, event: "end" }
    );
    const text = await screen.findByTestId("going-answer-text");
    await waitFor(() => expect(text).toHaveTextContent("The first answer"));
    expect(text.className).toContain("line-clamp-3");
    const card = screen.getByTestId("going-answer");
    const list = screen.getByRole("listbox");
    // Above the hits, never among them.
    expect(card.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
    expect(list.contains(card)).toBe(false);
    const open = within(card).getByRole("button", { name: "Open in panel" });
    expect(list.contains(open)).toBe(false);
    expect(screen.getAllByRole("option").some((option) => option.contains(open))).toBe(false);
    // The card says nothing of Enter: the line Ask has its own key.
    expect(screen.queryByTestId("going-answer-full")).toBeNull();

    // Moving the highlight changes nothing of the card.
    await user.keyboard("{ArrowDown}");
    expect(within(card).getByRole("button", { name: "Open in panel" })).toBe(open);
  } finally {
    bar.unmount();
  }
});

// An answer to words the person has typed past is no question any more: its card, drawn as it
// stood, offers no button, so an old preview cannot submit an old query. The card of the words in
// the box now does, and its button is the line Ask's Enter: one run, of the words typed now, with
// the line "ask", and the conversation handed to the panel at the side, one run for a double press
// too. Receiving the answer started no run and opened no panel.
test("Open in panel is offered only on the answer to the words in the box, and submits those words once", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const start = spyOn(api, "createGoingRun").mockResolvedValue({
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: new Date().toISOString(),
    finished_at: null,
    id: "r-1",
    issues: [],
    level: "standard",
    line: "ask",
    mode: "ask",
    standing_count: 0,
    state: "queued",
    stop_reason: null,
    undoable: false,
    undoable_count: 0,
    undone_at: null,
    undone_count: 0,
    used: { calls: 0, tokens: 0, working_ms: 0 },
    words: "astrolabes",
  });
  const follow = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => Promise.withResolvers<void>().promise
  );
  const bar = renderBar();
  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    send(streams[0], { data: { text: "The first answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Open in panel" })).toBeInTheDocument()
    );
    // The answer arrived: nothing started and nothing opened.
    expect(start).not.toHaveBeenCalled();
    expect(goingState().place).not.toBe("side");

    // One more letter makes that answer the earlier words': it stays drawn, and offers no button.
    await user.keyboard("s");
    await expectKeptFor("astrolabe");
    expect(screen.queryByRole("button", { name: "Open in panel" })).toBeNull();
    expect(start).not.toHaveBeenCalled();

    // The answer to the words now brings the button back, and a double press of it submits those
    // words once. Both presses reach the button's handler before the bar draws again, so the
    // second runs against the store the first left, its start out, and starts nothing.
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    send(streams[1], { data: { text: "The second answer" }, event: "text" });
    const open = await screen.findByRole("button", { name: "Open in panel" });
    await act(async () => {
      open.click();
      open.click();
    });
    expect(start).toHaveBeenCalledTimes(1);
    expect(start.mock.calls[0]?.[0]).toMatchObject({
      level: "standard",
      line: "ask",
      words: "astrolabes",
    });
    expect(start.mock.calls[0]?.[0]).not.toHaveProperty("continues");
    expect(goingState().place).toBe("side");
  } finally {
    bar.unmount();
    follow.mockRestore();
    start.mockRestore();
  }
});

// A failed short answer says why in one sentence, in the words a run's same failure says, and
// its one next step, the full answer; never its code (design v3, rules 1 and 7). A Dispatch with
// no answering model says the one sentence of state 13, and offers no step. A withdrawn answer -
// one that read a comment or an issue message deleted since - says so in a withdrawn run's words.
test("a failed short answer says why in words with its next step, and never its code", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  const failures: readonly (readonly [string, string])[] = [
    [GOING_ERROR_ANSWER_FAILED, "Dispatch could not reach the model. Ask for the full answer."],
    [
      GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
      "The model took too long to answer. Ask for the full answer.",
    ],
    [
      GOING_ERROR_NO_MODEL_CONFIGURED,
      "Answers are off on this Dispatch: it has no answering model.",
    ],
    [
      GOING_ERROR_ANSWER_WITHDRAWN,
      "Dispatch withdrew this answer because a comment or message it read was deleted. Ask for the full answer.",
    ],
    ["A_LATER_CODE", "Dispatch could not write a short answer. Ask for the full answer."],
  ];
  try {
    await type(user, "astrolabe");
    for (const [index, [code, words]] of failures.entries()) {
      if (index > 0) await user.keyboard("s");
      await tick(index === 0 ? pauseRest : MEANING_PAUSE_MS);
      await waitFor(() => expect(streams).toHaveLength(index + 1));
      send(streams[index], { data: { code }, event: "error" });
      await waitFor(() => expect(screen.getByTestId("going-answer-error").textContent).toBe(words));
      expect(screen.getByTestId("going-answer").textContent).not.toContain(code);
      // The line Ask is highlighted, and the failure says the step: the foot does not again.
      expect(screen.getByRole("combobox", { name: "Search" })).toHaveAttribute(
        "aria-activedescendant",
        "search-option-line"
      );
      // The failure's one next step, the full answer, is the button, but for a Dispatch with no
      // answering model, which says its sentence and offers none.
      expect(screen.queryByRole("button", { name: "Open in panel" }) === null).toBe(
        code === GOING_ERROR_NO_MODEL_CONFIGURED
      );
    }
  } finally {
    bar.unmount();
  }
});
