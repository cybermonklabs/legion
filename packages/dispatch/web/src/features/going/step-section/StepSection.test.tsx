import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { onFakeClock, tick } from "../../../__tests__/fake-clock";
import { dangerText, textMutedOnSurface, textSecondaryOnSurface } from "../../../theme/classes";
import type { RunStep } from "../run-frames";
import { elapsedTimeWords, foldWords, pastHeader, StepSection } from "./StepSection";

// A run's steps as the bar draws them (design v2, states 3 and 4): one line for each call, the
// running ones in the present tense with a shimmer and the ended ones in the past tense, the
// newest line with the run's timer; past three calls at once, "and 2 more…"; at most three
// finished lines, two or more older ones folded into one line that counts them and opens on a
// click; the model's prose as a grey header above its calls; keys as
// plain links; and once the run no longer works, every step folded into "Worked for 1 min 48 s ·
// read 14 items, searched twice ›".

afterEach(cleanup);

/** An ended read of `DEMO-<id>`, as a tool frame leaves it, with `fields` over it. */
function step(id: number, fields: Partial<RunStep> = {}): RunStep {
  return {
    count: 0,
    header: "",
    id,
    items: [`DEMO-${id + 1}`],
    kind: "read",
    label: `Read DEMO-${id + 1}`,
    refused: false,
    running: false,
    slip: false,
    step: id + 2,
    tool: "read",
    ...fields,
  };
}

/** A call of `id` that runs, as its call frame starts it. */
function running(id: number, label: string): RunStep {
  return step(id, { items: [], label, running: true });
}

/** The words of each step line drawn, without the timer. */
function labels(): readonly (string | null)[] {
  return screen
    .queryAllByTestId("going-step")
    .map((line) => within(line).getByTestId("going-step-label").textContent);
}

/** The one line `steps` fold into once their run ended, having worked for `workedMs`, drawn on
 *  its own in place of whatever was drawn before. */
function foldedLine(steps: readonly RunStep[], workedMs: number | undefined): HTMLElement {
  cleanup();
  render(<StepSection folded live={false} since={Date.now()} steps={steps} workedMs={workedMs} />);
  return screen.getByTestId("going-steps-fold");
}

test("an elapsed time is said in its smallest useful units", () => {
  expect(elapsedTimeWords(0)).toBe("0 s");
  expect(elapsedTimeWords(999)).toBe("0 s");
  expect(elapsedTimeWords(-5_000)).toBe("0 s");
  expect(elapsedTimeWords(59_999)).toBe("59 s");
  expect(elapsedTimeWords(108_000)).toBe("1 min 48 s");
  expect(elapsedTimeWords(7_500_000)).toBe("2 h 5 min");
});

test("a fold says what its steps were made of: items read, how often it searched or looked, and what it drew", () => {
  const search = (id: number) => step(id, { kind: "search", tool: "search" });
  expect(foldWords([step(0), step(1, { count: 12 }), step(2)])).toBe("read 14 items");
  expect(foldWords([step(0)])).toBe("read 1 item");
  expect(foldWords([search(0)])).toBe("searched once");
  expect(foldWords([search(0), search(1)])).toBe("searched twice");
  expect(foldWords([search(0), search(1), search(2)])).toBe("searched 3 times");
  expect(foldWords([step(0, { kind: "look", tool: "find_tools" })])).toBe("looked for a way once");
  expect(foldWords([step(0, { kind: "chart", tool: "draw_block" })])).toBe("drew a chart");
  expect(
    foldWords([step(0, { kind: "chart", tool: "draw_block" }), step(1, { kind: "chart" })])
  ).toBe("drew 2 charts");
  // A step that drew a table or one number says so, as its kind does once its block came.
  expect(foldWords([step(0, { kind: "table", tool: "draw_block" })])).toBe("drew a table");
  expect(
    foldWords([step(0, { kind: "table", tool: "draw_block" }), step(1, { kind: "table" })])
  ).toBe("drew 2 tables");
  expect(foldWords([step(0, { kind: "metric", tool: "draw_block" })])).toBe("showed a number");
  expect(
    foldWords([
      step(0),
      step(1, { kind: "chart", tool: "draw_block" }),
      step(2, { kind: "table", tool: "draw_block" }),
    ])
  ).toBe("read 1 item, drew a chart, drew a table");
  // Each kind in one order, joined with ", "; a failed call counts as failed, not as its kind.
  expect(
    foldWords([
      search(0),
      step(1, { count: 3 }),
      search(2),
      step(3, { refused: true }),
      step(4, { kind: "", tool: "call_tool" }),
    ])
  ).toBe("read 3 items, searched twice, 1 other step, 1 step failed");
  expect(foldedLine([step(0), search(1), search(2)], 108_000)).toHaveAccessibleName(
    "Worked for 1 min 48 s · read 1 item, searched twice"
  );
  // A run that worked for less than a second, or for a time the page does not know, says none.
  expect(foldedLine([step(0)], 400)).toHaveAccessibleName("Read 1 item");
  expect(foldedLine([step(0)], undefined)).toHaveAccessibleName("Read 1 item");
});

