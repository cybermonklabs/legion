import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { runRow } from "../../__tests__/timeline-fixture";
import { ApiError, api } from "../../api/client";
import type {
  Agent,
  DeliveryPR,
  ProductionEvent,
  ProjectDeployRunView,
  ProjectTimelineRunRow,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DrillDown } from "./DrillDown";

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
            project="ACME"
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

// The deploy's drill-down on project ACME's tab (dispatch://INFRA-2918, D14): its run read under
// the project, and the record's controls, each row of the plan's "The state table of the
// drill-down's controls and the P0 link" a test, with the reads and writes answered by a fake
// client in the order the row names.

/** Every spy a test installed, put back when it ends. */
const spies: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** Deploy run 41 as the project timeline's row names it: in acme/deploys, shipping two pull
 *  requests. */
const deployRun: ProjectTimelineRunRow = {
  ...runRow(41, "2026-11-23T09:00:00Z", "deploy"),
  prs: [
    { id: "acme/widgets#1", title: "feat: widget 1" },
    { id: "acme/widgets#2", title: "fix: widget 2" },
  ],
};

/** A row of dispatch://INFRA-978's record, a deploy unless `overrides` says otherwise. */
function productionEvent(overrides: Partial<ProductionEvent>): ProductionEvent {
  return {
    actor: { id: "deploy-bot", kind: "service" },
    commits: [],
    created_at: "2026-11-23T09:10:00Z",
    deploy_id: null,
    detected_at: null,
    environment: "production",
    finished_at: "2026-11-23T09:10:00Z",
    id: "deploy-41",
    kind: "deploy",
    note: "",
    project_key: "SHIP",
    pull_requests: ["acme/widgets#1", "acme/widgets#2"],
    repo: "acme/deploys",
    restored_at: null,
    restored_by: null,
    restored_by_deploy: null,
    signal: null,
    source: "github",
    source_ref: "acme/deploys#41",
    started_at: "2026-11-23T09:00:00Z",
    state: "counted",
    state_at: null,
    state_by: null,
    state_reason: null,
    status: "success",
    url: deployRun.url,
    ...overrides,
  };
}

/** The deploy row the GitHub adapter wrote for run 41, under project SHIP, whose deploy source
 *  reads acme/deploys: another project than the tab's. */
const DEPLOY = productionEvent({});

/** A failure `id` that blames run 41's deploy: a person's flag under the deploy row's project
 *  unless `overrides` says otherwise. */
function failure(id: string, overrides: Partial<ProductionEvent> = {}): ProductionEvent {
  return productionEvent({
    actor: { id: "alice", kind: "user" },
    deploy_id: DEPLOY.id,
    detected_at: "2026-11-23T10:00:00Z",
    finished_at: null,
    id,
    kind: "failure",
    pull_requests: ["acme/widgets#2"],
    signal: "person",
    source: "person",
    source_ref: `person:${DEPLOY.id}:alice`,
    started_at: null,
    status: "",
    url: "",
    ...overrides,
  });
}

/** A check's failure, written under ACME, the project of the issue its check ran for. */
const CHECK_FAILURE = failure("failure-check", {
  actor: { id: "check-results", kind: "service" },
  project_key: "ACME",
  signal: "check_fail",
  source: "check_results",
  source_ref: "acme/widgets#2/checks/e2e",
});

/** The person reading the page, who writes the record. */
const BOB = { id: "bob", kind: "user" } as const;

/** The run's record as Dispatch holds it. A test changes it as the write it answers would, so the
 *  read that write settles into answers what the write did. */
interface RecordServer {
  deploy: ProductionEvent | null;
  failures: ProductionEvent[];
}

