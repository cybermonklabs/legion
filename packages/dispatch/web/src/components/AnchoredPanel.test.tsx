import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the Escape row below reads
import { fireEvent, render, screen } from "@testing-library/react";
import { type ReactNode, useRef, useState } from "react";

import { AnchoredPanel } from "./AnchoredPanel";

/** A control and the panel it opens, holding a field a reader types in, as a picker's search box
 *  is held (`MultiSelect`). The panel goes as it is dismissed. */
function Host(): ReactNode {
  const anchor = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(true);
  return (
    <>
      <button ref={anchor} type="button">
        Filters
      </button>
      {open ? (
        <AnchoredPanel anchorRef={anchor} className="" onDismiss={() => setOpen(false)}>
          <input aria-label="Search filters" />
        </AnchoredPanel>
      ) : null}
    </>
  );
}

// A reader typing Chinese, Japanese or Korean in the panel's field drops a candidate with Escape:
// that Escape is the input method's, marked `isComposing`, or sent unmarked with keyCode 229 after
// the composition's end, as Safari sends it (WebKit bug 165004), and the panel stays open. A plain
// Escape after it closes the panel.
test("an input method's Escape in the panel's field leaves the panel open, and a plain Escape closes it", () => {
  const view = render(<Host />);
  try {
    // The panel is portalled to the body, outside the render's container.
    const field = screen.getByRole("textbox", { name: "Search filters" });
    // user-event cannot mark a key as an input method's, so the two such Escapes are fired.
    expect(fireEvent.keyDown(field, { isComposing: true, key: "Escape" })).toBe(true);
    expect(fireEvent.keyDown(field, { isComposing: false, key: "Escape", keyCode: 229 })).toBe(
      true
    );
    expect(screen.queryByRole("textbox", { name: "Search filters" })).toBe(field);

    // The plain Escape goes the same way as the two above, so only the composition mark differs.
    expect(fireEvent.keyDown(field, { key: "Escape" })).toBe(false);
    expect(screen.queryByRole("textbox", { name: "Search filters" })).toBeNull();
  } finally {
    view.unmount();
  }
});
