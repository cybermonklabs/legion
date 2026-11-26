// What the rows of the Issues tab's views share (dispatch://INFRA-3221, Verification k2 to k10,
// k12 to k14 and k19): ACME as the rows seed it, the live socket's board frames held, changed or
// counted with `page.routeWebSocket`, the requests a page makes, the tabs, the rows, a column's
// filter and the Board's columns as the page draws them, Open's Queue order, an issue's Legion
// facts and the words the page writes a span or hours in, the lines of Time per phase's tooltip,
// a saved view, and a whole-card drag on the Board, whose two waits `board.e2e.ts` uses too.
//
// ACME is seeded with `seedLegionFixture`, its daemon named at a `startFakeLegionDaemon`, and
// with `seedLegionMetricsCorpus`. The fake daemon answers 404 to every route, so every pass of
// the sampler records a failed read and sends each open board a new frame; a row that measures a
// position seeds a failed read first and holds every board frame after the first
// (`seedMeasuredAcme`).
import type {
  LegionBoardResponse as Board,
  LegionBoardCard as Card,
  EvalItemRow,
  IssueSummary,
} from "../web/src/api/types";
import {
  type FakeLegionDaemon,
  LEGION_PROJECT,
  seedFailedLegionRead,
  seedLegionFixture,
  seedLegionMetricsCorpus,
  startFakeLegionDaemon,
} from "./legion-seed";
import { settledState } from "./settled";
import { expect, type Locator, type Page, type WebSocketRoute } from "./test";

export const ACME = LEGION_PROJECT;

/** `cols=` with every column the table has. */
export const EVERY_COL =
  "priority,queue,key,title,status,legion_phase,assignee,author,labels,claim,asks,created,updated,closed,roles,activity,waited,waiting_reason,time_in_phase,work_hours,review_rounds,legion,phase_hours";

/** ACME as every ACME row seeds it: its five issues with their cards, the closed tree of the
 *  metrics corpus, and its daemon named at a fake that answers 404. */
export async function seedAcme(): Promise<FakeLegionDaemon> {
  const daemon = await startFakeLegionDaemon();
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  return daemon;
}

/** What a frame of a Legion board subscription carries that a row rewrites: a snapshot's body,
 *  and a delta's columns and its entries of the `issues` map. */
export interface BoardParts {
  columns?: Board["columns"];
  issues?: Record<string, unknown> | null;
}

/** A frame of the live socket as it reaches the page: its type, its subscription, and a board
 *  snapshot's body or a delta's fields and map entries. */
export interface BoardFrame {
  readonly type?: string;
  readonly sub?: string;
  readonly body?: Board;
  readonly set?: BoardParts;
  readonly maps?: BoardParts;
}

/** How `routeLive` treats the board frames of the page's live sockets. */
export interface LiveRules {
  /** Rewrites each board frame before the page reads it. */
  readonly edit?: (frame: BoardFrame) => void;
  /** Drops each board frame after a subscription's first. */
  readonly hold?: boolean;
  /** Holds each subscription's first board frame until the promise this answers settles. It is
   *  called when that frame arrives, so a hold it starts is measured from the frame. */
  readonly first?: () => Promise<unknown>;
  /** Drops every frame of a socket, board or not, once its first board snapshot has passed:
   *  the socket stays open and nothing more reaches the page. A socket opened again is read
   *  the same way, so a reconnect brings one snapshot and no more. */
  readonly block?: boolean;
  /** Called with every frame the server sends, whether it passes or not. */
  readonly seen?: (frame: BoardFrame, board: boolean) => void;
}

/** Passes every frame of the page's live sockets on, under `rules`, where it is a frame of a
 *  Legion board subscription. A frame of another kind passes as it came, unless `block` holds
 *  the socket. */
