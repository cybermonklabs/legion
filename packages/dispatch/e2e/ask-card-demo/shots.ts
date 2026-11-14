// The ask card demo's screenshot run (README.md beside this file): against a running demo, it
// opens each state the design shows and writes one PNG per picture, named `ask-card-NN-<what>`,
// then fails if a picture never reached its state, the stub left a request unanswered, the page
// threw, or a check failed.
//
//   bun e2e/ask-card-demo/shots.ts --base http://127.0.0.1:5191 --out <dir> \
//     [--file] [--chromium <path>] [--only <name part>,…]
//
// --file opens the single file (--base file:///….html) with the network off and clicks through
// it: the Inbox, the menu, a rewrite, Define, and an issue page by its fragment address.
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { type Browser, chromium, type Page } from "@playwright/test";

const { values: flags } = parseArgs({
  options: {
    base: { type: "string", default: "http://127.0.0.1:5191" },
    out: { type: "string" },
    file: { type: "boolean", default: false },
    chromium: { type: "string" },
    only: { type: "string" },
  },
});
if (flags.out === undefined) throw new Error("name --out <dir>");
const out = flags.out;
const base = flags.base;

const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };

/** The asks the pictures turn on (`web/src/demo/fixtures.ts`). */
const FLIP = "a5000000-0000-4000-8000-000000000005";
const CHECKED = "a2000000-0000-4000-8000-000000000002";
const ORDER = "a3000000-0000-4000-8000-000000000003";
const APPROVAL = "a1000000-0000-4000-8000-000000000001";

interface Shot {
  name: string;
  viewport: { width: number; height: number };
  take: (page: Page) => Promise<void>;
}

interface Report {
  shots: string[];
  unanswered: string[];
  errors: string[];
  checks: string[];
}

const report: Report = { shots: [], unanswered: [], errors: [], checks: [] };

/** The address of `path`: the demo's own, or the single file's fragment, which its router reads
 *  (web/src/demo/file-router.ts). */
function address(path: string): string {
  return base.endsWith(".html") ? `${base}#${path}` : `${base}${path}`;
}

const card = (id: string) => `[data-ask-card="${id}"]`;

/** Opens `path` and waits until `ready` is drawn, then for the drawing to settle. */
async function open(page: Page, path: string, ready: string): Promise<void> {
  await page.goto(address(path));
  await page.waitForLoadState("networkidle").catch(() => undefined);
  await page.locator(ready).first().waitFor({ timeout: 30_000 });
  await page.waitForTimeout(900);
}

/** The Inbox with one question's card in the reading pane. */
const inbox = (page: Page, id: string) =>
  open(page, `/?item=${id}`, `${card(id)} [data-ask-question]`);

function check(passed: boolean, what: string): void {
  if (passed) report.checks.push(`ok: ${what}`);
  else report.errors.push(`check failed: ${what}`);
}

/** Selects `words` inside the first element `root` names, as a reader's drag would. */
async function select(page: Page, root: string, words: string): Promise<void> {
  await page.evaluate(
    ([selector, wanted]) => {
      const element = document.querySelector(selector);
      if (element === null) throw new Error(`no ${selector}`);
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const at = node.textContent?.indexOf(wanted) ?? -1;
        if (at < 0) continue;
        const range = document.createRange();
        range.setStart(node, at);
        range.setEnd(node, at + wanted.length);
        const selection = window.getSelection();
        selection?.removeAllRanges();
        selection?.addRange(range);
        return;
      }
      throw new Error(`no "${wanted}" under ${selector}`);
    },
    [root, words] as const
  );
  await page.waitForTimeout(600);
}

/** Presses Rewrite on the card in the reading pane and waits for the badge to say `state`. */
async function rewrite(page: Page, id: string, state: "done" | "failed"): Promise<void> {
  await page.keyboard.press("r");
  await page.locator(`${card(id)} [data-ask-rewrite="running"]`).waitFor({ timeout: 5_000 });
  await page.locator(`${card(id)} [data-ask-rewrite="${state}"]`).waitFor({ timeout: 10_000 });
  await page.waitForTimeout(400);
}

/** The menu of the split button, open. */
async function openMenu(page: Page, id: string): Promise<void> {
  await page.locator(`${card(id)} [data-ask-menu-button]`).click();
  await page.locator("[data-ask-menu]").waitFor();
  await page.waitForTimeout(300);
}

