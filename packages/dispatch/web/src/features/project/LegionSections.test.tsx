import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type {
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  LegionMetricsResponse,
  RepoProject,
} from "../../api/types";
import { legionPhaseScale } from "../../theme/classes";
import { LegionSections } from "./LegionSections";

/** Every spy a test installed, put back when it ends. */
const spies: { mockRestore(): void }[] = [];
afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

const WINDOW = { from: "2026-10-06T00:00:00.000Z", to: "2026-11-03T00:00:00.000Z" };

/** One answer of `GET /api/v1/legion/metrics`, as the server writes it (`metrics.Report`). */
function report(overrides: Partial<LegionMetricsResponse> = {}): LegionMetricsResponse {
  return {
    computed_at: "2026-11-03T05:04:00Z",
    finished: 2,
    from: WINDOW.from,
    phase_time: {
      bars: [
        {
          hours: 6,
          issues: 1,
          median_hours_per_issue: 6,
          role: "implementer",
          segments: {
            ci_wait: 1,
            idle: 1,
            in_a_turn: 4,
            waiting_on_children: 0,
            waiting_on_person: 0,
            unknown: 0,
          },
          issue_keys: ["ACME-6", "ACME-7"],
          segment_issues: {
            ci_wait: ["ACME-7"],
            idle: ["ACME-6"],
            in_a_turn: ["ACME-6", "ACME-7"],
          },
        },
        {
          hours: 2,
          issues: 1,
          median_hours_per_issue: 2,
          role: "no_role",
          segments: {
            ci_wait: 0,
            idle: 0,
            in_a_turn: 0,
            waiting_on_children: 0,
            waiting_on_person: 2,
            unknown: 0,
          },
          issue_keys: ["ACME-6"],
          segment_issues: { waiting_on_person: ["ACME-6"] },
        },
      ],
      idle_share: null,
      observed_hours: 0,
      unknown_hours: 0,
    },
    project: "ACME",
    rework: {
      hours_after_first_review: 5,
      issues: [
        {
          implementing_hours_after_first_review: 4,
          key: "ACME-7",
          review_rounds: 1,
          sent_back: true,
          testing_hours_after_first_review: 1,
        },
      ],
      median_hours_after_first_review: 5,
      median_review_rounds: 1,
      sent_back_share: 0.5,
    },
    scope: "all",
    splitting: {
      children_per_root: 1,
      filed: [
        {
          at: "2026-11-01T00:00:00Z",
          class: "in_tree_at_design",
          filer: "architect",
          filer_issue: "ACME-6",
          filer_phase: "design",
          key: "ACME-7",
          title: "The finished child",
        },
      ],
      finished_roots: 1,
      median_hours_roots_with_children: 40,
      median_hours_roots_without_children: 0,
      weeks: [
        {
          filed: {
            by_the_controller: 0,
            in_tree_at_design: 1,
            in_tree_later: 0,
            out_of_tree: 0,
          },
          finished: 2,
          week: "2026-10-26T00:00:00Z",
        },
      ],
    },
    to: WINDOW.to,
    work_time: {
      issues: [
        {
          closed_at: "2026-11-02T00:00:00Z",
          hours: 40,
          key: "ACME-6",
          merged_at: null,
          parked_hours: 2,
          priority: 1,
          queued_hours: 3,
          root: true,
          started_at: "2026-10-31T00:00:00Z",
          title: "The finished root",
        },
        {
          closed_at: "2026-11-01T00:00:00Z",
          hours: 8,
          key: "ACME-7",
          merged_at: "2026-11-01T00:00:00Z",
          parked_hours: 0,
          priority: 2,
          queued_hours: 0,
          root: false,
          started_at: "2026-10-31T12:00:00Z",
          title: "The finished child",
        },
      ],
      median_hours: 24,
      p90_hours: 36.8,
    },
    phase_breakdown: {
      hours: 48,
      issues: [
        {
          hours: 40,
          key: "ACME-6",
          phases: { planning: 10, waiting_on_children: 30 },
          title: "The finished root",
        },
        {
          hours: 8,
          key: "ACME-7",
          phases: { implementing: 6, reviewing: 2 },
          title: "The finished child",
        },
      ],
      phases: [
        { hours: 10, id: "planning", label: "Planning", share: 0.2083 },
        { hours: 6, id: "implementing", label: "Implementing", share: 0.125 },
        { hours: 2, id: "reviewing", label: "Reviewing", share: 0.0417 },
        { hours: 30, id: "waiting_on_children", label: "Waiting on children", share: 0.625 },
      ],
    },
    ...overrides,
  };
}

