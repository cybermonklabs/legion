import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction, useLocation, useNavigate } from "react-router-dom";

import { ApiError, api } from "../../../api/client";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type { ArtifactBlock, IssueAskRow, IssueDetails } from "../../../api/types";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { answeredAsk } from "../../inbox/ask-card-fixture";
import { MarginProvider } from "../../margin/margin-context";
import { buildDispatchReference, parseIssuePath } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { presetPath } from "../issue-address";
import { TWO_THREADS, waitInAct } from "../outline-fixtures";
import { EMPTY_STATE_LINES, type Preset } from "../page-words";
import { ProvidersSlot } from "../slots/providers";
import { StageSlot } from "../slots/stage";
import { blockNodeId } from "../tree-drafts";
import { issueAsksQuery } from "../useIssueTree";
import { check, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import {
  artifact,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  KEY,
  READER,
  specRef,
} from "./decisions-fixtures";

/** Every read the page's tree is built from, answered with nothing, and who is reading. */
function stubReads(): { mockRestore(): void }[] {
  return [
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: KEY,
      requirements: [],
      spec_artifact_id: "",
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: KEY,
      verification_artifact_id: "",
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
  ];
}

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The page's stage at the issue's address of `preset`, Decisions where none is given, with
 *  `after` after it, a search or a fragment, under the page's own providers, as the page's root
 *  draws it (`IssueOutlinePage.tsx`): the stage's own views, each slot filled as `SLOT_FILLED`
 *  has it. The issue is `issueDetails()` and the client a fresh one where the case gives none,
 *  and `held.navigate` moves the router once the stage is drawn (`AddressedStage`). */
function drawStage(
  after: string,
  {
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
    held = {},
    issue = issueDetails(),
    preset = "decisions",
  }: {
    client?: QueryClient;
    held?: { navigate?: NavigateFunction };
    issue?: IssueDetails;
    preset?: Preset;
  } = {}
): RenderResult {
  return render(
    <MemoryRouter initialEntries={[`${presetPath(KEY, preset)}${after}`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <AddressedStage held={held} issue={issue} />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The page's stage at the address the router holds, under the page's own providers, as the
 *  page's root draws it (`IssueOutlinePage.tsx`): the route read from the address at every
 *  render. `held.navigate` moves the router, as a press of a view's control does. */
function AddressedStage({
  held,
  issue,
}: {
  held: { navigate?: NavigateFunction };
  issue: IssueDetails;
}): ReactNode {
  const { pathname, search } = useLocation();
  held.navigate = useNavigate();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <StageSlot issue={issue} route={route} />
    </ProvidersSlot>
  );
}

/** The line the body draws once the reads that say which entries there are have answered. */
async function emptyLineOf(view: RenderResult): Promise<string | null | undefined> {
  await waitFor(() =>
    expect(view.container.querySelector("[data-decisions-body] p")).not.toBeNull()
  );
  return view.container.querySelector("[data-decisions-body] p")?.textContent;
}

/** The node id of the row that holds the focus, or undefined where no row does. */
function focusedRow(): string | undefined {
  return document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
}

/** Which slot the stage drew, by each slot's own mark: a view's body, the Decisions view's or the
 *  Verification view's, or the body that draws the outline or a line in its place. */
function drawnSlot(view: RenderResult): string {
  const { container } = view;
  if (container.querySelector("[data-issue-body]") !== null) return "the body";
  const drawn = container.querySelector("[data-decisions-body], [data-verification-body]");
  return drawn === null ? "nothing" : "the view";
}

// The line the Decisions view's slot hands its body for an issue that holds no question and no
// decision it draws: the empty issue's own line where no words are typed, and the line that says
// nothing matched where they are. Words of spaces alone filter nothing (the view trims them), so
// they draw the empty issue's line too.
test("no words draw the empty issue's line, and words that match nothing the no-match line", async () => {
  expect(await emptyLineOf(drawStage(""))).toBe(EMPTY_STATE_LINES.decisions);
  cleanup();
  expect(await emptyLineOf(drawStage("?q=zzz"))).toBe(EMPTY_STATE_LINES.no_match);
});

test("words of spaces alone draw the empty issue's line, as no words do", async () => {
  expect(await emptyLineOf(drawStage("?q=%20%20"))).toBe(EMPTY_STATE_LINES.decisions);
});

/** An id that names no comment of the issue, which the comment's own read answers 404 for. */
const GONE_COMMENT = "5f0c2a9e-7d41-4b8a-9c3e-2e6d1b7a4c90";

// An address beside the Decisions view names a node of the outline. Where the view's tree holds
// none, the view lands on nothing, and the stage draws the outline's fallback in its place, as it
// did before the view was filled: for an id no comment holds, the comment's own read finds none,
// and the page says so where the outline would be, with the focus on that line.
test("a comment the issue holds none of, named beside the Decisions view, draws the outline's line that says so, with the focus on it", async () => {
  const read = spyOn(api, "getComment").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no such comment" })
  );
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`);
    const heading = await waitFor(() => view.getByRole("heading", { name: "Comment not found" }), {
      onTimeout: () =>
        new Error(
          `the address of a comment the issue holds none of drew no line saying so; the stage drew ${drawnSlot(view)}, with the focus on ${document.activeElement?.tagName}`
        ),
    });
    expect(read).toHaveBeenCalledWith(GONE_COMMENT);
    await waitFor(() =>
      expect(document.activeElement, "the line that names no comment does not hold the focus").toBe(
        heading
      )
    );
    expect(drawnSlot(view), "the view was drawn beside the line").toBe("the body");
  } finally {
    read.mockRestore();
  }
});

// A block the tree holds no node of, named by a `#b-` fragment on the Decisions view's address:
// the stage draws the outline's line that the outline has no row for it, with the focus on it.
test("a block no node carries, named on the Decisions view's address, draws the outline's line that says so, with the focus on it", async () => {
  const view = drawStage("#b-no-such-block");
  const heading = await waitFor(
    () => view.getByRole("heading", { name: "No row for this block" }),
    {
      onTimeout: () =>
        new Error(
          `the address of a block no node carries drew no line saying so; the stage drew ${drawnSlot(view)}, with the focus on ${document.activeElement?.tagName}`
        ),
    }
  );
  await waitFor(() =>
    expect(document.activeElement, "the line that names no block does not hold the focus").toBe(
      heading
    )
  );
  expect(heading.parentElement?.textContent).toContain(
    `That link names no row of ${KEY}'s outline.`
  );
  expect(drawnSlot(view), "the view was drawn beside the line").toBe("the body");
});

// A comment the tree holds, named beside the Decisions view, is the view's to land on: it draws the
// comment as a row before its parts, with the focus on it, and the stage keeps the view once every
// read is held.
test("a comment the tree holds, named beside the Decisions view, keeps the view, with the focus on the comment's row", async () => {
  const comments = spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
  try {
    const view = drawStage("?comment=reply-b");
    await waitFor(() =>
      expect(focusedRow(), `the stage drew ${drawnSlot(view)}, with the focus on no row`).toBe(
        "reply-b"
      )
    );
    // Every read has answered, and the view has said whether its tree draws the comment.
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
    expect(focusedRow(), "the focus left the comment's row").toBe("reply-b");
  } finally {
    comments.mockRestore();
  }
});

/** The issue's asks as the asks read answers them, with no reply under either: a question still
 *  open, and one the reader answered. */
const ASKED: readonly IssueAskRow[] = [
  answeredAsk({ id: "ask-open", answer: null, question: "Which flag?", state: "open" }),
  answeredAsk({ id: "ask-answered" }),
].map((asked) => ({ ...asked, last_reply: null }));

/** The decisions read: the reader's write-up of their answer to `ask-answered`, a section the
 *  server marks a person's because it names that ask. */
const WRITE_UP = decisionsRead([
  decision("writeup", { asks: ["ask-answered"], kind: "person", waiting_review: false }),
]);

/** The write-up's node, which the view draws no row for once the asks read tells it. */
const WRITE_UP_NODE = decisionNode("writeup").id;

// A person's decision whose section names a question of this issue they answered is their
// write-up of that answer, which the view draws no row for: the question's own row is the
// decision. An address naming the write-up keeps the view and lands on the row of the question
// it answers, though a question still open stands first in the list and the stage would give its
// row the focus.
test("a person's write-up of an answered question, named on the Decisions view's address, keeps the view, with the focus on the question's row", async () => {
  const asks = spyOn(api, "listIssueAsks").mockResolvedValue([...ASKED]);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(WRITE_UP);
  try {
    const view = drawStage("#b-writeup");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
      ).toBe("ask-answered")
    );
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
    expect(focusedRow(), "the focus left the question's row").toBe("ask-answered");
    expect(
      view.container.querySelector(`[data-node-id="${WRITE_UP_NODE}"]`),
      "the view drew the write-up as a row"
    ).toBeNull();
  } finally {
    asks.mockRestore();
    decisions.mockRestore();
  }
});

// Until the asks read answers, nothing tells the write-up from a decision of the person's own, so
// the list draws it as one, under the reader's decisions. The address waits for the asks rather
// than land on that row, which leaves the list once they answer and would take the focus with it.
test("a write-up's address waits for the asks read, which answers after the decisions read, and lands on the question's row", async () => {
  const asksOut = Promise.withResolvers<IssueAskRow[]>();
  const asks = spyOn(api, "listIssueAsks").mockReturnValue(asksOut.promise);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(WRITE_UP);
  try {
    const view = drawStage("#b-writeup");
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-node-id="${WRITE_UP_NODE}"]`),
        "the write-up was drawn as no decision of its own while the asks read is out"
      ).not.toBeNull()
    );
    await act(async () => asksOut.resolve([...ASKED]));
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus elsewhere once the asks answered; the stage drew ${drawnSlot(view)}`
      ).toBe("ask-answered")
    );
  } finally {
    asks.mockRestore();
    decisions.mockRestore();
  }
});

