import { afterEach, expect, test } from "bun:test";

import { canHoldFocus } from "./focus-keeper";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import { usableControl } from "./usable-control";

/** A button with no attribute of its own, for a case to wrap. */
const SAVE = '<button type="button">Save</button>';

/** Details' drawer while Details is closed: `hidden`, which a browser draws as `display: none`
 *  and happy-dom does not draw at all, so the style is set too, as a browser has it. */
function closedDetails(inside: string): string {
  return `<aside ${DETAILS_ATTRIBUTE} hidden style="display: none">${inside}</aside>`;
}

/** What each case drew on the page, taken off again after it. */
const drawn: HTMLElement[] = [];

afterEach(() => {
  for (const box of drawn.splice(0)) box.remove();
});

/** Draws `html` on the page, and returns the one button it holds. */
function drawButton(html: string): HTMLButtonElement {
  const box = document.createElement("div");
  box.innerHTML = html;
  document.body.append(box);
  drawn.push(box);
  const button = box.querySelector("button");
  if (button === null) throw new Error(`the case drew no button: ${html}`);
  return button;
}

/** Draws `html` on the page, and takes it off again with its one button. */
function removedButton(html: string): HTMLButtonElement {
  const button = drawButton(html);
  drawn.pop()?.remove();
  return button;
}

// These show that the test's DOM refuses nothing by itself, so each refusal below comes from the
// attribute or the removal its case names.
test.each([
  ["a button on the page", () => drawButton(SAVE)],
  [
    "a button in the open Details drawer",
    () => drawButton(`<aside ${DETAILS_ATTRIBUTE}>${SAVE}</aside>`),
  ],
] as const)("%s can hold the focus and takes a press", (_element, draw) => {
  const button = draw();
  expect(canHoldFocus(button), "the button cannot hold the focus").toBe(true);
  expect(usableControl(button), "the button takes no press").toBe(true);
});

// What takes the focus away takes the press with it: `usableControl` reads `canHoldFocus` for
// these, and adds nothing that could let one through.
test.each([
  ["a removed button", () => removedButton(SAVE)],
  ["a disabled button", () => drawButton('<button disabled type="button">Save</button>')],
  ["a button under a hidden element", () => drawButton(`<div hidden>${SAVE}</div>`)],
  ["a button under an inert element", () => drawButton(`<div inert>${SAVE}</div>`)],
  // `canHoldFocus` reads every element around the control, so an open drawer that something
  // above it hides or makes inert holds no control that takes a press.
  [
    "a button in the open Details drawer under a hidden element",
    () => drawButton(`<div hidden><aside ${DETAILS_ATTRIBUTE}>${SAVE}</aside></div>`),
  ],
  [
    "a button in the open Details drawer under an inert element",
    () => drawButton(`<div inert><aside ${DETAILS_ATTRIBUTE}>${SAVE}</aside></div>`),
  ],
] as const)("%s can hold no focus and takes no press", (_element, draw) => {
  const button = draw();
  expect(canHoldFocus(button), "the button can hold the focus").toBe(false);
  expect(usableControl(button), "the button takes a press").toBe(false);
});

// `canHoldFocus` takes only an HTML element, so an element of another namespace, which no
// control of the page is, takes no press either.
test("an SVG element can hold no focus and takes no press", () => {
  const box = document.createElement("div");
  box.innerHTML = '<svg><a href="#save"><text>Save</text></a></svg>';
  document.body.append(box);
  drawn.push(box);
  const link = box.querySelector("svg a");
  expect(link, "the case drew no SVG link").not.toBeNull();
  expect(canHoldFocus(link), "the SVG link can hold the focus").toBe(false);
  expect(usableControl(link), "the SVG link takes a press").toBe(false);
});

// A control can hold the focus and still refuse a press: busy while its save is out, or passed
// over by a screen reader. Only the press rule refuses these.
test.each([
  [
    "an aria-disabled button",
    () => drawButton('<button aria-disabled="true" type="button">Save</button>'),
  ],
  [
    "a button under an aria-hidden element",
    () => drawButton(`<div aria-hidden="true">${SAVE}</div>`),
  ],
] as const)("%s can hold the focus and takes no press", (_element, draw) => {
  const button = draw();
  expect(canHoldFocus(button), "the button cannot hold the focus").toBe(true);
  expect(usableControl(button), "the button takes a press").toBe(false);
});

// The one place the two differ on purpose: the key `l` and Add's list must find a control of
// Details while Details is closed, so the press rule does not read the drawer's own hidden
// `aside`, which keeps the focus out until Details opens.
test("a button in the closed Details drawer can hold no focus and takes a press", () => {
  const button = drawButton(closedDetails(SAVE));
  expect(canHoldFocus(button), "the button can hold the focus").toBe(false);
  expect(usableControl(button), "the button takes no press").toBe(true);
});

// Inside the closed drawer the press rule reads the control and the elements between it and the
// `aside` itself, since `canHoldFocus` refuses everything there: each refusal it reads elsewhere
// still holds there.
test.each([
  ["a button removed with the closed Details drawer", () => removedButton(closedDetails(SAVE))],
  [
    "a disabled button in the closed Details drawer",
    () => drawButton(closedDetails('<button disabled type="button">Save</button>')),
  ],
  [
    "an aria-disabled button in the closed Details drawer",
    () => drawButton(closedDetails('<button aria-disabled="true" type="button">Save</button>')),
  ],
  [
    "a button under a hidden element in the closed Details drawer",
    () => drawButton(closedDetails(`<div hidden>${SAVE}</div>`)),
  ],
  [
    "a button under an inert element in the closed Details drawer",
    () => drawButton(closedDetails(`<div inert>${SAVE}</div>`)),
  ],
  [
    "a button under an aria-hidden element in the closed Details drawer",
    () => drawButton(closedDetails(`<div aria-hidden="true">${SAVE}</div>`)),
  ],
] as const)("%s takes no press", (_element, draw) => {
  expect(usableControl(draw()), "the button takes a press").toBe(false);
});
