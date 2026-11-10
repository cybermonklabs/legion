/** What the machine pages draw from one fixed answer: the servers, strips and pane boxes of a
 *  seeded tmux layout, the lines the page writes when it has nothing to draw, the addresses a
 *  window pick writes, and the reads the page makes. Every row here asserts the drawing and
 *  nothing the server decides, so each runs against a recording of `GET /api/v1/agents` rather
 *  than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer").
 *
 *  What is not here is what the clock decides: a pane's state and a drawing's age are read from
 *  `Date.now()` against the `last_seen` the registry reported, so a recording of either answers
 *  differently an hour after it was taken. Those rows, and the phone-width case the `iphone`
 *  project alone runs, stay in `machines.e2e.ts`.
 *
 *  Two seeds, because one of these rows counts the machines the list names. Every row but that
 *  one reads one machine's own page, so they share a seeding of five machines; the list row
 *  takes a seeding of one.
 */
import { expect, type Page } from "@playwright/test";

import type { TmuxPlace } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import {
  assertPaneProportions,
  DAEMON_SOCKET,
  DRAW_HOST,
  drawnMachineSessions,
  FOUR_PANE_LAYOUT,
  honestyIds,
  lonePlace,
  machineSession,
  onePaneLayout,
  openMachine,
  PERSON_SOCKET,
  stripIsOneRow,
  stripScrolls,
  THREE_PANE_SIZE,
  THREE_PANES,
  tmuxPlace,
  WIDE,
  WIDE_SIZE,
  WIDTH_HOST,
  widthMachineSessions,
} from "./machines";
import { stubbed } from "./stub/stub-test";

const CLUSTER_HOST = "cluster-pool";
const SHARED_NAME_HOST = "legion-infra";
const UNDRAWN_HOST = "example-host-split";
const REPEATED_HOST = "sami-twoservers";

/** The four-pane window above with its lower right pane split in two: the layout the session in
 *  the new pane `%7` reports, which the window is not drawn from while a fresher registration
 *  still carries the four-pane string. */
const FIVE_PANE_LAYOUT =
  "c8d2,200x50,0,0[200x24,0,0{100x24,0,0,0,99x24,101,0,1}," +
  "200x25,0,25{100x25,0,25,2,99x25,101,25[99x12,101,25,3,99x12,101,38,7]}]";

const undrawnIds = {
  builder: "019a1d11-0000-7000-8000-000000000002",
  planner: "019a1d11-0000-7000-8000-000000000001",
  reviewer: "019a1d11-0000-7000-8000-000000000004",
  split: "019a1d11-0000-7000-8000-000000000005",
  tester: "019a1d11-0000-7000-8000-000000000003",
} as const;

const repeatedIds = {
  builds: "019a1e11-0000-7000-8000-000000000004",
  daemon: "019a1e11-0000-7000-8000-000000000003",
  editor: "019a1e11-0000-7000-8000-000000000002",
  shell: "019a1e11-0000-7000-8000-000000000001",
} as const;

/**
 * A session in a pane the drawn layout does not name: the four-pane window with its lower right
 * pane split in two. Each session reports its own copy of the window's layout, read at its own
 * moment, and a window is drawn from the layout of the report heard from most recently that
 * carries one, so the Planner's four-pane string wins over the split session's five-pane one.
 */
function undrawnMachineSessions(now: number) {
  const place = (overrides: Partial<TmuxPlace>): TmuxPlace =>
    tmuxPlace({ layout: FOUR_PANE_LAYOUT, window_id: "@1", window_index: 1, ...overrides });
  return [
    machineSession(undrawnIds.planner, "Planner", {
      host: UNDRAWN_HOST,
      // The freshest report of this window, and its layout was read from the cache before the
      // split below, so it names four panes and not five.
      lastSeen: now,
      place: place({ pane_id: "%0", pane_index: 0 }),
    }),
    machineSession(undrawnIds.builder, "Builder", {
      host: UNDRAWN_HOST,
      lastSeen: now - 20_000,
      place: place({ pane_id: "%1", pane_index: 1 }),
    }),
    machineSession(undrawnIds.tester, "Tester", {
      host: UNDRAWN_HOST,
      lastSeen: now - 20_000,
      place: place({ pane_id: "%2", pane_index: 2 }),
    }),
    machineSession(undrawnIds.reviewer, "Reviewer", {
      host: UNDRAWN_HOST,
      lastSeen: now - 20_000,
      place: place({ pane_id: "%3", pane_index: 3 }),
    }),
    // The session in the pane the split made, heard from a moment before the Planner above and
    // reporting the layout that names its own pane.
    machineSession(undrawnIds.split, "Just split", {
      host: UNDRAWN_HOST,
      lastSeen: now - 1_000,
      place: place({
        layout: FIVE_PANE_LAYOUT,
        pane_id: "%7",
        pane_index: 4,
      }),
    }),
  ];
}

