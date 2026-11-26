import { expect, test } from "bun:test";
import {
  // biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the Escape that ends an IME composition as Safari sends it, unmarked with keyCode 229, which user-event has no form for
  fireEvent,
  type RenderResult,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import type { IssueDetails, UserIssueState } from "../../api/types";
import { issue as marginIssue } from "../margin/margin-fixture";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useDialog } from "../shell/useDialog";
import { BlockedByControl } from "./BlockedByControl";
import { blockedByControlName } from "./details-names";
import { type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { PROPERTY_LABELS } from "./page-structure";
import { draw, undraw } from "./shared-controls-fixture";
import { useIssueBlockedBy } from "./useIssueBlockedBy";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

/** The issue whose fields the forms edit: the margin fixture's issue, waiting on none, with no
 *  documents, as `shared-controls.options.test.tsx` builds its own. */
const issue: IssueDetails = {
  ...marginIssue,
  artifacts: [],
  blocked_by: [],
  last_seq: 250,
  primary_artifact_id: "",
  status: "todo",
};

const state: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

/** The blocked-by row's control as Details draws it (`IssueMargin.tsx`): the list typed into it
 *  and whether its editor is open are the panel's, and its write is the page's own. */
function BlockedByRow(): ReactNode {
  const [draft, setDraft] = useState("");
  const [editing, setEditing] = useState(false);
  const write = useIssueBlockedBy(issue.key, () => setEditing(false));
  return (
    <BlockedByControl
      draft={draft}
      editing={editing}
      issue={issue}
      onDraft={setDraft}
      onEditing={setEditing}
      titleOf={() => undefined}
      write={write}
    />
  );
}

/** The identity block's editor of `field`, open as the page opens it, on the drafts of the page's
 *  `useIssueHeaderWrites`, and closed by its `onClose`. */
function OpenFieldForm({ field }: { field: IssueField }): ReactNode {
  const writes = useIssueHeaderWrites(issue, state);
  const [open, setOpen] = useState<IssueField | null>(field);
  return (
    <IssueFieldForms
      field={open}
      isClosed={false}
      issue={issue}
      onClose={() => setOpen(null)}
      writes={writes}
    />
  );
}

/** A form a reader types a draft in whose own Escape closes it. */
interface TypedForm {
  /** The form, for a failure's message. */
  readonly name: string;
  /** The form as the page draws it, open or not yet. */
  readonly node: ReactNode;
  /** The name of the form's box. */
  readonly box: string;
  /** What the reader types in the box. */
  readonly draft: string;
  /** Opens the form, if it is not open. */
  readonly open: (view: RenderResult, user: UserEvent) => Promise<void>;
}

const TYPED_FORMS: readonly TypedForm[] = [
  {
    box: `Issues ${issue.key} waits on`,
    draft: "CORE-2",
    name: "the blocked-by editor",
    node: <BlockedByRow />,
    open: async (view, user) => {
      await user.click(
        within(view.container).getByRole("button", { name: blockedByControlName(issue.key, 0) })
      );
    },
  },
  ...(
    [
      ["route", PROPERTY_LABELS.route, "role:reviewer"],
      ["parent", PROPERTY_LABELS.parent, "CORE-12"],
    ] as const
  ).map(
    ([field, box, draft]): TypedForm => ({
      box,
      draft,
      name: `the ${field} editor`,
      node: <OpenFieldForm field={field} />,
      open: async () => undefined,
    })
  ),
];

// An Escape that ends an IME composition gives up what the reader was composing, not the form they
// compose in. Chromium and Firefox mark that press `isComposing`; Safari sends it after the
// composition's end, unmarked, with keyCode 229 (`inputMethodKey`). Each form a reader types a
// draft in stays open on that press, with the draft and the focus in its box, and marks the press
// taken, so the page's own Escape passes over it too; a plain Escape then closes the form, marked
// taken. The title is the one the verification's `c-units` names, though the forms are three.
test("the Escape that ends an input method's composition closes neither form", async () => {
  const user = userEvent.setup();
  for (const form of TYPED_FORMS) {
    // Each Escape pressed, read once its press is over for whether a handler marked it taken.
    const presses: KeyboardEvent[] = [];
    const record = (event: KeyboardEvent) => {
      if (event.key === "Escape") presses.push(event);
    };
    document.addEventListener("keydown", record, true);
    const view = draw(form.node, "inside");
    try {
      const scope = within(view.container);
      await form.open(view, user);
      const box = scope.getByRole<HTMLInputElement>("textbox", { name: form.box });
      await waitFor(() => expect(document.activeElement).toBe(box));
      await user.clear(box);
      await user.keyboard(form.draft);
      expect(box.value).toBe(form.draft);

      // user-event has no form for this: it cannot send a key with keyCode 229.
      fireEvent.keyDown(box, { isComposing: false, key: "Escape", keyCode: 229 });
      expect(
        scope.queryByRole("textbox", { name: form.box }),
        `the Escape that ended a composition closed ${form.name}`
      ).toBe(box);
      expect(box.value, `the Escape that ended a composition threw ${form.name}'s draft away`).toBe(
        form.draft
      );
      expect(
        document.activeElement,
        `the Escape that ended a composition moved the focus out of ${form.name}`
      ).toBe(box);
      expect(
        presses.map((event) => event.defaultPrevented),
        `${form.name} left the Escape that ended a composition unmarked`
      ).toEqual([true]);

      await user.keyboard("{Escape}");
      expect(
        scope.queryByRole("textbox", { name: form.box }),
        `a plain Escape left ${form.name} open`
      ).toBeNull();
      expect(
        presses.map((event) => event.defaultPrevented),
        `${form.name} left a plain Escape unmarked`
      ).toEqual([true, true]);
    } finally {
      document.removeEventListener("keydown", record, true);
      undraw(view);
    }
  }
});

/** Details as a phone draws it: a sheet of the key registry's (`useDialog`), which Escape closes
 *  through its `dialog.close` binding wherever the focus is in it, a field included. */
function Sheet({ children }: { children: ReactNode }): ReactNode {
  const [open, setOpen] = useState(true);
  const dialog = useDialog<HTMLDivElement>({ onClose: () => setOpen(false), open });
  return open ? (
    <div aria-label={SHEET} ref={dialog.containerRef} role="dialog">
      {children}
    </div>
  ) : null;
}

/** The sheet's name. */
const SHEET = "Details";

// On a phone each form stands in Details' sheet, which the key registry closes on Escape, after the
// form has heard the press. Safari's Escape that ends a composition closes neither the form nor
// the sheet around it; a plain Escape closes the form and leaves the sheet open.
test("the Escape that ends an input method's composition in a dialog closes neither the form nor the dialog", async () => {
  const user = userEvent.setup();
  for (const form of TYPED_FORMS) {
    const view = draw(
      <KeymapProvider>
        <Sheet>{form.node}</Sheet>
      </KeymapProvider>,
      "inside"
    );
    try {
      const scope = within(view.container);
      await form.open(view, user);
      const box = scope.getByRole<HTMLInputElement>("textbox", { name: form.box });
      await waitFor(() => expect(document.activeElement).toBe(box));
      await user.clear(box);
      await user.keyboard(form.draft);

      // user-event has no form for this: it cannot send a key with keyCode 229.
      fireEvent.keyDown(box, { isComposing: false, key: "Escape", keyCode: 229 });
      expect(
        scope.queryByRole("dialog", { name: SHEET }),
        `the Escape that ended a composition in ${form.name} closed the sheet`
      ).not.toBeNull();
      expect(
        scope.queryByRole("textbox", { name: form.box }),
        `the Escape that ended a composition closed ${form.name}`
      ).toBe(box);
      expect(box.value).toBe(form.draft);

      await user.keyboard("{Escape}");
      expect(
        scope.queryByRole("textbox", { name: form.box }),
        `a plain Escape left ${form.name} open`
      ).toBeNull();
      expect(
        scope.queryByRole("dialog", { name: SHEET }),
        `a plain Escape in ${form.name} closed the sheet with it`
      ).not.toBeNull();
    } finally {
      undraw(view);
    }
  }
});
