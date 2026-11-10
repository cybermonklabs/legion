import { expect, test } from "bun:test";

import type {
  Artifact,
  ArtifactBlock,
  BlockAttributeValue,
  Comment,
  Decision,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  IssueReferences,
  Message,
  MessageRead,
  RequirementCoverage,
} from "../../api/types";
import { buildIssueTree, FIRST_LINE_LIMIT, firstLineOf, type TreeNode } from "./issue-tree";
import { blockNodeId } from "./tree-drafts";

/**
 * The tree model, read as the page reads it: one call with the page's reads in and the issue's
 * root node out (dispatch://INFRA-1196 R4, `dispatch://INFRA-1196/artifact/plan-md#b-t13`).
 *
 * The `from` edge the placements are read from is not in today's block schema - a `requirement`
 * declares no attributes at all - so the tests that prove the placement hand the model a block
 * carrying one, which is exactly the shape the chain read will answer: the day it lands the reads
 * change and this module does not.
 */

const KEY = "CORE-1";
const SPEC = "spec";
const VERIFICATION = "verification-md";

function artifact(id: string, slug: string, name: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

function issueDetails(artifacts: readonly Artifact[]): IssueDetails {
  return {
    artifacts: [...artifacts],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: KEY,
    labels: [],
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    decisions_artifact_id: "",
    primary_artifact_id: artifacts.find((one) => one.primary)?.id ?? "",
    verification_artifact_id: "",
    priority: null,
    project: "CORE",
    progress: { children: null, tasks: null },
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    holder_state: null,
    last_seen: null,
    takeable_at: null,
    status: "todo",
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
  };
}

/** One block of a document, with whatever `block[]` pointers the case needs. */
function block(
  id: string,
  type: string,
  pointers: Readonly<Record<string, string>> = {}
): ArtifactBlock {
  const attributes: BlockAttributeValue[] = Object.entries(pointers).map(([name, value]) => ({
    kind: "block[]",
    name,
    value,
  }));
  return {
    attributes,
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

function requirement(id: string, text: string, gaps: readonly string[] = []): RequirementCoverage {
  return {
    block_id: id,
    checks: 0,
    gaps: [...gaps],
    plan_tasks: 0,
    ref: `dispatch://${KEY}/spec#b-${id}`,
    text,
  };
}

function coverage(requirements: readonly RequirementCoverage[]): IssueCoverage {
  return {
    gaps: requirements.flatMap((one) => one.gaps ?? []),
    has_plan: false,
    issue: KEY,
    requirements: [...requirements],
    spec_artifact_id: "artifact-spec",
  };
}

/** One decision of `decisions.md` whose blocks link `shaped`, which is the `shaped` edge as
 *  today's read carries it: a link from the decision's block to a block of the spec. */
function decision(blockId: string, text: string, shaped: readonly string[]): Decision {
  return {
    asks: [],
    block_id: blockId,
    changed_at: null,
    changed_version: 1,
    heading: text,
    kept: null,
    kind: "agent",
    links: shaped.map((one) => ({
      created_at: "2026-11-05T00:00:00Z",
      created_version: 1,
      from_block: blockId,
      stale: false,
      to: {
        block_id: one,
        changed_at: null,
        changed_by: null,
        id: "artifact-spec",
        kind: "block",
        made_at: null,
        made_by: null,
        parent: null,
        parent_version: null,
        ref: `dispatch://${KEY}/spec#b-${one}`,
      },
    })),
    ref: `dispatch://${KEY}/artifact/decisions-md#b-${blockId}`,
    replaced: false,
    text,
    text_sha256: "sum",
    unsure: false,
    waiting_review: false,
  };
}

function decisions(made: readonly Decision[]): IssueDecisions {
  return { decisions: [...made], document: "artifact-decisions", open_questions: [] };
}

/** The node `id` under `root`, wherever it sits; the first placement of a node drawn twice. */
function nodeOf(root: TreeNode, id: string): TreeNode | undefined {
  if (root.id === id) return root;
  for (const child of root.children) {
    const found = nodeOf(child, id);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** Every placement of `id`, with the node each one sits under. */
function placementsOf(root: TreeNode, id: string): { parent: string; node: TreeNode }[] {
  const found: { parent: string; node: TreeNode }[] = [];
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (child.id === id) found.push({ node: child, parent: node.id });
      walk(child);
    }
  };
  walk(root);
  return found;
}

/** How many levels the deepest branch of `node` reaches, the node itself being one. */
function depthOf(node: TreeNode): number {
  return 1 + Math.max(0, ...node.children.map(depthOf));
}

type ReferenceMember = NonNullable<IssueReferences["members"]>[number];

/** One member of the references read: a document the issue reaches, named by the document that
 *  cites it and the blocks of that document the link stands in (`?own=1`). */
function member(cited: Artifact, via: Artifact, blocks: readonly string[]): ReferenceMember {
  return {
    artifact: cited,
    depth: 1,
    via: { id: via.id, kind: "artifact" },
    via_blocks: [...blocks],
  };
}

function comment(id: string, createdAt: string): Comment {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: `Comment ${id}.`,
    created_at: createdAt,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: KEY,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

function thread(id: string, createdAt: string): MessageRead {
  const message: Message = {
    author: { id: "bob", kind: "user" },
    body: `Message ${id}.`,
    broadcast_id: null,
    created_at: createdAt,
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: KEY,
    reply_block: null,
    target: null,
  };
  return { message, replies: [] };
}

// R4: a check serves every requirement its `serves` names, edited under the first and
// transcluded under the rest, and each placement names the other requirement it sits under.
test("a check that serves two requirements is drawn under both", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first."), requirement("r2", "The second.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement"), block("r2", "requirement")] },
      {
        artifactId: checks.id,
        blocks: [
          block("c1", "check", {
            serves: `dispatch://${KEY}/spec#b-r1 dispatch://${KEY}/spec#b-r2`,
          }),
        ],
      },
    ],
    issue: issueDetails([spec, checks]),
  });

  const drawn = placementsOf(root, blockNodeId(VERIFICATION, "c1"));
  expect(drawn.map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
    blockNodeId(SPEC, "r2"),
  ]);
  expect(drawn.map((one) => one.node.transcluded)).toEqual([false, true]);
  expect(drawn[0]?.node.alsoUnder.map((chip) => chip.ref)).toEqual([`dispatch://${KEY}/spec#b-r2`]);
  expect(drawn[1]?.node.alsoUnder.map((chip) => chip.ref)).toEqual([`dispatch://${KEY}/spec#b-r1`]);
  // One node, two placements: both open the same block, so both carry the same text.
  expect(drawn[0]?.node.text).toBe(drawn[1]?.node.text ?? "");
});

