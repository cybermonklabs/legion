import { expect, test } from "@playwright/test";
import { seedLegionFixture, seedLegionMetricsCorpus, startFakeLegionDaemon } from "./legion-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

interface BoardCard {
  key: string;
  waiting_place: number | null;
}

interface Board {
  columns: readonly { cards: readonly BoardCard[] }[];
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("Legion columns equal the board and link to the timeline @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues?view=open");
    await page.getByRole("button", { name: "Columns" }).click();
    for (const column of ["Phase", "Slot use", "Waiting", "Waited", "In phase", "Tree"]) {
      await page.getByRole("checkbox", { name: column, exact: true }).check();
    }
    await page.keyboard.press("Escape");
    const board = (await page.evaluate(async () => {
      const response = await fetch("/api/v1/legion/board?project=ACME");
      return response.json();
    })) as Board;
    const cards = board.columns.flatMap((column) => column.cards);

    for (const card of cards) {
      if (card.waiting_place === null) continue;
      await expect(
        page.locator(`[data-issue-row="${card.key}"] [data-cell="queue"]`)
      ).toHaveAttribute("data-value", String(card.waiting_place));
    }
    await expect(page.locator('[data-issue-row="ACME-1"] [data-cell="slot_use"]')).toHaveText(
      "Working"
    );
    await expect(page.locator('[data-issue-row="ACME-5"] [data-cell="waiting_reason"]')).toHaveText(
      "Blocked by ACME-1"
    );

    const phaseLink = page.locator(
      '[data-issue-row="ACME-1"] [data-cell="phase"] a[data-timeline-link]'
    );
    await phaseLink.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(/\/projects\/ACME\/legion\/ACME-1$/u);

    await page.goto("/projects/ACME/issues?view=finished");
    await page.getByRole("button", { name: "Columns" }).click();
    await page.getByRole("checkbox", { name: "Time per phase", exact: true }).check();
    await page.keyboard.press("Escape");
    const mode = page.getByRole("group", { name: "Time per phase mode" });
    await expect(mode.getByRole("button", { name: "Share" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await mode.getByRole("button", { name: "Hours" }).click();
    await expect(page.locator("[data-phase-hours-total]").first()).toHaveText(/h$/);

    const phaseHoursLink = page.locator(
      '[data-issue-row="ACME-6"] [data-cell="phase_hours"] a[data-timeline-link]'
    );
    await phaseHoursLink.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(/\/projects\/ACME\/legion\/ACME-6$/u);
  } finally {
    await context.close();
    await daemon.close();
  }
});
