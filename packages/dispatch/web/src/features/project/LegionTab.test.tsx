import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
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
    waiting_on: null,
    ...overrides,
  };
}

/** Three slots: one working, one held for its child who is implementing, one idle; a waiting
 *  line of a ready root and one a dependency holds. */
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
          ? [card("ACME-2", { phase: "accepting", slot: 1, children: 1, phase_since: null })]
          : column.id === "merging"
            ? [card("ACME-3", { phase: "awaiting_merge", slot: 2 })]
            : column.id === "queue"
              ? [
                  card("ACME-4", { phase: "admitted" }),
                  card("ACME-5", { phase: "admitted", blocked_by: ["ACME-1"] }),
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
    slots_in_use: 3,
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

async function renderTab(issue?: string) {
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
    spyOn(api, "getLegionIssueTimeline").mockResolvedValue(TIMELINE)
  );
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

test("the slots name each holder's phase, its turn and whether the slot is held or idle", async () => {
  await renderTab();

  const state = (slot: number) =>
    document.querySelector(`[data-legion-slot="${slot}"] [data-legion-slot-state]`)?.textContent;
  expect(state(0)).toBe("working");
  expect(state(1)).toBe("held for children only");
  expect(state(2)).toBe("idle");
  expect(document.querySelector("[data-legion-slot-summary]")?.textContent).toContain(
    "3 of 4 slots in use"
  );
  // The phase is the issues table's own cell.
  expect(
    document.querySelector('[data-legion-slot="0"] [data-legion-phase="implementing"]')?.textContent
  ).toContain("Implementing");
  expect(document.querySelector('[data-legion-slot="0"]')?.textContent).toContain("implementer");
});

test("the waiting line names each root ready or held by its dependency", async () => {
  await renderTab();

  const line = screen.getByRole("region", { name: "Waiting line" });
  expect(line.textContent).toContain("held by ACME-1");
  expect(line.textContent).toContain("ready");
  expect(document.querySelector("[data-legion-waiting-summary]")?.textContent).toContain(
    "2 roots waiting for a slot · 1 ready · 1 held by a dependency"
  );
});

test("an issue key opens that issue's timeline address", async () => {
  await renderTab();

  fireEvent.click(screen.getByRole("button", { name: "ACME-3" }));
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
  expect(reviewing.title).toContain("from observed events");
  expect(timeline.querySelector('[data-legion-lane="Turn: implementer"]')).not.toBeNull();
  expect(timeline.querySelector('[data-legion-lane="Child ACME-21"]')).not.toBeNull();
  expect(timeline.querySelector('[data-legion-timeline-figure="Work time"]')?.textContent).toBe(
    "10.0h"
  );
});

test("a slot whose phase entry time is unknown shows no time in phase, never 0 m", async () => {
  await renderTab();

  const time = (slot: number) =>
    document.querySelector(`[data-legion-slot="${slot}"] [data-legion-fact="phase-time"]`);
  expect(time(1)?.textContent).toBe("—");
  expect(time(1)?.getAttribute("title")).toBe("Phase entry time unknown");
  expect(time(0)?.textContent).toMatch(/ in phase$/);
});

test("a holder between turns says so, never 'Nobody'", async () => {
  await renderTab();

  expect(document.querySelector('[data-legion-slot="2"]')?.textContent).toContain("between turns");
  expect(document.querySelector("[data-legion-slots]")?.textContent).not.toContain("Nobody");
});

test("the CI column writes a short mark with the full verdict on hover", async () => {
  await renderTab();

  const ci = document.querySelector('[data-legion-slot="0"] [data-legion-slot-ci]');
  expect(ci?.textContent).toBe("✓");
  expect(ci?.getAttribute("title")).toBe("green");
});
