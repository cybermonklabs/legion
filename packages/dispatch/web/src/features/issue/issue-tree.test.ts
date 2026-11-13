import { expect, test } from "bun:test";

import type {
  Artifact,
  ArtifactBlock,
  Ask,
  AskResolution,
  BlockAttributeValue,
  CheckBlock,
  CheckResult,
  Comment,
  Decision,
  DisplayTree,
  IssueCheckResults,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  Message,
  MessageRead,
  RequirementCoverage,
} from "../../api/types";
import { askResolutionBadge } from "../refs/actor";
import { buildIssueTree, FIRST_LINE_LIMIT, firstLineOf, type TreeNode } from "./issue-tree";
import { nodeStateWord } from "./page-words";
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
    design_gates: [],
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

/** The commit the fixture's results ran at, which is also the head their check compares them
 *  with, so each recorded pass stands at what the check compared with. */
const RESULT_COMMIT = "c".repeat(40);

/** One result recorded against a check, with the negative control it recorded; `""` is a result
 *  that recorded none, which is what the page draws the gap for (R8). `extra` is whatever the
 *  case changes about it: the verdict it reached, the retraction that withdrew it, or the words
 *  of its check having moved under it. */
function recorded(
  id: string,
  negativeControl: string,
  extra: Partial<CheckResult> = {}
): CheckResult {
  return {
    actor: { id: "tester", kind: "session" },
    check_artifact_id: "artifact-verification",
    check_block_id: "c1",
    check_issue_key: KEY,
    check_sha256: "sum",
    check_version: 1,
    check_words_moved: false,
    commit_sha: RESULT_COMMIT,
    commit_stands: { answer: "at_basis" },
    created_at: "2026-11-05T00:00:00Z",
    excluded: 0,
    failed: 0,
    id,
    issue_key: KEY,
    negative_control: negativeControl,
    observed: `What result ${id} saw.`,
    role: "",
    run: "",
    screenshots: [],
    session: "session-1",
    skipped: 0,
    surface: "",
    verdict: "pass",
    ...extra,
  };
}

/** One check of the Verification document with the results recorded against it, newest first,
 *  as `GET /api/v1/issues/{key}/results` answers them. `payload` is whatever the case changes
 *  about the check the server sends, including a field it sends no value for at all. */
function checkedResults(
  blocks: readonly {
    blockId: string;
    payload?: Partial<CheckBlock>;
    results: readonly CheckResult[];
  }[]
): IssueCheckResults {
  const checks: CheckBlock[] = blocks.map((one) => ({
    artifact_id: "artifact-verification",
    block_id: one.blockId,
    compared_with: [{ commit: RESULT_COMMIT, kind: "pull_request", pull_request: 7 }],
    issue_key: KEY,
    negative_control: "",
    pass_stands: { answer: "yes", roles: ["tester"] },
    ref: `dispatch://${KEY}/artifact/${VERIFICATION}#b-${one.blockId}`,
    results: [...one.results],
    run: "bun test",
    sha256: "sum",
    surface: "cli",
    text: `Check ${one.blockId} of the redesign.`,
    version: 1,
    when: "before_merge",
    ...one.payload,
  }));
  return { checks, issue: KEY, verification_artifact_id: "artifact-verification" };
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

/** One question asked of the issue, under no document unless `extra` names its block. */
function question(id: string, extra: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: `Question ${id}?`,
    state: "open",
    urgency: "med",
    ...extra,
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

// R4, R8: a node whose spine parent is absent sits under the issue, as a node like any other and
// never as an error. No requirement carries a `from` and no decision a question it settles under
// today's model, so the absence tells no node apart from its siblings and no gap says it: the
// requirement keeps the one gap that does, that no check serves it, and the decision keeps none.
test("a requirement no decision shaped sits under the issue as a node nothing explains", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "Nothing explains this.", ["r1 has no check"])]),
    decisions: decisions([decision("d1", "Shapes nothing.", [])]),
    documents: [{ artifactId: spec.id, blocks: [block("r1", "requirement")] }],
    issue: issueDetails([spec, made]),
  });

  expect(placementsOf(root, blockNodeId(SPEC, "r1")).map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, blockNodeId(SPEC, "r1"))?.gaps).toEqual(["no_check"]);
  const settled = blockNodeId("decisions-md", "d1");
  expect(placementsOf(root, settled).map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, settled)?.gaps).toEqual([]);
});

