import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { InfoHint } from "./InfoHint";

afterEach(cleanup);

test("a hint opens on a press and on focus, and Escape closes it and gives the button back", async () => {
  const user = userEvent.setup();
  render(
    <InfoHint label="About work time">Work time is the hours an issue was worked on.</InfoHint>
  );
  const button = screen.getByRole("button", { name: "About work time" });
  expect(screen.queryByText(/Work time is the hours/)).toBeNull();

  await user.click(button);
  expect(screen.getByText(/Work time is the hours/)).toBeDefined();
  expect(button.getAttribute("aria-expanded")).toBe("true");

  await user.keyboard("{Escape}");
  expect(screen.queryByText(/Work time is the hours/)).toBeNull();
  expect(document.activeElement).toBe(button);
});

test("a press on the hint reaches nothing around it", async () => {
  const user = userEvent.setup();
  const pressed: string[] = [];
  render(
    // Stands for a header that sorts on a press: the claim is that the hint's press is not it.
    // A hint inside a sort button is what `meta.control` keeps apart in the table's own header.
    <button onClick={() => pressed.push("header")} type="button">
      <InfoHint label="About work time">Hours worked.</InfoHint>
    </button>
  );
  await user.click(screen.getByRole("button", { name: "About work time" }));
  expect(pressed).toEqual([]);
});
