import { expect, mock, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { pickIn } from "../../__tests__/issue-picks";
import { panelOf, rows, textBoxes } from "../../__tests__/list-picks";
import { WindowControl } from "./WindowControl";

test("a preset calls back with that preset", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    await pickIn(user, () => screen.getByLabelText("Window"), "Last 7 days");
    expect(onChange).toHaveBeenCalledWith({ kind: "preset", preset: "7d" });
  } finally {
    cleanup();
  }
});

// A `<select>` set to the value it holds fires no change when that value is picked again. The
// list opens on the window it shows, so Enter at once picks that window, and the page is called
// back with nothing: it keeps its brush.
test("a pick of the window the list shows calls back with nothing", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    const trigger = screen.getByLabelText("Window");
    await user.click(trigger);
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
    expect(trigger).toHaveAccessibleName("Window Last 28 days");
    expect(trigger).toHaveFocus();
  } finally {
    cleanup();
  }
});

// R3: every list of options a reader picks from holds a text box that narrows the options. The
// Window is the shared list's single pick of the presets and the custom range: pressed, it opens
// with one text box, a typed word leaves the windows it matches, and Enter on the one left is the
// pick a select's was - a preset calls back with that preset, and Custom range… opens the range's
// fields.
test("the Window list opens with a text box, a typed word narrows the windows, and a pick calls back or opens the range", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    const trigger = screen.getByLabelText("Window");
    expect(trigger).toHaveAccessibleName("Window Last 28 days");
    expect(textBoxes().length).toBe(0);
    await user.click(trigger);
    const panel = panelOf(trigger);
    expect(textBoxes().length).toBe(1);
    expect(within(panel).getByRole("combobox", { name: "Search windows" })).toHaveFocus();
    expect(rows(panel)).toEqual([
      "Last 7 days",
      "Last 14 days",
      "✓Last 28 days",
      "Last 84 days",
      "Custom range…",
    ]);
    await user.keyboard("14");
    expect(rows(panel)).toEqual(["Last 14 days"]);
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenCalledWith({ kind: "preset", preset: "14d" });
    expect(trigger).toHaveFocus();

    await user.click(trigger);
    await user.keyboard("cust");
    expect(rows(panelOf(trigger))).toEqual(["Custom range…"]);
    await user.keyboard("{Enter}");
    expect(trigger).toHaveAccessibleName("Window Custom range…");
    expect(screen.getByLabelText("From")).toBeDefined();
    expect(screen.getByLabelText("To")).toBeDefined();
    expect(onChange).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
  }
});

test("a custom range applies its whole UTC days", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    await pickIn(user, () => screen.getByLabelText("Window"), "Custom range…");
    await user.click(screen.getByLabelText("From"));
    await user.paste("2026-01-01");
    await user.click(screen.getByLabelText("To"));
    await user.paste("2026-01-07");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    expect(onChange).toHaveBeenCalledWith({
      kind: "custom",
      from: "2026-01-01T00:00:00.000Z",
      to: "2026-01-08T00:00:00.000Z",
    });
  } finally {
    cleanup();
  }
});

test("a custom range over 84 days is refused with the limit stated, and nothing changes", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    await pickIn(user, () => screen.getByLabelText("Window"), "Custom range…");
    await user.click(screen.getByLabelText("From"));
    await user.paste("2026-01-01");
    await user.click(screen.getByLabelText("To"));
    await user.paste("2026-04-30");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    expect(screen.getByRole("alert").textContent).toBe(
      "The window can span at most 84 days; this range spans 120."
    );
    expect(onChange).not.toHaveBeenCalled();
  } finally {
    cleanup();
  }
});

test("a custom window from the URL shows its own dates", () => {
  render(
    <WindowControl
      onChange={() => {}}
      value={{ kind: "custom", from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" }}
    />
  );
  try {
    expect(screen.getByLabelText("Window")).toHaveAccessibleName("Window Custom range…");
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe("2026-01-01");
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe("2026-01-07");
  } finally {
    cleanup();
  }
});

// The page gives the control no key, so a new window keeps the Window list mounted, and the focus
// a pick left on its trigger. A new window - the control's own pick, or the address changing
// under it - starts the draft again from that window instead.
test("a new window starts the fields and the refusal again from it, on the same list", async () => {
  const user = userEvent.setup();
  const view = render(
    <WindowControl onChange={() => {}} value={{ kind: "preset", preset: "28d" }} />
  );
  try {
    const trigger = screen.getByLabelText("Window");
    await pickIn(user, () => trigger, "Custom range…");
    await user.click(screen.getByLabelText("From"));
    await user.paste("2026-01-01");
    await user.click(screen.getByLabelText("To"));
    await user.paste("2026-04-30");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    expect(screen.getByRole("alert")).toBeDefined();

    view.rerender(
      <WindowControl
        onChange={() => {}}
        value={{ kind: "custom", from: "2026-02-01T00:00:00.000Z", to: "2026-02-03T00:00:00.000Z" }}
      />
    );
    expect(screen.getByLabelText("Window")).toBe(trigger);
    expect(trigger).toHaveAccessibleName("Window Custom range…");
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe("2026-02-01");
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe("2026-02-02");
    expect(screen.queryByRole("alert")).toBeNull();

    view.rerender(<WindowControl onChange={() => {}} value={{ kind: "preset", preset: "7d" }} />);
    expect(screen.getByLabelText("Window")).toBe(trigger);
    expect(trigger).toHaveAccessibleName("Window Last 7 days");
    expect(screen.queryByLabelText("From")).toBeNull();
  } finally {
    cleanup();
  }
});
