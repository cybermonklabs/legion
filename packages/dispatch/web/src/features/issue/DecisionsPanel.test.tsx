import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { quietConsoleError } from "../../__tests__/act-warnings";
import { displayTree } from "../../__tests__/display-trees";
import { realClock, settle } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerQuestionOf, NAMED_ASK, NAMED_QUESTION } from "../../__tests__/question-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError } from "../../api/client";
import type {
  Artifact,
  AskRead,
  Comment,
  DecisionEntry,
  DecisionKeep,
  DecisionLink,
  DisplayTree,
  IssueAskRow,
  IssueDecisions,
} from "../../api/types";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { DecisionsPanel } from "./DecisionsPanel";
import type { DecisionsRead } from "./decision-entries";

const documentId = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
const askId = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const questionId = "qqqqqqqq-qqqq-4qqq-8qqq-qqqqqqqqqqqq";

const artifacts = [
  { id: "spec-id", issue_key: "CORE-1", kind: "doc", primary: true, project: "", slug: "spec" },
  {
    id: documentId,
    issue_key: "CORE-1",
    kind: "doc",
    name: "notes.md",
    primary: false,
    project: "",
    slug: "notes-md",
  },
] as unknown as Artifact[];

/** An entry of the Decisions read: by default an agent's decision under block `block` of
 *  notes.md, whose node holds it. */
function entry(
  fields: Partial<DecisionEntry> & Pick<DecisionEntry, "text"> & { block: string }
): DecisionEntry {
  const { block, ...entryFields } = fields;
  return {
    answer: null,
    answer_state: null,
    answered_at: null,
    answered_by: null,
    changed_at: "2026-11-01T00:00:00Z",
    changed_version: 3,
    decided_by: { id: "session-1", kind: "session" },
    declined: false,
    declined_note: null,
    document: {
      artifact_id: documentId,
      block: `CORE-1/notes-md#${block}`,
      block_id: block,
      ref: "dispatch://CORE-1/artifact/notes-md",
    },
    from_ask: [],
    kept: null,
    kept_by: null,
    kept_text_sum: null,
    kind: "agent",
    multiple: null,
    node: { id: `CORE-1/notes-md#${block}`, kind: "block" },
    options: null,
    question: null,
    readings: [],
    ref: `dispatch://CORE-1/artifact/notes-md#b-${block}`,
    replaced: false,
    resolution: null,
    selected: null,
    shaped: [],
    text_sha256: `sum-${block}`,
    // The panel draws the reading Go made of the decision's text and splits nothing itself, so
    // a decision a test invents carries the reading the page's own engine makes of that text
    // (`__tests__/display-trees`). `warmMarkdownRenderer` must have resolved first, which is
    // why every view below is built inside its test rather than beside it.
    text_tree: displayTree(fields.text),
    uncertainty: null,
    uncertainty_reason: null,
    urgency: null,
    waiting_review: false,
    ...entryFields,
  };
}

function shapedLink(blockId: string, stale: boolean): DecisionLink {
  return {
    changed_since_tie: false,
    created_at: "2026-11-01T00:00:00Z",
    created_version: 1,
    from_block: "store",
    stale,
    to: graphNode({
      block_id: blockId,
      id: `CORE-1/spec#${blockId}`,
      kind: "block",
      text: blockId === "p1" ? "Notices are stored." : "Old rows expire.",
    }),
  };
}

