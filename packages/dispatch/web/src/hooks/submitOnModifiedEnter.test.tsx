import { expect, test } from "bun:test";
import { render } from "@testing-library/react";

import { pressAsInputMethod } from "../__tests__/input-method-keys";
import { submitOnModifiedEnter } from "./submitOnModifiedEnter";

// A reader typing Chinese, Japanese or Korean confirms a word with Enter: that Enter is the input
// method's, marked `isComposing`, or sent unmarked with keyCode 229 after the composition's end, as
// Safari sends it (WebKit bug 165004), and with Ctrl or Cmd held it still submits nothing. A plain
// Ctrl+Enter after it submits the box's form.
test("the Ctrl+Enter that confirms an input method's word submits nothing, and a plain Ctrl+Enter submits", () => {
  let submitted = 0;
  const view = render(
    <form
      onSubmit={(event) => {
        event.preventDefault();
        submitted += 1;
      }}
    >
      <textarea aria-label="Words" onKeyDown={(event) => submitOnModifiedEnter(event)} />
    </form>
  );
  try {
    const field = view.getByLabelText("Words");
    expect(
      pressAsInputMethod(field, "Enter", {
        beforePlain: () => expect(submitted).toBe(0),
        held: { ctrlKey: true },
      })
    ).toEqual({ composing: true, plain: false, safari: true });
    expect(submitted).toBe(1);
  } finally {
    view.unmount();
  }
});