export async function routeLive(page: Page, rules: LiveRules): Promise<void> {
  await page.routeWebSocket(/\/ws\/live$/u, (route: WebSocketRoute) => {
    const server = route.connectToServer();
    const boards = new Set<string>();
    const answered = new Set<string>();
    let blocked = false;
    // Frames reach the page in the order the server sent them, a held first snapshot included.
    let delivered = Promise.resolve();
    route.onMessage((message) => {
      const frame = JSON.parse(String(message)) as {
        readonly type?: string;
        readonly sub?: string;
        readonly view?: { readonly kind?: string };
      };
      if (frame.type === "subscribe" && frame.view?.kind === "legion" && frame.sub !== undefined)
        boards.add(frame.sub);
      server.send(message);
    });
    const deliver = async (message: string | Buffer) => {
      const frame = JSON.parse(String(message)) as BoardFrame;
      const board =
        frame.sub !== undefined &&
        boards.has(frame.sub) &&
        (frame.type === "snapshot" || frame.type === "delta");
      rules.seen?.(frame, board);
      if (blocked) {
        // A held socket still answers its heartbeat and starts a new subscription, so the page
        // keeps it open; nothing that would change what the page draws passes.
        const opening =
          frame.sub !== undefined &&
          !answered.has(frame.sub) &&
          (frame.type === "events_start" || frame.type === "snapshot");
        if (frame.type === "heartbeat" || opening) {
          if (frame.sub !== undefined) answered.add(frame.sub);
          route.send(message);
        }
        return;
      }
      if (!board || frame.sub === undefined) {
        route.send(message);
        return;
      }
      const first = !answered.has(frame.sub);
      if (rules.hold === true && !first) return;
      answered.add(frame.sub);
      if (first && rules.first !== undefined) await rules.first();
      if (rules.edit === undefined) route.send(message);
      else {
        rules.edit(frame);
        route.send(JSON.stringify(frame));
      }
      if (rules.block === true) blocked = true;
    };
    server.onMessage((message) => {
      delivered = delivered.then(() => deliver(message));
    });
  });
}

/** ACME seeded for a row that measures where things draw: a failed read of its daemon first, so
 *  the alert under the tabs is there from the start, and every board frame after the first held. */
export async function seedMeasuredAcme(page: Page): Promise<FakeLegionDaemon> {
  const daemon = await seedAcme();
  await seedFailedLegionRead(ACME, "the daemon did not answer its state route");
  await routeLive(page, { hold: true });
  return daemon;
}

/** Waits until a page of ACME seeded by `seedMeasuredAcme` has drawn the board's first snapshot,
 *  whose header carries the failed read as the alert under the tabs. Until then the view's first
 *  draw is not whole (`viewKey` in `web/src/features/project/issues-tab-reads.ts`): the
 *  snapshot's commit starts the table at its top again (`useDataTable`) and the anchor holds
 *  nothing through it, so a row measures where things draw only after this. */
export async function boardDrawn(page: Page): Promise<void> {
  await expect(
    page.getByRole("alert").filter({ hasText: "The last read of the Legion daemon failed" })
  ).toBeVisible();
}

/** A tab's accessible name: the view's name, its count where it shows one, and its mark. */
export const tabName = (name: string, count?: number, changed = false) =>
  `${name}${count === undefined ? "" : `, ${count} ${count === 1 ? "issue" : "issues"}`}${changed ? ", changed" : ""}`;

/** The view tabs' accessible names, in their order. */
export function tabNames(page: Page): Promise<string[]> {
  return page
    .getByRole("tablist", { name: "Views" })
    .getByRole("tab")
    .evaluateAll((tabs) => tabs.map((tab) => tab.getAttribute("aria-label") ?? ""));
}

/** The view tabs' titles, in their order: each tab's name without its count and its mark. */
export async function tabTitles(page: Page): Promise<string[]> {
  return (await tabNames(page)).map((tab) => tab.split(",")[0]);
}

/** The count a tab's name gives, or undefined where it shows none. */
export async function tabCount(page: Page, name: string): Promise<number | undefined> {
  // This helper reads the tab names at one instant, by design. Each caller polls it or calls it
  // after a wait for the page.
  const named = (await tabNames(page)).find((tab) => tab === name || tab.startsWith(`${name},`));
  const count = named?.match(/^[^,]+, (\d+) issues?/)?.[1];
  return count === undefined ? undefined : Number(count);
}

/** The project table of the Issues tab. */
export const issuesTable = (page: Page): Locator =>
  page.getByRole("table", { name: "Project issues table" });

/** The keys of the rows `table` draws, in their order. */
export function rowKeys(table: Locator): Promise<string[]> {
  return table
    .locator("tbody tr[data-issue-row]")
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-issue-row") ?? ""));
}

