import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { ID_AMBIGUOUS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { Link, MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { fakeDocumentRuntime, markedText } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import {
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../__tests__/proof-document";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { AGENT_LIST_REFILLING, ApiError, api } from "../../api/client";
import type {
  Agent,
  ArtifactBlock,
  ArtifactText,
  ArtifactVersionText,
  Ask,
  GraphReferences,
  InboxRow,
  IssueAskThreadRow,
  IssueCheckResults,
  IssueCoverage,
  IssueDetails,
  IssueReferences,
  Message,
  Project,
  ReferenceTargets,
  Subscriber,
  UserIssueState,
} from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { FilledMarginSlot, MarginSlotProvider, useMarginSlotFill } from "../margin/margin-slot";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap } from "../shell/keymap";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { IssuePage } from "./IssuePage";
import { HELP_LINES } from "./page-structure";
import { EMPTY_STATE_LINES } from "./page-words";

/** Where this browser keeps the conversation state the file's signed-in reader last picked. */
const KEPT_STATE = userPreferenceStorageKey("alice", "issue.conversation-state");

const issue = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
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

const itemComment = {
  // Anchored: the landing has to wait on the issue to learn which document the anchor names,
  // which is the case that turned a failed issue read into a permanent "Loading issue…".
  anchor: {
    artifact_id: "artifact-1",
    block_id: null,
    mark_id: "m-1",
    orphaned: false,
    quote: "spec",
    version: 1,
  },
  ask_id: null,
  author: { id: "alice", kind: "user" as const },
  body: "Anchored note.",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "comment-1",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
  ...commentDeliveryFields(),
};

function subscriber(sessionId: string, title: string, live = true): Subscriber {
  return {
    last_seen: 1,
    live,
    removable: true,
    session_id: sessionId,
    title,
    topics: [],
  };
}

function listedAgent(sessionId: string, title: string): Agent {
  return {
    capabilities: [],
    dir: "/workspaces/legion",
    host: "example-host-devbox",
    last_activity: null,
    last_seen: 1_700_000_000_000,
    machine_id: "example-host-devbox",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    state: "live",
    title,
  };
}

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
  urgency: "med",
};

function issueWithExternalLink(url: string): IssueDetails {
  return { ...issue, external_links: [{ url }] };
}

/** The issue the file's stub is answering for, so the handlers below hand a panel that re-reads
 *  one of its documents the same document the test stubbed. */
let stubbedIssue: IssueDetails = issue;

function stubIssuePage(
  nextIssue: IssueDetails,
  inbox: InboxRow[] = [],
  subscribers: Subscriber[] = [],
  state: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  agents: Agent[] = []
): () => void {
  const originalGetIssue = api.getIssue;
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListIssueAsks = api.listIssueAsks;
  const originalListIssueAskThreads = api.listIssueAskThreads;
  const originalListComments = api.listComments;
  const originalListIssueMessages = api.listIssueMessages;
  const originalListHandoffs = api.listHandoffs;
  const originalGetInbox = api.getInbox;
  const originalGetMyState = api.getMyState;
  const originalListAgents = api.listAgents;
  const originalGetIssueSubscribers = api.getIssueSubscribers;
  const originalListProjects = api.listProjects;
  stubbedIssue = nextIssue;
  api.getIssue = async () => nextIssue;
  api.getInbox = async () => inbox;
  api.getMyState = async () => ({ "CORE-1": state });
  api.getIssueEvents = async () => [];
  // The Conversation builds its list from the issue's own asks, comments, messages and handoffs;
  // it asks the ask list for each thread, where the margin reads the same route without one.
  api.listIssueAsks = async () => [];
  api.listIssueAskThreads = async () => [];
  api.listComments = async () => [];
  api.listIssueMessages = async () => [];
  api.listHandoffs = async () => [];
  api.listAgents = async () => agents;
  api.getIssueSubscribers = async () => subscribers;
  // The header reads the shared project list to learn which design-gate steps the project
  // gates; a list without this issue's project gates nothing, which is the one chip these
  // tests assert.
  api.listProjects = async () => [];
  return () => {
    api.getIssue = originalGetIssue;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
    api.getIssueEvents = originalGetIssueEvents;
    api.listIssueAsks = originalListIssueAsks;
    api.listIssueAskThreads = originalListIssueAskThreads;
    api.listComments = originalListComments;
    api.listIssueMessages = originalListIssueMessages;
    api.listHandoffs = originalListHandoffs;
    api.listAgents = originalListAgents;
    api.getIssueSubscribers = originalGetIssueSubscribers;
    api.listProjects = originalListProjects;
    stubbedIssue = issue;
  };
}

/**
 * The reads the issue page's own panels make: the header's repository and component lines, the
 * Spec tab's requirements, the Artifacts tab's lists, the issue's references, and each
 * document's own record, text, blocks and versions, with the decisions and read marks
 * `decisionsAndReadMarkHandlers` answers. Each is answered with the API's empty shape, so a
 * panel renders its loaded-empty state rather than its error state. A test that asserts one of
 * these spies the `api` method that fetches it (`spyOn(api, "getArtifactText")`) and never
 * reaches these handlers.
 */
function answerIssuePageReads(): void {
  answerRequests(
    http.get("/api/v1/settings/repo-projects", () => Response.json([])),
    http.get("/api/v1/projects/:project/architecture-sources", () => Response.json([])),
    http.get("/api/v1/issues/:key/coverage", ({ params }) =>
      Response.json({
        gaps: [],
        has_plan: false,
        issue: String(params.key),
        requirements: [],
        spec_artifact_id: "",
      } satisfies IssueCoverage)
    ),
    http.get("/api/v1/issues/:key/artifacts", () => Response.json([])),
    // The issue's own conversation, which the tree and the panel's list share. A row that
    // asserts one of them stubs `api` itself; this answers every other render, including one
    // that moves to a second issue.
    http.get("/api/v1/issues/:key/asks", () => Response.json([])),
    http.get("/api/v1/issues/:key/comments", () => Response.json([])),
    http.get("/api/v1/issues/:key/messages", () => Response.json([])),
    // The tree's own reads, which every address draws: its check results, and the one batched
    // read of every `dispatch://` address its nodes hold.
    http.get("/api/v1/issues/:key/results", ({ params }) =>
      Response.json({
        checks: [],
        issue: String(params.key),
        verification_artifact_id: "",
      } satisfies IssueCheckResults)
    ),
    http.get("/api/v1/reference-targets", () =>
      Response.json({ targets: [] } satisfies ReferenceTargets)
    ),
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({ members: [], truncated: false } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/artifacts/:id/text", () =>
      Response.json({ markdown: "", token: "sha256:empty", version: null } satisfies ArtifactText)
    ),
    http.get("/api/v1/artifacts/:id/versions/:version", ({ params }) =>
      Response.json({
        authors: [],
        created_at: "2026-09-09T00:00:00Z",
        markdown: "",
        named: false,
        number: Number(params.version),
        summary: null,
      } satisfies ArtifactVersionText)
    ),
    // The document the page re-reads is the one the stubbed issue carries.
    artifactRecordHandler((id) => (stubbedIssue.artifacts ?? []).find((each) => each.id === id)),
    ...decisionsAndReadMarkHandlers()
  );
}

beforeEach(answerIssuePageReads);

function CurrentRoute() {
  const location = useLocation();
  return (
    <output data-testid="current-route">{`${location.pathname}${location.search}${location.hash}`}</output>
  );
}

/** A stand-in for the shell's margin, which draws whatever the page put in the slot the way
 *  `MarginSheet` does (`features/margin/margin-slot.tsx`). A row mounts one to watch the panel
 *  move between the margin's column and the page's own. */
function StandInMargin(): ReactNode {
  const fill = useMarginSlotFill();
  return (
    <aside data-testid="stand-in-margin">
      <FilledMarginSlot fill={fill}>{null}</FilledMarginSlot>
    </aside>
  );
}

function renderIssuePage(
  path = "/issues/CORE-1",
  navigateTo?: string,
  seedText?: string,
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  }),
  /** Whether the shell's margin is keeping a column for the page's panel. No margin is
   *  mounted here by default, so the default is the state a collapsed margin and a phone both
   *  leave the page in: the page draws its panel itself (`features/margin/margin-slot.tsx`). */
  marginDrawsPanel = false,
  /** Whether a stand-in for the shell's margin is mounted at all. Off by default: with none
   *  mounted, a page that hands its panel to the margin draws it nowhere, which is what the
   *  rows below read. */
  margin = false
) {
  const runtime = fakeDocumentRuntime({ text: seedText });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const tree = (drawsSlot: boolean) => (
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={runtime.runtime}>
            <MarginProvider>
              <MarginSlotProvider drawsSlot={drawsSlot}>
                <CurrentRoute />
                {navigateTo === undefined ? null : (
                  <Link to={navigateTo}>Navigate to test route</Link>
                )}
                <Routes>
                  <Route path="/issues/:key/*" element={<IssuePage />} />
                </Routes>
                {margin ? <StandInMargin /> : null}
              </MarginSlotProvider>
            </MarginProvider>
          </DocumentRuntime.Provider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(marginDrawsPanel));
  window.setTimeout(runtime.sync);
  return {
    ...view,
    runtime,
    /** Shows the shell's margin, or collapses it again: the shell answers whether the margin
     *  keeps a column for the page's panel, and the reader's own control moves that answer. */
    showMargin: (drawsSlot: boolean) => view.rerender(tree(drawsSlot)),
  };
}

/** happy-dom answers every media query against its own 1024 px window, which is a width the
 *  shell's margin is a sheet at. A test that wants the desktop layout asks for a wider one, as
 *  `IssueTree.test.tsx` does. */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: width <= Number(/max-width:\s*(\d+)/.exec(query)?.[1] ?? 0),
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
}

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
});

async function openSubscribedAgents(count: number) {
  const user = userEvent.setup();
  const toggle = await screen.findByRole("button", { name: `Subscribers: ${count}` });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  await user.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  return screen.findByRole("region", { name: "Subscribed agents" });
}

/** Who opened the issue, where the redesigned page draws it: the panel's People section, with
 *  the label beside the name as text rather than the tooltip the metadata rail carried
 *  (dispatch://INFRA-1196 R11). */
async function expectCreator(label: string): Promise<HTMLElement> {
  const panel = await screen.findByTestId("issue-margin");
  const openedBy = await within(panel).findByText("Opened by:");
  const row = openedBy.parentElement as HTMLElement;
  expect(row.textContent).toBe(`Opened by:${label}`);
  const creator = row.querySelector<HTMLElement>("[data-author-text]") as HTMLElement;
  expect(creator.textContent).toBe(label);
  return creator;
}

