/** The seeded tmux layouts the machine pages are read against, and the measurements the rows
 *  make of them.
 *
 *  Two spec files share this: `machines.e2e.ts`, whose rows read a drawing against the clock,
 *  and `machines.stub.e2e.ts`, whose rows read only what the page draws from a fixed answer
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). The layout strings, the cells they name
 *  and the proportions each pane must hold live here so the two cannot drift apart.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import type { TmuxPlace } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";

/** A box of a tmux window, in terminal cells, as the layout string states it. The spec writes
 *  its own cells rather than reading the page's parser, so a drawing is measured against the
 *  seeded string and not against the code that drew it. */
export interface Cell {
  readonly height: number;
  /** Carrying its `%`, as `TmuxPlace.pane_id` and the drawing's `data-pane` both do. */
  readonly paneId: string;
  readonly width: number;
  readonly x: number;
  readonly y: number;
}

/** A window's own size in cells: what every pane's share is taken over. */
export interface WindowSize {
  readonly columns: number;
  readonly rows: number;
}

/** The three-pane window this design recorded from tmux 3.5a: `%0` over `%1` beside `%2`. tmux
 *  spends one cell on the divider and gives it to neither pane, so the two lower panes are 40
 *  and 39 wide in an 80-cell window. */
export const THREE_PANE_SIZE: WindowSize = { columns: 80, rows: 24 };
export const THREE_PANE_LAYOUT =
  "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";
export const THREE_PANES: readonly Cell[] = [
  { height: 12, paneId: "%0", width: 80, x: 0, y: 0 },
  { height: 11, paneId: "%1", width: 40, x: 0, y: 13 },
  { height: 11, paneId: "%2", width: 39, x: 41, y: 13 },
];

/** A window of four panes, two over two, in a 200x50 window. */
export const FOUR_PANE_SIZE: WindowSize = { columns: 200, rows: 50 };
export const FOUR_PANE_LAYOUT =
  "b7c1,200x50,0,0[200x24,0,0{100x24,0,0,0,99x24,101,0,1},200x25,0,25{100x25,0,25,2,99x25,101,25,3}]";
export const FOUR_PANES: readonly Cell[] = [
  { height: 24, paneId: "%0", width: 100, x: 0, y: 0 },
  { height: 24, paneId: "%1", width: 99, x: 101, y: 0 },
  { height: 25, paneId: "%2", width: 100, x: 0, y: 25 },
  { height: 25, paneId: "%3", width: 99, x: 101, y: 25 },
];

/** A window holding one pane, for a strip that needs a window more than it needs a drawing. */
export function onePaneLayout(paneNumber: number): string {
  return `b262,80x24,0,0,${paneNumber}`;
}

/**
 * The 22-pane window the width check seeds: a 203x64 window of four rows of panes, three rows of
 * six and one of four. A real production session names a pane `agents:3.21`, so a window of 22
 * panes is one the page meets. tmux spends one cell on each divider and gives it to neither
 * neighbour, so six panes across 203 columns are 33 wide with five dividers between them
 * (6 x 33 + 5 = 203) and four are 50 wide with three (4 x 50 + 3 = 203); the rows are 15, 15, 15
 * and 16 cells tall with a divider row after each of the first three (15 + 1 + 15 + 1 + 15 + 1 +
 * 16 = 64).
 *
 * Which number decides the fallback, in the spec's own terms. A pane box is drawn at the
 * layout's own percentages inside a box `W` pixels wide and `W x 2 x rows / columns` tall, so
 * the box is `width x W / columns` pixels wide and `height x 2 x W / columns` pixels tall. The
 * **96 px** bound on a pane box's width is the one that decides here: the narrowest box is
 * `33 x W / 203`, which reaches 96 px only once the drawing is 591 px wide. A 390 px phone can
 * never be that wide - its drawing is the 390 px viewport less the shell's padding - so every
 * pane there is too narrow and the window is listed; at 1440 px the drawing is over a thousand
 * pixels wide and the same box is about 174 px, so the window is drawn. The 42 px bound on a
 * box's height never decides: the shortest box is `15 x 2 x W / 203`, which is over 42 px from a
 * drawing 285 px wide on, so it is met at both widths.
 */
export const WIDE_SIZE: WindowSize = { columns: 203, rows: 64 };

/** Each row of that window: how many panes it holds, how tall it is, and where its top is. */
const WIDE_ROW_PLAN = [
  { height: 15, panes: 6, y: 0 },
  { height: 15, panes: 6, y: 16 },
  { height: 15, panes: 6, y: 32 },
  { height: 16, panes: 4, y: 48 },
] as const;

