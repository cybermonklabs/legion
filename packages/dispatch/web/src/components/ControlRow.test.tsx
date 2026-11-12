import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { ControlRow } from "./ControlRow";

/**
 * Tailwind's own definitions of the four utilities the row's height rule is written in. happy-dom
 * applies a stylesheet it is given but loads none of the application's generated CSS, so the rule
 * is read from the computed style with these in the document: a row that stopped holding one line,
 * or a slot that started shrinking, computes something else here whatever class it carries.
 */
const HEIGHT_RULE_CSS =
  ".flex{display:flex}.flex-wrap{flex-wrap:wrap}.flex-nowrap{flex-wrap:nowrap}.shrink-0{flex-shrink:0}.flex-1{flex:1 1 0%}.gap-2{gap:0.5rem}";
/** Tailwind's `sm:` utilities as they apply from 640 px up, which is where R1's two widths
 *  (1440 and 1280) are. Below it the row and the page's own slot wrap, so that a phone's search
 *  box is a search box and a phone's own controls stay on the screen. */
const DESKTOP_CSS = String.raw`.sm\:flex-nowrap{flex-wrap:nowrap}.sm\:shrink-0{flex-shrink:0}`;

function Row() {
  return (
    <ControlRow
      chips={<span data-testid="chips" />}
      display={<span data-testid="display" />}
      search={<span data-testid="search" />}
      trigger={<span data-testid="trigger" />}
    >
      <span data-testid="own" />
    </ControlRow>
  );
}

function row(): HTMLElement {
  const found = document.querySelector<HTMLElement>('[data-control="control-row"]');
  if (found === null) throw new Error("no control row is drawn");
  return found;
}

/** The box the row drew around what a page handed one of its slots. */
function slotOf(testId: string): HTMLElement {
  const box = screen.getByTestId(testId).parentElement;
  if (box === null) throw new Error(`${testId} is drawn outside a slot of its own`);
  return box;
}

test("the slots are drawn in the row's own order, whatever order the page names them in", () => {
  const view = render(<Row />);
  try {
    expect(row()).toHaveAttribute("data-control", "control-row");
    const drawn = Array.from(row().querySelectorAll("[data-testid]")).map((node) =>
      node.getAttribute("data-testid")
    );
    expect(drawn).toEqual(["search", "trigger", "chips", "own", "display"]);
  } finally {
    view.unmount();
  }
});

test("a slot the page leaves out is no empty box in the row", () => {
  const view = render(<ControlRow search={<span data-testid="search" />}>{null}</ControlRow>);
  try {
    expect(row().children.length).toBe(1);
    expect(row().children[0]).toBe(slotOf("search"));
  } finally {
    view.unmount();
  }
});

test("a row with chips and one without differ by at most the chips' own extra line", () => {
  const style = document.createElement("style");
  style.textContent = `${HEIGHT_RULE_CSS}${DESKTOP_CSS}`;
  document.head.appendChild(style);
  const view = render(<Row />);
  try {
    // The row itself is one line from `sm` up: no slot of it is ever pushed under another.
    expect(getComputedStyle(row()).flexWrap).toBe("nowrap");
    // 8 px between the slots, and inside the chips box between one line of chips and the next:
    // with a control 44 px tall, a second line of chips costs 52 px and nothing else costs any.
    expect(getComputedStyle(row()).gap).toBe("0.5rem");

    const chips = slotOf("chips");
    expect(getComputedStyle(chips).flexWrap).toBe("wrap");
    expect(getComputedStyle(chips).gap).toBe("0.5rem");
    // And it is the only box in the row that may take a second line.
    const wrapping = Array.from(row().querySelectorAll("*")).filter(
      (node) => getComputedStyle(node).flexWrap === "wrap"
    );
    expect(wrapping.length).toBe(1);
    expect(wrapping[0]).toBe(chips);

    // The search box is the one slot that gives way where the window is narrow, as it does on
    // every page today; it still lays its own nodes out in one line.
    expect(getComputedStyle(slotOf("search")).flexGrow).toBe("1");
    expect(getComputedStyle(slotOf("search")).flexWrap).toBe("nowrap");
    for (const testId of ["trigger", "own", "display"]) {
      expect(getComputedStyle(slotOf(testId)).flexShrink).toBe("0");
      expect(getComputedStyle(slotOf(testId)).flexWrap).toBe("nowrap");
    }
  } finally {
    view.unmount();
    style.remove();
  }
});

test("below the phone breakpoint the row and the page's own controls wrap onto the screen", () => {
  const style = document.createElement("style");
  // The same utilities with no `sm:` rule applied, which is a window narrower than 640 px.
  style.textContent = HEIGHT_RULE_CSS;
  document.head.appendChild(style);
  const view = render(<Row />);
  try {
    // A 390 px phone cannot hold the search box, the trigger, the chips, the page's own
    // controls and the display menu on one line, so the row wraps rather than running past the
    // right edge with a control the reader cannot reach (the Artifacts tab's staged upload).
    expect(getComputedStyle(row()).flexWrap).toBe("wrap");
    expect(getComputedStyle(slotOf("own")).flexWrap).toBe("wrap");
    expect(getComputedStyle(slotOf("own")).flexShrink).not.toBe("0");
  } finally {
    view.unmount();
    style.remove();
  }
});
