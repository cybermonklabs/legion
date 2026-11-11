import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";
import {
  type Browser,
  type BrowserContext,
  chromium,
  errors,
  type Locator,
  type Page,
} from "@playwright/test";

import { delayLegionBoard } from "../e2e/legion-board-delay";
import * as themeClasses from "../web/src/theme/classes";
import {
  BUILT_IN_VIEW_IDS,
  type BuiltInViewID,
  type ExpectedBoard,
  type ExpectedCard,
  type ExpectedIssue,
  type ExpectedViewCountsInput,
  expectedStageCount,
  expectedStageOf,
  expectedViewCounts,
} from "./issue-views-check/expect";

type Verdict = "PASS" | "FAIL" | "NOT-YET";

interface CheckResult {
  readonly id: CheckID;
  verdict: Verdict;
  evidence: string[];
}

interface Report {
  readonly at: string;
  readonly base: string;
  readonly project: string;
  readonly commit: string;
  readonly checks: CheckResult[];
}

interface Options {
  readonly base: string;
  readonly project: string;
  readonly token: string;
  readonly login: string;
  readonly repo: string;
  readonly checks: readonly CheckID[];
  readonly out: string;
}

interface RouteCard extends ExpectedCard {
  readonly waiting: {
    readonly reason: string;
    readonly since: string | null;
    readonly blocked_by: readonly string[];
  } | null;
  readonly waiting_place?: number | null;
  readonly phase_since?: string | null;
  readonly slot_since?: string | null;
}

interface RouteFacts {
  readonly phase_hours?: Readonly<Record<string, number>>;
  readonly work_hours?: number;
  readonly review_rounds?: number;
  readonly root?: boolean;
  readonly tree?: string;
}

interface RouteBoard extends ExpectedBoard {
  readonly columns: readonly {
    readonly id: string;
    readonly cards: readonly RouteCard[];
  }[];
  readonly issues?: Readonly<Record<string, RouteFacts>>;
}

interface Metrics {
  readonly phase_breakdown?: {
    readonly issues?: readonly {
      readonly key: string;
      readonly hours: number;
      readonly phases: Readonly<Record<string, number>>;
    }[];
  };
  readonly work_time?: {
    readonly issues?: readonly { readonly key: string; readonly hours: number }[];
  };
  readonly rework?: {
    readonly issues?: readonly { readonly key: string; readonly review_rounds: number }[];
  };
}

interface CheckInputs extends ExpectedViewCountsInput {
  readonly board: RouteBoard;
}

const CHECK_IDS = ["k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"] as const;
type CheckID = (typeof CHECK_IDS)[number];
const CELL_COLUMNS = [
  "key",
  "closed",
  "work_hours",
  "review_rounds",
  "slot_use",
  "waited",
  "waiting_reason",
  "queue",
  "time_in_phase",
  "legion",
  "stage",
  "phase_hours",
] as const;
const DAY_MS = 24 * 60 * 60 * 1000;
const SCREENSHOTS: readonly { readonly width: number; readonly height: number }[] = [
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
];
const COMPACT_VIEWPORTS: readonly { readonly width: number; readonly height: number }[] = [
  { width: 1280, height: 720 },
  { width: 390, height: 844 },
];

class NotYetError extends Error {}

/** How long a page may take to read what it shows before a check reads it as it stands. */
const SETTLE_MS = 30_000;

/** The number `text` spells, or NaN when it is missing or blank: `Number` reads both as 0. */
function numberOf(text: string | null | undefined): number {
  const trimmed = text?.trim() ?? "";
  return trimmed === "" ? Number.NaN : Number(trimmed);
}

/** Resolves once `scope` shows nothing still loading (a busy skeleton or a "Loading …" line),
 *  or after `ms`; a page still loading then is read as it stands. */
async function untilLoaded(scope: Locator, ms: number): Promise<void> {
  try {
    await scope
      .locator('[aria-busy="true"]')
      .or(scope.getByText(/^Loading\b.*…$/u))
      .first()
      .waitFor({ state: "detached", timeout: Math.max(ms, 1) });
  } catch (error) {
    if (!(error instanceof errors.TimeoutError)) throw error;
  }
}

async function isNotFound(page: Page): Promise<boolean> {
  return (
    (await page.getByRole("heading", { name: /not found/i }).count()) !== 0 ||
    (await page.getByText(/^Not found$/iu).count()) !== 0
  );
}

/** Waits until the Issues tab has read its data, since rows and counts arrive after the section
 * does: the In flight tab's count (which waits on the Legion board), the current view's count
 * where that route supplies one, and nothing in the tab is still loading. A tab that never
 * settles is read as it stands, so a missing selector still reports as it did. */
