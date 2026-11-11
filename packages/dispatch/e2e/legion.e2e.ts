import { expect, test } from "@playwright/test";

import { baseUrl, postLegionEvents, userHeaders } from "./api";
import {
  LEGION_PROJECT,
  seedLegionFixture,
  seedLegionMetricsCorpus,
  seedLegionProject,
  startFakeLegionDaemon,
} from "./legion-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("the issues table shows the daemon's phases and issue facts @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    const table = page.getByRole("table", { name: "Project issues table" });
    await expect(table).toBeVisible();
    await expect(page.locator("[data-issues-counted]")).toHaveText("5 issues");
    await page.getByRole("button", { name: "Columns", exact: true }).click();
    for (const name of ["Slot", "Roles", "CI"]) {
      await page.getByRole("checkbox", { name, exact: true }).check();
    }
    await page.keyboard.press("Escape");
    const issue = table.locator('[data-issue-row="ACME-1"]');
    await expect(issue).toContainText("Ship the widgets");
    await expect(issue.getByRole("combobox", { name: "Priority of ACME-1" })).toHaveValue("0");
    await expect(issue.locator('[data-legion-fact="phase-time"]')).toContainText("2 h");
    await expect(issue.locator('[data-legion-fact="slot"]')).toHaveText("0");
    await expect(issue.locator('[data-legion-fact="roles"]')).toHaveText("implementer");
    await expect(issue.locator('[data-legion-fact="checks"]')).toHaveText("failing");
    expect(
      await table
        .locator("[data-issue-row]")
        .evaluateAll((rows) => rows.slice(0, 3).map((row) => row.getAttribute("data-issue-row")))
    ).toEqual(["ACME-4", "ACME-3", "ACME-5"]);
    await expect(
      table.locator('[data-issue-row="ACME-5"] [data-legion-fact="blocked-by"]')
    ).toContainText("Blocked by ACME-1");
    await expect(
      table.locator('[data-issue-row="ACME-2"] [data-legion-fact="waiting-on"]')
    ).toContainText("Waiting on ask");
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("the Settings page's control changes the admission cap of a daemon @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({ admission_cap: { applied: "at-once" } });
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // The control sits in the project's Legion setting on the Settings page (dispatch://INFRA-979).
    await page.goto("/settings");

    // Each setting with its value and where it came from, as the daemon reports them.
    const cap = page.locator('[data-legion-setting="admission_cap"]');
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("4");
    await expect(cap.locator('[data-legion-setting-source="file"]')).toContainText(
      "configuration file"
    );
    await expect(
      page.locator('[data-legion-setting="review_round_cap"] [data-legion-setting-source="route"]')
    ).toContainText("Changed here by bob");

    await page.getByLabel("New value for admission_cap").fill("6");
    await page.getByRole("button", { name: "Change admission_cap" }).click();

    // What the daemon said about the key, and the new value with the route as its source and the
    // person who changed it.
    await expect(cap.locator("[data-legion-setting-result]")).toHaveText("Applied at once");
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("6");
    await expect(cap.locator('[data-legion-setting-source="route"]')).toContainText(
      "Changed here by alice"
    );

    // Dispatch relayed the change itself, naming the person who asked for it.
    expect(daemon.changes).toEqual([{ by: "alice", changes: { admission_cap: 6 } }]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The daemon's third verdict, the one a person must not read as a success: a key it took into
// its own settings and could not carry into the thing that holds a copy of the value
// (`failed`, the daemon's internal/api/settings_route.go). The row's answer is announced
// (`role="alert"`, on the span RowAnswer renders), reads the holder's own error, and the value
// stays the new one, because the daemon's settings carry it and only a client, listener or
// connection kept the old.
//
// It drives `admission_cap`: the fixture seeds that key and `review_round_cap` and no other
// (`legion-seed.ts`), and the panel reads the verdict alone — `describeVerdict` keys on nothing
// about the path. The verdict is the shape a real failure has, the daemon's own
// `worker_rpc_timeout_seconds`: `holder` is the row's named holder, while the holder that kept
// the old value is named inside `failed`, which the daemon writes as `<holder>: <error>`
// (internal/daemon/holders.go). So the row proves the other half too — the text names what the
// daemon's text names, never the row's holder.
test("the Settings page's control shows a failed verdict as a failure and keeps the new value @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({
    admission_cap: {
      failed: "nats: reopen the intake consumers: nats: no servers available for connection",
      holder: "worker-stream",
      old: "4",
      new: "6",
    },
  });
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    const cap = page.locator('[data-legion-setting="admission_cap"]');
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("4");

    await page.getByLabel("New value for admission_cap").fill("6");
    await page.getByRole("button", { name: "Change admission_cap" }).click();

    const answer = cap.locator("[data-legion-setting-result]");
    const failed =
      "The daemon took the new value, and what holds it kept the old one: " +
      "nats: reopen the intake consumers: nats: no servers available for connection";
    await expect(answer).toHaveText(failed);
    // Announced as a failure, and naming no holder the daemon's own text did not name.
    await expect(answer.locator('[role="alert"]')).toHaveText(failed);
    await expect(answer).not.toContainText("worker-stream");
    // The daemon took the value, so the row shows the new one rather than rolling back to 4.
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("6");
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a project without a Legion setting gains phase columns when Settings names its daemon @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionProject();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
    await expect(page.getByRole("columnheader").filter({ hasText: /^Phase/ })).toHaveCount(0);
    await expect(page.getByRole("tab", { name: "Evals", exact: true })).toHaveCount(0);

    // The Settings page, beside the Delivery settings.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Legion" })).toBeVisible();
    await page.getByLabel("Legion daemon state URL").fill(daemon.url);
    await page
      .getByRole("combobox", { exact: true, name: "Legion project" })
      .selectOption(LEGION_PROJECT);
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
    );
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    expect((await saved).status()).toBe(200);

    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("columnheader").filter({ hasText: /^Phase/ })).toBeVisible();
    await expect(page.locator('[data-issue-row="ACME-1"] [data-legion-phase]')).toContainText(
      "Implementing"
    );
    await expect(page.getByRole("tab", { name: "Evals" })).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The other half of the Settings control: the URL it refuses, and the Remove that takes the
