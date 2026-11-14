import { expect, test } from "bun:test";
import { render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { InPlaceField } from "./InPlaceField";

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
