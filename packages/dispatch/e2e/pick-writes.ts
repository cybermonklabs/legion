/** What the browser rows of dispatch://INFRA-299 ("picker keeps the newest pick", "picker keeps a
 *  pick on screen", "a pick that is late and still being tried") hold, log and read: each save of
 *  an issue held by Playwright's route control with the window it was held in, or answered `503`
 *  until the row lets it through, with the shell's Stop trying for a pick gone late; each read of
 *  a list or of the issue with when it was sent and answered, what the server stored from the
 *  issue's own events, every value a control or a table's cell shows at every animation frame,
 *  and the issue page's own picker, with the controls that reach it and the Inbox drawer on either
 *  issue page: the one with the sidebar, which holds Peek, and whose `main` holds every picker,
 *  and that of dispatch://INFRA-1196, whose icon rail opens the navigation that holds Peek and
 *  whose Details holds the repository. The rows live in the spec files of the pages they drive
 *  (`project.e2e.ts`, `board.e2e.ts`, `issues-table.e2e.ts`, `inbox-drawer.e2e.ts`,
 *  `inbox-reading-pane.e2e.ts`); this module holds no row. */
import { setTimeout as sleep } from "node:timers/promises";

import { expect, type Locator, type Page, type Request } from "@playwright/test";

import type { IssueField } from "../web/src/features/issue/issue-field-writes";
import { getIssueEvents } from "./api";
import { drawer, peekTrigger } from "./inbox-drawer";
import { keptWrites, pendingWrites } from "./keyed-writes";
import { settledState } from "./settled";

/** At least how long `holdIssuePatches` holds each save: long enough for a row to make its next
 *  pick, reload, or have a read sent and answered while the save is out, as a slow network would. */
export const PICK_HOLD_MS = 3_000;

/** One save (`PATCH /issues/<KEY>`) as the route held it: what it carried, when it reached the
 *  route, and when the route was done with it - passed on to the server, or dropped because the
 *  page gave it up (`Date.now()` in the runner). `endedAt` is unset while it is held. */
export interface HeldPatch {
  readonly arrivedAt: number;
  readonly body: unknown;
  endedAt?: number;
}

/** The saves of one issue a page makes, as `holdIssuePatches` holds them. */
export interface HeldPatches {
  /** Each save the page sent, in the order the route was given them. */
  readonly sent: HeldPatch[];
  /** The body of each save the route let through to the server, in the order it let them. */
  readonly forwarded: unknown[];
  /** How many saves the route holds now. */
  readonly holding: () => number;
  /** Lets each save held now, and each the page makes after, through to the server once it has
   *  been held `PICK_HOLD_MS`. */
  readonly release: () => void;
}

/** Sleeps until `Date.now()` reads at least `until`, the clock `HeldPatch` records. A timer of
 *  `ms` can end before `Date.now()` has moved `ms`, for two causes. The first is the timer: Node's
 *  ends up to 1 ms early. A probe on the session image
 *  (dispatch://INFRA-983/comment/74b7abbc-95c8-4815-99b6-8b4d37d273c9) measured Node 24's
 *  `sleep(3000)` from `node:timers/promises` ending 2,999 ms later by `Date.now()` in 13 of 200
 *  runs, and the shortest lasted 2,999.250 ms by `performance.now()`, so the timer ended early on
 *  the monotonic clock too; Bun 1.4.4 ended none of 200 early. The second is the clock: the timer
 *  counts on the monotonic clock, and `Date.now()` is the wall clock, which a runner may slew or
 *  step against it. Merge-group runs 44424277368, 44632529216 and 44650607607 read holds of
 *  2,999 ms. So the rest is slept again until the clock the rows read says the hold is whole. */
async function sleepUntil(until: number): Promise<void> {
  for (let left = until - Date.now(); left > 0; left = until - Date.now()) {
    // The rest of a save's hold, measured on the wall clock the rows read the hold with.
    await sleep(left);
  }
}

/**
 * Holds every save of `issueKey` the page makes - a pick, a board move, a key of the issue page -
 * with Playwright's route control, each for at least `PICK_HOLD_MS` and until the row lets the
 * saves go (`release`), then passes it on to the server. The write store sends the writes of one
 * issue one at a time (`web/src/features/writes/write-store.ts`, one line per `issue:<KEY>`), so
 * a held save keeps every save behind it off the wire. A save still held when its document
 * reloads is passed on all the same, as the browser sent it: the reloaded page sends it again
 * under the same `Idempotency-Key`, and the server applies the key once.
 */
