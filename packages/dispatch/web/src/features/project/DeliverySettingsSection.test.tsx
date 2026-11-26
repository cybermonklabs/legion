import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { DISPATCH_LOGIN_HEADER } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { MemoryRouter } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { seedDeliveryReads } from "../../__tests__/live-socket-harness";
import { answerRequests } from "../../__tests__/network";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { keepPageSession } from "../../api/agent-session";
import { ApiError, api, NetworkError } from "../../api/client";
import {
  evalDetailQuery,
  evalsAcrossProjectsQuery,
  evalsViewQuery,
  itemsViewQuery,
  projectTimelineViewQuery,
} from "../../api/queries";
import type {
  AuthenticatedUser,
  ProjectDeliverySettings,
  ProjectDeploySource,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { DeliverySettingsSection } from "./DeliverySettingsSection";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// The editor's states (dispatch://INFRA-978): one test per state and event, each driving the
// event on the state and reading what then shows.

const PROJECT = "ACME";
const DEFAULT_REPO = "acme/widgets";
const SAVE = "Save delivery settings";

/** The seven target boxes, in the order the section lists them. */
const TARGET_FIELDS = [
  "Deploy frequency (deploys a day)",
  "Change failure rate (a share from 0 to 1)",
  "Merge to production (minutes)",
  "PR open to merge (minutes)",
  "Deploy runs reaching production (a share from 0 to 1)",
  "Failed deployment recovery time (minutes)",
  "P0 issues with no owner",
];

/** The four failure sources' boxes. */
const FAILURE_FIELDS = [
  "A check that fails after its merge",
  "A merged revert",
  "An issue labelled incident",
  "A failure posted to the project's production events",
];

function githubSource(overrides: Partial<ProjectDeploySource> = {}): ProjectDeploySource {
  return {
    deploy_workflow_path: ".github/workflows/deploy.yml",
    id: "source-1",
    kind: "github_workflow",
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    pr_checks_rollup_job_name: null,
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_apply_step_name: null,
    production_job_names: ["widgets-release / widgets-release", "release / release"],
    reconciled_through: null,
    repo: null,
    resolved_repo: DEFAULT_REPO,
    staging_apply_job_name: null,
    staging_e2e_job_name: null,
    ...overrides,
  };
}

const eventsSource: ProjectDeploySource = {
  deploy_workflow_path: null,
  id: "source-events",
  kind: "events",
  last_error: null,
  last_event_at: null,
  last_reconcile_at: null,
  pr_checks_rollup_job_name: null,
  pr_checks_workflow_path: null,
  production_apply_step_name: null,
  production_job_names: null,
  reconciled_through: null,
  repo: null,
  resolved_repo: null,
  staging_apply_job_name: null,
  staging_e2e_job_name: null,
};

/** A project with delivery on: one source reading the default repository, two failure sources,
 *  and the targets INFRA-2916 seeds. */
const record: ProjectDeliverySettings = {
  default_repo: DEFAULT_REPO,
  delivery: true,
  excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
  failure_sources: ["check_results", "reverts"],
  failures_measured_since: "2026-11-01",
  population_authors: ["octocat", "octocat-agent[bot]"],
  project: PROJECT,
  sources: [githubSource()],
  targets: {
    "change-fail-rate": 0.05,
    "deploy-frequency": 20,
    "deploy-run-success": 0.9,
    "failed-deploy-recovery": "off",
    "merge-to-production": 45,
    "pr-open-to-merge": 60,
    "unowned-p0": 0,
  },
  updated_at: "2026-11-20T12:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

/** `record` as a later save stored it. */
function savedLater(overrides: Partial<ProjectDeliverySettings> = {}): ProjectDeliverySettings {
  return { ...record, updated_at: "2026-11-21T09:00:00Z", ...overrides };
}

/** The section of PROJECT, drawn for `viewer` (`/auth/whoami`'s answer) where one is given, which
 *  is in the cache before the first render. */
function renderSection(
  defaultRepo: string | null = DEFAULT_REPO,
  viewer?: AuthenticatedUser
): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (viewer !== undefined) queryClient.setQueryData<AuthenticatedUser>(["whoami"], viewer);
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DeliverySettingsSection defaultRepo={defaultRepo} project={PROJECT} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

function field(label: string): HTMLInputElement | HTMLTextAreaElement {
  return screen.getByLabelText(label) as HTMLInputElement | HTMLTextAreaElement;
}

/** Deploy source `n` (from 1), as its legend names it. */
function source(n: number): HTMLElement {
  return screen.getByRole("group", { name: `Deploy source ${n}` });
}

function sourceField(n: number, label: string): HTMLInputElement | HTMLTextAreaElement {
  return within(source(n)).getByLabelText(label) as HTMLInputElement | HTMLTextAreaElement;
}

function deliverySwitch(): HTMLInputElement {
  return screen.getByRole("switch", { name: "Delivery" }) as HTMLInputElement;
}

/** Fills the empty `input` with `value`, pasted in one edit as a person pastes a draft. */
async function fill(user: UserEvent, input: HTMLElement, value: string): Promise<void> {
  await user.click(input);
  await user.paste(value);
}

/** Replaces what `input` holds with `value`. */
async function replace(user: UserEvent, input: HTMLElement, value: string): Promise<void> {
  await user.clear(input);
  await user.click(input);
  await user.paste(value);
}

/** The required fields of deploy source `n`, filled. */
async function fillRule(user: UserEvent, n: number): Promise<void> {
  await fill(user, sourceField(n, "Deploy workflow"), ".github/workflows/deploy.yml");
  await fill(user, sourceField(n, "Production jobs"), "widgets-release / widgets-release");
  await fill(user, sourceField(n, "PR checks workflow"), ".github/workflows/pr-checks.yml");
}

/** The body of the `call`th save (from 0). */
function sentBody(put: { readonly mock: { readonly calls: readonly unknown[][] } }, call = 0) {
  return put.mock.calls[call]?.[1];
}

test("the section says it is loading until the record arrives", async () => {
  const answer = Promise.withResolvers<ProjectDeliverySettings | null>();
  const get = spyOn(api, "getProjectDeliverySettings").mockImplementation(() => answer.promise);

  try {
    renderSection();

    expect(await screen.findByText("Loading delivery settings…")).toBeDefined();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: SAVE })).not.toBeInTheDocument();
    expect(get).toHaveBeenCalledWith(PROJECT);

    answer.resolve(null);
    expect(await screen.findByRole("switch", { name: "Delivery" })).not.toBeChecked();
    expect(screen.queryByText("Loading delivery settings…")).not.toBeInTheDocument();
  } finally {
    cleanup();
    get.mockRestore();
  }
});