/**
 * The preset the page is on. happy-dom's window is 1024 px wide, which is the width the row of
 * five presets becomes one control named by the active preset (dispatch://INFRA-1196 R3), so
 * that one control is what these tests read and press. The control draws the preset and its
 * count, and so is named by both - a word drawn inside a control belongs in that control's
 * name (WCAG 2.5.3, `IssuePresets.tsx`) - so the preset is its name with the count taken out.
 */
async function activePreset(): Promise<string> {
  const row = await screen.findByTestId("issue-presets");
  const named = within(row).getByRole("button").cloneNode(true) as HTMLElement;
  for (const count of named.querySelectorAll("[data-preset-count]")) count.remove();
  return (named.textContent ?? "").replace(/\s+/g, " ").trim();
}

async function expectPreset(name: string): Promise<void> {
  await waitFor(async () => {
    expect(await activePreset()).toBe(name);
  });
}

/** Moves to `name` through that one control's menu. */
async function openPreset(name: string): Promise<void> {
  const user = userEvent.setup();
  const row = await screen.findByTestId("issue-presets");
  await user.click(within(row).getByRole("button"));
  await user.click(await screen.findByRole("menuitem", { name }));
  await expectPreset(name);
}

/** Opens the identity block's one actions control, which is where close, reopen and the control
 *  that sets the parent live now (R11: row one holds the actions, never a property). */
async function openIssueActions(): Promise<void> {
  const user = userEvent.setup();
  const control = await screen.findByRole("button", { name: "Issue actions" });
  // The control is a toggle and a press inside it closes the group, so this opens it only while
  // it is closed: two calls in a row leave the actions on screen, not hidden again.
  if (control.getAttribute("aria-expanded") !== "true") {
    await user.click(control);
  }
}

