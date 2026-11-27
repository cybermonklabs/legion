/** Time per phase is read against the board route's hours as the facts consumer writes them from
 *  role events, phase observations and an ask, with every board frame of the live socket
 *  rewritten in flight. */
// Time per phase on the Issues tab (dispatch://INFRA-3221, Verification k8): its two modes, the
// tooltip that names each phase's hours and share, where that tooltip opens near the window's
// edges, each phase's fixed fill in both colour schemes, and the bars and totals of Hours on
// each page.
//
// WIDGETS is seeded with `seedLegionFactsFixture`, with two issues more, so 26 open issues carry
// Legion work: each has a role event and phase observations in three phases at least, and the
// facts consumer's row (`waitForIssueFacts`, up to 75 s, the one wait of this row). Issue P walks
// every phase from Design to Held and holds an ask open on the way, so its facts carry ten parts;
// the row adds an eleventh, Other, to every board frame P is in with `page.routeWebSocket`, since
// phase observations of `breakdownParts` leave no hour to Other (metrics/breakdown.go). Each
// expected value is read from the board route.
import type { LegionBoardResponse as Board } from "../web/src/api/types";
import { getLegionBoard } from "./api";
import {
  type BoardFrame,
  hoursWords,
  issuesTable,
  routeLive,
  rowKeys,
  tooltipLines,
} from "./issues-views-shared";
import {
  LEGION_FACTS_PROJECT,
  roleEventStatement,
  seedLegionFactsFixture,
  waitForIssueFacts,
} from "./legion-facts-seed";
import {
  SEED_ACTOR,
  settledCounters,
  startFakeLegionDaemon,
  writeEventsForFacts,
} from "./legion-seed";
import { sql } from "./psql";
import { expect, type Locator, test } from "./test";
import { asUser } from "./users";

const WIDGETS = LEGION_FACTS_PROJECT;

/** Issue P, which walks every phase, and the second issue this row adds. */
const P = `${WIDGETS}-25`;
const SECOND = `${WIDGETS}-26`;

/** The hours the row adds to P under Other in every board frame. */
const OTHER_HOURS = 1.5;

/** P's ten parts, the keys the board's `hours_by_phase` holds for it. */
const P_KEYS = [
  "design",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
  "awaiting_person",
  "held",
];

/** Each phase's words and its fill, in the light scheme and in the dark one, as the Design's
 *  table lists them, Retro's from the architect's correction
 *  (dispatch://INFRA-3221/comment/57ac6e4d-c2d8-4e1d-a082-f26b0dc34ae7), in the board's order
 *  with Other last. */
const PHASES: readonly (readonly [string, string, string, string])[] = [
  ["design", "Design", "#E69F00", "#E69F00"],
  ["planning", "Planning", "#56B4E9", "#56B4E9"],
  ["implementing", "Implementing", "#009E73", "#009E73"],
  ["testing", "Testing", "#F0E442", "#F0E442"],
  ["reviewing", "Reviewing", "#0072B2", "#0072B2"],
  ["accepting", "Accepting", "#D55E00", "#D55E00"],
  ["retro", "Retro", "#666666", "#666666"],
  ["merging", "Merging", "#CC79A7", "#CC79A7"],
  ["awaiting_person", "Waiting on a person", "#000000", "#FFFFFF"],
  ["held", "Held", "#999999", "#999999"],
  ["other", "Other", "#D9D9D9", "#D9D9D9"],
];

/** A `#RRGGBB` colour as the browser's computed `rgb(r, g, b)`. */
function rgb(hex: string): string {
  const [r, g, b] = [1, 3, 5].map((at) => Number.parseInt(hex.slice(at, at + 2), 16));
  return `rgb(${r}, ${g}, ${b})`;
}

/** The facts the board holds for `key`, with the Other hours the row adds to P's frames. */
function hoursOf(board: Board, key: string): Record<string, number> {
  const held = { ...(board.issues?.[key]?.hours_by_phase ?? {}) };
  if (key === P) held.other = (held.other ?? 0) + OTHER_HOURS;
  return held;
}

const totalOf = (hours: Readonly<Record<string, number>>): number =>
  Object.values(hours).reduce((sum, value) => (value > 0 ? sum + value : sum), 0);

