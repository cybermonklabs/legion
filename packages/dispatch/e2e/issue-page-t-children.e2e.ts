/** the rows read the children's section above the outline of an issue whose children a person
 *  and an agent wrote, claimed and blocked through the server, and the tree read it answers */

import type { Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { CONTROL_LABELS, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import {
  claimIssue,
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  patchIssue,
} from "./api";
import { asPlanner, PLANNER, PROJECT, seedSessions } from "./issue-layout-seed-support";
import { planDocument, specDocument, verificationDocument } from "./issue-layout-seed-text";
import { gotoIssue, issueRead, phase } from "./issue-page";
import { DESKTOP, expectOpeningLanded, PHONE } from "./issue-page-g";
import { childEntry, childRow, childRows, childrenSection, childrenToggle } from "./issue-page-i";
import { firstRowOutOfPlace, openOutline, PAGE_VIEWS, ROW_MS } from "./issue-page-t-rows";
import { owedUnless, treeReadGate } from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Row 9 of the rows that draw the new issue page from the tree read: the children are a section
 * above the outline in Conversation, in each filter and in Graph, open until the reader closes
 * it, 20 at a time (spec R14, R15). The head of `e2e/issue-page-t-rows.ts` says what the rows read
 * and what their gates are.
 */

/** How many children the seeded issue holds: three past the section's first page of 20. */
const CHILDREN = 23;

/** How many children the section lists before its control lists the rest (spec R14). */
const CHILDREN_PAGE = 20;

/** What `seedChildren` wrote. */
interface ChildrenSeed {
  /** A child another child waits on, which the planner holds in progress. */
  readonly running: string;
  /** The child that waits on `running`. */
  readonly blocked: string;
  readonly children: readonly string[];
  readonly key: string;
  /** A child with no child of its own. */
  readonly leaf: string;
  /** A child with children of its own. */
  readonly withChildren: string;
}

/** An issue of 23 children, two of which hold two children each, one of those done, and one child
 *  done; the first child is in progress, held by the planner, and the second waits on it. The
 *  issue holds a statement with a check that serves it and a plan task, and an ask of the
 *  planner's, so each filter draws a row under the section. A person writes each issue and
 *  document through the route a person writes through, and the planner's session asks and claims
 *  the first child through the route an agent writes through. */
async function seedChildren(): Promise<ChildrenSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: 1, summary: false }),
    title: "An issue of twenty-three children",
  });
  await createIssueArtifact(issue.key, {
    content: verificationDocument({ checks: 1, issueKey: issue.key, requirements: 1 }),
    name: "verification.md",
  });
  await createIssueArtifact(issue.key, {
    content: planDocument({ issueKey: issue.key, planTasks: 2, requirements: 1 }),
    name: "plan.md",
  });
  await seedSessions(issue.key, [PLANNER]);
  await createAsk(issue.key, { question: "Which child goes first?" }, asPlanner);
  await patchIssue(issue.key, { status: "in_progress" });
  const children: string[] = [];
  for (let index = 1; index <= CHILDREN; index += 1) {
    const child = await createIssue({
      force: true,
      parent: issue.key,
      project: PROJECT,
      title: `Child ${index} of the twenty-three`,
    });
    children.push(child.key);
  }
  const [running = "", blocked = "", done = "", , withChildren = "", leaf = ""] = children;
  await patchIssue(running, { status: "in_progress" });
  await claimIssue(running, asPlanner);
  await patchIssue(blocked, { blocked_by: [running] });
  await patchIssue(done, { status: "done" });
  const grandchildren: string[] = [];
  for (const parent of [running, withChildren]) {
    for (let index = 1; index <= 2; index += 1) {
      const grandchild = await createIssue({
        force: true,
        parent,
        project: PROJECT,
        title: `Grandchild ${index} under ${parent}`,
      });
      grandchildren.push(grandchild.key);
    }
  }
  await patchIssue(grandchildren[0] ?? "", { status: "done" });
  return { blocked, children, key: issue.key, leaf, running, withChildren };
}

/** How far below the section's foot the outline's first row stands: at least 0 where the
 *  section stands above the outline. */
function outlineBelowSection(page: Page): Promise<number> {
  return page.evaluate(() => {
    const foot = document.querySelector("[data-children-section]")?.getBoundingClientRect().bottom;
    const first = document
      .querySelector("[data-issue-page] [data-node-row]")
      ?.getBoundingClientRect().top;
    return (first ?? Number.NaN) - (foot ?? Number.NaN);
  });
}