test("IssuePage shows a session creator's live title and personal-token owner", async () => {
  const restore = stubIssuePage(
    {
      ...issue,
      created_by: {
        id: "creator-session",
        kind: "session",
        origin: { session_title: "Stamped title" },
        owner: "sami",
      },
    },
    [],
    [],
    undefined,
    [
      {
        capabilities: [],
        dir: "/workspaces/chief-of-staff",
        host: "example-host-devbox",
        last_activity: null,
        last_seen: 1_700_000_000_000,
        machine_id: "example-host-devbox",
        open_asks: 0,
        roles: [],
        session_id: "creator-session",
        state: "live",
        title: "chief of staff",
      },
    ]
  );
  const view = renderIssuePage();

  try {
    const creator = await expectCreator("chief of staff (for sami)");
    expect(within(creator).getByRole("link", { name: "chief of staff" })).toHaveAttribute(
      "href",
      "/agents/creator-session/live"
    );
    expect(creator.querySelector("[data-agent-name]")?.getAttribute("data-agent-state")).toBe(
      "running"
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage shows a human creator by login", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage();

  try {
    const creator = await expectCreator("alice");
    expect(within(creator).queryByRole("link")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage omits creator metadata for historical issues", async () => {
  const restore = stubIssuePage({ ...issue, created_by: null });
  const view = renderIssuePage();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(screen.queryByText(/^Opened by/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage's header warns when nobody holds the route, and adds nothing when it is live", async () => {
  // The warning sits in row four of the identity block, beside the route it speaks for: both
  // wrap, so neither is ever reached by scrolling a row sideways (dispatch://INFRA-1196 R11).
  const stateRow = async () => {
    const row = await screen.findByTestId("issue-work-row");
    await within(row).findByRole("button", { name: "Messages default to role:sre" });
    return row;
  };

  const unheld = stubIssuePage({ ...issue, route: "role:sre", route_status: "no_holder" });
  const unheldView = renderIssuePage();
  try {
    const marker = within(await stateRow()).getByTestId("issue-route-unreachable");
    expect(marker).toHaveTextContent("role:sre:");
    expect(marker).toHaveTextContent("Nobody holds it right now");
  } finally {
    unheldView.unmount();
    unheld();
  }

  const live = stubIssuePage({
    ...issue,
    route: "role:sre",
    route_status: "live",
    route_holder: "ses-sre",
  });
  const liveView = renderIssuePage();
  try {
    const row = await stateRow();
    expect(within(row).queryByTestId("issue-route-unreachable")).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-unknown")).not.toBeInTheDocument();
  } finally {
    liveView.unmount();
    live();
  }

  const unjudged = stubIssuePage({
    ...issue,
    route: "role:sre",
    route_status: "unknown",
    route_status_reason: "unreachable",
  });
  const unjudgedView = renderIssuePage();
  try {
    const row = await stateRow();
    expect(within(row).getByTestId("issue-route-unknown").textContent).toBe("Route reach unknown");
    expect(within(row).queryByTestId("issue-route-refilling")).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-unreachable")).not.toBeInTheDocument();
  } finally {
    unjudgedView.unmount();
    unjudged();
  }

  // A listener that is refilling its agent list says so: the route is unknown for that reason,
  // never unheld.
  const refilling = stubIssuePage({
    ...issue,
    route: "role:sre",
    route_status: "unknown",
    route_status_reason: "refilling",
  });
  const refillingView = renderIssuePage();
  try {
    const row = await stateRow();
    expect(within(row).getByTestId("issue-route-refilling").textContent).toBe(
      `Route reach unknown: ${AGENT_LIST_REFILLING}`
    );
    expect(within(row).queryByTestId("issue-route-unknown")).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-unreachable")).not.toBeInTheDocument();
  } finally {
    refillingView.unmount();
    refilling();
  }
});

// A route whose holder has missed its heartbeats is neither unreachable nor unjudged: the
// listener judged it and said the session is stalled, and what is sent to the issue waits for
// it. Folding that into either of the other two markers tells a reader to re-staff a role that
// is still held, or hides that anything is wrong at all (dispatch://INFRA-392).
test("IssuePage's header marks an unresponsive route holder apart from an unheld and a refilling route", async () => {
  const stateRow = async () => {
    const row = await screen.findByTestId("issue-work-row");
    await within(row).findByRole("button", { name: "Messages default to role:sre" });
    return row;
  };

  const stalled = stubIssuePage({
    ...issue,
    route: "role:sre",
    route_status: "holder_unresponsive",
    route_holder: "ses-sre",
  });
  const stalledView = renderIssuePage();
  try {
    const row = await stateRow();
    const marker = within(row).getByTestId("issue-route-unresponsive");
    expect(marker).toHaveTextContent("role:sre:");
    expect(marker).toHaveTextContent("Unresponsive, still reached");
    // The page raises no tooltip (dispatch://INFRA-1196 R21), so the marker's explanation is
    // drawn for a screen reader rather than held in its `title`.
    expect(marker).toHaveTextContent("waits until it answers");
    expect(within(row).queryByTestId("issue-route-unreachable")).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-unknown")).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-refilling")).not.toBeInTheDocument();
  } finally {
    stalledView.unmount();
    stalled();
  }

  // The other two states leave this marker off, so the three never read as one another.
  for (const reach of [
    { route_status: "no_holder" } as const,
    { route_status: "unknown", route_status_reason: "refilling" } as const,
  ]) {
    const other = stubIssuePage({ ...issue, route: "role:sre", ...reach });
    const otherView = renderIssuePage();
    try {
      expect(
        within(await stateRow()).queryByTestId("issue-route-unresponsive")
      ).not.toBeInTheDocument();
    } finally {
      otherView.unmount();
      other();
    }
  }
});

test("IssuePage omits creator metadata when an issue response omits it", async () => {
  // IssueDetails models the supported wire contract; this fixture probes a future raw response
  // that omits the optional value rather than serializing it as null.
  const issueWithoutCreator = { ...issue, created_by: undefined } as unknown as IssueDetails;
  const restore = stubIssuePage(issueWithoutCreator);
  const view = renderIssuePage();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(screen.queryByText(/^Opened by/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage shows subscribed agents with their title and their running state from the agent registry", async () => {
  const restore = stubIssuePage(
    issue,
    [],
    [
      subscriber("0123456789abcdef", "Planner (live)", true),
      // The subscribers endpoint's own `live` is a second snapshot of the listener; the chip
      // shows the registry's state, as every agent on the page does.
      subscriber("fedcba9876543210", "Historical (offline)", true),
    ],
    undefined,
    [listedAgent("0123456789abcdef", "Planner (live)")]
  );
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(2);
    expect(within(subscribed).getAllByRole("listitem")).toHaveLength(2);
    const planner = within(subscribed).getByRole("link", { name: "Planner (live)" });
    expect(planner).toHaveAttribute("href", "/agents/0123456789abcdef/live");
    // The panel raises no tooltip, so the session's own id is nowhere but in the link it
    // follows and the mark the registry reads (R21, `IssueMargin.tsx`'s `NoTooltips`).
    expect(planner.closest("[title]")).toBeNull();
    expect(within(subscribed).getByRole("link", { name: "Historical (offline)" })).toBeDefined();
    const states = () =>
      [...subscribed.querySelectorAll("[data-agent-name]")].map((agent) => [
        agent.getAttribute("data-agent-name"),
        agent.getAttribute("data-agent-state"),
      ]);
    await waitFor(() =>
      expect(states()).toEqual([
        ["0123456789abcdef", "running"],
        ["fedcba9876543210", "not-running"],
      ])
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage disables Unsubscribe and explains why for a subscriber reachable only via a broader wildcard", async () => {
  const restore = stubIssuePage(
    issue,
    [],
    [
      {
        last_seen: 1,
        live: true,
        removable: false,
        session_id: "0123456789abcdef",
        title: "Wide",
        topics: ["notifications.dispatch.>"],
        via: "notifications.dispatch.>",
      },
    ]
  );
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    // Why Unsubscribe is refused, in words: a disabled control takes no focus and raises no
    // tooltip, so the `title` it carried reached nobody (R21). The line says why in the
    // reader's words and draws no topic.
    expect(
      within(subscribed).getByText(
        "Its subscription covers other issues and documents too, so it cannot be unsubscribed from this one alone."
      )
    ).toBeDefined();
    const unsubscribeButton = within(subscribed).getByRole("button", { name: "Unsubscribe" });
    expect(unsubscribeButton).toHaveProperty("disabled", true);
    expect(unsubscribeButton.getAttribute("title")).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage shortens a subscriber id when its title is blank", async () => {
  const restore = stubIssuePage(issue, [], [subscriber("0123456789abcdef", " ")]);
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    const label = within(subscribed).getByText("session:01234567…");
    expect(label.closest("[title]")).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage hides the Subscribed agents header when there are no subscribers", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage lists project labels with issue labels selected and filters them", async () => {
  const user = userEvent.setup();
  const labeledIssue = { ...issue, labels: ["bug"] };
  const restore = stubIssuePage(labeledIssue);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { ...labeledIssue, open_asks: 0 },
    { ...issue, key: "CORE-2", labels: ["frontend", "backend"], open_asks: 0 },
    { ...issue, key: "CORE-3", labels: ["docs"], open_asks: 0 },
  ]);
  const view = renderIssuePage();

  try {
    await user.click(await screen.findByRole("button", { name: "Edit labels" }));
    const input = await screen.findByRole("combobox", { name: "Search or create label" });
    await waitFor(() => expect(listIssues).toHaveBeenCalledWith({ project: "CORE" }));
    expect(screen.getByRole("option", { name: "bug" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("option", { name: "frontend" })).toHaveAttribute(
      "aria-selected",
      "false"
    );

    await user.click(input);
    await user.paste("front");

    expect(screen.getByRole("option", { name: "frontend" })).toBeDefined();
    expect(screen.queryByRole("option", { name: "backend" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listIssues.mockRestore();
    restore();
  }
});

test("IssuePage creates a label from the multi-select and saves the final draft once on close", async () => {
  const user = userEvent.setup();
  const labeledIssue = { ...issue, labels: ["bug"] };
  const restore = stubIssuePage(labeledIssue);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { ...labeledIssue, open_asks: 0 },
    { ...issue, key: "CORE-2", labels: ["frontend"], open_asks: 0 },
  ]);
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...labeledIssue,
    labels: ["bug", "urgent"],
  });
  const view = renderIssuePage();

  try {
    await user.click(await screen.findByRole("button", { name: "Edit labels" }));
    const input = await screen.findByRole("combobox", { name: "Search or create label" });
    await user.click(input);
    await user.paste("urgent");
    await user.keyboard("{Enter}");

    expect(screen.getByRole("option", { name: "urgent" })).toHaveAttribute("aria-selected", "true");
    await user.click(screen.getByRole("button", { name: "Edit labels" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { labels: ["bug", "urgent"] }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    listIssues.mockRestore();
    restore();
  }
});

test("IssuePage does not save after a label is toggled off and back on", async () => {
  const user = userEvent.setup();
  const labeledIssue = { ...issue, labels: ["bug", "api"] };
  const restore = stubIssuePage(labeledIssue);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { ...labeledIssue, open_asks: 0 },
  ]);
  const patchIssue = answerIssuePatches().mockResolvedValue(labeledIssue);
  const view = renderIssuePage();

  try {
    const trigger = await screen.findByRole("button", { name: "Edit labels" });
    await user.click(trigger);
    await screen.findByRole("combobox", { name: "Search or create label" });
    await user.click(screen.getByRole("option", { name: "bug" }));
    await act(async () => {
      await Promise.resolve();
    });
    await user.click(screen.getByRole("option", { name: "bug" }));
    await user.click(trigger);
    await act(async () => {
      await Promise.resolve();
    });

    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    listIssues.mockRestore();
    restore();
  }
});

test("IssuePage returns focus to the label trigger when Escape closes an unchanged draft", async () => {
  const user = userEvent.setup();
  const labeledIssue = { ...issue, labels: ["bug"] };
  const restore = stubIssuePage(labeledIssue);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { ...labeledIssue, open_asks: 0 },
  ]);
  const patchIssue = answerIssuePatches().mockResolvedValue(labeledIssue);
  const view = renderIssuePage();

  try {
    const trigger = await screen.findByRole("button", { name: "Edit labels" });
    await user.click(trigger);
    await screen.findByRole("combobox", { name: "Search or create label" });
    await user.keyboard("{Escape}");

    expect(screen.queryByRole("listbox", { name: "Label options" })).not.toBeInTheDocument();
    expect(document.activeElement === trigger).toBe(true);
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    listIssues.mockRestore();
    restore();
  }
});

test("IssuePage shows a retryable error and hides the section when the subscribers lookup fails", async () => {
  const restore = stubIssuePage(issue);
  api.getIssueSubscribers = async () => {
    throw new Error("Envoy unavailable");
  };
  const view = renderIssuePage();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).not.toBeInTheDocument()
    );
    await screen.findByText("Subscribed agents unavailable — Envoy listener unreachable.");
  } finally {
    view.unmount();
    restore();
  }
});

// A refilling agent list is not an unreachable listener, and a failed read counts nobody: the
// header's toggle drops its number rather than say the issue has no subscribers.
test("IssuePage says the agent list is refilling and counts no subscribers from a failed read", async () => {
  const restore = stubIssuePage(issue);
  api.getIssueSubscribers = async () => {
    throw new ApiError(503, { code: "ENVOY_REFILLING", error: AGENT_LIST_REFILLING });
  };
  const view = renderIssuePage();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const notice = await screen.findByTestId("agent-list-refilling");
    expect(notice.textContent).toBe(AGENT_LIST_REFILLING);
    expect(
      screen.queryByText("Subscribed agents unavailable — Envoy listener unreachable.")
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Subscribers" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Subscribers: / })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage unsubscribes an agent after confirming the dialog", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  let currentSubscribers = [subscriber("0123456789abcdef", "Planner (live)")];
  api.getIssueSubscribers = async () => currentSubscribers;
  const unsubscribeCalls: [string, string][] = [];
  api.unsubscribeIssueSession = async (key, sessionId) => {
    unsubscribeCalls.push([key, sessionId]);
    currentSubscribers = [];
  };
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    await within(subscribed).findByText("Planner (live)");
    await user.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));

    const dialog = await screen.findByRole("dialog", { name: "Unsubscribe" });
    await user.click(within(dialog).getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(unsubscribeCalls).toEqual([["CORE-1", "0123456789abcdef"]]));
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage cancels an unsubscribe confirmation without calling the mutation", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue, [], [subscriber("0123456789abcdef", "Planner (live)")]);
  let unsubscribeCalled = false;
  api.unsubscribeIssueSession = async () => {
    unsubscribeCalled = true;
  };
  const view = renderIssuePage();

  try {
    const subscribed = await openSubscribedAgents(1);
    await user.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(unsubscribeCalled).toBe(false);
    expect(within(subscribed).getByText("Planner (live)")).toBeDefined();
  } finally {
    view.unmount();
    restore();
  }
});

const prTitleOnPullRequest = { check_suite_id: 10, event: "pull_request", name: "PR Title" };

for (const fixture of [
  {
    checkRuns: [{ check_suite: { id: 10 }, conclusion: "success", status: "completed" }],
    checks: "success",
    name: "success checks",
    workflowRuns: [prTitleOnPullRequest],
  },
  {
    checkRuns: [{ check_suite: { id: 10 }, conclusion: "failure", status: "completed" }],
    checks: "failure",
    name: "failure checks",
    workflowRuns: [prTitleOnPullRequest],
  },
  {
    checkRuns: [{ check_suite: { id: 10 }, conclusion: null, status: "in_progress" }],
    checks: "pending",
    name: "pending checks",
    workflowRuns: [prTitleOnPullRequest],
  },
  // A run a newer run of the same check, workflow and event replaced does not count, as GitHub's
  // merge reads it.
  {
    checkRuns: [
      { check_suite: { id: 10 }, conclusion: "cancelled", id: 1, status: "completed" },
      { check_suite: { id: 11 }, conclusion: "success", id: 3, status: "completed" },
      { check_suite: { id: 12 }, conclusion: "success", id: 2, name: "lint", status: "completed" },
    ],
    checks: "success",
    name: "a cancelled check its newer run passed",
    workflowRuns: [
      prTitleOnPullRequest,
      { ...prTitleOnPullRequest, check_suite_id: 11 },
      { check_suite_id: 12, event: "pull_request", name: "CI" },
    ],
  },
  // A failed run of one event still blocks beside a passing run of the same check on another.
  {
    checkRuns: [
      { check_suite: { id: 10 }, conclusion: "failure", id: 1, status: "completed" },
      { check_suite: { id: 11 }, conclusion: "success", id: 2, status: "completed" },
    ],
    checks: "failure",
    name: "a check failed for one event and passed for another",
    workflowRuns: [
      prTitleOnPullRequest,
      { ...prTitleOnPullRequest, check_suite_id: 11, event: "pull_request_review" },
    ],
  },
  // GitHub leaves a run whose event it does not evaluate out of a pull request's checks (an inline
  // review comment's run on sjawhar/legion#2234 and #2059): its failure or wait does not count.
  {
    checkRuns: [
      { check_suite: { id: 10 }, conclusion: "success", id: 1, status: "completed" },
      { check_suite: { id: 11 }, conclusion: "failure", id: 2, status: "completed" },
      { check_suite: { id: 12 }, conclusion: null, id: 3, status: "queued" },
    ],
    checks: "success",
    name: "a failed and a waiting check of an event GitHub does not evaluate",
    workflowRuns: [
      prTitleOnPullRequest,
      { ...prTitleOnPullRequest, check_suite_id: 11, event: "pull_request_review_comment" },
      { ...prTitleOnPullRequest, check_suite_id: 12, event: "workflow_dispatch" },
    ],
  },
  // A check run no workflow run made (an external App's) is its name and App: the App's newer run
  // replaces its older one.
  {
    checkRuns: [
      {
        app: { id: 50 },
        check_suite: { id: 20 },
        conclusion: "failure",
        id: 1,
        status: "completed",
      },
      {
        app: { id: 50 },
        check_suite: { id: 21 },
        conclusion: "success",
        id: 2,
        status: "completed",
      },
    ],
    checks: "success",
    name: "an external App's failed check its newer run passed",
    workflowRuns: [prTitleOnPullRequest],
  },
  // The same check name from two Apps is two checks: one App's failure still blocks.
  {
    checkRuns: [
      {
        app: { id: 50 },
        check_suite: { id: 20 },
        conclusion: "failure",
        id: 1,
        status: "completed",
      },
      {
        app: { id: 51 },
        check_suite: { id: 21 },
        conclusion: "success",
        id: 2,
        status: "completed",
      },
    ],
    checks: "failure",
    name: "one check name failed for one external App and passed for another",
    workflowRuns: [prTitleOnPullRequest],
  },
]) {
  test(`IssuePage renders a merged pull request with ${fixture.name}`, async () => {
    const restore = stubIssuePage(
      issueWithExternalLink("https://github.com/owner/repository/pull/7")
    );
    const checkRuns = fixture.checkRuns.map((run) => ({ id: 1, name: "pr-title", ...run }));
    const githubRest = spyOn(api, "githubRest").mockImplementation(async (path) => {
      if (path === "repos/owner/repository/pulls/7") {
        return new Response(
          JSON.stringify({
            head: { sha: "abcdef" },
            merged: true,
            state: "closed",
            title: "Merge native Dispatch",
          }),
          { headers: { "Content-Type": "application/json" } }
        );
      }
      if (path === "repos/owner/repository/commits/abcdef/check-runs?per_page=100&page=1") {
        return new Response(JSON.stringify({ check_runs: checkRuns }), {
          headers: { "Content-Type": "application/json" },
        });
      }
      if (path === "repos/owner/repository/actions/runs?head_sha=abcdef&per_page=100&page=1") {
        return new Response(JSON.stringify({ workflow_runs: fixture.workflowRuns }), {
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ state: "closed", title: "Wrong GitHub endpoint" }), {
        headers: { "Content-Type": "application/json" },
      });
    });
    const view = renderIssuePage();

    try {
      await screen.findByText("Merge native Dispatch");
      expect(screen.getByText("merged")).toBeDefined();
      // The chip is the page's word and GitHub's own count beside it, each marked as what it
      // is (dispatch://INFRA-1196 R21), so the chip is read whole rather than as one text node.
      expect(
        await screen.findByText(
          (_text, element) => element?.textContent === `checks: ${fixture.checks}`,
          { selector: "span[data-page-chrome]" }
        )
      ).toBeDefined();
    } finally {
      view.unmount();
      githubRest.mockRestore();
      restore();
    }
  });
}

test("IssuePage keeps a pull request's title and state when only its check-runs lookup fails", async () => {
  const restore = stubIssuePage(
    issueWithExternalLink("https://github.com/owner/repository/pull/7")
  );
  const githubRest = spyOn(api, "githubRest").mockImplementation(async (path) => {
    if (path === "repos/owner/repository/pulls/7") {
      return new Response(
        JSON.stringify({
          head: { sha: "abcdef" },
          merged: false,
          state: "open",
          waiting_on: "human",
          title: "Keep the title when checks vanish",
        }),
        { headers: { "Content-Type": "application/json" } }
      );
    }
    throw new Error("check-runs unavailable");
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = renderIssuePage("/issues/CORE-1", undefined, undefined, queryClient);

  try {
    // Only once the check-runs query has failed does the assertion mean anything: while it is
    // still pending the pre-fix code showed the title too.
    await waitFor(() =>
      expect(
        queryClient.getQueryState([
          "github-link-checks",
          "https://github.com/owner/repository/pull/7",
          "abcdef",
        ])?.status
      ).toBe("error")
    );
    // The failed check-runs read drops only the checks pill; the loaded title and state stay.
    const link = screen.getByRole("link", { name: /#7 Keep the title when checks vanish/ });
    expect(within(link).getByText("Keep the title when checks vanish")).toBeDefined();
    expect(within(link).getByText("open")).toBeDefined();
    expect(within(link).queryByText(/^checks:/)).not.toBeInTheDocument();
    // The link draws its repository, number, title and state; it carries no tooltip, because
    // the issue page draws none (dispatch://INFRA-1196 R21, `components/tooltips.tsx`).
    expect(link).not.toHaveAttribute("title");
  } finally {
    view.unmount();
    githubRest.mockRestore();
    restore();
  }
});

test("IssuePage continues to unfurl GitHub issues through the issues endpoint", async () => {
  const restore = stubIssuePage(
    issueWithExternalLink("https://github.com/owner/repository/issues/9")
  );
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(JSON.stringify({ state: "open", title: "Issue remains an issue" }), {
      headers: { "Content-Type": "application/json" },
    })
  );
  const view = renderIssuePage();

  try {
    await screen.findByText("Issue remains an issue");
    expect(screen.getByText("open")).toBeDefined();
    expect(githubRest).toHaveBeenCalledWith("repos/owner/repository/issues/9");
  } finally {
    view.unmount();
    githubRest.mockRestore();
    restore();
  }
});

test("IssuePage opens the Tree preset and leaves open asks out of the main column", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue, [
    {
      ...openIssueAsk,
      last_reply: null,
      priority: issue.priority,
      snoozed_until: null,
      thread: {
        answers: [],
        claimant: null,
        edits: [],
        followers: [],
        replies: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      },
    },
  ]);
  const view = renderIssuePage("/issues/CORE-1");

  try {
    await expectPreset("Tree");
    await user.click(within(await screen.findByTestId("issue-presets")).getByRole("button"));
    for (const preset of ["Tree", "Decisions", "Checks", "Spec", "Plan"] as const) {
      expect(screen.getByRole("menuitem", { name: preset })).toBeDefined();
    }
    expect(screen.queryByLabelText("Issue board")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

// `t a` is already the node menu's address, so Activity's chord is `t v`; the tablist is gone,
// so the chord opens the address the Activity tab answered and the page still resolves it. What
// that address draws is the issue's history in the page's column under the tree, and not in
// the panel, which is what the chord's label says.
test("IssuePage opens the activity address with the t v chord", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await expectPreset("Spec");
    await user.keyboard("t");
    await user.keyboard("v");

    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/activity")
    );
    await expectPreset("Tree");
    const history = document.getElementById("issue-activity-panel");
    expect(history).not.toBeNull();
    expect(history).not.toHaveAttribute("hidden");
    expect(history?.closest('[data-testid="issue-margin"]')).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

// What `?` lists for each chord: the page has no tablist, no Verification, Children, Artifacts
// or Collaborators tab, so a label naming one would send a reader looking for something the
// page does not have (dispatch://INFRA-1196 R21, `useIssueKeymap.ts`).
test("IssuePage names each chord by what it opens on this page", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage();

  try {
    await screen.findByTestId("issue-header");
    const labels = new Map(
      appKeymap
        .describe()
        .filter((binding) => binding.scope === "issue")
        .map((binding) => [binding.id, binding.label])
    );
    expect(labels.get("tab-tree")).toBe("Open the Tree preset");
    expect(labels.get("tab-verification")).toBe("Open the Checks preset");
    expect(labels.get("tab-plan")).toBe("Open the Plan preset");
    expect(labels.get("tab-conversation")).toBe("Open the conversation in the panel");
    expect(labels.get("tab-activity")).toBe("Open the issue's history under the tree");
    expect(labels.get("tab-children")).toBe("Open the issue's children");
    expect(labels.get("tab-artifacts")).toBe("Open the Files and documents menu");
    expect([...labels.values()].filter((label) => label.includes("tab"))).toEqual([]);
    // `t o` is gone with the chord: the spec's key table names two chords into the panel -
    // `t c` for the conversation and `t v` for the activity - and the collaborators were a
    // third it does not hold (dispatch://INFRA-1196 R9). `/collaborators` still resolves and
    // still opens them in the panel (R19); it is the key that is gone, and `?`, which lists
    // what the page binds, no longer offers a row for it.
    expect(labels.get("tab-collaborators")).toBeUndefined();
    expect(
      appKeymap
        .describe()
        .filter((binding) => binding.keys.join(" ") === "t o")
        .map((binding) => `${binding.scope}:${binding.id}`),
      "`t o` is still bound"
    ).toEqual([]);
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage opens the Decisions preset with the t d chord", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue({
    decisions: [],
    document: null,
    open_questions: [],
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await expectPreset("Spec");
    await user.keyboard("t");
    await user.keyboard("d");

    await expectPreset("Decisions");
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/decisions")
    );
    await waitFor(() => expect(decisions).toHaveBeenCalledWith("CORE-1"));
  } finally {
    view.unmount();
    decisions.mockRestore();
    restore();
  }
});

// `/collaborators` is the address the Collaborators tab carried. The tab is gone, and so is the
// `t o` chord that opened it: the spec's key table names two chords into the panel and that was
// a third (dispatch://INFRA-1196 R9). The address still resolves and still opens the panel's
// People section on who worked the issue (R12, R19).
test("IssuePage opens the issue's collaborators at their own address", async () => {
  const restore = stubIssuePage(issue);
  const collaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue({
    agents: [],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  });
  const view = renderIssuePage("/issues/CORE-1/collaborators");

  try {
    await screen.findByText("No one has worked this issue yet.");
    await waitFor(() => expect(collaborators).toHaveBeenCalledWith("CORE-1"));
  } finally {
    view.unmount();
    collaborators.mockRestore();
    restore();
  }
});

test("IssuePage renders a not-found view for an unrecognized tab suffix without fetching the issue", async () => {
  const getIssueSpy = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getMyStateSpy = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
  const view = renderIssuePage("/issues/CORE-1/not-a-tab");

  try {
    await screen.findByRole("heading", { name: "Page not found" });
    expect(screen.getByRole("link", { name: "Back to inbox" })).toBeDefined();
    expect(screen.queryByRole("tablist", { name: "Issue presets" })).not.toBeInTheDocument();
    expect(document.title).toBe("Not found · Dispatch");
    expect(getIssueSpy).not.toHaveBeenCalled();
    expect(getMyStateSpy).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getIssueSpy.mockRestore();
    getMyStateSpy.mockRestore();
  }
});

test("IssuePage keeps the Spec mounted across presets", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The mounted specification",
    version: 1,
    token: "sha256:mounted-specification",
  });
  const view = renderIssuePage("/issues/CORE-1/spec", undefined, "The mounted specification");

  try {
    await expectPreset("Spec");
    await waitFor(() =>
      expect(within(view.container).getByRole("article")).toHaveTextContent(
        "The mounted specification"
      )
    );

    await openPreset("Tree");
    await openPreset("Spec");

    expect(within(view.container).getByRole("article")).toHaveTextContent(
      "The mounted specification"
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage preserves an in-progress Conversation composer draft across a preset round-trip", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    await expectPreset("Tree");
    const composer = (await screen.findByLabelText("Comment")) as HTMLTextAreaElement;
    await user.click(composer);
    await user.paste("Draft in progress");
    expect(composer.value).toBe("Draft in progress");

    await openPreset("Spec");
    // A state picked in the panel opens no address from a preset: it would drop the preset, the
    // filter and the document the reader has open (R19), and the issue's own address is a
    // preset's address too. The way back to the conversation's own address is its chord.
    await openPreset("Tree");
    await user.keyboard("t");
    await user.keyboard("c");
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/conversation")
    );

    expect((screen.getByLabelText("Comment") as HTMLTextAreaElement).value).toBe(
      "Draft in progress"
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage remounts when switching issues, discarding unsaved local state", async () => {
  const user = userEvent.setup();
  const secondIssue: IssueDetails = {
    ...issue,
    artifacts: [
      {
        created_at: "2026-09-09T00:00:00Z",
        created_by: { id: "alice", kind: "user" },
        id: "artifact-2",
        issue_key: "CORE-2",
        project: "CORE",
        kind: "doc",
        name: "spec.md",
        primary: true,
        ref_key: "CORE-2/spec",
        session_id: null,
        slug: "spec",
        versions: [],
      },
    ],
    key: "CORE-2",
    primary_artifact_id: "artifact-2",
    route: "role:second-issue-route",
    title: "Second issue",
  };
  const originalGetIssue = api.getIssue;
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetInbox = api.getInbox;
  const originalGetMyState = api.getMyState;
  const originalGetIssueSubscribers = api.getIssueSubscribers;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Pre-warm CORE-2 so switching to it is an instant cache hit with no
  // pending/loading transition — the scenario where a stale-state leak would
  // otherwise go unnoticed by a naive loading-gate remount.
  queryClient.setQueryData(["issue", "CORE-2"], secondIssue);

  try {
    api.getIssue = async (key: string) => (key === "CORE-2" ? secondIssue : issue);
    api.getInbox = async () => [];
    api.getMyState = async () => ({
      "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
      "CORE-2": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
    });
    api.getIssueEvents = async () => [];
    api.getIssueSubscribers = async () => [];

    const view = render(
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>
        <QueryClientProvider client={queryClient}>
          <MarginProvider>
            <Link to="/issues/CORE-2">Go to CORE-2</Link>
            <Routes>
              <Route path="/issues/:key/*" element={<IssuePage />} />
            </Routes>
          </MarginProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );

    try {
      await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
      // What a route is, and what it is not: text beside the editor now, where the rail carried
      // it as a tooltip no phone could raise (dispatch://INFRA-1196 R11).
      expect(await screen.findByText(HELP_LINES.route)).toBeDefined();
      const routeInput = await screen.findByLabelText("Messages go to");
      await user.click(routeInput);
      await user.paste("role:not-saved-draft");
      await screen.findByDisplayValue("role:not-saved-draft");

      await user.click(screen.getByRole("link", { name: "Go to CORE-2" }));

      await screen.findByRole("heading", { level: 1, name: "Second issue" });
      // The route field marks itself dirty on edit and, absent a remount,
      // skips resyncing to the newly loaded issue's own route — leaking
      // CORE-1's unsaved draft onto CORE-2's page instead of showing CORE-2's
      // own route.
      expect(screen.queryByDisplayValue("role:not-saved-draft")).not.toBeInTheDocument();
      expect(
        await screen.findByRole("button", { name: "Messages default to role:second-issue-route" })
      ).toBeDefined();
    } finally {
      view.unmount();
    }
  } finally {
    api.getIssue = originalGetIssue;
    api.getIssueEvents = originalGetIssueEvents;
    api.getInbox = originalGetInbox;
    api.getMyState = originalGetMyState;
    api.getIssueSubscribers = originalGetIssueSubscribers;
  }
});

test("IssuePage highlights a historical quote from its comment deep link", async () => {
  const primaryArtifact = issue.artifacts?.[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "SQLite is local",
    version: 1,
    token: "sha256:sqlite-local",
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "SQLite is local",
    // The reading the server sends of that text, which the version page draws (`VersionView`).
    markdown_tree: {
      content: [{ content: [{ text: "SQLite is local", type: "text" }], type: "paragraph" }],
      type: "doc",
    },
    named: false,
    number: 1,
    summary: null,
  });
  const listComments = spyOn(api, "listComments").mockResolvedValue([
    {
      anchor: {
        artifact_id: primaryArtifact.id,
        block_id: null,
        mark_id: "m-1",
        orphaned: false,
        quote: "SQLit",
        version: 1,
      },
      ask_id: null,
      turn: null,
      author: { id: "alice", kind: "user" },
      body: "Check the storage engine.",
      created_at: "2026-09-09T00:00:00Z",
      id: "comment-1",
      issue_key: issue.key,
      reply_to: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      ...commentDeliveryFields(),
    },
  ]);
  const view = renderIssuePage("/issues/CORE-1/artifact/spec?v=1&comment=comment-1");

  try {
    await waitFor(() =>
      expect(
        view.runtime.editors.some((editor) =>
          markedText(editor.documentJSON, "proofComment").includes("SQLit")
        )
      ).toBe(true)
    );
    expect(view.runtime.editors.some((editor) => editor.focused.includes("comment-1"))).toBe(true);
    expect(
      within(
        view.container.querySelector<HTMLDivElement>("#issue-spec-panel") as HTMLDivElement
      ).getAllByRole("article")
    ).toHaveLength(1);
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    listComments.mockRestore();
    restore();
  }
});

test("IssuePage reports an ambiguous historical quote as changed text", async () => {
  const primaryArtifact = issue.artifacts?.[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "SQLite and SQLite",
    version: 1,
    token: "sha256:sqlite",
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "SQLite and SQLite",
    markdown_tree: {
      content: [{ content: [{ text: "SQLite and SQLite", type: "text" }], type: "paragraph" }],
      type: "doc",
    },
    named: false,
    number: 1,
    summary: null,
  });
  const listComments = spyOn(api, "listComments").mockResolvedValue([
    {
      anchor: {
        artifact_id: primaryArtifact.id,
        mark_id: "m-1",
        block_id: null,
        orphaned: false,
        quote: "SQLit",
        version: 1,
      },
      ask_id: null,
      turn: null,
      author: { id: "alice", kind: "user" },
      body: "Check the storage engine.",
      created_at: "2026-09-09T00:00:00Z",
      id: "comment-1",
      issue_key: issue.key,
      reply_to: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      ...commentDeliveryFields(),
    },
  ]);
  const view = renderIssuePage("/issues/CORE-1/artifact/spec?v=1&comment=comment-1");

  try {
    await within(view.container).findByText(
      "Text changed. The selected range no longer exists in this document."
    );
    expect(view.container.querySelector("mark.dispatch-anchor-history")).toBeNull();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    listComments.mockRestore();
    restore();
  }
});

test("IssuePage renders an image artifact at its own address", async () => {
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    project: "CORE",
    kind: "image" as const,
    name: "diagram.png",
    primary: false,
    ref_key: "CORE-1/diagram-png",
    session_id: null,
    slug: "diagram-png",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "image/png",
        named: false,
        number: 1,
        sha256: "image-sha",
        size: 10,
        summary: null,
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, imageArtifact] });
  const view = renderIssuePage("/issues/CORE-1/artifacts/diagram-png");

  try {
    await expectPreset("Tree");
    expect(screen.getByRole("img", { name: "diagram.png version 1" })).not.toBeNull();
    expect(screen.getByTestId("artifact-header").className).toContain("ring-2");
    // The ring says so to a reader and the mark says so to a layer that reads the page as a
    // reader does, which waits for both to go (`e2e/review-routes.ts`'s `waitForRouteSettled`).
    expect(screen.getByTestId("artifact-header").getAttribute("data-landing-ring")).toBe("true");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage does not mount an image as the hidden Spec document", async () => {
  const user = userEvent.setup();
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    project: "CORE",
    kind: "image" as const,
    name: "diagram.png",
    primary: false,
    ref_key: "CORE-1/diagram-png",
    session_id: null,
    slug: "diagram-png",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "image/png",
        named: false,
        number: 1,
        sha256: "image-sha",
        size: 10,
        summary: null,
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, imageArtifact] });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
    token: "sha256:primary",
  });
  const view = renderIssuePage("/issues/CORE-1/spec", "/issues/CORE-1/artifacts/diagram-png");

  try {
    await expectPreset("Spec");
    await waitFor(() => expect(getArtifactText).toHaveBeenCalledWith("artifact-1"));
    getArtifactText.mockClear();
    await user.click(screen.getByRole("link", { name: "Navigate to test route" }));
    await screen.findByRole("img", { name: "diagram.png version 1" });
    expect(getArtifactText).not.toHaveBeenCalledWith("artifact-image");
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage opens a non-spec document at its version route", async () => {
  const user = userEvent.setup();
  const documentArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-design",
    issue_key: "CORE-1",
    project: "CORE",
    kind: "doc" as const,
    name: "design.md",
    primary: false,
    ref_key: "CORE-1/design",
    session_id: null,
    slug: "design",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "text/markdown",
        named: true,
        number: 1,
        sha256: "document-sha",
        size: 10,
        summary: "Initial design",
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, documentArtifact] });
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...documentArtifact,
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design",
    version: 1,
    token: "sha256:design",
  });
  const view = renderIssuePage("/issues/CORE-1/artifacts/design");

  try {
    await expectPreset("Tree");
    expect(screen.getByRole("heading", { name: "design.md" })).not.toBeNull();
    expect(screen.getAllByRole("combobox", { name: "Version" })).toHaveLength(1);
    await user.selectOptions(screen.getByRole("combobox", { name: "Version" }), "1");
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        "/issues/CORE-1/artifacts/design?v=1"
      )
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage shows an unavailable image version instead of the latest image", async () => {
  const imageArtifact = {
    created_at: "2026-09-09T01:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-image",
    issue_key: "CORE-1",
    project: "CORE",
    kind: "image" as const,
    name: "diagram.png",
    primary: false,
    ref_key: "CORE-1/diagram-png",
    session_id: null,
    slug: "diagram-png",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T01:00:00Z",
        mime: "image/png",
        named: false,
        number: 1,
        sha256: "image-sha",
        size: 10,
        summary: null,
      },
    ],
  };
  const restore = stubIssuePage({ ...issue, artifacts: [...issue.artifacts, imageArtifact] });
  const view = renderIssuePage("/issues/CORE-1/artifacts/diagram-png?v=999");

  try {
    await expectPreset("Tree");
    expect(screen.getByText("Version 999 is not available for this artifact.")).not.toBeNull();
    expect(screen.queryByRole("img", { name: "diagram.png version 1" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage explains when an artifact slug does not exist", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/artifacts/missing");

  try {
    await screen.findByText("No artifact missing on CORE-1.");
    expect(screen.getByRole("link", { name: "View artifacts" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts"
    );
    await expectPreset("Tree");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage hides the artifact panel after switching to the Spec preset", async () => {
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/artifacts");

  try {
    await expectPreset("Tree");
    await openPreset("Spec");
    const artifactsPanel = view.container.querySelector<HTMLDivElement>("#issue-artifacts-panel");
    expect(artifactsPanel?.hidden).toBe(true);
    expect(artifactsPanel?.getAttribute("aria-hidden")).toBe("true");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage encodes primary-document version selection in the artifact route", async () => {
  const user = userEvent.setup();
  const primaryArtifact = issue.artifacts[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const versionedIssue = {
    ...issue,
    artifacts: [
      {
        ...primaryArtifact,
        versions: [
          {
            authors: [{ id: "alice", kind: "user" as const }],
            created_at: "2026-09-09T01:00:00Z",
            mime: "text/markdown",
            named: true,
            number: 1,
            sha256: "primary-sha",
            size: 10,
            summary: "Initial spec",
          },
        ],
      },
    ],
  };
  const restore = stubIssuePage(versionedIssue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...versionedIssue.artifacts[0],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
    token: "sha256:primary",
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await screen.findByLabelText("Version");
    await user.selectOptions(screen.getByLabelText("Version"), "1");
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        "/issues/CORE-1/artifacts/spec?v=1"
      )
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage renders exactly one Version combobox on the Spec tab", async () => {
  const primaryArtifact = issue.artifacts[0];
  if (primaryArtifact === undefined) {
    throw new Error("IssuePage test fixture needs a primary document.");
  }
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...primaryArtifact,
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
    token: "sha256:primary",
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    await screen.findByRole("combobox", { name: "Version" });
    expect(screen.getAllByRole("combobox", { name: "Version" })).toHaveLength(1);
    expect(screen.queryByTestId("artifact-header")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});
test("IssuePage keeps version controls in the active Spec tab row", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
    token: "sha256:primary",
  });
  const view = renderIssuePage("/issues/CORE-1/spec");

  try {
    const toolbar = await screen.findByTestId("issue-preset-toolbar");
    expect(within(toolbar).getByRole("combobox", { name: "Version" })).not.toBeNull();
    expect(within(toolbar).getByRole("button", { name: "Name version" })).not.toBeNull();
    expect(screen.queryByTestId("spec-document-toolbar")).not.toBeInTheDocument();

    // The Tree preset is the issue's own address, and the page draws the spec beside its tree
    // there, so that document keeps its toolbar: the version picker, Name version and the
    // approval are controls it has nowhere else (dispatch://INFRA-1196 R18). The three
    // presets below draw no document of this issue at all, so their row carries no toolbar.
    await openPreset("Tree");
    const treeToolbar = await screen.findByTestId("issue-preset-toolbar");
    expect(within(treeToolbar).getByRole("combobox", { name: "Version" })).not.toBeNull();
    for (const preset of ["Decisions", "Checks", "Plan"] as const) {
      await openPreset(preset);
      expect(screen.queryByTestId("issue-preset-toolbar")).not.toBeInTheDocument();
      expect(screen.queryByRole("combobox", { name: "Version" })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Name version" })).not.toBeInTheDocument();
    }
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage copies the issue key on click, its reference on a modifier click, and the Spec's reference at the shown version", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
    token: "sha256:primary",
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "# Primary v1",
    named: false,
    number: 1,
    summary: null,
  });
  const view = renderIssuePage("/issues/CORE-1/spec", "/issues/CORE-1/artifacts/spec?v=1");

  try {
    const keyButton = await screen.findByRole("button", { name: /^Copy issue key CORE-1 · / });
    await user.click(keyButton);
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith("CORE-1"));
    // A modifier click is the modifier held across the press: user-event's `click` takes no
    // modifier of its own.
    await user.keyboard("{Control>}");
    await user.click(keyButton);
    await user.keyboard("{/Control}");
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith("dispatch://CORE-1"));

    const toolbar = await screen.findByTestId("issue-preset-toolbar");
    await user.click(
      within(toolbar).getByRole("button", { name: "Copy reference dispatch://CORE-1/spec" })
    );
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith("dispatch://CORE-1/spec"));

    await user.click(screen.getByRole("link", { name: "Navigate to test route" }));
    await user.click(
      await within(toolbar).findByRole("button", {
        name: "Copy reference dispatch://CORE-1/artifact/spec@v1",
      })
    );
    await waitFor(() =>
      expect(writeText).toHaveBeenLastCalledWith("dispatch://CORE-1/artifact/spec@v1")
    );
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    restore();
  }
});

test("IssuePage updates whose turn when a human clarification is latest", async () => {
  const waitingIssue = {
    ...issue,
    open_asks: [{ ...openIssueAsk, last_reply: null, waiting_on: "human" as const }],
  };
  const restore = stubIssuePage(waitingIssue);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = renderIssuePage("/issues/CORE-1", undefined, undefined, queryClient);

  try {
    // The marker's words are the page's and the count beside them is the issue's, each marked
    // as what it is (dispatch://INFRA-1196 R21), so the marker is read whole.
    expect((await screen.findByTestId("issue-whose-turn")).textContent).toBe("Waiting on you (1)");
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
    expect((await screen.findByTestId("issue-whose-turn")).textContent).toBe(
      "Waiting on agents (1)"
    );
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage hides whose turn when the server sent open asks without waiting_on", async () => {
  const restore = stubIssuePage({
    ...issue,
    open_asks: [{ ...openIssueAsk, last_reply: null, waiting_on: undefined }],
  });
  const view = renderIssuePage("/issues/CORE-1");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(screen.queryByText(/^Waiting on (you|agents)/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});
test("IssuePage keeps state actions available while approval awaits", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const pin = screen.getByRole("button", { name: "Pin issue" });
    expect(pin).not.toBeNull();
    expect(pin).toHaveAttribute("aria-pressed", "false");
    expect(pin.querySelector("path")?.getAttribute("fill")).toBe("none");

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

test("IssuePage draws a gated root issue's steps where the header's one chip sat", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
  });
  const gated: Project = {
    created_at: "2026-09-09T00:00:00Z",
    gated_steps: ["decisions", "criteria", "spec"],
    open_asks: 0,
    key: "CORE",
    name: "Core",
  };
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([gated]);
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const stateActions = screen.getByTestId("issue-state-actions");
    const steps = await within(stateActions).findByTestId("issue-approval-steps");
    // One line per gated step, in the order the deployment published them.
    expect(
      Array.from(steps.querySelectorAll("[data-approval-step]")).map((step) =>
        step.getAttribute("data-approval-step")
      )
    ).toEqual(["decisions", "criteria", "spec"]);
    // The primary document keeps today's chip and its review actions, inside its own step.
    expect(within(steps).getByRole("button", { name: "Awaiting approval" })).not.toBeNull();
    expect(within(steps).getByRole("button", { name: "Approve" })).not.toBeNull();
    // The two steps nobody has written say so, rather than borrowing the spec's state.
    expect(within(steps).getAllByText("Not requested").length).toBe(2);
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

test("IssuePage keeps the primary document's chip when no gated step reviews it", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "awaiting" },
      },
    ],
  });
  // A deployment may gate the decisions and the Verification document and not the spec; the
  // spec still has an approval to answer, and only the header's own chip can answer it.
  const gated: Project = {
    created_at: "2026-09-09T00:00:00Z",
    gated_steps: ["decisions", "criteria"],
    open_asks: 0,
    key: "CORE",
    name: "Core",
  };
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([gated]);
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const stateActions = screen.getByTestId("issue-state-actions");
    const steps = await within(stateActions).findByTestId("issue-approval-steps");
    expect(
      Array.from(steps.querySelectorAll("[data-approval-step]")).map((step) =>
        step.getAttribute("data-approval-step")
      )
    ).toEqual(["decisions", "criteria"]);
    // Neither step holds the primary document, so neither carries its state or its actions.
    expect(within(steps).getAllByText("Not requested").length).toBe(2);
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