function issueView(): IssueDecisions {
  const table = entry({
    block: "store",
    shaped: [shapedLink("p1", false), shapedLink("p 2", true)],
    text: "## Store notices in a table\n\nA table keeps them queryable.\n\nUnsure: the index may be slow.",
    uncertainty: "high",
    waiting_review: true,
  });
  // A person's decision is the answered ask itself: its node holds it.
  const named = entry({
    answer: "The person chose the name.",
    answer_state: "answered",
    answered_at: "2026-11-01T00:00:00Z",
    answered_by: "bob",
    block: "name",
    kind: "person",
    node: { id: askId, kind: "ask" },
    question: "Name them notices",
    ref: `dispatch://CORE-1/ask/${askId}`,
    selected: [],
    text: "Name them notices",
    text_tree: undefined,
  });
  const file = entry({
    block: "file",
    replaced: true,
    text: "### Keep them in a file\n\nA file was simpler.",
  });
  const open = entry({
    answer_state: "open",
    block: "q1",
    kind: "person",
    node: { id: questionId, kind: "ask" },
    question: "Which index should the table carry?",
    ref: `dispatch://CORE-1/ask/${questionId}`,
    text: "Which index should the table carry?",
    text_tree: undefined,
  });
  return { decisions: [], document: null, entries: [open, table, named, file], open_questions: [] };
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

function renderPanel(
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  documents: readonly Artifact[] = artifacts
) {
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DecisionsPanel artifacts={documents} isClosed={false} issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function list(name: string): HTMLElement {
  return screen.getByRole("region", { name });
}

test("the panel sorts the view into its four lists, in order", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(issueView());
  try {
    renderPanel();
    await screen.findByRole("region", { name: "Open questions" });
    const headings = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(headings).toEqual([
      "Open questions",
      "Waiting for your review",
      "Your decisions",
      "Replaced",
    ]);

    const question = within(list("Open questions")).getByRole("link", {
      name: "Which index should the table carry?",
    });
    // Each entry links the node that holds it: an open question its ask.
    expect(question).toHaveAttribute("href", `/issues/CORE-1/asks/${questionId}`);

    const waiting = list("Waiting for your review");
    expect(within(waiting).getByText("Store notices in a table")).toBeDefined();
    expect(within(waiting).getByRole("button", { name: "Keep" })).toBeDefined();
    expect(within(waiting).getByRole("button", { name: "Change" })).toBeDefined();
    // The accent is on the block the server read the doubt in - one of the decision tree's own
    // top-level blocks - which is drawn in the commit that mounts it.
    await waitFor(() =>
      expect(within(waiting).getByTestId("decision-unsure")).toHaveTextContent(
        "the index may be slow."
      )
    );
    // The name links the node that holds the decision, and each link of what it shaped opens the
    // spec at that block, its id written as `encodeBlockId` writes it.
    const shaped = within(waiting)
      .getAllByRole("link")
      .map((link) => link.getAttribute("href"));
    expect(shaped).toEqual([
      "/issues/CORE-1/artifacts/notes-md#b-store",
      "/issues/CORE-1/spec#b-p1",
      "/issues/CORE-1/spec#b-p%202",
    ]);

    const mine = list("Your decisions");
    expect(within(mine).getByText("Name them notices")).toBeDefined();
    expect(within(mine).getByRole("link", { name: "The ask it answers" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/asks/${askId}`
    );
    expect(within(mine).queryByRole("button", { name: "Keep" })).not.toBeInTheDocument();

    expect(within(list("Replaced")).getByText("Keep them in a file")).toBeDefined();
  } finally {
    read.mockRestore();
  }
});

test("Keep records the decision and moves it from Waiting for your review to Your decisions", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(issueView());
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: documentId,
    block_id: "store",
    changed_version: 3,
    kept_at: "2026-11-02T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await user.click(within(waiting).getByRole("button", { name: "Keep" }));

    await waitFor(() => expect(keep).toHaveBeenCalled());
    expect(keep.mock.calls[0]?.slice(0, 3)).toEqual([documentId, "store", "sum-store"]);
    await waitFor(() =>
      expect(
        within(list("Waiting for your review")).queryByText("Store notices in a table")
      ).not.toBeInTheDocument()
    );
    expect(within(list("Your decisions")).getByText("Store notices in a table")).toBeDefined();
  } finally {
    read.mockRestore();
    keep.mockRestore();
  }
});

// A read of the decisions asked before a Keep answers the view from before it. Landing after the
// Keep's answer, it would put the kept decision back under Waiting for your review, Keep and all.
test("a read of the decisions asked before a Keep does not put the kept decision back as waiting", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const before = Promise.withResolvers<IssueDecisions>();
  const read = spyOn(api, "getIssueDecisions")
    .mockResolvedValueOnce(issueView())
    .mockReturnValueOnce(before.promise);
  const answer = Promise.withResolvers<DecisionKeep>();
  const keep = spyOn(api, "keepDecision").mockReturnValue(answer.promise);
  try {
    renderPanel(queryClient);
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    // A refresh of the decisions, asked before the Keep and still out.
    await act(async () => {
      void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", "CORE-1"] });
      await settle();
    });
    expect(read).toHaveBeenCalledTimes(2);

    await user.click(within(waiting).getByRole("button", { name: "Keep" }));
    await waitFor(() => expect(keep).toHaveBeenCalledTimes(1));
    const kept: DecisionKeep = {
      artifact_id: documentId,
      block_id: "store",
      changed_version: 3,
      kept_at: "2026-11-02T00:00:00Z",
      kept_by: "alice",
    };
    // The answer gives up the read still out and asks for the decisions again, after the Keep:
    // Dispatch holds the decision kept then.
    const view = issueView();
    read.mockResolvedValueOnce({
      ...view,
      entries: (view.entries ?? []).map((row) =>
        row.document?.block_id === kept.block_id
          ? { ...row, kept, kept_by: kept.kept_by, waiting_review: false }
          : row
      ),
    });
    await act(async () => {
      answer.resolve(kept);
      await settle();
    });
    await waitFor(() =>
      expect(within(list("Your decisions")).getByText("Store notices in a table")).toBeDefined()
    );

    // The read answers the view the server held before the Keep.
    await act(async () => {
      before.resolve(issueView());
      await settle();
    });
    await waitFor(() =>
      expect(queryClient.getQueryState(["decisions", "CORE-1"])?.fetchStatus).toBe("idle")
    );
    expect(
      within(list("Waiting for your review")).queryByText("Store notices in a table")
    ).not.toBeInTheDocument();
    expect(within(list("Your decisions")).getByText("Store notices in a table")).toBeDefined();
    expect(read).toHaveBeenCalledTimes(3);
  } finally {
    read.mockRestore();
    keep.mockRestore();
  }
});

test("Change opens the comment composer on notes.md, quoting the decision's first paragraph, and posts it as the issue's comment", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(issueView());
  const comment = spyOn(api, "createComment").mockResolvedValue({} as Comment);
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await user.click(within(waiting).getByRole("button", { name: "Change" }));

    const composer = within(waiting).getByRole("form", { name: "Change Store notices in a table" });
    expect(within(composer).getByTestId("decision-quote").textContent).toBe(
      "A table keeps them queryable."
    );
    await user.click(within(composer).getByLabelText("Your change"));
    await user.paste("Use a queue instead.");
    await user.click(within(composer).getByRole("button", { name: "Send" }));

    await waitFor(() => expect(comment).toHaveBeenCalled());
    expect(comment.mock.calls[0]?.slice(0, 2)).toEqual([
      "CORE-1",
      {
        anchor: { artifact: documentId, quote: "A table keeps them queryable." },
        body: "Use a queue instead.",
      },
    ]);
  } finally {
    read.mockRestore();
    comment.mockRestore();
  }
});

// An agent's decision written to settle a question names it in its `from_ask`, and its card
// links that ask, as a section that cited an ask linked it before decisions were nodes.
test("an agent's decision links each ask its from_ask settles", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue({
    decisions: [],
    document: null,
    entries: [
      entry({
        block: "store",
        from_ask: [askId],
        text: "## Store notices in a table\n\nA table keeps them queryable.",
        waiting_review: true,
      }),
    ],
    open_questions: [],
  });
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    expect(within(waiting).getByRole("link", { name: "The ask it answers" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/asks/${askId}`
    );
  } finally {
    read.mockRestore();
  }
});

const decisionsDocumentId = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee";

/** The issue's documents as the release before stored them: its decisions document beside them. */
const beforeArtifacts = [
  ...artifacts,
  {
    id: decisionsDocumentId,
    issue_key: "CORE-1",
    kind: "doc",
    name: "decisions.md",
    primary: false,
    project: "",
    slug: "decisions-md",
  },
] as unknown as Artifact[];

/** The Decisions read as a server of the release before answers it: no `entries`, its open
 *  questions, and its decisions, each a `##` section of the issue's decisions document addressed
 *  by its heading block - the agent's waiting one, a person's that cites the ask it answers, and
 *  a replaced one. */
function beforeView(): DecisionsRead {
  const section = (
    block: string,
    text: string,
    fields: Partial<NonNullable<DecisionsRead["decisions"]>[number]>
  ) => ({
    asks: [],
    block_id: block,
    changed_at: "2026-11-01T00:00:00Z",
    changed_version: 3,
    heading: "",
    kept: null,
    kind: "agent",
    links: [],
    ref: `dispatch://CORE-1/artifact/decisions-md#b-${block}`,
    replaced: false,
    text,
    text_sha256: `sum-${block}`,
    text_tree: displayTree(text),
    unsure: false,
    waiting_review: false,
    ...fields,
  });
  return {
    decisions: [
      section(
        "h-store",
        "## Store notices in a table\n\nA table keeps them queryable.\n\nUnsure: the index may be slow.",
        {
          heading: "Store notices in a table",
          links: [shapedLink("p1", false)],
          unsure: true,
          waiting_review: true,
        }
      ),
      section("h-name", "## Name them notices\n\nThe person chose the name.", {
        asks: [askId],
        heading: "Name them notices",
        kind: "person",
      }),
      section("h-file", "## Keep them in a file\n\nA file was simpler.", {
        heading: "Keep them in a file",
        replaced: true,
      }),
    ],
    document: "dispatch://CORE-1/artifact/decisions-md",
    open_questions: [
      {
        block_id: "q1",
        id: questionId,
        issue_key: "CORE-1",
        kind: "question",
        multiple: false,
        options: [],
        question: "Which index should the table carry?",
        state: "open",
        urgency: "normal",
      } as unknown as IssueAskRow,
    ],
  };
}

// During a roll a page of this build can reach a server of the release before, which answers no
// `entries`. The tab then draws that read's open questions and decisions as it drew them before
// decisions were nodes, and never says the issue has none; a Keep is written against the block of
// the decisions document the decision's address names.
test("a Decisions read of the release before, with no entries, is drawn from its own lists", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(beforeView() as IssueDecisions);
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: decisionsDocumentId,
    block_id: "h-store",
    changed_version: 3,
    kept_at: "2026-11-02T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  try {
    renderPanel(undefined, beforeArtifacts);
    await screen.findByRole("region", { name: "Open questions" });
    expect(screen.queryByText("This issue has no decisions yet.")).not.toBeInTheDocument();
    expect(
      within(list("Open questions")).getByRole("link", {
        name: "Which index should the table carry?",
      })
    ).toHaveAttribute("href", `/issues/CORE-1/asks/${questionId}`);

    const waiting = list("Waiting for your review");
    expect(within(waiting).getByText("Store notices in a table")).toBeDefined();
    await waitFor(() =>
      expect(within(waiting).getByTestId("decision-unsure")).toHaveTextContent(
        "the index may be slow."
      )
    );
    expect(
      within(waiting)
        .getAllByRole("link")
        .map((link) => link.getAttribute("href"))
    ).toEqual(["/issues/CORE-1/artifacts/decisions-md#b-h-store", "/issues/CORE-1/spec#b-p1"]);

    const mine = list("Your decisions");
    expect(within(mine).getByText("Name them notices")).toBeDefined();
    expect(within(mine).getByRole("link", { name: "The ask it answers" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/asks/${askId}`
    );
    expect(within(list("Replaced")).getByText("Keep them in a file")).toBeDefined();

    await user.click(within(waiting).getByRole("button", { name: "Keep" }));
    await waitFor(() => expect(keep).toHaveBeenCalled());
    expect(keep.mock.calls[0]?.slice(0, 3)).toEqual([
      decisionsDocumentId,
      "h-store",
      "sum-h-store",
    ]);
    await waitFor(() =>
      expect(within(list("Your decisions")).getByText("Store notices in a table")).toBeDefined()
    );
  } finally {
    read.mockRestore();
    keep.mockRestore();
  }
});

/** The view of one person's decision: an ask bob answered, `fields` its answer. */
function answeredView(fields: Partial<DecisionEntry>): IssueDecisions {
  const question = "Name them notices";
  return {
    decisions: [],
    document: null,
    entries: [
      entry({
        answer_state: "answered",
        answered_at: "2026-11-01T00:00:00Z",
        answered_by: "bob",
        block: "name",
        kind: "person",
        node: { id: askId, kind: "ask" },
        question,
        ref: `dispatch://CORE-1/ask/${askId}`,
        text: question,
        ...fields,
      }),
    ],
    open_questions: [],
  };
}

/** Counts the markdown parses the page's one engine makes from here on, which every body and
 *  preview of the panel reaches through (`markdown-engine.ts`, as `stored-text-drawing.test.tsx`
 *  counts them). Every tree a row seeds is read before this is called, since `displayTree` is a
 *  parse of that same engine. */
function countParses() {
  const engine = loadedEngine();
  if (engine === undefined) {
    throw new Error("the Markdown engine did not load");
  }
  return spyOn(engine, "parse");
}

// A person's decision is drawn by their answer, a stored text the server read for display
// beside it (`answer_tree`), so the tab draws that reading and parses no markdown, as it draws an
// agent's decision from the reading of its text.
test("a person's answer is drawn from the reading the server sent with it, parsing nothing", async () => {
  const answer = "The person chose **the name**.";
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    answeredView({ answer, answer_tree: displayTree(answer), selected: [] })
  );
  const parse = countParses();
  try {
    renderPanel();
    const mine = await screen.findByRole("region", { name: "Your decisions" });
    await waitFor(() => expect(within(mine).getByText("the name").tagName).toBe("STRONG"));
    expect(parse).not.toHaveBeenCalled();
  } finally {
    parse.mockRestore();
    read.mockRestore();
  }
});

// An answer that chose options and wrote nothing holds no stored text: the tab draws the labels
// chosen as the words they are, as the Answers page draws a chosen label, and parses nothing.
test("an answer of chosen options alone is drawn as their labels' words, parsing nothing", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    answeredView({ answer: null, selected: ["Notices", "A **bold** name"] })
  );
  const parse = countParses();
  try {
    renderPanel();
    const mine = await screen.findByRole("region", { name: "Your decisions" });
    expect(await within(mine).findByText("Notices, A **bold** name")).toBeDefined();
    expect(parse).not.toHaveBeenCalled();
  } finally {
    parse.mockRestore();
    read.mockRestore();
  }
});