async function settled(page: Page, root: Locator): Promise<void> {
  const deadline = Date.now() + SETTLE_MS;
  const view = await root.getAttribute("data-view");
  const params = new URL(page.url()).searchParams;
  for (const id of new Set(["in-flight", view ?? ""])) {
    if (id === "" || id.startsWith("saved:")) continue;
    // Finished's tab count is only the cached 28-day default. Its `all` route deliberately has
    // no count, including the one-row route k4 uses to compare a closed issue's cells.
    if (id === "finished" && params.get("f.closed") === "all") continue;
    const tab = tabFor(page, id);
    if ((await tab.count()) === 0) continue;
    try {
      await tab
        .locator("[data-tab-count]")
        .first()
        .waitFor({ state: "attached", timeout: Math.max(deadline - Date.now(), 1) });
    } catch (error) {
      if (!(error instanceof errors.TimeoutError)) throw error;
    }
  }
  await untilLoaded(root, deadline - Date.now());
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function requireValue(value: string | undefined, name: string): string {
  if (value === undefined || value.trim() === "") throw new Error(`missing required --${name}`);
  return value;
}

async function run(
  command: readonly string[],
  cwd?: string
): Promise<{
  readonly exitCode: number;
  readonly stdout: string;
  readonly stderr: string;
}> {
  const process = Bun.spawn({ cmd: [...command], cwd, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    process.exited,
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
  ]);
  return { exitCode, stdout, stderr };
}

async function repositoryRoot(): Promise<string> {
  const result = await run(["git", "rev-parse", "--show-toplevel"]);
  if (result.exitCode !== 0)
    throw new Error(`cannot find repository root: ${result.stderr.trim()}`);
  return result.stdout.trim();
}

async function optionsOf(): Promise<Options> {
  const { values } = parseArgs({
    args: Bun.argv.slice(2),
    options: {
      base: { type: "string" },
      project: { type: "string" },
      token: { type: "string" },
      "token-file": { type: "string" },
      login: { type: "string", default: "sami@theorem.dev" },
      repo: { type: "string" },
      checks: { type: "string" },
      out: { type: "string" },
    },
    strict: true,
  });
  if (values.token !== undefined && values["token-file"] !== undefined)
    throw new Error("pass only one of --token and --token-file");
  const token =
    values.token ??
    (values["token-file"] === undefined
      ? process.env.DISPATCH_TOKEN
      : (await readFile(values["token-file"], "utf8")).trim());
  const checks = (values.checks ?? CHECK_IDS.join(",")).split(",") as CheckID[];
  if (checks.some((check) => !CHECK_IDS.includes(check)))
    throw new Error(`--checks must name comma-separated values from ${CHECK_IDS.join(", ")}`);
  return {
    base: requireValue(values.base, "base").replace(/\/$/u, ""),
    project: requireValue(values.project, "project"),
    token: requireValue(token, "token"),
    login: requireValue(values.login, "login"),
    repo: resolve(values.repo ?? (await repositoryRoot())),
    checks,
    out: resolve(requireValue(values.out, "out")),
  };
}

function urlFor(base: string, path: string, params?: URLSearchParams): string {
  const url = new URL(path, `${base}/`);
  if (params !== undefined) url.search = params.toString();
  return url.toString();
}

/** Address of a page under the project, `tail` being the part after `/projects/<project>`. */
function projectUrl(options: Options, tail: string): string {
  return urlFor(options.base, `/projects/${encodeURIComponent(options.project)}${tail}`);
}

/** The built-in views a project shows: Waiting and Slots need a Legion board. */
function viewsFor(inputs: CheckInputs): readonly BuiltInViewID[] {
  return inputs.hasLegion
    ? BUILT_IN_VIEW_IDS
    : BUILT_IN_VIEW_IDS.filter((view) => view !== "waiting" && view !== "slots");
}

async function apiJson<T>(options: Options, path: string, params?: URLSearchParams): Promise<T> {
  const response = await fetch(urlFor(options.base, path, params), {
    headers: { Authorization: `Bearer ${options.token}` },
  });
  if (!response.ok) {
    const body = (await response.text()).slice(0, 500);
    throw new Error(`${path} answered ${response.status}: ${body}`);
  }
  return (await response.json()) as T;
}

function listFrom(value: unknown): ExpectedIssue[] {
  if (Array.isArray(value)) return value as ExpectedIssue[];
  if (typeof value === "object" && value !== null && "issues" in value) {
    const issues = value.issues;
    if (Array.isArray(issues)) return issues as ExpectedIssue[];
  }
  throw new Error("the issue list response did not contain an issues array");
}

async function inputsFor(options: Options, login: string, nowMs: number): Promise<CheckInputs> {
  const openParams = new URLSearchParams({ project: options.project, open: "true" });
  const closedParams = new URLSearchParams({
    project: options.project,
    status: "done",
    closed_since: new Date(nowMs - 28 * DAY_MS).toISOString(),
  });
  const [openResponse, doneResponse, boardResponse] = await Promise.all([
    apiJson<unknown>(options, "/api/v1/issues", openParams),
    apiJson<unknown>(options, "/api/v1/issues", closedParams),
    fetch(
      urlFor(
        options.base,
        "/api/v1/legion/board",
        new URLSearchParams({ project: options.project })
      ),
      {
        headers: { Authorization: `Bearer ${options.token}` },
      }
    ),
  ]);
  let hasLegion = true;
  let board: RouteBoard = { columns: [] };
  if (boardResponse.status === 404) {
    hasLegion = false;
  } else if (!boardResponse.ok) {
    throw new Error(`/api/v1/legion/board answered ${boardResponse.status}`);
  } else {
    board = (await boardResponse.json()) as RouteBoard;
  }
  return {
    openIssues: listFrom(openResponse),
    doneIssues: listFrom(doneResponse),
    board,
    hasLegion,
    login,
    nowMs,
  };
}

async function waitForIssuesTab(page: Page, settle = true): Promise<Locator> {
  const root = page.locator("section[data-issues-tab]");
  try {
    await root.waitFor({ state: "attached", timeout: 5_000 });
  } catch {
    throw new NotYetError("the Issues tab has no data-issues-tab selector yet");
  }
  if (settle) await settled(page, root);
  return root;
}

async function openIssuesView(
  page: Page,
  options: Options,
  query: string,
  settle = true
): Promise<Locator> {
  await page.goto(projectUrl(options, `/issues?${query}`));
  return waitForIssuesTab(page, settle);
}

async function ensureSignedIn(context: BrowserContext, options: Options): Promise<string> {
  const whoami = await context.request.get(urlFor(options.base, "/auth/whoami"));
  if (whoami.status() !== 200) {
    const page = await context.newPage();
    await page.goto(
      urlFor(
        options.base,
        "/auth/_dev/signin",
        new URLSearchParams({ login: options.login, next: "/" })
      )
    );
    await page.close();
  }
  const signedIn = await context.request.get(urlFor(options.base, "/auth/whoami"));
  if (signedIn.status() !== 200)
    throw new Error(`browser sign-in failed: /auth/whoami answered ${signedIn.status()}`);
  const identity = (await signedIn.json()) as { readonly login?: string };
  if (identity.login === undefined) throw new Error("/auth/whoami did not return login");
  return identity.login;
}

async function newSignedInPage(
  browser: Browser,
  options: Options
): Promise<{ readonly context: BrowserContext; readonly page: Page; readonly login: string }> {
  const context = await browser.newContext();
  const login = await ensureSignedIn(context, options);
  return { context, page: await context.newPage(), login };
}

async function required(locator: Locator, description: string): Promise<Locator> {
  if ((await locator.count()) === 0) throw new NotYetError(`${description} is not available yet`);
  return locator.first();
}

async function rowCount(root: Locator): Promise<number> {
  const scroll = await required(
    root.locator('[data-testid="issues-table-scroll"]'),
    "the table scroll box"
  );
  const count = numberOf(await scroll.getAttribute("data-row-count"));
  if (!Number.isFinite(count))
    throw new NotYetError("the table scroll box has no numeric data-row-count");
  return count;
}

/** The count a view tab's pill shows; the Slots pill reads `<in use>/<cap>`, and counts the
 *  first. */
async function countPill(tab: Locator): Promise<number> {
  const pill = await required(tab.locator("[data-tab-count]"), "the view count pill");
  const text = (await pill.textContent())?.trim() ?? "";
  const match = /^(\d+)(?:\s*(?:\/|of)\s*\d+)?$/u.exec(text);
  if (match === null) throw new Error(`the view count pill ${JSON.stringify(text)} is not numeric`);
  return Number(match[1]);
}

function tabFor(page: Page, id: string): Locator {
  return page.locator(`#view-${id}-tab`);
}

function raw(value: unknown): string {
  return value === null || value === undefined ? "" : String(value);
}

function cardsByKey(
  board: RouteBoard
): Map<string, { readonly card: RouteCard; readonly column: string }> {
  const cards = new Map<string, { readonly card: RouteCard; readonly column: string }>();
  for (const column of board.columns) {
    for (const card of column.cards) cards.set(card.key, { card, column: column.id });
  }
  return cards;
}

function phaseHours(facts: RouteFacts | undefined): number {
  return Object.values(facts?.phase_hours ?? {}).reduce((total, hours) => total + hours, 0);
}

async function cellValue(row: Locator, id: string): Promise<string> {
  const cell = await required(row.locator(`[data-cell="${id}"]`), `the ${id} cell selector`);
  const value = await cell.getAttribute("data-value");
  if (value === null) throw new NotYetError(`the ${id} cell has no data-value`);
  return value;
}

function near(actual: number, expected: number, label: string): void {
  if (!Number.isFinite(actual) || Math.abs(actual - expected) > 0.01)
    throw new Error(`${label}: expected ${expected}, got ${actual}`);
}

async function checkK1(page: Page, options: Options): Promise<string[]> {
  const tableCalls = await run(
    [
      "git",
      "grep",
      "-n",
      "-E",
      String.raw`\buseReactTable\s*\(`,
      "--",
      "packages/dispatch/web/src",
      ":(exclude,glob)packages/dispatch/web/src/**/*.test.tsx",
    ],
    options.repo
  );
  const legacyReferences = await run(
    [
      "git",
      "grep",
      "-n",
      "-E",
      "IssueGrid|stubItem|function (Slots|WaitingLine|FinishedIssues)",
      "--",
      "packages/dispatch/web/src",
    ],
    options.repo
  );
  const dataTableUse = await run(
    [
      "git",
      "grep",
      "-n",
      "<DataTable",
      "--",
      "packages/dispatch/web/src/features/project/IssuesTable.tsx",
    ],
    options.repo
  );
  const results = [tableCalls, legacyReferences, dataTableUse];
  if (results.some((result) => result.exitCode > 1))
    throw new Error("could not inspect the table implementation");

  const tableLines = tableCalls.stdout.trim().split("\n").filter(Boolean);
  const legacyLines = legacyReferences.stdout.trim().split("\n").filter(Boolean);
  const dataTableLines = dataTableUse.stdout.trim().split("\n").filter(Boolean);
  if (
    tableLines.length !== 1 ||
    !tableLines[0]?.includes("DataTable.tsx") ||
    legacyLines.length !== 0 ||
    dataTableLines.length === 0
  )
    throw new Error(
      `useReactTable calls: ${tableLines.join(" | ") || "none"}; legacy references: ${
        legacyLines.join(" | ") || "none"
      }; DataTable uses: ${dataTableLines.join(" | ") || "none"}`
    );

  await page.goto(projectUrl(options, "/evals"));
  if (await isNotFound(page)) throw new NotYetError("the Evals route is not available yet");
  await untilLoaded(page.locator("body"), SETTLE_MS);
  const tableButtons = page.getByRole("button", { name: "Table", exact: true });
  const tableButtonCount = await tableButtons.count();
  if (tableButtonCount === 0)
    throw new NotYetError("the Evals charts do not offer a Table mode yet");
  for (let index = 0; index < tableButtonCount; index += 1) await tableButtons.nth(index).click();
  if ((await page.locator("[data-issue-row]").count()) !== 0)
    throw new Error("the Evals tab still renders issue rows");
  const issueHeader = page.locator("table thead th").filter({ hasText: /^Issue$/u });
  if ((await issueHeader.count()) !== 0)
    throw new Error("an Evals chart table still begins with Issue");
  return ["one DataTable useReactTable call", "Evals has no issue table"];
}

/** Opens the In flight view with `query` and requires `expectedRows` rows plus the changed mark. */
async function requireInFlightOverride(
  page: Page,
  options: Options,
  query: string,
  expectedRows: number,
  failure: string
): Promise<void> {
  const root = await openIssuesView(page, options, `view=in-flight&${query}`);
  const tab = await required(tabFor(page, "in-flight"), "In flight view tab");
  if (
    (await rowCount(root)) !== expectedRows ||
    (await tab.locator("[data-tab-marked]").count()) !== 1
  )
    throw new Error(failure);
}

async function checkK2(page: Page, options: Options, inputs: CheckInputs): Promise<string[]> {
  const counts = expectedViewCounts(inputs);
  const evidence: string[] = [];
  for (const view of viewsFor(inputs)) {
    const root = await openIssuesView(page, options, `view=${view}`);
    const tab = await required(tabFor(page, view), `${view} view tab`);
    const [tabRows, listedRows] = await Promise.all([countPill(tab), rowCount(root)]);
    if (tabRows !== counts[view] || listedRows !== counts[view])
      throw new Error(`${view}: expected ${counts[view]}, tab ${tabRows}, table ${listedRows}`);
    evidence.push(`${view} ${listedRows}`);
  }

  await requireInFlightOverride(
    page,
    options,
    "status=triage",
    0,
    "the status override does not show zero rows and the changed mark"
  );
  const expectedImplementing = expectedStageCount(inputs, "phase:implementing");
  await requireInFlightOverride(
    page,
    options,
    "f.stage=phase:implementing",
    expectedImplementing,
    `the implementing override expected ${expectedImplementing} rows and a changed mark`
  );
  return evidence;
}

async function checkK3(browser: Browser, options: Options, inputs: CheckInputs): Promise<string[]> {
  const first = await newSignedInPage(browser, options);
  try {
    await openIssuesView(
      first.page,
      options,
      "view=in-flight&f.stage=phase:implementing&sort=-priority"
    );
    const save = await required(
      first.page.getByRole("button", { name: "Save view", exact: true }),
      "Save view"
    );
    await save.click();
    const name = await required(
      first.page.locator("input:focus, textarea:focus"),
      "the saved view name field"
    );
    await name.fill("Test");
    const confirm = await required(
      first.page.getByRole("button", { name: "Save", exact: true }),
      "the Save button"
    );
    await confirm.click();
    await first.page.reload();
    await waitForIssuesTab(first.page);
    await openIssuesView(first.page, options, "view=open");
    const saved = await required(
      first.page.locator("#view-saved-0-tab"),
      "the Test saved view tab"
    );
    await saved.click();
    await waitForIssuesTab(first.page);
    const query = new URL(first.page.url()).searchParams;
    if (query.get("f.stage") !== "phase:implementing" || query.get("sort") !== "-priority")
      throw new Error("the saved view did not restore its stage and sort overrides");
    if (
      (await rowCount(await waitForIssuesTab(first.page))) !==
      expectedStageCount(inputs, "phase:implementing")
    )
      throw new Error("the saved view row count does not match the implementing stage");

    const second = await newSignedInPage(browser, options);
    try {
      await openIssuesView(second.page, options, "view=in-flight");
      if ((await second.page.locator("#view-saved-0-tab").count()) !== 0)
        throw new Error("a saved view leaked into a fresh browser context");
    } finally {
      await second.context.close();
    }

    const remove = await required(
      first.page.getByRole("button", { name: "Delete view", exact: true }),
      "Delete view"
    );
    await remove.click();
    await first.page.reload();
    await waitForIssuesTab(first.page);
    if ((await first.page.locator("#view-saved-0-tab").count()) !== 0)
      throw new Error("the deleted saved view remains after reload");
    return ["saved view persists in one context and not a fresh context"];
  } finally {
    await first.context.close();
  }
}

async function checkK4(page: Page, options: Options, inputs: CheckInputs): Promise<string[]> {
  if (!inputs.hasLegion) throw new NotYetError("the project has no Legion board to compare");
  const cards = cardsByKey(inputs.board);
  const open = inputs.openIssues.filter((issue) => cards.has(issue.key)).slice(0, 10);
  const closed = inputs.doneIssues
    .filter((issue) => inputs.board.issues?.[issue.key] !== undefined)
    .slice(0, 10);
  if (open.length < 10 || closed.length < 10)
    throw new NotYetError(
      `need 10 open cards and 10 closed facts; found ${open.length} and ${closed.length}`
    );
  const metrics = await apiJson<Metrics>(
    options,
    "/api/v1/legion/metrics",
    new URLSearchParams({
      project: options.project,
      from: new Date(inputs.nowMs - 28 * DAY_MS).toISOString(),
      to: new Date(inputs.nowMs).toISOString(),
    })
  );
  const work = new Map((metrics.work_time?.issues ?? []).map((issue) => [issue.key, issue.hours]));
  const rounds = new Map(
    (metrics.rework?.issues ?? []).map((issue) => [issue.key, issue.review_rounds])
  );
  const evidence: string[] = [];

  for (const [issues, closedIssue] of [
    [open, false],
    [closed, true],
  ] as const) {
    for (const issue of issues) {
      const root = await openIssuesView(
        page,
        options,
        closedIssue
          ? `view=finished&only=${encodeURIComponent(issue.key)}&f.closed=all&cols=${CELL_COLUMNS.join(",")}`
          : `view=open&only=${encodeURIComponent(issue.key)}&cols=${CELL_COLUMNS.join(",")}`
      );
      if ((await rowCount(root)) !== 1) throw new Error(`${issue.key} did not narrow to one row`);
      const row = await required(
        root.locator(`[data-issue-row="${issue.key}"]`),
        `${issue.key} row`
      );
      const card = cards.get(issue.key);
      const facts = inputs.board.issues?.[issue.key];
      const expected: Readonly<Record<(typeof CELL_COLUMNS)[number], string>> = {
        key: issue.key,
        closed: raw(issue.closed_at),
        work_hours: raw(facts?.work_hours),
        review_rounds: raw(facts?.review_rounds),
        slot_use: raw(card?.card.slot_use),
        waited: raw(card?.card.waiting?.since),
        waiting_reason: raw(card?.card.waiting?.reason),
        queue: raw(card?.card.waiting_place),
        time_in_phase: raw(card?.card.phase_since),
        legion: raw(facts?.tree),
        stage: expectedStageOf(issue, card?.column),
        phase_hours: facts?.phase_hours === undefined ? "" : raw(phaseHours(facts)),
      };
      for (const [id, value] of Object.entries(expected)) {
        const actual = await cellValue(row, id);
        if (id === "closed" && value !== "" && actual !== "") {
          if (Date.parse(actual) !== Date.parse(value))
            throw new Error(`${issue.key} ${id}: expected ${value}, got ${actual}`);
        } else if (actual !== value) {
          throw new Error(`${issue.key} ${id}: expected ${value}, got ${actual}`);
        }
      }
      if (closedIssue) {
        near(Number(expected.work_hours), work.get(issue.key) ?? Number.NaN, `${issue.key} work`);
        near(
          Number(expected.review_rounds),
          rounds.get(issue.key) ?? Number.NaN,
          `${issue.key} rounds`
        );
      }
      evidence.push(issue.key);
    }
  }
  return [`compared ${evidence.length} issues`, ...evidence];
}

async function checkK5(browser: Browser, options: Options): Promise<string[]> {
  const cases: readonly {
    readonly query: string;
    readonly expected: (urls: URL[]) => boolean;
  }[] = [
    {
      query: "view=open",
      expected: (urls) =>
        urls.length > 0 && urls.every((url) => url.searchParams.get("open") === "true"),
    },
    {
      query: "view=finished",
      expected: (urls) =>
        urls.some(
          (url) => url.searchParams.get("status") === "done" && url.searchParams.has("closed_since")
        ),
    },
    {
      query: "view=finished&f.closed=2026-11-03..2026-11-04",
      expected: (urls) =>
        urls.some(
          (url) =>
            url.searchParams.get("status") === "done" &&
            url.searchParams.has("closed_since") &&
            url.searchParams.has("closed_before")
        ),
    },
    {
      query: "view=open&nofilter=1",
      expected: (urls) =>
        urls.some((url) => url.searchParams.get("open") === "true") &&
        urls.some(
          (url) => url.searchParams.get("status") === "done" && url.searchParams.has("closed_since")
        ),
    },
  ];
  const evidence: string[] = [];
  for (const item of cases) {
    const { context, page } = await newSignedInPage(browser, options);
    try {
      await delayLegionBoard(page, 5_000);
      const requests: URL[] = [];
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (
          url.pathname === "/api/v1/issues" &&
          url.searchParams.get("project") === options.project
        )
          requests.push(url);
      });
      const started = Date.now();
      const root = await openIssuesView(page, options, item.query, false);
      try {
        await root
          .locator("[data-issue-row]")
          .first()
          .waitFor({ state: "attached", timeout: Math.max(5_000 - (Date.now() - started), 1) });
      } catch (error) {
        if (!(error instanceof errors.TimeoutError)) throw error;
        throw new Error(`${item.query} issue rows did not show within 5 s`);
      }
      if (requests.some((url) => !url.searchParams.has("open") && !url.searchParams.has("status")))
        throw new Error(`${item.query} made a full issue listing request`);
      if (!item.expected(requests))
        throw new Error(`${item.query} made the wrong issue list requests`);
      const doneRequests = requests.filter((url) => url.searchParams.get("status") === "done");
      if (doneRequests.some((url) => !url.searchParams.has("closed_since")))
        throw new Error(`${item.query} read done issues without closed_since`);
      if (
        item.query.includes("2026-11-03..2026-11-04") &&
        doneRequests.some((url) => !url.searchParams.has("closed_before"))
      )
        throw new Error(`${item.query} read done issues without closed_before`);
      evidence.push(item.query);
    } finally {
      await context.close();
    }
  }
  return evidence;
}

