import { expect, test } from "@playwright/test";

import { seedLegionFixture, startFakeLegionDaemon } from "./legion-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

function segmentWidths(counts: readonly number[], floorPercent = 3): number[] {
  const total = counts.reduce((sum, count) => sum + count, 0);
  if (total <= 0) return counts.map(() => 100 / Math.max(counts.length, 1));
  const widths = counts.map((count) => (100 * count) / total);
  const atFloor = new Set<number>();
  for (;;) {
    const room = 100 - atFloor.size * floorPercent;
    const rest = widths.reduce((sum, width, index) => (atFloor.has(index) ? sum : sum + width), 0);
    const under = widths.findIndex(
      (width, index) => !atFloor.has(index) && (rest <= 0 || (room * width) / rest < floorPercent)
    );
    if (under === -1) {
      return widths.map((width, index) =>
        atFloor.has(index) ? floorPercent : rest <= 0 ? room / widths.length : (room * width) / rest
      );
    }
    atFloor.add(under);
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the bar counts the listed rows across the full width @legion @phone", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues?view=in-flight");

    const root = page.locator("section[data-issues-tab]");
    const bar = root.locator('[data-issues-bar][data-bar-column="stage"]');
    await expect(bar).toBeVisible();
    const rowCount = Number(
      await root.locator('[data-testid="issues-table-scroll"]').getAttribute("data-row-count")
    );
    const segments = bar.locator("[data-bar-segment]");
    const counts = await segments.evaluateAll((elements) =>
      elements.map((element) => Number(element.getAttribute("data-count")))
    );
    expect(counts.reduce((sum, count) => sum + count, 0)).toBe(rowCount);

    const [barBox, boxes] = await Promise.all([
      bar.boundingBox(),
      segments.evaluateAll((elements) =>
        elements.map((element) => element.getBoundingClientRect().toJSON())
      ),
    ]);
    expect(barBox).not.toBeNull();
    expect(boxes[0]?.x).toBeCloseTo(barBox?.x ?? Number.NaN, 1);
    for (const [index, width] of segmentWidths(counts).entries()) {
      expect(boxes[index]?.width).toBeCloseTo(((barBox?.width ?? 0) * width) / 100, 0);
    }

    const first = segments.first();
    const stage = await first.getAttribute("data-bar-segment");
    if (stage === null) throw new Error("the first bar segment has no stage value");
    await first.click();
    await expect.poll(() => new URL(page.url()).searchParams.getAll("f.stage")).toEqual([stage]);
    await first.click();
    await expect
      .poll(() =>
        root.locator('[data-testid="issues-table-scroll"]').getAttribute("data-row-count")
      )
      .toBe(String(rowCount));
  } finally {
    await context.close();
    await daemon.close();
  }
});