// Two places of the panel hold no markup and draw their text's plain-text projection: an open
// question's link, and the name the Keep and the Change are given (the Change's form here). Each
// drops the formatting and reads a `dispatch://` address as its target's name - the reading
// list's `issue-open-question` and `issue-decision`. Drawn as the characters they are, each would
// hold `**` and the address.
test("an open question and the Change's name read their text's plain-text projection", async () => {
  const user = userEvent.setup();
  const question = "**Which** index does dispatch://CORE-2 need?";
  const view: IssueDecisions = {
    decisions: [],
    document: null,
    entries: [
      entry({
        answer_state: "open",
        block: "q1",
        kind: "person",
        node: { id: questionId, kind: "ask" },
        question,
        ref: `dispatch://CORE-1/ask/${questionId}`,
        text: question,
      }),
      entry({
        block: "only",
        text: "## Warm dispatch://CORE-2 first\n\nIt is cheap.",
        waiting_review: true,
      }),
    ],
    open_questions: [],
  };
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  const summary = spyOn(api, "getIssueSummary").mockImplementation(async (key) => ({
    documents: [],
    key,
    project: "CORE",
    status: "in_progress",
    title: "Cut the release",
  }));
  try {
    renderPanel();
    const open = await screen.findByRole("region", { name: "Open questions" });
    expect(
      await within(open).findByRole("link", {
        name: "Which index does CORE-2 Cut the release need?",
      })
    ).toHaveAttribute("href", `/issues/CORE-1/asks/${questionId}`);
    const waiting = list("Waiting for your review");
    await user.click(within(waiting).getByRole("button", { name: "Change" }));
    expect(
      await within(waiting).findByRole("form", { name: "Change Warm CORE-2 Cut the release first" })
    ).toBeDefined();
  } finally {
    read.mockRestore();
    summary.mockRestore();
  }
});