/** The layout string and the cells it names, built from one description so the string the test
 *  seeds and the proportions it asserts cannot drift apart. Pane ids run from `%100` in layout
 *  order, which is the order tmux writes them in. */
function wideWindow(): { readonly cells: readonly Cell[]; readonly layout: string } {
  const cells: Cell[] = [];
  const rows: string[] = [];
  let paneNumber = 100;
  for (const row of WIDE_ROW_PLAN) {
    const width = (WIDE_SIZE.columns - (row.panes - 1)) / row.panes;
    const written: string[] = [];
    for (let index = 0; index < row.panes; index += 1) {
      const x = index * (width + 1);
      cells.push({ height: row.height, paneId: `%${paneNumber}`, width, x, y: row.y });
      written.push(`${width}x${row.height},${x},${row.y},${paneNumber}`);
      paneNumber += 1;
    }
    rows.push(`${WIDE_SIZE.columns}x${row.height},0,${row.y}{${written.join(",")}}`);
  }
  return {
    cells,
    layout: `9f2a,${WIDE_SIZE.columns}x${WIDE_SIZE.rows},0,0[${rows.join(",")}]`,
  };
}

export const WIDE = wideWindow();

export function machineSession(
  sessionId: string,
  title: string,
  options: { dir?: string; host: string; lastSeen: number; place?: TmuxPlace }
): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: options.dir ?? "/workspaces/legion",
    host: options.host,
    last_seen: options.lastSeen,
    machine_id: options.host,
    roles: [],
    session_id: sessionId,
    title,
    tmux_place: options.place,
  };
}

export async function openMachine(page: Page, host: string): Promise<void> {
  await page.goto(`/machines/${encodeURIComponent(host)}`);
  await expect(page.getByRole("heading", { level: 1, name: host })).toBeVisible();
}

/**
 * Every drawn pane box of `drawing` (one `window-drawing` section), as a share of that
 * drawing's own box, against the share the seeded layout gives it - the check's "within one
 * percent", read as one percent of the expected share. Each pane sits at the layout's own
 * percentages with no border, padding or margin on the measured box, so what is compared is the
 * geometry and nothing else. The boxes are asserted to be in the layout's own order first, so a
 * drawing that holds the right rectangles in the wrong order fails here rather than passing.
 */
export async function assertPaneProportions(
  drawing: Locator,
  cells: readonly Cell[],
  size: WindowSize
): Promise<void> {
  const whole = await drawing.locator('[data-testid="window-layout"]').boundingBox();
  if (whole === null) throw new Error("the drawing has no box");
  const boxes = drawing.locator('[data-testid="window-pane"]');
  expect(await boxes.evaluateAll((panes) => panes.map((pane) => pane.dataset.pane))).toEqual(
    cells.map((cell) => cell.paneId)
  );
  for (const cell of cells) {
    const box = await drawing
      .locator(`[data-testid="window-pane"][data-pane="${cell.paneId}"]`)
      .boundingBox();
    if (box === null) throw new Error(`pane ${cell.paneId} has no box`);
    const widthShare = cell.width / size.columns;
    const heightShare = cell.height / size.rows;
    expect(box.width / whole.width, `pane ${cell.paneId} width share`).toBeGreaterThanOrEqual(
      widthShare * 0.99
    );
    expect(box.width / whole.width, `pane ${cell.paneId} width share`).toBeLessThanOrEqual(
      widthShare * 1.01
    );
    expect(box.height / whole.height, `pane ${cell.paneId} height share`).toBeGreaterThanOrEqual(
      heightShare * 0.99
    );
    expect(box.height / whole.height, `pane ${cell.paneId} height share`).toBeLessThanOrEqual(
      heightShare * 1.01
    );
  }
}

// ---------------------------------------------------------------------------------------------
// k5: the machine page draws each tmux server's windows and panes.
// ---------------------------------------------------------------------------------------------

export const DRAW_HOST = "sami-devbox";
/** The parts are in socket order, so the person's socket is the first part and the Legion
 *  daemon's private one the second. */
export const PERSON_SOCKET = "/tmp/tmux-1000/default";
export const DAEMON_SOCKET = "/var/run/legion/tmux-daemon.sock";

