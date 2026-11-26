import { beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { fakeDocumentRuntime, markedText } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import type {
  Agent,
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
  ReferenceTargets,
  Subscriber,
  UserIssueState,
} from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { MarginSlotProvider } from "./margin-slot";
import { CONTROL_LABELS, EMPTY_STATE_LINES } from "./page-words";

/*
 * The whole new issue page, mounted through part A's root (`IssueOutlinePage.tsx`) with every
 * part's slot filled (dispatch://INFRA-1196 part N's c-units). Each case is a case of
 * `features/issue/IssuePage.test.tsx` at the checkpoint 29111db16c41, under its title there, read
 * on the new page: its documents opened in place of the outline, its version control beside the
 * views, and every address of today's page. `main`'s `IssuePage.test.tsx` is today's page's, and
 * does not change.
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

const itemComment = {
  // Anchored in the spec: a comment's own address opens the outline on its node all the same,
  // not the document it quotes (R20).
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

// INFRA-869. An address may write an item's id short; the page resolves it, opens the item
// under the whole id, and rewrites the address to it.
const WHOLE_ID = "4c9f3e12-1111-4222-8333-444444444444";
const SHORT_ID = "4c9f3e12";

/** The comment the ids above name. */
const shortIdComment = { ...itemComment, anchor: null, id: WHOLE_ID };

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
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
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

/** The new page at `path`, through part A's root, signed in as `alice`. No shell margin is
 *  mounted, so the page draws Details itself (`features/margin/margin-slot.tsx`). */
function renderIssuePage(
  path = "/issues/CORE-1",
  navigateTo?: string,
  seedText?: string,
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  })
) {
  const runtime = fakeDocumentRuntime({ text: seedText });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={runtime.runtime}>
            <MarginProvider>
              <MarginSlotProvider drawsSlot={false}>
                <CurrentRoute />
                {navigateTo === undefined ? null : (
                  <Link to={navigateTo}>Navigate to test route</Link>
                )}
                <Routes>
                  <Route path="/issues/:key/*" element={<IssueOutlinePage />} />
                </Routes>
              </MarginSlotProvider>
            </MarginProvider>
          </DocumentRuntime.Provider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  window.setTimeout(runtime.sync);
  return { ...view, runtime };
}

/**
 * The control of the filter row the page is on. happy-dom's window is 1024 px wide, which is the
 * width the row of five becomes one control naming the one on (dispatch://INFRA-1196 R2), so
 * that one control is what these tests read and press. The control draws the name and its
 * signal, and so is named by both - a word drawn inside a control belongs in that control's
 * name (WCAG 2.5.3, `IssuePresets.tsx`) - so the name is read from its own part.
 */
async function activePreset(): Promise<string> {
  const row = await screen.findByTestId("issue-presets");
  return within(row).getByRole("button").querySelector("[data-preset-name]")?.textContent ?? "";
}

async function expectPreset(name: string): Promise<void> {
  await waitFor(async () => {
    expect(await activePreset()).toBe(name);
  });
}

/** One item of that control's menu, by the filter's name: each is a `menuitemradio` named by
 *  its name and its signal, so the name is matched at the start of it. */
function presetItem(name: string): RegExp {
  return new RegExp(`^${name}\\b`, "u");
}

/** Moves to `name` through that one control's menu. */
async function openPreset(name: string): Promise<void> {
  const user = userEvent.setup();
  const row = await screen.findByTestId("issue-presets");
  await user.click(within(row).getByRole("button"));
  await user.click(await screen.findByRole("menuitemradio", { name: presetItem(name) }));
  await expectPreset(name);
}