test("a failed load offers a retry, and the retry loads", async () => {
  const user = userEvent.setup();
  const retried = Promise.withResolvers<ProjectDeliverySettings | null>();
  const get = spyOn(api, "getProjectDeliverySettings")
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "database unavailable" }))
    .mockImplementationOnce(() => retried.promise);

  try {
    renderSection();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't load the delivery settings.");
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Loading delivery settings…")).toBeDefined();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(get).toHaveBeenCalledTimes(2);

    retried.resolve(record);
    expect(await screen.findByRole("switch", { name: "Delivery" })).toBeChecked();
  } finally {
    cleanup();
    get.mockRestore();
  }
});

test("with no record the switch is off and no field or target shows", async () => {
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(null);

  try {
    renderSection();

    expect(await screen.findByRole("switch", { name: "Delivery" })).not.toBeChecked();
    const defaultRepo = field("Default repository") as HTMLInputElement;
    expect(defaultRepo.value).toBe(DEFAULT_REPO);
    expect(defaultRepo.readOnly).toBe(true);
    expect(screen.queryByRole("group", { name: "Deploy sources" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Repository")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Deploy workflow")).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Failure sources" })).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Targets" })).not.toBeInTheDocument();
    for (const label of TARGET_FIELDS) {
      expect(screen.queryByLabelText(label)).not.toBeInTheDocument();
    }
    expect(screen.getByRole("button", { name: SAVE })).toBeEnabled();
  } finally {
    cleanup();
    get.mockRestore();
  }
});

