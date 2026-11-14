import { afterEach, beforeEach, expect, jest, type Mock, test } from "bun:test";
import { screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { UNCONFIGURED_CAPABILITY } from "../../__tests__/network";
import { userOnFakeClock } from "../../__tests__/user";
import type { api } from "../../api/client";
import type { GoingEvent } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
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
// drawn as its line, and the last answer drawn again as the bar reopens. The clock is held, as in
// SearchPalette.test.tsx, so which of the bar's timers has fired is the row's to decide.

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

const pauseRest = MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS;

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
    first?.send({ data: { text: "The first answer" }, event: "text" });
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
    streams[1]?.send({ data: { text: "The second answer" }, event: "text" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer-text")).toHaveTextContent("The second answer")
    );
    // A frame the let-go stream still delivers reaches no panel, the new run's included.
    first?.send({ data: { text: " and more" }, event: "text" });
    await tick(0);
    expect(screen.getByTestId("going-answer-text").textContent).toBe("The second answer");
  } finally {
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
    streams[1]?.send({ data: { items: 0, words: 2 }, event: "end" });
    await waitFor(() =>
      expect(screen.getByTestId("going-answer")).toHaveAttribute("data-state", "ended")
    );
  } finally {
    again.unmount();
  }
  expect(cancelGoingRequest.mock.calls.map(([id]) => id)).toEqual(["run-1"]);
});

test("each frame of the stream is drawn as its own line, and the item frames are the only links", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    const stream = streams[0];
    for (const frame of [
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
      { data: { items: 1, words: 120 }, event: "end" },
    ] satisfies GoingEvent[]) {
      stream?.send(frame);
    }
    const text = await screen.findByTestId("going-answer-text");
    await waitFor(() =>
      expect(text).toHaveTextContent(
        "See LEGION-3, [x](/issues/LEGION-9), ![pic](/p.png) and <b>bold</b>"
      )
    );
    // Not a link, an image or a tag, and not a reference to the issue key either.
    expect(text.querySelector("a, img, b")).toBeNull();
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

test("no answer text says none was found, and an answer citing nothing says it rests on no source", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();

  try {
    await type(user, "astrolabe");
    await tick(pauseRest);
    await waitFor(() => expect(streams).toHaveLength(1));
    streams[0]?.send({ data: {}, event: "none" });
    streams[0]?.send({ data: { items: 0, words: 0 }, event: "end" });
    await screen.findByTestId("going-answer-none");
    expect(screen.queryByTestId("going-answer-no-source")).not.toBeInTheDocument();
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0);

    await user.keyboard("s");
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(streams).toHaveLength(2));
    streams[1]?.send({ data: { text: "Plain words" }, event: "text" });
    streams[1]?.send({ data: { age_ms: 0, source: "stored" }, event: "roster" });
    streams[1]?.send({ data: { items: 0, words: 2 }, event: "end" });
    await screen.findByTestId("going-answer-no-source");
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
  streams[0]?.send({ data: { items: 0, words: 1 }, event: "end" });
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

test("a Dispatch that does not answer asks nothing and says what the bar cannot do", async () => {
  const user = userOnFakeClock();
  goingCapability.mockResolvedValue(UNCONFIGURED_CAPABILITY);
  const bar = renderBar();
  try {
    await type(user, "astrolabe");
    await tick(MEANING_PAUSE_MS);
    for (const id of [
      "going-cannot-answer",
      "going-cannot-decide",
      "going-no-session",
      "going-no-label",
    ]) {
      expect(await screen.findByTestId(id)).toBeInTheDocument();
    }
    expect(createGoingRequest).not.toHaveBeenCalled();
  } finally {
    bar.unmount();
  }
});
