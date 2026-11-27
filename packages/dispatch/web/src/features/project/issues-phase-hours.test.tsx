import { afterEach, expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { legionPhaseFill } from "../../theme/classes";
import { WORK_PHASES } from "./issues-legion";
import { PhaseHoursCell, type PhaseHoursMode, PhaseHoursModeControl } from "./issues-phase-hours";

/** The Design's colour for each phase, in the board's order with Other last: one in both
 *  schemes, but Waiting on a person's. Retro's `#666666` is from the spec's correction
 *  (dispatch://INFRA-3221/comment/57ac6e4d-c2d8-4e1d-a082-f26b0dc34ae7). */
const DESIGN_COLOURS: Readonly<Record<string, string>> = {
  design: "bg-[#E69F00]",
  planning: "bg-[#56B4E9]",
  implementing: "bg-[#009E73]",
  testing: "bg-[#F0E442]",
  reviewing: "bg-[#0072B2]",
  accepting: "bg-[#D55E00]",
  retro: "bg-[#666666]",
  merging: "bg-[#CC79A7]",
  awaiting_person: "bg-[#000000] dark:bg-[#FFFFFF]",
  held: "bg-[#999999]",
  other: "bg-[#D9D9D9]",
};

/** Hours in every part `hours_by_phase` can hold, given out of the board's order. */
const EVERY_PART: Record<string, number> = {
  other: 1,
  held: 1,
  reviewing: 3,
  retro: 1,
  design: 2,
  awaiting_person: 3,
  implementing: 8,
  planning: 2,
  merging: 1,
  testing: 2,
  accepting: 1,
};

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
});

function draw(tree: ReactNode) {
  const view = render(<div data-testid="cell">{tree}</div>);
  restores.push(() => view.unmount());
  return view;
}

function cell(hours: Record<string, number> | null, mode: PhaseHoursMode = "share", pageMax = 0) {
  return draw(<PhaseHoursCell facts={{ hours_by_phase: hours }} mode={mode} pageMax={pageMax} />);
}

const segments = () =>
  [...document.querySelectorAll<HTMLElement>("[data-phase-segment]")].map((segment) => ({
    className: segment.className,
    phase: segment.dataset.phaseSegment,
  }));

test("each phase has the Design's fixed colour in the board's order with Other last, no two the same, and Queue has none", () => {
  expect([...WORK_PHASES]).toEqual(Object.keys(DESIGN_COLOURS));
  expect(Object.keys(legionPhaseFill)).toEqual([...WORK_PHASES]);
  const fills: Readonly<Record<string, string>> = legionPhaseFill;
  expect({ ...fills }).toEqual(DESIGN_COLOURS);
  // Retro is one gray in both schemes, apart from Held's and Other's.
  expect(legionPhaseFill.retro).toBe("bg-[#666666]");
  expect(new Set(Object.values(legionPhaseFill)).size).toBe(WORK_PHASES.length);
  expect(Object.keys(legionPhaseFill)).not.toContain("queue");
});

test("Share draws each phase with hours in its colour, in the board's order and Other last, across the whole cell", () => {
  cell(EVERY_PART);
  expect(segments()).toEqual(
    WORK_PHASES.map((phase) => ({ className: DESIGN_COLOURS[phase], phase }))
  );
  // Waiting on a person is black on a light page and white on a dark one.
  const waiting = document.querySelector('[data-phase-segment="awaiting_person"]');
  expect(waiting).toHaveClass("bg-[#000000]");
  expect(waiting).toHaveClass("dark:bg-[#FFFFFF]");
  // Each segment's part of the bar is its share of the row's total.
  const implementing = document.querySelector<HTMLElement>('[data-phase-segment="implementing"]');
  expect(implementing?.style.flexGrow).toBe(String(8 / 25));
  // The bar runs across the whole cell.
  const bar = document.querySelector<HTMLElement>("[data-phase-hours-bar]");
  expect(bar?.style.width).toBe("100%");
  // Share writes no total.
  expect(screen.getByTestId("cell").textContent).toBe("");
});

test("Queue draws no segment: hours under a key this build does not draw count as Other's", () => {
  cell({ implementing: 2, queue: 3, other: 1 });
  expect(segments().map(({ phase }) => phase)).toEqual(["implementing", "other"]);
  const other = document.querySelector<HTMLElement>('[data-phase-segment="other"]');
  expect(Number(other?.style.flexGrow)).toBeCloseTo(4 / 6, 5);
});

test("hours under a phase a newer server adds have no colour and no line of their own: they are Other's", async () => {
  const user = userEvent.setup();
  cell({ implementing: 2, x_phase: 2 });
  expect(segments()).toEqual([
    { className: DESIGN_COLOURS.implementing, phase: "implementing" },
    { className: DESIGN_COLOURS.other, phase: "other" },
  ]);
  await user.hover(screen.getByRole("button", { name: "Time per phase, 4.0 h" }));
  const lines = [
    ...screen.getByRole("tooltip").querySelectorAll<HTMLElement>("[data-phase-hours-line]"),
  ];
  expect(
    lines.map((line) => [
      line.dataset.phaseHoursLine,
      ...[...line.children].slice(1).map((part) => part.textContent),
    ])
  ).toEqual([
    ["implementing", "Implementing", "2.0 h", "50%"],
    ["other", "Other", "2.0 h", "50%"],
  ]);
});

