import { expect, mock, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { WindowControl } from "./WindowControl";

test("a preset calls back with that preset", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    await user.selectOptions(screen.getByLabelText("Window"), "7d");
    expect(onChange).toHaveBeenCalledWith({ kind: "preset", preset: "7d" });
  } finally {
    cleanup();
  }
});

test("a custom range applies its whole UTC days", async () => {
  const user = userEvent.setup();
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    await user.selectOptions(screen.getByLabelText("Window"), "custom");
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
    await user.selectOptions(screen.getByLabelText("Window"), "custom");
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
    expect((screen.getByLabelText("Window") as HTMLSelectElement).value).toBe("custom");
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe("2026-01-01");
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe("2026-01-07");
  } finally {
    cleanup();
  }
});

// The page gives the control no key, so a new window keeps the Window select mounted, and the
// focus a keyboard pick left on it. A new window - the control's own pick, or the address
// changing under it - starts the draft again from that window instead.
test("a new window starts the fields and the refusal again from it, on the same select", async () => {
  const user = userEvent.setup();
  const view = render(
    <WindowControl onChange={() => {}} value={{ kind: "preset", preset: "28d" }} />
  );
  try {
    const select = screen.getByLabelText("Window") as HTMLSelectElement;
    await user.selectOptions(select, "custom");
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
    expect(screen.getByLabelText("Window")).toBe(select);
    expect(select.value).toBe("custom");
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe("2026-02-01");
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe("2026-02-02");
    expect(screen.queryByRole("alert")).toBeNull();

    view.rerender(<WindowControl onChange={() => {}} value={{ kind: "preset", preset: "7d" }} />);
    expect(screen.getByLabelText("Window")).toBe(select);
    expect(select.value).toBe("7d");
    expect(screen.queryByLabelText("From")).toBeNull();
  } finally {
    cleanup();
  }
});
