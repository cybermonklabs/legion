import { queryAllByRole } from "@testing-library/react";

/**
 * What a row of the outline is named by and which controls it holds, read off the DOM: for the
 * cases of the outline's row (`NodeRow.name.test.tsx`) and of the Decisions row
 * (`DecisionsFilterBody.drawn.test.tsx`), which both hold their margin - the kind's tag, the
 * author, the time - ahead of their words in the DOM, so that on a phone the margin floats beside
 * the words' first line (`NodeRow.tsx`'s `NODE_ROW_LAYOUT`).
 */

/** The accessible name of `row`, a `treeitem`, as the testing library's role query computes it. */
export function accessibleNameOf(row: HTMLElement): string {
  let named = "";
  queryAllByRole(row.ownerDocument.body, "treeitem", {
    name: (name, element) => {
      if (element === row) named = name;
      return element === row;
    },
  });
  return named;
}

/** A copy of `row` with its margin after its words' column and no name of its own - no
 *  `aria-labelledby`, and no id of the row's - so that its name is its text in reading order. It
 *  is put in the document outside the view `row` was drawn in; the caller removes it. */
export function withMarginAfterWords(row: HTMLElement): HTMLElement {
  const copy = row.cloneNode(true) as HTMLElement;
  copy.removeAttribute("aria-labelledby");
  for (const named of copy.querySelectorAll("[id]")) named.removeAttribute("id");
  const margin = copy.querySelector("[data-node-margin]");
  if (margin !== null) copy.querySelector("[data-node-content]")?.after(margin);
  row.ownerDocument.body.append(copy);
  return copy;
}

/** Each control of `row` that takes the focus, in the order the row reads in - its bullet, its
 *  words and what it draws under them, then its margin - by what it is: "fold" for the bullet's
 *  control, "the kind's link", and any other by its words. */
export function rowControls(row: HTMLElement): string[] {
  const margin = row.querySelector("[data-node-margin]");
  const found = [...row.querySelectorAll<HTMLElement>("a[href], button, [tabindex]")];
  return [
    ...found.filter((control) => margin?.contains(control) !== true),
    ...found.filter((control) => margin?.contains(control) === true),
  ].map((control) =>
    control.matches("[data-node-bullet]")
      ? "fold"
      : control.matches("[data-node-tag-link]")
        ? "the kind's link"
        : (control.textContent ?? "").replace(/\s+/g, " ").trim()
  );
}

/** Whether each element of `row` that is in the tab order, `row` itself among them, is `row`:
 *  `[]` for a row that holds no tab stop, `[true]` for the tree's one tab stop. */
export function tabStopsOf(row: HTMLElement): boolean[] {
  return [row, ...row.querySelectorAll<HTMLElement>("*")]
    .filter((element) => element.tabIndex >= 0 && !element.hasAttribute("disabled"))
    .map((element) => element === row);
}