function segmentWidths(counts: readonly number[]): number[] {
  const total = counts.reduce((sum, count) => sum + count, 0);
  if (total <= 0) return counts.map(() => 100 / Math.max(counts.length, 1));
  const widths = counts.map((count) => (100 * count) / total);
  const atFloor = new Set<number>();
  for (;;) {
    const room = 100 - atFloor.size * 3;
    const rest = widths.reduce((sum, width, index) => (atFloor.has(index) ? sum : sum + width), 0);
    const under = widths.findIndex(
      (width, index) => !atFloor.has(index) && (rest <= 0 || (room * width) / rest < 3)
    );
    if (under === -1)
      return widths.map((width, index) =>
        atFloor.has(index) ? 3 : rest <= 0 ? room / widths.length : (room * width) / rest
      );
    atFloor.add(under);
  }
}

async function stageScaleValues(): Promise<readonly string[]> {
  const theme: unknown = themeClasses;
  if (
    typeof theme !== "object" ||
    theme === null ||
    !("stageScale" in theme) ||
    typeof theme.stageScale !== "object" ||
    theme.stageScale === null
  )
    throw new Error("the web theme has no stageScale");
  const values = Object.values(theme.stageScale).filter(
    (value): value is string => typeof value === "string"
  );
  if (values.length === 0) throw new Error("stageScale has no CSS class values");
  return values;
}