// A link under "Shaped in the spec" names the spec block by its id, in the panel's own words: the
// decisions read sends no text of the block a link points to (`refs.BlockLinks` resolves each end
// as a graph node, whose text only a reference read fills), so there is no stored text to draw,
// and the panel reads none. Handed a node that did carry text, the link still reads `Block p1`:
// a projection of that text would read `Warm CORE-2 Cut the release before the cut.`.
test("a shaped block's link names the block by its id and draws no text of the block", async () => {
  const view = waitingView({
    shaped: [
      {
        changed_since_tie: false,
        created_at: "2026-11-01T00:00:00Z",
        created_version: 1,
        from_block: "only",
        stale: false,
        to: graphNode({
          block_id: "p1",
          id: "CORE-1/spec#p1",
          kind: "block",
          text: "**Warm** dispatch://CORE-2 before the cut.",
        }),
      },
    ],
    text: "## Keep the notices queryable\n\nIt is cheap.",
  });
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  const summary = spyOn(api, "getIssueSummary").mockImplementation(async (key) => ({
    documents: [],
    key,
    project: "CORE",
    status: "in_progress",
    title: "Cut the release",
  }));
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    expect(await within(waiting).findByRole("link", { name: "Block p1" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/spec#b-p1"
    );
    expect(within(waiting).queryByText(/before the cut/)).toBeNull();
  } finally {
    read.mockRestore();
    summary.mockRestore();
  }
});

// A Keep or a change the reader stopped may have been made: the card says so, and that its
// button is a press of its own, rather than the stopped write's own words, which are a
// composer's and promise a Retry the card does not offer.
test("a Keep the reader stopped says it may have been made, not a message's words", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(issueView());
  const keep = spyOn(api, "keepDecision").mockRejectedValue(
    new NetworkError("/api/v1/decisions/keep", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    renderPanel(queryClient);
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    jest.useFakeTimers();
    await userOnFakeClock().click(within(waiting).getByRole("button", { name: "Keep" }));
    await stopTheLateWrite(queryClient);
    expect(within(waiting).getByRole("alert").textContent).toBe(
      stoppedWriteNote("the Keep of “Store notices in a table”", "made", {
        anew: "Pressing Keep again sends a new Keep.",
      })
    );
  } finally {
    realClock();
    queryClient.clear();
    read.mockRestore();
    keep.mockRestore();
  }
});

test("a change the reader stopped says it may have been posted, not a message's words", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(issueView());
  const comment = spyOn(api, "createComment").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/comments", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    renderPanel(queryClient);
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(within(waiting).getByRole("button", { name: "Change" }));
    const composer = within(waiting).getByRole("form", { name: "Change Store notices in a table" });
    await user.click(within(composer).getByLabelText("Your change"));
    await user.paste("Use a queue instead.");
    await user.click(within(composer).getByRole("button", { name: "Send" }));
    await stopTheLateWrite(queryClient);
    expect(within(composer).getByRole("alert").textContent).toBe(
      stoppedWriteNote("your change", "posted", { anew: "Send posts it again as a new comment." })
    );
  } finally {
    realClock();
    queryClient.clear();
    read.mockRestore();
    comment.mockRestore();
  }
});

