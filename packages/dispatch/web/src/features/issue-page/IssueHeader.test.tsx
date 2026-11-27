import {
  beforeAll,
  beforeEach,
  expect,
  jest,
  type Mock,
  setSystemTime,
  spyOn,
  test,
} from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: user-event has no form for the presses and events the title's and the actions group's cases send: a press the browser cancels as it turns into a drag (its pointercancel and dragend, with no mouseup), a right press whose context menu takes its release (its contextmenu, with no pointerup), a tap's order (its pointer up before the mousedown that moves the focus, where user-event moves the focus as its pointer goes down), a blur of the window and its focus as the reader comes back, and a key pressed during an IME composition or, as Safari sends it, just after one.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import { artifactRecordHandler, readMarkHandlers } from "../../__tests__/proof-document";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type {
  ArtifactReview,
  Ask,
  Comment,
  IssueCoverage,
  IssueDetails,
  Project,
  StatusChangeAnswer,
  UserIssueState,
} from "../../api/types";
import { subsetRanges } from "../../fonts/inter/subset-script";
import {
  answerStatusChanges,
  fixtureAnswer,
  fixtureRefusal,
} from "../issue/parent-status/status-change-fixtures";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { parseIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../shell/keymap";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { ACTION_WORDS } from "./approval-chip-box";
import { decision, decisionsRead, shaped, specRef } from "./decisions-filter/decisions-fixtures";
import { readComment, waitInAct } from "./outline-fixtures";
import { CONTROL_LABELS } from "./page-words";
import { ShellNavigationContext } from "./shell-navigation";
import { HeaderSlot } from "./slots/header";
import { titleWrittenElsewhere } from "./title-elsewhere-fixture";
import { markedCommentNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

/** Answers the header's reads through `api`: the issue, the reader's state of it and the
 *  projects, whose gates only a root issue reads. A list without this issue's project gates
 *  nothing, which is the one chip these tests assert. The count reads a Change from the tree
 *  read, and no comments list: a read of one reaches the network, which fails the file. */
function stubHeader(
  nextIssue: IssueDetails,
  state: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 }
): () => void {
  const originalGetIssue = api.getIssue;
  const originalGetMyState = api.getMyState;
  const originalListProjects = api.listProjects;
  stubbedIssue = nextIssue;
  api.getIssue = async () => nextIssue;
  api.getMyState = async () => ({ "CORE-1": state });
  api.listProjects = async () => [];
  return () => {
    api.getIssue = originalGetIssue;
    api.getMyState = originalGetMyState;
    api.listProjects = originalListProjects;
    stubbedIssue = issue;
  };
}

/** The header's other reads, each answered with the API's empty shape: the coverage, whose
 *  statements the count's decisions are read against, the decisions and the document's record. */
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
    artifactRecordHandler((id) => (stubbedIssue.artifacts ?? []).find((each) => each.id === id)),
    ...readMarkHandlers()
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

// The count reads the reader's login, the decisions, the coverage and the tree, under the keys
// the outline reads them by, and a root issue's header reads the
// projects for its gated steps, under the key the shell's sidebar reads them by. The header mounts
// again each time the reader comes back to the issue's page, and under the app's `staleTime` a
// read is stale 30 s after it answered (`main.tsx`), each observer judging it by its own options:
// read with the app's defaults, the header mounted again past that would send all four and the
// projects read again, although the feed's events keep them fresh. It reads no comments list.
test("the header mounted again past 30 s sends none of its count's reads, nor the projects read, again", async () => {
  const restore = stubHeader(issue);
  // The issue's own read keeps the app's 30 s, so the header mounted again reads it again: that is
  // what says the mount came past the age the four and the projects read would go stale at.
  const getIssue = spyOn(api, "getIssue");
  // The login and the decisions answered here, and the coverage and the tree by what the file
  // already answers them with.
  const reads = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      entries: [],
      open_questions: [],
    }),
    spyOn(api, "getIssueCoverage"),
    spyOn(api, "getIssueTree"),
  ];
  const comments = spyOn(api, "listComments");
  // The issue has no parent, so its header reads the projects, answered by `stubHeader`'s list.
  const listProjects = spyOn(api, "listProjects");
  const sent = () => reads.map((read) => read.mock.calls.length);
  // The app's client's defaults (`main.tsx`), which a read that sets none of its own takes.
  const queryClient = new QueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
  let view = renderHeader("/issues/CORE-1", queryClient);

  try {
    // The login is the one the shell already holds, and each other read is sent once.
    await waitFor(() => expect(sent()).toEqual([0, 1, 1, 1]));
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
      0, 1, 1, 1,
    ]);
    expect(comments, "the header read the issue's comments").not.toHaveBeenCalled();
    expect(listProjects.mock.calls.length, "the header mounted again read the projects again").toBe(
      1
    );
  } finally {
    view.unmount();
    setSystemTime();
    for (const read of [...reads, comments]) read.mockRestore();
    listProjects.mockRestore();
    getIssue.mockRestore();
    restore();
  }
});

// A Change the reader asked of a decision counts under the decision whose block of the spec holds
// the block they wrote it in (`Anchor.block_id`), though another decision holds its words too: the
// tree read hangs it under that decision's node, which the header reads to tell which
// (`decision-review.ts`). Here the reader asked it of a decision tied to no statement, which is
// on no review list, and the decision of the list, read before it, holds the same words: that one
// still waits on the reader, and the count draws it. A count that read the Changes by their
// quotes alone would put the Change on the first decision whose words hold it, the one of the
// list, and count nothing.
test("the header counts a Change under the decision whose block of the spec holds its anchor", async () => {
  const listed = decision("listed", {
    links: [shaped(specRef("r1"))],
    text: ":::decision{#listed}\n## Draw it once\n\nDraw it once.\n:::\n",
  });
  const untied = decision("untied", {
    text: ":::decision{#untied}\n## Keep the model pure\n\nKeep the model pure. Draw it once.\n:::\n",
  });
  const change: Comment = {
    anchor: {
      artifact_id: issue.primary_artifact_id,
      block_id: "untied-words",
      mark_id: "change-1",
      orphaned: false,
      quote: "Draw it once.",
      version: 3,
    },
    answers: null,
    artifact_id: issue.primary_artifact_id,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "Say which pages this covers.",
    created_at: "2026-11-11T09:00:00Z",
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id: "change-1",
    issue_key: "CORE-1",
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
  const restore = stubHeader(issue);
  const reads = [
    spyOn(api, "getIssueDecisions").mockResolvedValue(
      decisionsRead([listed, untied], issue.artifacts[0])
    ),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: "CORE-1",
      requirements: [
        {
          block_id: "r1",
          checks: 1,
          gaps: [],
          plan_tasks: 0,
          ref: specRef("r1"),
          text: "The first.",
        },
      ],
      spec_artifact_id: issue.primary_artifact_id,
    }),
  ];
  const comments = spyOn(api, "listComments").mockResolvedValue([]);
  // The tree read's answer, which the header reads in place of the spec's blocks: each decision a
  // node of the issue, and the Change under the node of the decision it was written in.
  const tree = spyOn(api, "getIssueTree").mockResolvedValue(
    treeAnswer([
      treeNode("CORE-1", "issue", null),
      treeNode("listed", "decision", "CORE-1"),
      treeNode("untied", "decision", "CORE-1"),
      treeNode(change.id, "comment", "untied", {
        author: [change.author],
        made_at: change.created_at,
      }),
    ])
  );
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    expect((await screen.findByTestId("issue-whose-turn")).textContent).toBe("Waiting on you (1)");
    expect(tree).toHaveBeenCalled();
    expect(comments, "the header read the issue's comments").not.toHaveBeenCalled();
    expect(
      queryClient.getQueryCache().find({ exact: true, queryKey: ["comments", "CORE-1"] }),
      "the header observes the issue's comments list"
    ).toBeUndefined();
  } finally {
    view.unmount();
    for (const read of [...reads, tree, comments]) read.mockRestore();
    restore();
  }
});

// With the tree read answering a post's marks on its node (`edited_at`, `deleted_at`,
// `deleted_by`): a decision whose only Change the read answers deleted waits on the reader again,
// and one whose Change stands waits on its agent, so of two decisions of the review list the count
// draws one, read from Conversation's answer alone, with no comments list.
test("the header counts a decision whose only Change the tree read answers deleted, and not one whose Change stands", async () => {
  const asked = decision("asked", { links: [shaped(specRef("r1"))] });
  const dropped = decision("dropped", { links: [shaped(specRef("r1"))] });
  const changeOf = (id: string, deleted: boolean): Comment => ({
    ...readComment(id, 0),
    body: deleted ? "" : "Say which pages this covers.",
    created_at: "2026-11-11T09:00:00Z",
    ...(deleted
      ? { deleted_at: "2026-11-11T10:00:00Z", deleted_by: { id: "alice", kind: "user" } }
      : {}),
  });
  const restore = stubHeader(issue);
  const reads = [
    spyOn(api, "getIssueDecisions").mockResolvedValue(
      decisionsRead([asked, dropped], issue.artifacts[0])
    ),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: "CORE-1",
      requirements: [
        {
          block_id: "r1",
          checks: 1,
          gaps: [],
          plan_tasks: 0,
          ref: specRef("r1"),
          text: "The first.",
        },
      ],
      spec_artifact_id: issue.primary_artifact_id,
    }),
  ];
  const comments = spyOn(api, "listComments");
  const tree = spyOn(api, "getIssueTree").mockResolvedValue(
    treeAnswer([
      treeNode("CORE-1", "issue", null),
      treeNode("asked", "decision", "CORE-1"),
      markedCommentNode(changeOf("c-asked", false), "asked"),
      treeNode("dropped", "decision", "CORE-1"),
      markedCommentNode(changeOf("c-dropped", true), "dropped"),
    ])
  );
  const view = renderHeader("/issues/CORE-1", headerClient());
  try {
    await waitFor(() =>
      expect(screen.getByTestId("issue-whose-turn").textContent).toBe("Waiting on you (1)")
    );
    expect(comments, "the header read the issue's comments").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    for (const read of [...reads, tree, comments]) read.mockRestore();
    restore();
  }
});

