import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the Enter row below reads
import { fireEvent, render, within } from "@testing-library/react";
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

// A reader typing Chinese, Japanese or Korean confirms a word with Enter: that Enter is the input
// method's, marked `isComposing`, or sent unmarked with keyCode 229 after the composition's end, as
// Safari sends it (WebKit bug 165004). It saves nothing and leaves the field held; a plain Enter
// after it saves what was typed and leaves the field.
test("the Enter that confirms an input method's word saves nothing and keeps the field, and a plain Enter saves and leaves it", async () => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <InPlaceField label="Title" onSave={(next) => saved.push(next)} value="Old name" />
  );
  try {
    const field = within(view.container).getByRole("textbox", { name: "Title" });
    await user.clear(field);
    await user.type(field, "New name");
    // user-event cannot mark a key as an input method's, so the two such Enters are fired.
    expect(fireEvent.keyDown(field, { isComposing: true, key: "Enter" })).toBe(true);
    expect(fireEvent.keyDown(field, { isComposing: false, key: "Enter", keyCode: 229 })).toBe(true);
    expect(saved).toEqual([]);
    expect(document.activeElement).toBe(field);

    // The plain Enter goes the same way as the two above, so only the composition mark differs.
    expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(false);
    expect(saved.at(0)).toBe("New name");
    expect(document.activeElement).not.toBe(field);
  } finally {
    view.unmount();
  }
});
