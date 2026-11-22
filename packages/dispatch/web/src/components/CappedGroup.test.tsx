import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
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
    const more = screen.getByRole("button", { name: "Show 1 more labels" });
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
    expect(screen.getByRole("button", { name: "Show 1 more labels" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a cap below ten is the group's own, and the measure is told it", () => {
  const view = render(<Labels count={5} max={3} />);
  try {
    const group = view.container.querySelector<HTMLElement>('[data-layout-group="labels"]');
    expect(group).toHaveAttribute("data-layout-max", "3");
    expect(drawn(group as HTMLElement)).toEqual(labels(3));
    expect(screen.getByRole("button", { name: "Show 2 more labels" })).toHaveTextContent("+2");
  } finally {
    view.unmount();
  }
});
