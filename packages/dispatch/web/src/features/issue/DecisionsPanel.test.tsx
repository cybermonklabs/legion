import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type {
  Artifact,
  AskRead,
  Comment,
  Decision,
  DecisionKeep,
  DisplayTree,
  IssueAskRow,
  IssueDecisions,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { DecisionsPanel } from "./DecisionsPanel";

const documentId = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
const askId = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const questionId = "qqqqqqqq-qqqq-4qqq-8qqq-qqqqqqqqqqqq";

const artifacts = [
  { id: "spec-id", issue_key: "CORE-1", kind: "doc", primary: true, project: "", slug: "spec" },
  {
    id: documentId,
    issue_key: "CORE-1",
    kind: "doc",
    name: "decisions.md",
    primary: false,
    project: "",
    slug: "decisions-md",
  },
] as unknown as Artifact[];

function decision(fields: Partial<Decision> & Pick<Decision, "block_id" | "heading" | "text">) {
  return {
    asks: [],
    changed_at: "2026-11-01T00:00:00Z",
    changed_version: 3,
    kept: null,
    kind: "agent",
    links: [],
    ref: `dispatch://CORE-1/artifact/decisions-md#b-${fields.block_id}`,
    replaced: false,
    text_sha256: `sum-${fields.block_id}`,
    unsure: false,
    waiting_review: false,
    // The panel draws the reading Go made of the decision's text and splits nothing itself, so
    // a decision a test invents carries the reading the page's own engine makes of that text
    // (`__tests__/display-trees`). `warmMarkdownRenderer` must have resolved first, which is
    // why every view below is built inside its test rather than beside it.
    text_tree: displayTree(fields.text),
    ...fields,
  } as Decision;
}

function issueView(): IssueDecisions {
  const table = decision({
    block_id: "store",
    heading: "Store notices in a table",
    links: [
      {
        created_at: "2026-11-01T00:00:00Z",
        created_version: 1,
        from_block: "store-why",
        stale: false,
        to: graphNode({
          block_id: "p1",
          id: "CORE-1/spec#p1",
          kind: "block",
          text: "Notices are stored.",
        }),
      },
      {
        created_at: "2026-11-01T00:00:00Z",
        created_version: 1,
        from_block: "store-why",
        stale: true,
        to: graphNode({
          block_id: "p 2",
          id: "CORE-1/spec#p 2",
          kind: "block",
          text: "Old rows expire.",
        }),
      },
    ],
    text: "## Store notices in a table\n\nA table keeps them queryable.\n\nUnsure: the index may be slow.",
    unsure: true,
    waiting_review: true,
  });
  const named = decision({
    asks: [askId],
    block_id: "name",
    heading: "Name them notices",
    kind: "person",
    text: "## Name them notices\n\nThe person chose the name.",
  });
  const file = decision({
    block_id: "file",
    heading: "Keep them in a file",
    replaced: true,
    text: "### Keep them in a file\n\nA file was simpler.",
  });
  return {
    decisions: [table, named, file],
    document: "dispatch://CORE-1/artifact/decisions-md",
    open_questions: [
      {
        block_id: "q1",
        id: questionId,
        question: "Which index should the table carry?",
      } as unknown as IssueAskRow,
    ],
  };
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

function renderPanel() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DecisionsPanel artifacts={artifacts} isClosed={false} issueKey="CORE-1" />
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
    expect(question).toHaveAttribute("href", "/issues/CORE-1/spec#b-q1");

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
    // Each link opens the spec at the block it shaped, its id written as `encodeBlockId` writes it.
    const shaped = within(waiting)
      .getAllByRole("link")
      .map((link) => link.getAttribute("href"));
    expect(shaped).toEqual(["/issues/CORE-1/spec#b-p1", "/issues/CORE-1/spec#b-p%202"]);

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

test("Change opens the comment composer on decisions.md, quoting the decision's first paragraph, and posts it as the issue's comment", async () => {
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

/** The view one decision of its own makes: it waits for review, so the row draws its Keep and
 *  Change beside the blocks of its text. */
function waitingView(
  fields: Partial<Decision> & Pick<Decision, "heading" | "text">
): IssueDecisions {
  return {
    decisions: [decision({ block_id: "only", waiting_review: true, ...fields })],
    document: "dispatch://CORE-1/artifact/decisions-md",
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
      heading: "Name them notices",
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
      heading: "Wrap the lines",
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
      heading: "Read the keys",
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
      heading: "Keep a log",
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

// Each block of an answer that carried no reading cites what its own markdown holds and nothing
// the decision's other blocks hold. The browser parsed that block's markdown to draw it, so a
// `#q2` another block cites is no reference of this one: read from the whole decision, the
// escaped `\#q2` below would be painted into a link to an ask this block never named.
test("a block of an answer with no reading cites its own markdown, not the decision's", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({
      heading: "Name the region",
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
    // been linked would leave its paragraph unmatched and the row would end in that lookup's
    // timeout rather than here, where the count says what went wrong.
    expect(waiting.querySelectorAll("a[data-dispatch-ref]")).toHaveLength(1);
    // And it is the citing block's: the block that wrote the reference escaped draws it as the
    // text it is, under no anchor of its own.
    const literal = within(waiting)
      .getAllByText(/Write #q2 for the literal/, { ignore: false })
      .at(-1);
    expect(literal?.textContent).toContain("Write #q2 for the literal");
    expect(literal?.querySelector("a[data-dispatch-ref]")).toBeNull();
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
// would draw that text once per block, heading line and all.
test("a block whose tree the engine refuses shows its own text alone", async () => {
  const text = "## Keep a log\n\nA log is enough.\n\nUnsure: the index may be slow.";
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(
    waitingView({ heading: "Keep a log", text, text_tree: refusedTree(text) })
  );
  // The engine reports every tree it refuses, one report per block drawn.
  const logged = spyOn(console, "error").mockImplementation(() => {});
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
      heading: "Where the doubt is",
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
