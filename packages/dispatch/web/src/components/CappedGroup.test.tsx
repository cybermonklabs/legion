import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { CappedGroup } from "./CappedGroup";

function labels(count: number) {
  return Array.from({ length: count }, (_, index) => `label-${index + 1}`);
}

/** The entries the group draws, in order, leaving out its "+N" control as the layout measure
 *  does (`data-layout-more`). */
function drawn(group: HTMLElement): string[] {
  return [...group.children]
    .filter((child) => !child.hasAttribute("data-layout-more"))
    .map((child) => child.textContent ?? "");
}

function Labels({ count, max }: { count: number; max?: number }) {
  return (
    <CappedGroup label="labels" max={max} name="labels">
      {labels(count).map((label) => (
        <span key={label}>{label}</span>
      ))}
    </CappedGroup>
  );
}

test("a group of ten draws all ten and no control", () => {
  const view = render(<Labels count={10} />);
  try {
    const group = view.container.querySelector<HTMLElement>('[data-layout-group="labels"]');
    expect(group).not.toBeNull();
    expect(drawn(group as HTMLElement)).toEqual(labels(10));
    expect(screen.queryByRole("button")).toBeNull();
    // Ten is the rule's own cap, which the measure assumes when the group names none.
    expect(group).not.toHaveAttribute("data-layout-max");
  } finally {
    view.unmount();
  }
});

test("a group of eleven draws ten and +1, which opens the rest in place and folds them back", async () => {
  const user = userEvent.setup();
  const view = render(<Labels count={11} />);
  try {
    const group = view.container.querySelector<HTMLElement>('[data-layout-group="labels"]');
    expect(drawn(group as HTMLElement)).toEqual(labels(10));
    const more = screen.getByRole("button", { name: "+1, show all 11 labels" });
    expect(more).toHaveTextContent("+1");
    expect(more).toHaveAttribute("aria-expanded", "false");
    expect(more).toHaveAttribute("aria-controls", group?.id ?? "");
    expect(more).toHaveAttribute("data-layout-more");
    expect(group).not.toHaveAttribute("data-layout-open");

    await user.click(more);
    expect(drawn(group as HTMLElement)).toEqual(labels(11));
    // The same button, after every entry, so the press keeps its focus.
    const fewer = screen.getByRole("button", { name: "Show fewer labels" });
    expect(fewer).toBe(more);
    expect(fewer).toHaveFocus();
    expect(fewer).toHaveTextContent("Show fewer");
    expect(fewer).toHaveAttribute("aria-expanded", "true");
    expect(group?.lastElementChild).toBe(fewer);
    expect(group).toHaveAttribute("data-layout-open");

    await user.click(fewer);
    expect(drawn(group as HTMLElement)).toEqual(labels(10));
    expect(screen.getByRole("button", { name: "+1, show all 11 labels" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// The closed control's name opens with the text it shows, so a voice reader who says "+2" presses
// it (WCAG 2.5.3), and counts the whole group, never a "1 more" that would need a singular.
test("a cap below ten is the group's own, the measure is told it, and the control's name starts with what it shows", () => {
  const view = render(<Labels count={5} max={3} />);
  try {
    const group = view.container.querySelector<HTMLElement>('[data-layout-group="labels"]');
    expect(group).toHaveAttribute("data-layout-max", "3");
    expect(drawn(group as HTMLElement)).toEqual(labels(3));
    const more = screen.getByRole("button", { name: "+2, show all 5 labels" });
    expect(more).toHaveTextContent("+2");
    expect(more.getAttribute("aria-label")?.startsWith(more.textContent ?? "")).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a group of four capped at three names its control +1, show all 4", () => {
  const view = render(<Labels count={4} max={3} />);
  try {
    const more = screen.getByRole("button", { name: "+1, show all 4 labels" });
    expect(more).toHaveTextContent("+1");
  } finally {
    view.unmount();
  }
});

// A cap above ten breaks the rule's cap, so the group declares it and the layout measure, which
// reads a declared cap outside 1-10 as a breach, sees it.
test("a cap above ten is declared to the measure too", () => {
  const view = render(<Labels count={12} max={11} />);
  try {
    const group = view.container.querySelector<HTMLElement>('[data-layout-group="labels"]');
    expect(group).toHaveAttribute("data-layout-max", "11");
    expect(drawn(group as HTMLElement)).toEqual(labels(11));
  } finally {
    view.unmount();
  }
});

test("given a list name, the group is a named list whose control sits in an item of its own", async () => {
  const user = userEvent.setup();
  const view = render(
    <CappedGroup label="agents" list="Selected agents" name="agents">
      {labels(12).map((label) => (
        <span key={label}>{label}</span>
      ))}
    </CappedGroup>
  );
  try {
    const list = screen.getByRole("list", { name: "Selected agents" });
    expect(list).toHaveAttribute("data-layout-group", "agents");
    // Ten entries and the item holding the control, which the measure leaves out by its mark.
    expect(drawn(list)).toEqual(labels(10));
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(11);
    const more = screen.getByRole("button", { name: "+2, show all 12 agents" });
    expect(more.parentElement).toBe(items[10] ?? null);
    expect(items[10]).toHaveAttribute("data-layout-more");
    expect(more).not.toHaveAttribute("data-layout-more");

    await user.click(more);
    expect(drawn(list)).toEqual(labels(12));
    expect(within(list).getAllByRole("listitem")).toHaveLength(13);
    expect(screen.getByRole("button", { name: "Show fewer agents" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});
