import type { StatusChangeEntry, StatusChangeInput } from "../web/src/api/types";
import type { ParentStatusOption } from "../web/src/features/issue/parent-status/useParentStatusMove";
import { statusText } from "../web/src/features/project/board-model";
import {
  changeIssueStatus,
  createIssue,
  createProject,
  getIssue,
  listIssues,
  patchIssue,
} from "./api";
import { mouseDrag } from "./board-drag";
import { pickFromList } from "./filters";
import { type KeyedWrite, watchWrites } from "./keyed-writes";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/** What each choice's button says (`CHOICE_LABELS`, ParentStatusDialog.tsx), as a person reads
 *  it and clicks it. */
const choiceLabel: Readonly<Record<ParentStatusOption, string>> = {
  include_children: "Move them with it",
  include_parent: "Move the parents into work too",
  unlink_children: "Unlink them",
};

/** An issue as Dispatch holds it: its status and its parent. */
interface Standing {
  readonly parent: string | null;
  readonly status: string;
}

/** A parent in work with open issues two levels under it: a child in `todo` over a grandchild in
 *  `in_progress`, and a child in `backlog`. Answers the four issues and how each was seeded. */
async function seedTree(name: string) {
  const parent = await createIssue({ project: "CORE", title: `${name} parent` });
  const working = await createIssue({
    parent: parent.key,
    project: "CORE",
    title: `${name} child in todo`,
  });
  const grandchild = await createIssue({
    parent: working.key,
    project: "CORE",
    title: `${name} grandchild`,
  });
  const waiting = await createIssue({
    parent: parent.key,
    project: "CORE",
    title: `${name} child in backlog`,
  });
  await Promise.all([
    patchIssue(parent.key, { status: "in_progress" }),
    patchIssue(working.key, { status: "todo" }),
    patchIssue(grandchild.key, { status: "in_progress" }),
    patchIssue(waiting.key, { status: "backlog" }),
  ]);
  const seeded: Record<string, Standing> = {
    [parent.key]: { parent: null, status: "in_progress" },
    [working.key]: { parent: parent.key, status: "todo" },
    [grandchild.key]: { parent: working.key, status: "in_progress" },
    [waiting.key]: { parent: parent.key, status: "backlog" },
  };
  return { grandchild, parent, seeded, waiting, working };
}

/** Each issue of CORE, or each of `keys`, as Dispatch holds it now. */
async function standing(keys?: readonly string[]): Promise<Record<string, Standing>> {
  const issues = await listIssues("CORE");
  return Object.fromEntries(
    issues
      .filter((issue) => keys === undefined || keys.includes(issue.key))
      .map((issue) => [issue.key, { parent: issue.parent, status: issue.status }])
  );
}

/** Whether a body the page sent to the status-change route is one. */
function isStatusChange(body: unknown): body is StatusChangeInput {
  return typeof body === "object" && body !== null && "status" in body;
}

/** The status changes of `posts` that are no dry run: what the page wrote, not what it checked. */
function madeChanges(posts: readonly KeyedWrite<StatusChangeInput>[]): StatusChangeInput[] {
  return posts.map(({ input }) => input).filter((input) => input.dry_run !== true);
}

/** The question a status write of `key` to `status` asks while Dispatch's rule would refuse it
 *  (`ParentStatusDialog`). */
function question(page: Page, key: string, status: string): Locator {
  return page.getByRole("dialog", { name: `Move ${key} to ${statusText(status)}?` });
}

/** One issue as a choice's list says the change leaves it (`entryText`, ParentStatusDialog.tsx). */
function entryLine(entry: Pick<StatusChangeEntry, "key" | "parent" | "status">): string {
  const parent = entry.parent === null ? "no parent" : `under ${entry.parent}`;
  return `${entry.key}: ${statusText(entry.status)}, ${parent}`;
}

/** `lines` in one order, so two lists are compared for what they hold. */
function sorted(lines: readonly string[]): string[] {
  return [...lines].sort();
}

/**
 * Holds `dialog`, the question of `key`'s move to `status`, to what Dispatch says of that move
 * now: it names `holders` as the issues that stop the move, each with its relation to `key` and
 * its status; it offers `options` and no other choice; and under each choice it lists what the
 * test's own call of the API's dry run of that option answers. Answers those dry runs' entries.
 */
async function expectQuestion(
  dialog: Locator,
  key: string,
  status: string,
  holders: readonly { readonly key: string; readonly relation: "child" | "parent" }[],
  options: readonly ParentStatusOption[]
): Promise<Partial<Record<ParentStatusOption, readonly StatusChangeEntry[]>>> {
  const named = await Promise.all(
    holders.map(async ({ key: holder, relation }) => {
      const issue = await getIssue(holder);
      return `${issue.key} ${issue.title} (${relation}, ${statusText(issue.status)})`;
    })
  );
  await expect(
    dialog.getByRole("list", { name: "Issues that stop this change" }).getByRole("listitem")
  ).toHaveText(named);
  await expect(dialog.getByRole("list", { name: "Choices" }).getByRole("button")).toHaveText(
    options.map((option) => choiceLabel[option])
  );
  const answers: Partial<Record<ParentStatusOption, readonly StatusChangeEntry[]>> = {};
  for (const option of options) {
    const input: StatusChangeInput = { dry_run: true, status };
    input[option] = true;
    const changed = (await changeIssueStatus(key, input)).changed ?? [];
    await expect(
      dialog
        .getByRole("list", { name: `What ${choiceLabel[option]} changes` })
        .getByRole("listitem")
    ).toHaveText(changed.map(entryLine));
    answers[option] = changed;
  }
  return answers;
}