// Decision 563 (4): a decision whose Change may stand on a comment page the tree read has not
// loaded - it last changed before the oldest comment the read's first page holds, which names an
// older page - is left out of the count until that page is read, which the header reads once the
// page has drawn. Then the count counts what the Change it finds, or finds none of, makes each: of
// two such decisions of the review list, the one the older page holds a Change of waits on its
// agent, and the other on the reader.
test("the header leaves a decision whose Change may stand on an older comment page out of its count until it reads that page", async () => {
  const changedAt = "2026-11-01T00:00:00Z";
  const asked = decision("asked", { changed_at: changedAt, links: [shaped(specRef("r1"))] });
  const unasked = decision("unasked", { changed_at: changedAt, links: [shaped(specRef("r1"))] });
  const held: Comment = { ...readComment("c-held", 0), created_at: "2026-11-11T09:00:00Z" };
  const change: Comment = {
    ...readComment("c-asked", 0),
    body: "Say which pages this covers.",
    created_at: "2026-11-05T09:00:00Z",
  };
  const restore = stubHeader({
    ...issue,
    open_asks: [{ ...openIssueAsk, last_reply: null, waiting_on: "human" as const }],
  });
  const reads = [
    spyOn(api, "getIssueDecisions").mockResolvedValue(
      decisionsRead([asked, unasked], issue.artifacts[0])
    ),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: "CORE-1",
      requirements: [
        {
          block_id: "r1",
          checks: 1,
          gaps: [],
          plan_tasks: 0,
          ref: specRef("r1"),
          text: "The first.",
        },
      ],
      spec_artifact_id: issue.primary_artifact_id,
    }),
  ];
  const older = Promise.withResolvers<TreeAnswer>();
  const tree = spyOn(api, "getIssueTree").mockImplementation(async (_key, query) =>
    query?.comments_before === undefined
      ? {
          ...treeAnswer([
            treeNode("CORE-1", "issue", null),
            treeNode("asked", "decision", "CORE-1"),
            treeNode("unasked", "decision", "CORE-1"),
            markedCommentNode(held, "CORE-1"),
          ]),
          comments_before: `${held.created_at},${held.id}`,
        }
      : older.promise
  );
  const pagesSent = () =>
    tree.mock.calls.filter(([, query]) => query?.comments_before !== undefined).length;
  const count = () => screen.getByTestId("issue-whose-turn").textContent;
  const view = renderHeader("/issues/CORE-1", headerClient());
  try {
    await waitFor(() => expect(pagesSent(), "the header read no older comment page").toBe(1));
    // Every read the count is drawn from has answered, and the older page is out.
    await waitInAct(20);
    expect(count(), "a decision whose Change may stand on the older page was counted").toBe(
      "Waiting on you (1)"
    );
    await act(async () =>
      older.resolve({
        ...treeAnswer([markedCommentNode(change, "CORE-1/tree#asked")]),
        comments_before: null,
      })
    );
    await waitFor(() => expect(count()).toBe("Waiting on you (2)"));
    expect(pagesSent(), "the older page was read other than once").toBe(1);
  } finally {
    view.unmount();
    for (const read of [...reads, tree]) read.mockRestore();
    restore();
  }
});