// R9: a decision the model marks unsure carries the mark its row says in one word, and one the
// model does not mark carries none.
test("a decision the model marks unsure is marked so, and one it does not is not", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const root = buildIssueTree({
    decisions: decisions([
      { ...decision("d1", "Fold to the frontier.", []), unsure: true },
      decision("d2", "Name every state.", []),
    ]),
    issue: issueDetails([spec, made]),
  });

  expect(nodeOf(root, blockNodeId("decisions-md", "d1"))?.unsure).toBe(true);
  expect(nodeOf(root, blockNodeId("decisions-md", "d2"))?.unsure).toBeUndefined();
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

// R21: a typed block is a directive, and the reference read names a block by its stored excerpt,
// which is its source: `:::plan_task{#t1 serves="…" needs="…"}`, its words, and the closing
// fence. The opening line and the fence are markup, so a block's row and every chip naming it
// carry its words alone.
test("a typed block's row and a chip naming it carry its words, not its directive's source", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const verification = artifact("artifact-verification", VERIFICATION, "verification.md");
  const serves = `dispatch://${KEY}/spec#b-r1`;
  const needs = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [block("t0", "plan_task", { serves }), block("t1", "plan_task", { needs, serves })],
      },
      { artifactId: verification.id, blocks: [block("c1", "check", { serves })] },
    ],
    issue: issueDetails([spec, plan, verification]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: `:::plan_task{#t0 serves="${serves}"}\nAdd the routes.\n:::`,
        ref: needs,
      },
      {
        gone: false,
        kind: "block",
        name: `:::plan_task{#t1 serves="${serves}" needs="${needs}"}\nDraw the panel.\n:::`,
        ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
      },
      {
        gone: false,
        kind: "block",
        name: `:::check{#c1 serves="${serves}"}\nThe page loads in one second.\n\nOn a phone too.\n:::`,
        ref: `dispatch://${KEY}/artifact/${VERIFICATION}#b-c1`,
      },
    ],
  });

  expect(nodeOf(root, blockNodeId("plan-md", "t0"))?.text).toBe("Add the routes.");
  const task = nodeOf(root, blockNodeId("plan-md", "t1"));
  expect(task?.text).toBe("Draw the panel.");
  expect(task?.chips.map(({ name }) => name)).toEqual(["Add the routes."]);
  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.text).toBe(
    "The page loads in one second.\n\nOn a phone too."
  );
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
  // The five requirements and nothing else: the document they are blocks of is no node (R3).
  expect(root.held).toBe(5);
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