/** The view one decision of its own makes: it waits for review, so the row draws its Keep and
 *  Change beside the blocks of its text. */
function waitingView(fields: Partial<DecisionEntry> & Pick<DecisionEntry, "text">): IssueDecisions {
  return {
    decisions: [],
    document: null,
    entries: [entry({ block: "only", waiting_review: true, ...fields })],
    open_questions: [],
  };
}

/** The quote the Change composer offers for the one decision `view` holds. */
async function quoteOf(view: IssueDecisions): Promise<string> {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await user.click(within(waiting).getByRole("button", { name: "Change" }));
    return within(waiting).getByTestId("decision-quote").textContent ?? "";
  } finally {
    read.mockRestore();
  }
}

// A Change anchors on the words of the decision's first block, which the server finds by
// matching them against the document's own flattened text: that text writes one space wherever
// two text runs are not adjacent in the document's positions (`pmdoc.buildFlattenedText`). The
// end of one list item and the start of the next are four positions apart, so the quote the
// composer sends must hold that space too, or `pmdoc.FindQuote` finds nothing to anchor on.
test("the quote of a first block that is a list holds one space between its items", async () => {
  const quote = await quoteOf(
    waitingView({
      text: "## Name them notices\n\n- **alpha**\n- beta\n\nThat is the whole of it.",
    })
  );
  expect(quote).toBe("alpha beta");
});

// The same rule at a hard break: the break is a leaf of its own and takes a position, so the
// two lines it divides are one space apart in the text the server matches a quote against.
test("the quote of a first block with a hard break holds one space between its lines", async () => {
  const quote = await quoteOf(
    waitingView({
      text: "## Wrap the lines\n\nline one  \nline two\n\nThat is the whole of it.",
    })
  );
  expect(quote).toBe("line one line two");
});

// `MarkdownBody` reads what a text cites out of the markdown it is handed, so each block is
// handed the decision's own markdown beside that block's reading. The words of a block hold
// neither a link's target nor the backticks that end a key, so a reading taken from them cites
// neither the ask this decision answers nor the key its code span names - both of which the
// server indexed for this same text.
test("a decision's references are read from its own markdown, not from the words of a block", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: `## Read the keys\n\nIt answers [the earlier ask](dispatch://CORE-1/ask/${askId}), and a \`CORE-9\`-style key names the issue it shaped.`,
    })
  );
  const summary = spyOn(api, "getIssueSummary").mockImplementation(async (key) => ({
    documents: [],
    key,
    project: "CORE",
    status: "in_progress",
    title: "The ninth issue",
  }));
  const ask = spyOn(api, "getAsk").mockRejectedValue(new Error("the ask is not read here"));
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    const link = await waitFor(() => {
      const found = waiting.querySelector<HTMLElement>("a[data-dispatch-ref]");
      if (found === null) {
        throw new Error("the ask the decision links is still plain text");
      }
      return found;
    });
    expect(link.getAttribute("data-dispatch-ref")).toBe(`dispatch://CORE-1/ask/${askId}`);
    expect(link.getAttribute("href")).toBe(`/issues/CORE-1/asks/${askId}`);
    await waitFor(() => expect(summary).toHaveBeenCalledWith("CORE-9"));
  } finally {
    ask.mockRestore();
    read.mockRestore();
    summary.mockRestore();
  }
});

