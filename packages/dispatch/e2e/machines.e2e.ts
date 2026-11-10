import { expect, test } from "@playwright/test";
import { setLiveSessions } from "./agents";
import {
  assertPaneProportions,
  DAEMON_SOCKET,
  DRAW_HOST,
  drawIds,
  FOUR_PANE_SIZE,
  FOUR_PANES,
  honestyIds,
  lonePlace,
  machineSession,
  openMachine,
  PERSON_SOCKET,
  seedDrawnMachine,
  seedWidthMachine,
  stripIsOneRow,
  stripScrolls,
  THREE_PANE_SIZE,
  THREE_PANES,
  WIDE,
  WIDTH_HOST,
  widthIds,
} from "./machines";
import { resetDatabase } from "./seed";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * The machine pages (`/machines` and `/machines/<machine>`), which draw a machine's tmux layout
 * from the `tmux_place` each `Agent` of `GET /api/v1/agents` carries. One `test.describe` per
 * check of dispatch://INFRA-883/artifact/verification-md, each carrying that check's grep tag:
 * `@machines-draw` is k5, `@machines-honesty` k7 and `@machines-width` k9. The width check is
 * two cases, because `packages/dispatch/AGENTS.md` refuses the `@phone` tag to a case that
 * starts at a desktop width and narrows itself: the untagged 1440 px case is a stubbed row
 * (`machines.stub.e2e.ts`), and the `@phone` case runs here at the `iphone` project's own
 * width, each measured against the ratios `e2e/machines.ts` computes from the same seeded
 * layout string.
 *
 * What is left here is what the clock decides and what one engine draws. A pane's state and a
 * drawing's age are read from `Date.now()` against the `last_seen` the registry reports, so a
 * recording of either would be a different answer an hour later; the rest of these checks -
 * k8, k12, the drawing's own proportions and everything the page says it does not know - are
 * stubbed rows, in `machines.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test.describe("@machines-draw", () => {
  test("the machine page draws both tmux servers, its windows in index order, and each pane in its own proportion", async ({
    browser,
  }) => {
    await seedDrawnMachine(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, DRAW_HOST);

      // One part per tmux server, in socket order: the person's first, the daemon's second.
      const servers = page.locator('[data-testid="machine-server"]');
      await expect(servers).toHaveCount(2);
      await expect(servers.nth(0)).toHaveAttribute("data-socket", PERSON_SOCKET);
      await expect(servers.nth(0)).toHaveAttribute("data-server-pid", "4242");
      await expect(servers.nth(1)).toHaveAttribute("data-socket", DAEMON_SOCKET);
      await expect(servers.nth(1)).toHaveAttribute("data-server-pid", "9001");

      // The first part's strip holds its two windows in window-index order, and the first is
      // the one drawn.
      const strip = servers.nth(0).locator('[data-testid="window-pick"]');
      await expect(strip).toHaveCount(2);
      expect(await strip.evaluateAll((picks) => picks.map((pick) => pick.dataset.window))).toEqual([
        "@1",
        "@2",
      ]);
      await expect(strip.nth(0)).toHaveAttribute("data-picked", "true");
      await expect(strip.nth(1)).toHaveAttribute("data-picked", "false");

      const drawn = servers.nth(0).locator('[data-testid="window-drawing"]');
      await expect(drawn).toHaveAttribute("data-window", "@1");
      await expect(drawn).toHaveAttribute("data-window-index", "1");
      await expect(drawn).toHaveAttribute("data-window-name", "agents");
      await expect(drawn).toHaveAttribute("data-drawn", "layout");

      // Four pane boxes, each at the layout's own proportion of the drawing.
      const boxes = drawn.locator('[data-testid="window-pane"]');
      await assertPaneProportions(drawn, FOUR_PANES, FOUR_PANE_SIZE);

      // A box whose pane holds a session shows that session's name and its state, and its link
      // is that session's live view.
      const planner = boxes.nth(0);
      await expect(planner).toHaveAttribute("data-agent", drawIds.planner);
      await expect(planner.getByRole("link", { name: "Planner" })).toHaveAttribute(
        "href",
        `/agents/${drawIds.planner}/live`
      );
      await expect(planner.locator('[data-testid="window-pane-state"]')).toHaveAttribute(
        "data-state",
        "idle"
      );
      await expect(planner.locator('[data-testid="window-pane-state"]')).toHaveText("Idle");
      await expect(boxes.nth(1)).toHaveAttribute("data-agent", drawIds.builder);
      await expect(boxes.nth(1).getByRole("link", { name: "Builder" })).toHaveAttribute(
        "href",
        `/agents/${drawIds.builder}/live`
      );

      // Two live sessions name `%2`: the page draws the one heard from most recently and says
      // how many others name it.
      const shared = boxes.nth(2);
      await expect(shared).toHaveAttribute("data-agent", drawIds.tester);
      await expect(shared.getByRole("link", { name: "Tester" })).toBeVisible();
      await expect(shared.getByText("Stray")).toHaveCount(0);
      await expect(shared.locator('[data-testid="window-pane-others"]')).toHaveText(
        "1 other session names this pane"
      );

      // The fourth pane holds no agent, and says so with its pane index.
      const empty = boxes.nth(3);
      await expect(empty).toHaveAttribute("data-pane", "%3");
      await expect(empty).toHaveAttribute("data-pane-index", "3");
      expect(await empty.getAttribute("data-agent")).toBeNull();
      await expect(empty).toContainText("Pane 3");
      await expect(empty).toContainText("No agent");

      // Picking the second window draws that window instead.
      await strip.nth(1).click();
      // The address names the server as well as the window (k12): tmux numbers windows per
      // server, so a bare `@2` would name a window on each server of this machine.
      await expect
        .poll(() => new URL(page.url()).searchParams.get("window"))
        .toBe(`${PERSON_SOCKET}:4242:@2`);
      await expect(drawn).toHaveAttribute("data-window", "@2");
      await expect(drawn).toHaveAttribute("data-window-name", "builds");
      await expect(strip.nth(1)).toHaveAttribute("data-picked", "true");
      await expect(servers.nth(0).locator('[data-testid="window-pane"]')).toHaveCount(1);
      await expect(servers.nth(0).locator('[data-testid="window-pane"]').first()).toHaveAttribute(
        "data-agent",
        drawIds.builds
      );

      // The daemon's own part is still its own window, whichever window the address names.
      await expect(servers.nth(1).locator('[data-testid="window-drawing"]')).toHaveAttribute(
        "data-window",
        "@7"
      );
    } finally {
      await alice.close();
    }
  });
});

test.describe("@machines-honesty", () => {
  test("a drawn server says it holds only the windows that hold an agent, and the page dates its drawing", async ({
    browser,
  }) => {
    const host = "fresh-devbox";
    await setLiveSessions([
      machineSession(honestyIds.drawn, "Planner", {
        host,
        lastSeen: Date.now() - 30_000,
        place: lonePlace(3),
      }),
    ]);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, host);

      await expect(page.locator('[data-testid="machine-windows-with-agents"]')).toHaveText(
        "This server shows only the windows that hold an agent: a window with no agent reports nothing."
      );
      // Heard from half a minute ago: dated, and not stale.
      await expect(page.locator('[data-testid="machine-drawing-age"]')).toHaveText(
        "Oldest report drawn: just now."
      );
      await expect(page.locator('[data-testid="machine-drawing-stale"]')).toHaveCount(0);
    } finally {
      await alice.close();
    }
  });

  test("a drawing made from a report four minutes old is marked stale", async ({ browser }) => {
    const host = "stale-devbox";
    await setLiveSessions([
      machineSession(honestyIds.drawn, "Planner", {
        host,
        lastSeen: Date.now() - 4 * 60_000,
        place: lonePlace(3),
      }),
    ]);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, host);

      await expect(page.locator('[data-testid="machine-drawing-age"]')).toHaveText(
        "Oldest report drawn: 4 minutes ago."
      );
      await expect(page.locator('[data-testid="machine-drawing-stale"]')).toHaveText(
        "This drawing is stale: its oldest report is over three minutes old."
      );
    } finally {
      await alice.close();
    }
  });
});

test.describe("@machines-width", () => {
  // The phone project's own, and only its own: every claim here is about what the phone layout
  // does at 390 px, which `chromium` at its own width would only contradict. A test that skips
  // every project but `iphone` is always tagged (`packages/dispatch/AGENTS.md`, R5).
  test("at a phone width the strip scrolls sideways, the drawn panes keep their proportions, and a 22-pane window is listed in layout order @phone", async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name !== "iphone", "the phone layout is the iphone project's");
    await seedWidthMachine(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, WIDTH_HOST);

      await expect(page.locator('[data-testid="window-pick"]')).toHaveCount(4);
      expect(await stripIsOneRow(page), "the strip is one row").toBe(true);
      expect(await stripScrolls(page), "the strip scrolls sideways").toBe(true);
      // The strip scrolls inside itself, so the page itself is no wider than the phone.
      await assertPageFits(page);

      // The three-pane window is still a drawing here, in the proportions the same seeded
      // layout string gives it.
      const firstWindow = page.locator('[data-testid="window-drawing"]');
      await expect(firstWindow).toHaveAttribute("data-drawn", "layout");
      await assertPaneProportions(firstWindow, THREE_PANES, THREE_PANE_SIZE);

      // The 22-pane window's narrowest box would be under 96 px here, so its panes are listed
      // in layout order instead, with the line that says why.
      await page.locator('[data-testid="window-pick"][data-window="@3"]').click();
      const drawn = page.locator('[data-testid="window-drawing"][data-window="@3"]');
      await expect(drawn).toHaveAttribute("data-drawn", "list");
      await expect(page.locator('[data-testid="window-pane"]')).toHaveCount(0);
      const list = drawn.locator('[data-testid="window-pane-list"]');
      await expect(list).toHaveAttribute("data-reason", "panes-too-small");
      await expect(list.locator('[data-testid="window-pane-list-reason"]')).toHaveText(
        "A pane of this window would be too small to read at this width, so its panes are listed in layout order instead."
      );
      const rows = list.locator('[data-testid="window-pane-row"]');
      await expect(rows).toHaveCount(22);
      expect(await rows.evaluateAll((items) => items.map((item) => item.dataset.pane))).toEqual(
        WIDE.cells.map((cell) => cell.paneId)
      );
      expect(
        await rows.evaluateAll((items) => items.map((item) => item.dataset.paneIndex))
      ).toEqual(WIDE.cells.map((_cell, index) => String(index)));
      await expect(rows.nth(21)).toHaveAttribute("data-agent", widthIds.agentsLast);
    } finally {
      await alice.close();
    }
  });
});