/** Every card of the board by its issue, with the column that holds it and its label. */
export function cardsOf(board: Board): Map<string, { column: string; label: string; card: Card }> {
  return new Map(
    (board.columns ?? []).flatMap((column) =>
      (column.cards ?? []).map(
        (card) => [card.key, { column: column.id, label: column.label, card }] as const
      )
    )
  );
}

/** `issues` as Open's Queue sort orders them: the issues with a Place on `board` by their Place,
 *  then the rest by rank, ties in the listing's own order. */
export function queueSorted(issues: readonly IssueSummary[], board: Board): IssueSummary[] {
  const cards = cardsOf(board);
  const place = (issue: IssueSummary) => cards.get(issue.key)?.card.waiting_place ?? undefined;
  return [...issues].sort((a, b) => {
    // Both places come from the board snapshot in hand, never from the page, so they hold still.
    const [left, right] = [place(a), place(b)];
    if (left !== undefined && right !== undefined) return left - right;
    if (left !== undefined) return -1;
    if (right !== undefined) return 1;
    return a.rank < b.rank ? -1 : a.rank > b.rank ? 1 : 0;
  });
}

/** One issue's Legion facts as a row compares them, from the board's `issues` map or from an
 *  items row of `legion-work-time`. */
export interface Facts {
  readonly work_hours: number;
  readonly work_first_at: string | null;
  readonly review_rounds: number;
  readonly tree_depth: number;
  readonly root_key: string;
  readonly hours_by_phase: Readonly<Record<string, number>>;
}

/** An items row's facts, read from its columns as the route serves them. */
export function itemFacts(row: EvalItemRow): Facts {
  const cols = row.cols ?? {};
  const hours: Record<string, number> = {};
  for (const [name, value] of Object.entries(cols))
    if (name.startsWith("hours_") && typeof value === "number") hours[name.slice(6)] = value;
  return {
    work_hours: Number(cols.work_hours),
    work_first_at: typeof cols.work_first_at === "string" ? cols.work_first_at : null,
    review_rounds: Number(cols.review_rounds),
    tree_depth: Number(cols.tree_depth),
    root_key: String(cols.root_key),
    hours_by_phase: hours,
  };
}

/** The facts the board's `issues` map holds for `key`, or undefined where it holds none. */
export function boardFacts(board: Board, key: string): Facts | undefined {
  const held = board.issues?.[key];
  return held === undefined ? undefined : { ...held, hours_by_phase: held.hours_by_phase ?? {} };
}

/** The work time a facts row measured, or undefined where none was: no facts, or `work_first_at`
 *  null with no hours. */
export const measuredWork = (facts: Facts | undefined): number | undefined =>
  facts === undefined || (facts.work_first_at === null && !(facts.work_hours > 0))
    ? undefined
    : facts.work_hours;

/** A span of whole minutes in the page's words: minutes under an hour, hours and minutes under a
 *  day, then days and hours. */
export function spanWords(minutes: number): string {
  if (minutes < 60) return `${minutes} m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ${minutes % 60} m`;
  return `${Math.floor(hours / 24)} d ${hours % 24} h`;
}

/** The Work time words of a span of `hours`: its whole minutes, none under zero, as `spanWords`
 *  writes them. */
export const workWords = (hours: number): string => spanWords(Math.floor(Math.max(0, hours) * 60));

/** Hours as Time per phase writes them: one decimal under 10 hours, whole above. */
export const hoursWords = (hours: number): string =>
  `${hours < 10 ? hours.toFixed(1) : Math.round(hours)} h`;

/** The open tooltip's lines of Time per phase: each phase's name, hours, share and swatch fill. */
export function tooltipLines(
  page: Page
): Promise<{ phase: string; name: string; hours: string; share: string; swatch: string }[]> {
  return page.getByRole("tooltip").evaluate((tooltip) =>
    [...tooltip.querySelectorAll("[data-phase-hours-line]")].map((line) => {
      const parts = [...line.children].map((part) => (part.textContent ?? "").trim());
      const swatch = line.querySelector("[data-phase-swatch]");
      return {
        phase: line.getAttribute("data-phase-hours-line") ?? "",
        name: parts[1] ?? "",
        hours: parts[2] ?? "",
        share: parts[3] ?? "",
        swatch: swatch === null ? "" : getComputedStyle(swatch).backgroundColor,
      };
    })
  );
}

/** One filter's options as its list draws them: each option's words and count, in order. */
export interface FilterRead {
  readonly opens: "list" | "dialog";
  readonly options: readonly (readonly [string, number])[];
}