test("IssuePage fills and marks the issue pin when it is pinned", async () => {
  const restore = stubIssuePage(issue, [], [], {
    dismissed: [],
    last_read_seq: 0,
    pinned: true,
    seq: 0,
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    const pin = screen.getByRole("button", { name: "Unpin issue" });
    expect(pin).toHaveAttribute("aria-pressed", "true");
    expect(pin.querySelector("path")?.getAttribute("fill")).toBe("currentColor");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage's pin refreshes the sidebar's pinned list once the pin is saved", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const saved = { dismissed: [], last_read_seq: 0, pinned: true, seq: 1 };
  const putIssueState = spyOn(api, "putIssueState").mockResolvedValue(saved);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The sidebar's read of the pinned issues, as it is cached beside the page.
  queryClient.setQueryData(["issues", "pinned"], []);
  const view = renderIssuePage("/issues/CORE-1/conversation", undefined, undefined, queryClient);

  try {
    await within(view.container).findByRole("heading", { level: 1, name: issue.title });
    expect(queryClient.getQueryState(["issues", "pinned"])?.isInvalidated).toBe(false);
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
test("IssuePage keeps an unrequested draft approval passive", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [
      {
        ...issue.artifacts[0],
        approval: { latest_version: 1, state: "draft" },
      },
    ],
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

  try {
    expect(screen.queryByRole("button", { name: "Draft" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Approve$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request changes" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage shows a failed historical document version", async () => {
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Primary",
    version: 1,
    token: "sha256:primary",
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockRejectedValue(
    new Error("missing version")
  );
  const view = renderIssuePage("/issues/CORE-1/artifacts/spec?v=999");

  try {
    await screen.findByText("No version 999 of spec.md.");
    expect(screen.getByRole("link", { name: "View current version" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/spec"
    );
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    getArtifactVersion.mockRestore();
    restore();
  }
});

test("IssuePage closes an issue and reopens it into Backlog", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue({
    ...issue.artifacts[0],
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The mounted specification",
    version: 1,
    token: "sha256:mounted-specification",
  });
  const closeSave = Promise.withResolvers<IssueDetails>();
  const reopenSave = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) => {
    if (input.status === "done") return closeSave.promise;
    if (input.status === "backlog") return reopenSave.promise;
    throw new Error(`Unexpected issue update: ${JSON.stringify(input)}`);
  });
  const view = renderIssuePage();

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
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage lets a human move a Todo issue to In progress", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    status: "in_progress",
  });
  const view = renderIssuePage("/issues/CORE-1/conversation");

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

test("IssuePage reports a failed issue read on an item deep link instead of loading forever", async () => {
  // The landing waits on the issue to tell it which document an anchor names, so a page that
  // read the landing's pending state before the issue's error showed "Loading issue…" for good.
  const restore = stubIssuePage(issue);
  api.getIssue = async () => {
    throw new ApiError(500, { code: "SERVER_ERROR", error: "boom" });
  };
  const getComment = spyOn(api, "getComment").mockResolvedValue({
    comment: { ...itemComment },
    replies: [],
  });
  const view = renderIssuePage(`/issues/CORE-1/comments/${itemComment.id}`);

  try {
    expect(await screen.findByText("Couldn't load this issue")).not.toBeNull();
    expect(screen.getByRole("button", { name: "Retry" })).not.toBeNull();
    expect(screen.queryByText("Loading issue…")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getComment.mockRestore();
    restore();
  }
});

// INFRA-869. An address may write an item's id short; the page resolves it, opens the item
// under the whole id, and rewrites the address to it.
const WHOLE_ID = "4c9f3e12-1111-4222-8333-444444444444";
const OTHER_ID = "4c9f3e12-5555-4666-8777-888888888888";
const SHORT_ID = "4c9f3e12";

/** The comment the ids above name: unanchored, so the landing opens its Conversation turn
 *  rather than a document. */
const shortIdComment = { ...itemComment, anchor: null, id: WHOLE_ID };

function GoBack(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate(-1)} type="button">
      Go back
    </button>
  );
}

/** The issue page over a history of its own, so a test can go back from the address it opened
 *  and see what the page left behind it. */
function renderLandingHistory(entries: string[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const runtime = fakeDocumentRuntime({});
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={runtime.runtime}>
            <MarginProvider>
              <CurrentRoute />
              <GoBack />
              <Routes>
                <Route path="/issues/:key/*" element={<IssuePage />} />
              </Routes>
            </MarginProvider>
          </DocumentRuntime.Provider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  window.setTimeout(runtime.sync);
  return view;
}

test("IssuePage reads an item whose address writes its id whole without resolving anything", async () => {
  const restore = stubIssuePage(issue);
  const getReferences = spyOn(api, "getReferences");
  const getComment = spyOn(api, "getComment").mockResolvedValue({
    comment: shortIdComment,
    replies: [],
  });
  const view = renderIssuePage(`/issues/CORE-1/comments/${WHOLE_ID}`);

  try {
    await waitFor(() => expect(getComment).toHaveBeenCalledWith(WHOLE_ID));
    // The tabs are gone (R3): the conversation's own turns are what the landing opens.
    await screen.findByRole("list", { name: "Conversation turns" });
    // The whole id is read once, under itself, and nothing is resolved for it.
    expect(getComment.mock.calls).toEqual([[WHOLE_ID]]);
    expect(getReferences).not.toHaveBeenCalled();
    expect(screen.getByTestId("current-route").textContent).toBe(
      `/issues/CORE-1/comments/${WHOLE_ID}`
    );
  } finally {
    view.unmount();
    getComment.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

test("IssuePage resolves an address that writes an id short and replaces it with the whole id", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({
      id: WHOLE_ID,
      kind: "comment",
      ref: `dispatch://CORE-1/comment/${WHOLE_ID}`,
    }),
  });
  const getComment = spyOn(api, "getComment").mockResolvedValue({
    comment: shortIdComment,
    replies: [],
  });
  const view = renderLandingHistory(["/issues/CORE-1", `/issues/CORE-1/comments/${SHORT_ID}`]);

  try {
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        `/issues/CORE-1/comments/${WHOLE_ID}`
      )
    );
    expect(getReferences).toHaveBeenCalledWith(`dispatch://CORE-1/comment/${SHORT_ID}`);
    // The item is read under the whole id alone: the prefix is no key anything is cached under.
    await waitFor(() => expect(getComment.mock.calls).toEqual([[WHOLE_ID]]));
    // The rewrite replaced the address, so Back leaves the page the reader came from rather
    // than returning to the short address.
    await user.click(screen.getByRole("button", { name: "Go back" }));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1")
    );
  } finally {
    view.unmount();
    getComment.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

test("IssuePage lists every item an ambiguous id names, each linking to its whole id", async () => {
  const restore = stubIssuePage(issue);
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new ApiError(409, {
      candidates: [
        {
          first_line: "The first note",
          id: WHOLE_ID,
          issue_key: "CORE-1",
          kind: "comment",
          ref: `dispatch://CORE-1/comment/${WHOLE_ID}`,
        },
        {
          first_line: "The other note",
          id: OTHER_ID,
          issue_key: "CORE-1",
          kind: "comment",
          ref: `dispatch://CORE-1/comment/${OTHER_ID}`,
        },
      ],
      code: ID_AMBIGUOUS,
      error: `the id prefix "${SHORT_ID}" names 2 items; write more of the id`,
    })
  );
  const getComment = spyOn(api, "getComment");
  const view = renderIssuePage(`/issues/CORE-1/comments/${SHORT_ID}`);

  try {
    await screen.findByText("Several items match that link");
    const first = await screen.findByRole("link", { name: "The first note" });
    const other = screen.getByRole("link", { name: "The other note" });
    expect(first).toHaveAttribute("href", `/issues/CORE-1/comments/${WHOLE_ID}`);
    expect(other).toHaveAttribute("href", `/issues/CORE-1/comments/${OTHER_ID}`);
    // The list is a view of the page at the address the reader followed, not a route of its own.
    expect(screen.getByTestId("current-route").textContent).toBe(
      `/issues/CORE-1/comments/${SHORT_ID}`
    );
    expect(getComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getComment.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

test("IssuePage lists a match the reader cannot open by its kind alone, with no empty line", async () => {
  const restore = stubIssuePage(issue);
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new ApiError(409, {
      candidates: [
        {
          first_line: "The first note",
          id: WHOLE_ID,
          issue_key: "CORE-1",
          kind: "comment",
          ref: `dispatch://CORE-1/comment/${WHOLE_ID}`,
        },
        // A direct message: no address to open and no line the reader may read.
        { id: OTHER_ID, kind: "message", ref: "" },
      ],
      code: ID_AMBIGUOUS,
      error: `the id prefix "${SHORT_ID}" names 2 items; write more of the id`,
    })
  );
  const view = renderIssuePage(`/issues/CORE-1/messages/${SHORT_ID}`);

  try {
    await screen.findByText("Several items match that link");
    const rows = screen.getAllByRole("listitem");
    const unreadable = rows.find((row) => row.textContent?.includes("Message"));
    if (unreadable === undefined) throw new Error("the unreadable match is not listed");
    expect(unreadable.textContent?.trim()).toBe("Message");
    expect(unreadable.querySelector("a")).toBeNull();
  } finally {
    view.unmount();
    getReferences.mockRestore();
    restore();
  }
});

test("IssuePage shows its not-found view for an id prefix that names nothing", async () => {
  const restore = stubIssuePage(issue);
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: `no comment at ${SHORT_ID}` })
  );
  const getComment = spyOn(api, "getComment");
  const view = renderIssuePage(`/issues/CORE-1/comments/${SHORT_ID}`);

  try {
    expect(await screen.findByText("Comment not found")).not.toBeNull();
    expect(getComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getComment.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The panel is drawn wherever the shell's margin is not drawing it: at the width the margin is
// a sheet, and - this row - from 1280 px up with the reader's margin collapsed to its rail.
// Without it that reader loses every property in the panel, the conversation with them, and
// `/conversation`, `/collaborators`, `t c` and `t o` reach nothing.
test("IssuePage draws its panel in its own column wherever the margin draws none", async () => {
  const restore = stubIssuePage({ ...issue, blocked_by: ["CORE-9"] });
  setViewport(1280);
  const view = renderIssuePage();

  try {
    const panel = await screen.findByTestId("issue-margin");
    expect(within(panel).getByText("Opened by:")).toBeInTheDocument();
    expect(within(panel).getByRole("link", { name: "CORE-9" })).toBeInTheDocument();
    expect(within(panel).getByRole("listbox", { name: "Conversation" })).toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage leaves the panel to the margin where the margin keeps a column for it", async () => {
  const restore = stubIssuePage(issue);
  setViewport(1280);
  const view = renderIssuePage("/issues/CORE-1", undefined, undefined, undefined, true);

  try {
    await screen.findByTestId("issue-header");
    expect(screen.queryByTestId("issue-margin")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    restore();
  }
});

// Picking a state in the panel is a change of state and not a change of address: the preset the
// reader is reading, the `?q=` that narrows it and the document it has open all stay.
test("IssuePage keeps the preset and the filter when a conversation state is picked", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/spec?q=socket");

  try {
    const states = await screen.findByRole("listbox", { name: "Conversation" });
    await expectPreset("Spec");
    await user.click(within(states).getByRole("option", { name: "Questions" }));

    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/spec?q=socket");
    await expectPreset("Spec");
    expect(
      within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
        name: "Questions",
      })
    ).toHaveAttribute("aria-selected", "true");
  } finally {
    view.unmount();
    restore();
  }
});

// `/conversation` and `/activity` are the two addresses that are a state of their own, so from
// one of them picking the other's state opens it - and the filter goes with it.
test("IssuePage carries the filter into the address a conversation state names", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/conversation?q=socket");

  try {
    const states = await screen.findByRole("listbox", { name: "Conversation" });
    await user.click(within(states).getByRole("option", { name: "Everything" }));

    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        "/issues/CORE-1/activity?q=socket"
      )
    );
  } finally {
    view.unmount();
    restore();
  }
});

// The issue's own address draws the spec beside the tree, and a `#b-` there names a block of
// it, so it is a preset's address like the others and a picked state keeps it whole: the spec,
// the fragment and the filter. This row used to pin the opposite - Everything and Words, the
// latter already the selected state, sent the reader to `/activity` and `/conversation`, which
// closed the spec and dropped the `#b-` - and nothing the reader asked for needed that (R19).
test("IssuePage keeps the issue's own address, its spec and its fragment when a state is picked", async () => {
  const user = userEvent.setup();
  window.localStorage.removeItem(KEPT_STATE);
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1?q=socket#b-r1");

  try {
    await expectPreset("Tree");
    const spec = document.getElementById("issue-spec-panel") as HTMLElement;
    expect(spec).not.toHaveAttribute("hidden");
    for (const name of ["Everything", "What people said"]) {
      const states = await screen.findByRole("listbox", { name: "Conversation" });
      await user.click(within(states).getByRole("option", { name }));

      expect(screen.getByTestId("current-route").textContent, `${name} left the address`).toBe(
        "/issues/CORE-1?q=socket#b-r1"
      );
      await expectPreset("Tree");
      expect(document.getElementById("issue-spec-panel"), `${name} closed the spec`).toBe(spec);
      expect(spec, `${name} hid the spec`).not.toHaveAttribute("hidden");
      expect(
        within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
          name,
        })
      ).toHaveAttribute("aria-selected", "true");
    }
  } finally {
    view.unmount();
    restore();
    window.localStorage.removeItem(KEPT_STATE);
  }
});

// Words and Everything are addresses of their own, and picking one from a preset used to send
// the reader to the bare `/conversation` or `/activity`: the preset gone, the `?q=` cleared
// and the document they were reading closed. From a preset the state is the panel's alone
// (R19, `issue-address.ts`'s `conversationStatePath`).
test("IssuePage keeps the preset and the filter when Words or Everything is picked", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/spec?q=socket");

  try {
    await expectPreset("Spec");
    for (const name of ["Everything", "What people said"]) {
      const states = await screen.findByRole("listbox", { name: "Conversation" });
      await user.click(within(states).getByRole("option", { name }));

      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/spec?q=socket");
      await expectPreset("Spec");
      expect(
        within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
          name,
        })
      ).toHaveAttribute("aria-selected", "true");
    }
  } finally {
    view.unmount();
    restore();
  }
});

// `/activity` is the Everything state's own address, and the issue's whole history in the
// page's column is what that state draws there. Picking Questions keeps the address now, so
// the history goes with the state rather than standing under a panel showing the questions
// alone (`IssuePresetPanels.tsx`).
test("IssuePage hides the activity list when a state other than Everything is picked", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/activity");

  try {
    await screen.findByTestId("issue-header");
    const activity = document.getElementById("issue-activity-panel") as HTMLElement;
    expect(activity).not.toHaveAttribute("hidden");

    const states = await screen.findByRole("listbox", { name: "Conversation" });
    await user.click(within(states).getByRole("option", { name: "Questions" }));

    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/activity");
    expect(activity).toHaveAttribute("hidden");

    // Everything brings it back, and it was kept mounted all along.
    await user.click(
      within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
        name: "Everything",
      })
    );
    expect(document.getElementById("issue-activity-panel")).toBe(activity);
    expect(activity).not.toHaveAttribute("hidden");
  } finally {
    view.unmount();
    restore();
  }
});