// view away again. A state URL that is not an absolute http(s) one with a host and no path is
// refused, and the page's own wording must be the server's own, since the page carries a second
// copy of the rule (`legionStateUrl` in LegionDaemonsSection.tsx) and only the server's copy
// decides what is stored.
test("the Settings page refuses a bad daemon URL and Remove takes the phase columns away @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const bad = `${daemon.url}/legion/v1/state`;

  try {
    // What the server says about this URL, so the page can be held to its wording.
    const refusal = await fetch(new URL(`/api/v1/settings/legion/${LEGION_PROJECT}`, baseUrl), {
      body: JSON.stringify({ state_url: bad }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    });
    expect(refusal.status).toBe(400);
    const refused = (await refusal.json()) as { code: string; error: string };
    expect(refused.code).toBe("LEGION_SETTING_INPUT");

    await page.goto("/settings");
    const section = page.locator("[data-legion-daemons]");
    await expect(section).toContainText(daemon.url);

    // The page refuses the same URL, in the server's own words, and sends no write at all.
    let wrote = false;
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        request.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
      ) {
        wrote = true;
      }
    });
    await page.getByLabel("Legion daemon state URL").fill(bad);
    await page
      .getByRole("combobox", { exact: true, name: "Legion project" })
      .selectOption(LEGION_PROJECT);
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    await expect(page.getByRole("alert")).toHaveText(refused.error);
    expect(wrote).toBe(false);
    // The refusal changed nothing: the project still reads the daemon it did.
    await expect(section).toContainText(daemon.url);

    // Remove stops the reading, and the Issues page loses its Legion view.
    const removed = page.waitForResponse(
      (response) =>
        response.request().method() === "DELETE" &&
        response.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
    );
    await page
      .getByRole("button", { name: `Remove the Legion daemon of ${LEGION_PROJECT}` })
      .click();
    expect((await removed).status()).toBeLessThan(300);

    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
    await expect(page.getByRole("columnheader").filter({ hasText: /^Phase/ })).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a live push moves an issue's phase without reloading the table @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    const phase = page.locator('[data-issue-row="ACME-1"] [data-legion-phase]');
    await expect(phase).toContainText("Implementing");
    expect(
      await postLegionEvents(
        {
          project: LEGION_PROJECT,
          generation: "e2e-legion",
          events: [
            {
              seq: 1,
              at: new Date().toISOString(),
              kind: "phase",
              issue: "ACME-1",
              value: "testing",
            },
          ],
        },
        { as: "agent" }
      )
    ).toEqual({ last_seq: 1 });
    await expect(phase).toContainText("Testing", { timeout: 15000 });
  } finally {
    await context.close();
    await daemon.close();
  }
});