// Decision 574 (B): the tree read pages its walk as well as its comments, and a comment may stand
// on any page of the walk. So while its answer names a next page (`cursor`), with no older comment
// page (`comments_before: null`), a decision with no Change held is left out of the count until
// that page is read, which the header reads once the page has drawn. Then the count counts what
// the Change it finds, or finds none of, makes each: the one the next page holds a Change of
// waits on its agent, and the other on the reader.
test("the header leaves a decision whose Change may stand on the tree read's next page out of its count until it reads that page", async () => {
  const asked = decision("asked", { links: [shaped(specRef("r1"))] });
  const unasked = decision("unasked", { links: [shaped(specRef("r1"))] });
  const change: Comment = {
    ...readComment("c-asked", 0),
    body: "Say which pages this covers.",
    created_at: "2026-11-12T09:00:00Z",
  };
  const restore = stubHeader({
    ...issue,
    open_asks: [{ ...openIssueAsk, last_reply: null, waiting_on: "human" as const }],
  });
  const reads = [
    spyOn(api, "getIssueDecisions").mockResolvedValue(
      decisionsRead([asked, unasked], issue.artifacts[0])
    ),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: "CORE-1",
      requirements: [
        {
          block_id: "r1",
          checks: 1,
          gaps: [],
          plan_tasks: 0,
          ref: specRef("r1"),
          text: "The first.",
        },
      ],
      spec_artifact_id: issue.primary_artifact_id,
    }),
  ];
  const next = Promise.withResolvers<TreeAnswer>();
  const tree = spyOn(api, "getIssueTree").mockImplementation(async (_key, query) =>
    query?.cursor === undefined
      ? {
          ...treeAnswer([
            treeNode("CORE-1", "issue", null),
            treeNode("unasked", "decision", "CORE-1"),
            treeNode("asked", "decision", "CORE-1"),
          ]),
          comments_before: null,
          cursor: "page-2",
          remaining: 1,
        }
      : next.promise
  );
  const pagesSent = () => tree.mock.calls.filter(([, query]) => query?.cursor !== undefined).length;
  const count = () => screen.getByTestId("issue-whose-turn").textContent;
  const view = renderHeader("/issues/CORE-1", headerClient());
  try {
    await waitFor(() => expect(tree, "the header read no tree").toHaveBeenCalled());
    // Every read the count is drawn from has answered.
    await waitInAct(20);
    expect(count(), "a decision whose Change may stand on the next page was counted").toBe(
      "Waiting on you (1)"
    );
    await waitFor(() => expect(pagesSent(), "the header read no next page").toBe(1));
    await act(async () =>
      next.resolve(treeAnswer([markedCommentNode(change, "CORE-1/tree#asked")]))
    );
    await waitFor(() => expect(count()).toBe("Waiting on you (2)"));
    expect(pagesSent(), "the next page was read other than once").toBe(1);
  } finally {
    view.unmount();
    for (const read of [...reads, tree]) read.mockRestore();
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
    proven_sessions: false,
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

test("the header draws the steps of a root whose parent is in another project", async () => {
  // CORE-1 under OTHR-6 is a root of CORE: a parent of another project passes nothing on, so the
  // issue stands in CORE's gate and the header reads the projects to draw it.
  const restore = stubHeader({ ...issue, parent: "OTHR-6" });
  const gated: Project = {
    created_at: "2026-09-09T00:00:00Z",
    gated_steps: ["criteria", "spec"],
    open_asks: 0,
    key: "CORE",
    name: "Core",
    proven_sessions: false,
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
    ).toEqual(["criteria", "spec"]);
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
    proven_sessions: false,
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
    // The step holds no primary document, so it carries neither its state nor its actions.
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

// The page's typeface holds only the characters of `subset.sh`'s UNICODES, which the face's
// `unicode-range` in styles.css repeats: a character outside them is drawn in a face of the
// reader's own machine, at that face's width, so the header draws such a shape as an inline svg -
// the navigation's three strokes, the actions' three dots, the status's shape. Of the status's
// three shapes only ◐, the shape of a status that moves, is outside the typeface (✓ and ○ are in
// it), so the stub issue is in progress. Its title, key, project and names are ASCII, so the case
// reads every text node the header draws, author text among them, with the navigation's control
// drawn as the shell gives it below the phone's width, and once every read the header made has
// answered.
test("the header draws no character the page's typeface lacks", async () => {
  const ranges = subsetRanges();
  const restore = stubHeader({ ...issue, status: "in_progress" });
  const queryClient = headerClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <ShellNavigationContext.Provider value={{ onOpen: () => {}, open: false }}>
      {headerTree("/issues/CORE-1/conversation", queryClient)}
    </ShellNavigationContext.Provider>
  );

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    const header = screen.getByTestId("issue-header");
    // The three controls that draw a shape in place of a character are all in the header read,
    // the status's pill with a status that moves.
    expect(within(header).getByRole("button", { name: "Open navigation" })).toBeInTheDocument();
    expect(
      within(header).getByRole("button", { name: CONTROL_LABELS.issue_actions })
    ).toBeInTheDocument();
    expect(within(header).getByRole("combobox", { name: "Status" })).toHaveValue("in_progress");
    const offenders: string[] = [];
    let read = 0;
    const walker = document.createTreeWalker(header, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = node.textContent ?? "";
      for (const character of text) {
        read += 1;
        const point = character.codePointAt(0) ?? 0;
        if (ranges.some(([first, last]) => point >= first && point <= last)) continue;
        // The element that drew it, and the part of the header around it, by the name a reader
        // hears, its test id or its words.
        const drawer = node.parentElement;
        const part = drawer?.closest("a, button, h1, select, [data-testid]");
        const partName =
          part?.getAttribute("aria-label") ??
          part?.getAttribute("data-testid") ??
          part?.textContent;
        const code = point.toString(16).toUpperCase().padStart(4, "0");
        offenders.push(
          `U+${code} ${character} in "${text}", drawn by <${drawer?.tagName.toLowerCase()}> in ${part?.tagName.toLowerCase()} "${partName}"`
        );
      }
    }
    expect(offenders).toEqual([]);
    // A walk that read no character would pass with nothing read.
    expect(read).toBeGreaterThan(0);
  } finally {
    view.unmount();
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

// A header write the reader stopped at the shell's indicator may have been saved: the header says
// so in today's header's words (`stoppedNote`, `stoppedWriteNote`), not that it failed. Without
// them it read "Could not update this issue." or "Could not save pin status.", a refusal
// Dispatch never gave.
test("a title save the reader stopped says it may have been saved, not that it failed", async () => {
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    await editTitle(userEvent.setup(), "Review the plan");
    jest.useFakeTimers();
    await userOnFakeClock().keyboard("{Enter}");
    await stopTheLateWrite(queryClient);
    expect(
      within(view.container).getByText(/^Dispatch has not confirmed this change/).textContent
    ).toBe(
      stoppedWriteNote("this change to the issue", "saved", {
        anew: "Retry sends what the field holds now as a new write.",
      })
    );
    expect(within(view.container).queryByText("Could not update this issue.")).toBeNull();
  } finally {
    realClock();
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

test("a pin the reader stopped says it may have been saved, not that it failed", async () => {
  const restore = stubHeader(issue);
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const putIssueState = spyOn(api, "putIssueState").mockRejectedValue(
    new NetworkError("/api/v1/me/issues/CORE-1/state", new TypeError("Failed to fetch"))
  );
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    await within(view.container).findByRole("heading", { level: 1, name: issue.title });
    await openIssueActions();
    const pin = within(view.container).getByRole("button", { name: "Pin issue" });
    jest.useFakeTimers();
    await userOnFakeClock().click(pin);
    await stopTheLateWrite(queryClient);
    expect(
      within(view.container).getByText(/^Dispatch has not confirmed the pin status/).textContent
    ).toBe(
      stoppedWriteNote("the pin status", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
    expect(within(view.container).queryByText("Could not save pin status.")).toBeNull();
  } finally {
    realClock();
    view.unmount();
    putIssueState.mockRestore();
    state.mockRestore();
    restore();
  }
});

// The shell's pending writes make a stopped pin again as well as the header's own Retry does
// (`Writes.retry`). Once that lands the pin is saved, so the header's line that Dispatch has not
// confirmed it goes, as today's header's does (`useKeyedWrite`).
test("a stopped pin's line goes once the shell's Retry of it lands", async () => {
  const restore = stubHeader(issue);
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const putIssueState = spyOn(api, "putIssueState").mockRejectedValue(
    new NetworkError("/api/v1/me/issues/CORE-1/state", new TypeError("Failed to fetch"))
  );
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    await within(view.container).findByRole("heading", { level: 1, name: issue.title });
    await openIssueActions();
    const pin = within(view.container).getByRole("button", { name: "Pin issue" });
    jest.useFakeTimers();
    await userOnFakeClock().click(pin);
    await stopTheLateWrite(queryClient);
    expect(
      within(view.container).queryByText(/^Dispatch has not confirmed the pin status/)
    ).not.toBeNull();
    const stoppedTries = putIssueState.mock.calls.length;
    putIssueState.mockResolvedValue({ dismissed: [], last_read_seq: 0, pinned: true, seq: 1 });
    const [row] = writesOf(queryClient).rows();
    act(() => writesOf(queryClient).retry(row?.id ?? 0));
    await tick(1_000);
    expect(putIssueState.mock.calls.length, "the shell's Retry sent the pin again").toBe(
      stoppedTries + 1
    );
    expect(writesOf(queryClient).rows(), "the shell's Retry of the pin did not land").toEqual([]);
    expect(
      within(view.container).queryByText(/^Dispatch has not confirmed the pin status/),
      "the stopped pin's line stayed after the shell's Retry of it landed"
    ).toBeNull();
  } finally {
    realClock();
    view.unmount();
    putIssueState.mockRestore();
    state.mockRestore();
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

/** Whether the header's key `id` applies now, as the key card reads it (`appKeymap.describe`). */
function keyApplies(id: string): boolean | undefined {
  return appKeymap.describe().find((key) => key.scope === "issue" && key.id === id)?.enabled;
}

// The pin stands in the actions group, which is drawn hidden while the menu is shut, and
// `Shift+P` presses it there: the key reads the pin up to that group (`useHeaderKeys.ts`), so it
// is offered and pins with the menu shut, and the menu stays shut.
test("Shift+P is offered with the actions menu shut, and pins the issue", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const putIssueState = spyOn(api, "putIssueState").mockResolvedValue({
    dismissed: [],
    last_read_seq: 0,
    pinned: true,
    seq: 1,
  });
  const view = renderHeader();

  try {
    const heading = await screen.findByRole("heading", { level: 1, name: issue.title });
    const control = screen.getByRole("button", { name: CONTROL_LABELS.issue_actions });
    expect(control).toHaveAttribute("aria-expanded", "false");
    expect(keyApplies("pin"), "Shift+P is off while the actions menu is shut").toBe(true);
    act(() => heading.focus());
    await user.keyboard("{Shift>}P{/Shift}");
    await waitFor(() =>
      expect(putIssueState, "Shift+P with the actions menu shut sent no pin").toHaveBeenCalledWith(
        "CORE-1",
        { pinned: true },
        keyedAttempt
      )
    );
    expect(control, "Shift+P opened the actions menu").toHaveAttribute("aria-expanded", "false");
  } finally {
    view.unmount();
    putIssueState.mockRestore();
    restore();
  }
});

// The pin is disabled while its write is out, and the menu it was pressed from is shut again by
// then: read up to that shut group, a disabled pin still takes no press, so `Shift+P` is off. A
// key press is not read here: a disabled button takes no click whatever the key rule says.
test("Shift+P is off while the pin's write is out", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const saved = Promise.withResolvers<UserIssueState>();
  const putIssueState = spyOn(api, "putIssueState").mockImplementation(() => saved.promise);
  const view = renderHeader();

  try {
    await screen.findByRole("heading", { level: 1, name: issue.title });
    await openIssueActions();
    const pin = screen.getByRole("button", { name: CONTROL_LABELS.pin_issue });
    await user.click(pin);
    await waitFor(() =>
      expect(pin, "the pin is not disabled while its write is out").toBeDisabled()
    );
    expect(screen.getByRole("button", { name: CONTROL_LABELS.issue_actions })).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    expect(keyApplies("pin"), "Shift+P is offered while the pin is disabled").toBe(false);
  } finally {
    saved.resolve({ dismissed: [], last_read_seq: 0, pinned: true, seq: 1 });
    view.unmount();
    putIssueState.mockRestore();
    restore();
  }
});

// The header's keys read the page's one rule (`usable-control.ts`), which refuses a control that
// is busy, `aria-disabled`, as it refuses a disabled one: `s` is off while the status select is
// `aria-disabled`, and focuses nothing.
test("s is off while the status select is aria-disabled", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const heading = await screen.findByRole("heading", { level: 1, name: issue.title });
    const status = within(screen.getByTestId("issue-state-actions")).getByRole("combobox", {
      name: "Status",
    });
    expect(keyApplies("status"), "s is off while the status select takes a press").toBe(true);
    act(() => status.setAttribute("aria-disabled", "true"));
    expect(keyApplies("status"), "s is offered while the status select is busy").toBe(false);
    act(() => heading.focus());
    await user.keyboard("s");
    expect(status, "s focused a busy status select").not.toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

// The header's focus keeper gives a control of the Request changes dialog, lost as the dialog
// shuts, to the document's chip (`chipOfLostAction`). The dialog gives the focus back to the
// header's Request changes as it shuts; once the request is answered the header draws neither
// action, so the focus falls from the dialog's own control: its submit, which reads "Submitting…"
// while the change request is written, its text box, which holds the reader's reason, or its
// Cancel. The keeper reads only where the lost control stood, never its words: a control that
// stood inside the box `ApprovalChip` draws the chip in - its two actions, and its dialogs'
// submit, text box and Cancel - goes to that chip, whatever it reads, rather than to "Issue
// actions". Where the reader sent a change request, the chip also hands the focus to itself once
// the read after it removes Request changes (`ApprovalChip`), so the first three rows below hold
// where the focus ends, whichever of the two moves it; the rows after them, where nothing was
// sent, are the keeper's alone.

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

/** The chip's two actions the header draws, found as the header finds them: by the words each
 *  draws (`ACTION_WORDS`), since a name may also say the document. The dialog stands inside the
 *  same line, so its own Request changes is left out. */
function headerReviewActions(stateActions: HTMLElement): HTMLElement[] {
  const patterns = Object.values(ACTION_WORDS);
  return within(stateActions)
    .queryAllByRole("button")
    .filter(
      (button) =>
        patterns.some((pattern) => pattern.test(button.textContent ?? "")) &&
        button.closest('[role="dialog"]') === null
    );
}

/** The document's approval chip on the header's line of state controls. */
function documentChip(stateActions: HTMLElement): HTMLElement {
  const chip = stateActions.querySelector<HTMLElement>('[data-approval-artifact="artifact-1"]');
  if (chip === null) throw new Error("the header draws no approval chip for the document");
  return chip;
}

/** Answers the issue's reads with the waiting request until the reader's change request lands,
 *  and with the answered request from then on, as the server does. The write lands once `landing`
 *  settles, at once unless a case holds it. Returns the spy of the review's write and what puts
 *  both back. */
function answerChangeRequest(landing: Promise<void> = Promise.resolve()): {
  readonly createArtifactReview: Mock<typeof api.createArtifactReview>;
  readonly restore: () => void;
} {
  let read = awaitingApprovalIssue();
  const restoreHeader = stubHeader(read);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async () => read);
  const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(async () => {
    await landing;
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

// Escape closes the dialog while its change request is written, and the write goes on: the dialog
// gives the focus back to the header's Request changes, which refuses a press until the write
// lands and its read is drawn. That read answers the request, so Request changes goes with the
// focus on it, and the focus must reach the document's chip, not "Issue actions" or the body.
test("a change request closed by Escape while its write is out leaves the focus on the document's chip once the answer removes Request changes", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<void>();
  const { createArtifactReview, restore } = answerChangeRequest(landing.promise);
  const view = renderHeader();

  try {
    const { dialog, stateActions } = await openRequestChanges(user);
    expect(headerReviewActions(stateActions).length).toBe(2);
    const requestChanges = headerReviewActions(stateActions).find((action) =>
      ACTION_WORDS["request-changes"].test(action.textContent ?? "")
    );
    const submit = within(dialog).getByRole("button", { name: "Request changes" });
    act(() => submit.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(submit).toHaveTextContent("Submitting…"));
    expect(submit).toHaveFocus();
    await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
    expect(createArtifactReview).toHaveBeenCalledWith(
      "artifact-1",
      { reason: CHANGE_REASON, state: "changes_requested" },
      keyedAttempt
    );

    // The write is held: Escape closes the dialog all the same, and the focus goes back to the
    // header's Request changes, refused while the write is out.
    await user.keyboard("{Escape}");
    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    await waitFor(() => expect(requestChanges).toHaveFocus());
    expect(requestChanges).toHaveAttribute("aria-disabled", "true");
    expect(createArtifactReview).toHaveBeenCalledTimes(1);

    // The write lands, and the issue's read after it answers the request: the header draws
    // neither action, and the focus is on the document's chip.
    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(headerReviewActions(stateActions)).toEqual([]));
    await waitFor(() => expect(documentChip(stateActions)).toHaveFocus());
    expect(createArtifactReview).toHaveBeenCalledTimes(1);
  } finally {
    // A case that fails while the write is held still lets it land before the header goes.
    landing.resolve();
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

// The keeper finds the document's chip from where the lost control stood (`chipOfLostAction`):
// the box `ApprovalChip` draws the chip in holds only the chip, its two actions and the dialogs it
// opens. The Status select stands on the same line but outside that box, so when the issue's read
// turns closed and disables it, the focus goes on to "Issue actions", not to the chip, whether the
// document's request still waits on the reader or not.
test.each([
  "waits on the reader",
  "is answered",
])("the Status select disabled as the issue turns closed while the document's request %s leaves the focus on Issue actions, not on the chip", async (request) => {
  const open =
    request === "is answered" ? answeredApprovalIssue("approved") : awaitingApprovalIssue();
  const restore = stubHeader(open);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const stateActions = await screen.findByTestId("issue-state-actions");
    const status = within(stateActions).getByRole("combobox", { name: "Status" });
    const chip = documentChip(stateActions);
    expect(headerReviewActions(stateActions).length).toBe(request === "is answered" ? 0 : 2);
    act(() => status.focus());
    act(() => {
      queryClient.setQueryData<IssueDetails>(["issue", "CORE-1"], {
        ...open,
        closed_at: "2026-09-12T10:00:00Z",
        status: "done",
      });
    });
    await waitFor(() => expect(status).toBeDisabled());

    const issueActions = screen.getByRole("button", { name: "Issue actions" });
    await waitFor(() => expect(issueActions).toHaveFocus());
    expect(chip).toBeInTheDocument();
    expect(chip).not.toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

/** Opens the title's editor by a click on the heading, which reads `title`, as a reader does, and
 *  answers the heading and the editor, which holds the focus. */
async function openTitleEditor(
  user: UserEvent,
  title = issue.title
): Promise<{ editor: HTMLElement; heading: HTMLElement }> {
  const heading = await screen.findByRole("heading", { level: 1, name: title });
  await user.click(heading);
  const editor = await screen.findByRole("textbox", { name: "Issue title" });
  await waitFor(() => expect(editor).toHaveFocus());
  return { editor, heading };
}

// A press of the primary button that the browser turns into a drag, as when the reader drags the
// words they selected in the editor, sends no mouseup: Chromium sends dragstart, pointercancel and
// dragend, and no mouseup and no click. Nothing waits on such a press, so an edit a key then ends
// draws the title clamped to two lines again at once, not whole until the reader next releases a
// mouse button somewhere.
test("a press a drag takes leaves the title clamped once Enter ends the edit", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const heading = await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(heading.className).toContain("line-clamp-2");
    const { editor } = await openTitleEditor(user);
    // The press goes down on the editor's selected words and becomes a drag: the events Chromium
    // sends for it, in its order, with no mouseup.
    fireEvent.pointerDown(editor, { button: 0, isPrimary: true, pointerType: "mouse" });
    fireEvent.mouseDown(editor, { button: 0 });
    fireEvent.dragStart(editor);
    fireEvent.pointerCancel(editor, { button: 0, isPrimary: true, pointerType: "mouse" });
    fireEvent.dragEnd(editor);
    expect(editor).toHaveFocus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(heading).toHaveFocus());
    expect(heading.className).toContain("line-clamp-2");
  } finally {
    view.unmount();
    restore();
  }
});

// The same drag from a press that ends the edit: the reader presses a link outside the title, whose
// mousedown takes the focus and so ends the edit while the button is down, and drags the link. The
// blur holds the title whole until the press is released, but a drag sends no mouseup, so the
// press is over at its pointercancel, as the drag starts, and the title is clamped to two lines
// again then.
test("a press that ends the edit and turns into a drag leaves the title clamped once the drag ends", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();
  const link = document.createElement("a");
  link.href = "/elsewhere";
  link.textContent = "Elsewhere";
  document.body.append(link);

  try {
    const { heading } = await openTitleEditor(user);
    // The press goes down on the link, which takes the focus inside its mousedown, and becomes a
    // drag: the events Chromium sends for it, in its order, with no mouseup.
    fireEvent.pointerDown(link, { button: 0, isPrimary: true, pointerType: "mouse" });
    fireEvent.mouseDown(link, { button: 0 });
    act(() => link.focus());
    await waitFor(() =>
      expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument()
    );
    // The press is down, so the title is held whole for now.
    expect(heading.className).not.toContain("line-clamp-2");
    fireEvent.dragStart(link);
    fireEvent.pointerCancel(link, { button: 0, isPrimary: true, pointerType: "mouse" });
    fireEvent.dragEnd(link);
    await waitFor(() => expect(heading.className).toContain("line-clamp-2"));
  } finally {
    link.remove();
    view.unmount();
    restore();
  }
});

// A right press as Chromium sends it where the context menu opens as the button goes down: its
// pointerdown, its mousedown and its contextmenu, and no release, which the menu takes. The press
// is over at its contextmenu, so an edit a key then ends draws the title clamped to two lines again
// at once, not whole until the reader next presses and releases somewhere.
test("a right press whose context menu takes its release leaves the title clamped once Enter ends the edit", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const heading = await screen.findByRole("heading", { level: 1, name: issue.title });
    expect(heading.className).toContain("line-clamp-2");
    const { editor } = await openTitleEditor(user);
    // The press goes down with the focus still in the editor. user-event sends no contextmenu
    // without a pointerup, and moves the focus as its pointer goes down.
    fireEvent.pointerDown(document.body, { button: 2, isPrimary: true, pointerType: "mouse" });
    fireEvent.mouseDown(document.body, { button: 2 });
    fireEvent.contextMenu(document.body, { button: 2 });
    expect(editor).toHaveFocus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(heading).toHaveFocus());
    expect(heading.className).toContain("line-clamp-2");
  } finally {
    view.unmount();
    restore();
  }
});

// A tap on a control outside the title: its pointer goes down and comes up with the focus still in
// the editor, and only the mousedown the browser sends after that moves the focus and so ends the
// edit. The tap's press is under way until its click, so that blur holds the title whole, and the
// title is clamped to two lines again once the click has been dispatched.
test("a tap that ends the edit holds the title whole until its click", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();
  const outside = document.createElement("button");
  outside.type = "button";
  outside.textContent = "Elsewhere";
  document.body.append(outside);

  try {
    const { heading } = await openTitleEditor(user);
    // A tap's order, its pointer up before the mousedown that takes the focus: user-event moves
    // the focus as its pointer goes down.
    fireEvent.pointerDown(outside, { button: 0, isPrimary: true, pointerType: "touch" });
    fireEvent.pointerUp(outside, { button: 0, isPrimary: true, pointerType: "touch" });
    fireEvent.mouseDown(outside, { button: 0 });
    act(() => outside.focus());
    await waitFor(() =>
      expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument()
    );
    // The tap is under way, so the title is held whole for now.
    expect(heading.className).not.toContain("line-clamp-2");
    // The tap's mouseup and click, which the browser sends after its mousedown.
    fireEvent.mouseUp(outside, { button: 0 });
    fireEvent.click(outside);
    await waitFor(() => expect(heading.className).toContain("line-clamp-2"));
  } finally {
    outside.remove();
    view.unmount();
    restore();
  }
});

/** Opens the title's editor (`openTitleEditor`) and writes `draft` in it in place of the title,
 *  with the editor keeping the focus. An empty draft is the title cleared: user-event types no
 *  empty text. */
async function editTitle(user: UserEvent, draft: string) {
  const { editor, heading } = await openTitleEditor(user);
  await user.clear(editor);
  if (draft !== "") await user.keyboard(draft);
  expect(editor).toHaveValue(draft);
  return { editor, heading };
}

/** Takes the page's focus away as a switch to another window does: `focused` loses the focus
 *  with no element to go to and stays the document's active element, and the document has no
 *  focus until the reader comes back by the function this answers. A write the blur sends goes on
 *  in the case's next wait (`waitFor` or user-event), which keeps its updates inside React's act
 *  scope, so a case waits through nothing else before it. */
function leaveWindow(focused: HTMLElement): () => void {
  const hasFocus = spyOn(document, "hasFocus").mockReturnValue(false);
  // What a browser sends as its window loses the focus, with no relatedTarget and the focus left
  // where it was: user-event has no form for a window's blur. testing-library's `fireEvent.blur`
  // also sends the focusout that React's `onBlur` hears.
  fireEvent.blur(focused, { relatedTarget: null });
  fireEvent.blur(window);
  return () => hasFocus.mockRestore();
}

/** Brings the reader back to the window a switch left (`leaveWindow`), once the function that
 *  answers has given the document its focus again. */
function returnToWindow(): void {
  // The window's focus as the reader comes back: user-event has no form for it.
  fireEvent.focus(window);
}

/** Takes the page's focus away as a switch to another window does (`leaveWindow`), and runs
 *  `whileAway` before the reader comes back. */
function blurWindow(focused: HTMLElement, whileAway?: () => void): void {
  const comeBack = leaveWindow(focused);
  try {
    whileAway?.();
  } finally {
    comeBack();
  }
}

/** Waits until the write of `title` has landed: the issue's read holds Dispatch's answer. */
async function titleLanded(queryClient: QueryClient, title: string): Promise<void> {
  await waitFor(() =>
    expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.title).toBe(title)
  );
}

/** Ends the title's edit by a press elsewhere on the page, and waits for the editor to be gone. */
async function endTitleEditByPress(user: UserEvent): Promise<void> {
  await user.click(document.body);
  await waitFor(() =>
    expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument()
  );
}

// A switch to another window or tab blurs the editor with no element for the focus to go to, and
// the editor stays the page's focused element, which the browser focuses again as the reader comes
// back. The reader has not left the editor, so it stays open with their draft, and the draft is
// sent all the same, as today's page sends it as the window loses the focus (decision 295). The
// title a switch sends is the draft trimmed, as every title is, and the draft takes that trimmed
// title as it goes; its write lands with the edit still open, and through both the editor keeps
// the reader's place - their text, with the space they typed before their next word, and their
// caret. The reader's return sends nothing more, and neither does the press that later ends the
// edit: the draft went once.
test("a switch to another window sends the title's changed draft once and keeps the reader's text and caret as its write lands", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, title: "Review the plan" });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor } = await editTitle(user, "Review the plan ");
    const place = editor as HTMLTextAreaElement;
    // The reader's caret, put back after "Review".
    place.setSelectionRange(6, 6);
    blurWindow(editor);
    expect(editor).toHaveValue("Review the plan ");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    expect(editor).toBeInTheDocument();
    expect(editor).toHaveFocus();
    await titleLanded(queryClient, "Review the plan");

    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    expect(editor).toHaveValue("Review the plan ");
    expect([place.selectionStart, place.selectionEnd]).toEqual([6, 6]);
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await endTitleEditByPress(user);
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The issue can be closed elsewhere before a switch's write reaches Dispatch, which then refuses
// the title, and the page may not have drawn the close yet. Dispatch refuses that title every
// time, and the draft went once, so neither another switch, nor the reader's return, nor the press
// that later ends the edit sends it again, though it is still not the issue's title: only a change
// of it sends again.
test("a title a switch sent and Dispatch refused is not sent again by another switch, the reader's return or the end of the edit", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const refusal = "CORE-1 is closed; reopen it first";
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(409, { code: "ISSUE_CLOSED", error: refusal })
  );
  const view = renderHeader();

  try {
    const { editor } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await screen.findByText(refusal);
    expect(patchIssue).toHaveBeenCalledTimes(1);
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);

    returnToWindow();
    blurWindow(editor);
    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await endTitleEditByPress(user);
    expect(screen.getByText(refusal)).toBeInTheDocument();
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A draft goes once only until the reader changes it. Once a switch's write of the draft has
// landed, another reader's write can change the title, and the draft, not changed, takes that
// title. A reader who then writes back the title the switch sent wants it to be the issue's title
// again, so the end of the edit sends it.
test("a title a switch sent and the reader writes back after another write changed it is sent again as the edit ends", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, title: "Review the plan" });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await titleLanded(queryClient, "Review the plan");
    expect(patchIssue).toHaveBeenCalledTimes(1);
    returnToWindow();
    titleWrittenElsewhere(queryClient, "Plan the review");
    await waitFor(() => expect(editor).toHaveValue("Plan the review"));

    await user.clear(editor);
    await user.keyboard("Review the plan");
    await endTitleEditByPress(user);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Review the plan" },
      keyedAttempt
    );
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The reader opens the title's editor, types a character and takes it back out, and another
// reader then renames the issue. The keystroke that left the draft on the issue's title handed it
// back to the issue (decision 332), so the editor follows the rename, and the end of the edit
// sends nothing. Dispatch's hook before this change does the same on today's page: no write. Kept
// as text the reader typed, the draft held the issue's old title in the editor and sent it over
// the rename.
test("a character typed and taken back out leaves the title's editor following another reader's title, and the end of the edit sends nothing", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue(issue);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor, heading } = await openTitleEditor(user);
    await user.keyboard("x{Backspace}");
    expect(editor).toHaveValue(issue.title);
    titleWrittenElsewhere(queryClient, "Plan the review");
    const shown = (editor as HTMLTextAreaElement).value;

    await endTitleEditByPress(user);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ shown, heading: heading.textContent, writes: patchIssue.mock.calls }).toEqual({
      shown: "Plan the review",
      heading: "Plan the review",
      writes: [],
    });
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A switch to another window sends the reader's title and leaves the edit open (decision 295).
// While that write is out the reader comes back, types a character and takes it back out, which
// leaves the draft on the title out. The write's settle then hands the draft back to the issue
// because the write landed with it (decision 332), whatever the keystroke made it, so the editor
// follows the title another reader sets next, and the end of the edit sends nothing: the title
// went once. An earlier form of this change's hook handed an ended choice back on another
// reader's title and kept a typed draft, as the keystroke made this one, so it held the title the
// switch sent over the other reader's, and the end of the edit sent it a second time.
test("a character typed and taken back out while a switch's write of the title is out leaves the editor following another reader's title once that write lands, and the end of the edit sends nothing", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches().mockImplementation(() => landing.promise);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor, heading } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    await user.keyboard("x{Backspace}");
    expect(editor).toHaveValue("Review the plan");

    await act(async () => {
      landing.resolve({ ...issue, title: "Review the plan" });
      await landing.promise;
    });
    await titleLanded(queryClient, "Review the plan");
    titleWrittenElsewhere(queryClient, "Plan the review");
    const shown = (editor as HTMLTextAreaElement).value;

    await endTitleEditByPress(user);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ shown, heading: heading.textContent, writes: patchIssue.mock.calls }).toEqual({
      shown: "Plan the review",
      heading: "Plan the review",
      writes: [["CORE-1", { title: "Review the plan" }, keyedAttempt]],
    });
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A switch to another window sends the reader's title, and Dispatch refuses it, so the title stays
// in the open editor, the reader's unsent choice. Another reader then sets the issue to that very
// title, and then to a title of their own. Of the answers (a landing, a refusal, another reader's
// title), only the settle of the title's own write hands a typed or ended editor back to the
// issue, never another reader's title (decision 332); the reader's own keystroke back to the
// issue's title, an end that changes nothing or an Escape do too. No title write has settled
// since the refusal and the reader makes none of those, so the editor keeps the reader's title
// through both. Handed back as the other reader's title matched it, the editor took their next
// title, which the reader never typed.
test("a title a switch sent and Dispatch refused stays in the open editor as another reader sets the issue to it and then to a title of their own", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
  );
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await screen.findByText("Could not update this issue.");
    expect(patchIssue).toHaveBeenCalledTimes(1);
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());

    titleWrittenElsewhere(queryClient, "Review the plan");
    const matched = (editor as HTMLTextAreaElement).value;
    titleWrittenElsewhere(queryClient, "Plan the review");
    expect({ matched, shown: (editor as HTMLTextAreaElement).value }).toEqual({
      matched: "Review the plan",
      shown: "Review the plan",
    });
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A switch while a write of the title is out queues the draft behind that write: one write of the
// issue's fields goes at a time, so the draft has not gone. Dispatch then refuses the write out,
// and nothing drains the draft, which is still the reader's to send, so the end of the edit sends
// it. Taken for a draft that went, it was never sent, and the heading read a title Dispatch did
// not have.
test("a draft a switch queued behind a title's write that Dispatch then refuses is sent as the edit ends", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => landing.promise)
    .mockImplementationOnce(async () => ({ ...issue, title: "Plan the review" }));
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    await waitFor(() => expect(heading).toHaveFocus());

    const { editor } = await openTitleEditor(user, "Review the plan");
    await user.clear(editor);
    await user.keyboard("Plan the review");
    blurWindow(editor);
    expect(editor).toHaveValue("Plan the review");
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      landing.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" }));
      await landing.promise.catch(() => undefined);
    });
    await screen.findByText("Could not update this issue.");
    expect(patchIssue).toHaveBeenCalledTimes(1);

    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    await endTitleEditByPress(user);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Plan the review" },
      keyedAttempt
    );
    await titleLanded(queryClient, "Plan the review");
    expect(heading).toHaveTextContent(/^Plan the review$/);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A draft a switch queued goes as the write ahead of it lands, and Dispatch then refuses it. It
