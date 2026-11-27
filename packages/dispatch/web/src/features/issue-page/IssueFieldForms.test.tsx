import { expect, test } from "bun:test";
import { waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { pressAsInputMethod } from "../../__tests__/input-method-keys";
import { PROPERTY_LABELS } from "./page-structure";
import { draw, OpenFieldForm, undraw } from "./shared-controls-fixture";

/**
 * The editors of an issue's typed fields, the route and the parent (`IssueFieldForms`), as the
 * identity block opens them on the new issue page. Their other rows are drawn beside the other
 * shared controls (`shared-controls.options.test.tsx`).
 */

// A press that ends an IME composition gives up what the reader was composing, not the editor:
// an Escape that is the input method's, marked `isComposing`, or sent unmarked with keyCode 229
// after the composition's end, as Safari sends it (WebKit bug 165004), leaves each editor open
// with its draft. A plain Escape after it closes the editor.
test("an input method's Escape leaves an issue field's editor open, and a plain Escape closes it", async () => {
  const user = userEvent.setup();
  for (const [field, label, draft] of [
    ["route", PROPERTY_LABELS.route, "role:reviewer"],
    ["parent", PROPERTY_LABELS.parent, "CORE-12"],
  ] as const) {
    const view = draw(<OpenFieldForm field={field} />, "inside");
    const scope = within(view.container);
    try {
      const box = scope.getByRole("textbox", { name: label }) as HTMLInputElement;
      await waitFor(() => expect(document.activeElement).toBe(box));
      await user.clear(box);
      await user.keyboard(draft);
      expect(
        pressAsInputMethod(box, "Escape", {
          beforePlain: () => {
            expect(
              scope.queryByRole("textbox", { name: label }),
              `the ${field} editor closed`
            ).toBe(box);
            expect(box.value).toBe(draft);
          },
        })
      ).toEqual({ composing: true, plain: false, safari: true });
      expect(
        scope.queryByRole("textbox", { name: label }),
        `the ${field} editor stayed`
      ).toBeNull();
    } finally {
      undraw(view);
    }
  }
});