async function checkK6(page: Page, options: Options): Promise<string[]> {
  const root = await openIssuesView(page, options, "view=in-flight");
  const bar = await required(root.locator("[data-issues-bar]"), "the Issues bar");
  const segments = await bar.locator("[data-bar-segment]").all();
  if (segments.length === 0) throw new NotYetError("the Issues bar has no segments");
  const oldScales = await run(
    [
      "git",
      "grep",
      "-n",
      "-E",
      "scaleOf|issueStatusScale|legionPhaseScale",
      "packages/dispatch/web/src",
    ],
    options.repo
  );
  if (oldScales.exitCode === 0) throw new Error(`old scales remain: ${oldScales.stdout.trim()}`);
  if (oldScales.exitCode > 1)
    throw new Error(`could not search old scales: ${oldScales.stderr.trim()}`);
  const stageScales = await stageScaleValues();
  const counts: number[] = [];
  const boxes: { readonly x: number; readonly width: number }[] = [];
  for (const segment of segments) {
    const count = numberOf(await segment.getAttribute("data-count"));
    const box = await segment.boundingBox();
    if (!Number.isFinite(count) || box === null)
      throw new Error("a bar segment lacks count or geometry");
    const classes = (await segment.getAttribute("class"))?.split(/\s+/u) ?? [];
    if (
      !stageScales.some((scale) =>
        scale.split(/\s+/u).every((className) => classes.includes(className))
      )
    )
      throw new Error("a bar segment colour is not a stageScale value");
    counts.push(count);
    boxes.push(box);
  }
  const total = counts.reduce((sum, count) => sum + count, 0);
  if (total !== (await rowCount(root)))
    throw new Error("bar segment counts do not equal the table row count");
  const barBox = await bar.boundingBox();
  if (barBox === null || Math.abs(boxes[0].x - barBox.x) > 0.5)
    throw new Error("the first bar segment does not start at the bar's left edge");
  for (const [index, width] of segmentWidths(counts).entries()) {
    if (Math.abs(boxes[index].width - (barBox.width * width) / 100) > 0.5)
      throw new Error(`bar segment ${index} is not ${width}% of the bar`);
  }
  return [`${counts.length} segments`, `${total} rows`];
}