test("a running call's line is its present words with a shimmer, and the newest line carries the run's timer", async () => {
  await onFakeClock(async () => {
    render(
      <StepSection
        folded={false}
        live
        since={Date.now()}
        steps={[step(14), running(15, "Reading the comments of DEMO-7")]}
        workedMs={undefined}
      />
    );
    expect(labels()).toEqual(["Read DEMO-15", "Reading the comments of DEMO-7…"]);
    const [ended, live] = screen.getAllByTestId("going-step");
    expect(live?.getAttribute("data-running")).toBe("true");
    expect(within(live as HTMLElement).getByTestId("going-step-label").className).toContain(
      "motion-safe:animate-pulse"
    );
    expect(within(ended as HTMLElement).getByTestId("going-step-label").className).not.toContain(
      "animate-pulse"
    );
    // One timer, on the newest line, counting from the person's question.
    expect(screen.getAllByTestId("going-step-timer")).toHaveLength(1);
    const timer = within(live as HTMLElement).getByTestId("going-step-timer");
    expect(timer.textContent).toBe("0 s");
    await tick(2000);
    expect(timer.textContent).toBe("2 s");
  });
});

// A run that works with no call running - its model writes, or decides what to call next - reads
// as working, not idle (design v3, state 3): its lines end with "Thinking…" and the run's timer,
// which no step line then carries; the header over calls that all ended goes past tense.
test("a working run with no call running ends with Thinking and the timer, not a timer on an ended line", async () => {
  await onFakeClock(async () => {
    render(
      <StepSection
        folded={false}
        live
        since={Date.now()}
        steps={[
          step(14, { header: "Reading the telescope's issues…" }),
          step(15, { header: "Reading the telescope's issues…" }),
        ]}
        workedMs={undefined}
      />
    );
    expect(screen.queryByTestId("going-step-timer")).toBeNull();
    const thinking = screen.getByTestId("going-steps-thinking");
    expect(thinking).toHaveTextContent("Thinking…");
    const timer = within(thinking).getByTestId("going-thinking-timer");
    expect(timer.textContent).toBe("0 s");
    await tick(3000);
    expect(timer.textContent).toBe("3 s");
    expect(screen.getByTestId("going-step-words").textContent).toBe("Read the telescope's issues");
    // Each ended line reads darker than the header, at 13 px.
    for (const line of screen.getAllByTestId("going-step")) {
      expect(line.className).toContain("text-[13px]");
      expect(line.className).toContain(textSecondaryOnSurface);
    }
  });
});

test("while a call runs, its header keeps the words the model wrote, and no Thinking line shows", () => {
  render(
    <StepSection
      folded={false}
      live
      since={Date.now()}
      steps={[
        step(14, { header: "Reading…" }),
        { ...running(15, "Reading DEMO-7"), header: "Reading…" },
      ]}
      workedMs={undefined}
    />
  );
  expect(screen.getByTestId("going-step-words").textContent).toBe("Reading…");
  expect(screen.queryByTestId("going-steps-thinking")).toBeNull();
  expect(screen.getAllByTestId("going-step-timer")).toHaveLength(1);
});

