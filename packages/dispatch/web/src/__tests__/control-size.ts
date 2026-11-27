/**
 * What a control's size draws, read from the computed style. A shared control takes the page's
 * size (`ControlSize`) and writes it in Tailwind's utilities: the padding and the 44 px touch
 * target of `controlPadding` (`components/control-names.ts`), or a map of its own beside it, as
 * the column filter's funnel keeps the target alone. happy-dom applies a stylesheet it is given but
 * loads none of the application's generated CSS, so a case puts Tailwind's own definitions of
 * those utilities in the document and reads what they compute. A case that reads only which size
 * a control was given reads its classes instead (`drawnSize`).
 */
import { type ControlSize, controlPadding } from "../components/control-names";

/** The size `control` draws (`controlPadding`): the one whose every class it carries, else
 *  none of the two. */
export function drawnSize(control: Element): ControlSize | "a size of its own" {
  const drawn = control.className.split(/\s+/);
  const sizes: ControlSize[] = ["compact", "comfortable"];
  return (
    sizes.find((size) => controlPadding[size].split(" ").every((name) => drawn.includes(name))) ??
    "a size of its own"
  );
}

/** Tailwind's definitions of the utilities a size draws below `md`: the 44 px target and each
 *  padding a control's size takes. */
export const SIZE_CSS =
  ".min-h-11{min-height:2.75rem}.min-w-11{min-width:2.75rem}.p-1{padding:0.25rem}" +
  ".px-2{padding-left:0.5rem;padding-right:0.5rem}.px-3{padding-left:0.75rem;padding-right:0.75rem}" +
  ".px-4{padding-left:1rem;padding-right:1rem}.py-1{padding-top:0.25rem;padding-bottom:0.25rem}" +
  ".py-2{padding-top:0.5rem;padding-bottom:0.5rem}";

/** Tailwind's `md:` sizes as they apply from 768 px up, where a mouse is the pointer and the
 *  compact size gives the 44 px target up for 32 px. Read after `SIZE_CSS`, as Tailwind orders
 *  them. */
export const MOUSE_CSS = String.raw`.md\:min-h-8{min-height:2rem}.md\:min-w-8{min-width:2rem}`;

/** What one control's size draws under a stylesheet. happy-dom computes each length in px. */
export interface DrawnSize {
  readonly minHeight: string;
  readonly minWidth: string;
  /** Top, right, bottom and left. */
  readonly padding: string;
}

/** The size `element` draws with `css` in the document, which is taken out again afterwards. */
export function sizeUnder(element: Element, css: string): DrawnSize {
  const sheet = document.createElement("style");
  sheet.textContent = css;
  document.head.appendChild(sheet);
  try {
    const style = getComputedStyle(element);
    return {
      minHeight: style.minHeight,
      minWidth: style.minWidth,
      padding: [style.paddingTop, style.paddingRight, style.paddingBottom, style.paddingLeft].join(
        " "
      ),
    };
  } finally {
    sheet.remove();
  }
}