/**
 * One machine running two tmux servers, the person's own and a Legion daemon's, each holding a
 * window `@0` and a window `@1`: tmux numbers windows from `@0` on each server it starts, so
 * the ids repeat across the two and `@1` alone names no window on this machine. Each window
 * holds one pane, and each pane's number is its own, so which window was drawn is read off the
 * pane in it.
 */
function twoServerSessions(now: number) {
  return [
    machineSession(repeatedIds.shell, "Shell", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(0),
        pane_id: "%0",
        pane_index: 0,
        window_id: "@0",
        window_index: 0,
        window_name: "shell",
      }),
    }),
    machineSession(repeatedIds.editor, "Editor", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(1),
        pane_id: "%1",
        pane_index: 0,
        window_id: "@1",
        window_index: 1,
        window_name: "agents",
      }),
    }),
    machineSession(repeatedIds.daemon, "Legion daemon", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(9),
        pane_id: "%9",
        pane_index: 0,
        server_pid: "9001",
        session_name: "legion",
        socket: DAEMON_SOCKET,
        window_id: "@0",
        window_index: 0,
        window_name: "daemon",
      }),
    }),
    machineSession(repeatedIds.builds, "Builds", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(8),
        pane_id: "%8",
        pane_index: 0,
        server_pid: "9001",
        session_name: "legion",
        socket: DAEMON_SOCKET,
        window_id: "@1",
        window_index: 1,
        window_name: "builds",
      }),
    }),
  ];
}

/** The five machines every row below one reads: one whose sessions are all outside tmux, one
 *  whose name several directories share, the one the width case measures, the one holding a
 *  pane its drawn layout does not name, and the one running two tmux servers. */
const test = stubbed(async () => {
  const now = Date.now();
  await setLiveSessions([
    machineSession(honestyIds.kubeOne, "Runner one", {
      host: CLUSTER_HOST,
      lastSeen: now - 30_000,
    }),
    machineSession(honestyIds.kubeTwo, "Runner two", {
      host: CLUSTER_HOST,
      lastSeen: now - 30_000,
    }),
    // On another machine, so the Agents page the link opens has something to leave out.
    machineSession(honestyIds.elsewhere, "Devbox planner", {
      host: "some-devbox",
      lastSeen: now - 30_000,
    }),
    machineSession(honestyIds.here, "Planner", {
      dir: "/workspaces/cybermonklabs/legion/infra-883",
      host: SHARED_NAME_HOST,
      lastSeen: now - 30_000,
      place: lonePlace(3),
    }),
    machineSession(honestyIds.reviewer, "Reviewer", {
      dir: "/workspaces/cybermonklabs/legion/infra-812",
      host: SHARED_NAME_HOST,
      lastSeen: now - 30_000,
      place: lonePlace(4),
    }),
    ...widthMachineSessions(now),
    ...undrawnMachineSessions(now),
    ...twoServerSessions(now),
  ]);
  return {
    clusterHost: CLUSTER_HOST,
    elsewhere: honestyIds.elsewhere,
    kubeOne: honestyIds.kubeOne,
    repeated: repeatedIds,
    repeatedHost: REPEATED_HOST,
    sharedNameHost: SHARED_NAME_HOST,
    undrawn: undrawnIds,
    undrawnHost: UNDRAWN_HOST,
    widthHost: WIDTH_HOST,
  };
});

/** One machine and no other, for the row that counts the rows of the machines list. */
const oneMachine = stubbed(async () => {
  await setLiveSessions(drawnMachineSessions(Date.now()));
  return { drawHost: DRAW_HOST };
});

