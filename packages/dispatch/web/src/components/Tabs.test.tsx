import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the tablist's own scroll events below, which user-event has no form for
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { installResizeObserver, layOutScrollBox } from "../__tests__/scroll-box";
import { scrollFadeTrailing, scrollFadeTrailingPx } from "../theme/classes";
import { Tabs } from "./Tabs";

function TabsHarness() {
  const [activeTab, setActiveTab] = useState<"issues" | "documents">("issues");
  return (
    <>
      <Tabs
        activeTab={activeTab}
        ariaLabel="Project"
        idPrefix="project"
        onSelect={setActiveTab}
        tabs={[
          { id: "issues", label: "Issues" },
          { id: "documents", label: "Documents" },
        ]}
      />
      <output data-testid="active-tab">{activeTab}</output>
    </>
  );
}

test("tablist moves focus and selection with arrow, Home, and End keys and marks aria-selected", async () => {
  const user = userEvent.setup();
  const view = render(<TabsHarness />);

  try {
    const issues = screen.getByRole("tab", { name: "Issues" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    expect(issues).toHaveAttribute("aria-selected", "true");
    expect(documents).toHaveAttribute("aria-selected", "false");

    issues.focus();
    await user.keyboard("{End}");
    expect(documents).toHaveFocus();
    expect(documents).toHaveAttribute("aria-selected", "true");

    await user.keyboard("{Home}");
    expect(issues).toHaveFocus();
    expect(issues).toHaveAttribute("aria-selected", "true");

    await user.keyboard("{ArrowLeft}");
    expect(documents).toHaveFocus();
    expect(screen.getByTestId("active-tab").textContent).toBe("documents");
  } finally {
    view.unmount();
  }
});

test("a counted tab shows the count as a pill and is named `<label> <count>`", () => {
  const view = render(
    <Tabs
      activeTab="issues"
      ariaLabel="Project"
      idPrefix="project"
      onSelect={() => {}}
      tabs={[
        { id: "issues", label: "Issues" },
        { count: 3, id: "documents", label: "Documents" },
      ]}
    />
  );

  try {
    const documents = screen.getByRole("tab", { name: "Documents 3" });
    expect(documents.textContent).toBe("Documents 3");
    expect(screen.getByRole("tab", { name: "Issues" }).textContent).toBe("Issues");
  } finally {
    view.unmount();
  }
});

function rect(left: number, right: number): DOMRect {
  return {
    bottom: 44,
    height: 44,
    left,
    right,
    toJSON: () => ({}),
    top: 0,
    width: right - left,
    x: left,
    y: 0,
  };
}

test("a tab reached from the keyboard is scrolled into the tablist, without moving the page", async () => {
  const user = userEvent.setup();
  const view = render(<TabsHarness />);

  try {
    const tablist = screen.getByRole("tablist", { name: "Project" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    // A 300 px tablist whose second and last tab overhangs its right edge by 60 px; the tab's
    // rect follows the tablist's scroll like a real layout would.
    layOutScrollBox(tablist, { client: 300, scroll: 360 });
    tablist.getBoundingClientRect = () => rect(0, 300);
    documents.getBoundingClientRect = () =>
      rect(200 - tablist.scrollLeft, 360 - tablist.scrollLeft);
    tablist.scrollLeft = 0;
    const scrollY = window.scrollY;

    // `user.keyboard` sends to the focused element, and a click here would select the tab
    // rather than reach it from the keyboard, which is what this row measures.
    screen.getByRole("tab", { name: "Issues" }).focus();
    await user.keyboard("{ArrowRight}");

    expect(documents).toHaveFocus();
    expect(tablist.scrollLeft).toBe(60);
    expect(window.scrollY).toBe(scrollY);
  } finally {
    view.unmount();
  }
});

// The issue page's ten tabs outrun the tablist at 1440 px with the comment margin open, and the
// tabs past its edge are cut off at a tab's boundary, which reads as the whole list. The tablist
// wears the trailing fade the issue header's details rail wears, on the same terms.
test("a tablist with tabs past its trailing edge wears the trailing fade, and one that fits or is scrolled to its end does not", () => {
  const view = render(<TabsHarness />);

  try {
    const tablist = screen.getByRole("tablist", { name: "Project" });
    const box = { client: 300, scroll: 300 };
    layOutScrollBox(tablist, box);
    // The tablist's own scroll event, which user-event has no form for, has the hook measure it.
    fireEvent.scroll(tablist);
    expect(tablist).not.toHaveClass(scrollFadeTrailing);

    box.scroll = 360;
    // Tabs now lie past the edge; the next scroll event (no user-event form) measures them.
    fireEvent.scroll(tablist);
    expect(tablist).toHaveClass(scrollFadeTrailing);

    // At the end of the scroll the last tab is all there is to see; a fade there would hide it.
    tablist.scrollLeft = 60;
    fireEvent.scroll(tablist);
    expect(tablist).not.toHaveClass(scrollFadeTrailing);

    // The reader scrolls back to the start: a scroll event, which user-event has no form for.
    tablist.scrollLeft = 0;
    fireEvent.scroll(tablist);
    expect(tablist).toHaveClass(scrollFadeTrailing);
  } finally {
    view.unmount();
  }
});

test("a tab reached from the keyboard stops clear of the trailing fade while tabs lie past it", async () => {
  const user = userEvent.setup();
  const view = render(
    <Tabs
      activeTab="issues"
      ariaLabel="Project"
      idPrefix="project"
      onSelect={() => {}}
      tabs={[
        { id: "issues", label: "Issues" },
        { id: "documents", label: "Documents" },
        { id: "settings", label: "Settings" },
      ]}
    />
  );

  try {
    const tablist = screen.getByRole("tablist", { name: "Project" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    // Documents ends 10 px inside a 300 px tablist, under the fade's last 32 px, and Settings
    // runs on past the edge.
    layOutScrollBox(tablist, { client: 300, scroll: 420 });
    tablist.getBoundingClientRect = () => rect(0, 300);
    documents.getBoundingClientRect = () =>
      rect(180 - tablist.scrollLeft, 290 - tablist.scrollLeft);

    screen.getByRole("tab", { name: "Issues" }).focus();
    await user.keyboard("{ArrowRight}");

    expect(documents).toHaveFocus();
    expect(documents.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);
  } finally {
    view.unmount();
  }
});

// The issue page's comment margin opens with no change of tab, and the tablist narrows under the
// selected tab: it is brought back whole and clear of the fade then too, not only when it is
// chosen.
test("a selected tab the tablist narrows over is scrolled back clear of the trailing fade", () => {
  const sizes = installResizeObserver();
  const view = render(
    <Tabs
      activeTab="documents"
      ariaLabel="Project"
      idPrefix="project"
      onSelect={() => {}}
      tabs={[
        { id: "issues", label: "Issues" },
        { id: "documents", label: "Documents" },
        { id: "settings", label: "Settings" },
      ]}
    />
  );

  try {
    const tablist = screen.getByRole("tablist", { name: "Project" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    // The tabs fit a 420 px tablist; Documents sits at 180 to 290.
    const box = { client: 420, scroll: 420 };
    let right = 420;
    layOutScrollBox(tablist, box);
    tablist.getBoundingClientRect = () => rect(0, right);
    documents.getBoundingClientRect = () =>
      rect(180 - tablist.scrollLeft, 290 - tablist.scrollLeft);
    sizes.resize();
    expect(tablist.scrollLeft).toBe(0);

    // The tablist narrows to 300 px: Documents ends under the fade, and Settings runs past it.
    box.client = 300;
    right = 300;
    sizes.resize();
    expect(documents.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);
  } finally {
    view.unmount();
    sizes.restore();
  }
});
