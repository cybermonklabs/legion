import { expect, spyOn, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { Tabs } from "./Tabs";

function TabsHarness({ initialTab = "issues" }: { initialTab?: "issues" | "documents" } = {}) {
  const [activeTab, setActiveTab] = useState<"issues" | "documents">(initialTab);
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
    // A 300 px tablist whose second tab overhangs its right edge by 60 px; the tab's rect
    // follows the tablist's scroll like a real layout would.
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

// A rectangle read inside the commit that mounted the tablist forces the whole page's style and
// layout into that task: on a phone at 4x CPU throttling those two reads were 170-200 ms of the
// one task that mounts an issue page, which is already over the 500 ms a reader feels as a
// freeze (dispatch://INFRA-341, R11). So the reveal waits for a frame, where the layout it reads
// is the one the browser was about to compute, and still scrolls before that frame is painted.
test("the active tab is revealed in a frame of its own, never in the commit that mounted it", async () => {
  let measured = 0;
  // The same 300 px tablist as the row above, with its second tab overhanging by 60 px, measured
  // wherever the reveal asks; `measured` is what says which task asked.
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ): DOMRect {
    measured += 1;
    if (this.getAttribute("role") === "tablist") {
      return rect(0, 300);
    }
    const scrolled = this.parentElement?.scrollLeft ?? 0;
    return rect(200 - scrolled, 360 - scrolled);
  });
  const view = render(<TabsHarness initialTab="documents" />);

  try {
    expect(measured).toBe(0);
    const tablist = screen.getByRole("tablist", { name: "Project" });
    expect(tablist.scrollLeft).toBe(0);

    const painted = Promise.withResolvers<void>();
    requestAnimationFrame(() => painted.resolve());
    await painted.promise;

    expect(measured).toBeGreaterThan(0);
    expect(tablist.scrollLeft).toBe(60);
  } finally {
    boxes.mockRestore();
    view.unmount();
  }
});