// R1, R3, R4, R12: a document is a node of the issue and not of its conversation, so the tree
// draws none; every typed block of an issue the conversion has not reached sits under the
// issue, the spec's blocks first and each document's in its own order, and a block the model
// places - a plan task by its `serves` - still sits where it says. A child issue is a node
// under the issue, where the model places every child today.
test("no document is a node, and each typed block and child sits under the issue", () => {
  const notes = artifact("artifact-notes", "notes-md", "notes.md");
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const image: Artifact = { ...artifact("artifact-image", "shot-png", "shot.png"), kind: "image" };
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: notes.id, blocks: [block("n1", "callout")] },
      {
        artifactId: spec.id,
        blocks: [block("summary", "callout"), block("r1", "requirement"), block("q1", "ask")],
      },
      {
        artifactId: plan.id,
        blocks: [
          block("t1", "plan_task", { serves: `dispatch://${KEY}/spec#b-r1` }),
          block("t2", "plan_task"),
        ],
      },
    ],
    // The issue carries its spec second, so a reading that ordered the blocks by their
    // document's place in the list alone would draw the notes' block first.
    issue: {
      ...issueDetails([notes, spec, plan, image]),
      children: [
        {
          active_at: "2026-11-05T00:00:00Z",
          blocked_by: [],
          claim: null,
          external_links: [],
          key: "CORE-2",
          status: "todo",
          subtree_done: 0,
          subtree_total: 0,
          title: "A child.",
        },
      ],
    },
  });

  const kinds = new Set<string>();
  const walk = (node: TreeNode): void => {
    kinds.add(node.type);
    for (const child of node.children) walk(child);
  };
  walk(root);
  for (const kind of ["document", "image", "handoff"]) {
    expect(kinds.has(kind), kind).toBe(false);
  }
  for (const id of [SPEC, "notes-md", "plan-md", "shot-png"]) {
    expect(nodeOf(root, id), id).toBeUndefined();
  }
  expect(root.children.map((child) => child.id)).toEqual([
    blockNodeId(SPEC, "summary"),
    blockNodeId(SPEC, "q1"),
    blockNodeId("notes-md", "n1"),
    blockNodeId("plan-md", "t2"),
    blockNodeId(SPEC, "r1"),
    "CORE-2",
  ]);
  expect(nodeOf(root, "CORE-2")?.type).toBe("child_issue");
  expect(placementsOf(root, blockNodeId("plan-md", "t1")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
});

// The coverage read names a requirement by its words with every `://` word left out
// (`blockWords`, `api/results.go`), so the document a requirement cites is read from the
// block's own source, which the reference read names it by.
test("a requirement whose source cites a document carries its chip, whatever words name it", () => {
  const notes = artifact("artifact-notes", "notes-md", "notes.md");
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const cites = `dispatch://${KEY}/artifact/notes-md`;
  const root = buildIssueTree({
    coverage: coverage([requirement("r-cites", "The measurements behind it are in")]),
    documents: [{ artifactId: spec.id, blocks: [block("r-cites", "requirement")] }],
    issue: issueDetails([notes, spec]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: `:::requirement{#r-cites}\nThe measurements behind it are in ${cites}.\n:::`,
        ref: `dispatch://${KEY}/spec#b-r-cites`,
      },
    ],
  });

  const chips = nodeOf(root, blockNodeId(SPEC, "r-cites"))?.chips ?? [];
  expect(chips.map(({ name, ref }) => ({ name, ref }))).toEqual([{ name: "notes.md", ref: cites }]);
});

// R3: a document is no node, so a block whose words cite one carries a chip that opens it, at
// its own address and named by the document's name - the spec's included, whose `spec`
// reference is the Spec filter's address. A citation of one block of a document is that
// block's, and draws no document chip.
test("a block whose text cites a document carries a chip that opens it", () => {
  const notes = artifact("artifact-notes", "notes-md", "notes.md");
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const root = buildIssueTree({
    coverage: coverage([
      requirement("r1", `The measurements are in dispatch://${KEY}/artifact/notes-md.`),
      requirement("r2", `Read dispatch://${KEY}/spec and dispatch://${KEY}/artifact/notes-md#b-x.`),
      requirement("r3", "Cites nothing."),
    ]),
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("r1", "requirement"),
          block("r2", "requirement"),
          block("r3", "requirement"),
        ],
      },
    ],
    issue: issueDetails([notes, spec]),
  });

  const chipsOf = (id: string) =>
    (nodeOf(root, blockNodeId(SPEC, id))?.chips ?? []).map(({ name, ref }) => ({ name, ref }));
  expect(chipsOf("r1")).toEqual([{ name: "notes.md", ref: `dispatch://${KEY}/artifact/notes-md` }]);
  expect(chipsOf("r2")).toEqual([{ name: "spec.md", ref: `dispatch://${KEY}/artifact/${SPEC}` }]);
  expect(chipsOf("r3")).toEqual([]);
});

