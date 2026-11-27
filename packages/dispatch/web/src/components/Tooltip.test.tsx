import { afterEach, expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";
import { AnchoredPanel } from "./AnchoredPanel";
import { Tooltip, useTooltip } from "./Tooltip";

/** A control with a tooltip between two plain buttons. `paused` stands for a column drag. */
function Host({ paused = false, last = false }: { paused?: boolean; last?: boolean }): ReactNode {
  const anchorRef = useRef<HTMLButtonElement>(null);
  const tooltip = useTooltip({ paused });
  return (
    <>
      <button type="button">Before</button>
      <button {...tooltip.props} ref={anchorRef} type="button">
        Sort
      </button>
      <Tooltip anchorRef={anchorRef} trigger={tooltip}>
        Sort by this column
      </Tooltip>
      {last ? null : <button type="button">After</button>}
    </>
  );
}

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
});

function draw(tree: ReactNode) {
  const view = render(tree);
  restores.push(() => view.unmount());
  return view;
}

const anchor = () => screen.getByRole("button", { name: "Sort" });

test("a hover opens the tooltip, which the control names, and the pointer leaving closes it", async () => {
  const user = userEvent.setup();
  draw(<Host />);
  expect(screen.queryByRole("tooltip")).toBeNull();
  await user.hover(anchor());
  const tip = screen.getByRole("tooltip");
  expect(tip).toHaveTextContent("Sort by this column");
  expect(anchor()).toHaveAttribute("aria-describedby", tip.id);
  expect(anchor()).toHaveAccessibleDescription("Sort by this column");
  // It takes no focus: the reader stays where they were.
  expect(document.activeElement).toBe(document.body);
  await user.unhover(anchor());
  expect(screen.queryByRole("tooltip")).toBeNull();
  expect(anchor()).not.toHaveAttribute("aria-describedby");
});

test("keyboard focus opens the tooltip, the focus leaving closes it, and Escape closes it with the focus kept", async () => {
  const user = userEvent.setup();
  draw(<Host />);
  await user.tab();
  await user.tab();
  expect(anchor()).toHaveFocus();
  expect(screen.getByRole("tooltip")).toHaveTextContent("Sort by this column");
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("tooltip")).toBeNull();
  expect(anchor()).toHaveFocus();
  await user.tab({ shift: true });
  await user.tab();
  expect(screen.getByRole("tooltip")).toBeDefined();
  // Tab goes on to the next control of the page, not into the tooltip, which closes.
  await user.tab();
  expect(screen.getByRole("button", { name: "After" })).toHaveFocus();
  expect(screen.queryByRole("tooltip")).toBeNull();
});

test("Escape closes a tooltip a hover opened while the focus is on another control", async () => {
  const user = userEvent.setup();
  draw(<Host />);
  const before = screen.getByRole("button", { name: "Before" });
  act(() => before.focus());
  await user.hover(anchor());
  expect(screen.getByRole("tooltip")).toBeDefined();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("tooltip")).toBeNull();
  expect(before).toHaveFocus();
});

/** A filter's panel, open with the focus in it, beside a control whose tooltip a hover opens. */
function PanelBesideTooltip(): ReactNode {
  const triggerRef = useRef<HTMLButtonElement>(null);
  const anchorRef = useRef<HTMLButtonElement>(null);
  const tooltip = useTooltip();
  const [open, setOpen] = useState(true);
  return (
    <>
      <button ref={triggerRef} type="button">
        Filter
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={triggerRef}
          className=""
          label="Filter"
          onDismiss={() => setOpen(false)}
          role="dialog"
        >
          <button type="button">Inside</button>
        </AnchoredPanel>
      ) : null}
      <button {...tooltip.props} ref={anchorRef} type="button">
        Sort
      </button>
      <Tooltip anchorRef={anchorRef} trigger={tooltip}>
        Sort by this column
      </Tooltip>
    </>
  );
}

// INFRA-3221's tester: an open panel takes Escape at the document in the capture phase and stops
// it there, so a tooltip that read Escape as it bubbled stayed open until a second press.
test("one Escape closes an open panel and a tooltip a hover opened beside it, and goes no further", async () => {
  const user = userEvent.setup();
  const behind: string[] = [];
  const hear = (event: KeyboardEvent) => behind.push(event.key);
  window.addEventListener("keydown", hear);
  restores.push(() => window.removeEventListener("keydown", hear));
  draw(<PanelBesideTooltip />);
  expect(screen.getByRole("button", { name: "Inside" })).toHaveFocus();
  await user.hover(anchor());
  expect(screen.getByRole("tooltip")).toBeDefined();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
  expect(screen.queryByRole("tooltip")).toBeNull();
  // The Escape is still the panel's alone: the page behind it hears none, and the reader is back
  // on the panel's trigger.
  expect(behind).toEqual([]);
  expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
});