test.describe("@machines-draw", () => {
  oneMachine(
    "the machines list names the machine with its session count and how many of those are in a pane @pr",
    async ({ page, scene }) => {
      await page.goto("/machines");
      await expect(page.locator('[data-testid="machines-page"]')).toBeVisible();

      const rows = page.locator('[data-testid="machine-row"]');
      await expect(rows).toHaveCount(1);
      const row = rows.first();
      await expect(row).toHaveAttribute("data-machine", scene.drawHost);
      // Seven sessions on the machine, six of them in a pane.
      await expect(row).toHaveAttribute("data-sessions", "7");
      await expect(row).toHaveAttribute("data-in-tmux", "6");
      await expect(row).toContainText("7 sessions · 6 in a pane");
      await expect(row.getByRole("link", { name: scene.drawHost })).toHaveAttribute(
        "href",
        `/machines/${scene.drawHost}`
      );

      await row.getByRole("link", { name: scene.drawHost }).click();
      await expect(page.locator('[data-testid="machine-page"]')).toHaveAttribute(
        "data-machine",
        scene.drawHost
      );
    }
  );
});

test.describe("@machines-honesty", () => {
  test("a machine whose sessions are all outside tmux draws nothing and sends the reader to its agents @pr", async ({
    page,
    scene,
  }) => {
    const host = scene.clusterHost;
    await openMachine(page, host);

    await expect(page.locator('[data-testid="machine-none-in-tmux"]')).toHaveText(
      "No session on this machine is in tmux, so there is no layout to draw."
    );
    await expect(page.locator('[data-testid="machine-server"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="window-drawing"]')).toHaveCount(0);

    const toAgents = page
      .locator('[data-testid="machine-page"]')
      .getByRole("link", { name: "Agents on this machine" });
    await expect(toAgents).toHaveAttribute("href", `/agents?machine=${host}`);
    await toAgents.click();
    await expect(page).toHaveURL(/\/agents\?machine=cluster-pool$/);
    // The Agents page opens on this machine's filter, so the session on another machine is
    // left out of the list. A row the filter excludes stays mounted and hidden.
    await expect(page.getByRole("combobox", { name: "Machine" })).toHaveValue(host);
    await expect(page.locator(`[data-agent-row="${scene.elsewhere}"]`)).toBeHidden();
    await expect(page.locator(`[data-agent-row="${scene.kubeOne}"]`)).toBeAttached();
  });

  test("a machine name several sessions share with different directories is marked a logical name @pr", async ({
    page,
    scene,
  }) => {
    await openMachine(page, scene.sharedNameHost);

    await expect(page.locator('[data-testid="machine-shared-name"]')).toHaveText(
      "This machine name is one a deployment chose, not one host: its sessions report more than one directory."
    );
  });
});

// ---------------------------------------------------------------------------------------------
// k8: the page reads and never writes.
// ---------------------------------------------------------------------------------------------

/** One request the page made, as this check reads it. */
interface Recorded {
  readonly method: string;
  readonly path: string;
}

/** Every request the page makes from here on. The live stream rides a websocket, which
 *  Playwright reports as neither a request nor a response, so nothing here counts it. */
function recordRequests(page: Page): Recorded[] {
  const made: Recorded[] = [];
  page.on("request", (request) => {
    made.push({ method: request.method(), path: new URL(request.url()).pathname });
  });
  return made;
}

/** Every page of the app a control on these pages may open: the machine list, a machine, the
 *  Agents page and a session's live view. */
const APP_PAGE_PATH = /^\/machines(\/[^/]+)?$|^\/agents$|^\/agents\/[^/]+\/live$/;

/** A path a route of these pages' own would be named by. The pages read the agents list, the
 *  per-person agent state and the Inbox tally, all three of which the application shell already
 *  makes; what they must not do is ask for a route of their own. */
const OWN_ROUTE = /machine|tmux|pane|window/i;