// Under words the answered question does not hold, the list draws neither the write-up, which is
// no row here, nor the question it answers, only the open question the words match, so the
// write-up's address has no row of the view to land on. The stage draws the outline in the view's
// place, which draws the write-up as a row under the question it answers and lands on it.
test("a write-up's address under words its question does not hold draws the outline, with the focus on the write-up's row", async () => {
  const asks = spyOn(api, "listIssueAsks").mockResolvedValue([...ASKED]);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(WRITE_UP);
  try {
    const view = drawStage("?q=flag#b-writeup");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus on ${document.activeElement?.tagName}; the stage drew ${drawnSlot(view)}`
      ).toBe(WRITE_UP_NODE)
    );
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the view, which has no row for the write-up").toBe(
      "the body"
    );
    expect(focusedRow(), "the focus left the write-up's row").toBe(WRITE_UP_NODE);
  } finally {
    asks.mockRestore();
    decisions.mockRestore();
  }
});

// The page's reads are refreshed by events, never by their age, and the stage draws the outline
// anew as the reader comes back to Conversation from a view. Under the app's `staleTime` a read is
// stale 30 s after it answered (`main.tsx`), each observer judging it by its own options: the
// outline's tree and its rows each read who is reading, and would ask for it again on a return
// more than 30 s after the page loaded unless they read it as the tree's own reads are kept fresh
// (`useIssueTree.ts`'s `EVENT_FRESH`). Here every read the page asks answered 31 s ago.
test("coming back to the outline from Decisions after the app's staleTime asks who is reading no second time", async () => {
  // The stub of who is reading that `stubReads` set: `spyOn` hands back that same spy, which
  // keeps its answer, and this counts its calls.
  const who = spyOn(api, "whoAmI");
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const issue = issueDetails();
  const answered = { updatedAt: Date.now() - 31_000 };
  client.setQueryData(
    whoAmIQuery().queryKey,
    { event_head: "0", kind: "user", login: READER },
    answered
  );
  client.setQueryData(
    issueCoverageQuery(KEY).queryKey,
    { gaps: [], has_plan: false, issue: KEY, requirements: [], spec_artifact_id: "" },
    answered
  );
  client.setQueryData(
    issueResultsQuery(KEY).queryKey,
    { checks: [], issue: KEY, verification_artifact_id: "" },
    answered
  );
  client.setQueryData(issueDecisionsQuery(KEY, true).queryKey, decisionsRead([]), answered);
  client.setQueryData(issueAsksQuery(KEY).queryKey, [], answered);
  // The issue holds two threads, so the outline draws rows.
  client.setQueryData(["comments", KEY], [...TWO_THREADS], answered);
  client.setQueryData(["issue-messages", KEY], [], answered);
  // The blocks of each of the issue's documents, the spec and the decisions document.
  for (const { id } of issue.artifacts ?? []) {
    client.setQueryData(artifactBlocksQuery(id).queryKey, [], answered);
  }
  // The names of what the tree's rows name: the issue itself and each of its comments.
  const named = [
    buildDispatchReference({ key: KEY, kind: "issue" }),
    ...TWO_THREADS.map(({ id }) => buildDispatchReference({ id, key: KEY, kind: "comment" })),
  ];
  client.setQueryData(referenceTargetsQuery(named).queryKey, { targets: [] }, answered);
  const held: { navigate?: NavigateFunction } = {};
  const view = drawStage("", { client, held, issue });
  expect(await emptyLineOf(view)).toBe(EMPTY_STATE_LINES.decisions);
  expect(who, "the Decisions view asked who is reading").not.toHaveBeenCalled();
  // The reader leaves the view for the outline, as a press of Conversation does.
  await act(async () => held.navigate?.(presetPath(KEY, "conversation")));
  await waitFor(() =>
    expect(
      view.container.querySelector('[data-issue-body] [data-node-id="head-b"]'),
      `the stage drew ${drawnSlot(view)}, with no row of the issue's threads`
    ).not.toBeNull()
  );
  // Long enough for a read the outline's mount started to have been sent.
  await waitInAct(50);
  expect(who, "the outline drawn again asked who is reading").not.toHaveBeenCalled();
});