/** Opens column `name`'s filter from its header and reads it, then shuts it. A filter of picks
 *  opens a list of options, each named `<words>, <n> issue(s)`; Key, Title and Closed open a
 *  panel instead, which reads as no options. */
export async function readFilter(page: Page, name: string): Promise<FilterRead> {
  const trigger = page.getByRole("button", {
    name: new RegExp(`^Filter ${name}(?:: \\d+ picked)?$`),
  });
  await trigger.click();
  const list = page.getByRole("listbox", { name: `${name} options` });
  const panel = page.getByRole("dialog", { name: `Filter ${name}` });
  const opened = await settledState(`the ${name} filter`, [
    ["list", list],
    ["panel", panel],
  ]);
  if (opened === "panel") {
    await page.keyboard.press("Escape");
    await expect(panel).toHaveCount(0);
    return { opens: "dialog", options: [] };
  }
  const names = await list
    .getByRole("option")
    .evaluateAll((options) => options.map((option) => option.getAttribute("aria-label") ?? ""));
  await page.keyboard.press("Escape");
  await expect(list).toHaveCount(0);
  return {
    opens: "list",
    options: names.map((named) => {
      // An option whose rows are not read yet (Done on Open) says so in place of a count: -1.
      const match = named.match(/^(.*), (?:(\d+) issues?|not loaded)$/);
      return [match?.[1] ?? named, match === null ? Number.NaN : Number(match[2] ?? -1)] as const;
    }),
  };
}

/** One column or rail of the Board as it draws: its id, its title, whether it is a rail, the
 *  count its header draws (NaN where it draws none) and its cards in their order. */
export interface DrawnColumn {
  readonly id: string;
  readonly title: string;
  readonly rail: boolean;
  readonly count: number;
  readonly cards: readonly string[];
}

/** The Board's columns and rails, in their order. */
export function drawnColumns(page: Page): Promise<DrawnColumn[]> {
  return page
    .getByRole("region", { name: "Project board" })
    .locator("section[data-board-column]")
    .evaluateAll((sections) =>
      sections.map((section) => {
        const label = section.getAttribute("aria-label") ?? "";
        const rail = label.endsWith("(collapsed)");
        const head = section.querySelector(
          rail ? '[data-testid="board-column-rail"]' : '[data-testid="board-column-header"]'
        );
        // A column's header ends with its count; a rail's count stands before its name.
        const text = (head?.textContent ?? "").trim();
        const count = (rail ? text.match(/^(\d+)/) : text.match(/(\d+)$/))?.[1];
        return {
          id: section.getAttribute("data-board-column") ?? "",
          title: rail ? label.slice(0, -" (collapsed)".length) : label,
          rail,
          count: count === undefined ? Number.NaN : Number(count),
          cards: [...section.querySelectorAll("[data-board-card]")].map(
            (card) => card.getAttribute("data-board-card") ?? ""
          ),
        };
      })
    );
}

/** The count a Board column's header draws, NaN where it draws none. */
export async function columnCount(page: Page, id: string): Promise<number> {
  const header = page
    .locator(`section[data-board-column="${id}"]`)
    .getByTestId("board-column-header");
  return Number(((await header.textContent()) ?? "").match(/(\d+)\s*$/)?.[1] ?? Number.NaN);
}

/** Saves the view the page is on under `name` from the control row's Save view. */
export async function saveView(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { exact: true, name: "Save view" }).click();
  const panel = page.getByRole("dialog", { name: "Save view" });
  await panel.getByLabel("Name of the view").fill(name);
  await panel.getByRole("button", { exact: true, name: "Save" }).click();
  await expect(panel).toHaveCount(0);
  await expect(page.getByRole("tab", { name: new RegExp(`^${name}(?:,|$)`) })).toBeVisible();
}

/** One request a page made: its method, its address, its status and the bytes of its answer. */
export interface Recorded {
  readonly method: string;
  readonly url: URL;
  readonly status: number;
  readonly bytes: number;
}

/** Records every request to the API `page` makes from now on, with the size of its answer, in
 *  the order they finish; `clear` empties the list and `settled` waits for the answers still
 *  out. */
