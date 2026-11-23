import { expect, test } from "bun:test";
import { render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { jump, onFakeClock } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { InPlaceField, SAVE_AFTER_MS } from "./InPlaceField";

/**
 * The field as a page holds it: `value` is the text the server answered with, and it moves only
 * when something writes it — the reader's own write coming back, or another writer's. The two
 * buttons stand for those two moments, so a test says when each happens rather than waiting on
 * a mutation it cannot see.
 */
function Field(): ReactNode {
  const [value, setValue] = useState("Old name");
  const [sent, setSent] = useState<string>();
  return (
    <>
      <InPlaceField label="Title" onSave={setSent} value={value} />
      <button onClick={() => setValue(sent ?? value)} type="button">
        Land the write
      </button>
      <button onClick={() => setValue("Written elsewhere")} type="button">
        Write it elsewhere
      </button>
    </>
  );
}

// Queries are scoped to this render's container: bun runs every test file in one document, and
// a sibling file's un-unmounted render would otherwise leak its fields into a `screen` query.
test("the text a reader left stands through the write, rather than the old text returning", async () => {
  const user = userEvent.setup();
  const view = render(<Field />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" });
  await user.clear(field);
  await user.type(field, "New name");
  // Leaving the field sends the write. The answer is not back, so the value under the field is
  // still the old text; the reader must not see their own words undone while it travels.
  await user.tab();
  expect((field as HTMLInputElement).value).toBe("New name");
  await user.click(page.getByRole("button", { name: "Land the write" }));
  expect((field as HTMLInputElement).value).toBe("New name");
  view.unmount();
});

test("a text written elsewhere replaces what a field the reader is not holding shows", async () => {
  const user = userEvent.setup();
  const view = render(<Field />);
  const page = within(view.container);
  await user.click(page.getByRole("button", { name: "Write it elsewhere" }));
  expect((page.getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe(
    "Written elsewhere"
  );
  view.unmount();
});

// The rest and the leaving are each a moment of their own here: the clock is held, so the rest
// comes when the row moves it past `SAVE_AFTER_MS` and never between two keys of the typing. The
// server's text stays the old one throughout, as it does while the rested write's answer is out.
test("a text written when the typing rested is not written again when the field is left", async () => {
  await onFakeClock(async () => {
    const user = userOnFakeClock();
    const writes: string[] = [];
    const view = render(
      <InPlaceField label="Title" onSave={(next) => writes.push(next)} value="Old name" />
    );
    const field = within(view.container).getByRole("textbox", { name: "Title" });
    await user.clear(field);
    await user.type(field, "New name");
    expect(writes).toEqual([]);
    await jump(SAVE_AFTER_MS);
    expect(writes).toEqual(["New name"]);
    await user.tab();
    expect(writes).toEqual(["New name"]);
    view.unmount();
  });
});

test("a text changed after the typing rested is written when the field is left", async () => {
  await onFakeClock(async () => {
    const user = userOnFakeClock();
    const writes: string[] = [];
    const view = render(
      <InPlaceField label="Title" onSave={(next) => writes.push(next)} value="Old name" />
    );
    const field = within(view.container).getByRole("textbox", { name: "Title" });
    await user.clear(field);
    await user.type(field, "New");
    await jump(SAVE_AFTER_MS);
    await user.type(field, " name");
    await user.tab();
    expect(writes).toEqual(["New", "New name"]);
    view.unmount();
  });
});
