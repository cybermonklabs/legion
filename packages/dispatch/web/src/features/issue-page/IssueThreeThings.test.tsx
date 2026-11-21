import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import { artifactBlocksQuery, issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type {
  Artifact,
  ArtifactBlock,
  Decision,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildDispatchReference } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { IssueThreeThings } from "./IssueThreeThings";
import { IssueTree } from "./IssueTree";
import { buildIssueTree, type TreeNode } from "./issue-tree";
import { useIssueNames } from "./one-line";
import { EMPTY_STATE_LINES, type NodeType } from "./page-words";
import { ContentsSlot } from "./slots/contents";
import { ProvidersSlot } from "./slots/providers";
import { specSummary } from "./spec-summary";
import { blockNodeId, blockWords } from "./tree-drafts";
import { nodeRowSelectorOf } from "./tree-focus";

/**
 * The head of the page draws the issue in plain language: at most three bullets, each one spoken
 * sentence of the spec's summary (R5), out of the server's reading of the
 * summary node's text (its `tree`), which the tree the page already holds carries - the head
 * reads no document of its own (R20).
 *
 * Every row below renders the head with no `QueryClientProvider` around it, which is the whole
 * proof of R20 here: a component that read anything would throw for want of a client, so these
 * rows pass only while the head makes no read at all.
 */

// A summary's reading is made with the page's own engine (`displayTree`), which loads once.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const KEY = "CORE-1";

/** A node of the tree with nothing on it but its kind, and `fields`. A requirement here is one
 *  of the issue's own spec, which the page's model marks a statement (`isStatement`). */
function treeNode(id: string, type: NodeType, fields: Partial<TreeNode> = {}): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id,
    question: false,
    replaced: false,
    text: "",
    transcluded: false,
    type,
    waiting: false,
    ...(type === "requirement" ? { statement: true as const } : {}),
    ...fields,
  };
}

/** The spec's summary node, as the tree holds it: a `callout` whose text is the block's stored
 *  excerpt with its fence left out (`blockWords`), and whose `tree` is the server's reading of
 *  the block's whole text (`displayTree`), which is what the head reads. */
function summaryNode(text: string): TreeNode {
  return treeNode("spec#b-summary", "callout", {
    text: blockWords(`:::callout{#summary title="What this issue is"}\n${text}\n:::`),
    tree: displayTree(text),
  });
}

/** The head as a slot draws it: what the summary cites named once over `root`, the whole tree
 *  the summary is a node of (`slots/contents.tsx`), for an issue whose spec's blocks are held
 *  unless `specHeld` says not. */
function Head({
  look,
  root,
  specHeld = true,
  summary,
}: {
  look: "contents" | "spec";
  root: TreeNode;
  specHeld?: boolean;
  summary: TreeNode | undefined;
}): ReactNode {
  const issue = useMemo(() => issueDetails(), []);
  const { nameOf } = useIssueNames(issue, root);
  return <IssueThreeThings look={look} nameOf={nameOf} specHeld={specHeld} summary={summary} />;
}

/** The sentences the head draws, in order, and the label before each. `reading` is whether the
 *  reads that carry the summary are still out; `root` is the tree the summary is a node of,
 *  which is the issue holding it alone unless a case says more. */
function bullets(
  summary: TreeNode | undefined,
  reading = false,
  look: "contents" | "spec" = "contents",
  root: TreeNode = treeNode(KEY, "issue", { children: summary === undefined ? [] : [summary] })
): { labels: string[]; lines: string[]; unmount: () => void } {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <Head look={look} root={root} specHeld={!reading} summary={summary} />
    </MemoryRouter>
  );
  const items = within(screen.getByTestId("issue-three-things")).queryAllByRole("listitem");
  return {
    labels: items.map((item) => item.querySelector("[data-page-chrome]")?.textContent ?? ""),
    lines: items.map((item) => item.querySelector("[data-author-text]")?.textContent ?? ""),
    unmount: view.unmount,
  };
}