// A header too long for its row is cut at the row's edge (`truncate`), so it keeps its whole
// words on hover: its title holds them as the page shows them, with a time the model
// wrote in the words the line draws it in.
test("a long header keeps its whole words in its title, a time in the words the page shows", () => {
  const words =
    "Reading every issue of the telescope's mount, its spec and its comments since 5 days ago, to find the one that orders the motor";
  const header = words.replace("5 days ago", "[5 days ago](dispatch-time:2026-11-20T11:35:00Z)");
  render(
    <StepSection
      folded={false}
      live
      since={Date.now()}
      steps={[{ ...running(3, "Reading DEMO-4"), header }]}
      workedMs={undefined}
    />
  );
  const line = screen.getByTestId("going-step-words");
  expect(line.className).toContain("truncate");
  expect(line).toHaveAttribute("title", words);
  expect(line.textContent).toBe(words);
});

// Rule 2: a step is in the present tense while it runs and in the past tense once it ends, and so
// is the header the model wrote over its calls (review M5): its first word, a verb in "-ing", takes
// its past form, regular or not, and a header that starts any other way keeps its words.
test("a header over calls that ended takes the past tense of its first verb, and keeps any other words", () => {
  expect(pastHeader("Reading the telescope's issues…")).toBe("Read the telescope's issues");
  expect(pastHeader("Checking the children of DEMO-1.")).toBe("Checked the children of DEMO-1.");
  expect(pastHeader("Comparing the two specs...")).toBe("Compared the two specs");
  expect(pastHeader("Planning the split…")).toBe("Planned the split");
  expect(pastHeader("Verifying the dates…")).toBe("Verified the dates");
  expect(pastHeader("Finding who holds it…")).toBe("Found who holds it");
  expect(pastHeader("Cross-checking the labels…")).toBe("Cross-checked the labels");
  // Words in "-ing" that are no verb, and headers that start with any other word, keep theirs.
  expect(pastHeader("Nothing else waits on it…")).toBe("Nothing else waits on it");
  expect(pastHeader("Let me read OPS-2 first.")).toBe("Let me read OPS-2 first.");
  expect(pastHeader("OPS-2 is reading as done…")).toBe("OPS-2 is reading as done");
});

test("calls that run at once each have a line, and past three the rest fold into and 2 more…", () => {
  render(
    <StepSection
      folded={false}
      live
      since={Date.now()}
      steps={[1, 2, 3, 4, 5].map((id) => running(id, `Reading DEMO-${id}`))}
      workedMs={undefined}
    />
  );
  expect(labels()).toEqual(["Reading DEMO-1…", "Reading DEMO-2…", "Reading DEMO-3…"]);
  const more = screen.getByTestId("going-steps-more");
  expect(within(more).getByText("and 2 more…")).toBeInTheDocument();
  // The fold is the newest line, so it carries the timer.
  expect(within(more).getByTestId("going-step-timer")).toBeInTheDocument();
  expect(screen.getAllByTestId("going-step-timer")).toHaveLength(1);
});

