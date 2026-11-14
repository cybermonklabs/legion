// The views demo's screenshot run (README.md beside this file): against a running demo, it opens
// each page the design shows and writes one PNG per picture, named by what it shows, then fails
// if a page never drew, the stub left any request unanswered, the page threw, or a check failed.
//
//   bun e2e/views-demo/shots.ts --base http://127.0.0.1:5190 --out <dir> \
//     [--before | --file] [--chromium <path>] [--project <KEY>] [--only <name part>,…]
//
// --before takes the pictures of the page before the design (the List, the Board and the Legion
// view of cybermonklabs/legion#2731). --file opens the single file (--base file:///….html) with
// the network off, and clicks through it: a view tab, the Board, a drag on the Board, Save view
// and its delete, the Columns menu and the Closed filter. With neither, it takes the design's
// pictures.
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { type Browser, type CDPSession, chromium, type Locator, type Page } from "@playwright/test";

const { values: flags } = parseArgs({
  options: {
    base: { type: "string", default: "http://127.0.0.1:5190" },
    out: { type: "string" },
    before: { type: "boolean", default: false },
    file: { type: "boolean", default: false },
    chromium: { type: "string" },
    project: { type: "string", default: "INFRA" },
    // A real project with no Legion daemon in the fixtures (fixtures.ts --no-daemon-project).
    "no-daemon-project": { type: "string", default: "GTM" },
    only: { type: "string" },
  },
});
if (flags.out === undefined) throw new Error("name --out <dir>");
const out = flags.out;
const base = flags.base;
const project = flags.project;
const issuesPath = `/projects/${project}/issues`;
const mode = flags.before ? "before" : flags.file ? "file" : "after";

const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };
/** The page's time zone and language, the same on every machine, so a time of day and a date
 *  read the same in every run (the README runs it with TZ=UTC as well). */
const TIME_ZONE = "UTC";
const LOCALE = "en-US";

/** One picture: its file name, the window it is taken in, and how to reach what it shows. */
interface Shot {
  name: string;
  viewport: { width: number; height: number };
  take: (page: Page) => Promise<void>;
}

interface Report {
  /** What the pictures were drawn with: the browser, its time zone and language, and every
   *  font the browser drew the pictures' text in, with how many glyphs. */
  browser: { version: string; time_zone: string; locale: string };
  fonts: Record<string, number>;
  shots: string[];
  unanswered: string[];
  errors: string[];
  checks: string[];
}

const report: Report = {
  browser: { version: "", time_zone: TIME_ZONE, locale: LOCALE },
  fonts: {},
  shots: [],
  unanswered: [],
  errors: [],
  checks: [],
};

/** The address of `path`: the demo's own, or the single file's fragment, which its router reads
 *  (web/src/demo/file-router.ts). */
function address(path: string): string {
  return base.endsWith(".html") ? `${base}#${path}` : `${base}${path}`;
}

/** What a page of issues draws once its rows are in: a table row, a Board card or, before the
 *  design, a Legion view card. */
const ROWS = "[data-issue-row], [data-board-card], [data-legion-card]";

