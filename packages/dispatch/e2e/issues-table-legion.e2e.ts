import { expect, type Page, test } from "@playwright/test";
import { patchIssue } from "./api";
import {
  pushLegionObservation,
  seedLegionFixture,
  seedLegionProject,
  startFakeLegionDaemon,
} from "./legion-seed";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Each assertion reads the real table and the board's stored observations. The fixture's
// waiting line reverses Dispatch rank, so deriving queue positions locally cannot pass.
test.beforeEach(async () => {
  await resetDatabase();
});
const row = (page: Page, key: string) => page.locator(`[data-issue-row="${key}"]`);
async function pick(page: Page, column: string, value: string): Promise<void> {
  await page.getByRole("button", { name: new RegExp(`^Filter ${column}(?::.*)?$`) }).click();
  await page.getByRole("combobox", { name: `Search ${column}` }).fill(value);
  await page.getByRole("option", { name: new RegExp(`^${value},`) }).click();
  await page.keyboard.press("Escape");
}

// Phone coverage keeps the real narrow viewport: horizontal scrolling must expose the same
// headers and searchable MultiSelects, without pinning the phase underneath the identity cells.
test("Legion phase groups, workflow sort and searchable filters @phone", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues");
    await page.getByRole("button", { name: /^Phase$/ }).click();
    await expect(page.locator("[data-issue-row]")).toHaveCount(5);
    expect(
      await page
        .locator("[data-issue-row]")
        .evaluateAll((rows) => rows.map((r) => r.getAttribute("data-issue-row")))
    ).toEqual(["ACME-4", "ACME-3", "ACME-5", "ACME-1", "ACME-2"]);
    await expect(row(page, "ACME-1").locator('[data-legion-fact="phase-time"]')).toContainText(
      "2 h"
    );
    await page.getByRole("combobox", { name: "Group by" }).selectOption("phase");
    await expect(
      page.getByRole("button", { name: /^Collapse Implementing, 1 issue$/ })
    ).toBeVisible();
    await expect(page.getByRole("button", { name: /^Collapse Queue, 3 issues$/ })).toBeVisible();
    await pick(page, "Phase", "Implementing");
    await expect(page.locator("[data-issue-row]")).toHaveCount(1);
    await expect(row(page, "ACME-1")).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("daemon queue replaces Dispatch order and optional Legion columns sort and filter @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues");
    await expect(row(page, "ACME-4").getByText("#1", { exact: true })).toBeVisible();
    await expect(row(page, "ACME-3").getByText("#2", { exact: true })).toBeVisible();
    for (const label of ["Slot", "Roles in a turn", "CI verdict"]) {
      await expect(
        page
          .getByRole("columnheader")
          .filter({ has: page.getByRole("button", { name: label, exact: true }) })
      ).toHaveCount(0);
    }
    await page.getByRole("button", { name: /^Columns/ }).click();
    for (const label of ["Slot", "Roles in a turn", "CI verdict"])
      await page.getByRole("checkbox", { name: label, exact: true }).check();
    await page.keyboard.press("Escape");
    await expect(row(page, "ACME-1").locator('[data-legion-fact="slot"]')).toHaveText("0");
    await expect(row(page, "ACME-1").locator('[data-legion-fact="roles"]')).toHaveText(
      "implementer"
    );
    await expect(row(page, "ACME-1").locator('[data-legion-fact="checks"]')).toHaveText("failing");
    for (const [label, value] of [
      ["Slot", "0"],
      ["Roles in a turn", "implementer"],
      ["CI verdict", "failing"],
    ] as const) {
      await page.getByRole("button", { name: label, exact: true }).click();
      await expect(
        page
          .getByRole("columnheader")
          .filter({ has: page.getByRole("button", { name: new RegExp(`^${label} (▲|▼)$`) }) })
      ).toHaveAttribute("aria-sort", /ascending|descending/);
      await pick(page, label, value);
      await expect(page.locator("[data-issue-row]")).toHaveCount(1);
      await expect(row(page, "ACME-1")).toBeVisible();
      await page.getByRole("button", { name: "Clear filters", exact: true }).click();
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("phase-or-status bar and slot cap move live with a stored Legion event @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await sql(`INSERT INTO issues (key, project_key, number, title, status, created_by, rank)
    VALUES ('ACME-6', 'ACME', 6, 'Ordinary Dispatch work', 'todo', '{"kind":"user","id":"alice"}', 'Z')`);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const frames: string[] = [];
    let legionSub: string | undefined;
    page.on("websocket", (socket) => {
      if (!socket.url().includes("/ws/live")) return;
      socket.on("framesent", ({ payload }) => {
        const frame = JSON.parse(String(payload));
        if (frame.type === "subscribe" && frame.view?.kind === "legion") legionSub = frame.sub;
      });
      socket.on("framereceived", ({ payload }) => frames.push(String(payload)));
    });
    await page.goto("/projects/ACME/issues?group=phase&sort=phase");
    const bar = page.getByRole("group", { name: "Issues by phase or status" });
    await expect(bar.getByRole("button", { name: "Implementing 1", exact: true })).toBeVisible();
    await expect(bar.getByRole("button", { name: "Todo 1", exact: true })).toBeVisible();
    await expect(page.locator("[data-legion-slots]")).toHaveText("2 of 4 slots in use");
    await expect(page.locator("[data-legion-read-at]")).toContainText("Read");
    const navigations: string[] = [];
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) navigations.push(frame.url());
    });
    await pushLegionObservation("ACME-1", "phase", '"reviewing"');
    await expect(row(page, "ACME-1").locator("[data-legion-phase]")).toContainText("Reviewing");
    await expect(page.getByRole("button", { name: /^Collapse Reviewing, 1 issue$/ })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Collapse Implementing,/ })).toHaveCount(0);
    await expect(bar.getByRole("button", { name: "Reviewing 1", exact: true })).toBeVisible();
    await expect(bar.getByRole("button", { name: "Implementing 1", exact: true })).toHaveCount(0);
    expect(
      frames.some((payload) => {
        const frame = JSON.parse(payload);
        return (
          frame.type === "delta" &&
          frame.sub === legionSub &&
          JSON.stringify(frame.set?.columns ?? []).includes('"phase":"reviewing"')
        );
      })
    ).toBe(true);
    expect(navigations).toEqual([]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("failed board read marks Legion data stale and names when it was read @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await sql(`UPDATE legion_reads SET read_at = now() - interval '121 seconds', ok = false,
    error = 'No daemon event or heartbeat for 120 seconds' WHERE project = 'ACME'`);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues");
    const stale = page.locator("[data-legion-read-error]");
    await expect(stale).toContainText("Legion data is stale since");
    await expect(stale).toContainText("No daemon event or heartbeat for 120 seconds");
    await expect(stale.locator("time")).toHaveAttribute("datetime", /T/);
    await expect(row(page, "ACME-1").locator("[data-legion-phase]")).toContainText("Implementing");
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a project without Legion keeps the Dispatch queue and ignores Legion URL controls @phone", async ({
  browser,
}) => {
  await seedLegionProject();
  for (const key of ["ACME-3", "ACME-4", "ACME-5"]) await patchIssue(key, { labels: ["legion"] });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues?sort=phase&group=phase&f.slot=0");
    await expect(page.locator("[data-issue-row]")).toHaveCount(5);
    await expect(row(page, "ACME-3").getByText("#1", { exact: true })).toBeVisible();
    await expect(row(page, "ACME-4").getByText("#2", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Phase", exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Columns", exact: true }).click();
    for (const name of ["Phase", "Slot", "Roles in a turn", "CI verdict"]) {
      await expect(page.getByRole("checkbox", { name, exact: true })).toHaveCount(0);
    }
    await expect(page.locator("[data-legion-slots]")).toHaveCount(0);
  } finally {
    await context.close();
  }
});