// c-children-section.
// SLOW(>=2s): an issue of twenty-three children seeded, each of six views loaded at two widths
// with each load waited out, the section closed, the page loaded again and another issue opened,
// and the children's address and chord each pressed.
test("the children are a section above the outline, 20 at a time @phone", async ({ browser }) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedChildren, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize(size);
      for (const view of PAGE_VIEWS) {
        const where = `in ${PRESET_NAMES[view]} at ${size.width} px`;
        const traffic = watchApiRequests(page);
        await gotoIssue(page, presetPath(seeded.key, view));
        await expectLoadSettled(traffic);
        await expect(childrenSection(page), `${where}, no children's section`).toHaveCount(1);
        await expect(childrenToggle(page), `${where}, the section opens expanded`).toHaveAttribute(
          "aria-expanded",
          "true"
        );
        await expect(
          childrenSection(page).locator("[data-child-entry]"),
          `${where}, the section lists 20 children`
        ).toHaveCount(CHILDREN_PAGE);
        await expect(
          childrenSection(page).locator("[data-children-more]"),
          `${where}, the section's control does not say 3 are left`
        ).toHaveText(`${CHILDREN - CHILDREN_PAGE} ${CONTROL_LABELS.children_page_children}`);
        expect(
          await outlineBelowSection(page),
          `${where}, the section stands above the outline`
        ).toBeGreaterThanOrEqual(0);
      }
      // The control shows the 3 left, and each child listed draws its status, what blocks it and
      // who holds it.
      const where = `at ${size.width} px`;
      await gotoIssue(page, presetPath(seeded.key, "conversation"));
      await childrenSection(page).locator("[data-children-more]").click();
      await expect(
        childrenSection(page).locator("[data-child-entry]"),
        `${where}, the section's control does not show the 3 left`
      ).toHaveCount(CHILDREN);
      for (const key of seeded.children) {
        await expect(
          childEntry(page, key).locator("[data-child-facts][data-child-status]"),
          `${where}, the section draws no status of ${key}`
        ).toHaveCount(1);
      }
      await expect(
        childEntry(page, seeded.blocked).locator('[data-testid="issue-child-blocked-by"]'),
        `${where}, the section does not say what blocks ${seeded.blocked}`
      ).toContainText(seeded.running);
      await expect(
        childEntry(page, seeded.running).locator("[data-child-holder]"),
        `${where}, the section does not say who holds ${seeded.running}`
      ).not.toBeEmpty();
    }

    // The outline draws each child once, where the tree places it.
    await page.setViewportSize(DESKTOP);
    const opened = await openOutline(page, seeded.key, "conversation");
    for (const key of seeded.children) {
      await expect(childRow(page, key), `the outline draws ${key} other than once`).toHaveCount(1);
    }
    await expect(childRows(page), "the outline draws a child other than its own").toHaveCount(
      seeded.children.length
    );
    expect(
      firstRowOutOfPlace("in Conversation", opened.rows, opened),
      "each child stands where the tree places it"
    ).toBeUndefined();

    // Collapsed by the reader: its header keeps the bar and the two counts, each labelled, and
    // the two differ; the reader's choice holds after a reload and on another issue.
    await childrenToggle(page).click();
    await expect(childrenToggle(page), "the section does not collapse").toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await expect(
      childrenSection(page).locator("[data-child-entry]").first(),
      "the collapsed section still lists its children"
    ).toBeHidden();
    const direct = childrenSection(page).getByTestId("issue-progress-children");
    const subtree = childrenSection(page).getByTestId("issue-progress-subtree");
    await expect(direct, "the collapsed header draws no count of the children").toContainText(
      CONTROL_LABELS.children
    );
    await expect(subtree, "the collapsed header draws no count of the whole tree").toContainText(
      CONTROL_LABELS.whole_tree
    );
    expect(
      await direct.getAttribute("aria-label"),
      "the collapsed header's two counts are the same"
    ).not.toBe(await subtree.getAttribute("aria-label"));
    await page.reload();
    await issueRead(page);
    await expect(
      childrenToggle(page),
      "the section is not still collapsed after a reload"
    ).toHaveAttribute("aria-expanded", "false");
    await gotoIssue(page, presetPath(seeded.withChildren, "conversation"));
    await expect(
      childrenToggle(page),
      "the section is not still collapsed on another issue with children"
    ).toHaveAttribute("aria-expanded", "false");

    // `t h` and `/children` move the focus to the section.
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectOpeningLanded(page);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await expect(childrenToggle(page), "t h does not focus the section").toBeFocused();
    await gotoIssue(page, `/issues/${seeded.key}/children`);
    await expect(childrenToggle(page), "/children does not focus the section").toBeFocused();

    // An issue with no children draws no section.
    await gotoIssue(page, presetPath(seeded.leaf, "conversation"));
    await expect(childrenSection(page), "an issue with no children draws a section").toHaveCount(0);
  });
});
