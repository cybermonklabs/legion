import { beforeEach, expect, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import { artifactRecordHandler } from "../../__tests__/proof-document";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type {
  ArtifactReview,
  Ask,
  IssueCoverage,
  IssueDetails,
  Project,
  UserIssueState,
} from "../../api/types";
import { buildDispatchReference, parseIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../shell/keymap";
import { CONTROL_LABELS } from "./page-words";
import { HeaderSlot } from "./slots/header";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";

/**
 * The header's cases of `features/issue/IssuePage.test.tsx` at the new page's checkpoint, each
 * mounting the header's slot alone (`slots/header.tsx`), as the new page's root mounts it: from
 * the issue's own read, with the address parsed.
 */

const issue = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  advances: [],
  advances_revision: 0,
  artifacts: [
    {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      project: "CORE",
      kind: "doc",
      name: "spec.md",
      primary: true,
      ref_key: "CORE-1/spec",
      session_id: null,
      slug: "spec",
      versions: [],
    },
  ],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  design_gates: [],
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  open_asks: [],
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  plan_artifact_id: "",
  decisions_artifact_id: "",
  primary_artifact_id: "artifact-1",
  project: "CORE",
  referenced_by_count: 0,
  route: null,
  repo: null,
  resolved_repo: null,
  status: "todo",
  priority: null,
  rank: "U",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
  // `satisfies`, not an annotation: the tests below read `issue.artifacts[0]` and spread the
  // list, which the route table's `Artifact[] | null` would not let them.
} satisfies IssueDetails;

const openIssueAsk: Ask = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "session-1", kind: "session" },
  block_id: null,
  created_at: "2026-09-10T00:00:00Z",
  edited_at: null,
  id: "ask-open",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Should this ship?",
  state: "open",
  waiting_on: "human",
  // The person the server resolved the question to: the reader these tests sign in as, so the
  // header's count of what waits on the reader counts it (`waiting.ts`).
  waiting_on_person: "alice",
  urgency: "med",
};

/** An open approval request on the issue's spec whose turn is a person's: it waits on the
 *  reader, so the header's approval control carries Approve and Request changes. */
function approvalAsk(): NonNullable<IssueDetails["open_asks"]>[number] {
  return {
    ...openIssueAsk,
    artifact_id: issue.artifacts[0]?.id ?? null,
    id: "ask-approval",
    kind: "approval",
    last_reply: null,
    question: "Approve the spec?",
  };
}

/** The issue the file's stub is answering for, so the document's own record is the one the test
 *  stubbed. */
let stubbedIssue: IssueDetails = issue;

/** Answers the header's reads through `api`: the issue, the reader's state of it, its comments
 *  (which the count reads a Change from) and the projects, whose gates only a root issue reads.
 *  A list without this issue's project gates nothing, which is the one chip these tests assert. */
function stubHeader(
  nextIssue: IssueDetails,
  state: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 }
): () => void {
  const originalGetIssue = api.getIssue;
  const originalGetMyState = api.getMyState;
  const originalListComments = api.listComments;
  const originalListProjects = api.listProjects;
  stubbedIssue = nextIssue;
  api.getIssue = async () => nextIssue;
  api.getMyState = async () => ({ "CORE-1": state });
  api.listComments = async () => [];
  api.listProjects = async () => [];
  return () => {
    api.getIssue = originalGetIssue;
    api.getMyState = originalGetMyState;
    api.listComments = originalListComments;
    api.listProjects = originalListProjects;
    stubbedIssue = issue;
  };
}

/** The header's other reads, each answered with the API's empty shape: the coverage, whose
 *  statements the count's decisions are read against, and the document's record. */
function answerHeaderReads(): void {
  answerRequests(
    http.get("/api/v1/issues/:key/coverage", ({ params }) =>
      Response.json({
        gaps: [],
        has_plan: false,
        issue: String(params.key),
        requirements: [],
        spec_artifact_id: "",
      } satisfies IssueCoverage)
    ),
    // The tree read, whose decision nodes the count reads a Change under (`waiting.ts`): an
    // issue with no node, as the coverage above holds no statement.
    http.get("/api/v1/issues/:key/tree", () =>
      Response.json({
        converted: false,
        cursor: null,
        nodes: [],
        tree: null,
        tuples: [],
        view_of: null,
      })
    ),
    artifactRecordHandler((id) => (stubbedIssue.artifacts ?? []).find((each) => each.id === id))
  );
}