// The design shows at most three finished lines (review a7); the older ones fold into one grey
// line that says in plain words how many steps it holds (review N2), and opens them on a click.
test("at most three finished lines show, and two or more older ones fold into one line that counts them and a click opens", async () => {
  const user = userEvent.setup();
  render(
    <StepSection
      folded={false}
      live
      since={Date.now()}
      steps={[
        step(0, { count: 3, kind: "search", label: "Searched DEMO for “moons” · 3 hits" }),
        step(1),
        step(2),
        step(3),
        step(4),
        running(5, "Reading DEMO-6"),
      ]}
      workedMs={undefined}
    />
  );
  expect(labels()).toEqual(["Read DEMO-3", "Read DEMO-4", "Read DEMO-5", "Reading DEMO-6…"]);
  const older = screen.getByTestId("going-steps-older");
  expect(older.textContent).toBe("2 earlier steps›");
  expect(older).toHaveAccessibleName("2 earlier steps");
  expect(older.getAttribute("aria-expanded")).toBe("false");
  // The fold is the bar's one grey, as every hint and status line is.
  expect(older.className).toContain(textMutedOnSurface);

  await user.click(older);
  expect(older.getAttribute("aria-expanded")).toBe("true");
  expect(labels()).toEqual([
    "Searched DEMO for “moons” · 3 hits",
    "Read DEMO-2",
    "Read DEMO-3",
    "Read DEMO-4",
    "Read DEMO-5",
    "Reading DEMO-6…",
  ]);
  await user.click(older);
  expect(labels()).toHaveLength(4);
});

// A line folded into one line saves nothing (review N2): one finished step past the newest three
// shows as its own line, in its own words, and no fold line shows.
test("one finished step past the newest three shows as itself, and nothing folds", () => {
  const look = "Looked for a way to list an issue's messages";
  render(
    <StepSection
      folded={false}
      live
      since={Date.now()}
      steps={[
        step(0, { items: [], kind: "look", label: look, tool: "find_tools" }),
        step(1),
        step(2),
        step(3),
      ]}
      workedMs={undefined}
    />
  );
  expect(labels()).toEqual([look, "Read DEMO-2", "Read DEMO-3", "Read DEMO-4"]);
  expect(screen.queryByTestId("going-steps-older")).toBeNull();
});

test("the model's prose heads its calls as one grey line, and folds with them", async () => {
  const user = userEvent.setup();
  const first = "Reading the issue first.";
  const children = "Checking the children of DEMO-1.";
  render(
    <StepSection
      folded={false}
      live
      since={Date.now()}
      steps={[
        step(0, { header: first }),
        step(1, { header: first }),
        step(2, { header: children }),
        step(3, { header: children }),
        step(4, { header: children }),
        running(5, "Reading DEMO-6"),
      ]}
      workedMs={undefined}
    />
  );
  // The first two calls folded, and their header with them; the others stand under theirs, in the
  // past tense once its calls ended.
  expect(screen.getAllByTestId("going-step-words").map((line) => line.textContent)).toEqual([
    "Checked the children of DEMO-1.",
  ]);
  expect(labels()).toEqual(["Read DEMO-3", "Read DEMO-4", "Read DEMO-5", "Reading DEMO-6…"]);
  await user.click(screen.getByTestId("going-steps-older"));
  expect(screen.getAllByTestId("going-step-words").map((line) => line.textContent)).toEqual([
    "Read the issue first.",
    "Checked the children of DEMO-1.",
  ]);
});

test("keys in a step's line are plain links to their issues, and a failed call reads as its sentence", () => {
  render(
    <StepSection
      folded={false}
      live={false}
      since={Date.now()}
      steps={[
        step(0, { items: ["DEMO-15", "DEMO-7"], label: "Read DEMO-15 and its parent DEMO-7" }),
        step(1, {
          items: ["DEMO-12"],
          label: "Could not read DEMO-12: you have no access",
          refused: true,
        }),
      ]}
      workedMs={undefined}
    />
  );
  expect(screen.getByRole("link", { name: "DEMO-15" }).getAttribute("href")).toBe(
    "/issues/DEMO-15"
  );
  expect(screen.getByRole("link", { name: "DEMO-7" }).getAttribute("href")).toBe("/issues/DEMO-7");
  expect(screen.queryByTestId("item-chip")).toBeNull();
  expect(labels()).toEqual([
    "Read DEMO-15 and its parent DEMO-7",
    "Could not read DEMO-12: you have no access",
  ]);
  // A run that no longer works draws no timer.
  expect(screen.queryByTestId("going-step-timer")).toBeNull();
});