// A pick on `/activity` holds while the reader stays on that address and no longer. They pick
// Questions there, leave for the Spec preset and come back: the address is theirs to read
// again, and it names Everything and the history Everything draws there. The pick used to stay
// tied to the address for the life of the page, so every later return showed no history.
test("IssuePage draws the activity again when the reader comes back to /activity after a pick there", async () => {
  const user = userEvent.setup();
  window.localStorage.removeItem(KEPT_STATE);
  const restore = stubIssuePage(issue);
  const view = renderIssuePage("/issues/CORE-1/activity", "/issues/CORE-1/spec");

  try {
    await screen.findByTestId("issue-header");
    const activity = document.getElementById("issue-activity-panel") as HTMLElement;
    const states = await screen.findByRole("listbox", { name: "Conversation" });
    await user.click(within(states).getByRole("option", { name: "Questions" }));
    expect(activity).toHaveAttribute("hidden");

    await user.click(screen.getByRole("link", { name: "Navigate to test route" }));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/spec")
    );
    await expectPreset("Spec");
    // Elsewhere the kept pick is the state the panel opens in (R13).
    expect(
      within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
        name: "Questions",
      })
    ).toHaveAttribute("aria-selected", "true");

    await user.keyboard("t");
    await user.keyboard("v");
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/activity")
    );
    expect(
      within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
        name: "Everything",
      })
    ).toHaveAttribute("aria-selected", "true");
    expect(
      activity,
      "/activity shows no history after a pick made there earlier"
    ).not.toHaveAttribute("hidden");
  } finally {
    view.unmount();
    restore();
    window.localStorage.removeItem(KEPT_STATE);
  }
});

