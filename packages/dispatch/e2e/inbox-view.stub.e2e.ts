/** The Inbox's **View** menu and its **Filter** bar over one fixed list: what a row wears, what
 *  each of the five fields filters, bands and orders by, how a band folds, and how the two
 *  columns are sized.
 *
 *  Every row here reads what the page draws over one answer - boxes, counts, bands, an order
 *  the SPA computes and a width the reader's browser keeps - so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The row
 *  that follows a hundred and twenty threads through a live priority change stayed in
 *  `inbox-view.e2e.ts`.
 */
import { expect } from "@playwright/test";

import { inboxList, inboxPane } from "./inbox-pane";
import {
  bandHeadings,
  bandLabel,
  chooseView,
  countsOf,
  drawnIds,
  INBOX,
  lastActivity,
  openField,
  seedEveryField,
  sortedIds,
  valueLabel,
  valuesOf,
} from "./inbox-view";
import { stubbed } from "./stub/stub-test";

/** The eleven rows, and when each was last written in - what `newest` orders by, which the two
 *  reads answer. The map travels as its entries, a scene being JSON. */
const test = stubbed(async () => {
  const seeded = await seedEveryField();
  return { ...seeded, at: [...(await lastActivity())] };
});

test("a row wears its marks without overlap, shows its issue key whole, and the bar holds three buttons @pr", async ({
  page,
  scene,
}) => {
  const { rows } = scene;
  await page.goto(INBOX);
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);

  // Every row starts with its kind's icon, at one left edge, before its title and its marks.
  const geometry = await page.locator("[data-inbox-row]").evaluateAll((nodes) =>
    nodes.map((node) => {
      const box = (element: Element | null) => element?.getBoundingClientRect() ?? null;
      const icon = node.querySelector("[data-inbox-kind]");
      const title = node.querySelector("a[data-inbox-owner], span.grow");
      const key = [...node.querySelectorAll("a > span")].find((span) =>
        /^[A-Z][A-Z0-9]*-\d+$/.test((span.textContent ?? "").trim())
      );
      return {
        icon: box(icon),
        id: node.getAttribute("data-inbox-row") ?? "",
        key:
          key === undefined
            ? null
            : {
                box: key.getBoundingClientRect(),
                clientWidth: key.clientWidth,
                scrollWidth: key.scrollWidth,
                text: (key.textContent ?? "").trim(),
              },
        kind: icon?.getAttribute("data-inbox-kind") ?? null,
        priority: box(node.querySelector("[data-inbox-priority]")),
        title: box(title),
        urgency: box(node.querySelector("[data-inbox-urgency]")),
      };
    })
  );
  const byId = new Map(rows.map((row) => [row.id, row]));
  const lefts = new Set<number>();
  for (const row of geometry) {
    const seeded = byId.get(row.id);
    expect(seeded, row.id).toBeDefined();
    expect(row.kind, row.id).toBe(seeded?.type);
    expect(row.icon, row.id).not.toBeNull();
    lefts.add(Math.round(row.icon?.left ?? -1));
    expect((row.icon?.left ?? 0) < (row.title?.left ?? 0), row.id).toBe(true);
    // The priority tag and, for an ask, the urgency mark.
    expect(row.priority === null, `${row.id} priority`).toBe(seeded?.priority === "none");
    expect(row.urgency === null, `${row.id} urgency`).toBe(seeded?.urgency === "none");
    // No two marks overlap: each box is clear of the others.
    const boxes = [
      ["priority", row.priority],
      ["urgency", row.urgency],
      ["title", row.title],
      ["key", row.key?.box ?? null],
    ] as const;
    for (const [leftName, left] of boxes) {
      for (const [rightName, right] of boxes) {
        if (leftName >= rightName || left === null || right === null) continue;
        const overlaps =
          left.left < right.right &&
          right.left < left.right &&
          left.top < right.bottom &&
          right.top < left.bottom;
        expect(overlaps, `${row.id}: ${leftName} over ${rightName}`).toBe(false);
      }
    }
    // The issue key shows whole: its own text, in a box its text fits.
    if (seeded?.issueKey === undefined) {
      expect(row.key, row.id).toBeNull();
    } else {
      expect(row.key?.text, row.id).toBe(seeded.issueKey);
      expect(row.key?.scrollWidth ?? 0, row.id).toBeLessThanOrEqual(
        (row.key?.clientWidth ?? 0) + 1
      );
    }
  }
  expect(lefts.size).toBe(1);

  // The bar carries three buttons and no other control of this issue's.
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]").first()).toBeVisible();
  await expect(page.locator("[data-inbox-bar] button")).toHaveText([
    "Filter",
    "Mark all read",
    "View",
  ]);
});