// R4: a node whose spine parent is absent sits under the issue, drawn as a node nothing explains
// and never as an error, and a requirement no decision shaped draws no disagreement.
test("a requirement no decision shaped sits under the issue as a node nothing explains", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "Nothing explains this.", ["r1 has no check"])]),
    decisions: decisions([decision("d1", "Shapes nothing.", [])]),
    documents: [{ artifactId: spec.id, blocks: [block("r1", "requirement")] }],
    issue: issueDetails([spec]),
  });

  expect(placementsOf(root, blockNodeId(SPEC, "r1")).map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, blockNodeId(SPEC, "r1"))?.gaps).toEqual(["no_origin", "no_check"]);
});

// R4: where a requirement's `from` and a decision's `shaped` disagree, the page places the
// requirement by its `from` and draws the disagreement as a gap, never under the decision.
test("a `from` and a `shaped` that disagree place by `from` and draw the gap", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const root = buildIssueTree({
    coverage: coverage([
      requirement("r-origin", "The requirement the other one came from."),
      requirement("r-shaped", "The requirement the decision claims."),
    ]),
    decisions: decisions([decision("d1", "Shapes the second.", ["r-shaped"])]),
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("r-origin", "requirement"),
          block("r-shaped", "requirement", { from: `dispatch://${KEY}/spec#b-r-origin` }),
        ],
      },
      { artifactId: made.id, blocks: [block("d1", "decision")] },
    ],
    issue: issueDetails([spec, made]),
  });

  expect(placementsOf(root, blockNodeId(SPEC, "r-shaped")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r-origin"),
  ]);
  expect(nodeOf(root, blockNodeId(SPEC, "r-shaped"))?.gaps).toEqual(["shaped_disagreement"]);
  // The decision names what it says it shaped, as a chip, and places nothing by it.
  expect(nodeOf(root, blockNodeId("decisions-md", "d1"))?.chips.map((chip) => chip.ref)).toEqual([
    `dispatch://${KEY}/spec#b-r-shaped`,
  ]);
});