// went once, as a draft a switch sent goes once, so the end of the edit sends it no second time,
// though it is not the issue's title.
test("a draft a switch queued that went as the write ahead of it landed, and that Dispatch refused, is not sent again as the edit ends", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const first = Promise.withResolvers<IssueDetails>();
  const second = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(heading).toHaveFocus());

    const { editor } = await openTitleEditor(user, "Review the plan");
    await user.clear(editor);
    await user.keyboard("Plan the review");
    blurWindow(editor);
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.resolve({ ...issue, title: "Review the plan" });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Plan the review" },
      keyedAttempt
    );
    await act(async () => {
      second.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" }));
      await second.promise.catch(() => undefined);
    });
    await screen.findByText("Could not update this issue.");

    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    await endTitleEditByPress(user);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A draft a switch queued waits behind the write out, so it has not gone. Dispatch then refuses
// that write, and nothing drains a draft after a refusal: it is still the reader's to send, and a
// hide of the page sends it, as a page hidden before the tab is closed must. Taken for a draft
// that went, it was sent by neither a hide nor a second switch, and a closed tab lost it.
test("a draft a switch queued behind a title's write that Dispatch then refuses is sent as the page is hidden", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => landing.promise)
    .mockImplementationOnce(async () => ({ ...issue, title: "Plan the review" }));
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    await waitFor(() => expect(heading).toHaveFocus());

    const { editor } = await openTitleEditor(user, "Review the plan");
    await user.clear(editor);
    await user.keyboard("Plan the review");
    blurWindow(editor);
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      landing.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" }));
      await landing.promise.catch(() => undefined);
    });
    await screen.findByText("Could not update this issue.");
    expect(patchIssue).toHaveBeenCalledTimes(1);

    Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
    try {
      act(() => {
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    } finally {
      Reflect.deleteProperty(document, "visibilityState");
    }
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Plan the review" },
      keyedAttempt
    );
    expect(editor).toHaveValue("Plan the review");
    await titleLanded(queryClient, "Plan the review");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A draft goes once in the edit that sent it. A new edit is the reader's own: one opened on the
// title a switch sent and Dispatch refused, and ended with nothing typed, sends that title again,
// as any edit ended on a title the issue does not hold does.
test("a title a switch sent and Dispatch refused is sent again as a new edit opened on it ends", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches()
    .mockRejectedValueOnce(
      new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
    )
    .mockResolvedValueOnce({ ...issue, title: "Review the plan" });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await screen.findByText("Could not update this issue.");
    expect(patchIssue).toHaveBeenCalledTimes(1);
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    await endTitleEditByPress(user);
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await openTitleEditor(user, "Review the plan");
    await endTitleEditByPress(user);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Review the plan" },
      keyedAttempt
    );
    await titleLanded(queryClient, "Review the plan");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The reader can come back while a switch's write is out and type more, which makes the draft
// theirs to send again, then press Escape, which drops what they typed and puts back the title the
// switch sent, the title the issue is about to hold (decision 316). The write's landing then
// leaves the heading on the title it lands with (decision 304), and no edit sends a title again.
test("an Escape after the reader typed more while a switch's write of the title was out leaves the heading on the title that write lands with, and nothing sends a title again", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches().mockImplementation(() => landing.promise);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor, heading } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    await user.keyboard(" today");
    expect(editor).toHaveValue("Review the plan today");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(heading).toHaveFocus());
    expect(heading).toHaveTextContent(/^Review the plan$/);

    await act(async () => {
      landing.resolve({ ...issue, title: "Review the plan" });
      await landing.promise;
    });
    await titleLanded(queryClient, "Review the plan");
    expect(heading).toHaveTextContent(/^Review the plan$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);

    const { editor: reopened } = await openTitleEditor(user, "Review the plan");
    expect(reopened).toHaveValue("Review the plan");
    await endTitleEditByPress(user);
    expect(heading).toHaveTextContent(/^Review the plan$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// Escape after a switch's write landed and another reader's write then changed the title, which
// the draft, not changed, took: what the reader typed after that is all the Escape drops, so the
// heading reads the issue's title as it now stands, not the one the switch sent, and nothing sends
// a title again.
test("an Escape after a switch's write landed, another write changed the title and the reader typed more leaves the heading on that title", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, title: "Review the plan" });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor, heading } = await editTitle(user, "Review the plan");
    blurWindow(editor);
    await titleLanded(queryClient, "Review the plan");
    returnToWindow();
    titleWrittenElsewhere(queryClient, "Plan the review");
    await waitFor(() => expect(editor).toHaveValue("Plan the review"));
    await user.keyboard(" today");
    expect(editor).toHaveValue("Plan the review today");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(heading).toHaveFocus());
    expect(heading).toHaveTextContent(/^Plan the review$/);

    await openTitleEditor(user, "Plan the review");
    await endTitleEditByPress(user);
    expect(heading).toHaveTextContent(/^Plan the review$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A switch with a draft that is not changed sends nothing and leaves the edit open as the reader
// left it: the title as it stands; the title with a space typed after it, which is the title once
// trimmed, and which the switch leaves as typed; and an empty draft, which says nothing while the
// reader is away, its "Title is required." waiting for the end of the edit.
test.each([
  ["the title as it stands", issue.title],
  ["the title with a space after it", `${issue.title} `],
  ["an empty draft", ""],
])("a switch to another window with %s sends no write and keeps the title's editor open", async (_draftName, draft) => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue(issue);
  const view = renderHeader();

  try {
    const { editor } = await editTitle(user, draft);
    blurWindow(editor);
    expect(editor).toBeInTheDocument();
    expect(editor).toHaveValue(draft);
    expect(editor).toHaveFocus();

    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    expect(editor).toHaveValue(draft);
    expect(screen.queryByText("Title is required.")).not.toBeInTheDocument();
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The reader saves a new title, and while its write is out opens the edit again and types the
// issue's title back, to undo the change. The draft then equals the title the issue still has, so
// it reads as unchanged; but the reader typed it, and the write's landing must not put the title
// they took out back into the editor under them. The end of the edit then sends the title they
// typed back.
test("a title typed back to the issue's title while a write of another is out stays in the editor as that write lands, and is sent as the edit ends", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => landing.promise)
    .mockImplementationOnce(async () => ({ ...issue, title: issue.title }));
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    await waitFor(() => expect(heading).toHaveFocus());

    // The write is out. The reader opens the edit again and types the issue's title back.
    await user.click(heading);
    const reopened = await screen.findByRole("textbox", { name: "Issue title" });
    await waitFor(() => expect(reopened).toHaveFocus());
    await user.clear(reopened);
    await user.keyboard(issue.title);
    expect(reopened).toHaveValue(issue.title);

    await act(async () => {
      landing.resolve({ ...issue, title: "Review the plan" });
      await landing.promise;
    });
    await titleLanded(queryClient, "Review the plan");
    expect(reopened).toHaveValue(issue.title);

    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: issue.title }, keyedAttempt);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The reader saves "a", and while its write is out opens the edit again and types a space after
// it, as a reader does before the next word. The write then lands with "a". The space is the
// reader's text, so the draft as typed is not the title the write landed with, the settle hands
// nothing back, and the open editor keeps "a " through a title another reader sets next (decision
// 332). In what a landing does to text typed while its write was out, this is the one place this
// change means to differ from Dispatch's hook before it, which trims the draft as the write lands:
// the editor here still reads "a " then, as it keeps the reader's text while the draft is that
// text trimmed, but the draft is the issue's, so the other reader's title replaces the reader's
// text in the open editor.
test("a space typed after a saved title while its write is out stays in the open editor as that write lands and another reader sets a title", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches().mockImplementationOnce(() => landing.promise);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "a");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "a" }, keyedAttempt);
    await waitFor(() => expect(heading).toHaveFocus());

    // The write is out. The reader opens the edit again and types a space after the title.
    const { editor } = await openTitleEditor(user, "a");
    await user.keyboard("{End} ");
    expect(editor).toHaveValue("a ");

    await act(async () => {
      landing.resolve({ ...issue, title: "a" });
      await landing.promise;
    });
    await titleLanded(queryClient, "a");
    const landed = (editor as HTMLTextAreaElement).value;
    titleWrittenElsewhere(queryClient, "Plan the review");
    expect({ landed, renamed: (editor as HTMLTextAreaElement).value }).toEqual({
      landed: "a ",
      renamed: "a ",
    });
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The same undo, ended before the write lands: the reader saves a new title, opens the edit again
// while its write is out, types the issue's title back and presses Enter. The issue still has
// that title, but the write out is about to replace it, so the title typed back is a change to
// send. It waits for the write out, goes once that write lands, and the heading ends on it.
test("a title typed back to the issue's title and ended while a write of another is out is sent once that write lands", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => landing.promise)
    .mockImplementationOnce(async () => ({ ...issue, title: issue.title }));
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    await waitFor(() => expect(heading).toHaveFocus());

    // The write is out. The reader opens the edit again, types the issue's title back and ends
    // the edit.
    const { editor: reopened } = await openTitleEditor(user, "Review the plan");
    await user.clear(reopened);
    await user.keyboard(issue.title);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(heading).toHaveFocus());
    // One write of the issue's fields at a time: the title typed back waits for the first.
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      landing.resolve({ ...issue, title: "Review the plan" });
      await landing.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: issue.title }, keyedAttempt);
    await titleLanded(queryClient, issue.title);
    expect(heading).toHaveTextContent(/^Review the spec$/);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The reader saves a new title, opens the edit again while its write is out and ends it with
