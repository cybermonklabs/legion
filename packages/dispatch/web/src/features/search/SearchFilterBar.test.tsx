import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the author row below reads
import { fireEvent, render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { SearchFilterBar } from "./SearchFilterBar";
import type { SearchBarFilters } from "./search-filters";

// A reader typing Chinese, Japanese or Korean confirms a word with Enter: that Enter is the input
// method's, marked `isComposing`, or sent unmarked with keyCode 229 after the composition's end, as
// Safari sends it (WebKit bug 165004). In the author box it takes no author; a plain Enter after it
// takes what was typed.
test("the Enter that confirms an input method's word in the author box takes no author, and a plain Enter takes it", async () => {
  const user = userEvent.setup();
  const narrowed: SearchBarFilters[] = [];
  const view = render(
    <SearchFilterBar filters={{}} onFilters={(filters) => narrowed.push(filters)} projects={[]} />
  );
  try {
    const author = within(view.container).getByRole("searchbox", { name: "Author" });
    await user.click(author);
    await user.paste("alice");
    // user-event cannot mark a key as an input method's, so the two such Enters are fired.
    expect(fireEvent.keyDown(author, { isComposing: true, key: "Enter" })).toBe(true);
    expect(fireEvent.keyDown(author, { isComposing: false, key: "Enter", keyCode: 229 })).toBe(
      true
    );
    expect(narrowed).toEqual([]);

    // The plain Enter goes the same way as the two above, so only the composition mark differs.
    expect(fireEvent.keyDown(author, { key: "Enter" })).toBe(false);
    expect(narrowed).toEqual([{ author: "alice" }]);
  } finally {
    view.unmount();
  }
});