test("once the answer came, every step folds into Worked for… with what they were made of, and a click opens them", async () => {
  const user = userEvent.setup();
  const search = (id: number) => step(id, { kind: "search", tool: "search" });
  render(
    <StepSection
      folded
      live={false}
      since={Date.now()}
      steps={[search(0), step(1, { count: 12 }), search(2), step(3), step(4)]}
      workedMs={108_000}
    />
  );
  const fold = screen.getByTestId("going-steps-fold");
  expect(fold.textContent).toBe("Worked for 1 min 48 s · read 14 items, searched twice›");
  // The time stands in an element of its own, which a screenshot masks.
  expect(within(fold).getByTestId("going-steps-fold-time").textContent).toBe("1 min 48 s");
  // Its mark points along the line while the fold is closed, and turns down once it opens.
  const mark = fold.lastElementChild as HTMLElement;
  expect(fold.getAttribute("aria-expanded")).toBe("false");
  expect(mark.className).not.toContain("rotate-90");
  expect(labels()).toEqual([]);

  await user.click(fold);
  expect(fold.getAttribute("aria-expanded")).toBe("true");
  expect(mark.className).toContain("rotate-90");
  expect(labels()).toEqual([
    "Read DEMO-1",
    "Read DEMO-2",
    "Read DEMO-3",
    "Read DEMO-4",
    "Read DEMO-5",
  ]);
  expect(screen.queryByTestId("going-step-timer")).toBeNull();
});

// A slip - a step the model guessed that Dispatch does not have, which it mends itself - is no
// failure of the person's: its line is grey with what the model tried, and no fold counts it,
// neither as failed nor as its kind. "1 step failed" counts only what the person can act on.
const SLIP = "Tried a step Dispatch does not have";

test("a slip counts in no fold, and a failure the person can act on still counts as failed", () => {
  const slip = (id: number) => step(id, { items: [], label: SLIP, slip: true, tool: "x" });
  expect(foldWords([step(0), slip(1)])).toBe("read 1 item");
  expect(foldWords([step(0), slip(1), step(2, { refused: true })])).toBe(
    "read 1 item, 1 step failed"
  );
  expect(foldedLine([step(0), slip(1)], 108_000)).toHaveAccessibleName(
    "Worked for 1 min 48 s · read 1 item"
  );
  // Steps that were all slips say only how long the run worked, or that they are steps.
  expect(foldWords([slip(0), slip(1)])).toBe("");
  expect(foldedLine([slip(0)], 12_000)).toHaveAccessibleName("Worked for 12 s");
  expect(foldedLine([slip(0)], undefined)).toHaveAccessibleName("Steps");
});

test("a slip's line is grey with what the model tried, and a failure's line is red", async () => {
  const user = userEvent.setup();
  const steps = [
    step(0),
    step(1, { items: [], label: SLIP, slip: true, tool: "x" }),
    step(2, {
      items: ["DEMO-12"],
      label: "Could not read DEMO-12: you have no access",
      refused: true,
    }),
  ];
  const view = render(
    <StepSection
      folded={false}
      live={false}
      since={Date.now()}
      steps={steps}
      workedMs={undefined}
    />
  );
  expect(labels()).toEqual(["Read DEMO-1", SLIP, "Could not read DEMO-12: you have no access"]);
  const [endedLine, slipLine, failedLine] = screen.getAllByTestId("going-step");
  expect(endedLine?.className).toContain(textSecondaryOnSurface);
  expect(slipLine?.className).toContain(textMutedOnSurface);
  expect(slipLine?.className).not.toContain(dangerText);
  expect(failedLine?.className).toContain(dangerText);

  // Folded once the answer came, the slip is a line under the fold, and the fold names it not.
  view.rerender(
    <StepSection folded live={false} since={Date.now()} steps={steps} workedMs={108_000} />
  );
  const fold = screen.getByTestId("going-steps-fold");
  expect(fold.textContent).toBe("Worked for 1 min 48 s · read 1 item, 1 step failed›");
  await user.click(fold);
  expect(labels()).toEqual(["Read DEMO-1", SLIP, "Could not read DEMO-12: you have no access"]);
});