/** Adds the Other hours to P's entry of the `issues` map a board frame carries. */
function addOther(frame: BoardFrame): void {
  const entries = [frame.body?.issues, frame.maps?.issues, frame.set?.issues];
  for (const issues of entries) {
    const entry = (issues as Record<string, { hours_by_phase?: Record<string, number> }> | null)?.[
      P
    ];
    if (entry === undefined || entry === null) continue;
    entry.hours_by_phase = {
      ...(entry.hours_by_phase ?? {}),
      other: (entry.hours_by_phase?.other ?? 0) + OTHER_HOURS,
    };
  }
}

/** One event of `issue` at its next seq, `ago` before now: what the broker would have written. */
async function writeEvent(
  issue: string,
  type: string,
  actor: string,
  payload: string,
  ago: string
): Promise<void> {
  await sql(
    `WITH next AS (UPDATE issues SET last_seq = last_seq + 1 WHERE key = '${issue}' RETURNING last_seq)
     INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at)
       SELECT '${issue}', last_seq, '${type}', '${actor}'::jsonb, '${payload}'::jsonb, false,
         now() - interval '${ago}'
       FROM next`
  );
}

/** WIDGETS with 26 open issues of Legion work: the fixture's 24 cards, each given two older phase
 *  observations and a role event, P with a phase observation in each of nine phases and an ask a
 *  Legion session of it held open for half an hour of its Implementing, and SECOND with three
 *  phases. Answers every key. */
async function seedPhases(daemonUrl: string): Promise<string[]> {
  const fixture = await seedLegionFactsFixture(daemonUrl);
  await sql(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank, created_at) VALUES
       ('${P}', '${WIDGETS}', 25, 'Walk the widget through every phase', 'in_progress', 1, ${SEED_ACTOR}, '0012m', now() - interval '30 hours'),
       ('${SECOND}', '${WIDGETS}', 26, 'Plan the gadget follow-up', 'in_progress', 2, ${SEED_ACTOR}, '0026', now() - interval '30 hours')`,
    ...settledCounters(WIDGETS)
  );
  const observed: string[] = [];
  const observe = (issue: string, phase: string, ago: string) => {
    observed.push(
      `('${WIDGETS}', '${issue}', 'phase', '"${phase}"'::jsonb, now() - interval '${ago}')`
    );
  };
  const early = ["design", "planning", "implementing", "testing"];
  // Each card's column, which for the four early phases is the phase it is in now.
  const phaseOf = new Map(
    ((await getLegionBoard(WIDGETS)).columns ?? []).flatMap((column) =>
      (column.cards ?? []).map((card) => [card.key, column.id] as const)
    )
  );
  for (const key of fixture.keys) {
    const [older, newer] = early.filter((phase) => phase !== phaseOf.get(key));
    observe(key, older ?? "design", "10 hours");
    observe(key, newer ?? "planning", "6 hours");
  }
  // P: from its first worker event 21 hours ago, two hours in each phase and the last four in
  // Held, the ask taking half an hour out of Implementing.
  const walk = [
    "design",
    "planning",
    "implementing",
    "testing",
    "reviewing",
    "accepting",
    "retro",
    "merging",
    "held",
  ];
  for (const [index, phase] of walk.entries()) observe(P, phase, `${20 - index * 2} hours`);
  observe(SECOND, "design", "9 hours");
  observe(SECOND, "planning", "5 hours");
  observe(SECOND, "implementing", "2 hours");
  await sql(
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at) VALUES ${observed.join(", ")}`,
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${WIDGETS}', now(), true, null, 8192)`,
    `SELECT pg_notify('dispatch_legion_state', '${WIDGETS}')`
  );
  const keys = [...fixture.keys, P, SECOND];
  await writeEventsForFacts(
    ...fixture.keys.map((key) => roleEventStatement(key, "implementer", "12 hours")),
    roleEventStatement(SECOND, "planner", "10 hours"),
    roleEventStatement(P, "architect", "21 hours")
  );
  const session = `{"kind":"session","id":"s-architect-${P}","origin":{"session_title":"Legion architect · ${P}"}}`;
  const ask = `{"id":"00000000-0000-4000-8000-000000000025"}`;
  await writeEvent(P, "ask.opened", session, ask, "15 hours 30 minutes");
  await writeEvent(P, "ask.answered", `{"kind":"user","id":"alice"}`, ask, "15 hours");
  await waitForIssueFacts(keys);
  return keys;
}