test.describe("@machines-readonly", () => {
  oneMachine(
    "every control on a drawn machine page is a link to a page of the app, and every request it makes is a read @pr",
    async ({ page, scene }) => {
      const made = recordRequests(page);
      await openMachine(page, scene.drawHost);
      await expect(page.locator('[data-testid="window-layout"]').first()).toBeVisible();

      // A person picks each window of each server.
      const picks = page.locator('[data-testid="window-pick"]');
      const windows = await picks.evaluateAll((nodes) =>
        nodes.map((node) => node.dataset.window ?? "")
      );
      expect(windows.length).toBeGreaterThan(1);
      for (const window of windows) {
        await page.locator(`[data-testid="window-pick"][data-window="${window}"]`).click();
        await expect(
          page.locator(`[data-testid="window-drawing"][data-window="${window}"]`)
        ).toBeVisible();
      }

      // Every control of the page is a link, and every link opens a page of this app.
      const controls = await page
        .locator('[data-testid="machine-page"]')
        .locator("a, button")
        .evaluateAll((nodes) =>
          nodes.map((node) => ({
            href: node instanceof HTMLAnchorElement ? node.href : null,
            tag: node.tagName.toLowerCase(),
          }))
        );
      expect(controls.length).toBeGreaterThan(0);
      const origin = new URL(page.url()).origin;
      const notPages = controls.filter(
        (control) =>
          control.href === null ||
          new URL(control.href).origin !== origin ||
          !APP_PAGE_PATH.test(new URL(control.href).pathname)
      );
      expect(notPages, "every control is a link to a page of this app").toEqual([]);

      // Every request the page made while it drew and while the windows were picked is a read.
      expect(
        made.filter((request) => request.method !== "GET"),
        "the page made a request that is not a GET"
      ).toEqual([]);
      const read = [
        ...new Set(made.filter((r) => r.path.startsWith("/api/")).map((r) => r.path)),
      ].sort();
      // The agents list is the one read the drawing itself needs, and it is the shell's own.
      expect(read).toContain("/api/v1/agents");
      expect(
        read.filter((path) => OWN_ROUTE.test(path)),
        "the page asked for a route of its own"
      ).toEqual([]);
    }
  );
});

test.describe("@machines-width", () => {
  // Untagged, and so this project's alone: it sets a laptop viewport of its own, which a phone
  // project would only have to undo.
  test("at 1440 pixels the strip is one row, the drawing fills the width of the page, and every pane keeps its proportion @pr", async ({
    page,
    scene,
  }) => {
    expect(WIDE.cells.length, "the seeded window holds 22 panes").toBe(22);
    await page.setViewportSize({ height: 900, width: 1440 });
    await openMachine(page, scene.widthHost);

    await expect(page.locator('[data-testid="window-pick"]')).toHaveCount(4);
    expect(await stripIsOneRow(page), "the strip is one row").toBe(true);
    expect(await stripScrolls(page), "the strip scrolls sideways").toBe(false);

    // The drawing fills the width of the page: nothing between it and the page takes any of
    // that width.
    const drawn = page.locator('[data-testid="window-drawing"]');
    const pageWidth = await page
      .locator('[data-testid="machine-page"]')
      .evaluate((section) => section.clientWidth);
    const whole = await page.locator('[data-testid="window-layout"]').boundingBox();
    if (whole === null) throw new Error("the drawing has no box");
    expect(Math.abs(whole.width - pageWidth), "the drawing fills the page").toBeLessThan(1);

    await expect(drawn).toHaveAttribute("data-drawn", "layout");
    await assertPaneProportions(drawn, THREE_PANES, THREE_PANE_SIZE);

    // At this width every box of the 22-pane window is wide enough to read, so the window is
    // drawn, and its boxes are in the layout's own order.
    await page.locator('[data-testid="window-pick"][data-window="@3"]').click();
    await expect(drawn).toHaveAttribute("data-window", "@3");
    await expect(drawn).toHaveAttribute("data-drawn", "layout");
    await assertPaneProportions(drawn, WIDE.cells, WIDE_SIZE);
  });
});

test.describe("@machines-accounts-for-every-session", () => {
  test("a session in a pane the drawn layout does not name is still accounted for @pr", async ({
    page,
    scene,
  }) => {
    await page.setViewportSize({ height: 900, width: 1440 });
    await openMachine(page, scene.undrawnHost);
    await expect(page.locator('[data-testid="machine-server"]')).toHaveCount(1);

    // What the page says it holds.
    await expect(page.locator('[data-testid="machine-page"] header')).toContainText(
      "5 sessions · 5 in a pane"
    );

    // What the page shows. Every pane box and every pane row names its session in `data-agent`.
    const accounted = await page
      .locator('[data-testid="machine-page"] [data-agent]')
      .evaluateAll((nodes) => [
        ...new Set(nodes.map((node) => node.getAttribute("data-agent") ?? "")),
      ]);
    expect(
      accounted.sort(),
      "the page counts five sessions in a pane and must account for all five"
    ).toEqual([...Object.values(scene.undrawn)].sort());
  });
});