// Each sentence stands after its label, in the order the summary holds them, in the Contents
// slot (the pane and the Outline sheet) and the Spec view's first group alike; and the two rows
// the spec named under them are drawn nowhere.
test("each sentence stands after its label, and the two rows are not drawn", () => {
  for (const look of ["contents", "spec"] as const) {
    const { labels, lines, unmount } = bullets(
      summaryNode("One body. Every property shows. The reasoning is one press away."),
      false,
      look
    );
    try {
      expect(labels).toEqual(["What it is:", "Why it matters:", "Done when:"]);
      expect(lines).toEqual([
        "One body.",
        "Every property shows.",
        "The reasoning is one press away.",
      ]);
      const head = screen.getByTestId("issue-three-things");
      expect(head.textContent).not.toContain("How it gets done");
      expect(head.textContent).not.toContain("What you will be shown");
      expect(head.querySelector("a")).toBeNull();
    } finally {
      unmount();
    }
  }
});

test("a summary of four sentences draws the first three of them", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "The issue page draws one ordered body. Every property is visible at every width. " +
        "The reasoning behind each requirement is one press away. Nothing the page drew is lost."
    )
  );
  try {
    expect(lines).toEqual([
      "The issue page draws one ordered body.",
      "Every property is visible at every width.",
      "The reasoning behind each requirement is one press away.",
    ]);
  } finally {
    unmount();
  }
});

// An author who wrote two sentences gets two: no third bullet, and nothing drawn in its place.
test("a summary of two sentences draws two bullets and says nothing about the third", () => {
  const { lines, unmount } = bullets(
    summaryNode("The page is one outline. It opens folded to the work in front of the reader.")
  );
  try {
    expect(lines).toEqual([
      "The page is one outline.",
      "It opens folded to the work in front of the reader.",
    ]);
    expect(screen.getByTestId("issue-three-things").textContent).not.toContain(
      EMPTY_STATE_LINES.spec_summary
    );
  } finally {
    unmount();
  }
});

// The head reads the summary's reading and never the node's text, which is the stored excerpt,
// so a summary node whose reading the batched reference read has not sent yet draws nothing of
// that text - and, being a summary all the same, never says that the spec has none.
test("a summary node whose reading has not come says nothing", () => {
  const { lines, unmount } = bullets({
    ...summaryNode("The page is one outline. It fits a phone."),
    tree: undefined,
  });
  try {
    expect(lines).toEqual([]);
    const head = screen.getByTestId("issue-three-things").textContent;
    expect(head).not.toContain("The page is one outline.");
    for (const line of Object.values(EMPTY_STATE_LINES)) {
      expect(head).not.toContain(line);
    }
  } finally {
    unmount();
  }
});

// R1: a summary whose words hold no sentence end still says something, and never that it is too
// long: its first line, by the one-line rule every place that draws one line for a node keeps,
// is its one sentence - whole where it is short, and cut at a word with its ellipsis past
// `FIRST_LINE_LIMIT`'s 100 characters.
test("a summary with no sentence end draws its first line as one sentence", () => {
  for (const [text, line] of [
    ["The page is one outline", "The page is one outline"],
    [
      "The page is one outline that a reader opens folded to the work in front of them, and " +
        "reads at every width with nothing lost along the way",
      "The page is one outline that a reader opens folded to the work in front of them, and " +
        "reads at every…",
    ],
  ] as const) {
    const { lines, unmount } = bullets(summaryNode(text));
    try {
      expect(lines).toEqual([line]);
      const head = screen.getByTestId("issue-three-things").textContent;
      for (const empty of Object.values(EMPTY_STATE_LINES)) {
        expect(head).not.toContain(empty);
      }
    } finally {
      unmount();
    }
  }
});

// A summary may say one sentence twice. Each is a bullet of its own, and React is told the two
// apart by their place: keyed by its words alone, the two bullets shared one key.
test("a summary that says one sentence twice draws both, each under a key of its own", () => {
  const errors = spyOn(console, "error").mockImplementation(() => {});
  try {
    const { lines, unmount } = bullets(
      summaryNode("The page is one outline. The page is one outline. It fits a phone.")
    );
    try {
      expect(lines).toEqual([
        "The page is one outline.",
        "The page is one outline.",
        "It fits a phone.",
      ]);
      expect(errors.mock.calls.filter((call) => String(call[0]).includes("same key"))).toEqual([]);
    } finally {
      unmount();
    }
  } finally {
    errors.mockRestore();
  }
});