export const drawIds = {
  builder: "019a1c22-7e5c-7f9b-8a4a-5b2c6d9e0f22",
  builds: "019a1c25-ab8f-7c2e-9d7d-8e5f9a2b3c25",
  daemon: "019a1c26-bc9a-7d3f-ae8e-9f6a0b3c4d26",
  outside: "019a1c27-cdab-7e40-bf9f-0a7b1c4d5e27",
  planner: "019a1c21-6d4b-7e8a-bf39-4a1b5c8d9e21",
  stray: "019a1c24-9a7e-7b1d-8c6c-7d4e8f1a2b24",
  tester: "019a1c23-8f6d-7a0c-9b5b-6c3d7e0f1a23",
} as const;

/** The place every seeded machine is drawn from: the first pane of the first window of the
 *  person's tmux server. Each case names what it is about and keeps the rest. */
export function tmuxPlace(overrides: Partial<TmuxPlace> = {}): TmuxPlace {
  return {
    layout: FOUR_PANE_LAYOUT,
    pane_id: "%0",
    pane_index: 0,
    server_pid: "4242",
    session_name: "main",
    socket: PERSON_SOCKET,
    window_id: "@1",
    window_index: 1,
    window_name: "agents",
    zoomed: false,
    ...overrides,
  };
}

/**
 * One machine with two tmux servers: the person's socket, holding one tmux session of two
 * windows, and the Legion daemon's private socket, holding one. The first window's layout names
 * four panes and three of them hold live sessions; a fourth session names the pane `%2` already
 * holds, from an older heartbeat, so the page shows the freshest and counts the other. One more
 * session on the machine is in no pane at all, so the machine's two counts differ.
 */
export function drawnMachineSessions(now: number): FakeSession[] {
  return [
    machineSession(drawIds.planner, "Planner", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({ pane_id: "%0", pane_index: 0 }),
    }),
    machineSession(drawIds.builder, "Builder", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({ pane_id: "%1", pane_index: 1 }),
    }),
    machineSession(drawIds.tester, "Tester", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({ pane_id: "%2", pane_index: 2 }),
    }),
    // Heard from two minutes before the Tester, and naming the Tester's pane: one of the two
    // moved or ended without the registry hearing of it.
    machineSession(drawIds.stray, "Stray", {
      host: DRAW_HOST,
      lastSeen: now - 150_000,
      place: tmuxPlace({ pane_id: "%2", pane_index: 2 }),
    }),
    machineSession(drawIds.builds, "Builds", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(4),
        pane_id: "%4",
        pane_index: 0,
        window_id: "@2",
        window_index: 2,
        window_name: "builds",
      }),
    }),
    machineSession(drawIds.daemon, "Legion daemon", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: {
        layout: onePaneLayout(9),
        pane_id: "%9",
        pane_index: 0,
        server_pid: "9001",
        session_name: "legion",
        socket: DAEMON_SOCKET,
        window_id: "@7",
        window_index: 0,
        window_name: "daemon",
        zoomed: false,
      },
    }),
    // In Kubernetes rather than tmux: on the machine, in no pane.
    machineSession(drawIds.outside, "Archivist", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
    }),
  ];
}

/** That machine and nothing else. */
export async function seedDrawnMachine(now: number): Promise<void> {
  await setLiveSessions(drawnMachineSessions(now));
}

// ---------------------------------------------------------------------------------------------
// k7: the page says what it does not know.
// ---------------------------------------------------------------------------------------------

export const honestyIds = {
  drawn: "019a1c33-8f6d-7a0c-9b5b-6c3d7e0f1a33",
  elsewhere: "019a1c35-ab8f-7c2e-9d7d-8e5f9a2b3c35",
  here: "019a1c34-9a7e-7b1d-8c6c-7d4e8f1a2b34",
  kubeOne: "019a1c31-6d4b-7e8a-bf39-4a1b5c8d9e31",
  kubeTwo: "019a1c32-7e5c-7f9b-8a4a-5b2c6d9e0f32",
  /** The second session of the machine whose name several sessions share. It is its own id
   *  rather than `elsewhere` because one seeding holds that machine and the machine whose
   *  sessions are all outside tmux at once (`machines.stub.e2e.ts`), and a session is on one
   *  machine. */
  reviewer: "019a1c36-bc9a-7d3f-ae8e-9f6a0b3c4d36",
};

/** A single-pane place on a tmux server of its own, for a machine the page has to draw before
 *  it can say how old its drawing is. */
export function lonePlace(paneNumber: number): TmuxPlace {
  return tmuxPlace({
    layout: onePaneLayout(paneNumber),
    pane_id: `%${paneNumber}`,
    server_pid: "7700",
    session_name: "work",
  });
}