const DAY = {
  cancelled: 0,
  failure: 1,
  median_minutes: 12,
  n: 4,
  p90_minutes: 20,
  success: 3,
  success_rate: 0.75,
};

/** The pipeline route's answer, in the parts the CI card reads. */
const PIPELINE = {
  days: ["2026-11-01", "2026-11-02"],
  pr_checks: {
    run: {
      days: [
        { ...DAY, day: "2026-11-01", partial: false },
        { ...DAY, day: "2026-11-02", partial: true },
      ],
      total: { ...DAY, n: 8, success: 6, failure: 2 },
    },
  },
} as unknown as DeliveryPipelineResponse;

/** The measures route's answer, in the parts the deploy card reads. */
const MEASURES = {
  measures: {
    daily: [
      {
        cancelled: 0,
        concluded: 3,
        day: "2026-11-01",
        deploys: 3,
        partial: false,
        reached_production: 3,
        run_success_rate: 1,
      },
      {
        cancelled: 0,
        concluded: 2,
        day: "2026-11-02",
        deploys: 1,
        partial: true,
        reached_production: 1,
        run_success_rate: 0.5,
      },
    ],
    deploy_frequency: {
      deploys_with_prs: 4,
      per_day: 2,
      successful_deploys: 4,
      with_prs_per_day: 2,
    },
    deploy_run_success: { cancelled: 0, concluded: 5, rate: 0.8, reached_production: 4 },
    lead_time: { merge_to_production: { max_minutes: 90, median_minutes: 30, p90_minutes: 80 } },
  },
} as unknown as DeliveryMeasuresResponse;

function mapping(project: string, repo: string): RepoProject {
  return { created_by: { id: "alice", kind: "user" }, default: true, project, repo } as RepoProject;
}

/** The panel for `project`, with the Legion settings, the repository mappings and the deploy
 *  repository given. */
async function renderSections(
  answer: (query: { project: string; scope: string }) => LegionMetricsResponse,
  options: { repos?: RepoProject[]; deployRepo?: string | null } = {}
) {
  const metrics = spyOn(api, "getLegionMetrics").mockImplementation((query) =>
    Promise.resolve(answer(query))
  );
  const repos = spyOn(api, "listRepoProjects").mockResolvedValue(options.repos ?? []);
  const delivery = spyOn(api, "getDeliverySettings").mockResolvedValue(
    options.deployRepo === undefined || options.deployRepo === null
      ? null
      : ({ deploy_repo: options.deployRepo } as never)
  );
  const pipeline = spyOn(api, "getDeliveryPipeline").mockResolvedValue(PIPELINE);
  const measures = spyOn(api, "getDeliveryMeasures").mockResolvedValue(MEASURES);
  spies.push(metrics, repos, delivery, pipeline, measures);
  const opened: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <LegionSections onOpenIssue={(key) => opened.push(key)} project="ACME" />
    </QueryClientProvider>
  );
  await screen.findByRole("region", { name: "Where the finished issues' time went" });
  return { measures, metrics, opened, pipeline };
}

/** Switches a chart to its table and answers the table. */
async function openTable(label: string): Promise<Element> {
  fireEvent.click(
    screen
      .getByRole("group", { name: `${label} view` })
      .querySelector("button:last-of-type") as HTMLElement
  );
  return waitFor(() => {
    const found = document.querySelector(`[data-chart-table="${label}"]`);
    if (found === null) throw new Error("no table");
    return found;
  });
}

function headline(id: string): string {
  return (
    document.querySelector(`[data-legion-section="${id}"] [data-legion-headline]`)?.textContent ??
    ""
  );
}

test("the sections come in one column, in the order a reader asks, each with its headline", async () => {
  await renderSections(() => report());

  expect(
    [...document.querySelectorAll("[data-legion-section]")].map((one) =>
      one.getAttribute("data-legion-section")
    )
  ).toEqual(["phase_breakdown", "work_time", "rework", "splitting"]);
  expect(headline("work_time")).toBe(
    "2 issues finished · median 24.0h from first worker event to merge, less parked and queued time · p90 36.8h"
  );
  expect(headline("phase_breakdown")).toContain("most of it waiting on children: 63% (30.0h)");
  expect(document.querySelector("[data-health-tile]")).toBeNull();
});

test("the phase bar draws each part in the issues table's colour with its share", async () => {
  await renderSections(() => report());

  const part = document.querySelector(
    '[data-legion-phase-part="waiting_on_children"]'
  ) as HTMLElement;
  for (const one of (legionPhaseScale.waiting_on_children ?? "").split(" ")) {
    expect(part.className).toContain(one);
  }
  expect(part.textContent).toBe("Waiting on children 63%");
  expect(part.style.width).toBe("62.5%");
  expect(document.querySelector("[data-legion-phase-legend]")?.textContent).toContain(
    "Planning10.0h21%"
  );
});