/** The ask a landing row's `/asks/<id>` names, as the conversation lists it. */
const landingAsk: IssueAskThreadRow = {
  ...openIssueAsk,
  last_reply: null,
  thread: {
    answers: [],
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  },
};

/** The message a landing row's `/messages/<id>` names. */
const landingMessage: Message = {
  author: { id: "alice", kind: "user" },
  body: "A message the link names.",
  broadcast_id: null,
  created_at: "2026-09-11T00:00:00Z",
  deliveries: [],
  id: "message-landing",
  in_reply_to: null,
  issue_key: "CORE-1",
  reply_block: null,
  target: null,
};

/** The comment a landing row's `/comments/<id>` names: unanchored, so it lands on its turn. */
const landingComment = { ...itemComment, anchor: null, id: "comment-landing" };

// R19: every address resolves to what it names, and a comment, message or ask link names one
// entry of the conversation. Questions keeps the asks alone and Pinned what the reader pinned,
// so while the kept pick outranked these addresses the entry was not in the list at all: the
// page scrolled to nothing, opened no thread, and every later link from the Inbox, Replies or
// search landed nowhere for that login. The address names what people said, which holds the
// entry whatever the reader kept.
test("IssuePage lands a comment, message or ask link whatever conversation state the reader kept", async () => {
  const links = [
    { path: `/issues/CORE-1/comments/${landingComment.id}`, turn: `comment:${landingComment.id}` },
    { path: `/issues/CORE-1/messages/${landingMessage.id}`, turn: `message:${landingMessage.id}` },
    { path: `/issues/CORE-1/asks/${landingAsk.id}`, turn: `ask:${landingAsk.id}` },
  ];
  for (const kept of ["questions", "pinned"] as const) {
    for (const { path, turn } of links) {
      window.localStorage.setItem(KEPT_STATE, kept);
      const restore = stubIssuePage(issue);
      api.listComments = async () => [landingComment];
      api.listIssueMessages = async () => [{ message: landingMessage, replies: [] }];
      api.listIssueAskThreads = async () => [landingAsk];
      const reads = [
        spyOn(api, "getComment").mockResolvedValue({ comment: landingComment, replies: [] }),
        spyOn(api, "getMessage").mockResolvedValue({ message: landingMessage, replies: [] }),
        spyOn(api, "getAsk").mockResolvedValue({ ...landingAsk.thread, ask: landingAsk }),
      ];
      const view = renderIssuePage(path);

      try {
        await waitFor(
          () =>
            expect(
              document.querySelector(`li[data-turn="${turn}"]`)?.getAttribute("aria-current"),
              `${path} with ${kept} kept does not land on its entry`
            ).toBe("true"),
          { timeout: 5_000 }
        );
        expect(
          within(await screen.findByRole("listbox", { name: "Conversation" })).getByRole("option", {
            name: "What people said",
          })
        ).toHaveAttribute("aria-selected", "true");
      } finally {
        view.unmount();
        for (const read of reads) read.mockRestore();
        restore();
        window.localStorage.removeItem(KEPT_STATE);
      }
    }
  }
});