// R24 (INFRA-4731, k40): a status write that Dispatch's rule would refuse asks first, on the issue
// page and on the board. The e2e server keeps DISPATCH_PARENT_STATUS_RULE off (run-server.sh),
// which is the case D8 makes the page ask in: the dry run answers as the rule is on whatever the
// setting, so every write below would land unasked if the page did not ask. The issues table's
// question, which Move to epic asked, went with the epic tools (dispatch://INFRA-3221).
// SLOW(>=2s): fourteen seeded issues, two issue pages and the board with two whole-card drags, and
// seven questions, each with the page's dry runs and the row's own.
test("parent-status-choice: a parent's move asks which way its open issues go", async ({
  browser,
}) => {
  // One worker of an issue pod ran this row in 33 s, over half the suite's 60 s row budget, which
  // is sized for rows a fraction of its length.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const first = await seedTree("First");
  const second = await seedTree("Second");
  const third = await seedTree("Third");
  const shelved = await createIssue({ project: "CORE", title: "Parent on the backlog" });
  const child = await createIssue({
    parent: shelved.key,
    project: "CORE",
    title: "Child on the backlog",
  });
  await Promise.all([
    patchIssue(shelved.key, { status: "backlog" }),
    patchIssue(child.key, { status: "backlog" }),
  ]);
  const seeded = await standing();
  // The seed is what each question below is about, read back from Dispatch.
  expect(seeded).toEqual({
    ...first.seeded,
    ...second.seeded,
    ...third.seeded,
    [shelved.key]: { parent: null, status: "backlog" },
    [child.key]: { parent: shelved.key, status: "backlog" },
  });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1920, height: 900 });

    // P1's issue page: Close asks which way P1's open children go, and lists what each choice
    // changes as the API's dry run of that choice answers it.
    const p1 = first.parent.key;
    const p1Posts = watchWrites(page, "POST", `/api/v1/issues/${p1}/status-change`, isStatusChange);
    const p1Patches = watchWrites(page, "PATCH", `/api/v1/issues/${p1}`);
    await page.goto(`/issues/${p1}`);
    await expect(page.getByRole("heading", { level: 1, name: first.parent.title })).toBeVisible();
    const closeButton = page.getByRole("button", { name: "Close issue", exact: true });
    // The issue page's Status is the shared single pick (INFRA-3135's R3), whose trigger carries
    // the status it shows in `data-issue-status`; its options are named as `statusText` names them.
    const statusPick = page.locator("[data-issue-status]");
    const openChildren = [
      { key: first.working.key, relation: "child" },
      { key: first.waiting.key, relation: "child" },
    ] as const;
    await closeButton.click();
    const closing = question(page, p1, "done");
    const choices = await expectQuestion(closing, p1, "done", openChildren, [
      "include_children",
      "unlink_children",
    ]);
    // What the lists are held to: every open issue under P1, both levels, closed with it; or P1's
    // two open children unlinked, each keeping its status.
    expect(sorted((choices.include_children ?? []).map(entryLine))).toEqual(
      sorted([
        entryLine({ key: p1, parent: null, status: "done" }),
        entryLine({ key: first.working.key, parent: p1, status: "done" }),
        entryLine({ key: first.grandchild.key, parent: first.working.key, status: "done" }),
        entryLine({ key: first.waiting.key, parent: p1, status: "done" }),
      ])
    );
    expect(sorted((choices.unlink_children ?? []).map(entryLine))).toEqual(
      sorted([
        entryLine({ key: p1, parent: null, status: "done" }),
        entryLine({ key: first.working.key, parent: null, status: "todo" }),
        entryLine({ key: first.waiting.key, parent: null, status: "backlog" }),
      ])
    );

    // Cancel writes nothing.
    await closing.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(closing).toBeHidden();
    expect(madeChanges(p1Posts)).toEqual([]);
    expect(p1Patches).toEqual([]);
    expect(await standing()).toEqual(seeded);

    // Nor does Keep as it is.
    await closeButton.click();
    await expect(closing.getByRole("list", { name: "Choices" }).getByRole("button")).toHaveText([
      choiceLabel.include_children,
      choiceLabel.unlink_children,
    ]);
    await closing.getByRole("button", { name: "Keep as it is", exact: true }).click();
    await expect(closing).toBeHidden();
    expect(madeChanges(p1Posts)).toEqual([]);
    expect(p1Patches).toEqual([]);
    expect(await standing()).toEqual(seeded);

    // To the backlog, the child in work stops P1; Move them with it takes every open issue under
    // P1 to the backlog.
    await pickFromList(statusPick, statusText("backlog"));
    const shelving = question(page, p1, "backlog");
    await expectQuestion(
      shelving,
      p1,
      "backlog",
      [{ key: first.working.key, relation: "child" }],
      ["include_children", "unlink_children"]
    );
    await shelving.getByRole("button", { name: choiceLabel.include_children }).click();
    await expect(shelving).toBeHidden();
    await expect
      .poll(() => standing(Object.keys(first.seeded)))
      .toEqual({
        [p1]: { parent: null, status: "backlog" },
        [first.working.key]: { parent: p1, status: "backlog" },
        [first.grandchild.key]: { parent: first.working.key, status: "backlog" },
        [first.waiting.key]: { parent: p1, status: "backlog" },
      });

    // Then to done, Unlink them leaves P1's open children roots and P1 done.
    await expect(statusPick).toHaveAttribute("data-issue-status", "backlog");
    await closeButton.click();
    await expectQuestion(closing, p1, "done", openChildren, [
      "include_children",
      "unlink_children",
    ]);
    await closing.getByRole("button", { name: choiceLabel.unlink_children }).click();
    await expect(closing).toBeHidden();
    await expect
      .poll(() => standing(Object.keys(first.seeded)))
      .toEqual({
        [p1]: { parent: null, status: "done" },
        [first.working.key]: { parent: null, status: "backlog" },
        [first.grandchild.key]: { parent: first.working.key, status: "backlog" },
        [first.waiting.key]: { parent: null, status: "backlog" },
      });

    // The project board: P2's card dragged to Done asks the same, and Move them with it closes
    // every open issue under P2.
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board", exact: true }).click();
    const inProgress = page.getByRole("region", { name: "In progress", exact: true });
    const p2 = second.parent.key;
    const p2Card = inProgress.getByRole("article", { name: `${p2} ${second.parent.title}` });
    await mouseDrag(page, p2Card, page.getByRole("region", { name: "Done (collapsed)" }));
    const p2Closing = question(page, p2, "done");
    await expectQuestion(
      p2Closing,
      p2,
      "done",
      [
        { key: second.working.key, relation: "child" },
        { key: second.waiting.key, relation: "child" },
      ],
      ["include_children", "unlink_children"]
    );
    await p2Closing.getByRole("button", { name: choiceLabel.include_children }).click();
    await expect(p2Closing).toBeHidden();
    await expect
      .poll(() => standing(Object.keys(second.seeded)))
      .toEqual({
        [p2]: { parent: null, status: "done" },
        [second.working.key]: { parent: p2, status: "done" },
        [second.grandchild.key]: { parent: second.working.key, status: "done" },
        [second.waiting.key]: { parent: p2, status: "done" },
      });
    await expect(p2Card).toHaveCount(0);

    // P3's card dragged to Backlog, and Unlink them: P3's open children are roots.
    const p3 = third.parent.key;
    await mouseDrag(
      page,
      inProgress.getByRole("article", { name: `${p3} ${third.parent.title}` }),
      page.getByRole("region", { name: "Backlog", exact: true })
    );
    const p3Shelving = question(page, p3, "backlog");
    await expectQuestion(
      p3Shelving,
      p3,
      "backlog",
      [{ key: third.working.key, relation: "child" }],
      ["include_children", "unlink_children"]
    );
    await p3Shelving.getByRole("button", { name: choiceLabel.unlink_children }).click();
    await expect(p3Shelving).toBeHidden();
    await expect
      .poll(() => standing(Object.keys(third.seeded)))
      .toEqual({
        [p3]: { parent: null, status: "backlog" },
        [third.working.key]: { parent: null, status: "todo" },
        [third.grandchild.key]: { parent: third.working.key, status: "in_progress" },
        [third.waiting.key]: { parent: null, status: "backlog" },
      });

    // The issue page of `child`: `child` into work under `shelved`, on the backlog, asks whether
    // `shelved` moves into work too, and lists `shelved`.
    await page.goto(`/issues/${child.key}`);
    await expect(page.getByRole("heading", { level: 1, name: child.title })).toBeVisible();
    await pickFromList(statusPick, statusText("in_progress"));
    const starting = question(page, child.key, "in_progress");
    const upward = await expectQuestion(
      starting,
      child.key,
      "in_progress",
      [{ key: shelved.key, relation: "parent" }],
      ["include_parent"]
    );
    expect(sorted((upward.include_parent ?? []).map(entryLine))).toEqual(
      sorted([
        entryLine({ key: shelved.key, parent: null, status: "in_progress" }),
        entryLine({ key: child.key, parent: shelved.key, status: "in_progress" }),
      ])
    );
    await starting.getByRole("button", { name: choiceLabel.include_parent }).click();
    await expect(starting).toBeHidden();
    await expect
      .poll(() => standing([shelved.key, child.key]))
      .toEqual({
        [shelved.key]: { parent: null, status: "in_progress" },
        [child.key]: { parent: shelved.key, status: "in_progress" },
      });
  } finally {
    await context.close();
  }
});
