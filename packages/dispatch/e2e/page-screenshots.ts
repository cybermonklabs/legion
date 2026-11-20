/** `bun e2e/page-screenshots.ts`: a light and a dark screenshot of each named scene of a page
 *  change, for the person who approves the page's design to see before they do.
 *
 *  Run it from packages/dispatch against a running `bun run dispatch:local`, prefixed with the
 *  variables that command prints on its "Run a spec against it" line (its `DATABASE_URL` and every
 *  port), which the e2e modules imported below read:
 *
 *    <those variables> bun e2e/page-screenshots.ts --issue <KEY> --out <dir> <scene> [<scene>...]
 *
 *  For each scene named it writes `<dir>/<scene>-light.png` and `<dir>/<scene>-dark.png`, then
 *  prints one `dispatch artifact` line per file, attaching it to the issue `--issue` names. It runs
 *  none of them: the person who read the images attaches them.
 *
 *  Each scene empties the instance and seeds its own state, as a row of the browser suite starts
 *  from `resetDatabase` (e2e/test.ts), so restart `dispatch:local` to have its own seed back. A
 *  request's scene seeds with the seed of the e2e test that proves its state, imported from the
 *  helper module that test imports it from, so the screenshot shows the state the test asserts.
 *  `page-inbox-today`, the Inbox before those requests, seeds with the helpers the one-list row of
 *  e2e/one-inbox.e2e.ts writes its rows with.
 *
 *  It is no test: every Playwright project matches `*.e2e.ts` files alone, and `bun run test`
 *  collects only test-named files. */
import { mkdirSync } from "node:fs";
import { basename, join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, expect as baseExpect, chromium, type Page } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { baseUrl, createAsk, createIssue, createProject, patchIssue } from "./api";
import { inboxList, inboxPane, inboxRow } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { shellQuote } from "./local-instance-support";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** Playwright's assertions with the e2e suite's 15 s wait (e2e/playwright.config.ts). */
const expect = baseExpect.configure({ timeout: 15_000 });

/** One page change in the state to show. */
interface Scene<Seeded> {
  /** The stem of its two files, `<name>-light.png` and `<name>-dark.png`. */
  readonly name: string;
  /** Writes the state into the emptied instance: the seed of the e2e test that proves it. */
  seed(): Promise<Seeded>;
  /** The address to open, relative to the instance's origin. */
  path(seeded: Seeded): string;
  /** Puts the opened page in the state to show, and ends on a wait for that state. */
  act(page: Page, seeded: Seeded): Promise<void>;
}

/** The two schemes each scene is taken in, switched on one page as e2e/visual.e2e.ts does. */
const THEMES = ["light", "dark"] as const;

/** The `visual` project's viewport (e2e/playwright.config.ts), which the Inbox draws its reading
 *  column at. */
const VIEWPORT = { height: 800, width: 1280 };

/** The Inbox as it stands, with the one read's opt-in on (`?one=1`, as e2e/one-inbox.e2e.ts opens
 *  it): an open ask waiting on the reader, a comment thread of theirs a session answered (a Reply
 *  row), and a session's reply to a message they sent it (an Agent row). */
const inboxToday: Scene<{ readonly rows: readonly string[] }> = {
  name: "page-inbox-today",
  async seed() {
    await setLiveSessions([THREAD_SESSION]);
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Release checklist" });
    await patchIssue(issue.key, { assignee: "alice" });
    const ask = await createAsk(
      issue.key,
      { question: "Approve the release window?" },
      asSession()
    );
    const thread = await seedCommentThread(
      issue.key,
      "Is the checklist complete?",
      "Two items are still open."
    );
    const agent = await seedAgentThread({
      body: "Please check the staging gate.",
      replies: ["The staging gate is open."],
    });
    return { rows: [ask.id, replyRowId(thread), agentRowId(agent)] };
  },
  path: () => "/?one=1",
  async act(page, { rows }) {
    await expect(page.getByRole("heading", { exact: true, level: 1, name: "Inbox" })).toBeVisible();
    await expect(inboxList(page).locator("[data-inbox-row]")).toHaveCount(rows.length);
    for (const row of rows) await expect(inboxRow(page, row)).toBeVisible();
    await expect(inboxPane(page)).toBeVisible();
    await expect(page.locator("[data-markdown-pending]")).toHaveCount(0);
  },
};

const scenes: readonly Scene<unknown>[] = [inboxToday];

/** Seeds `scene` into the emptied instance, opens it as alice, and writes its two files into
 *  `out`, returning their paths. */
async function shoot(browser: Browser, scene: Scene<unknown>, out: string): Promise<string[]> {
  await resetDatabase();
  const seeded = await scene.seed();
  const alice = await asUser(browser, "alice", { baseURL: baseUrl, viewport: VIEWPORT });
  try {
    const page = await alice.newPage();
    await page.goto(scene.path(seeded));
    await scene.act(page, seeded);
    const files: string[] = [];
    for (const colorScheme of THEMES) {
      await page.emulateMedia({ colorScheme });
      const file = join(out, `${scene.name}-${colorScheme}.png`);
      // `animations: "disabled"` runs the 150 ms colour ease the scheme switch starts
      // (web/src/styles.css) to its end, so no file is taken mid-ease.
      await page.screenshot({ animations: "disabled", caret: "hide", fullPage: true, path: file });
      files.push(file);
    }
    return files;
  } finally {
    await alice.close();
  }
}

const usage =
  "usage: bun e2e/page-screenshots.ts --issue <KEY> --out <dir> <scene> [<scene>...]\n" +
  `scenes: ${scenes.map((scene) => scene.name).join(", ")}`;

const { positionals, values } = parseArgs({
  allowPositionals: true,
  options: { issue: { type: "string" }, out: { type: "string" } },
  strict: true,
});
const named = positionals.map((name) => {
  const scene = scenes.find((candidate) => candidate.name === name);
  if (scene === undefined) {
    console.error(`No scene is named ${name}.\n${usage}`);
    process.exit(2);
  }
  return scene;
});
if (values.issue === undefined || values.out === undefined || named.length === 0) {
  console.error(usage);
  process.exit(2);
}
const { issue } = values;
const out = resolve(values.out);
mkdirSync(out, { recursive: true });

const browser = await chromium.launch();
const files: string[] = [];
try {
  for (const scene of named) files.push(...(await shoot(browser, scene, out)));
} catch (error) {
  console.error(
    `page-screenshots failed: ${error instanceof Error ? error.message : String(error)}`
  );
  process.exitCode = 1;
} finally {
  await browser.close();
}
for (const file of files) {
  console.log(
    `dispatch artifact --issue ${shellQuote(issue)} --name ${shellQuote(basename(file))} ` +
      `--path ${shellQuote(file)}`
  );
}
process.exit();