/** The deploy-runs read's answer over what `server` holds now. */
function runAnswer(server: RecordServer): ProjectDeployRunView {
  return {
    deploy: server.deploy,
    failures: [...server.failures],
    id: deployRun.id,
    jobs: [
      {
        completed_at: "2026-11-23T09:08:00Z",
        conclusion: "success",
        name: "deploy",
        started_at: "2026-11-23T09:01:00Z",
      },
    ],
    url: deployRun.url,
  };
}

/** The drill-down of deploy run 41 on ACME's tab, read by bob, a signed-in person, each run read
 *  answering what `server` holds then. */
function renderDeploy(server: RecordServer) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: BOB.id,
  });
  const read = spyOn(api, "getProjectDeployRun").mockImplementation(async () => runAnswer(server));
  const post = spyOn(api, "postProductionEvent");
  const put = spyOn(api, "putProductionEventState");
  spies.push(whoAmI, read, post, put);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  spies.push({ mockRestore: () => queryClient.clear() });
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DrillDown
            components={{}}
            onClose={() => {}}
            project="ACME"
            prs={[]}
            runs={[deployRun]}
            selection={{ id: deployRun.id, kind: "deploy" }}
            sessionGroups={[]}
            sessionTitles={{}}
          />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { post, put, read };
}

/** The deploy's record, once its read has answered and a person's controls are drawn. */
async function record(): Promise<HTMLElement> {
  const group = await screen.findByRole("group", { name: /^Failed changes/ });
  await waitFor(() => expect(within(group).queryAllByRole("button").length).toBeGreaterThan(0));
  return group;
}

