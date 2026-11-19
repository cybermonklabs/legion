import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the tablist's own scroll events below, which user-event has no form for
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { settle } from "../__tests__/fake-clock";
import {
  installResizeObserver,
  layOutScrollBox,
  type ResizeControl,
} from "../__tests__/scroll-box";
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

/** A tablist whose parent takes each selection at once, as both pages that use `Tabs` do: they
 *  navigate on select (`IssueTabs`, `ProjectPage`), so a keystroke that selects a tab also runs the
 *  effect that settles the selected tab. */
function SelectingTabs<Id extends string>({
  ariaLabel,
  idPrefix,
  initial,
  tabs,
}: {
  ariaLabel: string;
  idPrefix: string;
  initial: Id;
  tabs: readonly { count?: number; id: Id; label: string }[];
}) {
  const [activeTab, setActiveTab] = useState<Id>(initial);
  return (
    <Tabs
      activeTab={activeTab}
      ariaLabel={ariaLabel}
      idPrefix={idPrefix}
      onSelect={setActiveTab}
      tabs={tabs}
    />
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

    // The reader scrolls to the end: their wheel, then the strip's own scroll event, which
    // user-event has no form for. At the end of the scroll the last tab is all there is to see; a
    // fade there would hide it.
    fireEvent.wheel(tablist);
    tablist.scrollLeft = 60;
    fireEvent.scroll(tablist);
    expect(tablist).not.toHaveClass(scrollFadeTrailing);

    // The reader scrolls back to the start, by a wheel again.
    fireEvent.wheel(tablist);
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
    <SelectingTabs
      ariaLabel="Project"
      idPrefix="project"
      initial="issues"
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

// The issue page's ten tabs, as the visual row draws them at 1280 px with the comment margin open
// (read from the page in the pinned Playwright image): a 528 px tablist over 749 px of tabs, the
// selected Artifacts tab ending at 505, so one layout scrolls the strip 9 px to bring the tab clear
// of the trailing fade, and the baseline is shot there. While the page lays out, the tablist can be
// narrower for a moment; at 300 px Artifacts runs past the fade and the strip scrolls 237 px. Where
// the strip stops once the tablist is 528 px again must be where one layout at 528 px puts it, 9 px,
// whatever width it passed through first.
const issueTabs = [
  { id: "spec", label: "Spec" },
  { id: "verification", label: "Verification" },
  { id: "plan", label: "Plan" },
  { id: "steps", label: "Steps" },
  { id: "conversation", label: "Conversation" },
  { id: "children", label: "Children" },
  { count: 5, id: "artifacts", label: "Artifacts" },
  { id: "activity", label: "Activity" },
  { id: "decisions", label: "Decisions" },
  { id: "collaborators", label: "Collaborators" },
] as const;

function issueTablist(sizes: ResizeControl) {
  const tablist = screen.getByRole("tablist", { name: "Issue detail" });
  const artifacts = screen.getByRole("tab", { name: "Artifacts 5" });
  const activity = screen.getByRole("tab", { name: "Activity" });
  const box = { client: 528, scroll: 749 };
  layOutScrollBox(tablist, box);
  tablist.getBoundingClientRect = () => rect(0, box.client);
  artifacts.getBoundingClientRect = () => rect(410 - tablist.scrollLeft, 505 - tablist.scrollLeft);
  // Activity is the tab after Artifacts, 509 to 580 from the start of the scroll.
  activity.getBoundingClientRect = () => rect(509 - tablist.scrollLeft, 580 - tablist.scrollLeft);
  /** Lays the tablist out `client` px wide and tells its observers. A browser clamps the scroll to
   *  the new width as it lays the tablist out, before any observer runs, so this does too. */
  const layOut = (client: number) => {
    box.client = client;
    tablist.scrollLeft = Math.min(tablist.scrollLeft, Math.max(box.scroll - client, 0));
    sizes.resize();
  };
  /** The reader's own sideways scroll: their wheel, then the strip's scroll event, which user-event
   *  has no form for. */
  const scrollTo = (scrollLeft: number) => {
    fireEvent.wheel(tablist);
    tablist.scrollLeft = scrollLeft;
    fireEvent.scroll(tablist);
  };
  /** The browser clamping the strip to a layout no observer reports: the strip is laid out
   *  `wide` px wide and read, which clamps its scroll to that width, and laid out at its old width
   *  again inside the same task, so no size changed when the observers next look. Only the clamp's
   *  own scroll event is left. */
  const clampThrough = (wide: number) => {
    const client = box.client;
    box.client = wide;
    tablist.scrollLeft = Math.min(tablist.scrollLeft, Math.max(box.scroll - wide, 0));
    box.client = client;
    fireEvent.scroll(tablist);
  };
  return { activity, artifacts, clampThrough, layOut, scrollTo, tablist };
}

test("the strip stops where its last layout puts it, not where a narrower layout before it left it", () => {
  const sizes = installResizeObserver();
  const view = render(
    <Tabs
      activeTab="artifacts"
      ariaLabel="Issue detail"
      idPrefix="issue"
      onSelect={() => {}}
      tabs={issueTabs}
    />
  );

  try {
    const { artifacts, layOut, tablist } = issueTablist(sizes);
    layOut(528);
    expect(tablist.scrollLeft).toBe(9);

    // For a moment the tablist is 300 px: Artifacts runs past the fade, and the strip scrolls it
    // back clear of it.
    layOut(300);
    expect(tablist.scrollLeft).toBe(237);
    expect(artifacts.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);

    // The layout settles at 528 px again. The strip is back where one layout at 528 px puts it,
    // as the baseline shows it, and not at the end of its scroll.
    layOut(528);
    expect(tablist.scrollLeft).toBe(9);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

// The strip moves itself only when the selected tab or its layout changes; a sideways scroll the
// reader makes in between is the reader's, and stays until one of those comes. When one comes,
// the reader's scroll is not kept: the strip goes to where that layout puts the selected tab.
test("a sideways scroll the reader makes stays until the strip's layout changes", () => {
  const sizes = installResizeObserver();
  const view = render(
    <Tabs
      activeTab="artifacts"
      ariaLabel="Issue detail"
      idPrefix="issue"
      onSelect={() => {}}
      tabs={issueTabs}
    />
  );

  try {
    const { layOut, scrollTo, tablist } = issueTablist(sizes);
    layOut(528);
    expect(tablist.scrollLeft).toBe(9);

    scrollTo(221);
    expect(tablist.scrollLeft).toBe(221);

    // The tablist narrows to 400 px; the strip goes to where that layout puts Artifacts:
    // 505 - (400 - 32) = 137.
    layOut(400);
    expect(tablist.scrollLeft).toBe(137);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

// A layout that widens the strip, is read, and narrows again inside one browser task clamps the
// scroll to the wide layout and changes no size the observers see: only the clamp's own scroll
// event is left, and no reader input came before it. The strip must settle on it, back where its
// layout puts the selected tab. A reader's scroll, which comes after their input, stays (the row
// above).
test("a scroll the browser makes clamping the strip to a layout no observer reports settles the strip again", () => {
  const sizes = installResizeObserver();
  const view = render(
    <Tabs
      activeTab="artifacts"
      ariaLabel="Issue detail"
      idPrefix="issue"
      onSelect={() => {}}
      tabs={issueTabs}
    />
  );

  try {
    const { clampThrough, layOut, tablist } = issueTablist(sizes);
    // A 449 px tablist puts Artifacts at 505 - (449 - 32) = 88.
    layOut(449);
    expect(tablist.scrollLeft).toBe(88);

    // Laid out 749 px wide for a moment, every tab fits and the scroll clamps to 0.
    clampThrough(749);
    expect(tablist.scrollLeft).toBe(88);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

/** Waits out one animation frame, so a settle the effect defers to a frame has run before a row
 *  reads the strip. Without it a row would read the strip before such a settle and fail on a
 *  correct one; with it, a row also fails on a deferred reveal that moves by a delta. */
async function nextFrame(): Promise<void> {
  const painted = Promise.withResolvers<void>();
  requestAnimationFrame(() => painted.resolve());
  await painted.promise;
}

// A change of the selected tab settles the strip on the new tab from its layout alone, wherever
// the reader had scrolled it, even when the new tab was already in view.
test("a newly selected tab settles the strip where its layout puts that tab, not where the reader left it", async () => {
  const sizes = installResizeObserver();
  const renderTabs = (activeTab: (typeof issueTabs)[number]["id"]) => (
    <Tabs
      activeTab={activeTab}
      ariaLabel="Issue detail"
      idPrefix="issue"
      onSelect={() => {}}
      tabs={issueTabs}
    />
  );
  const view = render(renderTabs("artifacts"));

  try {
    const { layOut, scrollTo, tablist } = issueTablist(sizes);
    layOut(528);
    expect(tablist.scrollLeft).toBe(9);
    scrollTo(221);

    // Activity, whole at 288 to 359 of the 528 px tablist, is selected: the strip goes to where
    // the layout puts it, 580 - (528 - 32) = 84.
    view.rerender(renderTabs("activity"));
    await nextFrame();
    expect(tablist.scrollLeft).toBe(84);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

// A keystroke that selects a tab, with a parent that takes the selection at once as both pages
// do: the strip settles on the tab the keystroke reached, from the layout alone.
test("a tab a keystroke selects settles the strip where its layout puts that tab", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const view = render(
    <SelectingTabs ariaLabel="Issue detail" idPrefix="issue" initial="activity" tabs={issueTabs} />
  );

  try {
    const { activity, artifacts, layOut, scrollTo, tablist } = issueTablist(sizes);
    layOut(300);
    // The reader scrolls the strip to its end, 749 - 300 = 449.
    scrollTo(449);
    activity.focus();
    await user.keyboard("{ArrowLeft}");
    await nextFrame();

    // Artifacts is selected and focused; the strip is where the layout puts it,
    // 505 - (300 - 32) = 237.
    expect(artifacts).toHaveFocus();
    expect(artifacts).toHaveAttribute("aria-selected", "true");
    expect(tablist.scrollLeft).toBe(237);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

// A tab wider than what the fade leaves of the tablist cannot end clear of the fade: the strip
// shows the tab from its own left edge instead of cutting its start off.
test("a selected tab wider than what the fade leaves settles at its own left edge", () => {
  const sizes = installResizeObserver();
  const view = render(
    <Tabs
      activeTab="artifacts"
      ariaLabel="Issue detail"
      idPrefix="issue"
      onSelect={() => {}}
      tabs={issueTabs}
    />
  );

  try {
    const { artifacts, layOut, tablist } = issueTablist(sizes);
    // 100 px leaves 68 clear of the fade, and Artifacts is 95 px wide.
    layOut(100);
    expect(tablist.scrollLeft).toBe(410);
    expect(artifacts.getBoundingClientRect().left).toBe(0);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

// A live artifact count can widen a tab without changing the tablist's own box. The selected
// Activity tab after it must stay clear of the fade even though only Artifacts resized.
test("a live count widening an earlier tab keeps the selected tab clear of the fade", () => {
  const sizes = installResizeObserver();
  const renderTabs = (count: number) => (
    <Tabs
      activeTab="activity"
      ariaLabel="Issue detail"
      idPrefix="issue"
      onSelect={() => {}}
      tabs={[
        { count, id: "artifacts", label: "Artifacts" },
        { id: "activity", label: "Activity" },
        { id: "collaborators", label: "Collaborators" },
      ]}
    />
  );
  const view = render(renderTabs(9));

  try {
    const tablist = screen.getByRole("tablist", { name: "Issue detail" });
    const artifacts = screen.getByRole("tab", { name: "Artifacts 9" });
    const activity = screen.getByRole("tab", { name: "Activity" });
    const box = { client: 300, scroll: 420 };
    let activityLeft = 180;
    layOutScrollBox(tablist, box);
    tablist.getBoundingClientRect = () => rect(0, 300);
    activity.getBoundingClientRect = () =>
      rect(activityLeft - tablist.scrollLeft, activityLeft + 88 - tablist.scrollLeft);
    sizes.resize();
    expect(activity.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);

    activityLeft += 8;
    box.scroll += 8;
    view.rerender(renderTabs(10));
    // The list and Activity keep their dimensions; only the counted tab's width changed.
    sizes.resize(artifacts);
    expect(activity.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);
    expect(tablist.scrollLeft).toBe(8);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

// The project page puts its Architecture tab before Issues once the project's source answers. In
// an overflowing tablist that moves the selected tab right with no box changing size - the list's
// stays the width it is given, and no tab that was there resized - so the move itself must
// bring the selection back clear of the fade.
test("a tab added before the selected tab keeps the selected tab clear of the fade", async () => {
  const sizes = installResizeObserver();
  const renderTabs = (withArchitecture: boolean) => (
    <Tabs
      activeTab="documents"
      ariaLabel="Project"
      idPrefix="project"
      onSelect={() => {}}
      tabs={[
        ...(withArchitecture ? [{ id: "architecture", label: "Architecture" } as const] : []),
        { id: "issues", label: "Issues" },
        { id: "documents", label: "Documents" },
        { id: "settings", label: "Settings" },
      ]}
    />
  );
  const view = render(renderTabs(false));

  try {
    const tablist = screen.getByRole("tablist", { name: "Project" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    const box = { client: 300, scroll: 420 };
    let documentsLeft = 180;
    layOutScrollBox(tablist, box);
    tablist.getBoundingClientRect = () => rect(0, 300);
    documents.getBoundingClientRect = () =>
      rect(documentsLeft - tablist.scrollLeft, documentsLeft + 88 - tablist.scrollLeft);
    sizes.resize();
    expect(documents.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);

    // Architecture arrives 100 px wide before Issues; no box the tablist watched changed size.
    documentsLeft += 100;
    box.scroll += 100;
    view.rerender(renderTabs(true));
    await settle();
    expect(documents.getBoundingClientRect().right).toBe(300 - scrollFadeTrailingPx);
    expect(tablist.scrollLeft).toBe(100);
  } finally {
    view.unmount();
    sizes.restore();
  }
});