async function phaseCells(page: Page): Promise<readonly Locator[]> {
  const cells = await page.locator("[data-phase-hours-length]").all();
  if (cells.length === 0)
    throw new NotYetError("Time per phase cells lack data-phase-hours-length");
  return cells;
}

async function checkK7(page: Page, options: Options): Promise<string[]> {
  await openIssuesView(page, options, "view=in-flight&cols=key,phase_hours");
  const mode = await required(
    page.locator('[role="group"][aria-label="Time per phase mode"]'),
    "the Time per phase mode control"
  );
  const share = await required(
    mode.getByRole("button", { name: "Share", exact: true }),
    "the Share button"
  );
  const hours = await required(
    mode.getByRole("button", { name: "Hours", exact: true }),
    "the Hours button"
  );
  if ((await share.getAttribute("aria-pressed")) !== "true")
    throw new Error("Time per phase does not open on Share");
  await hours.click();

  const verifyPage = async (): Promise<void> => {
    const cells = await phaseCells(page);
    let longest = 0;
    for (const cell of cells) {
      const length = numberOf(await cell.getAttribute("data-phase-hours-length"));
      if (!Number.isFinite(length))
        throw new Error("a phase bar has no numeric data-phase-hours-length");
      longest = Math.max(longest, length);
      if (length > 0) {
        const row = await required(cell.locator("xpath=ancestor::tr"), "the phase bar row");
        const total = await required(
          row.locator("[data-phase-hours-total]"),
          "the phase-hours total"
        );
        if ((await total.textContent())?.trim() === "")
          throw new Error("a phase-hours total is empty");
      }
    }
    if (Math.abs(longest - 100) > 0.01)
      throw new Error(`the widest phase bar is ${longest}, not 100`);
  };
  await verifyPage();
  const next = page.getByRole("button", { name: "Next page", exact: true });
  if ((await next.count()) !== 0 && !(await next.isDisabled())) {
    await next.click();
    await verifyPage();
  }
  return ["Share opens selected", "Hours bars fill their page maximum"];
}