// ---------------------------------------------------------------------------------------------
// k9: the drawing holds its proportions at a laptop width and a phone width.
// ---------------------------------------------------------------------------------------------

export const WIDTH_HOST = "sami-laptop";

export const widthIds = {
  agentsFirst: "019a1c53-8f6d-7a0c-9b5b-6c3d7e0f1a53",
  agentsLast: "019a1c55-ab8f-7c2e-9d7d-8e5f9a2b3c55",
  agentsSixth: "019a1c54-9a7e-7b1d-8c6c-7d4e8f1a2b54",
  deploy: "019a1c52-7e5c-7f9b-8a4a-5b2c6d9e0f52",
  review: "019a1c56-bc9a-7d3f-ae8e-9f6a0b3c4d56",
  writerOne: "019a1c51-6d4b-7e8a-bf39-4a1b5c8d9e51",
  writerThree: "019a1c57-cdab-7e40-bf9f-0a7b1c4d5e57",
  writerTwo: "019a1c58-dbac-7f51-8a0a-1b8c2d5e6f58",
};

/** A place on the machine the width cases draw: one tmux server, one tmux session, windows the
 *  caller names. */
function widthPlace(overrides: Partial<TmuxPlace>): TmuxPlace {
  return tmuxPlace({
    layout: THREE_PANE_LAYOUT,
    server_pid: "5150",
    session_name: "work",
    window_name: "release-notes",
    ...overrides,
  });
}

/**
 * One tmux server of four windows, so the strip has more picks than a phone's width holds and
 * fewer than a laptop's. Window 1 is the three-pane window, drawn at both widths; window 3 is
 * the 22-pane window, drawn at 1440 px and listed at 390 px; windows 2 and 4 hold one pane each
 * and are there for the strip.
 */
export function widthMachineSessions(now: number): FakeSession[] {
  const lastSeen = now - 30_000;
  return [
    machineSession(widthIds.writerOne, "Release writer", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({ pane_id: "%0", pane_index: 0 }),
    }),
    machineSession(widthIds.writerTwo, "Release reviewer", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({ pane_id: "%1", pane_index: 1 }),
    }),
    machineSession(widthIds.writerThree, "Release tester", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({ pane_id: "%2", pane_index: 2 }),
    }),
    machineSession(widthIds.deploy, "Deploy watcher", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: onePaneLayout(20),
        pane_id: "%20",
        pane_index: 0,
        window_id: "@2",
        window_index: 2,
        window_name: "deploy-watch",
      }),
    }),
    // `agents:3.0`, `agents:3.5` and `agents:3.21` of the 22-pane window.
    machineSession(widthIds.agentsFirst, "Agent one", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: WIDE.layout,
        pane_id: "%100",
        pane_index: 0,
        window_id: "@3",
        window_index: 3,
        window_name: "agents",
      }),
    }),
    machineSession(widthIds.agentsSixth, "Agent six", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: WIDE.layout,
        pane_id: "%105",
        pane_index: 5,
        window_id: "@3",
        window_index: 3,
        window_name: "agents",
      }),
    }),
    machineSession(widthIds.agentsLast, "Agent twenty-two", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: WIDE.layout,
        pane_id: "%121",
        pane_index: 21,
        window_id: "@3",
        window_index: 3,
        window_name: "agents",
      }),
    }),
    machineSession(widthIds.review, "Layout reviewer", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: onePaneLayout(30),
        pane_id: "%30",
        pane_index: 0,
        window_id: "@4",
        window_index: 4,
        window_name: "layout-review",
      }),
    }),
  ];
}

/** That machine and nothing else. */
export async function seedWidthMachine(now: number): Promise<void> {
  await setLiveSessions(widthMachineSessions(now));
}

/** The window strip, and whether it scrolls sideways inside itself. */
export async function stripScrolls(page: Page): Promise<boolean> {
  return await page
    .locator('[data-testid="window-strip"]')
    .evaluate((strip) => strip.scrollWidth > strip.clientWidth + 0.5);
}

/** The picks of the strip all sit on one line. */
export async function stripIsOneRow(page: Page): Promise<boolean> {
  return await page
    .locator('[data-testid="window-pick"]')
    .evaluateAll((picks) =>
      picks.every(
        (pick) =>
          Math.abs(pick.getBoundingClientRect().top - picks[0].getBoundingClientRect().top) < 1
      )
    );
}
