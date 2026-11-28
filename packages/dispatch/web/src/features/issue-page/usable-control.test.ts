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

/** A group its caller shuts on purpose and names as the bound, as the header's keys name the
 *  actions menu's group that holds the pin: `hidden`, with the style a browser draws it with, as
 *  `closedDetails` has. */
function shutGroup(inside: string): string {
  return `<div data-bound hidden style="display: none">${inside}</div>`;
}

/** What each case drew on the page, taken off again after it. */
const drawn: HTMLElement[] = [];

afterEach(() => {
  for (const box of drawn.splice(0)) box.remove();
});

/** A link of another namespace than HTML's, which no control of the page is. */
const SVG_LINK = '<svg><a href="#save"><text>Save</text></a></svg>';

/** Draws `html` on the page, and returns the one element of it `selector` finds. */
function drawControl(html: string, selector: string): Element {
  const box = document.createElement("div");
  box.innerHTML = html;
  document.body.append(box);
  drawn.push(box);
  const control = box.querySelector(selector);
  if (control === null) throw new Error(`the case drew no ${selector}: ${html}`);
  return control;
}

/** Draws `html` on the page, and returns the one button it holds. */
function drawButton(html: string): Element {
  return drawControl(html, "button");
}

/** Draws `html` on the page, and takes it off again with its one button. */
function removedButton(html: string): Element {
  const button = drawButton(html);
  drawn.pop()?.remove();
  return button;
}

/** The element around `control`, itself included, that its case marks `data-${mark}`. */
function marked(control: Element, mark: "bound" | "shut"): Element {
  const found = control.closest(`[data-${mark}]`);
  if (found === null) throw new Error(`the case marked no ${mark} around its control`);
  return found;
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
test("an SVG link can hold no focus and takes no press", () => {
  const link = drawControl(SVG_LINK, "svg a");
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

// Inside the closed drawer the press rule makes the focus rule's own tests, reading the control
// and the elements between it and the `aside` itself, since `canHoldFocus` refuses everything
// there: each of them still refuses there, and only whether the control is drawn is not asked.
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
  ["an SVG link in the closed Details drawer", () => drawControl(closedDetails(SVG_LINK), "svg a")],
] as const)("%s takes no press", (_element, draw) => {
  expect(usableControl(draw()), "the element takes a press").toBe(false);
});

// Told a bound, `canHoldFocus` does not read the bound or what is above it, which its caller may
// have shut on purpose: a button there holds the focus within the bound, though it holds none
// with no bound named.
test.each([
  ["a button in a hidden bound", `<div data-bound hidden>${SAVE}</div>`],
  ["a button in an inert bound", `<div data-bound inert>${SAVE}</div>`],
  ["a button in a bound under a hidden element", `<div hidden><div data-bound>${SAVE}</div></div>`],
] as const)("%s can hold the focus within the bound", (_element, html) => {
  const button = drawButton(html);
  expect(canHoldFocus(button), "the button can hold the focus with no bound").toBe(false);
  expect(
    canHoldFocus(button, marked(button, "bound")),
    "the button cannot hold the focus within the bound"
  ).toBe(true);
});

// Below the bound every element is still read: a button that is hidden or inert, or that is under
// an element that is, holds no focus within the bound, and holds it once that attribute is gone.
test.each([
  [
    "a hidden button",
    '<div data-bound hidden><button data-shut hidden type="button">Save</button></div>',
  ],
  [
    "a button under a hidden element",
    `<div data-bound hidden><div data-shut hidden>${SAVE}</div></div>`,
  ],
  [
    "a button under an inert element",
    `<div data-bound hidden><div data-shut inert>${SAVE}</div></div>`,
  ],
] as const)("%s in a hidden bound can hold no focus within it", (_element, html) => {
  const button = drawButton(html);
  const bound = marked(button, "bound");
  expect(canHoldFocus(button, bound), "the button can hold the focus within the bound").toBe(false);
  const shut = marked(button, "shut");
  shut.removeAttribute("hidden");
  shut.removeAttribute("inert");
  expect(
    canHoldFocus(button, bound),
    "the button cannot hold the focus within the bound once nothing below it is shut"
  ).toBe(true);
});

// A bound that does not hold the element bounds nothing: every element around it is read.
test("a button under a hidden element can hold no focus within a bound beside it", () => {
  const button = drawButton(`<div hidden><span data-beside></span>${SAVE}</div>`);
  const beside = button.parentElement?.querySelector("[data-beside]") ?? null;
  if (beside === null) throw new Error("the case drew nothing beside the button");
  expect(canHoldFocus(button, beside), "the button can hold the focus").toBe(false);
});

// The press rule reads a group its caller names as the bound as it reads the closed Details: the
// header's `Shift+P` presses the pin in the shut actions menu. Named by no caller, the same group
// is one more hidden element, and nothing under it takes a press.
test("a button in a shut group takes a press only where the group is named as its bound", () => {
  const button = drawButton(shutGroup(SAVE));
  expect(
    usableControl(button, marked(button, "bound")),
    "the button takes no press within the shut group named as its bound"
  ).toBe(true);
  expect(usableControl(button), "the button takes a press with no bound named").toBe(false);
});

// Within a shut group named as the bound, the control and each element between it and the group
// are still read, as in the closed Details: each of these refuses there, and the button takes a
// press once that attribute is taken off.
test.each([
  [
    "an aria-disabled button",
    '<button aria-disabled="true" data-shut type="button">Save</button>',
    "aria-disabled",
  ],
  [
    "a button under an aria-hidden element",
    `<div aria-hidden="true" data-shut>${SAVE}</div>`,
    "aria-hidden",
  ],
  ["a button under a hidden element", `<div data-shut hidden>${SAVE}</div>`, "hidden"],
  ["a button under an inert element", `<div data-shut inert>${SAVE}</div>`, "inert"],
] as const)(
  "%s in a shut group named as its bound takes no press",
  (_element, inside, attribute) => {
    const button = drawButton(shutGroup(inside));
    const bound = marked(button, "bound");
    expect(usableControl(button, bound), "the button takes a press").toBe(false);
    marked(button, "shut").removeAttribute(attribute);
    expect(
      usableControl(button, bound),
      `the button takes no press once its ${attribute} is taken off`
    ).toBe(true);
  }
);