function floorMinute(nowMs: number): number {
  return Math.floor(nowMs / 60_000) * 60_000;
}

async function metricsFor(
  options: Options,
  from: string,
  to: string,
  excludeTree?: string
): Promise<Metrics> {
  const params = new URLSearchParams({ project: options.project, from, to });
  if (excludeTree !== undefined) params.append("exclude_tree", excludeTree);
  return apiJson<Metrics>(options, "/api/v1/legion/metrics", params);
}

async function checkK8(page: Page, options: Options, inputs: CheckInputs): Promise<string[]> {
  if (!inputs.hasLegion) throw new NotYetError("the project has no Legion metrics");
  const minute = floorMinute(inputs.nowMs);
  const to = new Date(minute).toISOString();
  const now = new Date(inputs.nowMs);
  const dayEnd = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const dayStart = new Date(dayEnd.getTime() - DAY_MS);
  const [dayMetrics, windowMetrics] = await Promise.all([
    metricsFor(options, dayStart.toISOString(), dayEnd.toISOString()),
    Promise.all(
      [28, 7].map(async (days) => ({
        days,
        metrics: await metricsFor(options, new Date(minute - days * DAY_MS).toISOString(), to),
      }))
    ),
  ]);
  const checks: { readonly query: string; readonly expected: number }[] = [];
  for (const { days, metrics } of windowMetrics) {
    const issues = metrics.phase_breakdown?.issues ?? [];
    checks.push({ query: `view=finished&f.closed=${days}d`, expected: issues.length });
    checks.push({
      query: `view=finished&f.closed=${days}d&f.phase_hours=reviewing`,
      expected: issues.filter((issue) => (issue.phases.reviewing ?? 0) > 0).length,
    });
  }
  checks.push({
    query: `view=finished&f.closed=${dayStart.toISOString().slice(0, 10)}..${dayEnd.toISOString().slice(0, 10)}`,
    expected: dayMetrics.phase_breakdown?.issues?.length ?? 0,
  });
  const trees = new Map<string, number>();
  for (const issue of inputs.doneIssues) {
    const tree = inputs.board.issues?.[issue.key]?.tree;
    if (tree !== undefined) trees.set(tree, (trees.get(tree) ?? 0) + 1);
  }
  const largestTree = [...trees.entries()].sort((left, right) => right[1] - left[1])[0]?.[0];
  if (largestTree === undefined)
    throw new NotYetError("no finished Legion tree is available for exclude_tree");
  const excluded = await metricsFor(
    options,
    new Date(minute - 28 * DAY_MS).toISOString(),
    to,
    largestTree
  );
  checks.push({
    query: `view=finished&f.closed=28d&exclude_tree=${encodeURIComponent(largestTree)}`,
    expected: excluded.phase_breakdown?.issues?.length ?? 0,
  });

  const evidence: string[] = [];
  for (const item of checks) {
    const root = await openIssuesView(page, options, item.query);
    if ((await rowCount(root)) !== item.expected)
      throw new Error(`${item.query}: expected ${item.expected}`);
    evidence.push(item.query);
  }
  return evidence;
}