test("the issues rank by work hours, or by the phase picked, and a key opens its timeline", async () => {
  const { opened } = await renderSections(() => report());

  const order = () =>
    [...document.querySelectorAll("[data-legion-breakdown-issue]")].map((one) =>
      one.getAttribute("data-legion-breakdown-issue")
    );
  expect(order()).toEqual(["ACME-6", "ACME-7"]);
  expect(
    document.querySelector('[data-legion-breakdown-issue="ACME-6"] [data-legion-breakdown-mostly]')
      ?.textContent
  ).toBe("mostly waiting on children, 75%");
  fireEvent.click(document.querySelector('[data-legion-phase-part="implementing"]') as HTMLElement);
  expect(order()).toEqual(["ACME-7", "ACME-6"]);
  fireEvent.click(screen.getByRole("button", { name: "ACME-6" }));
  expect(opened).toEqual(["ACME-6"]);
});

test("with no observation, in a turn and CI wait are named once as not observed yet", async () => {
  await renderSections(() => report());

  expect(document.querySelector("[data-legion-turn-note]")?.textContent).toContain(
    "Not observed yet: in a turn and CI wait."
  );
});

test("the work-time chart plots medians per day with axes and a legend; its table lists issues", async () => {
  const { opened } = await renderSections(() => report());

  const section = document.querySelector('[data-legion-section="work_time"]') as HTMLElement;
  expect(
    [...section.querySelectorAll('[data-chart-legend="window"]')].map((one) => one.textContent)
  ).toEqual(["Median work time", "p90 work time"]);
  expect(section.querySelector("[data-chart-unit]")?.textContent).toBe("hours");
  const table = await openTable("Work time to merge");
  expect(table.textContent).toContain("ACME-6");
  fireEvent.click(screen.getByRole("button", { name: "Show the issues of ACME-6" }));
  expect(opened).toEqual(["ACME-6"]);
});

test("splitting plots filings against finishes and lists the filings by class", async () => {
  await renderSections(() => report());

  const section = document.querySelector('[data-legion-section="splitting"]') as HTMLElement;
  expect(
    [...section.querySelectorAll('[data-chart-legend="window"]')].map((one) => one.textContent)
  ).toEqual(["Issues filed by Legion sessions", "Issues finished"]);
  expect(section.querySelector("[data-legion-filing-classes]")?.textContent).toContain(
    "In the filer's tree, while designing it1100%"
  );
});

test("the scope toggle reads the metrics again for the roots alone", async () => {
  const { metrics } = await renderSections((query) =>
    query.scope === "roots" ? report({ finished: 1, scope: "roots" }) : report()
  );

  fireEvent.click(screen.getByRole("button", { name: "Roots" }));
  await waitFor(() => expect(headline("work_time")).toStartWith("1 issues finished"));
  expect(metrics).toHaveBeenCalledWith(
    expect.objectContaining({ project: "ACME", scope: "roots" })
  );
});

test("a project whose repositories are not the deploy repository gets one line for CI and deploys", async () => {
  const { measures, pipeline } = await renderSections(() => report(), {
    deployRepo: "acme/deploy",
    repos: [mapping("ACME", "acme/widgets")],
  });

  expect(
    (
      await waitFor(() => {
        const line = document.querySelector("[data-legion-delivery-unmeasured]");
        if (line === null) throw new Error("no line");
        return line;
      })
    ).textContent
  ).toStartWith("CI and deploys: not measured for this project yet.");
  expect(document.querySelector('[data-legion-section="ci"]')).toBeNull();
  expect(pipeline).not.toHaveBeenCalled();
  expect(measures).not.toHaveBeenCalled();
});

test("a project that maps the deploy repository gets a CI and a deploys section", async () => {
  const { measures, pipeline } = await renderSections(() => report(), {
    deployRepo: "acme/deploy",
    repos: [
      mapping("ACME", "acme/deploy"),
      mapping("ACME", "acme/widgets"),
      mapping("CORE", "acme/core"),
    ],
  });

  await waitFor(() => expect(headline("deploys")).toContain("2.0 a day"));
  expect(headline("ci")).toContain("75% succeeded");
  expect(pipeline).toHaveBeenCalledWith(
    expect.objectContaining({ repo: ["acme/deploy", "acme/widgets"] })
  );
  expect(measures).toHaveBeenCalledWith(
    expect.objectContaining({ repo: ["acme/deploy", "acme/widgets"] })
  );
});