test("Hours draws the bar against the page's longest total and writes the total in hours", () => {
  cell({ implementing: 4, reviewing: 1 }, "hours", 20);
  expect(document.querySelector<HTMLElement>("[data-phase-hours-bar]")?.style.width).toBe("25%");
  expect(screen.getByTestId("cell").textContent).toBe("5.0 h");
  for (const restore of restores.splice(0)) restore();
  cell({ implementing: 12, reviewing: 8 }, "hours", 20);
  expect(document.querySelector<HTMLElement>("[data-phase-hours-bar]")?.style.width).toBe("100%");
  expect(screen.getByTestId("cell").textContent).toBe("20 h");
});

test("an issue whose facts hold no hours draws an empty cell", () => {
  cell(null);
  expect(screen.getByTestId("cell")).toBeEmptyDOMElement();
  for (const restore of restores.splice(0)) restore();
  cell({ implementing: 0 });
  expect(screen.getByTestId("cell")).toBeEmptyDOMElement();
});

test("a row with Retro hours draws its Retro segment and its tooltip line between Accepting and Merging", async () => {
  const user = userEvent.setup();
  cell({ merging: 1, other: 1, retro: 2, accepting: 1 });
  expect(segments()).toEqual([
    { className: "bg-[#D55E00]", phase: "accepting" },
    { className: "bg-[#666666]", phase: "retro" },
    { className: "bg-[#CC79A7]", phase: "merging" },
    { className: "bg-[#D9D9D9]", phase: "other" },
  ]);
  await user.hover(screen.getByRole("button", { name: "Time per phase, 5.0 h" }));
  const lines = [
    ...screen.getByRole("tooltip").querySelectorAll<HTMLElement>("[data-phase-hours-line]"),
  ];
  expect(lines.map((line) => [...line.children].slice(1).map((part) => part.textContent))).toEqual([
    ["Accepting", "1.0 h", "20%"],
    ["Retro", "2.0 h", "40%"],
    ["Merging", "1.0 h", "20%"],
    ["Other", "1.0 h", "20%"],
  ]);
  const retro = lines.find((line) => line.dataset.phaseHoursLine === "retro");
  expect(retro?.querySelector("[data-phase-swatch]")).toHaveClass("bg-[#666666]");
});

test("the cell is a Tab stop whose tooltip has one line per phase with hours: its swatch, name, hours and share", async () => {
  const user = userEvent.setup();
  cell({ held: 1, implementing: 6, design: 2, other: 1 });
  const button = screen.getByRole("button", { name: "Time per phase, 10 h" });
  await user.hover(button);
  const tip = screen.getByRole("tooltip");
  const lines = [...tip.querySelectorAll<HTMLElement>("[data-phase-hours-line]")];
  expect(lines.map((line) => line.dataset.phaseHoursLine)).toEqual([
    "design",
    "implementing",
    "held",
    "other",
  ]);
  expect(lines.map((line) => [...line.children].slice(1).map((part) => part.textContent))).toEqual([
    ["Design", "2.0 h", "20%"],
    ["Implementing", "6.0 h", "60%"],
    ["Held", "1.0 h", "10%"],
    ["Other", "1.0 h", "10%"],
  ]);
  // Each swatch in its segment's colour.
  for (const line of lines)
    expect(line.querySelector("[data-phase-swatch]")?.className).toContain(
      DESIGN_COLOURS[line.dataset.phaseHoursLine ?? ""] ?? "-"
    );
  await user.unhover(button);
  expect(screen.queryByRole("tooltip")).toBeNull();

  // The keyboard reaches the cell, and its focus opens the same tooltip; Escape closes it.
  await user.tab();
  expect(button).toHaveFocus();
  expect(within(screen.getByRole("tooltip")).getByText("Implementing")).toBeDefined();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("tooltip")).toBeNull();
  expect(button).toHaveFocus();

  // A tap, where there is no hover, opens it too.
  await user.click(button);
  expect(screen.getByRole("tooltip")).toBeDefined();
  // No part of the cell, nor its tooltip, carries a native title.
  expect(document.querySelectorAll("[title]")).toHaveLength(0);
});

test("the header's control offers Share and Hours and says which is on", async () => {
  const user = userEvent.setup();
  const picked: PhaseHoursMode[] = [];
  draw(<PhaseHoursModeControl mode="share" onChange={(mode) => picked.push(mode)} />);
  const group = screen.getByRole("group", { name: "Time per phase as" });
  expect(within(group).getByRole("button", { name: "Share" })).toHaveAttribute(
    "aria-pressed",
    "true"
  );
  expect(within(group).getByRole("button", { name: "Hours" })).toHaveAttribute(
    "aria-pressed",
    "false"
  );
  await user.click(within(group).getByRole("button", { name: "Hours" }));
  expect(picked).toEqual(["hours"]);
});