/** An Issues page, by the dev server's address or the single file's fragment. */
const ISSUES_PAGE = /\/projects\/[^/?#]+\/issues(?:[?#]|$)/;

/** Opens `path` and waits until the page has drawn what `ready` names, its rows by default. */
async function open(page: Page, path: string, ready?: Locator): Promise<void> {
  await page.goto(address(path));
  await settle(page, ready);
}

/** Waits for `ready`, the page's rows by default, and then for the drawing to settle. A page
 *  that never draws them fails its picture rather than leaving a loading state in it. The
 *  design's Issues page draws its view tabs first, which the page before the design has none
 *  of: a run of the design's pictures against that page fails at its first picture. */
async function settle(page: Page, ready?: Locator): Promise<void> {
  await page.waitForLoadState("networkidle").catch(() => undefined);
  if (mode !== "before" && ISSUES_PAGE.test(page.url())) {
    await page.locator("[data-view-tab]").first().waitFor({ timeout: 30_000 });
  }
  await (ready ?? page.locator(ROWS).first()).waitFor({ timeout: 30_000 });
  await page.waitForTimeout(1_200);
}

/** The fonts the browser drew the page's text in, from its own record of each text node
 *  (`CSS.getPlatformFontsForNode`), over the elements that hold the page's words. */
const FONT_SAMPLE =
  "h1, h2, [data-view-tab], thead th, [data-issue-row] td, [data-board-card], button, input, [data-testid='views-demo-banner'] div, [data-legion-settings] td";
async function recordFonts(cdp: CDPSession): Promise<void> {
  const { root } = await cdp.send("DOM.getDocument", { depth: 0 });
  const { nodeIds } = await cdp.send("DOM.querySelectorAll", {
    nodeId: root.nodeId,
    selector: FONT_SAMPLE,
  });
  for (const nodeId of nodeIds.slice(0, 80)) {
    const { fonts } = await cdp.send("CSS.getPlatformFontsForNode", { nodeId });
    for (const font of fonts) {
      report.fonts[font.familyName] = (report.fonts[font.familyName] ?? 0) + font.glyphCount;
    }
  }
}

function check(passed: boolean, what: string): void {
  if (passed) report.checks.push(`ok: ${what}`);
  else report.errors.push(`check failed: ${what}`);
}

const view = (id: string, extra = "", key = project) =>
  `/projects/${key}/issues?view=${id}${extra}`;
const tab = (page: Page, id: string) => page.locator(`[data-view-tab="${id}"]`);

/** Opens In flight narrowed to one stage, so the view is changed, and opens Save view with a
 *  name typed. */
async function openSaveView(page: Page, name: string): Promise<void> {
  await open(page, view("in-flight", "&f.stage=implementing"));
  await page.getByRole("button", { name: "Save view" }).click();
  await page.getByRole("textbox", { name: "Name" }).fill(name);
}

const beforeShots: Shot[] = [
  { name: "before-list-1440", viewport: DESKTOP, take: (page) => open(page, issuesPath) },
  {
    name: "before-board-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, issuesPath);
      await page.getByRole("button", { name: "Board", exact: true }).click();
      await settle(page, page.locator("[data-board-card]").first());
    },
  },
  {
    name: "before-legion-view-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, issuesPath);
      await page.getByRole("button", { name: "Legion", exact: true }).click();
      await settle(page, page.locator("[data-legion-card]").first());
    },
  },
  { name: "before-list-390", viewport: PHONE, take: (page) => open(page, issuesPath) },
];

const VIEWS = ["open", "triage", "waiting", "in-flight", "slots", "finished", "mine"];