// A spec that carries no summary node at all still draws its row in that same place, saying so.
test("a spec with no summary draws the page's own line and no bullet", () => {
  const { lines, unmount } = bullets(undefined);
  try {
    expect(lines).toEqual([]);
    expect(screen.getByTestId("issue-three-things").textContent).toContain(
      EMPTY_STATE_LINES.spec_summary
    );
  } finally {
    unmount();
  }
});

// R5 and R8: an issue with no spec has no summary to say is missing, so the head says nothing of
// one, where a spec with no summary says it has none.
test("an issue with no spec says nothing of a summary", () => {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <Head look="contents" root={treeNode(KEY, "issue")} specHeld={false} summary={undefined} />
    </MemoryRouter>
  );
  try {
    expect(screen.getByTestId("issue-three-things").textContent).not.toContain(
      EMPTY_STATE_LINES.spec_summary
    );
  } finally {
    view.unmount();
  }
});

// The page's first load draws the tree before the spec's blocks and their words arrive, so the
// summary node is missing, or there with no reading yet. That is no spec without a summary, and
// the head says nothing about one until the reads that carry it have answered.
test("while the reads that carry the summary are out, the head says nothing about it", () => {
  for (const summary of [
    undefined,
    { ...summaryNode("The page is one outline."), tree: undefined },
  ]) {
    const { lines, unmount } = bullets(summary, true);
    try {
      expect(lines).toEqual([]);
      expect(screen.getByTestId("issue-three-things").textContent).not.toContain(
        EMPTY_STATE_LINES.spec_summary
      );
    } finally {
      unmount();
    }
  }
});

// A summary written as bullets is read as the sentences its items hold, each item one bullet
// of the head whether or not its author ended it with a full stop: the item's own end is where
// it stops, and the marker in front of it is the list's syntax, not the author's words.
test("a summary written as bullets draws each item as a sentence", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "- What: the page is one outline\n- Why: a reader finds the work\n- Done: it fits a phone"
    )
  );
  try {
    expect(lines).toEqual([
      "What: the page is one outline",
      "Why: a reader finds the work",
      "Done: it fits a phone",
    ]);
  } finally {
    unmount();
  }
});

// A sentence in bold still ends at its full stop: the asterisks after it are the emphasis, and
// they are neither the sentence's end nor words of it.
test("a summary in bold, or ending in a bold sentence, draws that sentence", () => {
  const whole = bullets(summaryNode("**The page is one outline.**"));
  try {
    expect(whole.lines).toEqual(["The page is one outline."]);
  } finally {
    whole.unmount();
  }
  const last = bullets(
    summaryNode("The page is one outline. Nothing is lost. **It fits a phone.**")
  );
  try {
    expect(last.lines).toEqual([
      "The page is one outline.",
      "Nothing is lost.",
      "It fits a phone.",
    ]);
  } finally {
    last.unmount();
  }
});

// R1: each item of a list ends a sentence whether or not its author ended it with a mark, so a
// list's last item is drawn too. A cut excerpt with its fence left out ends in that item as it
// would in a fragment of one, so only the reading tells the two apart.
test("a bulleted summary draws its last item though it has no end mark", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "1. The page is one outline.\n2. A reader finds the work in it.\n3. It fits a phone"
    )
  );
  try {
    expect(lines).toEqual([
      "The page is one outline.",
      "A reader finds the work in it.",
      "It fits a phone",
    ]);
  } finally {
    unmount();
  }
});

// Review finding 4590606164: a leading label opens its sentence and ends none, and a full stop
// before a word in lower case ends an abbreviation, not a sentence, so each label names the
// sentence its author wrote.
test("a sentence is ended neither at a leading label nor before a word in lower case", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "R1. The page draws one body. It matters to readers, e.g. tester and owner. Done when it ships."
    )
  );
  try {
    expect(lines).toEqual([
      "R1. The page draws one body.",
      "It matters to readers, e.g. tester and owner.",
      "Done when it ships.",
    ]);
  } finally {
    unmount();
  }
});

