import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: these rows press board cards and change selects while a fake live socket holds the clock, which user-event has no form for here.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import {
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type {
  LegionBoardCard,
  LegionBoardColumnID,
  LegionBoardResponse,
  LegionIssueTimeline,
} from "../../api/types";
import { legionPhaseScale } from "../../theme/classes";
import { LegionTab } from "./LegionTab";

let sockets: FakeLiveSockets;
const spies: { mockRestore(): void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

const COLUMNS: readonly { id: LegionBoardColumnID; label: string }[] = [
  { id: "queue", label: "Queue" },
  { id: "design", label: "Design" },
  { id: "planning", label: "Planning" },
  { id: "implementing", label: "Implementing" },
  { id: "testing", label: "Testing" },
  { id: "reviewing", label: "Reviewing" },
  { id: "accepting", label: "Accepting" },
  { id: "merging", label: "Merging" },
  { id: "awaiting_person", label: "Waiting on a person" },
  { id: "held", label: "Held" },
];

function card(key: string, overrides: Partial<LegionBoardCard> = {}): LegionBoardCard {
  return {
    blocked_by: [],
    checks_verdict: null,
    children: 0,
    key,
    phase: "implementing",
    phase_since: "2026-11-09T22:00:00Z",
    priority: 1,
    roles: [],
    slot: null,
    title: `${key} title`,
    tree: key,
    tree_slot: null,
    slot_since: null,
    waiting_on: null,
    ...overrides,
  };
}

/** Four slots: one working, one held for its child who is implementing, one held with its
 *  children idle, and an accepting root whose architect is in a turn; a waiting
 *  line of a ready root and one a dependency holds, and a root awaiting a person's answer. */
const BOARD: LegionBoardResponse = {
  columns: COLUMNS.map((column) => ({
    ...column,
    cards:
      column.id === "implementing"
        ? [
            card("ACME-1", { slot: 0, roles: ["implementer"], checks_verdict: "green" }),
            card("ACME-21", { tree: "ACME-2" }),
          ]
        : column.id === "accepting"
          ? [
              card("ACME-2", { phase: "accepting", slot: 1, children: 1, phase_since: null }),
              card("ACME-6", {
                phase: "accepting",
                slot: 3,
                roles: ["architect"],
                phase_since: null,
                slot_since: "2026-11-09T20:00:00Z",
              }),
            ]
          : column.id === "merging"
            ? [card("ACME-3", { phase: "awaiting_merge", slot: 2, checks_verdict: "red" })]
            : column.id === "queue"
              ? [
                  card("ACME-4", { phase: "admitted" }),
                  card("ACME-5", { phase: "admitted", blocked_by: ["ACME-1"] }),
                ]
              : column.id === "awaiting_person"
                ? [
                    card("ACME-7", {
                      phase: "awaiting_person",
                      phase_since: null,
                      waiting_on: { kind: "ask", since: "2026-11-08T00:00:00Z", answered_at: null },
                    }),
                  ]
                : [],
  })),
  computed_at: "2026-11-10T00:00:00Z",
  header: {
    queue_length: 2,
    read_at: "2026-11-10T00:00:00Z",
    read_error: null,
    read_ok: true,
    slot_cap: 4,
    slots_in_use: 4,
    trees: 0,
  },
  project: "ACME",
  setting_sources: {},
  settings: {},
};

const TIMELINE: LegionIssueTimeline = {
  asks: [],
  children: [
    {
      key: "ACME-21",
      phases: [
        {
          from: "2026-11-09T12:00:00Z",
          hours: 2,
          name: "implementing",
          source: "observed",
          to: "2026-11-09T14:00:00Z",
        },
      ],
      source: "observed",
      title: "A child",
    },
  ],
  ci_waits: [],
  closed: true,
  columns: {
    implementing: { id: "implementing", label: "Implementing" },
    reviewing: { id: "reviewing", label: "Reviewing" },
  },
  computed_at: "2026-11-10T00:00:00Z",
  ended_at: "2026-11-09T20:00:00Z",
  key: "ACME-2",
  parked: [],
  phases: [
    {
      from: "2026-11-09T10:00:00Z",
      hours: 6,
      name: "implementing",
      source: "observed",
      to: "2026-11-09T16:00:00Z",
    },
    {
      from: "2026-11-09T16:00:00Z",
      hours: 4,
      name: "reviewing",
      source: "observed",
      to: "2026-11-09T20:00:00Z",
    },
  ],
  project: "ACME",
  queued_hours: 0,
  parked_hours: 0,
  review_rounds: 1,
  root: "ACME-2",
  segment_hours: {
    ci_wait: 0,
    idle: 8,
    in_a_turn: 2,
    unknown: 0,
    waiting_on_children: 0,
    waiting_on_person: 0,
  },
  segments: [],
  sent_back: false,
  source: "observed",
  started_at: "2026-11-09T10:00:00Z",
  title: "A root",
  turns: [
    {
      from: "2026-11-09T10:00:00Z",
      hours: 2,
      name: "implementer",
      source: "observed",
      to: "2026-11-09T12:00:00Z",
    },
  ],
  work_hours: 10,
};

function Location(): null {
  const location = useLocation();
  document.body.dataset.path = location.pathname;
  return null;
}

async function renderTab(issue?: string, timeline: typeof TIMELINE = TIMELINE) {
  spies.push(
    spyOn(api, "listLegionSettings").mockResolvedValue([
      {
        created_by: { id: "alice", kind: "user" },
        project: "ACME",
        state_url: "http://legion.example:13370",
        updated_at: "2026-11-01T00:00:00Z",
      },
    ]),
    spyOn(api, "getLegionMetrics").mockImplementation(() => new Promise(() => {})),
    spyOn(api, "listRepoProjects").mockResolvedValue([]),
    spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    spyOn(api, "getLegionIssueTimeline").mockResolvedValue(timeline),
    // The tables are the Issues tab's grid, which joins its rows to the project's listing.
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listAgents").mockResolvedValue([])
  );
  // The unit DOM has no layout engine and the grid's rows are virtual: give its box a height,
  // and a desktop width, so the grid keeps every column (below 640 px it keeps its primary ones).
  const height = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issue-grid-scroll" ? 600 : (height?.get?.call(this) ?? 0);
    },
  });
  const rect = HTMLElement.prototype.getBoundingClientRect;
  HTMLElement.prototype.getBoundingClientRect = function bounds(this: HTMLElement) {
    const measured = rect.call(this) as DOMRect;
    if (this.dataset.testid !== "issue-grid-scroll") return measured;
    return { ...measured.toJSON(), width: 1000, height: 600 } as DOMRect;
  };
  spies.push({
    mockRestore: () => {
      if (height !== undefined)
        Object.defineProperty(HTMLElement.prototype, "clientHeight", height);
      HTMLElement.prototype.getBoundingClientRect = rect;
    },
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={["/projects/ACME/legion"]}>
      <QueryClientProvider client={queryClient}>
        <Location />
        <LegionTab issue={issue} project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const frame = await waitFor(() => socket.subscribed("legion"));
  act(() => socket.receive(snapshotFrame(frame.sub, BOARD)));
  await screen.findByRole("region", { name: "Slots" });
}

/** A table row of one of the tab's grids, by its region and issue key. */
function row(region: string, key: string): HTMLElement | null {
  return screen.getByRole("region", { name: region }).querySelector(`[data-issue-row="${key}"]`);
}

test("the slots name each holder's phase, its turn and whether the slot is held or idle", async () => {
  await renderTab();

  const slot = (key: string) => row("Slots", key);
  const use = (key: string) => slot(key)?.querySelector("[data-legion-slot-state]")?.textContent;
  await waitFor(() => expect(slot("ACME-1")).not.toBeNull());
  // Each slot use is one short word or two; the hover says why.
  expect(use("ACME-1")).toBe("working");
  expect(slot("ACME-1")?.querySelector("[data-legion-slot-state]")?.getAttribute("title")).toBe(
    "implementer in a turn"
  );
  expect(use("ACME-2")).toBe("held: children");
  expect(use("ACME-3")).toBe("held: idle");
  // An architect in a turn makes the slot working, whatever the phase.
  expect(use("ACME-6")).toBe("working");
  // A phone reads the slot, its use and the time under the phase name, in the phase's own cell.
  expect(slot("ACME-6")?.querySelector("[data-legion-slot-line]")?.textContent).toMatch(
    /^slot 3 · working · \d+ (d|h|m)/
  );
  expect(document.querySelector("[data-legion-slot-summary]")?.textContent).toBe(
    "4 slots, all in use · 2 working · 0 between turns · 1 held · 0 on a person · 1 idle"
  );
  // The phase is the issues table's own cell.
  expect(slot("ACME-1")?.querySelector('[data-legion-phase="implementing"]')).not.toBeNull();
  expect(screen.getByRole("region", { name: "Slots" }).textContent).not.toContain("Nobody");
});

test("the waiting line keeps the daemon's order, says why each root waits and how long", async () => {
  await renderTab();

  await waitFor(() => expect(row("Waiting line", "ACME-5")).not.toBeNull());
  expect(row("Waiting line", "ACME-5")?.textContent).toContain("held by ACME-1");
  expect(row("Waiting line", "ACME-4")?.textContent).toContain("ready");
  expect(row("Waiting line", "ACME-4")?.querySelector("[data-legion-waited]")).not.toBeNull();
  expect(document.querySelector("[data-legion-waiting-summary]")?.textContent).toBe(
    "3 issues · 1 ready · 1 held by a dependency · 1 awaiting a person"
  );
  // A root awaiting a person has no place in the queue, and its wait counts from the ask.
  const awaiting = row("Waiting line", "ACME-7");
  expect(awaiting?.querySelector("[data-legion-place-none]")?.getAttribute("title")).toBe(
    "Not in the queue: waits for a person"
  );
  expect(awaiting?.querySelector('[data-legion-waited="ACME-7"]')?.getAttribute("title")).toBe(
    "2026-11-08T00:00:00Z"
  );
  expect(awaiting?.textContent).toContain("awaiting a person");
});

test("an issue key opens that issue's timeline address", async () => {
  await renderTab();

  await waitFor(() => expect(row("Slots", "ACME-3")).not.toBeNull());
  fireEvent.click(row("Slots", "ACME-3")?.querySelector("[data-issue-link]") as HTMLElement);
  await waitFor(() => expect(document.body.dataset.path).toBe("/projects/ACME/legion/ACME-3"));
});

test("an open timeline draws its phases with the table's label and colour, and its children", async () => {
  await renderTab("ACME-2");

  const timeline = await screen.findByRole("region", { name: "ACME-2 timeline" });
  await waitFor(() =>
    expect(timeline.querySelector('[data-legion-span="reviewing"]')).not.toBeNull()
  );
  const reviewing = timeline.querySelector('[data-legion-span="reviewing"]') as HTMLElement;
  for (const part of (legionPhaseScale.reviewing ?? "").split(" ")) {
    expect(reviewing.className).toContain(part);
  }
  expect(reviewing.title).toContain("Reviewing");
  expect(reviewing.title).toContain("from the daemon's observed events");
  expect(timeline.querySelector('[data-legion-lane="Turn: implementer"]')).not.toBeNull();
  expect(timeline.querySelector('[data-legion-lane="Child ACME-21"]')).not.toBeNull();
  expect(timeline.querySelector('[data-legion-timeline-figure="Work time"]')?.textContent).toBe(
    "10.0 h"
  );
});

test("a slot whose phase start is unknown shows the time since admission, else no time, never 0 m", async () => {
  await renderTab();

  await waitFor(() => expect(row("Slots", "ACME-2")).not.toBeNull());
  const time = (key: string) => row("Slots", key)?.querySelector('[data-legion-fact="phase-time"]');
  // The cell shows the duration alone; its hover says what the duration counts.
  expect(time("ACME-2")?.textContent).toBe("—");
  expect(time("ACME-2")?.getAttribute("title")).toBe("phase start and admission not recorded");
  expect(time("ACME-6")?.textContent).toMatch(/^\d+ (d|h|m)/);
  expect(time("ACME-6")?.getAttribute("title")).toBe("since admission; phase start not recorded");
  expect(time("ACME-1")?.getAttribute("title")).toBe("in this phase");
});

test("a root's phase hatches the hours it waited on its children, and the key names each colour", async () => {
  await renderTab("ACME-2", {
    ...TIMELINE,
    segments: [
      {
        bar: "implementer",
        from: "2026-11-09T12:00:00Z",
        hours: 2,
        name: "waiting_on_children",
        source: "observed",
        to: "2026-11-09T14:00:00Z",
      },
    ],
  });

  const timeline = await screen.findByRole("region", { name: "ACME-2 timeline" });
  await waitFor(() => expect(timeline.querySelector("[data-legion-hatched]")).not.toBeNull());
  const phase = timeline.querySelector('[data-legion-lane="Phase"]') as HTMLElement;
  expect(
    [...phase.querySelectorAll("[data-legion-span]")].map(
      (one) => one.getAttribute("title")?.split(":")[0]
    )
  ).toEqual(["Implementing", "implementing · waiting on children", "Implementing", "Reviewing"]);
  expect(timeline.querySelector("[data-legion-timeline-key]")?.textContent).toContain(
    "hatched: root waits on children"
  );
});

test("a phase's parked hours read 'parked' in grey hatch, and an unknown stretch carries no text", async () => {
  await renderTab("ACME-2", {
    ...TIMELINE,
    parked: [
      {
        from: "2026-11-09T17:00:00Z",
        hours: 2,
        name: "parked",
        source: "observed",
        to: "2026-11-09T19:00:00Z",
      },
    ],
    segments: [
      {
        bar: "reviewer",
        from: "2026-11-09T10:00:00Z",
        hours: 6,
        name: "in_a_turn",
        source: "observed",
        to: "2026-11-09T16:00:00Z",
      },
      {
        bar: "reviewer",
        from: "2026-11-09T16:00:00Z",
        hours: 4,
        name: "unknown",
        source: "observed",
        to: "2026-11-09T20:00:00Z",
      },
    ],
  });

  const timeline = await screen.findByRole("region", { name: "ACME-2 timeline" });
  await waitFor(() =>
    expect(timeline.querySelector('[data-legion-hatched="parked"]')).not.toBeNull()
  );
  const phase = timeline.querySelector('[data-legion-lane="Phase"]') as HTMLElement;
  expect(
    [...phase.querySelectorAll("[data-legion-span]")].map(
      (one) => one.getAttribute("title")?.split(":")[0]
    )
  ).toEqual(["Implementing", "Reviewing", "parked", "Reviewing"]);
  expect(
    timeline.querySelector('[data-legion-lane="Where the hours went"] [data-legion-span="unknown"]')
      ?.textContent
  ).toBe("");
  expect(timeline.querySelector("[data-legion-timeline-key]")?.textContent).toContain(
    "hatched gray: parked"
  );
});

test("the drill-down counts each send-back by its cause and writes nothing as 'none'", async () => {
  const at = (hour: number) => `2026-11-09T${String(hour).padStart(2, "0")}:00:00Z`;
  const span = (name: string, from: number) => ({
    from: at(from),
    hours: 1,
    name,
    source: "status_history" as const,
    to: at(from + 1),
  });
  await renderTab("ACME-2", {
    ...TIMELINE,
    phases: [
      span("implementing", 1),
      span("testing", 2),
      span("implementing", 3),
      span("reviewing", 4),
      span("implementing", 5),
      span("testing", 6),
      span("reviewing", 7),
    ],
    review_rounds: 2,
    source: "status_history",
  });

  const timeline = await screen.findByRole("region", { name: "ACME-2 timeline" });
  await waitFor(() =>
    expect(timeline.querySelector('[data-legion-timeline-figure="Sent back"]')?.textContent).toBe(
      "2 (1 review, 1 tests or CI)"
    )
  );
  expect(timeline.querySelector('[data-legion-timeline-figure="Parked"]')?.textContent).toBe(
    "none"
  );
  expect(timeline.querySelector('[data-legion-timeline-figure="Queued"]')?.textContent).toBe(
    "none"
  );
});