test("each of the five fields filters, bands and orders the list @pr", async ({ page, scene }) => {
  const { rows } = scene;
  const at = new Map(scene.at);
  await page.goto(INBOX);
  const listed = page.locator("[data-inbox-row]");
  await expect(listed).toHaveCount(rows.length);

  for (const field of ["priority", "urgency", "type", "project", "assignee"] as const) {
    const values = valuesOf(rows, field);
    const counts = countsOf(rows, field);

    // The Filter control counts each value and narrows the list to the one chosen.
    const options = await openField(page, field);
    const offeredOptions = options.getByRole("option");
    // `count()` waits for nothing, so the popover's rows are waited for before they are
    // counted: every value of this field, plus `none` where the field has one.
    await expect(offeredOptions).toHaveCount(values.length);
    const offered = values.length;
    for (const value of values) {
      const name = valueLabel(field, value);
      const option = options.getByRole("option", { name: new RegExp(`^${name},`) });
      await expect(option, `${field}:${value}`).toHaveAttribute(
        "aria-label",
        new RegExp(`\\b${counts.get(value)} rows$`)
      );
    }
    const chosen = values[0] ?? "";
    const chosenName = valueLabel(field, chosen);
    await options.getByRole("option", { name: new RegExp(`^${chosenName},`) }).click();
    await expect(listed).toHaveCount(counts.get(chosen) ?? 0);
    expect(new Set(await drawnIds(page))).toEqual(
      new Set(rows.filter((row) => String(row[field]) === chosen).map((row) => row.id))
    );

    // Its search box narrows the values the popover lists.
    const searchLabel = field === "assignee" ? "Search people" : `Search ${field}`;
    const search = page.getByRole("combobox", { name: searchLabel });
    await search.fill(chosenName.slice(0, 3));
    // The search narrows the rows in a render of its own, which a count would not wait for.
    await expect(offeredOptions).not.toHaveCount(offered);
    const matching = await offeredOptions.count();
    expect(matching, `${field} search`).toBeGreaterThan(0);
    expect(matching, `${field} search`).toBeLessThan(offered);
    await search.fill("");

    // Off again, so the next field counts the whole list.
    await options.getByRole("option", { name: new RegExp(`^${chosenName},`) }).click();
    await expect(listed).toHaveCount(rows.length);
    await page.keyboard.press("Escape");

    // Banding by the field: one band per value, with its own count, in the field's order.
    await chooseView(page, "Group by", field);
    expect(await bandHeadings(page), `group by ${field}`).toEqual(
      values.map((value) => `${bandLabel(field, value)} (${counts.get(value)})`)
    );

    // Ordering by the field, over one list: the field's order, ties newest first.
    await chooseView(page, "Group by", "none");
    await chooseView(page, "Sort by", field);
    expect(await drawnIds(page), `sort by ${field}`).toEqual(sortedIds(rows, field, at));
    await chooseView(page, "Sort by", "newest");
    expect(await drawnIds(page), "sort by newest").toEqual(sortedIds(rows, "newest", at));
    await chooseView(page, "Group by", "priority");
  }

  // One list under Group by None, and today's turn bands under Waiting on.
  await chooseView(page, "Group by", "none");
  expect(await bandHeadings(page)).toEqual([]);
  await expect(listed).toHaveCount(rows.length);
  await chooseView(page, "Group by", "waiting-on");
  expect(await bandHeadings(page)).toEqual([
    `Waiting on a person (${rows.length - 1})`,
    "Waiting on agents (1)",
  ]);
});

test("a band folds and stays folded, and a thread the reader has read stays in the list @pr", async ({
  page,
  scene,
}) => {
  const { read, rows } = scene;
  await page.goto(INBOX);
  const listed = page.locator("[data-inbox-row]");
  await expect(listed).toHaveCount(rows.length);

  // Every band folds and unfolds.
  for (const value of valuesOf(rows, "priority")) {
    const label = `${valueLabel("priority", value)} (${countsOf(rows, "priority").get(value)})`;
    const heading = page.getByRole("button", { name: label });
    const held = countsOf(rows, "priority").get(value) ?? 0;
    await heading.click();
    await expect(listed).toHaveCount(rows.length - held);
    await heading.click();
    await expect(listed).toHaveCount(rows.length);
  }

  // A folded band is still folded after a reload: the fold is the reader's own, kept for
  // their browser rather than in the address.
  const p0 = page.getByRole("button", { name: "P0 (1)" });
  await p0.click();
  await expect(listed).toHaveCount(rows.length - 1);
  await page.reload();
  await expect(listed).toHaveCount(rows.length - 1);
  await expect(p0).toHaveAttribute("aria-expanded", "false");
  await p0.click();
  await expect(listed).toHaveCount(rows.length);

  // The thread the reader has already read is a row of the list like any other, drawn as read:
  // no unread dot, and a title in the plain weight rather than the unread one.
  const readRow = page.locator(`[data-inbox-row="${read}"]`);
  await expect(readRow).toBeVisible();
  await expect(readRow.locator("[aria-label='Unread']")).toHaveCount(0);
  await expect(readRow.locator("a[data-inbox-owner]").first()).not.toHaveClass(/font-semibold/);
  await expect(listed).toHaveCount(rows.length);
});

test("the divider resizes the two columns, the width survives a reload, and the pane opens at about 60% @pr", async ({
  page,
  scene,
}) => {
  const { rows } = scene;
  await page.goto(INBOX);
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  const list = inboxList(page);
  const pane = inboxPane(page);
  await expect(pane).toBeVisible();

  const widths = async (): Promise<{ list: number; pane: number }> => {
    const [listBox, paneBox] = await Promise.all([list.boundingBox(), pane.boundingBox()]);
    if (listBox === null || paneBox === null) throw new Error("the Inbox drew one column");
    return { list: listBox.width, pane: paneBox.width };
  };
  const opened = await widths();
  expect(opened.pane / (opened.pane + opened.list)).toBeCloseTo(0.6, 2);

  // Dragging the line left widens the pane and narrows the list.
  const divider = page.getByTestId("inbox-pane-divider");
  await expect(divider).toHaveAttribute("aria-label", "Resize reading pane");
  const handle = await divider.boundingBox();
  if (handle === null) throw new Error("the divider drew no box");
  await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
  await page.mouse.down();
  await page.mouse.move(handle.x - 120, handle.y + handle.height / 2, { steps: 8 });
  await page.mouse.up();
  const dragged = await widths();
  expect(dragged.pane).toBeGreaterThan(opened.pane + 50);
  expect(dragged.list).toBeLessThan(opened.list - 50);

  // The reader's browser keeps it.
  await page.reload();
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  const reloaded = await widths();
  expect(Math.abs(reloaded.pane - dragged.pane)).toBeLessThanOrEqual(2);
});