/** The plan and the Verification document of the issue the Verification cases draw, beside its
 *  spec and its decisions document. */
const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The issue with its plan and its Verification document too, each in its role. */
function verifiedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT, VERIFICATION_DOCUMENT],
    plan_artifact_id: PLAN_DOCUMENT.id,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
}

/** The block ids of the spec's one statement, and of the plan task of the plan and the check of
 *  the Verification document that each serve it. */
const STATEMENT = "r1";
const PLAN_TASK = "t1";
const CHECK = "c1";

/** One typed block of a document as the blocks read answers it, with the statement it serves
 *  (`serves`, a `block[]` pointer) where it is given one. */
function typedBlock(id: string, type: string, serves?: string): ArtifactBlock {
  return {
    attributes: serves === undefined ? [] : [{ kind: "block[]", name: "serves", value: serves }],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

/** Each document's blocks, by its id: the spec's statement, and the plan's task and the
 *  Verification document's check, whose `serves` places each under that statement
 *  (`tree-drafts.ts`). */
const BLOCKS: Readonly<Record<string, readonly ArtifactBlock[]>> = {
  "artifact-spec": [typedBlock(STATEMENT, "requirement")],
  [PLAN_DOCUMENT.id]: [typedBlock(PLAN_TASK, "plan_task", specRef(STATEMENT))],
  [VERIFICATION_DOCUMENT.id]: [typedBlock(CHECK, "check", specRef(STATEMENT))],
};

/** Answers the reads the tree and the Verification view are drawn from for `verifiedIssue`: the
 *  coverage read with the statement, each document's blocks, the results read with the check,
 *  and the issue's two threads. Each spy is the one `stubReads` set, which `spyOn` hands back,
 *  so the file's `afterEach` restores it. */
function answerVerificationReads(): void {
  spyOn(api, "getIssueCoverage").mockResolvedValue({
    gaps: [],
    has_plan: true,
    issue: KEY,
    requirements: [
      {
        block_id: STATEMENT,
        checks: 1,
        gaps: [],
        plan_tasks: 1,
        ref: specRef(STATEMENT),
        text: "The page draws the plan.",
      },
    ],
    spec_artifact_id: "artifact-spec",
  });
  spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => [...(BLOCKS[id] ?? [])]);
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [check({ block_id: CHECK })],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
}

// Under a statement the Verification view draws that statement's checks alone
// (`verificationReading`), so a plan task that serves the statement, which the tree places under
// it, is no row of the view, though the view keeps it as the address's node. The view tells the
// stage so, and the stage draws the outline in the view's place, which lands on the task's row.
test("a plan task under a statement, named on the Verification view's address, draws the outline in the view's place, with the focus on the task's row", async () => {
  answerVerificationReads();
  const task = blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK);
  const view = drawStage(`#b-${PLAN_TASK}`, { issue: verifiedIssue(), preset: "verification" });
  await waitFor(() =>
    expect(
      focusedRow(),
      `the plan task's address left the focus on ${focusedRow() ?? document.activeElement?.tagName}; the stage drew ${drawnSlot(view)}`
    ).toBe(task)
  );
  // Every read has answered, and the view has said whether it draws the task.
  await waitInAct(20);
  expect(drawnSlot(view), "the stage drew the view, which has no row for the plan task").toBe(
    "the body"
  );
  expect(
    view.container.querySelector("[data-verification-body]"),
    "the Verification view was drawn beside the outline"
  ).toBeNull();
  expect(focusedRow(), "the focus left the plan task's row").toBe(task);
});

// A node the Verification view draws a row of keeps the view: a comment of the issue, which is
// of no kind the view draws, as a row of its own after the statements, and a check as a row
// under the statement it serves.
test("a comment of the issue or a check, named on the Verification view's address, keeps the view, which draws it as a row", async () => {
  answerVerificationReads();
  for (const [after, row] of [
    ["?comment=head-b", "head-b"],
    [`#b-${CHECK}`, blockNodeId(VERIFICATION_SLUG, CHECK)],
  ] as const) {
    const view = drawStage(after, { issue: verifiedIssue(), preset: "verification" });
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-verification-body] [data-node-id="${row}"]`),
        `the Verification view drew no row for ${after}; the stage drew ${drawnSlot(view)}`
      ).not.toBeNull()
    );
    // Every read has answered, and the view has said whether it draws the node.
    await waitInAct(20);
    expect(drawnSlot(view), `the stage drew the outline in place of the view for ${after}`).toBe(
      "the view"
    );
    cleanup();
  }
});
