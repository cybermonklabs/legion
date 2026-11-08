import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type {
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  LegionMetricsResponse,
  LegionSetting,
  RepoProject,
} from "../../api/types";
import { LegionHealthPanel } from "./LegionHealthPanel";

/** Every spy a test installed, put back when it ends. */
const spies: { mockRestore(): void }[] = [];
afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

const WINDOW = { from: "2026-10-06T00:00:00.000Z", to: "2026-11-03T00:00:00.000Z" };

function setting(project: string): LegionSetting {
  return {
    created_by: { kind: "user", id: "alice" },
    project,
    state_url: `http://legion-${project}.internal:13370`,
    updated_at: "2026-11-01T00:00:00Z",
  };
}

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
          },
          issue_keys: ["ACME-6"],
          segment_issues: { waiting_on_person: ["ACME-6"] },
        },
      ],
      idle_share: 0.125,
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
async function renderPanel(
  settings: LegionSetting[],
  answer: (query: { project: string; scope: string }) => LegionMetricsResponse,
  options: {
    project?: string;
    repos?: RepoProject[];
    deployRepo?: string | null;
    onSelectIssues?: (keys: string[], label: string) => void;
  } = {}
) {
  const list = spyOn(api, "listLegionSettings").mockResolvedValue(settings);
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
  spies.push(list, metrics, repos, delivery, pipeline, measures);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <LegionHealthPanel
        onSelectIssues={options.onSelectIssues}
        project={options.project ?? "ACME"}
      />
    </QueryClientProvider>
  );
  return { measures, metrics, pipeline };
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

function card(metric: string): HTMLElement {
  const found = document.querySelector(`[data-legion-metric="${metric}"]`);
  if (found === null) throw new Error(`no ${metric} card`);
  return found as HTMLElement;
}

function figure(metric: string, label: string): string {
  return card(metric).querySelector(`[data-legion-figure="${label}"]`)?.textContent ?? "";
}