const afterShots: Shot[] = [
  ...VIEWS.map((id) => ({
    name: `view-${id}-1440`,
    viewport: DESKTOP,
    take: (page: Page) => open(page, view(id)),
  })),
  {
    name: "board-in-flight-1440",
    viewport: DESKTOP,
    take: (page) => open(page, view("in-flight", "&layout=board")),
  },
  {
    name: "board-all-open-1440",
    viewport: DESKTOP,
    take: (page) => open(page, view("open", "&layout=board")),
  },
  {
    name: "finished-time-per-phase-hours-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("finished"));
      await page
        .getByRole("group", { name: "Time per phase as" })
        .getByRole("button", { name: "Hours" })
        .click();
      await page.waitForTimeout(500);
    },
  },
  {
    name: "save-view-popover-1440",
    viewport: DESKTOP,
    take: (page) => openSaveView(page, "Implementing"),
  },
  {
    name: "save-view-saved-tab-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await openSaveView(page, "Implementing");
      await page.getByRole("button", { name: "Save", exact: true }).click();
      await settle(page, page.locator('[data-saved-view-tab="Implementing"]'));
    },
  },
  {
    name: "columns-menu-legion-columns-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("open"));
      await page.getByRole("button", { name: /^Columns/ }).click();
      await page.waitForTimeout(500);
    },
  },
  {
    name: "closed-date-range-filter-open-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("finished"));
      await page.getByRole("button", { name: /^Filter Closed/ }).click();
      await page.waitForTimeout(500);
    },
  },
  {
    name: "project-settings-legion-panel-1440",
    viewport: DESKTOP,
    // The panel draws once the board answers; a settings row is what it waits for.
    take: (page) =>
      open(
        page,
        `/projects/${project}/settings`,
        page.locator("[data-legion-settings] tbody tr").first()
      ),
  },
  { name: "view-open-390", viewport: PHONE, take: (page) => open(page, view("open")) },
  { name: "view-in-flight-390", viewport: PHONE, take: (page) => open(page, view("in-flight")) },
  {
    name: "board-in-flight-390",
    viewport: PHONE,
    take: (page) => open(page, view("in-flight", "&layout=board")),
  },
  {
    name: "view-open-scrolled-to-right-end-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("open"));
      // The columns past the window's edge: the box the rows scroll in, at its right end.
      await page
        .getByTestId("issues-table-scroll")
        .evaluate((box) => box.scrollTo({ left: box.scrollWidth }));
      await page.waitForTimeout(500);
    },
  },
  {
    name: "board-all-open-390",
    viewport: PHONE,
    take: (page) => open(page, view("open", "&layout=board")),
  },
  {
    name: "finished-time-per-phase-hours-390",
    viewport: PHONE,
    take: async (page) => {
      await open(page, view("finished"));
      await page
        .getByRole("group", { name: "Time per phase as" })
        .getByRole("button", { name: "Hours" })
        .click();
      // Swipe the column in as a reader would: its left edge just after the pinned Key.
      await page.getByTestId("issues-table-scroll").evaluate((box) => {
        const headers = [...box.querySelectorAll("th")];
        const phase = headers.find((th) => th.textContent?.includes("Time per phase"));
        const key = headers.find((th) => th.textContent?.trim().startsWith("Key"));
        if (phase === undefined || key === undefined) return;
        box.scrollLeft += phase.getBoundingClientRect().left - key.getBoundingClientRect().right;
      });
      await page.waitForTimeout(500);
    },
  },
  {
    name: "view-open-info-hint-open-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("open"));
      await page.getByRole("button", { name: "About this view" }).click();
      await page.waitForTimeout(500);
    },
  },
  // The views of a project with no Legion daemon: no Waiting line or Slots tab, no board-fed
  // column, and In flight of the statuses Legion does not run.
  {
    name: "no-daemon-view-open-1440",
    viewport: DESKTOP,
    take: (page) => open(page, view("open", "", flags["no-daemon-project"])),
  },
  {
    name: "no-daemon-view-in-flight-1440",
    viewport: DESKTOP,
    take: (page) => open(page, view("in-flight", "", flags["no-daemon-project"])),
  },
  {
    name: "no-daemon-board-all-open-1440",
    viewport: DESKTOP,
    take: (page) => open(page, view("open", "&layout=board", flags["no-daemon-project"])),
  },
  // The daemon's newest read failed (the stub's demo=read-failed flag): the alert beside the
  // tabs, and the cards of the read before it.
  {
    name: "view-open-daemon-read-failed-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("open", "&demo=read-failed"));
      await page.locator("[data-legion-read-error]").waitFor({ timeout: 30_000 });
      check(!page.url().includes("demo="), "the read-failed flag leaves the address");
    },
  },
];