/** Off any control, so no hover state or tooltip stands in the picture. */
const rest = (page: Page) => page.mouse.move(1, 1);

const shots: Shot[] = [
  {
    name: "ask-card-01-closed-inbox",
    viewport: DESKTOP,
    take: (page) => inbox(page, FLIP),
  },
  {
    name: "ask-card-02-menu-open",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await page
        .locator(`${card(FLIP)} [data-ask-option]`)
        .first()
        .check();
      await openMenu(page, FLIP);
      await rest(page);
    },
  },
  {
    name: "ask-card-03-rewriting",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await page.keyboard.press("r");
      await page.locator(`${card(FLIP)} [data-ask-rewrite="running"]`).waitFor();
      await page.waitForTimeout(300);
    },
  },
  {
    name: "ask-card-04-rewritten",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await rewrite(page, FLIP, "done");
    },
  },
  {
    name: "ask-card-05-original-shown",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await rewrite(page, FLIP, "done");
      await page.keyboard.press("w");
      await page.locator(`${card(FLIP)} [data-ask-rewrite="original"]`).waitFor();
      await page.waitForTimeout(300);
      const question = await page.locator(`${card(FLIP)} [data-ask-question]`).textContent();
      check(
        question?.startsWith("R3 flip audit") === true,
        "w shows the original wording in place"
      );
    },
  },
  {
    name: "ask-card-06-rewrite-failed",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, ORDER);
      await rewrite(page, ORDER, "failed");
    },
  },
  {
    name: "ask-card-07-reviewer-rewritten-inbox",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, CHECKED);
      check(
        (await page.locator(`${card(CHECKED)} [data-ask-rewrite="done"]`).count()) === 1,
        "the ask the check rewrote wears the Rewritten badge"
      );
    },
  },
  {
    name: "ask-card-08-define-ask-from-record",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await select(page, `${card(FLIP)} [data-ask-question]`, "flip audit");
      await page.keyboard.press("d");
      await page.locator('[data-define-agent="asking"]').waitFor();
      await page.waitForTimeout(200);
    },
  },
  {
    name: "ask-card-09-define-ask-with-agent",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await select(page, `${card(FLIP)} [data-ask-question]`, "flip audit");
      await page.keyboard.press("d");
      await page.locator('[data-define-agent="answered"]').waitFor({ timeout: 10_000 });
      await page.waitForTimeout(200);
    },
  },
  {
    name: "ask-card-10-define-document-read-view",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, `/?item=${APPROVAL}`, '[data-testid="inbox-pane"] .ProseMirror');
      await page.waitForTimeout(1_500);
      await select(page, '[data-testid="inbox-pane"] .ProseMirror', "refused lease");
      await page.keyboard.press("d");
      await page.locator("[data-define-popover] p").first().waitFor();
      await page.waitForTimeout(300);
    },
  },
  {
    name: "ask-card-11-define-document-edit-bar",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, "/issues/PANE-5/spec", ".ProseMirror");
      await page.waitForTimeout(1_500);
      await page.locator(".ProseMirror").first().focus();
      await select(page, ".ProseMirror", "Flip audit");
      await page.locator('.dispatch-action-bar button:has-text("Define")').first().waitFor();
    },
  },
  {
    name: "ask-card-12-define-document-edit-view",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, "/issues/PANE-5/spec", ".ProseMirror");
      await page.waitForTimeout(1_500);
      await page.locator(".ProseMirror").first().focus();
      await select(page, ".ProseMirror", "Flip audit");
      // The editor's own key for Define: Ctrl+Alt+Y, Cmd+Alt+Y on a Mac.
      await page.keyboard.press("Control+Alt+y");
      await page.locator("[data-define-popover] p").first().waitFor();
      await page.waitForTimeout(300);
    },
  },
  {
    name: "ask-card-13-shortcut-help",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await rewrite(page, FLIP, "done");
      await select(page, `${card(FLIP)} [data-ask-question]`, "ledger");
      // The row in hand, as a reader who walked the list with j and k stands.
      await page.locator(`[data-inbox-row="${FLIP}"]`).focus();
      await page.keyboard.press("Shift+Slash");
      await page.getByRole("dialog", { name: "Keyboard shortcuts" }).waitFor();
      await page.waitForTimeout(300);
    },
  },
  {
    name: "ask-card-14-phone-card",
    viewport: PHONE,
    take: async (page) => {
      await open(page, "/", `${card(FLIP)} [data-ask-question]`);
      await page
        .locator(card(FLIP))
        .evaluate((element) => element.scrollIntoView({ block: "start" }));
      await page.evaluate(() => window.scrollBy(0, -64));
      await page.waitForTimeout(300);
    },
  },
  {
    name: "ask-card-15-phone-menu",
    viewport: PHONE,
    take: async (page) => {
      await open(page, "/", `${card(FLIP)} [data-ask-question]`);
      await page
        .locator(card(FLIP))
        .evaluate((element) => element.scrollIntoView({ block: "start" }));
      await page.evaluate(() => window.scrollBy(0, -64));
      await page.waitForTimeout(300);
      await openMenu(page, FLIP);
    },
  },
  {
    name: "ask-card-16-approval-menu",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, "/issues/PANE-1/conversation", `main ${card(APPROVAL)}`);
      await page
        .locator(`main ${card(APPROVAL)}`)
        .evaluate((element) => element.scrollIntoView({ block: "start" }));
      await page.waitForTimeout(300);
      await page.locator(`main ${card(APPROVAL)} [data-ask-menu-button]`).click();
      await page.locator("[data-ask-menu]").waitFor();
      // The chevron turns as the menu opens; the picture waits for it to stand pointing up.
      await page.waitForTimeout(400);
      const items = await page
        .locator("[data-ask-menu-item]")
        .evaluateAll((all) => all.map((item) => item.getAttribute("data-ask-menu-item")));
      check(
        items.join(",") === "ask-back,hand-on,define",
        `the approval menu holds Ask back, Hand this question to and Define (${items.join(",")})`
      );
      await rest(page);
    },
  },
];

