import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { DisplayMenu, type DisplayMenuSection } from "./DisplayMenu";

const SORT: DisplayMenuSection = {
  id: "sort",
  label: "Sort by",
  onChange: () => undefined,
  options: [
    { label: "Newest first", value: "newest" },
    { label: "Urgency", value: "urgency" },
  ],
  value: "urgency",
};

const SHOW: DisplayMenuSection = {
  id: "show",
  label: "Show",
  onChange: () => undefined,
  options: [
    { label: "Inbox", value: "inbox" },
    { label: "Dismissed", value: "dismissed" },
  ],
  value: "inbox",
};

function Host({ sections }: { sections: readonly DisplayMenuSection[] }) {
  const [open, setOpen] = useState(false);
  return <DisplayMenu label="View" onOpenChange={setOpen} open={open} sections={sections} />;
}

test("the panel is drawn only while the menu is open, under the trigger's own control name", async () => {
  const user = userEvent.setup();
  const view = render(<Host sections={[SORT]} />);
  try {
    const trigger = screen.getByRole("button", { name: "View" });
    expect(trigger).toHaveAttribute("data-control", "display-menu");
    // One name per control: the display menu is found by its own, not by the button it is built
    // from, so a row looking for a page's display menu cannot land on a column filter.
    expect(document.querySelector('[data-control="panel-trigger"]')).toBeNull();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "View" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Sort by" })).toBeTruthy();

    await user.click(trigger);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("each section is a group of radios whose checked one is the section's value", async () => {
  const user = userEvent.setup();
  const view = render(<Host sections={[SORT, SHOW]} />);
  try {
    await user.click(screen.getByRole("button", { name: "View" }));

    const sort = within(screen.getByRole("group", { name: "Sort by" }));
    expect(sort.getByRole("radio", { name: "Urgency" })).toBeChecked();
    expect(sort.getByRole("radio", { name: "Newest first" })).not.toBeChecked();

    const show = within(screen.getByRole("group", { name: "Show" }));
    expect(show.getByRole("radio", { name: "Inbox" })).toBeChecked();
    expect(show.getByRole("radio", { name: "Dismissed" })).not.toBeChecked();

    // Two sections, two picks: choosing in one leaves the other's radio alone.
    expect(sort.getAllByRole("radio").length).toBe(2);
    expect(show.getAllByRole("radio").length).toBe(2);
  } finally {
    view.unmount();
  }
});

test("choosing an option calls that section's onChange with the option's value", async () => {
  const user = userEvent.setup();
  const sorts: string[] = [];
  const shows: string[] = [];
  const view = render(
    <Host
      sections={[
        { ...SORT, onChange: (value) => sorts.push(value) },
        { ...SHOW, onChange: (value) => shows.push(value) },
      ]}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "View" }));
    await user.click(screen.getByRole("radio", { name: "Newest first" }));
    expect(sorts).toEqual(["newest"]);
    expect(shows).toEqual([]);

    // A choice leaves the panel open: a reader setting the order usually sets the next thing too.
    expect(screen.getByRole("dialog", { name: "View" })).toBeTruthy();
    await user.click(screen.getByRole("radio", { name: "Dismissed" }));
    expect(shows).toEqual(["dismissed"]);
    expect(sorts).toEqual(["newest"]);
  } finally {
    view.unmount();
  }
});