test("an open tooltip adds no Tab stop to the page and takes no focus from a press on it", async () => {
  const user = userEvent.setup();
  draw(<Host last />);
  await user.tab();
  await user.tab();
  expect(anchor()).toHaveFocus();
  const tip = screen.getByRole("tooltip");
  // Every Tab stop on the page is one of its own two buttons: the tooltip draws none.
  expect([...document.querySelectorAll('[tabindex]:not([tabindex="-1"])')]).toEqual([]);
  expect(tip.parentElement).not.toHaveAttribute("tabindex");
  act(() => {
    tip.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
  });
  expect(anchor()).toHaveFocus();
});

test("a column drag shuts the tooltip, and no hover opens it until the drop", async () => {
  const user = userEvent.setup();
  const view = draw(<Host />);
  await user.hover(anchor());
  expect(screen.getByRole("tooltip")).toBeDefined();
  view.rerender(<Host paused />);
  expect(screen.queryByRole("tooltip")).toBeNull();
  await user.unhover(anchor());
  await user.hover(anchor());
  expect(screen.queryByRole("tooltip")).toBeNull();
  expect(anchor()).not.toHaveAttribute("aria-describedby");
  // The drop opens nothing by itself; the next hover does.
  view.rerender(<Host />);
  expect(screen.queryByRole("tooltip")).toBeNull();
  await user.unhover(anchor());
  await user.hover(anchor());
  expect(screen.getByRole("tooltip")).toBeDefined();
});

/** Lays the anchor out at `box` and the tooltip's box at `width` by `height`: the unit DOM lays
 *  nothing out. */
function layOut(
  element: HTMLElement,
  box: { left: number; top: number; width: number; height: number },
  size: { width: number; height: number }
): void {
  const scrollHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  const offsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  const isTooltip = (node: HTMLElement) =>
    node.firstElementChild?.getAttribute("role") === "tooltip";
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return isTooltip(this) ? size.height : 0;
    },
  });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return isTooltip(this) ? size.width : 0;
    },
  });
  element.getBoundingClientRect = () =>
    new DOMRect(box.left, box.top, box.width, box.height) as DOMRect;
  restores.push(() => {
    if (scrollHeight === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", scrollHeight);
    if (offsetWidth === undefined) Reflect.deleteProperty(HTMLElement.prototype, "offsetWidth");
    else Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidth);
  });
}

const placeOf = () => {
  const panel = screen.getByRole("tooltip").parentElement;
  return { left: panel?.style.left, top: panel?.style.top };
};

test("the tooltip stands under its control, 8 px inside each edge of the window", async () => {
  const user = userEvent.setup();
  draw(<Host />);
  layOut(anchor(), { height: 28, left: 100, top: 100, width: 60 }, { height: 40, width: 200 });
  await user.hover(anchor());
  expect(placeOf()).toEqual({ left: "100px", top: "132px" });
  await user.unhover(anchor());

  // Near the right edge it moves left, to end 8 px inside the window.
  layOut(
    anchor(),
    { height: 28, left: window.innerWidth - 40, top: 100, width: 30 },
    { height: 40, width: 200 }
  );
  await user.hover(anchor());
  expect(placeOf()).toEqual({ left: `${window.innerWidth - 200 - 8}px`, top: "132px" });
  await user.unhover(anchor());

  // At the left edge it starts 8 px inside the window.
  layOut(anchor(), { height: 28, left: 2, top: 100, width: 30 }, { height: 40, width: 200 });
  await user.hover(anchor());
  expect(placeOf()).toEqual({ left: "8px", top: "132px" });
});

test("with no room under its control the tooltip stands above it, never over it", async () => {
  const user = userEvent.setup();
  draw(<Host />);
  const top = window.innerHeight - 60;
  layOut(anchor(), { height: 28, left: 100, top, width: 60 }, { height: 120, width: 200 });
  await user.hover(anchor());
  // Its bottom 4 px above the control's top.
  expect(placeOf()).toEqual({ left: "100px", top: `${top - 4 - 120}px` });
});
