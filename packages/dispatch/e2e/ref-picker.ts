import { expect, type Locator, type Page } from "@playwright/test";

/**
 * The reference picker, in every box that hosts it.
 *
 * One list behind the three doors `[[`, `@` and `#`, drawn by the shared text field
 * (`web/src/features/refs/RefPickerList.tsx`), by a document's body and by an agent's live
 * conversation (`features/agent-view/ReferenceTriggers.tsx`). Whichever host draws it, the list
 * is a `listbox` named `References`, its rows are `option`s under one heading per kind, and a
 * row's words are the first `span` inside its own text column.
 *
 * Only the box the reader is writing in has a picker open, so a page has at most one.
 */

/** The picker's list, wherever it is open. */
export function refPicker(scope: Page | Locator): Locator {
  return scope.getByRole("listbox", { name: "References" });
}

/** The docked comment composer's own box, in the Conversation and wherever else one is docked:
 *  `Comment` as a substring reaches an `Edit comment` box too, and a turn part-way through a
 *  tab mounts a box of its own, so the box is taken by its exact name and the last of them. */
export function commentField(page: Page): Locator {
  return page.getByRole("textbox", { exact: true, name: "Comment" }).last();
}

/** The picker's rows in the order a reader moves through them, each as `<heading>: <words>`: a
 *  row's heading is the nearest element before it that is not a row. */
export function pickerRows(list: Locator): Promise<string[]> {
  return list.evaluate((node) =>
    [...node.querySelectorAll('[role="option"]')].map((option) => {
      let heading = "";
      for (
        let previous = option.previousElementSibling;
        previous !== null;
        previous = previous.previousElementSibling
      ) {
        if (previous.getAttribute("role") === "option") continue;
        heading = previous.textContent ?? "";
        break;
      }
      const words = option.querySelector("span span")?.textContent ?? "";
      return `${heading}: ${words}`;
    })
  );
}

/** The kind headings the picker drew, in order and each once: this door's own ranking of the
 *  kinds it found rows for (`kindOrder`). */
export async function pickerHeadings(list: Locator): Promise<string[]> {
  const headings: string[] = [];
  for (const row of await pickerRows(list)) {
    const heading = row.slice(0, row.indexOf(": "));
    if (headings[headings.length - 1] !== heading) headings.push(heading);
  }
  return headings;
}

/** The row the picker has highlighted, which Enter and Tab take. */
export function highlightedRow(list: Locator): Locator {
  return list.locator('[role="option"][aria-selected="true"]');
}

/** Opens a door in `field` by writing `text` into it, and waits for the list. The whole value is
 *  written, so a caller that needs the box's own text kept types it itself. */
export async function openRefPicker(field: Locator, text: string): Promise<Locator> {
  await field.fill(text);
  const list = refPicker(field.page());
  await expect(list).toBeVisible();
  return list;
}

/** Writes `text` into `field` and takes the row named `option`, as a reader clicking it does.
 *  A row's accessible name is its words and the line under them, so a name another kind's row
 *  also holds - a role token and the session that holds it - is given as an anchored pattern. */
export async function pickReference(
  field: Locator,
  text: string,
  option: string | RegExp
): Promise<void> {
  const list = await openRefPicker(field, text);
  await list.getByRole("option", { name: option }).first().click();
}