export async function holdIssuePatches(page: Page, issueKey: string): Promise<HeldPatches> {
  const path = `/api/v1/issues/${issueKey}`;
  const released = Promise.withResolvers<void>();
  const sent: HeldPatch[] = [];
  const forwarded: unknown[] = [];
  let holding = 0;
  await page.route(`**${path}`, async (route) => {
    const request = route.request();
    if (request.method() !== "PATCH") return route.fallback();
    holding += 1;
    const patch: HeldPatch = { arrivedAt: Date.now(), body: request.postDataJSON() };
    sent.push(patch);
    try {
      // The hold is the row's whole point: the save stays out at least this long, and until the
      // row lets it go, so the next pick, reload or read happens while it is out.
      await Promise.all([sleepUntil(patch.arrivedAt + PICK_HOLD_MS), released.promise]);
      await route.fallback();
      forwarded.push(patch.body);
    } catch {
      // The page closed with the save held: it never left.
    } finally {
      patch.endedAt = Date.now();
      holding -= 1;
    }
  });
  return { forwarded, holding: () => holding, release: () => released.resolve(), sent };
}

/** Waits until no save of `held` is held and the tab keeps no write for a reload to send: every
 *  save the page made has ended. */
export async function savesEnded(page: Page, held: HeldPatches): Promise<void> {
  await expect
    .poll(async () => held.holding() === 0 && (await keptWrites(page)) === 0, { timeout: 30_000 })
    .toBe(true);
}

/** The saves of one issue a page makes while `refuseIssuePatches` answers them. */
export interface RefusedPatches {
  /** When the route answered each attempt it refused, in order (`Date.now()` in the runner). */
  readonly refused: number[];
  /** The body of each save the route let through to the server, in the order it let them. */
  readonly forwarded: unknown[];
  /** Lets each save the page makes from now on through to the server. */
  readonly admit: () => void;
}

/**
 * Answers every save of `issueKey` the page makes as Dispatch answers one its database is too busy
 * to take, `503 DATABASE_BUSY` with `Retry-After: 1`, until the row lets the saves through
 * (`admit`), then passes each on to the server. The write store makes a save so answered again
 * under its key once that second is out (`busyRetryAfterMs`, `web/src/api/client.ts`, a wait
 * `web/src/features/writes/keyed-write.ts` keeps), so a pick is still being tried when it passes
 * `SEND_DEADLINE_MS` and goes late, and its first attempt after `admit` reaches the server about
 * a second later.
 */
export async function refuseIssuePatches(page: Page, issueKey: string): Promise<RefusedPatches> {
  const refused: number[] = [];
  const forwarded: unknown[] = [];
  let refusing = true;
  await page.route(`**/api/v1/issues/${issueKey}`, async (route) => {
    const request = route.request();
    if (request.method() !== "PATCH") return route.fallback();
    if (refusing) {
      refused.push(Date.now());
      return route.fulfill({
        headers: { "Retry-After": "1" },
        json: { code: "DATABASE_BUSY", error: "the database is too busy to take this request" },
        status: 503,
      });
    }
    await route.fallback();
    forwarded.push(request.postDataJSON());
  });
  const admit = () => {
    refusing = false;
  };
  return { admit, forwarded, refused };
}

/** The Stop trying the shell's Pending changes offers for the write it names `label` (a pick's
 *  is `CORE-1 set to P1`, `pickLabel`): drawn while that write is late, past `SEND_DEADLINE_MS`
 *  and still being tried, and gone once it ends (`web/src/features/writes/
 *  PendingWritesIndicator.tsx`). */
export function stopTrying(page: Page, label: string): Locator {
  return pendingWrites(page)
    .getByRole("listitem")
    .filter({ hasText: label })
    .getByRole("button", { name: "Stop trying", exact: true });
}

/** One read the page made, with when it was sent and when its answer's body landed
 *  (`Date.now()` in the runner); `answeredAt` is unset until it has. */
export interface LoggedRead {
  readonly url: string;
  readonly sentAt: number;
  answeredAt?: number;
}

/** Logs every `GET` the page makes whose URL `matches` - the issue's detail, a project's list, the
 *  work under a priority, the Inbox - with when it was sent and when it was answered. */
export function logReads(page: Page, matches: (url: URL) => boolean): LoggedRead[] {
  const reads: LoggedRead[] = [];
  const byRequest = new Map<Request, LoggedRead>();
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() !== "GET" || !matches(url)) return;
    const read: LoggedRead = { sentAt: Date.now(), url: `${url.pathname}${url.search}` };
    reads.push(read);
    byRequest.set(request, read);
  });
  page.on("requestfinished", (request) => {
    const read = byRequest.get(request);
    if (read !== undefined) read.answeredAt = Date.now();
  });
  return reads;
}

/** The read of one issue's detail, `GET /api/v1/issues/<KEY>`. */
export function detailRead(issueKey: string): (url: URL) => boolean {
  return (url) => url.pathname === `/api/v1/issues/${issueKey}`;
}