async function checkK9(page: Page, options: Options, inputs: CheckInputs): Promise<string[]> {
  for (const { query, cell, label } of [
    { query: "view=in-flight", cell: "phase", label: "Phase" },
    { query: "view=finished", cell: "phase_hours", label: "Time per phase" },
  ] as const) {
    const root = await openIssuesView(page, options, query);
    const link = await required(
      root.locator(`[data-cell="${cell}"] a[data-timeline-link]`),
      `a ${label} timeline link`
    );
    const row = await required(link.locator("xpath=ancestor::tr"), `the ${label} timeline row`);
    const key = await row.getAttribute("data-issue-row");
    if (key === null) throw new Error(`the ${label} timeline row has no issue key`);
    await link.click();
    const pathname = new URL(page.url()).pathname;
    if (!pathname.endsWith(`/projects/${options.project}/legion/${key}`))
      throw new Error(`the ${label} timeline link has the wrong address`);
  }

  const withoutFacts = inputs.openIssues.find(
    (issue) => inputs.board.issues?.[issue.key] === undefined
  );
  if (withoutFacts === undefined)
    throw new NotYetError("no open issue without Legion facts is available");
  const root = await openIssuesView(
    page,
    options,
    `view=open&only=${encodeURIComponent(withoutFacts.key)}`
  );
  if ((await rowCount(root)) !== 1)
    throw new Error("the issue without facts did not narrow to one row");
  await required(
    root.locator(`[data-issue-row="${withoutFacts.key}"]`),
    "the issue without facts row"
  );
  if ((await root.locator("a[data-timeline-link]").count()) !== 0)
    throw new Error("an issue without facts has a timeline link");
  return ["Phase and Time per phase use the stable timeline address"];
}