/** P's Time per phase cell, its row and the cell's button. */
const phaseCell = (table: Locator) => table.locator(`tr[data-issue-row="${P}"] [data-phase-hours]`);

/** The boxes of P's cell, the open tooltip and the window. */
function boxes(cell: Locator) {
  return cell.evaluate((node) => {
    const tooltip = document.querySelector('[role="tooltip"]');
    const own = node.getBoundingClientRect();
    const tip = tooltip?.getBoundingClientRect();
    return {
      cell: { top: own.top, bottom: own.bottom, left: own.left, right: own.right },
      tip:
        tip === undefined
          ? undefined
          : {
              top: tip.top,
              bottom: tip.bottom,
              left: tip.left,
              right: tip.right,
              height: tip.height,
            },
      window: { width: document.documentElement.clientWidth, height: window.innerHeight },
    };
  });
}

// SLOW(>=2s): the facts consumer's rows of 26 issues, up to 75 s, then one cell read on hover, on
// focus, at two sizes, in two schemes and in both modes on two pages.
test("Time per phase opens on Share, its tooltip names each of P's eleven parts on hover and on focus inside the window, each part takes its fixed fill in both schemes, and Hours draws each page's bars and totals through a reload", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await startFakeLegionDaemon();
  const context = await asUser(browser, "alice");
  try {
    const keys = await seedPhases(daemon.url);
    expect(keys.length, "the open issues with Legion work").toBeGreaterThanOrEqual(26);
    await expect
      .poll(
        async () => {
          const hours = (await getLegionBoard(WIDGETS)).issues?.[P]?.hours_by_phase ?? {};
          return Object.keys(hours)
            .filter((key) => (hours[key] ?? 0) > 0)
            .sort();
        },
        { message: "P's hours_by_phase on the board", timeout: 30_000 }
      )
      .toEqual([...P_KEYS].sort());
    const page = await context.newPage();
    await routeLive(page, { edit: addOther });
    // Tall enough that the table draws every row of its page: it draws only the rows in view.
    await page.setViewportSize({ height: 3000, width: 1440 });
    await page.goto(
      `/projects/${WIDGETS}/issues?view=open&size=25&cols=priority,key,title,status,phase_hours`
    );
    const table = issuesTable(page);
    const cell = phaseCell(table);
    await expect(cell).toBeVisible();
    await expect
      .poll(() => cell.evaluate((node) => node.querySelectorAll("[data-phase-segment]").length))
      .toBe(11);

    // The mode control opens on Share.
    const modes = page.getByRole("group", { name: "Time per phase as" });
    await expect(modes.getByRole("button", { name: "Share" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(modes.getByRole("button", { name: "Hours" })).toHaveAttribute(
      "aria-pressed",
      "false"
    );

    // On hover: eleven lines, no Queue, each with its swatch, words, hours and share.
    const board = await getLegionBoard(WIDGETS);
    const pHours = hoursOf(board, P);
    const pTotal = totalOf(pHours);
    await cell.hover();
    await expect(page.getByRole("tooltip")).toBeVisible();
    await expect(page.getByRole("tooltip").locator("[data-phase-hours-line]")).toHaveCount(
      PHASES.length
    );
    // The count wait above drew all eleven lines of the tooltip that this loop walks.
    const lines = await tooltipLines(page);
    expect(lines.map((line) => line.phase)).toEqual(PHASES.map(([phase]) => phase));
    expect(lines.map((line) => line.name)).toEqual(PHASES.map(([, name]) => name));
    for (const line of lines) {
      const fill = PHASES.find(([phase]) => phase === line.phase)?.[2] ?? "";
      expect(line.swatch, `${line.phase}'s swatch`).toBe(rgb(fill));
      expect(line.hours, `${line.phase}'s hours`).toMatch(/^\d+(?:\.\d)? h$/);
      expect(line.share).toMatch(/^\d+%$/);
    }
    expect(lines.find((line) => line.phase === "other")?.hours).toBe(hoursWords(OTHER_HOURS));
    const shares = lines.reduce((sum, line) => sum + Number.parseInt(line.share, 10), 0);
    expect(Math.abs(shares - 100), `the shares add up to ${shares}%`).toBeLessThanOrEqual(
      lines.length / 2
    );
    expect(pTotal).toBeGreaterThan(0);
    await page.mouse.move(5, 5);
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    // On focus, and closed by Escape.
    await table.locator(`tr[data-issue-row="${P}"] a`).last().focus();
    for (let press = 0; press < 40; press++) {
      // A Tab press moves the focus before the press resolves, so this read sees where the
      // focus is now.
      if (await cell.evaluate((node) => node === document.activeElement)) break;
      await page.keyboard.press("Tab");
    }
    await expect(cell).toBeFocused();
    await expect(page.getByRole("tooltip")).toBeVisible();
    expect((await tooltipLines(page)).length).toBe(11);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    // No part of the cell carries a title.
    expect(
      await table
        .locator(`tr[data-issue-row="${P}"] td`)
        .filter({ has: page.locator("[data-phase-hours]") })
        .evaluate(
          (node) =>
            [...node.querySelectorAll("[title]")].length + (node.hasAttribute("title") ? 1 : 0)
        )
    ).toBe(0);

    // At 1440x900, with P's row the last whole row in the window, the tooltip opens above the
    // cell, inside the window. P stands in the middle of its page, so rows follow it.
    await page.setViewportSize({ height: 900, width: 1440 });
    const box = page.getByTestId("issues-table-scroll");
    await box.evaluate((node) => {
      node.scrollTop = 0;
      window.scrollTo(0, 0);
    });
    await expect(cell).toBeAttached();
    const placed = await box.evaluate((node, key) => {
      const rows = [...node.querySelectorAll("tr[data-issue-row]")];
      const index = rows.findIndex((one) => one.getAttribute("data-issue-row") === key);
      const row = rows[index];
      if (row === undefined) throw new Error("P's row is not drawn");
      // The box first, then the window, so the row's bottom meets the bottom of what the window
      // shows of the box.
      const visible = () => Math.min(window.innerHeight, node.getBoundingClientRect().bottom);
      // The window first, so the box ends at the window's bottom where the page lets it.
      window.scrollBy(0, node.getBoundingClientRect().bottom - window.innerHeight);
      node.scrollTop += row.getBoundingClientRect().bottom - visible();
      window.scrollBy(0, Math.max(0, row.getBoundingClientRect().bottom - window.innerHeight));
      const next = rows[index + 1];
      return {
        bottom: row.getBoundingClientRect().bottom,
        next: next === undefined ? null : next.getBoundingClientRect().bottom,
        visible: visible(),
      };
    }, P);
    expect(placed.bottom, "P's row is not whole in the window").toBeLessThanOrEqual(
      placed.visible + 0.5
    );
    expect(
      placed.next ?? Number.POSITIVE_INFINITY,
      "the row after P is whole in the window too"
    ).toBeGreaterThan(placed.visible + 0.5);
    await cell.hover();
    await expect(page.getByRole("tooltip")).toBeVisible();
    // The wait above showed the tooltip, which takes its place before it paints, so its box is
    // where it opens.
    const wide = await boxes(cell);
    expect(wide.cell.bottom, "P's row is not whole in the window").toBeLessThanOrEqual(
      wide.window.height
    );
    if (wide.tip === undefined) throw new Error("no tooltip box");
    for (const [edge, value] of [
      ["left", wide.tip.left],
      ["top", wide.tip.top],
      ["right", wide.window.width - wide.tip.right],
      ["bottom", wide.window.height - wide.tip.bottom],
    ] as const)
      expect(
        value,
        `at 1440x900 the tooltip is ${value} px from the window's ${edge}`
      ).toBeGreaterThanOrEqual(8);
    // Where the space under P's cell is less than the tooltip's height, the tooltip opens above
    // the cell.
    if (wide.window.height - wide.cell.bottom < wide.tip.height)
      expect(wide.tip.bottom, "the tooltip does not open above the cell").toBeLessThanOrEqual(
        wide.cell.top + 0.5
      );
    await page.mouse.move(5, 5);
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    // At 390x844, the table scrolled sideways to P's cell.
    await page.setViewportSize({ height: 844, width: 390 });
    await cell.scrollIntoViewIfNeeded();
    await cell.hover();
    await expect(page.getByRole("tooltip")).toBeVisible();
    // The wait above showed the tooltip, which takes its place before it paints, so its box is
    // where it opens.
    const phone = await boxes(cell);
    if (phone.tip === undefined) throw new Error("no tooltip box");
    for (const [edge, value] of [
      ["left", phone.tip.left],
      ["top", phone.tip.top],
      ["right", phone.window.width - phone.tip.right],
      ["bottom", phone.window.height - phone.tip.bottom],
    ] as const)
      expect(
        value,
        `at 390x844 the tooltip is ${value} px from the window's ${edge}`
      ).toBeGreaterThanOrEqual(8);
    await page.mouse.move(5, 5);
    await page.setViewportSize({ height: 3000, width: 1440 });

    // Each segment's fill in each scheme, with a 1 px gap between each two.
    for (const [scheme, column] of [
      ["light", 2],
      ["dark", 3],
    ] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      await expect(cell.locator("[data-phase-segment]")).toHaveCount(PHASES.length);
      // The count wait above drew P's eleven segments, and emulateMedia set the scheme's fills
      // before it resolved.
      const segments = await cell.evaluate((node) =>
        [...node.querySelectorAll("[data-phase-segment]")].map((segment) => {
          const rect = segment.getBoundingClientRect();
          return {
            phase: segment.getAttribute("data-phase-segment") ?? "",
            fill: getComputedStyle(segment).backgroundColor,
            left: rect.left,
            right: rect.right,
          };
        })
      );
      expect(segments.map((segment) => segment.phase)).toEqual(PHASES.map(([phase]) => phase));
      for (const segment of segments) {
        const fill = PHASES.find(([phase]) => phase === segment.phase)?.[column] ?? "";
        expect(segment.fill, `${scheme}: ${segment.phase}'s fill`).toBe(rgb(fill));
      }
      for (let index = 1; index < segments.length; index++) {
        const gap = (segments[index]?.left ?? 0) - (segments[index - 1]?.right ?? 0);
        expect(
          Math.abs(gap - 1),
          `${scheme}: the gap before ${segments[index]?.phase}`
        ).toBeLessThanOrEqual(0.5);
      }
    }
    await page.emulateMedia({ colorScheme: "light" });

    // Hours: on each page the longest total fills its cell, and each cell writes its total.
    await modes.getByRole("button", { name: "Hours" }).click();
    await expect(modes.getByRole("button", { name: "Hours" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    const hoursDiffer = async () => {
      const now = await getLegionBoard(WIDGETS);
      const drawn = await table.locator("tbody tr[data-issue-row]").evaluateAll((rows) =>
        rows.map((row) => {
          const button = row.querySelector("[data-phase-hours]");
          const bar = row.querySelector<HTMLElement>("[data-phase-hours-bar]");
          const track = bar?.parentElement;
          return {
            key: row.getAttribute("data-issue-row") ?? "",
            total: button === null ? null : (button.firstElementChild?.textContent ?? "").trim(),
            share:
              bar === null || bar === undefined || track === null || track === undefined
                ? null
                : bar.getBoundingClientRect().width / track.getBoundingClientRect().width,
          };
        })
      );
      const totals = drawn.map((row) => totalOf(hoursOf(now, row.key)));
      const longest = Math.max(...totals);
      return drawn.flatMap((row, index) => {
        const total = totals[index] ?? 0;
        const wanted = total > 0 ? hoursWords(total) : null;
        const wrong: string[] = [];
        if (row.total !== wanted) wrong.push(`${row.key} writes ${row.total}, its hours ${wanted}`);
        if (total > 0 && Math.abs((row.share ?? 0) - total / longest) > 0.02)
          wrong.push(`${row.key}'s bar is ${row.share} of its cell, its total ${total / longest}`);
        return wrong;
      });
    };
    await expect.poll(hoursDiffer, { message: "page 1 in Hours" }).toEqual([]);
    expect((await rowKeys(table)).length).toBe(25);
    await page.getByRole("button", { name: "Next ›" }).click();
    await expect.poll(async () => (await rowKeys(table)).length).toBe(keys.length - 25);
    await expect.poll(hoursDiffer, { message: "page 2 in Hours" }).toEqual([]);

    // After a reload the column is still in Hours.
    await page.reload();
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expect(
      page.getByRole("group", { name: "Time per phase as" }).getByRole("button", { name: "Hours" })
    ).toHaveAttribute("aria-pressed", "true");
  } finally {
    await context.close();
    await daemon.close();
  }
});