/** The listed failure `id`. */
function failureRow(id: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-failure="${id}"]`);
  if (row === null) throw new Error(`no failure ${id} is listed`);
  return row;
}

/** Every control of the record: its buttons, check boxes and fields. */
function controls(group: HTMLElement): HTMLElement[] {
  return [...group.querySelectorAll<HTMLElement>("button, input, textarea")];
}

const NOTE = "The widget page answers 500 since this deploy.";

/** The flag form, opened, with #2 ticked and the note written: the press Dispatch has not answered
 *  yet, its write held until the test answers it. */
async function flagPending() {
  const user = userEvent.setup();
  const server: RecordServer = { deploy: DEPLOY, failures: [] };
  const calls = renderDeploy(server);
  const group = await record();
  await user.click(within(group).getByRole("button", { name: "Flag" }));
  const form = within(group).getByRole("form", { name: "Flag this deploy" });
  await user.click(
    within(form).getByRole("checkbox", { name: "acme/widgets#2 \u2014 fix: widget 2" })
  );
  await user.type(within(form).getByRole("textbox", { name: "Note" }), NOTE);
  const written = Promise.withResolvers<ProductionEvent>();
  calls.post.mockImplementation(() => written.promise);
  await user.click(within(form).getByRole("button", { name: "Flag this deploy" }));
  await waitFor(() => expect(calls.post).toHaveBeenCalledTimes(1));
  return { ...calls, form, group, server, user, written };
}

test("the run is read under the tab's project from the repository its timeline row names", async () => {
  const { read } = renderDeploy({ deploy: DEPLOY, failures: [] });
  await record();
  expect(read.mock.calls).toEqual([["ACME", 41, "acme/deploys"]]);
  expect(screen.getByText("deploy")).toBeInTheDocument();
  expect(screen.getByText("success (420s)")).toBeInTheDocument();
});

test("a run with no deploy row offers no Flag", async () => {
  renderDeploy({ deploy: null, failures: [] });
  const group = await screen.findByRole("group", { name: /^Failed changes/ });
  expect(
    within(group).getByText("No deploy row records this run, so it cannot be flagged.")
  ).toBeInTheDocument();
  expect(within(group).queryByRole("button", { name: "Flag" })).toBeNull();
});

const controlRows: [string, () => Promise<void>][] = [
  [
    "idle | Flag pressed | form: the deploy's shipped pull requests to tick, a note",
    async () => {
      const user = userEvent.setup();
      renderDeploy({ deploy: DEPLOY, failures: [] });
      const group = await record();
      await user.click(within(group).getByRole("button", { name: "Flag" }));
      const form = within(group).getByRole("form", { name: "Flag this deploy" });
      const boxes = within(form).getAllByRole("checkbox");
      expect(boxes.map((box) => box.closest("label")?.textContent)).toEqual([
        "acme/widgets#1 \u2014 feat: widget 1",
        "acme/widgets#2 \u2014 fix: widget 2",
      ]);
      expect(boxes.every((box) => !(box as HTMLInputElement).checked)).toBe(true);
      expect(within(form).getByRole("textbox", { name: "Note" })).toHaveValue("");
    },
  ],
  [
    "form | submit with no pull request ticked | form: submit disabled",
    async () => {
      const user = userEvent.setup();
      const { post } = renderDeploy({ deploy: DEPLOY, failures: [] });
      const group = await record();
      await user.click(within(group).getByRole("button", { name: "Flag" }));
      const form = within(group).getByRole("form", { name: "Flag this deploy" });
      await user.type(within(form).getByRole("textbox", { name: "Note" }), NOTE);
      const submit = within(form).getByRole("button", { name: "Flag this deploy" });
      expect(submit).toHaveAttribute("aria-disabled", "true");
      await user.click(submit);
      expect(post).not.toHaveBeenCalled();
      expect(within(group).getByRole("form", { name: "Flag this deploy" })).toBe(form);
    },
  ],
  [
    "form | submit | pending: every control disabled, the form kept",
    async () => {
      const { form, group, post } = await flagPending();
      // The flag goes to the deploy row's project, naming the run, its repository, the pull
      // request ticked and the note.
      expect(post.mock.calls[0]?.slice(0, 2)).toEqual([
        "SHIP",
        {
          "cicd.pipeline.run.id": "41",
          kind: "failure",
          note: NOTE,
          pull_requests: ["acme/widgets#2"],
          repo: "acme/deploys",
        },
      ]);
      expect(controls(group).length).toBeGreaterThan(0);
      for (const control of controls(group)) expect(control).toBeDisabled();
      expect(within(group).getByRole("form", { name: "Flag this deploy" })).toBe(form);
      expect(within(form).getByRole("textbox", { name: "Note" })).toHaveValue(NOTE);
      expect(
        within(form).getByRole("checkbox", { name: "acme/widgets#2 \u2014 fix: widget 2" })
      ).toBeChecked();
      expect(within(form).getByRole("button", { name: "Flagging…" })).toBeDisabled();
    },
  ],
  [
    "pending | write answered 201 or 200 | re-reading: the run detail read again; then idle with the new failure row (signal person, the pull requests, the note, the person)",
    async () => {
      const { form, group, read, server, written } = await flagPending();
      const reread = Promise.withResolvers<ProjectDeployRunView>();
      read.mockImplementation(() => reread.promise);
      const flag = failure("failure-bob", {
        actor: BOB,
        note: NOTE,
        source_ref: `person:${DEPLOY.id}:bob`,
      });
      server.failures.push(flag);
      act(() => written.resolve(flag));
      // Re-reading: the run is read again, and the form and its disabled controls stay until
      // that read answers.
      await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
      expect(within(group).getByRole("form", { name: "Flag this deploy" })).toBe(form);
      for (const control of controls(group)) expect(control).toBeDisabled();

      act(() => reread.resolve(runAnswer(server)));
      await waitFor(() =>
        expect(within(group).queryByRole("form", { name: "Flag this deploy" })).toBeNull()
      );
      const row = failureRow("failure-bob");
      expect(row).toHaveAttribute("data-signal", "person");
      expect(row).toHaveAttribute("data-state", "counted");
      expect(row.textContent).toContain("A person's flag by bob");
      expect(row.querySelector("[data-blamed]")?.textContent).toBe("Pull requests: acme/widgets#2");
      // The note, read back from the deploy-runs answer.
      expect(row.querySelector("[data-note]")?.textContent).toBe(`Note: ${NOTE}`);
      // Idle: every control can be pressed again.
      for (const control of controls(group)) expect(control).toBeEnabled();
      expect(within(group).getByRole("button", { name: "Flag" })).toBeEnabled();
    },
  ],
  [
    "pending | write refused 403 or 409 | form: the refusal's error text shown, nothing re-read",
    async () => {
      const { form, group, post, read, user, written } = await flagPending();
      act(() =>
        written.reject(
          new ApiError(403, { code: "FORBIDDEN", error: "a flag is a person's: sign in" })
        )
      );
      await waitFor(() =>
        expect(within(group).getByRole("alert").textContent).toBe(
          "Couldn't flag this deploy — a flag is a person's: sign in"
        )
      );
      expect(within(group).getByRole("form", { name: "Flag this deploy" })).toBe(form);
      expect(within(form).getByRole("textbox", { name: "Note" })).toHaveValue(NOTE);
      for (const control of controls(group)) expect(control).toBeEnabled();

      post.mockRejectedValue(
        new ApiError(409, {
          code: "PRODUCTION_EVENT_NO_DEPLOY",
          error: "no deploy row is run 41 of acme/deploys",
        })
      );
      await user.click(within(form).getByRole("button", { name: "Flag this deploy" }));
      await waitFor(() =>
        expect(within(group).getByRole("alert").textContent).toBe(
          "Couldn't flag this deploy — no deploy row is run 41 of acme/deploys"
        )
      );
      expect(post).toHaveBeenCalledTimes(2);
      expect(within(group).getByRole("form", { name: "Flag this deploy" })).toBe(form);
      // Nothing was read again.
      expect(read).toHaveBeenCalledTimes(1);
    },
  ],
  [
    "idle, a person's counted failure | Mark restored | pending, then re-reading: the row shows when and by whom it was restored",
    async () => {
      const user = userEvent.setup();
      const flag = failure("failure-alice");
      const server: RecordServer = { deploy: DEPLOY, failures: [flag, CHECK_FAILURE] };
      const { put, read } = renderDeploy(server);
      const group = await record();
      // Mark restored is a person's flag's alone; the check's failure has Reject only.
      expect(
        within(failureRow("failure-check"))
          .getAllByRole("button")
          .map((button) => button.textContent)
      ).toEqual(["Reject"]);
      const written = Promise.withResolvers<ProductionEvent>();
      put.mockImplementation(() => written.promise);
      await user.click(
        within(failureRow("failure-alice")).getByRole("button", { name: "Mark restored" })
      );
      await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
      expect(put.mock.calls[0]?.slice(0, 3)).toEqual([
        "SHIP",
        "failure-alice",
        { state: "restored" },
      ]);
      for (const control of controls(group)) expect(control).toBeDisabled();

      const restored = {
        ...flag,
        restored_at: "2026-11-23T11:00:00Z",
        restored_by: BOB,
      };
      server.failures = [restored, CHECK_FAILURE];
      act(() => written.resolve(restored));
      await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
      await waitFor(() =>
        expect(failureRow("failure-alice").querySelector("[data-restored]")?.textContent).toBe(
          `Restored ${new Date("2026-11-23T11:00:00Z").toLocaleString()} by bob`
        )
      );
      expect(
        within(failureRow("failure-alice")).queryByRole("button", { name: "Mark restored" })
      ).toBeNull();
      for (const control of controls(group)) expect(control).toBeEnabled();
    },
  ],
  [
    "idle, any failure | Reject pressed | reject form: a reason, submit disabled while empty",
    async () => {
      const user = userEvent.setup();
      const { put } = renderDeploy({
        deploy: DEPLOY,
        failures: [failure("failure-alice"), CHECK_FAILURE],
      });
      await record();
      // Every failure has Reject, whatever signal made it.
      for (const id of ["failure-alice", "failure-check"]) {
        expect(within(failureRow(id)).getByRole("button", { name: "Reject" })).toBeEnabled();
      }
      await user.click(within(failureRow("failure-check")).getByRole("button", { name: "Reject" }));
      const form = within(failureRow("failure-check")).getByRole("form", {
        name: "Reject this failure",
      });
      const reason = within(form).getByRole("textbox", { name: "Reason" });
      expect(reason).toHaveValue("");
      const submit = within(form).getByRole("button", { name: "Reject this failure" });
      expect(submit).toHaveAttribute("aria-disabled", "true");
      await user.type(reason, "   ");
      expect(submit).toHaveAttribute("aria-disabled", "true");
      await user.click(submit);
      expect(put).not.toHaveBeenCalled();
    },
  ],
  [
    "reject form | submit with a reason | pending, then re-reading: the row shows rejected, its reason and its writer, and stays listed",
    async () => {
      const user = userEvent.setup();
      const server: RecordServer = {
        deploy: DEPLOY,
        failures: [failure("failure-alice"), CHECK_FAILURE],
      };
      const { put, read } = renderDeploy(server);
      const group = await record();
      await user.click(within(failureRow("failure-check")).getByRole("button", { name: "Reject" }));
      const form = within(group).getByRole("form", { name: "Reject this failure" });
      const why = "The e2e check flaked; the deploy was fine.";
      await user.type(within(form).getByRole("textbox", { name: "Reason" }), why);
      const written = Promise.withResolvers<ProductionEvent>();
      put.mockImplementation(() => written.promise);
      await user.click(within(form).getByRole("button", { name: "Reject this failure" }));
      await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
      // The reject goes to the failure's own project, with its reason.
      expect(put.mock.calls[0]?.slice(0, 3)).toEqual([
        "ACME",
        "failure-check",
        { reason: why, state: "rejected" },
      ]);
      for (const control of controls(group)) expect(control).toBeDisabled();

      const rejected: ProductionEvent = {
        ...CHECK_FAILURE,
        state: "rejected",
        state_at: "2026-11-23T11:30:00Z",
        state_by: BOB,
        state_reason: why,
      };
      server.failures = [server.failures[0] as ProductionEvent, rejected];
      act(() => written.resolve(rejected));
      await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
      await waitFor(() =>
        expect(failureRow("failure-check")).toHaveAttribute("data-state", "rejected")
      );
      expect(failureRow("failure-check").querySelector("[data-rejected]")?.textContent).toBe(
        `Rejected by bob: ${why}`
      );
      expect(within(group).queryByRole("form", { name: "Reject this failure" })).toBeNull();
      // It stays listed beside the other failure.
      expect(
        [...group.querySelectorAll("[data-failure]")].map((row) => row.getAttribute("data-failure"))
      ).toEqual(["failure-alice", "failure-check"]);
    },
  ],
  [
    "idle, a rejected failure | (none) | no Reject and no Mark restored on it",
    async () => {
      renderDeploy({
        deploy: DEPLOY,
        failures: [
          failure("failure-alice", {
            state: "rejected",
            state_at: "2026-11-23T11:30:00Z",
            state_by: BOB,
            state_reason: "Flagged the wrong deploy.",
          }),
          CHECK_FAILURE,
        ],
      });
      await record();
      expect(within(failureRow("failure-alice")).queryAllByRole("button")).toEqual([]);
      expect(failureRow("failure-alice").querySelector("[data-rejected]")?.textContent).toBe(
        "Rejected by bob: Flagged the wrong deploy."
      );
      // The counted failure beside it keeps its Reject.
      expect(
        within(failureRow("failure-check")).getByRole("button", { name: "Reject" })
      ).toBeEnabled();
    },
  ],
];

test.each(controlRows)("%s", async (_row, run) => {
  await run();
});
