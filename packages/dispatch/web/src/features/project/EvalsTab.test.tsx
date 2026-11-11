import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { LegionIssueTimeline } from "../../api/types";
import { stageClass } from "../../theme/classes";
import { EvalsTab } from "./EvalsTab";

const spies: { mockRestore(): void }[] = [];
afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

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

async function renderTab(issue = "ACME-2", timeline: LegionIssueTimeline = TIMELINE) {
  const listing = spyOn(api, "listIssues").mockResolvedValue([]);
  spies.push(
    listing,
    spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    spyOn(api, "getLegionMetrics").mockImplementation(() => new Promise(() => {})),
    spyOn(api, "getLegionIssueTimeline").mockResolvedValue(timeline),
    spyOn(api, "listRepoProjects").mockResolvedValue([])
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={["/projects/ACME/legion/ACME-2"]}>
      <QueryClientProvider client={queryClient}>
        <EvalsTab issue={issue} project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await screen.findByRole("region", { name: "ACME-2 timeline" });
  return listing;
}

test("the Evals tab reads metrics without issue rows or the issue listing", async () => {
  const listing = await renderTab();

  expect(document.querySelector("[data-evals-tab]")).not.toBeNull();
  expect(document.querySelector("[data-issue-row]")).toBeNull();
  expect(screen.queryByRole("table", { name: /Issue/u })).toBeNull();
  expect(listing).not.toHaveBeenCalled();
});
test("an open timeline draws its phases with the table's label and colour, and its children", async () => {
  await renderTab("ACME-2");

  const timeline = await screen.findByRole("region", { name: "ACME-2 timeline" });
  await waitFor(() =>
    expect(timeline.querySelector('[data-legion-span="reviewing"]')).not.toBeNull()
  );
  const reviewing = timeline.querySelector('[data-legion-span="reviewing"]') as HTMLElement;
  for (const part of stageClass("phase:reviewing").split(" ")) {
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
