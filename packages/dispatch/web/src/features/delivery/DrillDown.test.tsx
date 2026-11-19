import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Agent, DeliveryPR, DeliveryRun } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DrillDown } from "./DrillDown";
import type { TimelineSelection } from "./Timeline";

const pr: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: "Planner one",
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: ["sess-planner-1", "sess-helper-2", "sess-untitled-3"],
  title: "feat: widget 1",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

const helper: Agent = {
  capabilities: [],
  dir: "/home/alice/legion",
  host: "devbox-alice",
  last_activity: null,
  last_seen: 1,
  machine_id: "devbox-alice",
  open_asks: 0,
  roles: [],
  session_id: "sess-helper-2",
  state: "running",
  title: "Helper",
};

function renderDrillDown(sessions: readonly string[]) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The agent registry, as the shell's poller last read it: only the helper is running.
  queryClient.setQueryData(["agents"], [helper]);
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DrillDown
            components={{}}
            onClose={() => {}}
            prs={[{ ...pr, sessions: [...sessions] }]}
            runs={[]}
            selection={{ id: pr.id, kind: "pr" }}
            sessionGroups={[]}
            sessionTitles={{
              "sess-planner-1": "Planner one",
              "sess-untitled-3": "sess-untitled-3",
            }}
          />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The `<dd>` that follows the `<dt>` named `term`. */
function detail(term: string): HTMLElement {
  const dt = within(screen.getByRole("dialog", { name: "Details" })).getByText(term);
  const dd = dt.nextElementSibling;
  if (!(dd instanceof HTMLElement)) throw new Error(`no value after ${term}`);
  return dd;
}

// A pull request's sessions are agents, each linked to its live conversation and named by its
// registry title, else the title the delivery read stored, else its short id (a stored title
// that is the session's own id is no title).
test("a pull request's sessions each link to their live conversation", () => {
  renderDrillDown(pr.sessions ?? []);
  try {
    const sessions = within(detail("Sessions"));
    const links = sessions.getAllByRole("link");
    expect(links.map((link) => [link.textContent, link.getAttribute("href")])).toEqual([
      ["Planner one", "/agents/sess-planner-1/live"],
      ["Helper", "/agents/sess-helper-2/live"],
      ["session:sess-unt…", "/agents/sess-untitled-3/live"],
    ]);
    expect(
      links.map((link) => link.closest("[data-agent-name]")?.getAttribute("data-agent-state"))
    ).toEqual(["not-running", "running", "not-running"]);
    // The Parent agent stays the facet value the server wrote, not an agent.
    expect(within(detail("Parent agent(s)")).queryByRole("link")).toBeNull();
    expect(detail("Parent agent(s)").textContent).toBe("Planner one");
  } finally {
    cleanup();
  }
});

test("a pull request with no sessions shows a dash", () => {
  renderDrillDown([]);
  try {
    expect(detail("Sessions").textContent).toBe("\u2014");
  } finally {
    cleanup();
  }
});

/** Run 500, a deploy that shipped #1, and run 501, a pipeline failure: each drill-down reads the
 *  run's every job (`GET /api/v1/delivery/runs/{id}`) under its Jobs or All jobs section. */
const deploy: DeliveryRun = {
  completed_at: "2024-06-01T01:40:00Z",
  conclusion: "success",
  failed_jobs: [],
  head_at: "2024-06-01T01:00:00Z",
  head_sha: "deadbeef",
  id: 500,
  production: { completed_at: "2024-06-01T01:40:00Z", conclusion: "success" },
  prs: [{ id: pr.id, title: pr.title }],
  root_failing_job: null,
  started_at: "2024-06-01T01:30:00Z",
  url: "https://github.com/acme/widgets/actions/runs/500",
};
const failure: DeliveryRun = {
  ...deploy,
  conclusion: "failure",
  failed_jobs: [{ completed_at: "2024-06-01T12:20:00Z", name: "integration tests" }],
  id: 501,
  production: null,
  prs: [],
  url: "https://github.com/acme/widgets/actions/runs/501",
};

/** The drill-down of `selection` over runs 500 and 501, signed in as a restricted member
 *  (`member_kind: "restricted"`) when `restricted` is set and as a full member otherwise. */