// R4: every edge that is not the spine is a chip naming its target; a target nothing answers to
// now is drawn as gone, with no name, rather than dropped.
test("a pointer whose target is gone is drawn as a chip that says so", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const gone = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [block("t1", "plan_task", { needs: gone, serves: `dispatch://${KEY}/spec#b-r1` })],
      },
    ],
    issue: issueDetails([spec, plan]),
    targets: [
      { gone: true, kind: "block", name: "", ref: gone },
      {
        gone: false,
        kind: "block",
        name: "Task 1 of the redesign is one step of the build.",
        ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
      },
    ],
  });

  const task = nodeOf(root, blockNodeId("plan-md", "t1"));
  expect(task?.text).toBe("Task 1 of the redesign is one step of the build.");
  expect(task?.chips).toEqual([{ answered: true, gone: true, name: "", ref: gone }]);
});

// R4, R20: the model takes any depth. Today's data reaches two levels below the issue, because
// `document_blocks` holds no parent; a chain of `from` edges is drawn however long it is.
test("a tree of one level and a tree of six are both drawn", () => {
  const alone = buildIssueTree({ issue: issueDetails([]) });
  expect(depthOf(alone)).toBe(1);
  expect(alone.children).toEqual([]);
  expect(alone.held).toBe(0);

  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const deep = [1, 2, 3, 4, 5];
  const root = buildIssueTree({
    coverage: coverage(deep.map((at) => requirement(`r${at}`, `Requirement ${at}.`))),
    documents: [
      {
        artifactId: spec.id,
        blocks: deep.map((at) =>
          at === 1
            ? block("r1", "requirement")
            : block(`r${at}`, "requirement", { from: `dispatch://${KEY}/spec#b-r${at - 1}` })
        ),
      },
    ],
    issue: issueDetails([spec]),
  });

  expect(depthOf(root)).toBe(6);
  // The five requirements and the document they are blocks of, which is a node of the tree too.
  expect(root.held).toBe(6);
  expect(nodeOf(root, blockNodeId(SPEC, "r5"))?.children).toEqual([]);
  expect(placementsOf(root, blockNodeId(SPEC, "r5")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r4"),
  ]);
});

// R1: a row's first line is the node's own text to the first sentence end or 100 characters,
// whichever comes first, cut at a word where the limit falls near one and at the limit itself
// where it does not.
test("a first line ends at a sentence, at a word, or at the limit where there is no word", () => {
  expect(firstLineOf("One sentence. And another.")).toEqual({
    cut: true,
    line: "One sentence.",
  });
  const long =
    "Every property of the issue that carries a value is visible at every width the page draws, " +
    "with no control opened and no sideways scroll. The right-hand panel holds the rest.";
  const first = firstLineOf(long);
  expect(first.cut).toBe(true);
  expect(first.line.length).toBeLessThanOrEqual(100);
  expect(long.startsWith(first.line)).toBe(true);
  expect(long.charAt(first.line.length)).toBe(" ");
  expect(firstLineOf("Short enough.")).toEqual({ cut: false, line: "Short enough." });
  expect(firstLineOf("")).toEqual({ cut: false, line: "" });

  // An unbroken token past the limit holds no word boundary to cut at, so the line is the
  // limit's own 100 characters rather than the whole token.
  const unbroken = "a".repeat(FIRST_LINE_LIMIT + 20);
  expect(firstLineOf(unbroken)).toEqual({ cut: true, line: "a".repeat(FIRST_LINE_LIMIT) });
  // A space at the limit's own edge is a word boundary, and the line ends before it.
  const atTheEdge = `${"a".repeat(FIRST_LINE_LIMIT)} tail`;
  expect(firstLineOf(atTheEdge)).toEqual({ cut: true, line: "a".repeat(FIRST_LINE_LIMIT) });
});