// dispatch://INFRA-1196 R10, R19: a reader who follows a link to a node lands on that node's
// row, or the chain is out of their reach: the address moving to the Spec filter's `#b-r1` puts
// the focus on the statement's row, and so does a fresh load of that same address, which is how
// a citation pasted elsewhere arrives (the reviewer's finding on `/spec#b-<statement>`).
test("IssuePage focuses the node its address names, followed on the page or freshly loaded", async () => {
  const restore = stubIssuePage(issue);
  answerRequests(
    http.get("/api/v1/issues/:key/coverage", () =>
      Response.json({
        gaps: ["r1 has no check"],
        has_plan: false,
        issue: "CORE-1",
        requirements: [
          {
            block_id: "r1",
            checks: 0,
            gaps: ["r1 has no check"],
            plan_tasks: 0,
            ref: "dispatch://CORE-1/spec#b-r1",
            text: "The page draws the whole issue in one reading.",
          },
        ],
        spec_artifact_id: "artifact-1",
      } satisfies IssueCoverage)
    ),
    http.get("/api/v1/artifacts/:id/blocks", () =>
      Response.json([
        {
          attributes: [],
          from: 0,
          id: "r1",
          references: { asks: 0, comments: 0 },
          to: 1,
          token: "r1",
          type: "requirement",
        },
      ])
    )
  );
  const row = (container: HTMLElement) =>
    container.querySelector<HTMLElement>('[data-node-row][data-node-id="spec#b-r1"]');
  const focusedRow = () =>
    document.activeElement?.closest("[data-node-row]")?.getAttribute("data-node-id");
  const user = userEvent.setup();
  try {
    const followed = renderIssuePage("/issues/CORE-1/conversation", "/issues/CORE-1/spec#b-r1");
    try {
      await waitFor(() => expect(row(followed.container)).not.toBeNull());
      await user.click(screen.getByRole("link", { name: "Navigate to test route" }));
      await waitFor(() => expect(focusedRow()).toBe("spec#b-r1"));
    } finally {
      followed.unmount();
    }

    const loaded = renderIssuePage("/issues/CORE-1/spec#b-r1");
    try {
      await waitFor(() => expect(row(loaded.container)).not.toBeNull());
      await waitFor(() => expect(focusedRow()).toBe("spec#b-r1"));
    } finally {
      loaded.unmount();
    }
  } finally {
    restore();
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
    // The spec's own address opens it in place of the outline, once (R3).
    expect(within(screen.getByTestId("opened-artifact")).getAllByRole("article")).toHaveLength(1);
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
    await expectPreset("Conversation");
    expect(screen.getByRole("img", { name: "diagram.png version 1" })).not.toBeNull();
    expect(screen.getByTestId("artifact-header").className).toContain("ring-2");
    // The ring says so to a reader and the mark says so to a layer that reads the page as a
    // reader does, which waits for both to go (`e2e/review-routes.ts`'s `waitForRouteSettled`).
    expect(screen.getByTestId("artifact-header").getAttribute("data-highlighted")).toBe("");
  } finally {
    view.unmount();
    restore();
  }
});

test("IssuePage reads no document's text on the Spec filter, nor an image's when it opens", async () => {
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
    // The Spec filter draws no document, so it reads no document's text (R3, R21).
    expect(getArtifactText).not.toHaveBeenCalled();
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
    await expectPreset("Conversation");
    expect(screen.getByRole("heading", { name: "design.md" })).not.toBeNull();
    // The version control is behind its own small control at the filter row's right end.
    await user.click(screen.getByRole("button", { name: "Version" }));
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
    await expectPreset("Conversation");
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
    // The page's own line (part N's R2): not the slug, and not the old tab's word.
    await screen.findByText(EMPTY_STATE_LINES.document_missing);
    expect(screen.getByTestId("opened-artifact-missing")).not.toHaveTextContent("missing");
    // Its way back is the opened document's own control, to Documents' list.
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.back_to_documents }),
      "the missing document draws no way back to Documents' list"
    ).not.toBeNull();
    await expectPreset("Conversation");
  } finally {
    view.unmount();
    restore();
  }
});