// R4: a decision sits under the ask it answers - the asks its blocks mention - edited under the
// first and shown under the others, each placement naming the rest; an ask the tree holds no
// node for stays a chip and places nothing, and a decision that answers no question of the
// issue sits under the issue. The ask a document's block is answers by that block's node.
test("a decision sits under the ask it answers, and one that answers none under the issue", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const own = question("ask-own", { state: "answered" });
  const inSpec = question("ask-block", {
    block_artifact: { id: spec.id, primary: true, slug: SPEC },
    block_id: "q1",
  });
  const root = buildIssueTree({
    asks: [own, inSpec],
    decisions: decisions([
      { ...decision("d1", "Answers the issue's own question.", []), asks: ["ask-own"] },
      { ...decision("d2", "Answers both.", []), asks: ["ask-block", "ask-own"] },
      { ...decision("d3", "Answers a question of another issue.", []), asks: ["ask-elsewhere"] },
      decision("d4", "Answers nothing.", []),
    ]),
    documents: [{ artifactId: spec.id, blocks: [block("q1", "ask")] }],
    issue: issueDetails([spec, made]),
  });

  const parents = (id: string) =>
    placementsOf(root, blockNodeId("decisions-md", id)).map((one) => one.parent);
  expect(parents("d1")).toEqual(["ask-own"]);
  expect(parents("d2")).toEqual([blockNodeId(SPEC, "q1"), "ask-own"]);
  expect(
    placementsOf(root, blockNodeId("decisions-md", "d2")).map((one) => one.node.transcluded)
  ).toEqual([false, true]);
  expect(parents("d3")).toEqual([KEY]);
  expect(parents("d4")).toEqual([KEY]);
  // An ask that places the decision is its spine, so no chip names it; one that places
  // nothing is still named.
  expect(nodeOf(root, blockNodeId("decisions-md", "d1"))?.chips).toEqual([]);
  expect(nodeOf(root, blockNodeId("decisions-md", "d3"))?.chips.map((chip) => chip.ref)).toEqual([
    `dispatch://${KEY}/ask/ask-elsewhere`,
  ]);
});

// R4: a decision is never a requirement's parent, whatever the requirement's own `from` names:
// the requirement sits under the issue instead.
test("a requirement whose `from` names a decision sits under the issue", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first.")]),
    decisions: decisions([decision("d1", "Shapes nothing.", [])]),
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("r1", "requirement", { from: `dispatch://${KEY}/artifact/decisions-md#b-d1` }),
        ],
      },
    ],
    issue: issueDetails([spec, made]),
  });

  expect(placementsOf(root, blockNodeId(SPEC, "r1")).map((one) => one.parent)).toEqual([KEY]);
});

// INFRA-1150 R3, R1: a person's answer is part of the ask and no node of its own. The ask's
// node carries it as the store holds it, who answered, what they chose and what they wrote, and
// the ask holds no child for it.
test("an ask's answer is part of the ask's node and no node of its own", () => {
  const answered = question("ask-1", {
    answer: {
      at: "2026-11-06T00:00:00Z",
      selected: ["1280 px"],
      text: "The preset row keeps its labels.\n\nAbove that width.",
      user: "alice",
    },
    state: "answered",
  });
  const root = buildIssueTree({ asks: [answered], issue: issueDetails([]) });

  const node = nodeOf(root, "ask-1");
  expect(node?.answer).toEqual({
    by: { id: "alice", kind: "user" },
    selected: ["1280 px"],
    text: "The preset row keeps its labels.\n\nAbove that width.",
  });
  expect(node?.children).toEqual([]);
  expect(nodeOf(root, "ask-2")).toBeUndefined();
});