test("the four metrics are drawn, each through the one chart component", async () => {
  await renderPanel([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  for (const [metric, label] of [
    ["work_time", "Work time to merge"],
    ["phase_time", "Phase time by worker type"],
    ["rework", "Rework"],
    ["splitting", "Splitting and follow-ups"],
  ]) {
    expect(card(metric).querySelector(`[data-chart="${label}"]`)).not.toBeNull();
    // Each chart has the two controls every chart of a measure over a range has.
    expect(card(metric).querySelector(`select[data-chart-compare="${label}"]`)).not.toBeNull();
    expect(screen.getByRole("group", { name: `${label} view` })).toBeDefined();
  }
  expect(document.querySelectorAll("[data-legion-metric]")).toHaveLength(4);
});

test("each card's headline numbers are the answer's own", async () => {
  await renderPanel([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  expect(figure("work_time", "Median")).toBe("24.0h");
  expect(figure("work_time", "p90")).toBe("36.8h");
  expect(figure("work_time", "Issues finished")).toBe("2");
  expect(figure("phase_time", "Idle share")).toBe("12.5%");
  expect(figure("rework", "Median review rounds")).toBe("1.0");
  expect(figure("rework", "Sent back at least once")).toBe("50.0%");
  expect(figure("splitting", "Issues filed")).toBe("1");
  expect(figure("splitting", "Roots finished")).toBe("1");
});

test("a card's table holds every number its graph draws", async () => {
  await renderPanel([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  fireEvent.click(
    screen
      .getByRole("group", { name: "Phase time by worker type view" })
      .querySelector("button:last-of-type") as HTMLElement
  );
  const table = await waitFor(() => {
    const found = document.querySelector('[data-chart-table="Phase time by worker type"]');
    if (found === null) throw new Error("no table");
    return found;
  });
  // One row per worker type, named as the card draws it, with a column per segment.
  expect(table.textContent).toContain("Worker type");
  expect(table.textContent).toContain("implementer");
  expect(table.textContent).toContain("no role");
  for (const segment of ["Waiting on a person", "In a turn", "CI wait", "Idle"]) {
    expect(table.textContent).toContain(segment);
  }
});

test("the scope toggle reads the metrics again for the roots alone", async () => {
  const { metrics } = await renderPanel([setting("ACME")], (query) =>
    query.scope === "roots" ? report({ finished: 1, scope: "roots" }) : report()
  );

  await screen.findByText("Work time to merge");
  expect(figure("work_time", "Issues finished")).toBe("2");

  fireEvent.click(screen.getByRole("button", { name: "Roots" }));

  await waitFor(() => expect(figure("work_time", "Issues finished")).toBe("1"));
  expect(metrics).toHaveBeenCalledWith(
    expect.objectContaining({ project: "ACME", scope: "roots" })
  );
});

test("a project that names no daemon says so and reads no metrics", async () => {
  const { metrics } = await renderPanel([setting("LINUX")], () => report());

  await waitFor(() =>
    expect(document.querySelector("[data-legion-health-unconfigured]")).not.toBeNull()
  );
  expect(document.querySelectorAll("[data-legion-metric]")).toHaveLength(0);
  expect(metrics).not.toHaveBeenCalled();
});

test("the metrics are read for the panel's own project over the last seven days", async () => {
  const { metrics } = await renderPanel([setting("ACME"), setting("LINUX")], (query) =>
    report({ project: query.project })
  );

  await screen.findByText("Work time to merge");
  const query = metrics.mock.calls[0]?.[0];
  expect(query?.project).toBe("ACME");
  expect(Date.parse(query?.to ?? "") - Date.parse(query?.from ?? "")).toBe(7 * 24 * 3600 * 1000);
});

test("a segment of a worker type's bar names the issues behind it", async () => {
  const selected: [string[], string][] = [];
  await renderPanel([setting("ACME")], () => report(), {
    onSelectIssues: (keys, label) => selected.push([keys, label]),
  });

  await screen.findByText("Work time to merge");
  await openTable("Phase time by worker type");
  fireEvent.click(
    screen.getByRole("button", { name: "Show the issues behind CI wait of implementer" })
  );
  fireEvent.click(screen.getByRole("button", { name: "Show the issues of implementer" }));
  expect(selected).toEqual([
    [["ACME-7"], "CI wait on the implementer bar"],
    [["ACME-6", "ACME-7"], "Time on the implementer bar"],
  ]);
});

test("a work time row and the finished count name their issues", async () => {
  const selected: [string[], string][] = [];
  await renderPanel([setting("ACME")], () => report(), {
    onSelectIssues: (keys, label) => selected.push([keys, label]),
  });

  await screen.findByText("Work time to merge");
  fireEvent.click(screen.getByRole("button", { name: "Show the issues: Issues finished" }));
  await openTable("Work time to merge");
  fireEvent.click(screen.getByRole("button", { name: "Show the issues of ACME-6" }));
  expect(selected).toEqual([
    [["ACME-6", "ACME-7"], "Finished in the range"],
    [["ACME-6"], "Work time: ACME-6"],
  ]);
});

test("a host that filters nothing gets no buttons in the tables", async () => {
  await renderPanel([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  await openTable("Work time to merge");
  expect(document.querySelector("[data-chart-select]")).toBeNull();
});

test("a project whose repositories are not the deploy repository has no CI or deploy measured", async () => {
  const { measures, pipeline } = await renderPanel([setting("ACME")], () => report(), {
    deployRepo: "acme/deploy",
    repos: [mapping("ACME", "acme/widgets")],
  });

  expect(await screen.findByText("No deploy measured for this project yet.")).toBeDefined();
  expect(screen.getByText("No CI measured for this project yet.")).toBeDefined();
  expect(pipeline).not.toHaveBeenCalled();
  expect(measures).not.toHaveBeenCalled();
});

test("a project that maps the deploy repository shows its CI and deploys for its repositories", async () => {
  const { measures, pipeline } = await renderPanel([setting("ACME")], () => report(), {
    deployRepo: "acme/deploy",
    repos: [
      mapping("ACME", "acme/deploy"),
      mapping("ACME", "acme/widgets"),
      mapping("CORE", "acme/core"),
    ],
  });

  await waitFor(() =>
    expect(
      document.querySelector('[data-health-card="deploy"] [data-health-figure="Deploys a day"]')
        ?.textContent
    ).toBe("2.0")
  );
  expect(
    document.querySelector('[data-health-card="ci"] [data-health-figure="Success"]')?.textContent
  ).toBe("75%");
  expect(pipeline).toHaveBeenCalledWith(
    expect.objectContaining({ repo: ["acme/deploy", "acme/widgets"] })
  );
  expect(measures).toHaveBeenCalledWith(
    expect.objectContaining({ repo: ["acme/deploy", "acme/widgets"] })
  );
});

test("the panel collapses and reads nothing more while closed", async () => {
  await renderPanel([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  fireEvent.click(screen.getByRole("button", { expanded: true }));
  expect(screen.queryByText("Work time to merge")).toBeNull();
  expect(screen.getByRole("button", { expanded: false })).toBeDefined();
});