// nothing typed. The draft is the title of that write, so nothing is sent, and the heading keeps
// it: the issue's old title is not put back while the write is out. Dispatch then refuses the
// write, and the draft is still the reader's title, so Retry sends it.
test("an edit ended with nothing typed while a title's write is out leaves the heading on that title, and Retry sends it once that write is refused", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => landing.promise)
    .mockImplementationOnce(async () => ({ ...issue, title: "Review the plan" }));
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    await waitFor(() => expect(heading).toHaveFocus());

    // The write is out. The reader opens the edit again and ends it with nothing typed.
    const { editor: reopened } = await openTitleEditor(user, "Review the plan");
    expect(reopened).toHaveValue("Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(heading).toHaveFocus());
    expect(heading).toHaveTextContent(/^Review the plan$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      landing.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" }));
      await landing.promise.catch(() => undefined);
    });
    await screen.findByText("Could not update this issue.");
    expect(heading).toHaveTextContent(/^Review the plan$/);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Review the plan" },
      keyedAttempt
    );
    await titleLanded(queryClient, "Review the plan");
    expect(heading).toHaveTextContent(/^Review the plan$/);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The same edit ended with nothing typed, and the write Dispatch then refuses: the issue keeps its
// old title, so the draft is a change again, and the reader's. A status pick's write then lands
// with the old title, and the heading keeps the reader's: marked not changed, the draft would take
// the old title, and Retry would have nothing to send.
test("a title an edit ended with nothing typed while its write was out stays the reader's after that write is refused and a status pick lands, and Retry sends it", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => landing.promise)
    .mockImplementationOnce(async () => ({ ...issue, status: "in_progress" }))
    .mockImplementationOnce(async () => ({
      ...issue,
      status: "in_progress",
      title: "Review the plan",
    }));
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(heading).toHaveFocus());
    await openTitleEditor(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(heading).toHaveFocus());

    await act(async () => {
      landing.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" }));
      await landing.promise.catch(() => undefined);
    });
    await screen.findByText("Could not update this issue.");
    expect(heading).toHaveTextContent(/^Review the plan$/);

    await user.selectOptions(screen.getByLabelText("Status"), "in_progress");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "in_progress" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.status).toBe(
        "in_progress"
      )
    );
    expect(heading).toHaveTextContent(/^Review the plan$/);

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(3));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Review the plan" },
      keyedAttempt
    );
    await titleLanded(queryClient, "Review the plan");
    expect(heading).toHaveTextContent(/^Review the plan$/);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// Another reader can change the title while a write of another field is out, and that write
