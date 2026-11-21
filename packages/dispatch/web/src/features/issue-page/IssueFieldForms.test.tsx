import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the Escape row below reads
import { fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import type { IssueDetails, UserIssueState } from "../../api/types";
import { issue as marginIssue } from "../margin/margin-fixture";
import { type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { PROPERTY_LABELS } from "./page-structure";
import { draw, undraw } from "./shared-controls-fixture";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * The editors of an issue's typed fields, the route and the parent (`IssueFieldForms`), as the
 * identity block opens them on the new issue page. Their other rows are drawn beside the other
 * shared controls (`shared-controls.options.test.tsx`).
 */

/** The issue the editors are open on. */
const ISSUE: IssueDetails = {
  ...marginIssue,
  artifacts: [],
  last_seq: 250,
  primary_artifact_id: "",
  status: "todo",
};

/** The reader's own state of `ISSUE`. */
const STATE: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

/** The editor of `field`, open as the page opens it, on the drafts of the page's
 *  `useIssueHeaderWrites`, and closed as the page closes it. */
function OpenFieldForm({ field }: { field: IssueField }): ReactNode {
  const writes = useIssueHeaderWrites(ISSUE, STATE);
  const [open, setOpen] = useState<IssueField | null>(field);
  return (
    <IssueFieldForms
      field={open}
      isClosed={false}
      issue={ISSUE}
      onClose={() => setOpen(null)}
      writes={writes}
    />
  );
}

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
      // user-event cannot mark a key as an input method's, so the two such Escapes are fired.
      expect(fireEvent.keyDown(box, { isComposing: true, key: "Escape" })).toBe(true);
      expect(fireEvent.keyDown(box, { isComposing: false, key: "Escape", keyCode: 229 })).toBe(
        true
      );
      expect(scope.queryByRole("textbox", { name: label }), `the ${field} editor closed`).toBe(box);
      expect(box.value).toBe(draft);

      // The plain Escape goes the same way as the two above, so only the composition mark differs.
      expect(fireEvent.keyDown(box, { key: "Escape" })).toBe(false);
      expect(
        scope.queryByRole("textbox", { name: label }),
        `the ${field} editor stayed`
      ).toBeNull();
    } finally {
      undraw(view);
    }
  }
});
