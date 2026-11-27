import type { Page } from "@playwright/test";

/**
 * Shared constants and readers for R7's block editor tests (`e2e/issue-page-block-editor.e2e.ts`,
 * Row 6 and Rows 4-5 of `e2e/issue-page-load.e2e.ts`, and `e2e/issue-page-load-same.e2e.ts`).
 */

/** The `performance.mark` set when the page starts making the block editor ready. */
export const BLOCK_EDITOR_START_MARK = "block-editor-start";

/** The `performance.mark` set when the first sync is applied and the block editor is built. */
export const BLOCK_EDITOR_READY_MARK = "block-editor-ready";

/** The `performance.measure` name recorded by each task of the work that makes the editor ready. */
export const BLOCK_EDITOR_TASK_MEASURE = "block-editor-task";

/** R7's per-task limit for the work that makes the block editor ready, in ms. */
export const BLOCK_EDITOR_TASK_MS = 100;

/** Pathname pattern of the document room's WebSocket (`/ws/doc/<artifactId>`). */
export const ROOM_SOCKET_PATTERN = /^\/ws\/doc\/[^/?#]+$/;

/**
 * URL path-and-query patterns for every read R7 forbids the block editor's work from sending:
 * - live document text (`liveTextQueryOptions`: `/api/v1/artifacts/<id>/text`)
 * - document blocks (`artifactBlocksQuery`: `/api/v1/artifacts/<id>/blocks`)
 * - artifact asks (`ownerAsksQueryOptions` with no issue: `/api/v1/artifacts/<id>/asks`)
 * - issue asks and ask threads (`issueAsksQuery`, `issueAskThreadsQuery`: `/api/v1/issues/<key>/asks`)
 * - spec view-of tree read (`issueViewOfQuery`: `/api/v1/issues/<key>/tree?view=spec`)
 * - decisions tree read and legacy decisions read (`issueDecisionsQuery`:
 *   `/api/v1/issues/<key>/tree?view=decisions` and `/api/v1/issues/<key>/decisions`)
 * - read marker (`getDocumentRead`, `documentReadRequest`: `/api/v1/me/documents/<id>/read`)
 *
 * Does not match Conversation's tree read (`/api/v1/issues/<key>/tree?kind=conversation`),
 * Graph's tree read (`/api/v1/issues/<key>/tree`), or `/auth/whoami`.
 */
export const FORBIDDEN_READS: readonly RegExp[] = [
  /^\/api\/v1\/artifacts\/[^/?#]+\/text(?:\?|$)/,
  /^\/api\/v1\/artifacts\/[^/?#]+\/blocks(?:\?|$)/,
  /^\/api\/v1\/artifacts\/[^/?#]+\/asks(?:\?|$)/,
  /^\/api\/v1\/issues\/[^/?#]+\/asks(?:\?|$)/,
  /^\/api\/v1\/issues\/[^/?#]+\/tree\?(?:[^#]*&)?view=spec(?:&|$)/,
  /^\/api\/v1\/issues\/[^/?#]+\/tree\?(?:[^#]*&)?view=decisions(?:&|$)/,
  /^\/api\/v1\/issues\/[^/?#]+\/decisions(?:\?|$)/,
  /^\/api\/v1\/me\/documents\/[^/?#]+\/read(?:\?|$)/,
];

/** Normalizes a full URL or relative path+query into `<pathname><search>`. */
export function pathAndSearch(urlOrPath: string): string {
  if (urlOrPath.startsWith("/")) return urlOrPath;
  const parsed = new URL(urlOrPath);
  return `${parsed.pathname}${parsed.search}`;
}

/** Whether `url` is a document room WebSocket connection. */
export function isRoomSocket(url: string | URL): boolean {
  const pathname =
    typeof url === "string" ? new URL(url, "http://dispatch.local").pathname : url.pathname;
  return ROOM_SOCKET_PATTERN.test(pathname);
}

/** Whether `urlOrPath` matches any read forbidden by R7. */
export function isForbiddenRead(urlOrPath: string): boolean {
  const candidate = pathAndSearch(urlOrPath);
  return FORBIDDEN_READS.some((pattern) => pattern.test(candidate));
}

/** Every entry of `urls` matching `FORBIDDEN_READS`, in order. */
export function forbiddenReadsIn(urls: readonly string[]): string[] {
  return urls.map(pathAndSearch).filter(isForbiddenRead);
}

/** One `performance.measure` entry read from the page. */
export interface TaskMeasure {
  readonly duration: number;
  readonly name: string;
  readonly startTime: number;
}

/** Waits until `page` records a `performance.mark` named `name`, returning its first `startTime`. */
export async function waitForMark(page: Page, name: string, timeoutMs = 30_000): Promise<number> {
  try {
    await page.waitForFunction(
      (markName) => performance.getEntriesByName(markName, "mark").length > 0,
      name,
      { timeout: timeoutMs }
    );
  } catch (cause) {
    throw new Error(`Missing performance mark ${JSON.stringify(name)}`, { cause });
  }
  const startTime = await page.evaluate(
    (markName) => performance.getEntriesByName(markName, "mark")[0]?.startTime ?? null,
    name
  );
  if (startTime === null) {
    throw new Error(`performance.mark(${JSON.stringify(name)}) was not recorded`);
  }
  return startTime;
}

/** Reads the first `performance.mark` named `name` if already recorded, else null. */
export async function readMark(page: Page, name: string): Promise<number | null> {
  return await page.evaluate(
    (markName) => performance.getEntriesByName(markName, "mark")[0]?.startTime ?? null,
    name
  );
}

/** Reads `block-editor-task` `performance.measure` entries whose startTime lies in `[from, until]`. */
export async function blockEditorMeasures(
  page: Page,
  from: number,
  until: number
): Promise<TaskMeasure[]> {
  return await page.evaluate(
    ({ fromAt, measureName, untilAt }) =>
      performance
        .getEntriesByName(measureName, "measure")
        .filter((entry) => entry.startTime >= fromAt && entry.startTime <= untilAt)
        .map((entry) => ({
          duration: entry.duration,
          name: entry.name,
          startTime: entry.startTime,
        })),
    { fromAt: from, measureName: BLOCK_EDITOR_TASK_MEASURE, untilAt: until }
  );
}

/** The longest `longtask` entry in `tasks` whose startTime lies in `[from, until]`, or 0 if none. */
export function longestTaskInWindow(
  tasks: readonly { readonly duration: number; readonly startTime: number }[],
  from: number,
  until: number
): number {
  let longest = 0;
  for (const task of tasks) {
    if (task.startTime >= from && task.startTime <= until) {
      longest = Math.max(longest, task.duration);
    }
  }
  return longest;
}