// The panel moves between the shell's margin and the page's own column as the reader shows the
// margin and collapses it again. It is one panel, drawn once and moved (`IssuePage`'s
// `panelStage`): drawn afresh in each host, every move remounted it, and a message the reader
// had typed - which `MentionComposer` holds in its own state - went with it.
test("IssuePage keeps a message typed under the collapsed margin when the margin is shown", async () => {
  const user = userEvent.setup();
  const restore = stubIssuePage(issue);
  setViewport(1280);
  const view = renderIssuePage(
    "/issues/CORE-1/conversation",
    undefined,
    undefined,
    undefined,
    false,
    true
  );

  try {
    const column = await screen.findByTestId("issue-margin");
    const composer = await within(column).findByLabelText<HTMLTextAreaElement>("Comment");
    await user.type(composer, "half a thought");
    expect(composer).toHaveValue("half a thought");

    act(() => view.showMargin(true));

    const inTheMargin = await screen.findByTestId("issue-margin");
    expect(within(await screen.findByTestId("stand-in-margin")).getByTestId("issue-margin")).toBe(
      inTheMargin
    );
    const moved = within(inTheMargin).getByLabelText<HTMLTextAreaElement>("Comment");
    expect(moved).toBe(composer);
    expect(moved).toHaveValue("half a thought");
  } finally {
    view.unmount();
    restore();
  }
});

