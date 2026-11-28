import { expect, spyOn, test } from "bun:test";
import { render } from "@testing-library/react";

import shownValues from "../../../../../contracts/fixtures/shown-values.json";
import { columnText } from "../search/blocks/block-cells";
import { relativeTimeLabel, Timestamp } from "./Timestamp";

test("Timestamp renders the current relative label for a frozen clock", () => {
  const now = Date.parse("2026-09-10T12:00:00.000Z");
  const at = new Date(now - 5 * 60_000).toISOString();
  const nowSpy = spyOn(Date, "now").mockReturnValue(now);
  const view = render(<Timestamp at={at} />);

  try {
    const time = view.getByText("5 minutes ago");
    expect(time.tagName).toBe("TIME");
    expect(time.getAttribute("dateTime")).toBe(at);
    expect(time.getAttribute("title")).toBe(new Date(at).toLocaleString());
  } finally {
    view.unmount();
    nowSpy.mockRestore();
  }
});

test("Timestamp reflects a future time as 'in N minutes'", () => {
  const now = Date.parse("2026-09-10T12:00:00.000Z");
  const at = new Date(now + 2 * 60_000).toISOString();
  const nowSpy = spyOn(Date, "now").mockReturnValue(now);
  const view = render(<Timestamp at={at} />);

  try {
    expect(view.getByText("in 2 minutes")).toBeDefined();
  } finally {
    view.unmount();
    nowSpy.mockRestore();
  }
});

// packages/contracts/fixtures/shown-values.json holds the words the web writes for a priority and
// for a time relative to now, across each threshold of relativeTimeLabel. The command bar's agent
// gives its model the same words beside each such value of a tool result, and its Go test holds it
// to the same rows, so the agent's answer and the page say each value alike: a change of either
// rule fails a test until the fixture and the other side follow it.
test("the web writes each priority and time as packages/contracts' shown-values fixture says", () => {
  const times = shownValues.rows.filter((row) => row.kind === "time");
  const priorities = shownValues.rows.filter((row) => row.kind === "priority");
  expect(times.length).toBeGreaterThan(0);
  expect(priorities.map((row) => row.value)).toEqual([0, 1, 2, 3, null]);
  for (const row of times) {
    const written = relativeTimeLabel(new Date(String(row.value)), Date.parse(row.now));
    expect(`${row.value} at ${row.now}: ${written}`).toBe(
      `${row.value} at ${row.now}: ${row.shown}`
    );
  }
  for (const row of priorities) {
    expect(`priority ${row.value}: ${columnText("priority", row.value)}`).toBe(
      `priority ${row.value}: ${row.shown}`
    );
  }
});