beforeEach(answerHeaderReads);

/** The header's slot as the new page's root mounts it: from the issue's own read, once it has
 *  loaded, with the address parsed. */
function HeaderAlone(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  const read = useQuery(issueQuery(route?.key));
  if (route === undefined || read.data === undefined) return null;
  return <HeaderSlot issue={read.data} route={route} />;
}

/** The client each case renders with: a failed read is not retried, and an answer never goes
 *  stale. */
function headerClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** Pushes the `issue` scope, which the page's view row pushes (`useIssueKeymap.ts`) and the
 *  header's slot does not: the header's keys register into it, and fire only while it is on. */
function IssueScope(): null {
  useKeymapScope("issue");
  return null;
}

/** The header's slot at `path`, under the router, the client and the keys the page gives it. */
function headerTree(path: string, queryClient: QueryClient): ReactNode {
  return (
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssueScope />
          <Routes>
            <Route path="/issues/:key/*" element={<HeaderAlone />} />
          </Routes>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function renderHeader(path = "/issues/CORE-1", queryClient = headerClient()) {
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return render(headerTree(path, queryClient));
}

/** Opens the header's one actions control, which is where the pin, close, reopen and the control
 *  that sets the parent live (row one holds the actions, never a property). */
async function openIssueActions(): Promise<void> {
  const user = userEvent.setup();
  const control = await screen.findByRole("button", { name: "Issue actions" });
  // The control is a toggle and a press inside it closes the group, so this opens it only while
  // it is closed: two calls in a row leave the actions on screen, not hidden again.
  if (control.getAttribute("aria-expanded") !== "true") {
    await user.click(control);
  }
}

test("the header updates whose turn when a human clarification is latest", async () => {
  const waitingIssue = {
    ...issue,
    open_asks: [{ ...openIssueAsk, last_reply: null, waiting_on: "human" as const }],
  };
  const restore = stubHeader(waitingIssue);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    // The marker's words are the page's and the count beside them is the issue's, each marked
    // as what it is, so the marker is read whole.
    expect((await screen.findByTestId("issue-whose-turn")).textContent).toBe("Waiting on you (1)");
    act(() => {
      queryClient.setQueryData<IssueDetails>(["issue", "CORE-1"], {
        ...waitingIssue,
        open_asks: [
          {
            ...openIssueAsk,
            last_reply: {
              author: { id: "alice", kind: "user" },
              created_at: "2026-09-11T01:00:00Z",
            },
            waiting_on: "agent",
          },
        ],
      });
    });
    await waitFor(() =>
      expect(screen.getByTestId("issue-whose-turn").textContent).toBe("Waiting on agents (1)")
    );
  } finally {
    view.unmount();
    restore();
  }
});

/** One node of the tree read's answer, under the issue, with what the case gives it. */
function treeNode(id: string, supertag: string, extra: Partial<TreeAnswerNode>): TreeAnswerNode {
  return {
    attributes: {},
    author: [],
    id,
    made_at: "2026-09-10T00:00:00Z",
    parent: issue.key,
    rank: null,
    state: "live",
    supertag,
    text: `Words of ${id}.`,
    visibility: "project",
    ...extra,
  };
}

// The count of what waits on the reader counts each decision of the review list, which the
// header reads off the tree read's decision nodes: an agent's decision that waits for review,
// tied to a statement the coverage read holds, counts beside the open question whose turn is the
// reader's.
test("the header's count counts a waiting decision node tied to a statement", async () => {
  const statement = buildDispatchReference({ block: { id: "r1" }, key: issue.key, kind: "spec" });
  const spec = { artifact_id: "artifact-1", slug: "spec" };
  answerRequests(
    http.get("/api/v1/issues/:key/coverage", ({ params }) =>
      Response.json({
        gaps: [],
        has_plan: false,
        issue: String(params.key),
        requirements: [
          { block_id: "r1", checks: 1, gaps: [], plan_tasks: 0, ref: statement, text: "R1." },
        ],
        spec_artifact_id: "artifact-1",
      } satisfies IssueCoverage)
    ),
    http.get("/api/v1/issues/:key/tree", () =>
      Response.json({
        converted: false,
        cursor: null,
        nodes: [
          treeNode("r1", "requirement", { document: { ...spec, position: 0 } }),
          treeNode("d1", "decision", {
            asks: [],
            changed_at: "2026-09-10T00:00:00Z",
            changed_version: 1,
            document: { ...spec, position: 1 },
            kept: null,
            kind: "agent",
            links: [
              {
                created_at: "2026-09-10T00:00:00Z",
                created_version: 1,
                from_block: "d1",
                stale: false,
                to: {
                  changed_at: null,
                  changed_by: null,
                  id: "r1",
                  kind: "block",
                  made_at: null,
                  made_by: null,
                  parent: null,
                  parent_version: null,
                  ref: statement,
                },
              },
            ],
            replaced: false,
            text_sha256: "sum-d1",
            unsure: false,
            waiting_review: true,
          }),
        ],
        tree: null,
        tuples: [{ from: "d1", kind: "shaped", to: "r1" }],
        view_of: null,
      } satisfies TreeAnswer)
    )
  );
  const restore = stubHeader({ ...issue, open_asks: [{ ...openIssueAsk, last_reply: null }] });
  const view = renderHeader();

  try {
    await waitFor(() =>
      expect(screen.getByTestId("issue-whose-turn").textContent).toBe("Waiting on you (2)")
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("the header hides whose turn when the server sent open asks without waiting_on", async () => {
  const restore = stubHeader({
    ...issue,
    open_asks: [{ ...openIssueAsk, last_reply: null, waiting_on: undefined }],
  });
  const view = renderHeader("/issues/CORE-1");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(screen.queryByText(/^Waiting on (you|agents)/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

// The count reads the reader's login, the coverage and the tree, whose decision nodes and the
// Changes under them it counts, under the keys the outline reads them by, and no decisions read
// and no comments read; and a root issue's header reads the projects for its gated steps, under
// the key the shell's sidebar reads them by. The header mounts again each time the reader comes
// back to the issue's page, and under the app's `staleTime` a read is stale 30 s after it answered
// (`main.tsx`), each observer judging it by its own options: read with the app's defaults, the
// header mounted again past that would send its count's reads and the projects read again,
// although the feed's events keep them fresh (decision 180).
test("the header mounted again past 30 s sends none of its count's reads, nor the projects read, again", async () => {
  const restore = stubHeader(issue);
  // The issue's own read keeps the app's 30 s, so the header mounted again reads it again: that is
  // what says the mount came past the age the five and the projects read would go stale at.
  const getIssue = spyOn(api, "getIssue");
  // The login answered here, and the coverage and the tree by what the file already answers them
  // with. The decisions and the comments are watched too, and the header reads neither.
  const reads = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      entries: [],
      open_questions: [],
    }),
    spyOn(api, "getIssueCoverage"),
    spyOn(api, "listComments"),
    spyOn(api, "getIssueTree"),
  ];
  // The issue has no parent, so its header reads the projects, answered by `stubHeader`'s list.
  const listProjects = spyOn(api, "listProjects");
  const sent = () => reads.map((read) => read.mock.calls.length);
  // The app's client's defaults (`main.tsx`), which a read that sets none of its own takes.
  const queryClient = new QueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
  let view = renderHeader("/issues/CORE-1", queryClient);

  try {
    // The login is the one the shell already holds, the coverage and the tree are each sent once,
    // and the decisions and the comments are not sent at all.
    await waitFor(() => expect(sent()).toEqual([0, 0, 1, 0, 1]));
    await waitFor(() => expect(listProjects).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    const issueReads = getIssue.mock.calls.length;
    view.unmount();

    // The reader comes back to the issue more than 30 s after the reads answered.
    setSystemTime(Date.now() + 31_000);
    view = render(headerTree("/issues/CORE-1", queryClient));
    await screen.findByRole("heading", { level: 1, name: issue.title });
    await waitFor(() => expect(getIssue.mock.calls.length).toBeGreaterThan(issueReads));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(sent(), "the header mounted again sent a read of its count's again").toEqual([
      0, 0, 1, 0, 1,
    ]);
    expect(listProjects.mock.calls.length, "the header mounted again read the projects again").toBe(
      1
    );
  } finally {
    view.unmount();
    setSystemTime();
    for (const read of reads) read.mockRestore();
    listProjects.mockRestore();
    getIssue.mockRestore();
    restore();
  }
});

test("the header keeps state actions available while approval awaits", async () => {
  // The request waits on the reader, so its chip carries Approve in the header.
  const restore = stubHeader({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
    open_asks: [approvalAsk()],
  });
  const view = renderHeader("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    // The pin is one of the overflow menu's actions.
    await openIssueActions();
    const pin = screen.getByRole("button", { name: "Pin issue" });
    expect(pin).toHaveAttribute("aria-pressed", "false");

    const stateActions = screen.getByTestId("issue-state-actions");
    expect(within(stateActions).getByRole("combobox", { name: "Status" })).not.toBeNull();
    expect(
      within(stateActions).getByRole("combobox", { name: "Priority of CORE-1" })
    ).not.toBeNull();
    expect(within(stateActions).getByRole("button", { name: "Awaiting approval" })).not.toBeNull();
    expect(within(stateActions).getByRole("button", { name: "Approve" })).not.toBeNull();
    // Close is an action, not a property, so it sits behind row one's one actions control.
    await openIssueActions();
    expect(screen.getByRole("button", { name: "Close issue" })).not.toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("the header draws a gated root issue's steps where the header's one chip sat", async () => {
  const restore = stubHeader({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
    open_asks: [approvalAsk()],
  });
  const gated: Project = {
    created_at: "2026-09-09T00:00:00Z",
    gated_steps: ["criteria", "spec"],
    open_asks: 0,
    key: "CORE",
    name: "Core",
  };
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([gated]);
  const view = renderHeader("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const stateActions = screen.getByTestId("issue-state-actions");
    const steps = await within(stateActions).findByTestId("issue-approval-steps");
    // One line per gated step, in the order the deployment published them.
    expect(
      Array.from(steps.querySelectorAll("[data-approval-step]")).map((step) =>
        step.getAttribute("data-approval-step")
      )
    ).toEqual(["criteria", "spec"]);
    // The primary document keeps today's chip and its review actions, inside its own step.
    expect(within(steps).getByRole("button", { name: "Awaiting approval" })).not.toBeNull();
    expect(within(steps).getByRole("button", { name: "Approve" })).not.toBeNull();
    // The step nobody has written says so, rather than borrowing the spec's state.
    expect(within(steps).getAllByText("Not requested").length).toBe(1);
    // The steps take the chip's place when one of them holds the primary document: the row
    // draws one approval area, not both, so it carries one pair of controls.
    expect(
      within(stateActions).queryAllByRole("button", { name: "Awaiting approval" }).length
    ).toBe(1);
    expect(within(stateActions).queryAllByRole("button", { name: "Approve" }).length).toBe(1);
  } finally {
    view.unmount();
    listProjects.mockRestore();
    restore();
  }
});

test("the header keeps the primary document's chip when no gated step reviews it", async () => {
  const restore = stubHeader({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
    open_asks: [approvalAsk()],
  });
  // A deployment may gate the Verification document and not the spec; the spec still has an
  // approval to answer, and only the header's own chip can answer it.
  const gated: Project = {
    created_at: "2026-09-09T00:00:00Z",
    gated_steps: ["criteria"],
    open_asks: 0,
    key: "CORE",
    name: "Core",
  };
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([gated]);
  const view = renderHeader("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const stateActions = screen.getByTestId("issue-state-actions");
    const steps = await within(stateActions).findByTestId("issue-approval-steps");
    expect(
      Array.from(steps.querySelectorAll("[data-approval-step]")).map((step) =>
        step.getAttribute("data-approval-step")
      )
    ).toEqual(["criteria"]);
    // The step does not hold the primary document, so it carries neither its state nor its actions.
    expect(within(steps).getAllByText("Not requested").length).toBe(1);
    expect(within(steps).queryByRole("button", { name: "Awaiting approval" })).toBeNull();
    // The primary document's own chip stays in the state row, with both of its controls.
    expect(within(stateActions).getByRole("button", { name: "Awaiting approval" })).not.toBeNull();
    expect(within(stateActions).getByRole("button", { name: "Approve" })).not.toBeNull();
    expect(within(stateActions).getByRole("button", { name: "Request changes" })).not.toBeNull();
  } finally {
    view.unmount();
    listProjects.mockRestore();
    restore();
  }
});

test("the header marks the issue pin when it is pinned", async () => {
  const restore = stubHeader(issue, {
    dismissed: [],
    last_read_seq: 0,
    pinned: true,
    seq: 0,
  });
  const view = renderHeader("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    await openIssueActions();
    const pin = await screen.findByRole("button", { name: "Unpin issue" });
    expect(pin).toHaveAttribute("aria-pressed", "true");
  } finally {
    view.unmount();
    restore();
  }
});

test("the header's pin refreshes the sidebar's pinned list once the pin is saved", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const saved = { dismissed: [], last_read_seq: 0, pinned: true, seq: 1 };
  const putIssueState = spyOn(api, "putIssueState").mockResolvedValue(saved);
  const queryClient = headerClient();
  // The sidebar's read of the pinned issues, as it is cached beside the page.
  queryClient.setQueryData(["issues", "pinned"], []);
  const view = renderHeader("/issues/CORE-1/conversation", queryClient);

  try {
    await within(view.container).findByRole("heading", { level: 1, name: issue.title });
    expect(queryClient.getQueryState(["issues", "pinned"])?.isInvalidated).toBe(false);
    await openIssueActions();
    await user.click(within(view.container).getByRole("button", { name: "Pin issue" }));
    await waitFor(() =>
      expect(queryClient.getQueryState(["issues", "pinned"])?.isInvalidated).toBe(true)
    );
    // The pin is a keyed write of the page's store, so its bounds name the key it goes under.
    expect(putIssueState).toHaveBeenCalledWith(
      "CORE-1",
      { pinned: true },
      expect.objectContaining({ key: expect.any(String) })
    );
  } finally {
    view.unmount();
    putIssueState.mockRestore();
    restore();
  }
});

test("the header keeps an unrequested draft approval passive", async () => {
  const restore = stubHeader({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "draft" },
      },
    ],
  });
  const view = renderHeader("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    // The shared chip draws nothing for a draft, so these words are the header's own: its
    // approval control says nobody has asked for one.
    expect(
      within(screen.getByTestId("issue-state-actions")).getByText(CONTROL_LABELS.not_requested)
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Draft" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Approve$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request changes" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("the header closes an issue and reopens it into Backlog", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const closeSave = Promise.withResolvers<IssueDetails>();
  const reopenSave = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) => {
    if (input.status === "done") return closeSave.promise;
    if (input.status === "backlog") return reopenSave.promise;
    throw new Error(`Unexpected issue update: ${JSON.stringify(input)}`);
  });
  const view = renderHeader();

  try {
    const openStatus = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    expect([...openStatus.options].map((option) => option.value)).not.toContain("done");

    await openIssueActions();
    await user.click(screen.getByRole("button", { name: "Close issue" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "done" }, keyedAttempt)
    );
    // While the write is in flight the control is disabled and says so: no second close can be
    // queued, and the words it shows are its name.
    await openIssueActions();
    expect((screen.getByRole("button", { name: "Closing…" }) as HTMLButtonElement).disabled).toBe(
      true
    );
    await act(async () => {
      closeSave.resolve({ ...issue, closed_at: "2026-09-12T10:00:00Z", status: "done" });
      await closeSave.promise;
    });

    expect(await screen.findByText("This issue is closed.")).toBeTruthy();
    const closedStatus = screen.getByLabelText("Status") as HTMLSelectElement;
    expect(closedStatus.disabled).toBe(true);
    expect(closedStatus.value).toBe("done");

    await openIssueActions();
    // The control is named by the words it shows, so a voice user who says what it reads
    // presses it (WCAG 2.5.3): while the write is out that is "Reopening…".
    await user.click(screen.getByRole("button", { name: "Reopen into Backlog" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );
    await openIssueActions();
    expect((screen.getByRole("button", { name: "Reopening…" }) as HTMLButtonElement).disabled).toBe(
      true
    );
    await act(async () => {
      reopenSave.resolve({ ...issue, status: "backlog" });
      await reopenSave.promise;
    });

    await waitFor(() =>
      expect(screen.queryByText("This issue is closed.")).not.toBeInTheDocument()
    );
    expect(((await screen.findByLabelText("Status")) as HTMLSelectElement).value).toBe("backlog");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

test("the header lets a human move a Todo issue to In progress", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    status: "in_progress",
  });
  const view = renderHeader("/issues/CORE-1/conversation");

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    expect(status.value).toBe("todo");
    expect(
      [...status.options].map((option) => ({ text: option.text, value: option.value }))
    ).toEqual([
      { text: "Triage", value: "triage" },
      { text: "Icebox", value: "icebox" },
      { text: "Backlog", value: "backlog" },
      { text: "Todo", value: "todo" },
      { text: "In progress", value: "in_progress" },
      { text: "Testing", value: "testing" },
      { text: "Needs review", value: "needs_review" },
      { text: "Retro", value: "retro" },
    ]);

    await user.selectOptions(status, "in_progress");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "in_progress" }, keyedAttempt)
    );
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// `a` and Shift+R are the approval's only with the focus on the chip of a document whose request
// waits on the reader, or on one of its two actions, and only while the action each presses takes
// a press. A stale approval's Approve says its version; while the review is written Approve and
// Request changes are `aria-disabled` and refuse a press, so neither key applies then.
test("the approval chip's keys press its own actions, and only while those take a press", async () => {
  const user = userEvent.setup();
  const restore = stubHeader({
    ...issue,
    artifacts: [
      { ...issue.artifacts[0], approval: { latest_version: 2, state: "stale", version: 1 } },
    ],
    open_asks: [approvalAsk()],
  });
  const answered = Promise.withResolvers<ArtifactReview>();
  const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
    () => answered.promise
  );
  const view = renderHeader();
  // Whether each of the two keys applies now, as the key card reads it (`appKeymap.describe`).
  const bothApply = () =>
    ["approve", "request-changes"].map(
      (id) => appKeymap.describe().find((key) => key.scope === "issue" && key.id === id)?.enabled
    );

  try {
    const heading = await screen.findByRole("heading", { level: 1, name: issue.title });
    const stateActions = screen.getByTestId("issue-state-actions");
    const approve = await within(stateActions).findByRole("button", { name: "Approve v2" });
    const requestChanges = within(stateActions).getByRole("button", { name: "Request changes" });
    const chip = stateActions.querySelector<HTMLElement>("[data-approval-artifact]");
    if (chip === null) throw new Error("the header draws no approval chip");

    // Off the chip neither key applies, and `a` sends no review.
    act(() => heading.focus());
    expect(bothApply()).toEqual([false, false]);
    await user.keyboard("a");
    expect(createArtifactReview).not.toHaveBeenCalled();

    // On the chip and on each of its actions both apply, and Shift+R opens Request changes.
    for (const focused of [chip, approve, requestChanges]) {
      act(() => focused.focus());
      expect(bothApply()).toEqual([true, true]);
    }
    act(() => chip.focus());
    await user.keyboard("{Shift>}R{/Shift}");
    expect(await screen.findByRole("dialog", { name: "Request changes" })).toBeInTheDocument();
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument()
    );
    expect(document.activeElement).toBe(chip);

    // `a` presses Approve, whose write is then out: neither key applies until it is answered.
    await user.keyboard("a");
    await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
    expect(createArtifactReview).toHaveBeenCalledWith(
      "artifact-1",
      { state: "approved" },
      keyedAttempt
    );
    await waitFor(() => expect(approve).toHaveAttribute("aria-disabled", "true"));
    expect(requestChanges).toHaveAttribute("aria-disabled", "true");
    expect(bothApply()).toEqual([false, false]);
    await user.keyboard("a");
    await user.keyboard("{Shift>}R{/Shift}");
    expect(createArtifactReview).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument();
  } finally {
    answered.reject(new Error("the connection dropped"));
    view.unmount();
    createArtifactReview.mockRestore();
    restore();
  }
});

// The header's focus keeper gives a control of the Request changes dialog, lost as the dialog
// shuts, to the document's chip, as it gives one of the chip's two actions (`chipOfLostAction`).
// The dialog gives the focus back to the header's Request changes as it shuts; once the request is
// answered the header draws neither action, so the focus falls from the dialog's own control: its
// submit, which reads "Submitting…" while the change request is written, its text box, which
// holds the reader's reason, or its Cancel. No action draws the words of any of them, so only
// their place inside the dialog sends the focus to the chip rather than to "Issue actions". Where
// the reader sent a change request, the chip also hands the focus to itself once the read after
// it removes Request changes (`ApprovalChip`), so the first two rows below hold where the focus
// ends, whichever of the two moves it; the rows after them, where nothing was sent, are the
// keeper's alone.

/** The reason each case types into the dialog: no action of the chip draws these words. */
const CHANGE_REASON = "Name the rollback path.";

/** The issue with its document's approval request waiting on the reader: the header draws Approve
 *  and Request changes beside the document's chip. */
function awaitingApprovalIssue(): IssueDetails {
  return {
    ...issue,
    artifacts: [{ ...issue.artifacts[0], approval: { latest_version: 1, state: "awaiting" } }],
    open_asks: [approvalAsk()],
  };
}

/** The same issue once its document's request is answered with `state`: no request is open, so
 *  the header draws the document's chip and neither action. */
function answeredApprovalIssue(state: "approved" | "changes_requested"): IssueDetails {
  return {
    ...issue,
    artifacts: [{ ...issue.artifacts[0], approval: { latest_version: 1, state, version: 1 } }],
    open_asks: [],
  };
}

/** The reader's change request on the document, as the server records it. */
const changeRequest: ArtifactReview = {
  actor: { id: "alice", kind: "user" },
  artifact_id: "artifact-1",
  ask_id: "ask-approval",
  content_sha256: null,
  content_sha256_source: null,
  created_at: "2026-09-11T00:00:00Z",
  id: "review-1",
  kind: "person",
  reason: CHANGE_REASON,
  state: "changes_requested",
  version: 1,
};

/** Opens the Request changes dialog by Enter on the header's Request changes, as a keyboard reader
 *  does, and types `CHANGE_REASON` into its text box, which the dialog focuses as it opens. Returns
 *  the dialog and the header's line of state controls, which the document's chip stands on. */
async function openRequestChanges(
  user: UserEvent
): Promise<{ readonly dialog: HTMLElement; readonly stateActions: HTMLElement }> {
  const stateActions = await screen.findByTestId("issue-state-actions");
  const requestChanges = await within(stateActions).findByRole("button", {
    name: "Request changes",
  });
  act(() => requestChanges.focus());
  await user.keyboard("{Enter}");
  const dialog = await screen.findByRole("dialog", { name: "Request changes" });
  const reason = within(dialog).getByRole("textbox", { name: "Reason" });
  await waitFor(() => expect(reason).toHaveFocus());
  await user.keyboard(CHANGE_REASON);
  return { dialog, stateActions };
}

/** The chip's two actions the header draws. The dialog stands inside the same line, so its own
 *  Request changes is left out. */
function headerReviewActions(stateActions: HTMLElement): HTMLElement[] {
  return within(stateActions)
    .queryAllByRole("button", { name: /^(Approve|Request changes)$/ })
    .filter((button) => button.closest('[role="dialog"]') === null);
}

/** The document's approval chip on the header's line of state controls. */
function documentChip(stateActions: HTMLElement): HTMLElement {
  const chip = stateActions.querySelector<HTMLElement>('[data-approval-artifact="artifact-1"]');
  if (chip === null) throw new Error("the header draws no approval chip for the document");
  return chip;
}

/** Answers the issue's reads with the waiting request until the reader's change request lands,
 *  and with the answered request from then on, as the server does. Returns the spy of the review's
 *  write and what puts both back. */
function answerChangeRequest(): {
  readonly createArtifactReview: Mock<typeof api.createArtifactReview>;
  readonly restore: () => void;
} {
  let read = awaitingApprovalIssue();
  const restoreHeader = stubHeader(read);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async () => read);
  const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(async () => {
    read = answeredApprovalIssue("changes_requested");
    stubbedIssue = read;
    return changeRequest;
  });
  return {
    createArtifactReview,
    restore: () => {
      createArtifactReview.mockRestore();
      getIssue.mockRestore();
      restoreHeader();
    },
  };
}

test("a change request sent by Enter on the dialog's submit leaves the focus on the document's chip", async () => {
  const user = userEvent.setup();
  const { createArtifactReview, restore } = answerChangeRequest();
  const view = renderHeader();

  try {
    const { dialog, stateActions } = await openRequestChanges(user);
    expect(headerReviewActions(stateActions).length).toBe(2);
    const submit = within(dialog).getByRole("button", { name: "Request changes" });
    act(() => submit.focus());
    await user.keyboard("{Enter}");

    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    expect(createArtifactReview).toHaveBeenCalledTimes(1);
    expect(createArtifactReview).toHaveBeenCalledWith(
      "artifact-1",
      { reason: CHANGE_REASON, state: "changes_requested" },
      keyedAttempt
    );
    expect(headerReviewActions(stateActions)).toEqual([]);
    await waitFor(() => expect(documentChip(stateActions)).toHaveFocus());
  } finally {
    view.unmount();
    restore();
  }
});

test("a change request sent by Ctrl+Enter in the dialog's text box leaves the focus on the document's chip", async () => {
  const user = userEvent.setup();
  const { createArtifactReview, restore } = answerChangeRequest();
  const view = renderHeader();

  try {
    const { dialog, stateActions } = await openRequestChanges(user);
    expect(headerReviewActions(stateActions).length).toBe(2);
    const reason = within(dialog).getByRole("textbox", { name: "Reason" });
    expect(reason).toHaveFocus();
    await user.keyboard("{Control>}{Enter}{/Control}");

    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    expect(createArtifactReview).toHaveBeenCalledTimes(1);
    expect(createArtifactReview).toHaveBeenCalledWith(
      "artifact-1",
      { reason: CHANGE_REASON, state: "changes_requested" },
      keyedAttempt
    );
    expect(headerReviewActions(stateActions)).toEqual([]);
    await waitFor(() => expect(documentChip(stateActions)).toHaveFocus());
  } finally {
    view.unmount();
    restore();
  }
});

// Another reviewer answers the request while the reader's dialog is open, so the issue's read
// draws no request: the header's two actions go and the dialog stays. The reader then shuts it, by
// its Cancel, pressed by Enter, or by Escape in its text box. Nothing was sent, so the chip has no
// hand-over of its own to make, and the header's keeper alone puts the focus anywhere.
test.each([
  "Cancel",
  "Escape",
])("the Request changes dialog shut by %s after its request was answered elsewhere leaves the focus on the document's chip", async (shutBy) => {
  const user = userEvent.setup();
  const restore = stubHeader(awaitingApprovalIssue());
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { dialog, stateActions } = await openRequestChanges(user);
    expect(headerReviewActions(stateActions).length).toBe(2);
    act(() => {
      queryClient.setQueryData<IssueDetails>(
        ["issue", "CORE-1"],
        answeredApprovalIssue("approved")
      );
    });
    await waitFor(() => expect(headerReviewActions(stateActions)).toEqual([]));
    expect(dialog).toBeInTheDocument();

    if (shutBy === "Cancel") {
      const cancel = within(dialog).getByRole("button", { name: "Cancel" });
      act(() => cancel.focus());
      await user.keyboard("{Enter}");
    } else {
      expect(within(dialog).getByRole("textbox", { name: "Reason" })).toHaveFocus();
      await user.keyboard("{Escape}");
    }

    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    await waitFor(() => expect(documentChip(stateActions)).toHaveFocus());
  } finally {
    view.unmount();
    restore();
  }
});