// lands with the issue as Dispatch then holds it. A title draft the reader never touched takes
// the title it lands with: left on the old title it would read as changed, and the end of the
// next edit would send the old title over the other reader's.
test("a title draft the reader never touched takes the title a status pick's write lands with, and an edit ended with nothing typed sends no title", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    status: "in_progress",
    title: "Plan the review",
  });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const heading = await screen.findByRole("heading", { level: 1, name: issue.title });
    await user.selectOptions(await screen.findByLabelText("Status"), "in_progress");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "in_progress" }, keyedAttempt)
    );
    await titleLanded(queryClient, "Plan the review");
    expect(heading).toHaveTextContent(/^Plan the review$/);

    const { editor } = await openTitleEditor(user, "Plan the review");
    expect(editor).toHaveValue("Plan the review");
    await endTitleEditByPress(user);
    expect(heading).toHaveTextContent(/^Plan the review$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A status pick and a title saved after it go in the issue's line one after the other, so the
// title's write is out, waiting, as the pick lands, and the pick's answer still carries the old
// title. The reader has the edit open again on the title they saved, and has typed a character
// and taken it back out: the pick's landing leaves the editor on the title of the write out
// rather than putting the old title back under them, and nothing sends a title again.
test("a status pick that lands while a title saved after it waits behind it leaves the reopened editor on that title, and nothing sends a title again", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const pick = Promise.withResolvers<IssueDetails>();
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => landing.promise);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    await user.selectOptions(await screen.findByLabelText("Status"), "in_progress");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "in_progress" }, keyedAttempt)
    );
    const { heading } = await editTitle(user, "Review the plan");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(heading).toHaveFocus());
    // The title's write waits behind the pick in the issue's line.
    expect(patchIssue).toHaveBeenCalledTimes(1);

    const { editor: reopened } = await openTitleEditor(user, "Review the plan");
    await user.keyboard("s{Backspace}");
    expect(reopened).toHaveValue("Review the plan");

    await act(async () => {
      pick.resolve({ ...issue, status: "in_progress" });
      await pick.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith(
      "CORE-1",
      { title: "Review the plan" },
      keyedAttempt
    );
    expect(reopened).toHaveValue("Review the plan");
    await endTitleEditByPress(user);
    expect(heading).toHaveTextContent(/^Review the plan$/);

    await act(async () => {
      landing.resolve({ ...issue, status: "in_progress", title: "Review the plan" });
      await landing.promise;
    });
    await titleLanded(queryClient, "Review the plan");
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(heading).toHaveTextContent(/^Review the plan$/);
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// A phone that hides the page may send the editor no blur, so a page hidden with the edit open
// sends a changed draft as a switch does (decision 295), and keeps the edit open. Dispatch refuses
// that write here, so the draft is still not the issue's title, and only its having gone once
// keeps the blur of the window that can follow, and the press that later ends the edit, from
// sending it again: a write that landed would have made it the issue's title, which sends nothing.
test("a page hidden with the title's editor open sends its changed draft once, and neither a window blur after it nor the end of the edit sends it again", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const refusal = "CORE-1 is closed; reopen it first";
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(409, { code: "ISSUE_CLOSED", error: refusal })
  );
  const view = renderHeader();

  try {
    const { editor } = await editTitle(user, "Review the plan");
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
    try {
      act(() => {
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await screen.findByText(refusal);
      expect(patchIssue).toHaveBeenCalledTimes(1);
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
      expect(editor).toBeInTheDocument();
      expect(editor).toHaveValue("Review the plan");
      expect(editor).toHaveFocus();

      blurWindow(editor);
      expect(editor).toHaveValue("Review the plan");
    } finally {
      Reflect.deleteProperty(document, "visibilityState");
    }
    returnToWindow();
    await waitFor(() => expect(editor).toHaveFocus());
    await endTitleEditByPress(user);
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// While the reader is away in another window the page's own code can move the focus, with no blur
// reaching the editor: Chromium sends none while the page has no focus. The page can draw again
// meanwhile, as an event's refresh draws it. Back in the window the editor no longer holds the
// focus, so the edit ends as its blur would have ended it, and the draft, which the switch sent,
// is not sent again.
test("a focus moved while the window is away ends the title's edit as the reader comes back and sends the draft once", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, title: "Review the plan" });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor } = await editTitle(user, "Review the plan");
    const control = screen.getByRole("button", { name: "Issue actions" });
    blurWindow(editor, () => {
      act(() => control.focus());
      // An event's refresh of the issue, which draws the header again.
      act(() => {
        queryClient.setQueryData<IssueDetails>(
          ["issue", "CORE-1"],
          (current) => current && { ...current, updated_at: "2026-09-12T10:00:00Z" }
        );
      });
    });
    expect(control).toHaveFocus();
    expect(editor).toBeInTheDocument();
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);

    returnToWindow();
    await waitFor(() =>
      expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument()
    );
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// An event's refresh can close the issue while the reader is away in another window, which
// disables the editor they left open. A closed issue takes no title, so the edit ends and drops
// its draft, and the heading reads the issue's title: no disabled editor is left holding a draft
// that nothing can end. The switch sent the draft before the close, and its write went, so the
// issue's title is the reader's: what the close drops is only a change made after that send, here
// the words the reader typed once back in the window, before the close reached the page.
test.each([
  ["while the window is away", false],
  ["after the reader came back and typed more", true],
] as const)("an issue closed %s ends the title's edit and keeps only the draft the switch sent", async (_when, typedMore) => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, title: "Review the plan" });
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);
  // The issue as Dispatch has it once the switch's write went and the issue was closed elsewhere.
  const closeIssue = () =>
    act(() => {
      queryClient.setQueryData<IssueDetails>(["issue", "CORE-1"], {
        ...issue,
        closed_at: "2026-09-12T10:00:00Z",
        status: "done",
        title: "Review the plan",
      });
    });

  try {
    const { editor, heading } = await editTitle(user, "Review the plan");
    const comeBack = leaveWindow(editor);
    try {
      await titleLanded(queryClient, "Review the plan");
      if (!typedMore) closeIssue();
    } finally {
      comeBack();
    }
    if (typedMore) {
      returnToWindow();
      await user.keyboard(" today");
      expect(editor).toHaveValue("Review the plan today");
      closeIssue();
    }
    await waitFor(() =>
      expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument()
    );
    expect(heading).toHaveTextContent(/^Review the plan$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The issue can be closed elsewhere while a switch's write of the title is out, and Dispatch then
// refuses that title. The close ends the edit, and while the write is out the heading reads its
// title, the title the issue is about to hold (decision 316); once the write is refused the issue
// never got it, and nothing will send it to a closed issue, so the heading reads the issue's title.
test("a title a switch sent that Dispatch refuses because the issue was closed meanwhile leaves the heading on the issue's title", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const refusal = "CORE-1 is closed; reopen it first";
  const landing = Promise.withResolvers<IssueDetails>();
  const patchIssue = answerIssuePatches().mockImplementation(() => landing.promise);
  const queryClient = headerClient();
  const view = renderHeader("/issues/CORE-1", queryClient);

  try {
    const { editor, heading } = await editTitle(user, "Review the plan");
    blurWindow(editor, () => {
      // The issue as Dispatch has it once it was closed elsewhere, before the switch's write.
      act(() => {
        queryClient.setQueryData<IssueDetails>(["issue", "CORE-1"], {
          ...issue,
          closed_at: "2026-09-12T10:00:00Z",
          status: "done",
        });
      });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    await waitFor(() =>
      expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument()
    );
    expect(heading).toHaveTextContent(/^Review the plan$/);

    await act(async () => {
      landing.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: refusal }));
      await landing.promise.catch(() => undefined);
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(heading).toHaveTextContent(/^Review the spec$/);
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The Escape that cancels an IME composition and the Enter that confirms one are the input
// method's keys: the editor stays open with the reader's draft and sends nothing, where an Enter
// would write the title mid-word. Chromium and Firefox mark such a key `isComposing`; Safari sends
// it after the composition's end, unmarked, with keyCode 229. A plain key after it still ends the
// edit and gives the focus back to the heading: Escape restores the title and sends nothing, and
// Enter writes the title, once.
test.each([
  ["Escape", "isComposing"],
  ["Enter", "isComposing"],
  ["Escape", "keyCode 229"],
  ["Enter", "keyCode 229"],
] as const)("an %s that ends an IME composition, marked by %s, leaves the title's editor open with its draft and sends no write", async (key, mark) => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, title: "Review the plan" });
  const view = renderHeader();

  try {
    const { editor, heading } = await editTitle(user, "Review the plan");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(
      editor,
      mark === "isComposing"
        ? { isComposing: true, key }
        : { isComposing: false, key, keyCode: 229 }
    );
    expect(editor).toBeInTheDocument();
    expect(editor).toHaveValue("Review the plan");
    expect(editor).toHaveFocus();
    expect(patchIssue).not.toHaveBeenCalled();

    await user.keyboard(`{${key}}`);
    await waitFor(() => expect(heading).toHaveFocus());
    expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument();
    if (key === "Escape") {
      expect(heading).toHaveTextContent(issue.title);
      expect(patchIssue).not.toHaveBeenCalled();
    } else {
      await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { title: "Review the plan" }, keyedAttempt);
    }
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The open actions group hangs over the header's first rows, and at 390 px it covers the title
// and the properties line whole. A reader who Tabs on past its last action puts the focus on a
// control under it, which they then cannot see (WCAG 2.4.11), so the group closes as the focus
// leaves it by Tab, as it does at a press outside it and at Escape: the ARIA menu-button pattern
// closes the menu at a Tab too.
test("the actions group closes once Tab takes the focus out of it", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const control = await screen.findByRole("button", { name: "Issue actions" });
    act(() => control.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(control).toHaveAttribute("aria-expanded", "true"));
    const group = document.getElementById(control.getAttribute("aria-controls") ?? "");
    if (group === null) throw new Error("the actions control names no group");
    const actions = within(group).getAllByRole("button");
    expect(actions.length).toBeGreaterThan(0);
    for (const action of actions) {
      await user.tab();
      expect(action).toHaveFocus();
      expect(control).toHaveAttribute("aria-expanded", "true");
    }
    // One Tab more takes the focus out of the group, to the control after it in the header.
    await user.tab();
    expect(group.contains(document.activeElement)).toBe(false);
    await waitFor(() => expect(control).toHaveAttribute("aria-expanded", "false"));
  } finally {
    view.unmount();
    restore();
  }
});

/** Opens the header's actions group from the keyboard, as a reader does: the focus on its control,
 *  then Enter, then one Tab onto the group's first action. Answers that first action, the control
 *  and the group it names. */
async function openIssueActionsByKey(user: UserEvent): Promise<{
  control: HTMLElement;
  first: HTMLElement;
  group: HTMLElement;
}> {
  const control = await screen.findByRole("button", { name: "Issue actions" });
  act(() => control.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(control).toHaveAttribute("aria-expanded", "true"));
  const group = document.getElementById(control.getAttribute("aria-controls") ?? "");
  if (group === null) throw new Error("the actions control names no group");
  const [first] = within(group).getAllByRole("button");
  if (first === undefined) throw new Error("the actions group holds no action");
  await user.tab();
  expect(first).toHaveFocus();
  return { control, first, group };
}

// Shift+Tab from the group's first action takes the focus back to the group's control, which
// is inside the box the group closes on leaving: the reader is still in the menu, so it stays
// open.
test("the actions group stays open as Shift+Tab takes the focus from its first action back to its control", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const { control } = await openIssueActionsByKey(user);
    await user.tab({ shift: true });
    expect(control).toHaveFocus();
    expect(control).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
    restore();
  }
});

