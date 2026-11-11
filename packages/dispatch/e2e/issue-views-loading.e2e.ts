import { expect, test } from "@playwright/test";

import { delayLegionBoard } from "./legion-board-delay";
import { seedLegionFixture, seedLegionMetricsCorpus, startFakeLegionDaemon } from "./legion-seed";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

function issueReads(urls: readonly string[]): URL[] {
  return urls
    .map((url) => new URL(url))
    .filter(
      (url) => url.pathname === "/api/v1/issues" && url.searchParams.get("project") === "ACME"
    );
}

// A delayed live snapshot never turns a board-free view into a skeleton, and every URL still
// selects one bounded listing read rather than the former full listing.
test("the views read only what they list, while board-free views draw before the board @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  await sql(`INSERT INTO issues (key, project_key, number, title, status, created_by, rank, closed_at)
    VALUES ('ACME-8', 'ACME', 8, 'Finished in the custom window', 'done', '{"kind":"user","id":"alice"}', 'Zb', '2026-11-03T12:00:00Z')`);
  const context = await asUser(browser, "alice");
  try {
    const views: readonly {
      path: string;
      expected: readonly Record<string, string>[];
      waitsForLegion?: boolean;
    }[] = [
      {
        expected: [{ open: "true" }],
        path: "/projects/ACME/issues?view=open",
      },
      {
        expected: [{ closed_since: "present", status: "done" }],
        path: "/projects/ACME/issues?view=finished",
        waitsForLegion: true,
      },
      {
        expected: [
          {
            closed_before: "2026-11-04T00:00:00Z",
            closed_since: "2026-11-03T00:00:00Z",
            status: "done",
          },
        ],
        path: "/projects/ACME/issues?view=finished&f.closed=2026-11-03..2026-11-04",
        waitsForLegion: true,
      },
      {
        expected: [{ open: "true" }, { closed_since: "present", status: "done" }],
        path: "/projects/ACME/issues?view=open&nofilter=1",
      },
    ];
    for (const entry of views) {
      const page = await context.newPage();
      const requests: string[] = [];
      page.on("request", (request) => requests.push(request.url()));
      await delayLegionBoard(page, 5_000);
      await page.goto(entry.path);
      if (entry.waitsForLegion) {
        await expect(page.getByLabel("Loading Legion data")).toBeVisible({ timeout: 4_000 });
        await expect(page.locator("[data-issue-row]")).toHaveCount(0);
      } else await expect(page.locator("[data-issue-row]").first()).toBeVisible({ timeout: 4_000 });
      const reads = issueReads(requests);
      expect(reads).not.toHaveLength(0);
      expect(
        reads.filter((url) => !url.searchParams.has("open") && !url.searchParams.has("status"))
      ).toEqual([]);
      for (const expected of entry.expected) {
        if (expected.closed_since === "present") {
          expect(
            reads.some(
              (url) =>
                url.searchParams.get("status") === expected.status &&
                url.searchParams.get("closed_since") !== null
            )
          ).toBe(true);
        } else {
          expect(
            reads.some((url) =>
              Object.entries(expected).every(([key, value]) => url.searchParams.get(key) === value)
            )
          ).toBe(true);
        }
      }
      await page.close();
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a cold Finished view waits for the board before it draws Legion-filtered rows @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  await sql(`INSERT INTO issues (key, project_key, number, title, status, created_by, rank, closed_at)
    VALUES ('ACME-8', 'ACME', 8, 'Finished outside Legion', 'done', '{"kind":"user","id":"alice"}', 'Zb', now() - interval '2 hours')`);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await delayLegionBoard(page, 5_000);
    await page.goto("/projects/ACME/issues?view=finished");
    await expect(page.getByLabel("Loading Legion data")).toBeVisible({ timeout: 4_000 });
    await expect(page.locator("[data-issue-row]")).toHaveCount(0);
    await expect(page.locator("[data-issue-row]")).toHaveCount(2, { timeout: 8_000 });
  } finally {
    await context.close();
    await daemon.close();
  }
});