// R9: the issue's own node carries the author the issue read names, so its row draws who
// opened it rather than an author nobody recorded.
test("the issue's own node carries who opened it", () => {
  expect(buildIssueTree({ issue: issueDetails([]) }).author).toEqual({ id: "alice", kind: "user" });
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

// R14: a reply is drawn under the entry it answers. The write climbs `reply_to` to the head of
// the thread before it stores, so a tree parented by `reply_to` draws every reply of a thread
// under its head; `answers` is the comment the write named, which is what nests a reply under
// the reply it answers.
test("a reply is drawn under the entry it answers", () => {
  const head = comment("c0", "2026-11-04T09:00:00Z");
  const first = { ...comment("c1", "2026-11-04T10:00:00Z"), answers: "c0", reply_to: "c0" };
  const second = { ...comment("c2", "2026-11-04T11:00:00Z"), answers: "c1", reply_to: "c0" };
  const root = buildIssueTree({ comments: [head, first, second], issue: issueDetails([]) });

  expect(placementsOf(root, "c2").map((one) => one.parent)).toEqual(["c1"]);
  // The rest of a turn's placement is unchanged: the head sits under the issue and the first
  // reply under the head, so the reply still belongs to its thread.
  expect(placementsOf(root, "c1").map((one) => one.parent)).toEqual(["c0"]);
  expect(placementsOf(root, "c0").map((one) => one.parent)).toEqual([KEY]);
});

// R14: a comment stored before `comments.answers` existed carries no answered entry, and its
// thread's head is then the one entry it names.
test("a reply carrying no answered entry is drawn under its thread's head", () => {
  const head = comment("c0", "2026-11-04T09:00:00Z");
  const older = { ...comment("c1", "2026-11-04T10:00:00Z"), reply_to: "c0" };
  const root = buildIssueTree({ comments: [head, older], issue: issueDetails([]) });

  expect(placementsOf(root, "c1").map((one) => one.parent)).toEqual(["c0"]);
  expect(placementsOf(root, "c0").map((one) => one.parent)).toEqual([KEY]);
});

// R4: the ring is opened at a member of the ring and never at a node hanging off one. A plan
// task serving a requirement of the ring cannot be reached from the issue either, and its
// order - a block's place in its own document - sorts before every requirement's, so the
// lowest order among the drafts the issue cannot reach is the task. Re-rooting it would draw
// the task under the issue, torn off the requirement it serves, and leave the ring to be
// opened all the same.
test("a ring of requirements is opened at a requirement, and what serves one keeps its place", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
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
      {
        artifactId: plan.id,
        blocks: [block("t1", "plan_task", { serves: `dispatch://${KEY}/spec#b-r1` })],
      },
    ],
    issue: issueDetails([spec, plan]),
  });

  expect(placementsOf(root, blockNodeId("plan-md", "t1")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
  expect(placementsOf(root, blockNodeId(SPEC, "r1")).map((one) => one.parent)).toEqual([KEY]);
  expect(placementsOf(root, blockNodeId(SPEC, "r2")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
});

// R21: an ask the server writes `resolved` stands in the state its resolution names, so the
// row says what the card and the margin beside it say. A declined question drawn "Resolved"
// would have the page contradicting itself about the same question.
test("a declined ask is drawn Declined, as the card beside it is", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const declining: AskResolution = {
    actor: { id: "alice", kind: "user" },
    at: "2026-11-06T00:00:00Z",
    kind: "declined",
    reason: "The release owner decides this one.",
  };
  const declined: Ask = {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_artifact: { id: spec.id, primary: true, slug: SPEC },
    block_id: "q1",
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which width does the tree read at?",
    resolution: declining,
    state: "resolved",
    urgency: "med",
  };
  const root = buildIssueTree({
    asks: [declined],
    documents: [{ artifactId: spec.id, blocks: [block("q1", "ask")] }],
    issue: issueDetails([spec]),
  });

  const node = nodeOf(root, blockNodeId(SPEC, "q1"));
  expect(node?.state).toBe("declined");
  expect(nodeStateWord(node?.state ?? "open")).toBe(askResolutionBadge(declining));
});

// R8: a plan task that names no requirement it serves and no declaration it attempts is work
// nobody asked for. A check is held to neither - a check is there to prove a requirement and
// attempts no declaration at all - so the gap is the plan task's alone.
test("a plan task that names no requirement and no declaration draws the gap", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [
          block("t1", "plan_task", { serves: `dispatch://${KEY}/spec#b-r1` }),
          block("t2", "plan_task"),
          // The declarations read of dispatch://INFRA-1278 lands as this pointer, read from
          // the same `block[]` attributes every other pointer is read from.
          block("t3", "plan_task", {
            attempts: `dispatch://${KEY}/artifact/declarations-md#b-d1`,
          }),
        ],
      },
      { artifactId: checks.id, blocks: [block("c1", "check")] },
    ],
    issue: issueDetails([spec, plan, checks]),
  });

  expect(nodeOf(root, blockNodeId("plan-md", "t1"))?.gaps).toEqual([]);
  expect(nodeOf(root, blockNodeId("plan-md", "t2"))?.gaps).toEqual(["nobody_asked"]);
  expect(nodeOf(root, blockNodeId("plan-md", "t3"))?.gaps).toEqual([]);
  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.gaps).toEqual([]);
});