/** A read of a list of issues, `GET /api/v1/issues?<param>=<value>`: a project's list
 *  (`project=CORE`) or the work under a priority (`advances=<id>`). */
export function listRead(param: string, value: string): (url: URL) => boolean {
  return (url) => url.pathname === "/api/v1/issues" && url.searchParams.get(param) === value;
}

/** The Inbox's read, `GET /api/v1/inbox`. */
export const inboxRead = (url: URL): boolean => url.pathname === "/api/v1/inbox";

/** Whether one of `reads` was sent at or after `since` and has been answered. */
export function readAnsweredSince(reads: readonly LoggedRead[], since: number): boolean {
  return reads.some((read) => read.sentAt >= since && read.answeredAt !== undefined);
}

/**
 * Asserts that one of `reads` was sent at or after `since` - the write that starts it - and was
 * both sent and answered inside the window one of `held`'s saves was held in: the read whose
 * answer a row checks the screen against landed while the save was out, so the server's answer
 * still carried the value from before the pick. Call it once every save has ended.
 */
export function expectReadWhileHeld(
  reads: readonly LoggedRead[],
  held: HeldPatches,
  since: number
): void {
  const inWindow = reads.some(
    (read) =>
      read.sentAt >= since &&
      read.answeredAt !== undefined &&
      held.sent.some(
        (patch) =>
          patch.arrivedAt <= read.sentAt &&
          patch.endedAt !== undefined &&
          read.answeredAt !== undefined &&
          read.answeredAt <= patch.endedAt
      )
  );
  const timeline = {
    reads: reads.map((read) => ({ ...read, answeredAt: read.answeredAt ?? null })),
    saves: held.sent.map((patch) => ({ ...patch, endedAt: patch.endedAt ?? null })),
    since,
  };
  expect(
    inWindow,
    `a read sent after ${since} was sent and answered while a save was held: ${JSON.stringify(timeline)}`
  ).toBe(true);
}

/** Where the issue page draws its own pickers, as a CSS selector: `main`, and Details, the drawer
 *  beside `main` in which the issue page of dispatch://INFRA-1196 keeps the repository. The Inbox
 *  drawer draws outside both. */
export const ISSUE_PAGE_PICKERS = ':is(main, aside[aria-label="Details"])';

/** The issue page's own picker named `name` (an `aria-label`, such as `Priority of CORE-1`),
 *  inside `ISSUE_PAGE_PICKERS`. */
export function pagePicker(page: Page, name: string): Locator {
  return page.locator(ISSUE_PAGE_PICKERS).getByRole("combobox", { name });
}

/**
 * Brings the issue page's own picker named `name` into view, as a reader does: the issue page
 * with the sidebar draws it in `main`; that of dispatch://INFRA-1196 keeps the repository in
 * Details, which the Details control in the issue's header opens. Settles on which of the two
 * the page draws (`settledState`), the picker first, since a picker that shows needs no press
 * even where the Details control shows beside it; presses Details only where the control showed
 * and the picker did not, and returns once the picker is visible.
 */
export async function revealPagePicker(page: Page, name: string): Promise<void> {
  const picker = pagePicker(page, name);
  const details = page
    .locator("header[data-issue-header]")
    .getByRole("button", { exact: true, name: "Details" });
  const shown = await settledState(`the picker ${name}`, [
    ["picker", picker],
    ["Details", details],
  ]);
  if (shown === "Details") await details.click();
  await expect(picker).toBeVisible();
}

/**
 * Opens the Inbox drawer over the page through the control the page draws for it, as a reader
 * does: the sidebar's Peek beside its Inbox (`peekTrigger`); or, on the issue page of
 * dispatch://INFRA-1196, which draws the icon rail in place of the sidebar, the rail's Open
 * navigation, then the Peek in the navigation it opens over the page. Settles on which of the two
 * the page draws (`settledState`), the Peek first, since a Peek that shows needs no navigation
 * opened; presses Open navigation only where it showed and the Peek did not. Either way it is the
 * same drawer over the same page.
 */
export async function openInboxDrawer(page: Page): Promise<void> {
  const peek = peekTrigger(page);
  const openNavigation = page
    .getByRole("navigation", { name: "App" })
    .getByRole("button", { exact: true, name: "Open navigation" });
  const shown = await settledState("the control that opens the Inbox drawer", [
    ["Peek", peek],
    ["Open navigation", openNavigation],
  ]);
  if (shown === "Open navigation") await openNavigation.click();
  await peek.click();
  await expect(drawer(page)).toBeVisible();
}

/** The sequence of the newest event of `issueKey`: the events a row reads after it are the
 *  writes the row made. */
