import { afterEach, expect, test } from "bun:test";
import type { Column } from "@tanstack/react-table";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { RangeFilter, rangeWords } from "./RangeFilter";

afterEach(cleanup);

/** Closed as the filter reads it: its header, the window its filter holds, and every window the
 *  filter wrote. */
function closed(
  value: string | undefined,
  wrote: (string | undefined)[]
): Column<unknown, unknown> {
  return {
    columnDef: { header: "Closed", meta: { range: true } },
    getFilterValue: () => value,
    id: "closed",
    setFilterValue: (next: string | undefined) => wrote.push(next),
  } as unknown as Column<unknown, unknown>;
}

/** The filter in a header that keeps its open state, as a table's header does. */
function Header({ column }: { column: Column<unknown, unknown> }): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <RangeFilter
      column={column}
      onOpenChange={setOpen}
      open={open}
      triggerClass={(active) => (active ? "on" : "off")}
    />
  );
}

/** Puts `day` into the date box named `label`, as a reader pastes a date there. */
async function enter(user: UserEvent, label: string, day: string) {
  await user.click(screen.getByLabelText(label));
  await user.paste(day);
}

test("the picker includes the To day it shows: 11/03 to 11/09 writes 2026-11-03..2026-11-10", async () => {
  const user = userEvent.setup();
  const wrote: (string | undefined)[] = [];
  render(<Header column={closed(undefined, wrote)} />);
  const trigger = screen.getByRole("button", { name: "Filter Closed" });
  expect(trigger).toHaveClass("off");
  await user.click(trigger);
  expect(screen.getByText("Or from and to (UTC)")).toBeDefined();
  await enter(user, "Closed from", "2026-11-03");
  await enter(user, "Closed to", "2026-11-09");
  await user.click(screen.getByRole("button", { name: "Apply" }));
  expect(wrote).toEqual(["2026-11-03..2026-11-10"]);
  // The choice closes the panel and leaves the reader on the trigger.
  expect(screen.queryByText("Or from and to (UTC)")).toBeNull();
  expect(trigger).toHaveFocus();
});

test("a window with one end leaves the other out, and no end at all writes nothing", async () => {
  const user = userEvent.setup();
  const wrote: (string | undefined)[] = [];
  render(<Header column={closed(undefined, wrote)} />);
  await user.click(screen.getByRole("button", { name: "Filter Closed" }));
  await user.click(screen.getByRole("button", { name: "Apply" }));
  expect(wrote).toEqual([]);
  await enter(user, "Closed to", "2026-11-09");
  await user.click(screen.getByRole("button", { name: "Apply" }));
  await user.click(screen.getByRole("button", { name: "Filter Closed" }));
  await enter(user, "Closed from", "2026-11-03");
  await user.click(screen.getByRole("button", { name: "Apply" }));
  expect(wrote).toEqual(["..2026-11-10", "2026-11-03.."]);
});

// k9: the picker takes no To before its From. Each date bounds the other, so the browser's
// calendar offers no such day and its check stops Apply, and Apply writes nothing for it.
test("the picker takes no To before its From", async () => {
  const user = userEvent.setup();
  const wrote: (string | undefined)[] = [];
  render(<Header column={closed(undefined, wrote)} />);
  await user.click(screen.getByRole("button", { name: "Filter Closed" }));
  await enter(user, "Closed from", "2026-11-10");
  expect(screen.getByLabelText("Closed to")).toHaveAttribute("min", "2026-11-10");
  expect(screen.getByLabelText("Closed from")).not.toHaveAttribute("max");
  await enter(user, "Closed to", "2026-11-03");
  expect(screen.getByLabelText("Closed from")).toHaveAttribute("max", "2026-11-03");
  await user.click(screen.getByRole("button", { name: "Apply" }));
  expect(wrote).toEqual([]);
  // The panel stays open with the two dates, for the reader to change.
  expect(screen.getByLabelText("Closed from")).toHaveValue("2026-11-10");
  expect(screen.getByLabelText("Closed to")).toHaveValue("2026-11-03");
});

test("it offers 1, 7, 28 and 90 days and All, each one press", async () => {
  const user = userEvent.setup();
  const wrote: (string | undefined)[] = [];
  render(<Header column={closed(undefined, wrote)} />);
  const trigger = screen.getByRole("button", { name: "Filter Closed" });
  for (const label of ["1 day", "7 days", "28 days", "90 days", "All"]) {
    await user.click(trigger);
    await user.click(screen.getByRole("button", { name: label }));
  }
  expect(wrote).toEqual(["1d", "7d", "28d", "90d", "all"]);
  // With no window set there is nothing to clear.
  await user.click(trigger);
  expect(screen.queryByRole("button", { name: "Clear" })).toBeNull();
});

test("an address read back shows the last day its window includes, and Clear takes it off", async () => {
  const user = userEvent.setup();
  const wrote: (string | undefined)[] = [];
  render(<Header column={closed("2026-11-03..2026-11-10", wrote)} />);
  const trigger = screen.getByRole("button", { name: "Filter Closed" });
  expect(trigger).toHaveClass("on");
  await user.click(trigger);
  expect(screen.getByLabelText("Closed from")).toHaveValue("2026-11-03");
  expect(screen.getByLabelText("Closed to")).toHaveValue("2026-11-09");
  await user.click(screen.getByRole("button", { name: "Clear" }));
  expect(wrote).toEqual([undefined]);
});

test("a preset in force is pressed, and All is no window", async () => {
  const user = userEvent.setup();
  render(<Header column={closed("7d", [])} />);
  await user.click(screen.getByRole("button", { name: "Filter Closed" }));
  expect(screen.getByRole("button", { name: "7 days" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: "28 days" })).toHaveAttribute("aria-pressed", "false");
  cleanup();
  render(<Header column={closed("all", [])} />);
  expect(screen.getByRole("button", { name: "Filter Closed" })).toHaveClass("off");
});

test("a window reads in the reader's words, which its chip shows", () => {
  expect(rangeWords("1d")).toBe("Last 1 day");
  expect(rangeWords("28d")).toBe("Last 28 days");
  expect(rangeWords("all")).toBe("All");
  expect(rangeWords("2026-11-03..2026-11-10")).toBe("2026-11-03 to 2026-11-09");
  expect(rangeWords("2026-11-03..")).toBe("From 2026-11-03");
  expect(rangeWords("..2026-11-10")).toBe("To 2026-11-09");
  expect(rangeWords("someday")).toBe("someday");
});