// R5: the head reads the spec's summary out of the tree, and the first load draws the tree
// before the spec's blocks and their words have arrived. A summary not read yet is no spec
// without a summary, so the head says nothing about one until those reads answer, and then says
// what they held: the summary's sentences, or the page's line for a spec that carries none.
test("IssuePage's head says nothing about the summary until the reads that carry it answer", async () => {
  const restore = stubIssuePage(issue);
  // The address the tree gives a block of the spec, which is what the batched read is asked for.
  const summary = "dispatch://CORE-1/artifact/spec#b-summary";
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({
    targets: [
      {
        gone: false,
        kind: "block",
        name: ':::callout{#summary title="What this issue is"}\nThe page is one outline.\n:::',
        ref: summary,
      },
    ],
  });
  const callout: ArtifactBlock = {
    attributes: [],
    from: 0,
    id: "summary",
    references: { asks: 0, comments: 0 },
    to: 1,
    token: "summary",
    type: "callout",
  };
  try {
    for (const { blocks, head } of [
      { blocks: [callout], head: "The page is one outline." },
      { blocks: [], head: EMPTY_STATE_LINES.spec_summary },
    ]) {
      const held = Promise.withResolvers<ArtifactBlock[]>();
      const read = spyOn(api, "getArtifactBlocks").mockReturnValue(held.promise);
      const view = renderIssuePage();
      try {
        const drawn = await screen.findByTestId("issue-three-things");
        await waitFor(() => expect(read).toHaveBeenCalled());
        expect(
          drawn.textContent,
          "the head says the spec has no summary before the spec's blocks were read"
        ).not.toContain(EMPTY_STATE_LINES.spec_summary);
        act(() => held.resolve(blocks));
        await waitFor(() => expect(drawn.textContent).toContain(head));
      } finally {
        view.unmount();
        read.mockRestore();
      }
    }
  } finally {
    names.mockRestore();
    restore();
  }
});