// The window losing the focus - the reader switching to another window - blurs the focused action
// with no element for the focus to go to, and the action stays the document's active element.
// The reader has not moved on in the page, so the group stays open for their return.
test("the actions group stays open as the window loses the focus from one of its actions", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const { control, first: action } = await openIssueActionsByKey(user);
    blurWindow(action);
    expect(action).toHaveFocus();
    expect(control).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
    restore();
  }
});

// A focus the page moves by code to a control outside the group leaves it as a Tab does, so the
// group closes there too and leaves that control uncovered.
test("the actions group closes once a focus moved by code goes to a control outside it", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issue);
  const view = renderHeader();

  try {
    const { control, group } = await openIssueActionsByKey(user);
    const status = within(screen.getByTestId("issue-state-actions")).getByRole("combobox", {
      name: "Status",
    });
    act(() => status.focus());
    expect(status).toHaveFocus();
    expect(group.contains(document.activeElement)).toBe(false);
    await waitFor(() => expect(control).toHaveAttribute("aria-expanded", "false"));
  } finally {
    view.unmount();
    restore();
  }
});

// The rule's question on the new page's header (INFRA-4731): the header draws what the dry run of a
// status write answers, so a write the rule would refuse never leaves the status pending with
// nothing on the screen and every later pick refused as one too many.