test("with delivery off Save keeps the stored fields", async () => {
  const user = userEvent.setup();
  const stored: ProjectDeliverySettings = {
    ...record,
    delivery: false,
    sources: [
      githubSource({
        production_apply_step_name: "Apply",
        repo: "acme/deploys",
        resolved_repo: "acme/deploys",
      }),
      eventsSource,
    ],
  };
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(stored);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(stored);

  try {
    renderSection();

    expect(await screen.findByRole("switch", { name: "Delivery" })).not.toBeChecked();
    expect(screen.queryByRole("group", { name: "Deploy sources" })).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Targets" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(put.mock.calls[0]?.[0]).toBe(PROJECT);
    // Every stored field goes back as stored, and no target: each eval keeps its own.
    expect(sentBody(put)).toEqual({
      delivery: false,
      excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
      failure_sources: ["check_results", "reverts"],
      failures_measured_since: "2026-11-01",
      population_authors: ["octocat", "octocat-agent[bot]"],
      sources: [
        {
          deploy_workflow_path: ".github/workflows/deploy.yml",
          kind: "github_workflow",
          pr_checks_rollup_job_name: null,
          pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
          production_apply_step_name: "Apply",
          production_job_names: ["widgets-release / widgets-release", "release / release"],
          repo: "acme/deploys",
          staging_apply_job_name: null,
          staging_e2e_job_name: null,
        },
        { kind: "events" },
      ],
    });
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("turning delivery on shows the source, failure and target fields", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(null);

  try {
    renderSection();
    await user.click(await screen.findByRole("switch", { name: "Delivery" }));

    expect(deliverySwitch()).toBeChecked();
    // One source, a GitHub workflow, every field empty with a placeholder saying what it holds.
    expect(screen.queryByRole("group", { name: "Deploy source 2" })).not.toBeInTheDocument();
    expect((within(source(1)).getByLabelText("Kind") as HTMLSelectElement).value).toBe(
      "github_workflow"
    );
    const placeholders: [string, string][] = [
      ["Repository", DEFAULT_REPO],
      ["Deploy workflow", ".github/workflows/<deploy>.yml"],
      ["Production jobs", "One job name per line, as a deploy run lists it"],
      ["Production apply step", "The step whose success means the job applied (optional)"],
      ["Staging end-to-end job", "The staging end-to-end job, as a deploy run lists it (optional)"],
      ["Staging deploy job", "The staging deploy job (optional)"],
      ["PR checks workflow", ".github/workflows/<checks>.yml"],
      [
        "PR checks roll-up job",
        "The PR-checks roll-up job the failing-jobs table leaves out (optional)",
      ],
    ];
    for (const [label, placeholder] of placeholders) {
      const input = sourceField(1, label);
      expect(input.value).toBe("");
      expect(input.placeholder).toBe(placeholder);
    }
    expect(sourceField(1, "Staging end-to-end job").required).toBe(false);
    expect(field("Population authors").placeholder).toBe("One GitHub login per line");
    expect(field("Excluded repositories").placeholder).toBe("One owner/repo per line");

    const failures = screen.getByRole("group", { name: "Failure sources" });
    for (const label of FAILURE_FIELDS) {
      expect(within(failures).getByLabelText(label)).not.toBeChecked();
    }
    expect(within(failures).getByLabelText("Failures counted from")).toHaveValue("");

    const targets = screen.getByRole("group", { name: "Targets" });
    expect(within(targets).getAllByRole("textbox")).toHaveLength(TARGET_FIELDS.length);
    for (const label of TARGET_FIELDS) {
      const input = within(targets).getByLabelText(label) as HTMLInputElement;
      expect(input.value).toBe("");
      expect(input.placeholder).toBe("A number, or off");
    }
  } finally {
    cleanup();
    get.mockRestore();
  }
});

test("the events source shows no workflow field", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(null);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);

  try {
    renderSection();
    await user.click(await screen.findByRole("switch", { name: "Delivery" }));
    await user.selectOptions(within(source(1)).getByLabelText("Kind"), "events");

    for (const label of [
      "Repository",
      "Deploy workflow",
      "Production jobs",
      "PR checks workflow",
    ]) {
      expect(within(source(1)).queryByLabelText(label)).not.toBeInTheDocument();
    }
    expect(source(1).textContent).toContain(
      "Counts the deploys posted to POST /api/v1/projects/ACME/production-events"
    );
    expect(screen.getByRole("group", { name: "Failure sources" })).toBeDefined();
    expect(screen.getByRole("group", { name: "Targets" })).toBeDefined();

    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(sentBody(put)).toMatchObject({ delivery: true, sources: [{ kind: "events" }] });
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("a target that is neither a number nor off is refused before any request", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    const target = field("Deploy frequency (deploys a day)");
    expect(target.value).toBe("20");

    await replace(user, target, "fast");
    await user.click(screen.getByRole("button", { name: SAVE }));
    expect(screen.getByRole("alert").textContent).toBe(
      "Deploy frequency (deploys a day): a target is a number, or off."
    );
    expect(target).toHaveAttribute("aria-invalid", "true");
    expect(put).not.toHaveBeenCalled();

    // `off` is a target too, and the same button then saves it beside the stored numbers.
    await replace(user, target, " off ");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(sentBody(put)).toMatchObject({
      targets: { ...record.targets, "deploy-frequency": "off" },
    });
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("the fields are disabled while the save is in flight", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<ProjectDeliverySettings>();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockImplementation(() => answer.promise);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });

    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    const controls = [
      deliverySwitch(),
      sourceField(1, "Repository"),
      sourceField(1, "Deploy workflow"),
      within(source(1)).getByLabelText("Kind"),
      within(source(1)).getByRole("button", { name: "Remove deploy source 1" }),
      screen.getByRole("button", { name: "Add a GitHub workflow source" }),
      field("Population authors"),
      screen.getByLabelText(FAILURE_FIELDS[0] ?? ""),
      field("Failures counted from"),
      field("Deploy frequency (deploys a day)"),
      screen.getByRole("button", { name: SAVE }),
    ];
    for (const control of controls) {
      expect(control).toBeDisabled();
    }

    answer.resolve(record);
    await waitFor(() => expect(screen.getByRole("button", { name: SAVE })).toBeEnabled());
    expect(deliverySwitch()).toBeEnabled();
    expect(sourceField(1, "Repository")).toBeEnabled();
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

/** The Evals tab's reads of PROJECT, keyed and marked (`meta`) by the tab's own query options,
 *  and a page of the cross-project read, each held in `queryClient`'s cache with data and no
 *  observer, so a test reads which of them a save invalidates. The live views are kept past their
 *  `gcTime` of 0. */
function seedEvalsReads(queryClient: QueryClient): {
  across: readonly unknown[];
  detail: readonly unknown[];
  live: readonly (readonly unknown[])[];
} {
  const window = { preset: 28 } as const;
  const views = [
    evalsViewQuery({
      compare: null,
      kind: "delivery",
      offset: 0,
      project: PROJECT,
      repos: [],
      window,
    }),
    itemsViewQuery({
      compare: null,
      limit: 200,
      project: PROJECT,
      repos: [],
      slug: "pr-ci",
      window,
    }),
    projectTimelineViewQuery({ project: PROJECT, query: "", window }),
  ];
  const across: readonly unknown[] = evalsAcrossProjectsQuery({
    compare: null,
    offset: 0,
    repos: [],
    window: { from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" },
  }).queryKey;
  const detail: readonly unknown[] = evalDetailQuery(PROJECT, "deploy-frequency", 1).queryKey;
  const cache = queryClient.getQueryCache();
  const live = views.map((view) => {
    const key: readonly unknown[] = view.queryKey;
    cache.build(queryClient, { queryKey: key, meta: view.meta, gcTime: Number.POSITIVE_INFINITY });
    queryClient.setQueryData(key, {});
    return key;
  });
  for (const key of [across, detail]) {
    cache.build(queryClient, { queryKey: key });
    queryClient.setQueryData(key, {});
  }
  return { across, detail, live };
}

test("a save restarts the fields from the stored record and refreshes the evals", async () => {
  const user = userEvent.setup();
  const stored = savedLater({ sources: [githubSource({ id: "source-stored" })] });
  // The record's read after the save answers what the save stored.
  const get = spyOn(api, "getProjectDeliverySettings")
    .mockResolvedValueOnce(null)
    .mockResolvedValue(stored);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(stored);

  try {
    const queryClient = renderSection();
    const reads = seedDeliveryReads(queryClient);
    const evals = seedEvalsReads(queryClient);
    await user.click(await screen.findByRole("switch", { name: "Delivery" }));
    const invalidate = spyOn(queryClient, "invalidateQueries");

    await fill(user, sourceField(1, "Deploy workflow"), ".github/workflows/deploy.yml ");
    await fill(
      user,
      sourceField(1, "Production jobs"),
      " widgets-release / widgets-release \n\nrelease / release\n"
    );
    await fill(user, sourceField(1, "PR checks workflow"), ".github/workflows/pr-checks.yml");
    await fill(user, field("Population authors"), " octocat\n\noctocat-agent[bot],  hubot \n");
    await fill(user, field("Excluded repositories"), "acme/dojo\r\n acme/widgets-smoke\n");
    await user.click(screen.getByLabelText("A check that fails after its merge"));
    await user.type(field("Failures counted from"), "2026-11-01");
    await fill(user, field("Deploy frequency (deploys a day)"), " 12 ");
    await fill(user, field("Failed deployment recovery time (minutes)"), "off");
    await user.click(screen.getByRole("button", { name: SAVE }));

    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    // Each field trimmed, each list split into its entries, an empty source repository the
    // default's (null), and only the targets that hold text.
    expect(sentBody(put)).toEqual({
      delivery: true,
      excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
      failure_sources: ["check_results"],
      failures_measured_since: "2026-11-01",
      population_authors: ["octocat", "octocat-agent[bot]", "hubot"],
      sources: [
        {
          deploy_workflow_path: ".github/workflows/deploy.yml",
          kind: "github_workflow",
          pr_checks_rollup_job_name: null,
          pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
          production_apply_step_name: null,
          production_job_names: ["widgets-release / widgets-release", "release / release"],
          repo: null,
          staging_apply_job_name: null,
          staging_e2e_job_name: null,
        },
      ],
      targets: { "deploy-frequency": 12, "failed-deploy-recovery": "off" },
    });

    expect(await screen.findByRole("status")).toHaveTextContent("Delivery settings saved");
    // The fields start over from what the server stored: its population and its targets.
    expect(field("Population authors").value).toBe("octocat\noctocat-agent[bot]");
    expect(field("Deploy frequency (deploys a day)").value).toBe("20");
    expect(field("Change failure rate (a share from 0 to 1)").value).toBe("0.05");
    // The save refreshes the project's settings, the delivery reads that are not live (an open
    // run's drill-down; a live view learns of the save from Dispatch, as a delta), the Evals tab's
    // reads of the project that are not live (an eval's detail; its evals, items and timeline
    // views learn of it as a delta), and the cross-project read, which lists the evals whose
    // targets it wrote.
    await waitFor(() =>
      expect(invalidate.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([
        ["project-delivery-settings", PROJECT],
        ["delivery"],
        ["evals", PROJECT],
        ["evals", "across"],
      ])
    );
    expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(reads.live)?.isInvalidated).toBe(false);
    expect(queryClient.getQueryState(evals.detail)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(evals.across)?.isInvalidated).toBe(true);
    for (const view of evals.live) {
      expect(queryClient.getQueryState(view)?.isInvalidated).toBe(false);
    }
    await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("a failed re-read of a loaded record keeps the editor and the draft", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockRejectedValue(new ApiError(500, { code: "INTERNAL", error: "database unavailable" }));

  try {
    const queryClient = renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    await replace(user, field("Population authors"), "hubot");

    // A re-read that fails - a window regaining focus, another save's refresh - leaves the record
    // the section already holds, and the fields drawn from it, as they were.
    await act(() =>
      queryClient.refetchQueries({ queryKey: ["project-delivery-settings", PROJECT] })
    );
    expect(get).toHaveBeenCalledTimes(2);
    expect(queryClient.getQueryState(["project-delivery-settings", PROJECT])?.status).toBe("error");
    expect(screen.queryByText("Couldn't load the delivery settings.")).not.toBeInTheDocument();
    expect(deliverySwitch()).toBeChecked();
    expect(field("Population authors").value).toBe("hubot");
  } finally {
    cleanup();
    get.mockRestore();
  }
});

test("each refusal shows the server's message and keeps the draft", async () => {
  const user = userEvent.setup();
  const refusals: [number, string, string][] = [
    [
      400,
      "DELIVERY_SETTINGS_INPUT",
      "deploy_workflow_path: a github_workflow source names its deploy workflow",
    ],
    [
      409,
      "DELIVERY_REPOSITORY_SHARED",
      "acme/elsewhere is already a deploy source of project CORE, which has delivery on; a repository is the deploy source of one project at most",
    ],
    [
      409,
      "DELIVERY_NO_DEFAULT_REPOSITORY",
      "source 0 of project ACME names no repository and the project has no default repository: name the source's repository, or set the project's default repository in Settings -> Projects",
    ],
    [
      409,
      "DELIVERY_SETTINGS_ACCESS",
      "source 0 reads acme/elsewhere, which Dispatch's GitHub App cannot read: the GitHub App is not installed on acme/elsewhere",
    ],
    [
      403,
      "PROJECT_SCOPE_DEPLOYMENT",
      "this request's credential may write project ACME and nothing else; PUT /api/v1/projects/{key}/settings/delivery belongs to the deployment rather than to a project",
    ],
  ];
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings");
  for (const [status, code, error] of refusals) {
    put.mockRejectedValueOnce(new ApiError(status, { code, error }));
  }

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    await fill(user, sourceField(1, "Repository"), "acme/elsewhere");

    for (const [index, [, code, message]] of refusals.entries()) {
      await user.click(screen.getByRole("button", { name: SAVE }));
      await waitFor(() => expect(put).toHaveBeenCalledTimes(index + 1));
      await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(message), {
        timeout: 2_000,
      });
      expect(sourceField(1, "Repository").value, code).toBe("acme/elsewhere");
      expect(deliverySwitch()).toBeChecked();
    }
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

// A save the reader stopped may have been made: the section says so, and that its button sends
// what the fields hold then, rather than a failure it cannot know of.
test("a save the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockRejectedValue(
    new NetworkError("/api/v1/projects/ACME/settings/delivery", new TypeError("Failed to fetch"))
  );
  const queryClient = renderSection();
  try {
    await screen.findByRole("switch", { name: "Delivery" });
    await user.click(screen.getByRole("button", { name: SAVE }));
    await stopTheLateWrite(queryClient);
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the delivery settings", "saved", {
        anew: "Save delivery settings sends what the form holds now.",
      })
    );
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    get.mockRestore();
    put.mockRestore();
  }
});

test("another save restarts the fields from the new record", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);

  try {
    const queryClient = renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    // The fields start from the stored record, one list entry per line.
    expect(deliverySwitch()).toBeChecked();
    expect(sourceField(1, "Repository").value).toBe("");
    expect(sourceField(1, "Deploy workflow").value).toBe(".github/workflows/deploy.yml");
    expect(sourceField(1, "Production jobs").value).toBe(
      "widgets-release / widgets-release\nrelease / release"
    );
    expect(sourceField(1, "PR checks workflow").value).toBe(".github/workflows/pr-checks.yml");
    expect(field("Population authors").value).toBe("octocat\noctocat-agent[bot]");
    expect(field("Excluded repositories").value).toBe("acme/dojo\nacme/widgets-smoke");
    expect(screen.getByLabelText("A check that fails after its merge")).toBeChecked();
    expect(screen.getByLabelText("A merged revert")).toBeChecked();
    expect(screen.getByLabelText("An issue labelled incident")).not.toBeChecked();
    expect(field("Failures counted from").value).toBe("2026-11-01");
    expect(TARGET_FIELDS.map((label) => field(label).value)).toEqual([
      "20",
      "0.05",
      "45",
      "60",
      "0.9",
      "off",
      "0",
    ]);

    await replace(user, field("Population authors"), "mallory");

    // Someone else saves: the record's read answers their record, with its own write time.
    get.mockResolvedValue(
      savedLater({
        population_authors: ["hubot"],
        sources: [githubSource({ repo: "acme/deploys", resolved_repo: "acme/deploys" })],
      })
    );
    await act(() => queryClient.refetchQueries({ queryKey: ["project-delivery-settings"] }));

    await waitFor(() => expect(field("Population authors").value).toBe("hubot"));
    expect(sourceField(1, "Repository").value).toBe("acme/deploys");
  } finally {
    cleanup();
    get.mockRestore();
  }
});