// Part N's R1 and R2: Documents' own address and today's Artifacts tab's each draw Documents'
// list in the stage, in place of the outline, and no three sentences, no children's section and
// no Contents pane (root R8, R12, R14). The same issue on Conversation draws all three, so their
// absence is Documents'.
test("`/documents` and `/artifacts` draw Documents' list and no three sentences, children's section or Contents pane", async () => {
  const notes = {
    ...issue.artifacts[0],
    id: "artifact-notes",
    name: "notes.md",
    primary: false,
    ref_key: "CORE-1/notes-md",
    slug: "notes-md",
  };
  const withChild: IssueDetails = {
    ...issue,
    artifacts: [...issue.artifacts, notes],
    children: [
      {
        active_at: "2026-09-09T00:00:00Z",
        external_links: [],
        key: "CORE-2",
        status: "todo",
        subtree_done: 0,
        subtree_total: 0,
        title: "The child issue",
      },
    ],
  };
  const restore = stubIssuePage(withChild);
  const childRow = () => document.querySelector('[data-node-row][data-node-id="CORE-2"]');
  const tree = () => screen.queryByRole("tree", { name: CONTROL_LABELS.tree });
  try {
    const conversation = renderIssuePage("/issues/CORE-1/conversation");
    try {
      await waitFor(() => expect(childRow(), "Conversation draws no child's row").not.toBeNull());
      expect(screen.getByTestId("issue-contents")).toBeInTheDocument();
      expect(screen.getByTestId("issue-three-things")).toBeInTheDocument();
    } finally {
      conversation.unmount();
    }
    for (const address of ["/issues/CORE-1/documents", "/issues/CORE-1/artifacts"]) {
      const view = renderIssuePage(address);
      try {
        const list = await screen.findByRole("list");
        expect(within(list).getByRole("link", { name: "spec.md" }), address).toHaveAttribute(
          "href",
          "/issues/CORE-1/artifacts/spec"
        );
        expect(within(list).getByRole("link", { name: "notes.md" }), address).toHaveAttribute(
          "href",
          "/issues/CORE-1/artifacts/notes-md"
        );
        expect(tree(), `${address} draws the outline`).toBeNull();
        expect(childRow(), `${address} draws the child's row`).toBeNull();
        expect(screen.queryByTestId("issue-three-things"), address).toBeNull();
        expect(screen.queryByTestId("issue-contents"), address).toBeNull();
        expect(screen.getByTestId("current-route").textContent).toBe(address);
      } finally {
        view.unmount();
      }
    }
  } finally {
    restore();
  }
});