// R1: the issue's three sentences read the same sentence end as a person's quote (`issue-tree.ts`'s
// `firstSentenceLength`), so a full stop that a number follows ends an abbreviation, not a
// sentence.
test("a sentence is not ended at an abbreviation that a number follows", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "The page keeps the old order (see p. 4) for now. It fits a phone. Done when it ships."
    )
  );
  try {
    expect(lines).toEqual([
      "The page keeps the old order (see p. 4) for now.",
      "It fits a phone.",
      "Done when it ships.",
    ]);
  } finally {
    unmount();
  }
});

// The issue's three sentences read the same sentence end as a person's quote, so a full stop
// that a page 1 follows ends an abbreviation, not a sentence, where no "2." follows it.
test("a sentence is not ended at an abbreviation that 1 follows", () => {
  const { lines, unmount } = bullets(
    summaryNode("Draw it whole, as on p. 1. It fits a phone. Done when it ships.")
  );
  try {
    expect(lines).toEqual([
      "Draw it whole, as on p. 1.",
      "It fits a phone.",
      "Done when it ships.",
    ]);
  } finally {
    unmount();
  }
});

// R1: a leading label and the words after it with no end hold no sentence end, so the summary's
// first line is its one sentence, with the label in it and not the label alone.
test("a summary of a leading label and words with no end draws its first line", () => {
  const { lines, unmount } = bullets(summaryNode("R1. The page draws one body"));
  try {
    expect(lines).toEqual(["R1. The page draws one body"]);
  } finally {
    unmount();
  }
});

// R1 and the tester's F20 (reading (b) of the plan): a sentence is a plain line and no link, so a
// block it cites is drawn by the one name the page gives that block - the words of a chip of the
// tree that names it - and never by its address or its block id.
test("a summary sentence that cites a block draws the block's name, not its address", () => {
  const cited = `dispatch://${KEY}/spec#b-r1`;
  const summary = summaryNode(`The page is one outline. It draws ${cited} first.`);
  const root = treeNode(KEY, "issue", {
    children: [
      summary,
      treeNode(blockNodeId("spec", "r1"), "requirement"),
      treeNode(blockNodeId("decisions", "d1"), "decision", {
        chips: [
          { answered: true, gone: false, name: "the whole issue in one reading", ref: cited },
        ],
      }),
    ],
  });
  const { lines, unmount } = bullets(summary, false, "contents", root);
  try {
    expect(lines).toEqual([
      "The page is one outline.",
      "It draws the whole issue in one reading first.",
    ]);
    const head = screen.getByTestId("issue-three-things").textContent;
    expect(head).not.toContain("dispatch://");
    expect(head).not.toContain("#b-");
  } finally {
    unmount();
  }
});

// R1: each sentence whole as its author wrote it, after its own label. A cited statement is
// named by its own first words (`useReferenceNames`), which in a spec that numbers its
// statements open with a label ("R1."), so a sentence end read after the naming splits the
// author's first sentence at the name and moves "Why it matters:" onto its second half.
test("a summary sentence that cites a labelled statement stays one sentence under its own label", () => {
  const cited = `dispatch://${KEY}/spec#b-r1`;
  const summary = summaryNode(
    `The page draws ${cited} first. Every reader finds the work. It fits a phone.`
  );
  const root = treeNode(KEY, "issue", {
    children: [
      summary,
      treeNode(blockNodeId("spec", "r1"), "requirement"),
      treeNode(blockNodeId("decisions", "d1"), "decision", {
        chips: [{ answered: true, gone: false, name: "R1. Every node is whole", ref: cited }],
      }),
    ],
  });
  const { labels, lines, unmount } = bullets(summary, false, "contents", root);
  try {
    expect(lines).toEqual([
      "The page draws R1. Every node is whole first.",
      "Every reader finds the work.",
      "It fits a phone.",
    ]);
    expect(labels).toHaveLength(3);
  } finally {
    unmount();
  }
});