// An answer with no reading of its text comes from a pod of the build before this one during a
// rolling deploy. The panel draws it the way every other view does: it splits the markdown
// itself and parses each block, so the decision keeps its body, its doubt and its Change.
test("a decision whose answer carried no reading is still drawn, quoted and marked unsure", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: "## Keep a log\n\nA log is enough.\n\nUnsure: the index may be slow.",
      text_tree: undefined,
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await waitFor(() => expect(within(waiting).getByText("A log is enough.")).toBeDefined());
    await waitFor(() =>
      expect(within(waiting).getByTestId("decision-unsure")).toHaveTextContent(
        "the index may be slow."
      )
    );
    await user.click(within(waiting).getByRole("button", { name: "Change" }));
    expect(within(waiting).getByTestId("decision-quote").textContent).toBe("A log is enough.");
  } finally {
    read.mockRestore();
  }
});

// The conversion of a decisions document moves a decision's `Unsure:` paragraph into its node's
// `uncertainty` and `uncertainty_reason`, so its words hold that paragraph no more. The tab draws
// the doubt where the paragraph drew, with the paragraph's words, and once.
test("a converted decision draws the doubt its node holds as its Unsure: paragraph drew", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: ':::decision{#only uncertainty="high" uncertainty_reason="the index may be slow."}\nKeep a log\n\nA log is enough.\n:::\n',
      uncertainty: "high",
      uncertainty_reason: "the index may be slow.",
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await waitFor(() => expect(within(waiting).getByText("A log is enough.")).toBeDefined());
    await waitFor(() =>
      expect(
        within(waiting)
          .queryAllByTestId("decision-unsure")
          .map((block) => block.textContent)
      ).toEqual(["Unsure: the index may be slow."])
    );
    // Drawn as a body draws a paragraph, not as the literal text a body falls back to.
    expect(within(waiting).getByTestId("decision-unsure").querySelector("p")?.textContent).toBe(
      "Unsure: the index may be slow."
    );
  } finally {
    read.mockRestore();
  }
});

// The conversion of a decisions document writes a section's heading as the first paragraph of the
// decision it becomes, and the server names a decision by that paragraph. The tab names the
// decision by it, as it named the section by its heading, and draws it once: the body below the
// name starts with the section's own words. A Change still quotes the decision's first block that
// is no heading, as the issue page's Change does (`anchorQuote`).
test("a converted decision is named by its first paragraph, which its body does not repeat", async () => {
  const user = userEvent.setup();
  const reason = "a reader who wants a file export would change this.";
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: `:::decision{#only uncertainty="high" uncertainty_reason="${reason}"}\nFiles stay out of the corpus\n\nWe write no file beside the database.\n:::\n`,
      uncertainty: "high",
      uncertainty_reason: reason,
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    expect(within(waiting).getByRole("heading", { level: 3 }).textContent).toBe(
      "Files stay out of the corpus"
    );
    await waitFor(() =>
      expect(within(waiting).getByText("We write no file beside the database.")).toBeDefined()
    );
    expect(within(waiting).getAllByText("Files stay out of the corpus")).toHaveLength(1);
    await user.click(within(waiting).getByRole("button", { name: "Change" }));
    const composer = within(waiting).getByRole("form", {
      name: "Change Files stay out of the corpus",
    });
    expect(within(composer).getByTestId("decision-quote").textContent).toBe(
      "Files stay out of the corpus"
    );
  } finally {
    read.mockRestore();
  }
});

// A card's title is plain text linked to the decision. A first paragraph that holds a link stays
// in the body so the reader can see and open that link.
test("a decision whose first paragraph holds a link keeps that paragraph, its link with it, in its body", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: ":::decision{#only}\nSee [the runbook](https://example.com/r) for the window.\n\nWe rotate weekly.\n:::\n",
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    expect(within(waiting).getByRole("heading", { level: 3 }).textContent).toBe(
      "See the runbook for the window."
    );
    await waitFor(() => expect(within(waiting).getByText("We rotate weekly.")).toBeDefined());
    const link = within(waiting).getByRole("link", { name: "the runbook" });
    expect(link).toHaveAttribute("href", "https://example.com/r");
  } finally {
    read.mockRestore();
  }
});