/** The single file, clicked through with the network off. */
const fileShots: Shot[] = [
  {
    name: "file-opens-on-all-open-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await page.goto(base);
      await settle(page);
      check(page.url().includes(`#${issuesPath}`), "the file opens on the Issues tab");
      check((await tab(page, "open").getAttribute("aria-selected")) === "true", "All open is on");
    },
  },
  {
    name: "file-switched-to-in-flight-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, issuesPath);
      await tab(page, "in-flight").click();
      await settle(page);
      check(
        (await tab(page, "in-flight").getAttribute("aria-selected")) === "true",
        "a press on the In flight tab opens In flight"
      );
    },
  },
  {
    name: "file-in-flight-board-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("in-flight"));
      await page.getByRole("button", { name: "Board", exact: true }).click();
      await settle(page, page.locator("[data-board-card]").first());
      check((await page.locator("[data-board-card]").count()) > 0, "the Board layout draws cards");
    },
  },
  {
    name: "file-board-drag-todo-to-backlog-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("open", "&layout=board"), page.locator("[data-board-card]").first());
      const card = page.locator('[data-board-column="todo"] [data-board-card]').first();
      const key = await card.getAttribute("data-board-card");
      const target = page.locator('[data-board-column="backlog"]').first();
      await drag(page, card, target);
      await page.waitForTimeout(1_500);
      const status = await page.evaluate(async (issue) => {
        const answer: unknown = await (await fetch(`/api/v1/issues/${issue}`)).json();
        return typeof answer === "object" && answer !== null && "status" in answer
          ? answer.status
          : undefined;
      }, key);
      check(status === "backlog", `a drag of ${key} from Todo onto Backlog sets its status`);
      check(page.url().includes(issuesPath), "the drop opens no issue: the Board stays");
      check(
        (await page.locator(`[data-board-column="todo"] [data-board-card="${key}"]`).count()) === 0,
        `${key} has left the Todo column`
      );
    },
  },
  {
    name: "file-save-view-saved-tab-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await openSaveView(page, "Implementing");
      await page.getByRole("button", { name: "Save", exact: true }).click();
      const saved = page.locator('[data-saved-view-tab="Implementing"]');
      await settle(page, saved);
      check((await saved.count()) === 1, "Save view adds the saved tab");
    },
  },
  {
    name: "file-saved-view-deleted-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await openSaveView(page, "Implementing");
      await page.getByRole("button", { name: "Save", exact: true }).click();
      const saved = page.locator('[data-saved-view-tab="Implementing"]');
      await settle(page, saved);
      await page.getByRole("button", { name: "Delete saved view Implementing" }).click();
      await page.waitForTimeout(500);
      check((await saved.count()) === 0, "Delete takes the saved tab away");
      await page.reload();
      await settle(page);
      check((await saved.count()) === 0, "the deleted view stays gone after a reload");
    },
  },
  {
    name: "file-columns-menu-shows-work-time-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("open"));
      const header = page.locator("thead th", { hasText: "Work time" });
      check((await header.count()) === 0, "All open shows no Work time column at first");
      await page.getByRole("button", { name: /^Columns/ }).click();
      await page.getByRole("checkbox", { name: "Work time" }).check();
      await page.keyboard.press("Escape");
      await page.waitForTimeout(500);
      check((await header.count()) === 1, "ticking Work time in the Columns menu shows the column");
    },
  },
  {
    name: "file-closed-filter-7-days-1440",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, view("finished"));
      const listed = async () => {
        const text = await page.getByText(/^\d+ of \d+ issues$/).textContent();
        return Number(/^(\d+)/.exec(text ?? "")?.[1] ?? Number.NaN);
      };
      const before = await listed();
      await page.getByRole("button", { name: /^Filter Closed/ }).click();
      await page.getByRole("button", { name: "7 days", exact: true }).click();
      // Off the rows, so no issue's hover preview stands in the picture.
      await page.mouse.move(1, 1);
      await settle(page);
      const after = await listed();
      // The rows the view should list: the done issues closed in the last 7 times 24 hours of
      // the page's own clock that have Legion work, from the stub's own two routes.
      const expected = await page.evaluate(async (key) => {
        const since = new Date(Date.now() - 7 * 86_400_000).toISOString();
        const rows: { key: string }[] = await (
          await fetch(`/api/v1/issues?project=${key}&status=done&closed_since=${since}`)
        ).json();
        const board: { issues: Record<string, unknown> } = await (
          await fetch(`/api/v1/projects/${key}/legion/board`)
        ).json();
        return rows.filter((row) => Object.hasOwn(board.issues, row.key)).length;
      }, project);
      check(page.url().includes("f.closed=7d"), "the 7 days preset writes f.closed=7d");
      check(
        after === expected && after > 0 && after < before,
        `Closed in the last 7 days lists ${after} rows of the 28 days' ${before}; the stub's rows say ${expected}`
      );
    },
  },
];

