import { expect, type Locator, type Page, test } from "@playwright/test";

import { answerAsk, createAsk, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The tester's drive of the Inbox's row stability (INFRA-450, round 8): a refetch must not move
 * the row the reader's hand is on, with the hand on the row and with it on the hover card that
 * row's own reference opened, on the Inbox page and in the Inbox drawer. Throwaway: it measures
 * and reports rather than pinning.
 */

const session = {
  actor: { id: "e2e-tester", kind: "session" as const },
  as: "agent" as const,
};

const TITLES = [
  "Anchor",
  "Bridge",
  "Cedar",
  "Dune",
  "Ember",
  "Fathom",
  "Gable",
  "Harbour",
  "Inlet",
  "Jetty",
  "Keel",
  "Lantern",
  "Marsh",
  "Nettle",
  "Orchard",
  "Pike",
  "Quarry",
  "Ridge",
  "Sable",
  "Thicket",
];

interface Seeded {
  id: string;
}

async function seedAsks(count: number): Promise<Seeded[]> {
  const asks: Seeded[] = [];
  for (let index = 0; index < count; index += 1) {
    const title = TITLES[index];
    if (title === undefined) throw new Error("out of issue titles");
    const issue = await createIssue({ project: "CORE", title });
    const ask = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: `Carded ask ${index + 1}` },
      session
    );
    asks.push({ id: ask.id });
  }
  return asks;
}

/** Newest first, so a row's place is counted back from the last ask seeded. */
function at(asks: Seeded[], fromTop: number): Seeded {
  const ask = asks[asks.length - 1 - fromTop];
  if (ask === undefined) throw new Error("ask missing");
  return ask;
}

async function topOf(locator: Locator): Promise<number> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error("row has no box");
  return box.y;
}

/** Every element between the row and the document that can scroll, and how far each has. */
async function scrollersOf(page: Page, id: string) {
  return page.evaluate((rowId) => {
    const row = document.querySelector(`[data-inbox-row="${rowId}"]`);
    const found: { tag: string; testid: string; scrollTop: number; clientHeight: number }[] = [];
    for (let node = row?.parentElement ?? null; node !== null; node = node.parentElement) {
      if (node.scrollHeight > node.clientHeight + 4) {
        found.push({
          clientHeight: node.clientHeight,
          scrollTop: Math.round(node.scrollTop),
          tag: node.tagName.toLowerCase(),
          testid: node.getAttribute("data-testid") ?? node.getAttribute("role") ?? "",
        });
      }
    }
    found.push({
      clientHeight: window.innerHeight,
      scrollTop: Math.round(window.scrollY),
      tag: "window",
      testid: "",
    });
    return found;
  }, id);
}

/**
 * The measurement: the hand comes to rest where `rest` puts it, a row above is answered
 * elsewhere, and the row the hand is on must not move. Answers how far it moved.
 */
async function measure({
  above,
  held,
  page,
  rest,
  within,
}: {
  above: Seeded;
  held: Seeded;
  page: Page;
  rest: "row" | "card";
  within: Locator;
}): Promise<number> {
  const row = within.locator(`[data-inbox-row="${held.id}"]`);
  await expect(row).toBeVisible();
  if (rest === "card") {
    const reference = row.locator("[data-dispatch-ref]").first();
    await reference.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await expect(reference).toHaveAttribute("aria-describedby", "ref-preview");
    // The card is drawn over the pointer, so this move lands inside it: the list hears
    // `pointerleave` although the reader left nothing.
    await card.hover();
  } else {
    await row.hover();
  }
  const before = await topOf(row);
  const scrollersBefore = await scrollersOf(page, held.id);
  await answerAsk(
    above.id,
    { expected_edited_at: null, selected: ["Ship"], text: "" },
    { login: "bob" }
  );
  await expect(within.locator(`[data-inbox-row="${above.id}"]`)).toHaveCount(0, {
    timeout: 20_000,
  });
  const after = await topOf(row);
  const scrollersAfter = await scrollersOf(page, held.id);
  console.log(
    `STABILITY rest=${rest} moved=${Math.round(after - before)}px ` +
      `before=${Math.round(before)} after=${Math.round(after)} ` +
      `scrollers=${JSON.stringify(scrollersBefore)} -> ${JSON.stringify(scrollersAfter)}`
  );
  return after - before;
}

async function openDrawer(page: Page): Promise<Locator> {
  await page.goto("/agents");
  await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
  await page.locator("body").click({ position: { x: 5, y: 400 } });
  await page.keyboard.press("i");
  const drawer = page.getByRole("dialog", { name: "Inbox" });
  await expect(drawer).toBeVisible();
  await expect(drawer.locator("[data-inbox-row]")).toHaveCount(20);
  return drawer;
}

test.beforeEach(async () => {
  await resetDatabase();
});

for (const rest of ["row", "card"] as const) {
  test(`probe: the Inbox page holds the row the hand rests on (${rest})`, async ({ browser }) => {
    test.setTimeout(180_000);
    await createProject({ key: "CORE", name: "Core" });
    const asks = await seedAsks(20);
    const context = await asUser(browser, "alice", { viewport: { height: 720, width: 1280 } });
    try {
      const page = await context.newPage();
      await page.goto("/");
      await expect(page.locator("[data-inbox-row]")).toHaveCount(20);
      const moved = await measure({
        above: at(asks, 10),
        held: at(asks, 11),
        page,
        rest,
        within: page.locator("body"),
      });
      expect(Math.abs(moved)).toBeLessThanOrEqual(2);
    } finally {
      await context.close();
    }
  });

  test(`probe: the Inbox drawer holds the row the hand rests on (${rest})`, async ({ browser }) => {
    test.setTimeout(180_000);
    await createProject({ key: "CORE", name: "Core" });
    const asks = await seedAsks(20);
    const context = await asUser(browser, "alice", { viewport: { height: 720, width: 1280 } });
    try {
      const page = await context.newPage();
      const drawer = await openDrawer(page);
      const held = at(asks, 11);
      // An exact scroll offset rather than scrollIntoViewIfNeeded, so every repeat starts the
      // drawer in the same place and the measurement cannot be decided by where it stopped.
      await drawer.locator(`[data-inbox-row="${held.id}"]`).scrollIntoViewIfNeeded();
      await page.evaluate(() => {
        const aside = document.querySelector<HTMLElement>('[data-testid="inbox-drawer"]');
        if (aside !== null) aside.scrollTop = 5839;
      });
      await page.waitForTimeout(500);
      const moved = await measure({
        above: at(asks, 10),
        held,
        page,
        rest,
        within: drawer,
      });
      expect(Math.abs(moved)).toBeLessThanOrEqual(2);
    } finally {
      await context.close();
    }
  });
}
