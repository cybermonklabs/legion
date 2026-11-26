import { afterEach, expect, mock, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { countStatuses, IssueStatusBar, statusSegments } from "./issues-table-bar";

afterEach(() => cleanup());

/** Six listed rows, given out of the status order: three in progress, two Todo, one in Triage. */
const LISTED = countStatuses([
  "in_progress",
  "todo",
  "triage",
  "in_progress",
  "todo",
  "in_progress",
]);

const segments = () =>
  [...document.querySelectorAll<HTMLElement>("[data-bar-segment]")].map((segment) => ({
    status: segment.dataset.barSegment,
    title: segment.getAttribute("title"),
    width: Number.parseFloat(segment.style.width),
  }));

test("the segments are the listed statuses in the status order, Triage first, with no empty one", () => {
  expect(statusSegments(LISTED)).toEqual([
    { status: "triage", count: 1 },
    { status: "todo", count: 2 },
    { status: "in_progress", count: 3 },
  ]);
  expect(statusSegments(countStatuses(["done", "triage"])).map(({ status }) => status)).toEqual([
    "triage",
    "done",
  ]);
  expect(statusSegments(countStatuses([]))).toEqual([]);
});

test("each segment is as wide as its share of the listed rows, and together they fill the bar", () => {
  render(<IssueStatusBar counts={LISTED} narrowTo={() => {}} picked={[]} />);
  const drawn = segments();
  expect(drawn.map(({ status }) => status)).toEqual(["triage", "todo", "in_progress"]);
  // R9: a segment carries no native title; the legend under the bar names each status and count.
  expect(drawn.map(({ title }) => title)).toEqual([null, null, null]);
  expect(drawn[0]?.width).toBeCloseTo(100 / 6);
  expect(drawn[1]?.width).toBeCloseTo(200 / 6);
  expect(drawn[2]?.width).toBeCloseTo(50);
  expect(drawn.reduce((sum, { width }) => sum + width, 0)).toBeCloseTo(100);
});

test("the legend has one entry for each segment", () => {
  render(<IssueStatusBar counts={LISTED} narrowTo={() => {}} picked={[]} />);
  const bar = screen.getByRole("region", { name: "Issues by status" });
  const entries = within(bar).getAllByRole("button");
  expect(entries.map((entry) => entry.textContent)).toEqual([
    "Triage 1",
    "Todo 2",
    "In progress 3",
  ]);
});

test("a press narrows to that status, and a press on the one status picked asks for the view back", async () => {
  const user = userEvent.setup();
  const narrowTo = mock((_status: string | undefined) => {});
  const { rerender } = render(
    <IssueStatusBar counts={LISTED} narrowTo={narrowTo} picked={["triage", "todo"]} />
  );
  const todo = screen.getByRole("button", { name: "Todo 2" });
  expect(todo).toHaveAttribute("aria-pressed", "false");
  await user.click(todo);
  expect(narrowTo).toHaveBeenLastCalledWith("todo");

  rerender(
    <IssueStatusBar
      counts={countStatuses(["todo", "todo"])}
      narrowTo={narrowTo}
      picked={["todo"]}
    />
  );
  const picked = screen.getByRole("button", { name: "Todo 2" });
  expect(picked).toHaveAttribute("aria-pressed", "true");
  await user.click(picked);
  expect(narrowTo).toHaveBeenLastCalledWith(undefined);
});