export function recordRequests(page: Page): {
  readonly list: Recorded[];
  readonly clear: () => void;
  readonly settled: () => Promise<void>;
} {
  const list: Recorded[] = [];
  const out = new Set<Promise<void>>();
  page.on("requestfinished", (request) => {
    const url = new URL(request.url());
    if (!url.pathname.startsWith("/api/")) return;
    const read = (async () => {
      const response = await request.response();
      const body = await response?.body().catch(() => undefined);
      list.push({
        method: request.method(),
        url,
        status: response?.status() ?? 0,
        bytes: body?.length ?? 0,
      });
    })();
    out.add(read);
    void read.finally(() => out.delete(read));
  });
  return {
    list,
    clear: () => {
      list.length = 0;
    },
    settled: async () => {
      await Promise.all([...out]);
    },
  };
}

/** The reads of `GET /api/v1/issues` among `requests` that name `project`. */
export const listingReads = (requests: readonly Recorded[], project: string): Recorded[] =>
  requests.filter(
    (request) =>
      request.method === "GET" &&
      request.url.pathname === "/api/v1/issues" &&
      request.url.searchParams.get("project") === project
  );

/** Waits until no card of the Board is mid-flight. dnd-kit lands every reorder it did not animate
 *  itself - the rollback and refetch after a rejected rank included - with a 200 ms transform
 *  transition, and a card later in the DOM paints over the ones it crosses, so a press during
 *  that flight lifts whichever card is passing the point rather than the one measured there. */
export async function boardAtRest(page: Page): Promise<void> {
  await page.getByRole("region", { name: "Project board" }).evaluate(async (board) => {
    for (;;) {
      const running = board.getAnimations({ subtree: true });
      if (running.length === 0) return;
      await Promise.allSettled(running.map((animation) => animation.finished));
    }
  });
}

/** Waits until a click reaches its target again. dnd-kit's pointer sensors stop every click at
 *  the document (capture phase) from the lift until a 50 ms timer after the drop, and a busy main
 *  thread runs that timer late, so a control clicked straight after a drop is silently ignored.
 *  The probe is a node under `body` outside the React root: only that suppressor can stop it. */
export async function clicksDelivered(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const probe = document.body.appendChild(document.createElement("div"));
    let delivered = false;
    probe.addEventListener("click", () => {
      delivered = true;
    });
    try {
      for (;;) {
        delivered = false;
        probe.dispatchEvent(new MouseEvent("click"));
        if (delivered) return;
        const turn = Promise.withResolvers<void>();
        setTimeout(turn.resolve, 0);
        await turn.promise;
      }
    } finally {
      probe.remove();
    }
  });
}

/** Drags a Board card with the mouse from its middle to `target` (a card or a column), `yOffset`
 *  px below the target's top where given, else to its middle. */
export async function mouseDrag(
  page: Page,
  source: Locator,
  target: Locator,
  yOffset?: number
): Promise<void> {
  await boardAtRest(page);
  await source.hover();
  // The hover above waited for the card to be visible and still, so its box is there.
  const from = await source.boundingBox();
  if (from === null) throw new Error("the card dragged is not drawn");
  await page.mouse.down();
  await page.mouse.move(from.x + from.width / 2 + 10, from.y + from.height / 2);
  await target.scrollIntoViewIfNeeded();
  // While a card is in flight, dnd-kit scrolls the Board sideways for as long as the pointer rests
  // in the outer fifth of the Board's box, and the drop takes the column under the pointer when
  // the button comes up. A Testing column drawn at the box's right edge scrolled 101 px under the
  // pointer during the move, so the drop could land in Needs review. The target's middle goes to
  // the box's middle first, as far as the box scrolls.
  await target.evaluate((node) => {
    const box = node.closest(
      '[data-testid="board-scroll-container"], [data-testid="board-lane-scroll-container"]'
    );
    if (box === null) return;
    const drawn = node.getBoundingClientRect();
    const edges = box.getBoundingClientRect();
    box.scrollLeft += drawn.left + drawn.width / 2 - (edges.left + edges.width / 2);
  });
  // The scrollIntoViewIfNeeded above waited for the target to be visible and still, so its box
  // is there.
  const to = await target.boundingBox();
  if (to === null) throw new Error("the place a card is dropped on is not drawn");
  await page.mouse.move(
    to.x + to.width / 2,
    yOffset === undefined ? to.y + to.height / 2 : to.y + yOffset,
    { steps: 24 }
  );
  await page.mouse.up();
  await clicksDelivered(page);
}