test("a required field holding only whitespace is empty, so Save is refused before any request", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(null);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);
  const filled: [string, string][] = [
    ["Deploy workflow", ".github/workflows/deploy.yml"],
    ["Production jobs", "widgets-release"],
    ["PR checks workflow", ".github/workflows/pr-checks.yml"],
  ];
  const blank: [string, string][] = [
    ["Deploy workflow", "   "],
    ["Production jobs", " \n\t \n"],
    ["PR checks workflow", " "],
  ];

  try {
    renderSection();
    await user.click(await screen.findByRole("switch", { name: "Delivery" }));
    for (const [label, value] of filled) {
      await fill(user, sourceField(1, label), value);
    }

    for (const [label, whitespace] of blank) {
      await replace(user, sourceField(1, label), whitespace);
      expect(sourceField(1, label).value).toBe("");
      // A save TanStack started would call the API only after its own await, and user-event's
      // click flushes that: it waits through its own act.
      await user.click(screen.getByRole("button", { name: SAVE }));
      expect(put).not.toHaveBeenCalled();
      await replace(
        user,
        sourceField(1, label),
        filled.find(([name]) => name === label)?.[1] ?? ""
      );
    }

    // With every field holding text again the same button saves, so the refusals above were the
    // whitespace's and nothing else's.
    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("Add makes a source that reads the default repository until it names one", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    await user.click(screen.getByRole("button", { name: "Add a GitHub workflow source" }));

    expect((within(source(2)).getByLabelText("Kind") as HTMLSelectElement).value).toBe(
      "github_workflow"
    );
    const repository = sourceField(2, "Repository");
    expect(repository.value).toBe("");
    expect(repository.placeholder).toBe(DEFAULT_REPO);
    for (const label of ["Deploy workflow", "Production jobs", "PR checks workflow"]) {
      expect(sourceField(2, label).value).toBe("");
    }
    // The first source stays as stored.
    expect(sourceField(1, "Deploy workflow").value).toBe(".github/workflows/deploy.yml");

    await fill(user, repository, "acme/deploys");
    await fillRule(user, 2);
    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(sentBody(put)).toMatchObject({
      sources: [
        { kind: "github_workflow", repo: null },
        {
          deploy_workflow_path: ".github/workflows/deploy.yml",
          kind: "github_workflow",
          production_job_names: ["widgets-release / widgets-release"],
          pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
          repo: "acme/deploys",
        },
      ],
    });
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("Remove drops that source alone", async () => {
  const user = userEvent.setup();
  const twoSources: ProjectDeliverySettings = {
    ...record,
    sources: [
      githubSource(),
      githubSource({
        deploy_workflow_path: ".github/workflows/release.yml",
        id: "source-2",
        repo: "acme/deploys",
        resolved_repo: "acme/deploys",
      }),
    ],
  };
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(twoSources);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(twoSources);

  try {
    renderSection();
    await screen.findByRole("group", { name: "Deploy source 2" });

    await user.click(screen.getByRole("button", { name: "Remove deploy source 1" }));
    expect(screen.queryByRole("group", { name: "Deploy source 2" })).not.toBeInTheDocument();
    expect(sourceField(1, "Repository").value).toBe("acme/deploys");
    expect(sourceField(1, "Deploy workflow").value).toBe(".github/workflows/release.yml");

    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    const body = sentBody(put) as { sources: unknown[] };
    expect(body.sources).toHaveLength(1);
    expect(body.sources[0]).toMatchObject({
      deploy_workflow_path: ".github/workflows/release.yml",
      repo: "acme/deploys",
    });
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("a named repository is saved and read back on its source", async () => {
  const user = userEvent.setup();
  const stored = savedLater({
    sources: [githubSource({ repo: "acme/deploys", resolved_repo: "acme/deploys" })],
  });
  const get = spyOn(api, "getProjectDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockResolvedValue(stored);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(stored);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });

    await fill(user, sourceField(1, "Repository"), " acme/deploys ");
    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(sentBody(put)).toMatchObject({
      sources: [{ kind: "github_workflow", repo: "acme/deploys" }],
    });

    expect(await screen.findByRole("status")).toHaveTextContent("Delivery settings saved");
    expect(sourceField(1, "Repository").value).toBe("acme/deploys");
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("an emptied repository saves null and reads the default", async () => {
  const user = userEvent.setup();
  const named: ProjectDeliverySettings = {
    ...record,
    sources: [
      githubSource({
        pr_checks_rollup_job_name: "checks-complete",
        production_apply_step_name: "Run ./.github/actions/apply",
        repo: "acme/deploys",
        resolved_repo: "acme/deploys",
        staging_apply_job_name: "staging-release / apply",
        staging_e2e_job_name: "staging-e2e / run",
      }),
    ],
  };
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(named);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(named);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    // The optional names start from the stored record.
    expect(sourceField(1, "Production apply step").value).toBe("Run ./.github/actions/apply");
    expect(sourceField(1, "Staging end-to-end job").value).toBe("staging-e2e / run");
    expect(sourceField(1, "Staging deploy job").value).toBe("staging-release / apply");
    expect(sourceField(1, "PR checks roll-up job").value).toBe("checks-complete");

    const repository = sourceField(1, "Repository");
    await user.clear(repository);
    expect(repository.placeholder).toBe(DEFAULT_REPO);
    expect(source(1).textContent).toContain(
      "Empty, the source reads the project's default repository, and moves with it."
    );
    // An optional name is sent trimmed, and one emptied saves null as the repository does.
    await replace(user, sourceField(1, "Production apply step"), " Run ./.github/actions/apply ");
    await replace(user, sourceField(1, "Staging deploy job"), " staging-release / go ");
    await replace(user, sourceField(1, "PR checks roll-up job"), "   ");
    await user.click(screen.getByRole("button", { name: SAVE }));

    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(sentBody(put)).toMatchObject({
      sources: [
        {
          kind: "github_workflow",
          pr_checks_rollup_job_name: null,
          production_apply_step_name: "Run ./.github/actions/apply",
          repo: null,
          staging_apply_job_name: "staging-release / go",
          staging_e2e_job_name: "staging-e2e / run",
        },
      ],
    });
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("a source naming no repository needs a default repository", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(null);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);

  try {
    renderSection(null);
    await user.click(await screen.findByRole("switch", { name: "Delivery" }));
    expect((field("Default repository") as HTMLInputElement).value).toBe("");

    // The line under the box names both settings a source can read.
    const repository = sourceField(1, "Repository");
    expect(repository.placeholder).toBe("owner/name");
    expect(source(1).textContent).toContain(
      "This project has no default repository: name this source's repository, or set the project's default repository in Settings → Projects."
    );
    for (const link of screen.getAllByRole("link", { name: "Settings → Projects" })) {
      expect(link).toHaveAttribute("href", "/settings");
    }

    await fillRule(user, 1);
    await user.click(screen.getByRole("button", { name: SAVE }));
    expect(screen.getByRole("alert").textContent).toBe(
      "Deploy source 1 names no repository, and this project has no default repository: name the source's repository, or set the project's default repository in Settings → Projects."
    );
    expect(repository).toHaveAttribute("aria-invalid", "true");
    expect(put).not.toHaveBeenCalled();

    await fill(user, repository, "acme/widgets");
    await user.click(screen.getByRole("button", { name: SAVE }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("a repository that is not owner/name is refused before any request", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    const repository = sourceField(1, "Repository");

    for (const typed of ["acme", "acme/widgets/extra", "acme/ widgets"]) {
      await replace(user, repository, typed);
      await user.click(screen.getByRole("button", { name: SAVE }));
      expect(screen.getByRole("alert").textContent, typed).toBe(
        "Deploy source 1: write its repository as owner/name."
      );
      expect(repository).toHaveAttribute("aria-invalid", "true");
    }
    expect(put).not.toHaveBeenCalled();
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("two sources naming one repository are refused before any request", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    await fill(user, sourceField(1, "Repository"), "acme/deploys");
    await user.click(screen.getByRole("button", { name: "Add a GitHub workflow source" }));
    await fill(user, sourceField(2, "Repository"), "Acme/Deploys");
    await fillRule(user, 2);

    await user.click(screen.getByRole("button", { name: SAVE }));
    expect(screen.getByRole("alert").textContent).toBe(
      "Deploy sources 1 and 2 both read Acme/Deploys: a repository is read by one deploy source at most."
    );
    expect(sourceField(1, "Repository")).toHaveAttribute("aria-invalid", "true");
    expect(sourceField(2, "Repository")).toHaveAttribute("aria-invalid", "true");

    // A source naming none reads the default repository, so it and one naming that repository
    // read one repository too.
    await user.clear(sourceField(1, "Repository"));
    await replace(user, sourceField(2, "Repository"), DEFAULT_REPO);
    await user.click(screen.getByRole("button", { name: SAVE }));
    expect(screen.getByRole("alert").textContent).toBe(
      `Deploy sources 1 and 2 both read ${DEFAULT_REPO}: a repository is read by one deploy source at most.`
    );
    expect(put).not.toHaveBeenCalled();
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});

test("a project holds one events source at most", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const options = (n: number) =>
    [...(within(source(n)).getByLabelText("Kind") as HTMLSelectElement).options].map(
      (option) => option.value
    );

  try {
    renderSection();
    await screen.findByRole("switch", { name: "Delivery" });
    expect(options(1)).toEqual(["github_workflow", "events"]);

    await user.click(screen.getByRole("button", { name: "Add the posted deploys source" }));
    expect((within(source(2)).getByLabelText("Kind") as HTMLSelectElement).value).toBe("events");
    // With one events source, Add offers no second, and no other source can become one.
    expect(
      screen.queryByRole("button", { name: "Add the posted deploys source" })
    ).not.toBeInTheDocument();
    expect(options(1)).toEqual(["github_workflow"]);
    expect(options(2)).toEqual(["github_workflow", "events"]);

    await user.click(screen.getByRole("button", { name: "Remove deploy source 2" }));
    expect(screen.getByRole("button", { name: "Add the posted deploys source" })).toBeDefined();
    expect(options(1)).toEqual(["github_workflow", "events"]);
  } finally {
    cleanup();
    get.mockRestore();
  }
});

/** What Dispatch received of one save: the body, and the login its `Dispatch-Login` named. */
interface ReceivedSave {
  readonly body: Record<string, unknown>;
  readonly login: string | null;
}

/** Saves a project whose delivery is off as `viewer` (`/auth/whoami`'s answer), through the page's
 *  own write path and client, and answers what Dispatch received. */
async function saveAs(viewer: AuthenticatedUser): Promise<ReceivedSave> {
  const user = userEvent.setup();
  const stored: ProjectDeliverySettings = { ...record, delivery: false };
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(stored);
  const received = Promise.withResolvers<ReceivedSave>();
  answerRequests(
    http.put(`/api/v1/projects/${PROJECT}/settings/delivery`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      received.resolve({ body, login: request.headers.get(DISPATCH_LOGIN_HEADER) });
      return Response.json(stored);
    })
  );

  try {
    renderSection(DEFAULT_REPO, viewer);
    await user.click(await screen.findByRole("button", { name: SAVE }));
    const save = await received.promise;
    expect(await screen.findByRole("status")).toHaveTextContent("Delivery settings saved");
    return save;
  } finally {
    cleanup();
    get.mockRestore();
  }
}

test("an agent viewer's save names the session its credential proved as its writer, and no person", async () => {
  // The session whoami answers is the one the save names, whatever the page's address named.
  keepPageSession("?session=ses_from_the_address");
  try {
    const save = await saveAs({
      event_head: "0",
      kind: "agent",
      login: "agent:ses_from_whoami",
      session: "ses_from_whoami",
    });
    expect(save.body.actor).toEqual({ id: "ses_from_whoami", kind: "session" });
    expect(save.body.delivery).toBe(false);
    expect(save.login).toBeNull();
  } finally {
    keepPageSession("");
  }
});

test("an agent viewer whose credential proves no session names the one the page's first address named", async () => {
  keepPageSession("?session=ses_from_the_address");
  try {
    const save = await saveAs({ event_head: "0", kind: "agent", login: "agent", session: null });
    expect(save.body.actor).toEqual({ id: "ses_from_the_address", kind: "session" });
    expect(save.login).toBeNull();
  } finally {
    keepPageSession("");
  }
});

test("a person's save names no writer in its body, and names the person in Dispatch-Login", async () => {
  keepPageSession("?session=ses_from_the_address");
  try {
    const save = await saveAs({ event_head: "0", kind: "user", login: "alice" });
    expect(save.body).not.toHaveProperty("actor");
    expect(save.login).toBe("alice");
  } finally {
    keepPageSession("");
  }
});

test("an agent viewer that names no session is told to open the page with one, and its save is not sent", async () => {
  const user = userEvent.setup();
  const get = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(record);
  const put = spyOn(api, "putProjectDeliverySettings").mockResolvedValue(record);
  // Neither the credential (`session: null`) nor the page's first address names a session.
  keepPageSession("");

  try {
    renderSection(DEFAULT_REPO, {
      event_head: "0",
      kind: "agent",
      login: "agent",
      session: null,
    });
    await user.click(await screen.findByRole("button", { name: SAVE }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      `An agent saves these settings as one of its sessions, and this page names none: open it at /projects/${PROJECT}/settings?session=<your session>.`
    );
    expect(put).not.toHaveBeenCalled();
    // The draft stays, as for every refusal made before a request.
    expect(deliverySwitch()).toBeChecked();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  } finally {
    cleanup();
    get.mockRestore();
    put.mockRestore();
  }
});