// A first block that holds inline code, emphasis, a reference, an image, or list structure carries
// content the plain title cannot show, so the card keeps that block in its body.
test("a decision whose first block holds inline code, emphasis, a mention, an image, or a list keeps that block in its body", async () => {
  const ask = spyOn(api, "getAsk").mockRejectedValue(new Error("the ask is not read here"));
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue({
    decisions: [],
    document: null,
    entries: [
      entry({
        block: "code",
        text: ":::decision{#code}\nUse `pg_stat` for timing.\n\nIt needs no restart.\n:::\n",
        waiting_review: true,
      }),
      entry({
        block: "em",
        text: ":::decision{#em}\nDo *not* drop the table.\n\nOld rows stay readable.\n:::\n",
        waiting_review: true,
      }),
      entry({
        block: "mention",
        text: `:::decision{#mention}\nSee dispatch://CORE-1/ask/${askId} for the window.\n\nWe rotate weekly.\n:::\n`,
        waiting_review: true,
      }),
      entry({
        block: "img",
        text: ":::decision{#img}\nLayout ![shot.png](https://example.com/shot.png)\n\nKeep the spacing.\n:::\n",
        waiting_review: true,
      }),
      entry({
        block: "list",
        text: ":::decision{#list}\n- Keep logs\n- Rotate weekly\n:::\n",
        waiting_review: true,
      }),
    ],
    open_questions: [],
  });
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    const cardOf = (block: string): HTMLElement => {
      const card = waiting.querySelector<HTMLElement>(`li[data-decision="${block}"]`);
      if (card === null) throw new Error(`missing card for ${block}`);
      return card;
    };
    await waitFor(() => expect(cardOf("code").querySelector("code")?.textContent).toBe("pg_stat"));
    expect(within(cardOf("code")).getByRole("heading", { level: 3 }).textContent).toBe(
      "Use pg_stat for timing."
    );
    expect(cardOf("em").querySelector("em")?.textContent).toBe("not");
    const mentionLink = await waitFor(() => {
      const found = cardOf("mention").querySelector<HTMLElement>("a[data-dispatch-ref]");
      if (found === null) throw new Error("the mention link is missing from the body");
      return found;
    });
    expect(mentionLink.getAttribute("href")).toBe(`/issues/CORE-1/asks/${askId}`);
    const image = cardOf("img").querySelector<HTMLImageElement>("img");
    expect(image?.getAttribute("src")).toBe("https://example.com/shot.png");
    expect(image?.getAttribute("alt")).toBe("shot.png");
    expect([...cardOf("list").querySelectorAll("li")].map((item) => item.textContent)).toEqual([
      "Keep logs",
      "Rotate weekly",
    ]);
  } finally {
    ask.mockRestore();
    read.mockRestore();
  }
});

// An answer with no reading of its text names the decision by the first line of its markdown
// (`decisionName`). A first paragraph that runs over two lines is more than that name, so the body
// still draws the whole paragraph: hiding it would lose every line after the first.
test("a decision with no reading whose first paragraph spans two lines keeps its words", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: ":::decision{#only}\nKeep a log for each run\nand rotate it weekly.\n\nA log is enough.\n:::\n",
      text_tree: undefined,
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    expect(within(waiting).getByRole("heading", { level: 3 }).textContent).toBe(
      "Keep a log for each run"
    );
    await waitFor(() => expect(within(waiting).getByText("A log is enough.")).toBeDefined());
    expect(
      within(waiting).getByText(/^Keep a log for each run\s+and rotate it weekly\.$/)
    ).toBeDefined();
  } finally {
    read.mockRestore();
  }
});

// Without a server reading, a single-line first paragraph that holds a link still stays in the
// body so the reader can open that link.
test("a decision with no reading whose first paragraph holds a link keeps that paragraph in its body", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: ":::decision{#only}\nSee [the runbook](https://example.com/r) for the window.\n\nWe rotate weekly.\n:::\n",
      text_tree: undefined,
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await waitFor(() => expect(within(waiting).getByText("We rotate weekly.")).toBeDefined());
    const link = within(waiting).getByRole("link", { name: "the runbook" });
    expect(link).toHaveAttribute("href", "https://example.com/r");
  } finally {
    read.mockRestore();
  }
});

// An open question is its ask's record, and a decision is a block of the document it is written
// in: a `#id` in either names the question of that record's document, so the question's link and
// the Change's name read it as the question it names, and the decision's body links it. Handed no
// record, each keeps `#q2`.
test("a #id in an open question and in a decision's name names the question of its record", async () => {
  const view: IssueDecisions = {
    decisions: [],
    document: null,
    entries: [
      entry({
        answer_state: "open",
        block: "q1",
        kind: "person",
        node: { id: questionId, kind: "ask" },
        question: "Decide #q2 now?",
        ref: `dispatch://CORE-1/ask/${questionId}`,
        text: "Decide #q2 now?",
      }),
      entry({
        block: "only",
        text: ":::decision{#only}\nSettle #q2 first\n\nIt is cheap.\n:::\n",
        waiting_review: true,
      }),
    ],
    open_questions: [],
  };
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  for (const [kind, id, words] of [
    ["ask", questionId, `Decide ${NAMED_QUESTION} now?`],
    ["artifact", documentId, `Change Settle ${NAMED_QUESTION} first`],
  ] as const) {
    const answers = answerQuestionOf(kind, id);
    const user = userEvent.setup();
    const panel = renderPanel();
    try {
      if (kind === "ask") {
        const open = await screen.findByRole("region", { name: "Open questions" });
        expect(await within(open).findByRole("link", { name: words })).toBeDefined();
      } else {
        const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
        await waitFor(() =>
          expect(waiting.querySelector(`[data-dispatch-ref="${NAMED_ASK}"]`)).not.toBeNull()
        );
        await user.click(within(waiting).getByRole("button", { name: "Change" }));
        await waitFor(() =>
          expect(within(waiting).getByRole("form", { name: words })).toBeDefined()
        );
      }
    } finally {
      panel.unmount();
      answers.restore();
    }
  }
  read.mockRestore();
});

