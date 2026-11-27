import { expect, test } from "bun:test";
import { render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { pressAsInputMethod } from "../../__tests__/input-method-keys";
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
    expect(
      pressAsInputMethod(author, "Enter", { beforePlain: () => expect(narrowed).toEqual([]) })
    ).toEqual({ composing: true, plain: false, safari: true });
    expect(narrowed).toEqual([{ author: "alice" }]);
  } finally {
    view.unmount();
  }
});