for (const path of ["/delivery", "/pipeline"]) {
  test(`${path} lands on the default project's Evals tab @legion`, async ({ browser }) => {
    const daemon = await startFakeLegionDaemon({});
    await seedLegionFixture(daemon.url);
    const context = await asUser(browser, "alice");
    const page = await context.newPage();

    try {
      await page.goto(path);
      await expect(page).toHaveURL(new RegExp(`/projects/${LEGION_PROJECT}/evals$`));
      await expect(page.locator("[data-evals-tab]")).toBeVisible();
    } finally {
      await context.close();
      await daemon.close();
    }
  });
}

test("the Evals tab keeps metrics and charts, not issue rows @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}/legion`);
    await expect(page).toHaveURL(new RegExp(`/projects/${LEGION_PROJECT}/evals$`));
    await expect(page.locator("[data-evals-tab]")).toBeVisible();
    await expect(page.locator("[data-legion-section]")).toHaveCount(4);
    await expect(page.locator('[data-legion-section="finished"]')).toHaveCount(0);
    await expect(page.locator("[data-issue-row]")).toHaveCount(0);
    await expect(page.getByRole("columnheader", { name: "Issue", exact: true })).toHaveCount(0);
    await expect(page.locator("[data-legion-delivery-unmeasured]")).toContainText(
      "CI and deploys: not measured"
    );
    const headline = (id: string) =>
      page.locator(`[data-legion-section="${id}"] [data-legion-headline]`);
    await expect(headline("phase_breakdown")).toContainText("2 issues finished");
    await expect(headline("work_time")).toContainText("median");
    await expect(headline("rework")).toContainText("median 0.5 review rounds");
    await expect(headline("rework")).toContainText("50% sent back");
    await expect(headline("splitting")).toContainText("1 issue filed");
    await expect(page.locator("[data-legion-range]")).toContainText(" to ");

    const work = page.locator('[data-legion-section="work_time"]');
    await expect(work.locator("[data-chart-unit]").first()).toContainText("hours");
    await expect(work.getByRole("group", { name: "Work time view" })).toBeVisible();
    await expect(work.locator('select[data-chart-compare="Work time"]')).toBeVisible();

    await page.getByRole("button", { name: "Roots" }).click();
    await expect(headline("phase_breakdown")).toContainText("1 issue finished");
    await page.getByRole("button", { name: "All issues" }).click();

    await page.goto(`/projects/${LEGION_PROJECT}/legion/${LEGION_PROJECT}-1`);
    await expect(page).toHaveURL(
      new RegExp(`/projects/${LEGION_PROJECT}/legion/${LEGION_PROJECT}-1$`)
    );
    await expect(page.locator("[data-evals-tab]")).toBeVisible();
    await expect(page.locator("[data-legion-timeline]")).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});