// Each block of an answer that carried no reading cites what its own markdown holds and nothing
// the decision's other blocks hold. The browser parsed that block's markdown to draw it, so a
// `#q2` another block cites is no reference of this one: read from the whole decision, the
// escaped `\#q2` below would be painted into a link to an ask this block never named.
test("a block of an answer with no reading cites its own markdown, not the decision's", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: "## Name the region\n\nIt answers #q2.\n\nWrite \\#q2 for the literal.",
      text_tree: undefined,
    })
  );
  const questions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: documentId,
    questions: [{ ask: askId, block_id: "q2", reference: `dispatch://CORE-1/ask/${askId}` }],
  });
  // The link reads the ask it names, so the row answers that read rather than leaving it to
  // reach a network no test serves.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    answers: [],
    ask: {
      block_id: "q2",
      id: askId,
      issue_key: "CORE-1",
      kind: "question",
      options: [],
      question: "Which region?",
      state: "open",
    } as unknown as IssueAskRow,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  } as unknown as AskRead);
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    const cited = await waitFor(() => {
      const found = waiting.querySelector<HTMLElement>("a[data-dispatch-ref]");
      if (found === null) {
        throw new Error("the `#q2` of the block that cites it is still plain text");
      }
      return found;
    });
    expect(cited.getAttribute("data-dispatch-ref")).toBe(`dispatch://CORE-1/ask/${askId}`);
    // The link stands in the block whose own markdown cites the question, and the reference
    // draws under the ask's own title rather than under the `#q2` the author wrote.
    expect(cited.closest("p")?.textContent).toContain("It answers");
    // One reference link in the whole card, asserted before the literal is looked for: a text
    // lookup finds only an element whose own text nodes hold the phrase, so a literal that had
    // been linked would leave its paragraph unmatched and the row would die where that lookup
    // throws for finding nothing, rather than here, where the count says what went wrong.
    expect(waiting.querySelectorAll("a[data-dispatch-ref]")).toHaveLength(1);
    // And it is the citing block's: the block that wrote the reference escaped draws it as the
    // text it is, under no anchor of its own.
    const literal = within(waiting).getByText(/Write #q2 for the literal/);
    expect(literal.querySelector("a[data-dispatch-ref]")).toBeNull();
  } finally {
    getAsk.mockRestore();
    questions.mockRestore();
    read.mockRestore();
  }
});

/** The reading of `text` with a mark no schema of this build carries over each of its blocks,
 *  which `engine.draw` refuses as it refuses any tree its schema does not fit
 *  (`markdown-serializer.ts`): a pod of a later build reads a text under its own schema, and a
 *  mark that schema grew is one this page cannot draw. A body that cannot draw its tree shows
 *  the text it was handed instead (`useRenderedMarkdown`), which is what this row reads. */
function refusedTree(text: string): DisplayTree {
  const tree = displayTree(text);
  const blocks = tree?.content as readonly DisplayTree[] | undefined;
  if (tree === undefined || blocks === undefined) {
    throw new Error("the engine read no tree for the decision's text");
  }
  return {
    ...tree,
    content: blocks.map((node) => ({ ...node, marks: [{ type: "aMarkOfALaterSchema" }] })),
  };
}

// A block whose tree cannot be drawn shows the text that body was handed, so each body is
// handed its own block's text and no more: the decision's whole text in every one of them
// would draw that text once per block, heading line and all. The heading links the node that
// holds the decision by its plain words, read from its own node of the same tree, so it shows
// its own words when that node is refused.
test("a block whose tree the engine refuses shows its own text alone", async () => {
  const text = "## Keep a log\n\nA log is enough.\n\nUnsure: the index may be slow.";
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({ text, text_tree: refusedTree(text) })
  );
  // The engine reports every tree it refuses, one report per block drawn.
  const logged = quietConsoleError();
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    const fallbacks = await waitFor(() => {
      const found = waiting.querySelectorAll<HTMLElement>("[data-markdown-fallback]");
      expect(found).toHaveLength(2);
      return found;
    });
    expect([...fallbacks].map((body) => body.textContent)).toEqual([
      "A log is enough.",
      "Unsure: the index may be slow.",
    ]);
    expect(within(waiting).getByRole("heading", { level: 3 }).textContent).toBe("Keep a log");
    // The heading is the card's own line, never a line of a body, and the text below it is
    // drawn once however many blocks it holds.
    expect(waiting.textContent).not.toContain("#");
    expect(waiting.textContent?.split("A log is enough.")).toHaveLength(2);
    expect(logged).toHaveBeenCalled();
  } finally {
    logged.mockRestore();
    read.mockRestore();
  }
});

// The server marks a decision unsure for a paragraph whose markdown starts `Unsure:` and for
// nothing else (`api/decisions.go`), so the accent the panel draws follows that rule exactly: a
// strong `Unsure:`, a list item, a quote and a code block are the decision's words like any
// other, and `decision.unsure` stays false for every one of them.
test("only a plain paragraph that starts Unsure: takes the accent", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      text: [
        "## Where the doubt is",
        "Unsure: the index may be slow.",
        "**Unsure:** the strong one.",
        "- Unsure: the list one.",
        "> Unsure: the quoted one.",
        "```\nUnsure: the fenced one.\n```",
      ].join("\n\n"),
    })
  );
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await waitFor(() =>
      expect(within(waiting).getByText("the fenced one.", { exact: false })).toBeDefined()
    );
    expect(
      within(waiting)
        .queryAllByTestId("decision-unsure")
        .map((block) => block.textContent)
    ).toEqual(["Unsure: the index may be slow."]);
  } finally {
    read.mockRestore();
  }
});
