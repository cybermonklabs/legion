import { afterEach, expect, test } from "bun:test";
import { cleanup, render } from "@testing-library/react";

import { PhaseHoursBar } from "./PhaseHoursBar";

afterEach(cleanup);

const HOURS = { implementing: 6, planning: 2, reviewing: 2 };

test("in hours mode the bar's length is the issue's hours against the table's longest", () => {
  const { container } = render(<PhaseHoursBar hours={HOURS} max={20} mode="hours" />);
  const bar = container.querySelector("[data-phase-hours-length]") as HTMLElement;
  expect(bar.style.width).toBe("50%");
  // The blocks come in workflow order, each its share of the issue's hours.
  const parts = [...container.querySelectorAll("[data-phase-hours-part]")] as HTMLElement[];
  expect(parts.map((part) => part.dataset.phaseHoursPart)).toEqual([
    "planning",
    "implementing",
    "reviewing",
  ]);
  expect(parts[1]?.style.width).toBe("60%");
  expect(parts[1]?.title).toBe("Implementing: 6.0 h, 60%");
});

test("in share mode every bar is full length", () => {
  const { container } = render(<PhaseHoursBar hours={HOURS} max={20} mode="share" />);
  expect((container.querySelector("[data-phase-hours-length]") as HTMLElement).style.width).toBe(
    "100%"
  );
});

test("an issue with no hours draws nothing", () => {
  const { container } = render(<PhaseHoursBar hours={{}} mode="hours" />);
  expect(container.querySelector("[data-phase-hours-bar]")).toBeNull();
});