function renderRun(selection: TimelineSelection, restricted: boolean) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["agents"], []);
  if (restricted) {
    queryClient.setQueryData(whoAmIQuery().queryKey, {
      event_head: "0",
      kind: "user",
      login: "bob@outside.example",
      member_kind: "restricted",
    });
  }
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DrillDown
            components={{}}
            onClose={() => {}}
            prs={[pr]}
            runs={[deploy, failure]}
            selection={selection}
            sessionGroups={[]}
            sessionTitles={{}}
          />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// A run's jobs are the deployment's (`GET /api/v1/delivery/runs/{id}`, read class
// `deployment`), which Dispatch refuses a restricted member. Their drill-down of a run shows the
// run, its link and the pull requests it shipped that they see, and leaves its jobs out without
// asking for them. A full member's drill-down reads and lists them, as it always did.
test("a restricted member's run drill-down shows the run and what it shipped, and reads no jobs", async () => {
  const getDeliveryRun = spyOn(api, "getDeliveryRun").mockResolvedValue({
    id: 500,
    jobs: [
      {
        completed_at: "2024-06-01T01:40:00Z",
        conclusion: "success",
        name: "widgets-release / widgets-release",
        started_at: "2024-06-01T01:32:00Z",
      },
    ],
    url: deploy.url,
  });
  try {
    for (const selection of [
      { id: 500, kind: "deploy" },
      { id: 501, kind: "failure" },
    ] as const) {
      renderRun(selection, true);
      const details = within(screen.getByRole("dialog", { name: "Details" }));
      expect(details.getByRole("link", { name: "Open run" }).getAttribute("href")).toBe(
        selection.kind === "deploy" ? deploy.url : failure.url
      );
      expect(details.queryByText("Jobs")).toBeNull();
      expect(details.queryByText("All jobs")).toBeNull();
      expect(details.queryByText("Loading…")).toBeNull();
      cleanup();
    }
    renderRun({ id: 500, kind: "deploy" }, true);
    const shipped = within(screen.getByRole("dialog", { name: "Details" }));
    expect(shipped.getByRole("heading", { name: "Deploy: run #500" })).toBeDefined();
    expect(shipped.getByText("PRs shipped (1)")).toBeDefined();
    expect(shipped.getByText(`${pr.id} \u2014 ${pr.title}`)).toBeDefined();
    expect(getDeliveryRun).not.toHaveBeenCalled();
    cleanup();

    renderRun({ id: 500, kind: "deploy" }, false);
    const full = within(screen.getByRole("dialog", { name: "Details" }));
    expect(await full.findByText("widgets-release / widgets-release")).toBeDefined();
    expect(full.getByText("Jobs")).toBeDefined();
    expect(getDeliveryRun).toHaveBeenCalledWith(500);
  } finally {
    cleanup();
    getDeliveryRun.mockRestore();
  }
});

// A refusal of the jobs read (403 DEPLOYMENT_READ_REFUSED) reaches any viewer whose credential
// changed while the page was open: the drill-down leaves the jobs out, as it does for a member
// who is never asked, rather than saying they could not be read. Any other failure still says so.
test("a run whose jobs read is refused draws no jobs section and no could-not-be-read line", async () => {
  const getDeliveryRun = spyOn(api, "getDeliveryRun").mockRejectedValue(
    new ApiError(403, {
      code: "DEPLOYMENT_READ_REFUSED",
      error: "this read is the deployment's, which a restricted member may not make",
    })
  );
  try {
    renderRun({ id: 500, kind: "deploy" }, false);
    const details = within(screen.getByRole("dialog", { name: "Details" }));
    await waitFor(() => expect(getDeliveryRun).toHaveBeenCalledWith(500));
    await waitFor(() => expect(details.queryByText("Loading…")).toBeNull());
    expect(details.queryByText("The run's jobs could not be read.")).toBeNull();
    expect(details.queryByText("Jobs")).toBeNull();
    expect(details.getByText("PRs shipped (1)")).toBeDefined();
    cleanup();

    getDeliveryRun.mockRejectedValue(new ApiError(500, { code: "INTERNAL", error: "down" }));
    renderRun({ id: 500, kind: "deploy" }, false);
    expect(await screen.findByText("The run's jobs could not be read.")).toBeDefined();
  } finally {
    cleanup();
    getDeliveryRun.mockRestore();
  }
});