// R10: a `serves` naming a node that is no longer a requirement draws the gap on the node
// holding the citation, and places nothing by it: the check keeps its place under the issue.
test("a check whose citation names no requirement draws the gap, and keeps its place", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = buildIssueTree({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      {
        artifactId: spec.id,
        // `r-was` is the retyped target: the block the citation names is a callout now.
        blocks: [block("r1", "requirement"), block("r-was", "callout")],
      },
      {
        artifactId: checks.id,
        blocks: [
          block("c1", "check", { serves: `dispatch://${KEY}/spec#b-r-was` }),
          block("c2", "check", { serves: `dispatch://${KEY}/spec#b-r1` }),
        ],
      },
    ],
    issue: issueDetails([spec, checks]),
  });

  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.gaps).toEqual(["serves_not_requirement"]);
  expect(placementsOf(root, blockNodeId(VERIFICATION, "c1")).map((one) => one.parent)).toEqual([
    KEY,
  ]);
  // The check whose citation names a requirement draws no gap and sits under it.
  expect(nodeOf(root, blockNodeId(VERIFICATION, "c2"))?.gaps).toEqual([]);
  expect(placementsOf(root, blockNodeId(VERIFICATION, "c2")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
});

// R10: a check is held to no gap about its results. A result that records no negative control
// is one dispatch://INFRA-1253 R3 refuses, so the lack names nothing the page could hold.
test("a check whose latest result records no negative control draws no gap", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = buildIssueTree({
    documents: [{ artifactId: checks.id, blocks: [block("c1", "check")] }],
    issue: issueDetails([spec, checks]),
    results: checkedResults([{ blockId: "c1", results: [recorded("result-latest", "")] }]),
  });

  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.gaps).toEqual([]);
  expect(nodeOf(root, "result-latest")?.gaps).toEqual([]);
});

/** The state the page reads for the one check of a Verification document: what `payload`
 *  changes about the check the server sends, over the results recorded against it. */
function stateOfOneCheck(
  payload: Partial<CheckBlock>,
  results: readonly CheckResult[]
): string | undefined {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = buildIssueTree({
    documents: [{ artifactId: checks.id, blocks: [block("c1", "check")] }],
    issue: issueDetails([spec, checks]),
    results: checkedResults([{ blockId: "c1", payload, results }]),
  });
  return nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.state;
}

/** The mark a retraction leaves on the result it withdrew (`CheckResultRetracted`). */
const WITHDRAWN: NonNullable<CheckResult["retracted"]> = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-11-06T00:00:00Z",
  reason: "The run was against the wrong branch.",
};

// R15: a check whose one pass was withdrawn is not a check that failed, and the page drew both
// as "Failed" - the server answers `no` to a pass standing either way. A reader who cannot tell
// the two apart is told a wrong word, which is worse than no word at all.
test("a check whose only pass was retracted is drawn as retracted, not as failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
    ])
  ).toBe("retracted");
  expect(nodeStateWord("retracted")).toBe("Retracted");
});

test("a check whose result stands and failed is still drawn as failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
    ])
  ).toBe("failed");
  // A withdrawn pass under a fail that stands says nothing about how the check stands: what
  // the reader is owed is the fail.
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
    ])
  ).toBe("failed");
});