test("IssuePage pins a version picked beside the Spec filter on the filter's own address", async () => {
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
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1/spec?v=1")
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
    // Drawn once, shut behind its small control until the reader opens it.
    await screen.findByRole("combobox", { hidden: true, name: "Version" });
    expect(screen.getAllByRole("combobox", { hidden: true, name: "Version" })).toHaveLength(1);
    expect(screen.queryByTestId("artifact-header")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage draws the version control beside the filters, with no approval", async () => {
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
    // The control is drawn shut, behind its small control, and holds what it held open.
    const control = await screen.findByTestId("issue-version-control");
    expect(screen.getByRole("button", { name: "Version" })).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    expect(within(control).getByRole("combobox", { hidden: true, name: "Version" })).not.toBeNull();
    expect(
      within(control).getByRole("button", { hidden: true, name: "Name version" })
    ).not.toBeNull();
    expect(within(control).queryByRole("button", { hidden: true, name: /^Approve/ })).toBeNull();
    // No editor is on the page, so no document's text was read for it (R21).
    expect(getArtifactText).not.toHaveBeenCalled();

    // With no filter on no version control is drawn (R2), and the Decisions filter has no
    // document of its role, so it draws none either.
    await openPreset("Conversation");
    expect(screen.queryByTestId("issue-version-control")).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Version" })).not.toBeInTheDocument();
    await openPreset("Decisions");
    expect(screen.queryByTestId("issue-version-control")).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Version" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getArtifact.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

test("IssuePage reports a failed issue read on an item deep link instead of loading forever", async () => {
  // The item's own read is still out when the issue's fails, so a page that read the landing's
  // pending state before the issue's error showed "Loading issue…" for as long as it was.
  const restore = stubIssuePage(issue);
  api.getIssue = async () => {
    throw new ApiError(500, { code: "SERVER_ERROR", error: "boom" });
  };
  const getComment = spyOn(api, "getComment").mockReturnValue(new Promise(() => {}));
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
    // The tabs are gone (R3), and so is the panel's list: the outline is what the landing opens.
    await screen.findByRole("tree", { name: "Issue tree" });
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

// R20: an item's own address, and an address naming it beside a document (`?comment=`,
// `?ask=`), opens that node, focused, in the outline, wherever the item is anchored. An
// anchored comment, an anchored ask and an ask indexed from a block of the spec used to be sent
// on to the spec's address (`/spec?comment=`, `/spec?ask=`, `/spec#b-`), and those opened the
// spec in place of the outline, so the node the address named was drawn nowhere.
test("IssuePage opens an item anchored in a document on its node in the outline, focused", async () => {
  const anchoredAsk: IssueAskThreadRow = {
    ...landingAsk,
    anchor: { ...itemComment.anchor, mark_id: "m-2" },
    artifact_id: "artifact-1",
    id: "ask-anchored",
  };
  const blockAsk: IssueAskThreadRow = {
    ...landingAsk,
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: "q1",
    id: "ask-block",
  };
  const links = [
    { ask: anchoredAsk, node: itemComment.id, path: `/issues/CORE-1/comments/${itemComment.id}` },
    { ask: anchoredAsk, node: anchoredAsk.id, path: `/issues/CORE-1/asks/${anchoredAsk.id}` },
    { ask: blockAsk, node: "spec#b-q1", path: `/issues/CORE-1/asks/${blockAsk.id}` },
    {
      ask: anchoredAsk,
      node: itemComment.id,
      path: `/issues/CORE-1/spec?comment=${itemComment.id}`,
    },
    {
      ask: anchoredAsk,
      node: itemComment.id,
      path: `/issues/CORE-1/artifacts/spec?comment=${itemComment.id}`,
    },
    { ask: anchoredAsk, node: anchoredAsk.id, path: `/issues/CORE-1/spec?ask=${anchoredAsk.id}` },
    { ask: blockAsk, node: "spec#b-q1", path: `/issues/CORE-1/artifacts/spec?ask=${blockAsk.id}` },
  ];
  for (const { ask, node, path } of links) {
    const restore = stubIssuePage(issue);
    api.listComments = async () => [itemComment];
    api.listIssueAsks = async () => [ask];
    api.listIssueAskThreads = async () => [ask];
    const reads = [
      spyOn(api, "getComment").mockResolvedValue({ comment: itemComment, replies: [] }),
      spyOn(api, "getAsk").mockResolvedValue({ ...ask.thread, ask }),
    ];
    const view = renderIssuePage(path);

    try {
      await waitFor(
        () =>
          expect(
            document.activeElement?.closest("[data-node-row]")?.getAttribute("data-node-id"),
            `${path} does not focus its node`
          ).toBe(node),
        { timeout: 5_000 }
      );
      expect(screen.getByTestId("current-route").textContent, `${path} left its address`).toBe(
        path
      );
      expect(screen.getByRole("tree", { name: "Issue tree" })).toBeInTheDocument();
      expect(screen.queryByRole("region", { name: "Document editor" })).not.toBeInTheDocument();
    } finally {
      view.unmount();
      for (const read of reads) read.mockRestore();
      restore();
    }
  }
});

// "View in document" means the document: its address is the document's own with the comment
// named by `?mark=`, which opens that document with the comment marked in it, where `?comment=`
// would open the outline on the comment's node (R20, `refs/routes.ts`'s `openDocumentPath`).
test("IssuePage opens a document's ?mark= address on the document, the comment marked", async () => {
  const restore = stubIssuePage(issue);
  const listComments = spyOn(api, "listComments").mockResolvedValue([itemComment]);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The spec",
    token: "sha256:spec",
    version: 1,
  });
  const view = renderIssuePage(`/issues/CORE-1/artifacts/spec?mark=${itemComment.id}`);

  try {
    expect(await screen.findByTestId("opened-artifact")).toBeInTheDocument();
    expect(screen.queryByRole("tree", { name: "Issue tree" })).not.toBeInTheDocument();
    // The document reads its own comments only to mark the one an address names in it
    // (`ArtifactDocument`'s `commentId`).
    await waitFor(() => expect(listComments).toHaveBeenCalledWith("CORE-1", "artifact-1"));
    expect(screen.getByTestId("current-route").textContent).toBe(
      `/issues/CORE-1/artifacts/spec?mark=${itemComment.id}`
    );
  } finally {
    view.unmount();
    listComments.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

// `?mark=` names an ask anchored in a document as it names a comment: the issue's comments do
// not hold the id, so the document reads the ask and marks it (`ArtifactDocument`'s `askId`).
test("IssuePage opens a document's ?mark= address naming an ask on the document, the ask marked", async () => {
  const restore = stubIssuePage(issue);
  const anchoredAsk: IssueAskThreadRow = {
    ...landingAsk,
    anchor: { ...itemComment.anchor, mark_id: "m-2" },
    artifact_id: "artifact-1",
    id: "ask-anchored",
  };
  const listComments = spyOn(api, "listComments").mockResolvedValue([itemComment]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ...anchoredAsk.thread,
    ask: anchoredAsk,
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The spec",
    token: "sha256:spec",
    version: 1,
  });
  const view = renderIssuePage(`/issues/CORE-1/artifacts/spec?mark=${anchoredAsk.id}`);

  try {
    expect(await screen.findByTestId("opened-artifact")).toBeInTheDocument();
    expect(screen.queryByRole("tree", { name: "Issue tree" })).not.toBeInTheDocument();
    await waitFor(() => expect(getAsk).toHaveBeenCalledWith(anchoredAsk.id));
    // Read as the ask it is, never looked for among the document's comments.
    expect(listComments).not.toHaveBeenCalledWith("CORE-1", "artifact-1");
    expect(screen.getByTestId("current-route").textContent).toBe(
      `/issues/CORE-1/artifacts/spec?mark=${anchoredAsk.id}`
    );
  } finally {
    view.unmount();
    listComments.mockRestore();
    getAsk.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

// `?mark=` written short is resolved as every address's short id is, and the whole id replaces
// it under `?mark=`: written under `?comment=` the address would open the outline instead.
test("IssuePage resolves a ?mark= written short and writes the whole id under ?mark=", async () => {
  const restore = stubIssuePage(issue);
  const marked = { ...itemComment, id: WHOLE_ID };
  const listComments = spyOn(api, "listComments").mockResolvedValue([marked]);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({
      id: WHOLE_ID,
      kind: "comment",
      ref: `dispatch://CORE-1/comment/${WHOLE_ID}`,
    }),
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "The spec",
    token: "sha256:spec",
    version: 1,
  });
  const view = renderIssuePage(`/issues/CORE-1/artifacts/spec?mark=${SHORT_ID}`);

  try {
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        `/issues/CORE-1/artifacts/spec?mark=${WHOLE_ID}`
      )
    );
    expect(getReferences).toHaveBeenCalledWith(`dispatch://CORE-1/comment/${SHORT_ID}`);
    expect(await screen.findByTestId("opened-artifact")).toBeInTheDocument();
  } finally {
    view.unmount();
    listComments.mockRestore();
    getReferences.mockRestore();
    getArtifactText.mockRestore();
    restore();
  }
});

// R20: `?comment=` on an issue's address opens the outline on the comment's node; written short,
// the id is resolved first and the address replaced with the whole one, which names the node.
test("IssuePage resolves a ?comment= written short and focuses the comment's node", async () => {
  const restore = stubIssuePage(issue);
  const named = { ...itemComment, id: WHOLE_ID };
  api.listComments = async () => [named];
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({
      id: WHOLE_ID,
      kind: "comment",
      ref: `dispatch://CORE-1/comment/${WHOLE_ID}`,
    }),
  });
  const view = renderIssuePage(`/issues/CORE-1/spec?comment=${SHORT_ID}`);

  try {
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe(
        `/issues/CORE-1/spec?comment=${WHOLE_ID}`
      )
    );
    expect(getReferences).toHaveBeenCalledWith(`dispatch://CORE-1/comment/${SHORT_ID}`);
    await waitFor(
      () =>
        expect(
          document.activeElement?.closest("[data-node-row]")?.getAttribute("data-node-id")
        ).toBe(WHOLE_ID),
      { timeout: 5_000 }
    );
    expect(screen.queryByRole("region", { name: "Document editor" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getReferences.mockRestore();
    restore();
  }
});

// R20: an approval request is no node of the outline; the identity block draws the approval of
// the document it reviews, so the request's own address puts the focus on that entry there.
test("IssuePage focuses an approval request's entry in the identity block", async () => {
  const restore = stubIssuePage({
    ...issue,
    artifacts: [{ ...issue.artifacts[0], approval: { latest_version: 1, state: "awaiting" } }],
  });
  const approvalAsk: Ask = {
    ...openIssueAsk,
    approval: {
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 1,
      version: 1,
    },
    id: "ask-approval",
    kind: "approval",
  };
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ...landingAsk.thread,
    ask: approvalAsk,
  });
  const view = renderIssuePage(`/issues/CORE-1/asks/${approvalAsk.id}`);

  try {
    await waitFor(() =>
      expect(document.activeElement?.getAttribute("data-approval-artifact")).toBe("artifact-1")
    );
    expect(screen.getByTestId("current-route").textContent).toBe(
      `/issues/CORE-1/asks/${approvalAsk.id}`
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    restore();
  }
});