// R1, R16: a spec that cites the plan while the plan cites the spec is ordinary data, and the
// references read answers each one via the other. Both documents are drawn, with every block
// they hold: a ring hanging off nothing would be lost from the tree entirely.
test("two documents that cite each other are both drawn, with what they hold", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const root = buildIssueTree({
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement"), block("q1", "ask")] },
      { artifactId: plan.id, blocks: [block("t1", "plan_task")] },
    ],
    issue: issueDetails([spec, plan]),
    references: {
      members: [member(plan, spec, ["r1"]), member(spec, plan, ["t1"])],
      truncated: false,
    },
  });

  // The spec is the issue's first document, so the citation it holds stands and the one that
  // would put it back inside the plan is the citation the ring is opened at.
  expect(placementsOf(root, SPEC).map((one) => one.parent)).toEqual([KEY]);
  expect(placementsOf(root, "plan-md").map((one) => one.parent)).toEqual([blockNodeId(SPEC, "r1")]);
  // Nothing the documents hold is lost with them: the question inside the spec is a row of the
  // tree, which is what the fold opens the path to (R6).
  expect(placementsOf(root, blockNodeId(SPEC, "q1")).map((one) => one.parent)).toEqual([SPEC]);
  expect(placementsOf(root, blockNodeId("plan-md", "t1")).map((one) => one.parent)).toEqual([
    "plan-md",
  ]);
});

// R4: a `from` edge that points in a circle is the same case one level down, and `assemble`
// answers it the same way: the first node of the ring comes home to the issue, so no node of
// it is lost whatever the documents say.
test("requirements whose `from` edges point in a circle are drawn under the issue", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first."), requirement("r2", "The second.")]),
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("r1", "requirement", { from: `dispatch://${KEY}/spec#b-r2` }),
          block("r2", "requirement", { from: `dispatch://${KEY}/spec#b-r1` }),
        ],
      },
    ],
    issue: issueDetails([spec]),
  });

  expect(placementsOf(root, blockNodeId(SPEC, "r1")).map((one) => one.parent)).toEqual([KEY]);
  expect(placementsOf(root, blockNodeId(SPEC, "r2")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
});

// R16: a slug is unique inside one issue only, so a member is this issue's document by its
// artifact id. Another issue's `plan-md` cited from the spec places nothing here, and it does
// not stop the member that names this issue's own `plan-md` either.
test("a document of another issue with the same slug places nothing", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const foreign: Artifact = {
    ...artifact("artifact-other-plan", "plan-md", "plan.md"),
    issue_key: "OTHER-1",
    project: "OTHER",
  };
  const root = buildIssueTree({
    documents: [
      {
        artifactId: spec.id,
        // Spans of their own, since the row a citation is drawn on is the listed block whose
        // span covers the citing one's (`citingRow`).
        blocks: [
          { ...block("r1", "requirement"), from: 0, to: 10 },
          { ...block("r2", "requirement"), from: 10, to: 20 },
        ],
      },
      { artifactId: plan.id, blocks: [] },
    ],
    issue: issueDetails([spec, plan]),
    references: {
      // The foreign document is answered first, as the read orders its members by depth.
      members: [member(foreign, spec, ["r1"]), member(plan, spec, ["r2"])],
      truncated: false,
    },
  });

  expect(placementsOf(root, "plan-md").map((one) => one.parent)).toEqual([blockNodeId(SPEC, "r2")]);
  expect(nodeOf(root, blockNodeId(SPEC, "r1"))?.children).toEqual([]);
});

// R1: the conversation is one order. The comments and the messages are two reads, each
// counting from zero, so a message sent today drawn among yesterday's comments would be the
// reader reading the conversation out of order.
test("the conversation's turns are drawn by the time each was made", () => {
  const root = buildIssueTree({
    comments: [
      comment("c0", "2026-11-04T09:00:00Z"),
      comment("c1", "2026-11-04T10:00:00Z"),
      comment("c2", "2026-11-04T11:00:00Z"),
    ],
    issue: issueDetails([]),
    // An id that sorts before every comment's, so a tie broken by id would draw it first.
    messages: [thread("a-message", "2026-11-05T09:00:00Z")],
  });

  expect(root.children.map((child) => child.id)).toEqual(["c0", "c1", "c2", "a-message"]);
});
