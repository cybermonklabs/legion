import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { type TabDefinition, Tabs } from "./Tabs";

const projectTabs = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
] as const;

/** The issue's ten tabs, one of them counted, which no phone width holds on one line. */
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

/** A tablist whose parent takes each selection at once, as both pages that use `Tabs` do: they
 *  navigate on select (`IssueTabs`, `ProjectPage`). */
function SelectingTabs<Id extends string>({
  ariaLabel = "Project",
  idPrefix = "project",
  initial,
  tabs,
}: {
  ariaLabel?: string;
  idPrefix?: string;
  initial: Id;
  tabs: readonly TabDefinition<Id>[];
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
  const view = render(<SelectingTabs initial="issues" tabs={projectTabs} />);

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
    expect(documents).toHaveAttribute("aria-selected", "true");
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

// The issue's ten tabs outrun a phone and the issue page beside its comment margin. The tablist
// wraps onto a further line there rather than scrolling (dispatch://INFRA-5200, the layout rule's
// 4: nothing scrolls sideways), so it is no scroll box, and every tab stays a tab of it, reached
// from the keyboard as before. `e2e/artifacts.e2e.ts` reads the lines it wraps onto in a browser.
test("the issue's tablist wraps its tabs rather than scrolling them, and the keyboard still reaches each", async () => {
  const user = userEvent.setup();
  const view = render(
    <SelectingTabs ariaLabel="Issue detail" idPrefix="issue" initial="spec" tabs={issueTabs} />
  );

  try {
    const tablist = screen.getByRole("tablist", { name: "Issue detail" });
    expect(tablist).toHaveClass("flex-wrap");
    expect(tablist.className).not.toMatch(/overflow-x-(auto|scroll)/);
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Spec",
      "Verification",
      "Plan",
      "Steps",
      "Conversation",
      "Children",
      "Artifacts 5",
      "Activity",
      "Decisions",
      "Collaborators",
    ]);

    screen.getByRole("tab", { name: "Spec" }).focus();
    await user.keyboard("{End}");
    const collaborators = screen.getByRole("tab", { name: "Collaborators" });
    expect(collaborators).toHaveFocus();
    expect(collaborators).toHaveAttribute("aria-selected", "true");
  } finally {
    view.unmount();
  }
});
