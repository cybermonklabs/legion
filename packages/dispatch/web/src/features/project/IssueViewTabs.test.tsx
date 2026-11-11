import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { IssueViewTabs } from "./IssueViewTabs";
import type { BuiltInViewId } from "./issues-table-views";

const counts: Partial<Record<BuiltInViewId, number | string>> = {
  "in-flight": 4,
  mine: 1,
  open: 8,
  slots: "2/4",
  triage: 3,
  waiting: 2,
};

function tabs(
  search = "",
  hasLegion = true,
  tabCounts: Partial<Record<BuiltInViewId, number | string>> = counts
) {
  return render(
    <MemoryRouter initialEntries={[`/projects/ACME/issues${search}`]}>
      <IssueViewTabs counts={tabCounts} hasLegion={hasLegion} project="ACME" saved={[]} />
    </MemoryRouter>
  );
}

test("a Legion project shows seven view tabs and their supplied counts", () => {
  const view = tabs();

  try {
    expect(screen.getAllByRole("tab")).toHaveLength(7);
    const slots = screen.getByRole("tab", { name: "Slots 2/4" });
    const open = screen.getByRole("tab", { name: "All open 8" });
    expect(slots.querySelector("[data-tab-count]")).toHaveAttribute("data-tab-count", "2/4");
    expect(open.querySelector("[data-tab-count]")).toHaveAttribute("data-tab-count", "8");
  } finally {
    view.unmount();
  }
});

test("a project without Legion shows five applicable tabs and falls back to All open", () => {
  const view = tabs("?view=slots", false);

  try {
    expect(screen.getAllByRole("tab")).toHaveLength(5);
    expect(screen.getByRole("tab", { name: "All open 8" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    expect(screen.queryByRole("tab", { name: /Waiting line/ })).toBeNull();
    expect(screen.queryByRole("tab", { name: /Slots/ })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("Legion counts stay absent before the board answers", () => {
  const view = tabs("", true, { mine: 1, open: 8, triage: 3 });

  try {
    for (const name of ["Waiting line", "In flight", "Slots"]) {
      expect(screen.getByRole("tab", { name }).querySelector("[data-tab-count]")).toBeNull();
    }
  } finally {
    view.unmount();
  }
});

test("an overridden built-in view marks its active tab", () => {
  const view = tabs("?view=in-flight&f.stage=phase%3Aimplementing");

  try {
    const active = screen.getByRole("tab", { name: "In flight 4" });
    expect(active).toHaveAttribute("aria-selected", "true");
    expect(active.querySelector("[data-tab-marked]")).not.toBeNull();
  } finally {
    view.unmount();
  }
});