test.describe("@machines-two-servers-one-window-id", () => {
  test("picking a window id two servers share draws the window of the server it was picked on, and leaves the other server where it was @pr", async ({
    page,
    scene,
  }) => {
    await openMachine(page, scene.repeatedHost);

    const servers = page.locator('[data-testid="machine-server"]');
    await expect(servers).toHaveCount(2);
    // In socket order: the person's own server first, the Legion daemon's second. Each part
    // names its server by its socket, in its heading and in `data-socket`.
    const person = servers.nth(0);
    const daemon = servers.nth(1);
    await expect(person).toHaveAttribute("data-socket", PERSON_SOCKET);
    await expect(person.getByRole("heading", { level: 2 })).toHaveText(PERSON_SOCKET);
    await expect(daemon).toHaveAttribute("data-socket", DAEMON_SOCKET);
    await expect(daemon.getByRole("heading", { level: 2 })).toHaveText(DAEMON_SOCKET);

    // Both servers hold a `@0` and a `@1`, and the same id in the two strips is two links:
    // each names the server its window is on.
    const personPicks = person.locator('[data-testid="window-pick"]');
    const daemonPicks = daemon.locator('[data-testid="window-pick"]');
    for (const picks of [personPicks, daemonPicks]) {
      expect(await picks.evaluateAll((all) => all.map((pick) => pick.dataset.window))).toEqual([
        "@0",
        "@1",
      ]);
    }
    expect(await personPicks.nth(1).getAttribute("href")).not.toBe(
      await daemonPicks.nth(1).getAttribute("href")
    );

    // With nothing in the address, each server draws its own first window.
    const personDrawing = person.locator('[data-testid="window-drawing"]');
    const daemonDrawing = daemon.locator('[data-testid="window-drawing"]');
    await expect(personDrawing).toHaveAttribute("data-window", "@0");
    await expect(daemonDrawing).toHaveAttribute("data-window", "@0");

    await personPicks.nth(1).click();

    // The address names the server and the window together, never the bare `@1`, which on
    // this machine would name a window on each of the two servers.
    await expect
      .poll(() => new URL(page.url()).searchParams.get("window"))
      .toBe(`${PERSON_SOCKET}:4242:@1`);

    // The person's server draws its own `@1`: the one pane that window holds and no other.
    await expect(personDrawing).toHaveAttribute("data-window", "@1");
    await expect(personDrawing).toHaveAttribute("data-window-name", "agents");
    const personPanes = personDrawing.locator('[data-testid="window-pane"]');
    await expect(personPanes).toHaveCount(1);
    await expect(personPanes.first()).toHaveAttribute("data-pane", "%1");
    await expect(personPanes.first()).toHaveAttribute("data-agent", scene.repeated.editor);
    await expect(personPicks.nth(1)).toHaveAttribute("data-picked", "true");

    // The daemon's server is where it was: its drawing is still its own `@0`, holding its own
    // pane, and its strip still highlights that window.
    await expect(daemonDrawing).toHaveAttribute("data-window", "@0");
    await expect(daemonDrawing).toHaveAttribute("data-window-name", "daemon");
    const daemonPanes = daemonDrawing.locator('[data-testid="window-pane"]');
    await expect(daemonPanes).toHaveCount(1);
    await expect(daemonPanes.first()).toHaveAttribute("data-agent", scene.repeated.daemon);
    await expect(daemonPicks.nth(0)).toHaveAttribute("data-picked", "true");
    await expect(daemonPicks.nth(1)).toHaveAttribute("data-picked", "false");
  });

  test("a deep link to one server's window draws that window and leaves the other server at its first @pr", async ({
    page,
    scene,
  }) => {
    const choice = encodeURIComponent(`${DAEMON_SOCKET}:9001:@1`);
    await page.goto(`/machines/${encodeURIComponent(scene.repeatedHost)}?window=${choice}`);
    await expect(page.getByRole("heading", { level: 1, name: scene.repeatedHost })).toBeVisible();

    const servers = page.locator('[data-testid="machine-server"]');
    // The link names the daemon's server, so the daemon's `@1` is drawn and the person's
    // server - which holds a `@1` of its own - is at its first window.
    await expect(servers.nth(1).locator('[data-testid="window-drawing"]')).toHaveAttribute(
      "data-window",
      "@1"
    );
    await expect(
      servers.nth(1).locator('[data-testid="window-drawing"] [data-testid="window-pane"]')
    ).toHaveAttribute("data-agent", scene.repeated.builds);
    await expect(servers.nth(0).locator('[data-testid="window-drawing"]')).toHaveAttribute(
      "data-window",
      "@0"
    );
    await expect(
      servers.nth(0).locator('[data-testid="window-drawing"] [data-testid="window-pane"]')
    ).toHaveAttribute("data-agent", scene.repeated.shell);
  });
});