export async function lastEventSeq(issueKey: string): Promise<number> {
  const events = await getIssueEvents(issueKey);
  return Math.max(0, ...events.map((event) => Number(event.seq)));
}

/** What the server stored `field` of `issueKey` as, write by write, after event `afterSeq`: the
 *  value each of the issue's own `issue.updated` events that moved the field carries, in the
 *  server's order. Each is a save the server received and applied. */
export async function storedValues(
  issueKey: string,
  field: IssueField,
  afterSeq: number
): Promise<unknown[]> {
  const events = await getIssueEvents(issueKey);
  return events.flatMap((event) => {
    if (event.type !== "issue.updated" || Number(event.seq) <= afterSeq) return [];
    const payload: { changed?: string[] } & Partial<Record<IssueField, unknown>> = event.payload;
    return payload.changed?.includes(field) === true ? [payload[field]] : [];
  });
}

/** What a page has shown under one accessible name since `watchShown` began. */
export interface Shown {
  /** Each value shown, in order, one entry per change: the value of every control of that name,
   *  joined with `|`, and `(none)` while none is drawn. */
  readonly values: string[];
}

let watchedNames = 0;

/** Exposes to every document `page` loads a binding of its own, which records each value it is
 *  given that differs from the one before, and returns its name and what it has recorded. */
async function exposeShown(page: Page): Promise<{ binding: string; shown: Shown }> {
  const values: string[] = [];
  watchedNames += 1;
  const binding = `__dispatchShown${watchedNames}`;
  await page.exposeBinding(binding, (_source, value: string) => {
    if (values.at(-1) !== value) values.push(value);
  });
  return { binding, shown: { values } };
}

/**
 * Records every value the controls named `name` (an `aria-label`, such as `Priority of CORE-1`)
 * show in `page`, read at every animation frame of every document the page loads from now on -
 * so from the first frame a reload paints. A `<select>` or an `<input>` shows its value - an
 * input's is what is typed into it as well as what it holds; any other element its text.
 * `within`, a CSS selector, reads only the controls inside what it matches - the page's `main`,
 * `main` and Details (`ISSUE_PAGE_PICKERS`), or the Inbox drawer
 * (`[role="dialog"][aria-label="Inbox"]`). Call it before the page loads the document a row
 * watches.
 */
export async function watchShown(page: Page, name: string, within = ":root"): Promise<Shown> {
  const { binding, shown } = await exposeShown(page);
  await page.addInitScript(
    ({ binding, name, within }: { binding: string; name: string; within: string }) => {
      const report = Reflect.get(window, binding) as (value: string) => Promise<void>;
      const selector = `${within} [aria-label="${CSS.escape(name)}"]`;
      let last: string | undefined;
      const sample = () => {
        const controls = [...document.querySelectorAll(selector)];
        const value =
          controls.length === 0
            ? "(none)"
            : controls
                .map((element) =>
                  element instanceof HTMLSelectElement || element instanceof HTMLInputElement
                    ? element.value
                    : (element.textContent ?? "")
                )
                .join("|");
        if (value !== last) {
          last = value;
          void report(value);
        }
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    },
    { binding, name, within }
  );
  return shown;
}

/**
 * Records every text the cell of the column headed `column` (such as `Assignee`) shows in the row
 * of `issueKey` of an issues table (`[data-issue-row="<KEY>"]`), read at every animation frame of
 * every document the page loads from now on, as `watchShown` reads a control: for a field a table
 * draws as plain text, with no name of its own. The cell is the one under the header whose button
 * reads `column`, in the row's own table; `(none)` while no such row, or no such column, is
 * drawn. Call it before the page loads the document a row watches.
 */
export async function watchCell(page: Page, issueKey: string, column: string): Promise<Shown> {
  const { binding, shown } = await exposeShown(page);
  await page.addInitScript(
    ({ binding, column, issueKey }: { binding: string; column: string; issueKey: string }) => {
      const report = Reflect.get(window, binding) as (value: string) => Promise<void>;
      const selector = `tr[data-issue-row="${CSS.escape(issueKey)}"]`;
      let last: string | undefined;
      const read = (row: HTMLTableRowElement): string => {
        const headers = [...(row.closest("table")?.tHead?.rows[0]?.cells ?? [])];
        const index = headers.findIndex(
          (header) => header.querySelector("button")?.textContent?.trim() === column
        );
        return index < 0 ? "(none)" : (row.cells[index]?.textContent ?? "(none)");
      };
      const sample = () => {
        const rows = [...document.querySelectorAll<HTMLTableRowElement>(selector)];
        const value = rows.length === 0 ? "(none)" : rows.map(read).join("|");
        if (value !== last) {
          last = value;
          void report(value);
        }
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    },
    { binding, column, issueKey }
  );
  return shown;
}