/** A drag with the pointer, in steps, as a person makes one: dnd-kit starts a drag only once the
 *  pointer has moved past its activation distance. The card is taken by its lower right corner
 *  and let go just under the column's title, both away from any link, so the release opens no
 *  issue. */
async function drag(page: Page, from: Locator, to: Locator): Promise<void> {
  const start = await from.boundingBox();
  const end = await to.boundingBox();
  if (start === null || end === null) throw new Error("the card or the column is not drawn");
  const x = start.x + start.width - 12;
  const y = start.y + start.height - 8;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x - 10, y - 10, { steps: 5 });
  await page.mouse.move(end.x + end.width / 2, end.y + 70, { steps: 25 });
  await page.waitForTimeout(200);
  await page.mouse.up();
  // Off the cards, so no issue's hover preview stands in the picture.
  await page.mouse.move(1, 1);
}

async function takeAll(browser: Browser, shots: Shot[]): Promise<void> {
  for (const shot of shots) {
    const wanted = flags.only?.split(",");
    if (wanted !== undefined && !wanted.some((part) => shot.name.includes(part))) continue;
    // A fresh context for each picture: nothing stored, as a first visit; and for the single
    // file no network at all, every request outside the page refused and recorded.
    const context = await browser.newContext({
      viewport: shot.viewport,
      offline: flags.file,
      timezoneId: TIME_ZONE,
      locale: LOCALE,
    });
    if (flags.file) {
      await context.route(/^(?!file:|data:|blob:)/, (route) => {
        report.errors.push(`${shot.name}: a request left the file: ${route.request().url()}`);
        return route.abort();
      });
    }
    const page = await context.newPage();
    const cdp = await context.newCDPSession(page);
    await cdp.send("DOM.enable");
    await cdp.send("CSS.enable");
    page.on("pageerror", (error) => report.errors.push(`${shot.name}: ${error.message}`));
    page.on("console", (message) => {
      if (message.type() === "error") report.errors.push(`${shot.name}: ${message.text()}`);
    });
    try {
      await shot.take(page);
      if (shot.viewport.width < 640) {
        // The phone's banner is its short form, which holds two lines at most.
        const lines = await page
          .locator('[data-testid="views-demo-banner"] .short')
          .evaluate((short) => {
            const height = Number.parseFloat(getComputedStyle(short).lineHeight);
            return Math.round(short.getBoundingClientRect().height / height);
          });
        check(lines <= 2, `${shot.name}: the phone banner takes ${lines} lines`);
      }
      await recordFonts(cdp);
      const file = join(out, `${shot.name}.png`);
      await page.screenshot({ path: file });
      report.shots.push(file);
      console.log(`wrote ${file}`);
    } catch (error) {
      report.errors.push(`${shot.name}: ${error instanceof Error ? error.message : String(error)}`);
      await page.screenshot({ path: join(out, `${shot.name}.failed.png`) }).catch(() => undefined);
    }
    const missed = await page
      .evaluate(() => window.__dispatchDemoUnanswered ?? [])
      .catch(() => [] as string[]);
    for (const entry of missed) report.unanswered.push(`${shot.name}: ${entry}`);
    await context.close();
  }
}

await mkdir(out, { recursive: true });
if (process.env.TZ !== TIME_ZONE) console.warn(`run with TZ=${TIME_ZONE}, as the README says`);
const browser = await chromium.launch({ executablePath: flags.chromium });
report.browser.version = `${browser.browserType().name()} ${browser.version()}`;
try {
  await takeAll(browser, { before: beforeShots, file: fileShots, after: afterShots }[mode]);
} finally {
  await browser.close();
}
await writeFile(join(out, `report-${mode}.json`), JSON.stringify(report, null, 2));
console.log(
  `${report.shots.length} pictures, ${report.checks.length} checks passed, ${report.unanswered.length} unanswered requests, ${report.errors.length} errors`
);
for (const line of [...report.checks, ...report.unanswered, ...report.errors]) {
  console.log(`  ${line}`);
}
if (report.unanswered.length > 0 || report.errors.length > 0) process.exit(1);
