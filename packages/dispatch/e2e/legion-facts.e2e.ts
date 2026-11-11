// The per-issue Legion facts under an open page (dispatch://INFRA-2915, k7): a write that changes
// one issue's facts is recomputed by the facts consumer, the write of the stored row logs a
// `delivery_changes` row of the kind `issue_facts`, and the open Legion view redraws that issue's
// card from the new row with no reload.
//
// It is modelled on `legion.e2e.ts`'s "a live push moves a card between columns with no reload",
// which proves the same path for a read of the daemon. This one starts from an event instead, so
// the half it adds is the consumer between the write and the view: nothing here writes an
// `issue_facts` row, and a spec that wrote one would pass with the consumer stopped.
//
// Tagged `@legion-facts`, so `--grep @legion-facts` runs it alone.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { LEGION_FACTS_PROJECT, seedLegionFactsFixture } from "./legion-facts-seed";
import { startFakeLegionDaemon } from "./legion-seed";
import { sql } from "./psql";
import { expect, test } from "./test";
import { asUser } from "./users";

/** How long a step that waits on the facts consumer is given. The consumer sweeps every 30 s
 *  whatever wakes it (`legionfacts.SweepEvery`), so one pass is 30 s away at worst and two are
 *  what this row waits out: the pass that first writes the issue's row, and the pass that
 *  rewrites it after the second event. */
const consumerPass = 75_000;

/** One event of a Legion role working the issue, which is what makes the project carry Legion
 *  phase work for it and where that issue's work time starts (`legion.RoleSessionWorks`). `ago`
 *  is a Postgres interval, and `seq` is the issue's own event number, unique within the issue. */
async function writeRoleEvent(
  issue: string,
  role: string,
  seq: number,
  ago: string
): Promise<void> {
  const actor = `{"kind":"session","id":"s-${role}-${issue}","origin":{"session_title":"Legion ${role} · ${issue}"}}`;
  await sql(
    `INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at)
       VALUES ('${issue}', ${seq}, 'comment.created', '${actor}'::jsonb, '{}'::jsonb, false,
         now() - interval '${ago}')`
  );
}

// SLOW(>=2s): the row waits out two passes of the facts consumer, which sweeps every 30 s, with a
// sign-in, a board seed of 24 cards and a live socket either side of them.
test("a facts write redraws a card's work time with no reload @legion-facts", async ({
  browser,
}) => {
  // Two consumer passes and the page work around them, against the 60 s every other row is
  // given. The deadline is the consumer's pace, not this row's own work.
  test.setTimeout(240_000);
  const daemon = await startFakeLegionDaemon({});
  const fixture = await seedLegionFactsFixture(daemon.url);
  // The first card of the fixture's board: an open issue in the Implementing column, which the
  // view draws and the consumer's sweep recomputes while it is open.
  const issue = fixture.keys[0];

  // The issue's Legion phase work, an hour and ten minutes old. The consumer writes its stored
  // row from this; the fixture writes no `issue_facts` row of its own.
  await writeRoleEvent(issue, "implementer", 1, "70 minutes");
  await expect
    .poll(
      async () =>
        Number(await sql(`SELECT count(*) FROM issue_facts WHERE issue_key = '${issue}'`)),
      {
        message: `the facts consumer writes the stored row of ${issue}`,
        timeout: consumerPass,
      }
    )
    .toBeGreaterThan(0);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // Every page load, so the redraw below can be read as one the open page made: a reload would
  // count a second one.
  let loads = 0;
  page.on("load", () => {
    loads += 1;
  });

  try {
    await page.goto(`/projects/${LEGION_FACTS_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();
    const card = page.locator(`[data-legion-card="${issue}"]`);
    const workTime = card.locator('[data-legion-fact="work-time"]');
    // The work time the server stored: an hour and ten minutes since the one worker event.
    await expect(workTime).toContainText("1 h", { timeout: consumerPass });
    const before = await workTime.textContent();

    // One event that changes that issue's facts: an earlier worker event, which moves the start
    // of its work time back and so makes its work time nine hours rather than one.
    await writeRoleEvent(issue, "architect", 2, "9 hours");

    // No reload: the open page draws the new work time under the reader.
    await expect(workTime).toContainText("9 h", { timeout: consumerPass });
    expect(await workTime.textContent()).not.toBe(before);
    expect(loads).toBe(1);

    // What woke the page: the facts write's own change row, which names the project and the
    // issue whose row moved.
    const change = await sql(
      `SELECT kind || ' ' || repo || ' ' || ref FROM delivery_changes
         WHERE kind = 'issue_facts' AND ref = '${issue}' ORDER BY seq DESC LIMIT 1`
    );
    expect(change).toBe(`issue_facts ${LEGION_FACTS_PROJECT} ${issue}`);
  } finally {
    await context.close();
    await daemon.close();
  }
});
