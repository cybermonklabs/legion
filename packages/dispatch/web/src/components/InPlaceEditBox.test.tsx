import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { type InPlaceBox, InPlaceEditBox, inPlaceBoxRules } from "./InPlaceEditBox";

/**
 * The classes that say what box an element draws: its padding, its border and its type metrics,
 * at every breakpoint. One box rule applied to both views means these are equal on the two, and
 * that is what a caller cannot work around - it passes no `className` at all.
 */
const METRIC =
  /^(?:sm:|md:|lg:|xl:|2xl:)?(?:p|px|py|pt|pb|pl|pr)-|^(?:sm:|md:|lg:|xl:|2xl:)?(?:border|rounded|leading-|font-|tracking-|min-w-|max-w-)|^(?:sm:|md:|lg:|xl:|2xl:)?text-(?:\[|xs|sm|base|lg|xl)/;

function metrics(element: Element): string[] {
  return [...element.classList].filter((token) => METRIC.test(token)).sort();
}

function boxRuleTokens(box: InPlaceBox): string[] {
  return inPlaceBoxRules[box].split(/\s+/).filter((token) => token !== "");
}

/** The title's editor: an edit view that fits the read view's box and covers it. */
function CoverHost({ disabled }: { disabled?: boolean }): ReactNode {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("A title that wraps onto two lines of the header");
  return (
    <InPlaceEditBox
      box="title"
      className="flex-1"
      disabled={disabled}
      editing={editing}
      input={{
        "aria-label": "Issue title",
        onBlur: () => setEditing(false),
        onChange: (event) => setValue(event.target.value),
        value,
      }}
      name="title"
      onOpen={() => setEditing(true)}
      overlay="cover"
      read={(box) => <h1 {...box}>{value}</h1>}
    />
  );
}

/** The route and parent editors: an edit view that cannot fit that box, so it opens over it. */
function PanelHost({ onDismiss }: { onDismiss?: () => void }): ReactNode {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("role:a");
  return (
    <>
      <button type="button">Elsewhere in the header</button>
      <InPlaceEditBox
        box="rail"
        editing={editing}
        name="route"
        onDismiss={() => {
          onDismiss?.();
          setEditing(false);
        }}
        onOpen={() => setEditing(true)}
        onSubmit={(event) => {
          event.preventDefault();
          setEditing(false);
        }}
        overlay="panel"
        read={(box) => (
          <button {...box} aria-label="Messages default to role:a" type="button">
            {value}
          </button>
        )}
      >
        <label htmlFor="route-field">Message route:</label>
        <input id="route-field" onChange={(event) => setValue(event.target.value)} value={value} />
        <button type="submit">Save route</button>
      </InPlaceEditBox>
    </>
  );
}

function cellOf(name: string): HTMLElement {
  const cell = document.querySelector<HTMLElement>(`[data-in-place-edit="${name}"]`);
  if (cell === null) {
    throw new Error(`no in-place edit cell named ${name}`);
  }
  return cell;
}

test("the read view and the edit view share one grid cell, with the edit view out of the flow", async () => {
  const user = userEvent.setup();
  const view = render(<CoverHost />);
  try {
    const cell = cellOf("title");
    const heading = screen.getByRole("heading", { level: 1 });
    expect(heading.parentElement).toBe(cell);
    expect(cell).toHaveClass("grid");
    expect(heading).toHaveClass("col-start-1", "row-start-1");

    await user.click(heading);
    const input = screen.getByLabelText("Issue title");
    expect(input.parentElement).toBe(cell);
    expect(input).toHaveClass("col-start-1", "row-start-1");
    // Out of the flow, so the read view alone sizes the cell in both states: an `input` in the
    // flow brings its own default width of about twenty characters.
    expect(input).toHaveClass("absolute");
    expect(heading).toHaveClass("col-start-1", "row-start-1");
  } finally {
    view.unmount();
  }
});

test("one box rule is applied to the read view and the edit view alike", async () => {
  const user = userEvent.setup();
  const view = render(<CoverHost />);
  try {
    const heading = screen.getByRole("heading", { level: 1 });
    await user.click(heading);
    const input = screen.getByLabelText("Issue title");
    for (const token of boxRuleTokens("title")) {
      expect(heading).toHaveClass(token);
      expect(input).toHaveClass(token);
    }
    // Neither view carries a padding, border or type class the other lacks, which is what no
    // caller can change: `InPlaceEditBox` takes no `className` for either view.
    expect(metrics(heading)).toEqual(metrics(input));
  } finally {
    view.unmount();
  }
});

test("the read view stays mounted, out of the accessibility tree and out of the tab order while the edit view is open", async () => {
  const user = userEvent.setup();
  const view = render(<CoverHost />);
  try {
    const heading = screen.getByRole("heading", { level: 1 });
    const title = heading.textContent;
    expect(heading.tabIndex).toBe(0);
    await user.click(heading);

    expect(heading).toBeInTheDocument();
    // It still holds the text that sizes the box, and it still keeps its space: invisible,
    // never `display: none`, which would hand the cell's size to the editor.
    expect(heading.textContent).toBe(title);
    expect(heading).toHaveClass("invisible");
    expect(heading).not.toHaveClass("hidden");
    expect(heading).toHaveAttribute("aria-hidden", "true");
    expect(heading).toHaveAttribute("inert");
    expect(heading.tabIndex).toBe(-1);
    // Out of the accessibility tree: the editor over it carries the same text.
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a read view that cannot be edited opens nothing and leaves the tab order", () => {
  const view = render(<CoverHost disabled />);
  try {
    const heading = screen.getByRole("heading", { level: 1 });
    expect(heading.tabIndex).toBe(-1);
    heading.click();
    expect(screen.queryByLabelText("Issue title")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("the cell keeps the floor of twenty characters in both states", async () => {
  const user = userEvent.setup();
  const view = render(<CoverHost />);
  try {
    const cell = cellOf("title");
    expect(cell).toHaveClass("min-w-[20ch]");
    await user.click(screen.getByRole("heading", { level: 1 }));
    expect(cell).toHaveClass("min-w-[20ch]");
  } finally {
    view.unmount();
  }
});

test("an edit view that cannot fit the box opens over the read view and costs the row no layout", async () => {
  const user = userEvent.setup();
  const view = render(<PanelHost />);
  try {
    const cell = cellOf("route");
    const trigger = screen.getByRole("button", { name: "Messages default to role:a" });
    expect(cell.contains(trigger)).toBe(true);
    for (const token of boxRuleTokens("rail")) {
      expect(trigger).toHaveClass(token);
    }

    await user.click(trigger);
    const editor = document.querySelector<HTMLElement>('[data-in-place-edit-editor="route"]');
    if (editor === null) {
      throw new Error("the panel holds no editor");
    }
    // Portalled and fixed: the row the read view sits in is not asked for a single pixel.
    expect(cell.contains(editor)).toBe(false);
    const panel = editor.parentElement;
    expect(panel?.style.position).toBe("fixed");
    expect(panel).toHaveClass("min-w-[20ch]");
    expect(editor.tagName).toBe("FORM");

    // The read view stays where it is, visible and reachable: it is the panel's anchor and the
    // control the reader pressed.
    expect(screen.getByRole("button", { name: "Messages default to role:a" })).toBe(trigger);
    expect(trigger).not.toHaveAttribute("aria-hidden");
    expect(trigger).not.toHaveAttribute("inert");
    expect(trigger).not.toHaveClass("invisible");
    expect(screen.getByLabelText("Message route:")).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Escape and a press outside the panel both reach the editor's own discard", async () => {
  const user = userEvent.setup();
  let dismissals = 0;
  const view = render(<PanelHost onDismiss={() => (dismissals += 1)} />);
  try {
    const trigger = screen.getByRole("button", { name: "Messages default to role:a" });
    await user.click(trigger);
    await user.keyboard("{Escape}");
    expect(dismissals).toBe(1);
    expect(document.querySelector('[data-in-place-edit-editor="route"]')).toBeNull();

    await user.click(trigger);
    expect(document.querySelector('[data-in-place-edit-editor="route"]')).not.toBeNull();
    await user.click(screen.getByRole("button", { name: "Elsewhere in the header" }));
    expect(dismissals).toBe(2);
    expect(document.querySelector('[data-in-place-edit-editor="route"]')).toBeNull();
  } finally {
    view.unmount();
  }
});