// R1: whether a summary holds a sentence end is read before the names go in, so a summary whose
// author wrote none is one sentence, its first line, though the statement it cites is named by
// words that open with a label ("R1.").
test("a summary with no sentence end that cites a labelled statement draws it as one sentence", () => {
  const cited = `dispatch://${KEY}/spec#b-r1`;
  const summary = summaryNode(`The page draws ${cited} first on every phone`);
  const root = treeNode(KEY, "issue", {
    children: [
      summary,
      treeNode(blockNodeId("spec", "r1"), "requirement"),
      treeNode(blockNodeId("decisions", "d1"), "decision", {
        chips: [{ answered: true, gone: false, name: "R1. Every node is whole", ref: cited }],
      }),
    ],
  });
  const { labels, lines, unmount } = bullets(summary, false, "contents", root);
  try {
    expect(lines).toEqual(["The page draws R1. Every node is whole first on every phone"]);
    expect(labels).toEqual(["What it is:"]);
  } finally {
    unmount();
  }
});

const decisionsDocument: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-decisions",
  issue_key: KEY,
  kind: "doc",
  name: "Decisions",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-decisions",
  session_id: null,
  slug: "decisions",
  versions: [],
};

/** The issue's spec, as the issue read lists it. */
const specDocument: Artifact = {
  ...decisionsDocument,
  id: "artifact-spec",
  name: "spec.md",
  primary: true,
  slug: "spec",
};

/** The issue as the issue read answers it, with a spec and a decisions document: what tells
 *  the page how this issue addresses a node (`treeContext`). */
function issueDetails(): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [specDocument, decisionsDocument],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    decisions_artifact_id: decisionsDocument.id,
    design_gates: [],
    external_links: [],
    holder_state: null,
    key: KEY,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    primary_artifact_id: specDocument.id,
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    takeable_at: null,
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: "",
  } satisfies IssueDetails;
}

const decision: Decision = {
  asks: [],
  block_id: "d1",
  changed_at: null,
  changed_version: 1,
  heading: "Ship behind a flag",
  kept: null,
  kind: "agent",
  links: [],
  ref: `dispatch://${KEY}/artifact/decisions#b-d1`,
  replaced: false,
  text: "## Ship behind a flag\n\nWe ship it behind a flag.",
  text_sha256: "sha256:one",
  unsure: false,
  waiting_review: false,
};

/** One block of a document, as the blocks read answers it. */
function block(id: string, type: string): ArtifactBlock {
  return {
    attributes: [],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

/** The spec's summary as the batched reference read names it: the block's stored excerpt, and
 *  the server's reading of its words. */
function summaryTarget(): ReferenceTarget {
  return {
    gone: false,
    kind: "block",
    name: ":::callout{#summary}\nWhat this is. Why it matters. What is different when done.\n:::",
    ref: buildDispatchReference({
      block: { id: "summary" },
      key: KEY,
      kind: "artifact",
      slug: "spec",
    }),
    text_tree: displayTree("What this is. Why it matters. What is different when done."),
  };
}

/** The issue the drawn-gap case reads: a spec with a summary and one requirement no check
 *  serves, and a decisions document with one decision, as the page's reads answer them. */
function fullyReadIssue(): TreeNode {
  return buildIssueTree({
    coverage: {
      gaps: ["r1 has no check"],
      has_plan: false,
      issue: KEY,
      requirements: [
        {
          block_id: "r1",
          checks: 0,
          gaps: ["r1 has no check"],
          plan_tasks: 0,
          ref: `dispatch://${KEY}/spec#b-r1`,
          text: "The page draws the whole issue in one reading.",
        },
      ],
      spec_artifact_id: specDocument.id,
    },
    decisions: {
      decisions: [decision],
      document: decisionsDocument.id,
      open_questions: [],
    },
    documents: [
      {
        artifactId: specDocument.id,
        blocks: [block("summary", "callout"), block("r1", "requirement")],
      },
    ],
    issue: issueDetails(),
    targets: [summaryTarget()],
  });
}

// R1 and R8: the Contents pane's three sentences are the spec's summary, so they wait on the
// spec's blocks alone. A plan whose blocks read was refused holds nothing they are drawn from,
// and changes nothing in them.
test("the Contents pane draws the three sentences though the plan's blocks could not be read", async () => {
  const plan: Artifact = {
    ...decisionsDocument,
    id: "artifact-plan",
    name: "plan.md",
    slug: "plan",
  };
  const issue: IssueDetails = {
    ...issueDetails(),
    artifacts: [specDocument, decisionsDocument, plan],
    plan_artifact_id: plan.id,
  };
  const route = { key: KEY, kind: "issue" } as const;
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  client.setQueryData(issueCoverageQuery(KEY).queryKey, { requirements: [] } as never);
  client.setQueryData(issueResultsQuery(KEY).queryKey, { checks: [] } as never);
  client.setQueryData(["decisions", KEY], { decisions: [], document: null, open_questions: [] });
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  client.setQueryData(["issue-messages", KEY], []);
  client.setQueryData(artifactBlocksQuery(specDocument.id).queryKey, [block("summary", "callout")]);
  client.setQueryData(artifactBlocksQuery(decisionsDocument.id).queryKey, []);
  // The plan's blocks are the one read left to ask for, and it is refused.
  const blocks = spyOn(api, "getArtifactBlocks").mockRejectedValue(new Error("refused"));
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({
    targets: [summaryTarget()],
  } as never);
  try {
    const view = render(
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <KeymapProvider>
            <MarginProvider>
              <ProvidersSlot issue={issue} route={route}>
                <ContentsSlot issue={issue} route={route} />
              </ProvidersSlot>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
    try {
      await waitFor(() =>
        expect(client.getQueryState(artifactBlocksQuery(plan.id).queryKey)?.status).toBe("error")
      );
      await waitFor(() =>
        expect(
          [
            ...view.container.querySelectorAll(
              '[data-testid="issue-three-things"] li [data-author-text]'
            ),
          ].map((sentence) => sentence.textContent)
        ).toEqual(["What this is.", "Why it matters.", "What is different when done."])
      );
    } finally {
      view.unmount();
    }
  } finally {
    blocks.mockRestore();
    names.mockRestore();
  }
});

/** happy-dom answers every media query against its own 1024 px window. The case below reads
 *  the outline at 1440 px: a `max-width` in px, a `min-width` in rem, and `not all and` either. */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
});

/** The page's own `issue` scope, which every one of the outline's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** Every row the outline drew for `nodeId`, in the order it drew them. */
function rowsFor(container: HTMLElement, nodeId: string): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(nodeRowSelectorOf(nodeId))];
}