// The server counts a retracted result as no evidence at all (`results_comparison.go`'s
// `passStandsOf` skips it), so the page reads how a check stands from the results that still
// stand. Read from the withdrawn newest result instead, a pass taken back at the compared commit
// over an older pass at another commit was drawn "Failed", although nothing failed. The result
// that stands is a pass at work the check no longer compares with, which is what "Out of date"
// says; "Retracted" is for a check that has nothing standing at all.
test("a retracted pass over an older pass at another commit is out of date, not failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
      recorded("result-elsewhere", "Keep the pass and it stands.", {
        commit_sha: "d".repeat(40),
        commit_stands: { answer: "other_commit" },
      }),
    ])
  ).toBe("older");
  expect(nodeStateWord("older")).toBe("Out of date");
});

// The same rule, from the other side: a pass withdrawn after a fail leaves the fail as the
// newest result that stands, and the server's `no` is that fail. Whatever commit the withdrawn
// result ran at, it does not move the check out of date.
test("a retracted pass over an older fail that stands is drawn as failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
    ])
  ).toBe("failed");
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", {
        commit_sha: "d".repeat(40),
        commit_stands: { answer: "other_commit" },
        retracted: WITHDRAWN,
      }),
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
    ])
  ).toBe("failed");
});

// The deployed server sends no `pass_stands` at all, and Dispatch rolls out with two builds
// behind one load balancer, so a page meets a payload without it. Reading it unguarded threw
// inside `buildIssueTree`, which is every preset of the issue page and not the checks alone.
test("a check the server sends no pass_stands for says the page could not read its state", () => {
  expect(stateOfOneCheck({ pass_stands: undefined }, [recorded("result-only", "A control.")])).toBe(
    "unreadable"
  );
  expect(nodeStateWord("unreadable")).toBe("Could not read");
});

/** The server's reading of a text holding `words`, as one paragraph: the shape the reads send
 *  beside a stored text (`DisplayTree`). */
function readingOf(words: string): DisplayTree {
  return {
    content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
    type: "doc",
  };
}

// R20: a read that sends the server's reading of a node's text hands that reading to the node,
// which draws it (`NodeRow.tsx`); the record the text is goes with it, so its references link
// as they do on every other surface. Every other read sends none, and its node holds none: the
// page parses no stored markdown to make one up.
test("a node holds the server's reading of its text where its read sends one", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const question: Ask = {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which **width**?",
    state: "open",
    urgency: "med",
  };
  const message = thread("m1", "2026-11-05T10:00:00Z");
  const root = buildIssueTree({
    asks: [{ ...question, question_tree: readingOf("ask") }],
    comments: [{ ...comment("c1", "2026-11-05T09:00:00Z"), body_tree: readingOf("comment") }],
    coverage: coverage([requirement("r1", "The **first**.")]),
    decisions: {
      ...decisions([{ ...decision("d1", "## Keep it", []), text_tree: readingOf("decision") }]),
      document: "artifact-decisions",
    },
    issue: issueDetails([spec, checks]),
    messages: [{ ...message, message: { ...message.message, body_tree: readingOf("message") } }],
    results: checkedResults([{ blockId: "c1", results: [recorded("res-1", "A control.")] }]),
  });

  const held = (id: string) => {
    const node = nodeOf(root, id);
    return { source: node?.source, tree: node?.tree };
  };
  expect(held("decisions-md#b-d1")).toEqual({
    source: { id: "artifact-decisions", kind: "artifact" },
    tree: readingOf("decision"),
  });
  expect(held("ask-1")).toEqual({ source: { id: "ask-1", kind: "ask" }, tree: readingOf("ask") });
  expect(held("c1")).toEqual({
    source: { id: "c1", kind: "comment" },
    tree: readingOf("comment"),
  });
  expect(held("m1")).toEqual({
    source: { id: "m1", kind: "message" },
    tree: readingOf("message"),
  });
  for (const id of [blockNodeId(SPEC, "r1"), blockNodeId(VERIFICATION, "c1"), "res-1"]) {
    expect({ id, ...held(id) }).toEqual({ id, source: undefined, tree: undefined });
  }
});
