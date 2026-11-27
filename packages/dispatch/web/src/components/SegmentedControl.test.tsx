import { expect, mock, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { SegmentedControl, type SegmentedControlProps } from "./SegmentedControl";

/** The Inbox's switch: its two parts, the second carrying the asks that wait for the reader. */
const INBOX: SegmentedControlProps = {
  label: "Inbox section",
  onChange: () => undefined,
  parts: [
    { label: "All", value: "all" },
    { count: 3, label: "Blocked on you", value: "needs-you" },
  ],
  value: "all",
};

/** The Issues tab's switch: its two layouts. */
const LAYOUTS: SegmentedControlProps = {
  label: "Layout",
  onChange: () => undefined,
  parts: [
    { label: "Table", value: "table" },
    { label: "Board", value: "board" },
  ],
  value: "table",
};

/** Each case runs with both switches, and each reads the second part, so a switch that drew its
 *  first part alone fails it by name. */
const SWITCHES = [
  ["All and Blocked on you", INBOX],
  ["Table and Board", LAYOUTS],
] as const;

/** The parts of the switch named `label`, in its order. */
function partsOf(label: string): HTMLElement[] {
  return within(screen.getByRole("group", { name: label })).getAllByRole("button");
}

/** A part's own words, its count after them. */
function wordsOf(part: (typeof INBOX.parts)[number]): string {
  return part.count === undefined || part.count === 0 ? part.label : `${part.label} ${part.count}`;
}

test.each(
  SWITCHES
)("with %s, the switch sets its own data-control and draws every part", (_name, props) => {
  const view = render(<SegmentedControl {...props} />);
  try {
    const group = screen.getByRole("group", { name: props.label });
    expect(group).toHaveAttribute("data-control", "segmented-control");
    expect(partsOf(props.label).map((part) => part.textContent)).toEqual(props.parts.map(wordsOf));
  } finally {
    view.unmount();
  }
});

test.each(
  SWITCHES
)("with %s, a press on a part calls the page's handler with that part's value", async (_name, props) => {
  const user = userEvent.setup();
  const onChange = mock((_value: string) => undefined);
  const view = render(<SegmentedControl {...props} onChange={onChange} />);
  try {
    for (const part of partsOf(props.label)) await user.click(part);
    expect(onChange.mock.calls.map(([value]) => value)).toEqual(
      props.parts.map((part) => part.value)
    );
  } finally {
    view.unmount();
  }
});

test.each(
  SWITCHES
)("with %s, the chosen part is exposed as chosen to a screen reader, and differs from the other in its surface and its weight", (_name, props) => {
  const second = props.parts[1];
  const view = render(<SegmentedControl {...props} value={second.value} />);
  try {
    const parts = partsOf(props.label);
    expect(parts.map((part) => part.getAttribute("aria-pressed"))).toEqual(["false", "true"]);
    // Two differences and not a colour alone: a filled surface, and a heavier label.
    const [first, chosen] = parts.map((part) => part.className.split(/\s+/));
    expect(chosen).toContain("font-semibold");
    expect(first).toContain("font-medium");
    expect(first).toContain("bg-transparent");
    expect(chosen?.filter((name) => name.startsWith("bg-"))).not.toEqual(
      first?.filter((name) => name.startsWith("bg-"))
    );
  } finally {
    view.unmount();
  }
});

test("a part shows its count beside its label, and none while the count is not known or at zero", () => {
  const [all, blocked] = INBOX.parts;
  const view = render(<SegmentedControl {...INBOX} />);
  try {
    expect(screen.getByRole("button", { name: "Blocked on you 3" })).toHaveTextContent(
      "Blocked on you 3"
    );
    for (const count of [undefined, 0]) {
      view.rerender(<SegmentedControl {...INBOX} parts={[all, { ...blocked, count }]} />);
      expect(screen.getByRole("button", { name: "Blocked on you" }).textContent).toBe(
        "Blocked on you"
      );
    }
  } finally {
    view.unmount();
  }
});

test("a part given a short label draws it in the narrow layout and keeps its full accessible name", () => {
  const [all, blocked] = INBOX.parts;
  const title = "Blocked on you: 3 items, oldest 2 days";
  // With the page's title, as the Inbox names Blocked on you, and without one.
  for (const named of [title, undefined]) {
    const parts = [
      { ...all, short: "All" },
      { ...blocked, short: "You", title: named },
    ] as const;
    const drawn = () =>
      partsOf("Inbox section").map((part) => ({
        name: part.getAttribute("aria-label") ?? part.textContent,
        words: part.textContent,
      }));
    const name = named ?? "Blocked on you 3";
    const view = render(<SegmentedControl {...INBOX} parts={parts} />);
    try {
      expect(drawn()).toEqual([
        { name: "All", words: "All" },
        { name, words: "Blocked on you 3" },
      ]);
      view.rerender(<SegmentedControl {...INBOX} narrow parts={parts} />);
      expect(drawn()).toEqual([
        { name: "All", words: "All" },
        { name, words: "You · 3" },
      ]);
      expect(screen.getByRole("button", { name })).toHaveTextContent("You · 3");
    } finally {
      view.unmount();
    }
  }
});

test("a part given a title is named by it, as the Inbox names Blocked on you by what waits", () => {
  const [all, blocked] = INBOX.parts;
  const title = "Blocked on you: 3 items, oldest 2 days";
  const view = render(<SegmentedControl {...INBOX} parts={[all, { ...blocked, title }]} />);
  try {
    const part = screen.getByRole("button", { name: title });
    expect(part).toHaveAttribute("title", title);
    expect(part).toHaveTextContent("Blocked on you 3");
  } finally {
    view.unmount();
  }
});

test("the switch takes its size from the page", () => {
  const view = render(<SegmentedControl {...LAYOUTS} size="compact" />);
  try {
    for (const part of partsOf("Layout")) expect(part).toHaveClass("md:min-h-8");
    view.rerender(<SegmentedControl {...LAYOUTS} size="comfortable" />);
    for (const part of partsOf("Layout")) {
      expect(part).not.toHaveClass("md:min-h-8");
      expect(part).toHaveClass("min-h-11");
    }
  } finally {
    view.unmount();
  }
});