async function checkK10(page: Page, options: Options, inputs: CheckInputs): Promise<string[]> {
  const initialViewport = page.viewportSize();
  const views = viewsFor(inputs);
  let infoHints = 0;
  try {
    for (const viewport of COMPACT_VIEWPORTS) {
      await page.setViewportSize(viewport);
      for (const view of views) {
        const root = await openIssuesView(page, options, `view=${view}`);
        const headers = await root.locator("th:visible").evaluateAll((cells) =>
          cells.map((cell) => {
            const copy = cell.cloneNode(true) as HTMLElement;
            for (const control of copy.querySelectorAll('button[aria-label], [role="group"]'))
              control.remove();
            return (copy.textContent ?? "").replace(/[▲▼↕]/gu, "").trim();
          })
        );
        for (const text of headers) {
          if (text.length > 16)
            throw new Error(`${view} header ${JSON.stringify(text)} exceeds 16 characters`);
        }
        for (const viewID of views) {
          const tab = await required(tabFor(page, viewID), `${viewID} view tab`);
          const text = await tab.evaluate((button) => {
            const copy = button.cloneNode(true) as HTMLElement;
            for (const mark of copy.querySelectorAll("[data-tab-count], [data-tab-marked]"))
              mark.remove();
            return (copy.textContent ?? "").trim();
          });
          if (text.length > 14)
            throw new Error(`${viewID} tab ${JSON.stringify(text)} exceeds 14 characters`);
        }
        const labels = await root
          .locator("button[aria-label]")
          .evaluateAll((buttons) =>
            buttons
              .filter((button) => button.textContent?.trim() === "i")
              .map((button) => button.getAttribute("aria-label")?.trim() ?? "")
          );
        if (labels.some((label) => label.length === 0))
          throw new Error(`${view} has an InfoHint without an accessible label`);
        infoHints += labels.length;
        const sentences = await root.evaluate((element) => {
          const text = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
          const found: string[] = [];
          for (;;) {
            const node = text.nextNode();
            if (node === null) break;
            const value = node.textContent?.trim() ?? "";
            const parent = node.parentElement;
            if (
              value.endsWith(".") &&
              parent?.checkVisibility() &&
              parent?.closest('[role="alert"], [role="tooltip"], [data-issue-row]') === null &&
              parent?.tagName !== "SCRIPT" &&
              parent?.tagName !== "STYLE"
            )
              found.push(value);
          }
          return found;
        });
        if (sentences.length > 0)
          throw new Error(`${view} exposes help text: ${sentences.join(" | ")}`);
      }
    }
  } finally {
    if (initialViewport !== null) await page.setViewportSize(initialViewport);
  }
  return [
    `${infoHints} InfoHint labels`,
    "headers, tabs and visible prose meet the compactness rules",
  ];
}

async function takeScreenshots(page: Page, options: Options): Promise<void> {
  for (const viewport of SCREENSHOTS) {
    await page.setViewportSize(viewport);
    for (const view of BUILT_IN_VIEW_IDS) {
      await page.goto(projectUrl(options, `/issues?view=${view}`));
      try {
        await waitForIssuesTab(page);
      } catch (error) {
        if (!(error instanceof NotYetError)) throw error;
      }
      await page.screenshot({
        fullPage: true,
        path: join(options.out, `issues-${view}-${viewport.width}.png`),
      });
    }
    await page.goto(projectUrl(options, "/evals"));
    if (await isNotFound(page)) await page.goto(projectUrl(options, "/legion"));
    await untilLoaded(page.locator("body"), SETTLE_MS);
    await page.screenshot({
      fullPage: true,
      path: join(options.out, `evals-${viewport.width}.png`),
    });
  }
}

function requireInputs(inputs: CheckInputs | undefined, failure: string | undefined): CheckInputs {
  if (inputs === undefined)
    throw new Error(`could not read check inputs: ${failure ?? "the input read did not return"}`);
  return inputs;
}

async function executeCheck(
  id: CheckID,
  page: Page,
  browser: Browser,
  options: Options,
  inputs: CheckInputs | undefined,
  inputFailure: string | undefined
): Promise<CheckResult> {
  try {
    const evidence = await (
      {
        k1: () => checkK1(page, options),
        k2: () => checkK2(page, options, requireInputs(inputs, inputFailure)),
        k3: () => checkK3(browser, options, requireInputs(inputs, inputFailure)),
        k4: () => checkK4(page, options, requireInputs(inputs, inputFailure)),
        k5: () => checkK5(browser, options),
        k6: () => checkK6(page, options),
        k7: () => checkK7(page, options),
        k8: () => checkK8(page, options, requireInputs(inputs, inputFailure)),
        k9: () => checkK9(page, options, requireInputs(inputs, inputFailure)),
        k10: () => checkK10(page, options, requireInputs(inputs, inputFailure)),
      } satisfies Record<CheckID, () => Promise<string[]>>
    )[id]();
    return { id, verdict: "PASS", evidence };
  } catch (error) {
    return {
      id,
      verdict: error instanceof NotYetError ? "NOT-YET" : "FAIL",
      evidence: [errorText(error)],
    };
  }
}

async function main(): Promise<void> {
  const options = await optionsOf();
  await mkdir(options.out, { recursive: true });
  const commit = (await run(["git", "rev-parse", "HEAD"], options.repo)).stdout.trim();
  const browser = await chromium.launch();
  let context: BrowserContext | undefined;
  try {
    context = await browser.newContext();
    const login = await ensureSignedIn(context, options);
    const page = await context.newPage();
    let inputs: CheckInputs | undefined;
    let inputFailure: string | undefined;
    try {
      inputs = await inputsFor(options, login, Date.now());
    } catch (error) {
      inputFailure = errorText(error);
    }
    const checks: CheckResult[] = [];
    for (const id of options.checks) {
      const result = await executeCheck(id, page, browser, options, inputs, inputFailure);
      checks.push(result);
      console.log(`${result.id} ${result.verdict}: ${result.evidence.join("; ")}`);
    }
    try {
      await takeScreenshots(page, options);
    } catch (error) {
      const result = checks.find((candidate) => candidate.id === "k10");
      if (result !== undefined) {
        result.verdict = "FAIL";
        result.evidence.push(`screenshots: ${errorText(error)}`);
      } else {
        console.error(`screenshots FAIL: ${errorText(error)}`);
      }
    }
    const report: Report = {
      at: new Date().toISOString(),
      base: options.base,
      project: options.project,
      commit,
      checks,
    };
    await writeFile(join(options.out, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
    if (checks.some((check) => check.verdict === "FAIL")) process.exitCode = 1;
  } finally {
    await context?.close();
    await browser.close();
  }
}

await main();