// R10, R22 and the person's ruling on helper text: a gap is drawn only where it tells a node
// apart from its siblings and names work somebody owes. Under today's model every requirement
// and every decision lacks an origin, and no issue records who wrote or confirmed its summary,
// so on a fully read issue the requirement, the decision and the head draw none of those three -
// while the requirement still draws the gap that does tell it apart: no check serves it. The
// head is drawn with no `QueryClientProvider` around it, as every case above draws it (R20).
test("a requirement, a decision and the head of a fully read issue draw no gap their whole kind shares", async () => {
  setViewport(1440);
  const root = fullyReadIssue();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <IssueTree
              artifacts={new Map([[decisionsDocument.slug, decisionsDocument]])}
              context={new Set()}
              emptyLine="Nothing here."
              filtering={false}
              focusReveal={false}
              holdsEscape={false}
              isClosed={false}
              onEditing={() => {}}
              reveal={undefined}
              root={root}
              rootRow
            />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const head = render(
    <MemoryRouter>
      <Head look="contents" root={root} summary={specSummary(root, "spec")} />
    </MemoryRouter>
  );
  try {
    const requirement = blockNodeId("spec", "r1");
    const settled = blockNodeId("decisions", "d1");
    await waitFor(() => expect(rowsFor(view.container, requirement).length).toBe(1));
    expect(rowsFor(view.container, settled).length).toBe(1);
    const kinds = (element: Element | undefined) =>
      [...(element?.querySelectorAll("[data-gap]") ?? [])].map((gap) =>
        gap.getAttribute("data-gap-kind")
      );
    expect(kinds(rowsFor(view.container, requirement)[0])).toEqual(["no_check"]);
    expect(kinds(rowsFor(view.container, settled)[0])).toEqual([]);
    expect(head.container.querySelector("li [data-author-text]")?.textContent).toBe(
      "What this is."
    );
    expect(kinds(head.container)).toEqual([]);
    for (const words of ["No source given", "Not confirmed in Dispatch", "No author in Dispatch"]) {
      expect(view.container.textContent).not.toContain(words);
      expect(head.container.textContent).not.toContain(words);
    }
  } finally {
    head.unmount();
    view.unmount();
  }
});