/** The single file, clicked through with the network off. */
const fileShots: Shot[] = [
  {
    name: "file-inbox-menu",
    viewport: DESKTOP,
    take: async (page) => {
      await page.goto(base);
      await page
        .locator("[data-ask-card] [data-ask-question]")
        .first()
        .waitFor({ timeout: 30_000 });
      check(page.url().includes("#/"), "the file opens on the Inbox");
      await inbox(page, FLIP);
      await openMenu(page, FLIP);
      check(
        (await page.locator("[data-ask-menu-item]").count()) === 5,
        "the menu lists five actions"
      );
    },
  },
  {
    name: "file-rewrite-and-define",
    viewport: DESKTOP,
    take: async (page) => {
      await inbox(page, FLIP);
      await rewrite(page, FLIP, "done");
      check(true, "a rewrite runs and lands from the file");
      await select(page, `${card(FLIP)} [data-ask-question]`, "ledger");
      await page.keyboard.press("d");
      await page.locator("[data-define-popover] p").first().waitFor();
      check(true, "Define answers from the file");
    },
  },
  {
    name: "file-issue-spec",
    viewport: DESKTOP,
    take: async (page) => {
      await open(page, "/issues/PANE-5/spec", ".ProseMirror");
      check(true, "an issue's spec opens by its fragment address");
    },
  },
];

async function takeAll(browser: Browser, list: Shot[]): Promise<void> {
  for (const shot of list) {
    const wanted = flags.only?.split(",");
    if (wanted !== undefined && !wanted.some((part) => shot.name.includes(part))) continue;
    // A fresh context for each picture: nothing stored, as a first visit; and for the single
    // file no network at all, every request outside the page refused and recorded.
    const context = await browser.newContext({ viewport: shot.viewport, offline: flags.file });
    if (flags.file) {
      await context.route(/^(?!file:|data:|blob:)/, (route) => {
        report.errors.push(`${shot.name}: a request left the file: ${route.request().url()}`);
        return route.abort();
      });
    }
    const page = await context.newPage();
    page.on("pageerror", (error) => report.errors.push(`${shot.name}: ${error.message}`));
    page.on("console", (message) => {
      if (message.type() === "error") report.errors.push(`${shot.name}: ${message.text()}`);
    });
    try {
      await shot.take(page);
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

const mode = flags.file ? "file" : "design";
await mkdir(out, { recursive: true });
const browser = await chromium.launch({ executablePath: flags.chromium });
try {
  await takeAll(browser, flags.file ? fileShots : shots);
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