/** The issue under a parent, so the rule judges each write of its status. */
const issueUnderAParent: IssueDetails = { ...issue, parent: "CORE-9" };

test("a status pick the rule would refuse asks the question, and Keep as it is puts the status back for the next pick", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issueUnderAParent);
  const statusChanges = answerStatusChanges().mockImplementation(async (_key, input) => {
    // Each choice's own dry run is held: the question is about to be closed unanswered.
    if (input.include_children === true || input.unlink_children === true) {
      return new Promise<StatusChangeAnswer>(() => {});
    }
    if (input.status === "in_progress") throw fixtureRefusal("conflict");
    return fixtureAnswer("dry-run");
  });
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issueUnderAParent,
    status: "backlog",
  });
  const view = renderHeader();

  try {
    const status = await screen.findByRole("combobox", { name: "Status" });
    await user.selectOptions(status, "in_progress");
    const question = await screen.findByRole("dialog", { name: "Move CORE-1 to In progress?" });
    expect(status).toHaveValue("in_progress");
    expect(patchIssue).not.toHaveBeenCalled();

    await user.click(within(question).getByRole("button", { name: "Keep as it is" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(status).toHaveValue("todo");
    expect(patchIssue).not.toHaveBeenCalled();

    // The next pick is taken: its dry run asks nothing, and its PATCH goes.
    await user.selectOptions(status, "backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );
    expect(statusChanges).toHaveBeenCalledWith(
      "CORE-1",
      { dry_run: true, status: "backlog" },
      expect.anything()
    );
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

test("a status pick whose dry run is refused for another reason says why, and the status goes back", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issueUnderAParent);
  const refusal = fixtureRefusal("stale");
  answerStatusChanges().mockRejectedValue(refusal);
  const patchIssue = answerIssuePatches();
  const view = renderHeader();

  try {
    const status = await screen.findByRole("combobox", { name: "Status" });
    await user.selectOptions(status, "in_progress");
    expect(
      await screen.findByText(`Couldn't change the status of CORE-1 — ${refusal.message}`)
    ).toHaveAttribute("role", "alert");
    expect(status).toHaveValue("todo");
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});

// The page takes a pick while an earlier one is out (its select is never disabled), and writes
// each in the order they came. A pick of an issue under a parent sends the rule's dry run first:
// a second pick made while that dry run is out shows at once and goes once the first has made its
// write, as a pick made while a write is out waits behind it in the issue's line.
test("a second status pick while the first one's dry run is out shows at once, and both are written in order", async () => {
  const user = userEvent.setup();
  const restore = stubHeader(issueUnderAParent);
  const firstCheck = Promise.withResolvers<StatusChangeAnswer>();
  const statusChanges = answerStatusChanges().mockImplementation(async (_key, input) =>
    input.status === "testing" ? firstCheck.promise : fixtureAnswer("dry-run")
  );
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) => ({
    ...issueUnderAParent,
    status: input.status ?? issueUnderAParent.status,
  }));
  const view = renderHeader();

  try {
    const status = await screen.findByRole("combobox", { name: "Status" });
    await user.selectOptions(status, "testing");
    expect(status).toHaveValue("testing");
    await user.selectOptions(status, "needs_review");
    expect(status).toHaveValue("needs_review");
    expect(statusChanges).toHaveBeenCalledTimes(1);
    expect(patchIssue).not.toHaveBeenCalled();

    await act(async () => {
      firstCheck.resolve(fixtureAnswer("dry-run"));
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue.mock.calls.map(([, input]) => input)).toEqual([
      { status: "testing" },
      { status: "needs_review" },
    ]);
    expect(statusChanges.mock.calls.map(([, input]) => input.status)).toEqual([
      "testing",
      "needs_review",
    ]);
    await waitFor(() => expect(status).toHaveValue("needs_review"));
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    restore();
  }
});
