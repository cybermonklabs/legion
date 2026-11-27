import { type Query, type Result, routeUrl } from "../web/src/api/paths";
import type { IssueTreeRead } from "../web/src/api/types";
import {
  baseUrl,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editArtifact,
  getArtifactBlocks,
  getComment,
  recordCheckResult,
  replyToCommentBlock,
  replyToMessageBlock,
  resolveComment,
  userHeaders,
} from "./api";
import { asPlanner, HEAD_SHA, pause } from "./issue-layout-seed-support";
import { specBlockRef } from "./issue-layout-seed-text";
import { GATES, type SkippedMarginItem } from "./issue-page-margin-gates";
import { addFinding, invokeLens, writeConstraint, writeRisk } from "./issue-page-w";

const PROJECT = "MRG";
const BLOCKS = {
  agentDecision: "d-agent",
  agentStatement: "r-agent",
  check: "c-shared",
  criterion: "k-room",
  decision: "d-alice",
  orphanStatement: "r-orphan",
  pagedStatement: "r-page",
  planTask: "t-margin",
  resolvedStatement: "r-settled",
  statementS1: "r-s1",
  statementS2: "r-s2",
} as const;
const PAGE_CHILDREN = Array.from({ length: 60 }, (_, index) => `r-page-${index + 1}`);
const PAGE_CHILD = "r-page-55";
const UNCITED_TEXT = ["The draft has a paper copy.", "The copy sits in the reading room."];
const ORPHAN_TEXT = "Keep the temporary blue note until someone reads this.";
const CHANGE_BODY = "Change the spacing so a reader can see the quoted words.";
const POST_TEXT = "The sample has room for another sample.";
const MESSAGE_TEXT = "Use a marker when the marker is visible.";
const LENS_TEXT = "The verdict keeps the verdict near its source.";
const RISK_TEXT = "A delay hides the delay from the next reader.";
// The lens on a statement, which Conversation and Graph draw as rows under it: each of its three
// texts holds two words twice, the seed's comment quoting the second match of one and row 2's
// selection the second match of the other.
const ROW_LENS_TEXT = "Each margin note keeps its margin, and each note stays in view.";
const ROW_FINDING_TEXT = "The record shows a line, and the line shows the record.";
const ROW_RISK_TEXT = "A slow reply leaves a slow reader behind the reply.";
const ROW_RISK_WARNING = "The warning comes before the reader asks.";
const ASK_TEXT = "Should a draft stay visible after another draft arrives?";
const PART = "b1";

type MarginTreeQuery = Query<"/api/v1/issues/{key}/tree", "get">;

/** alice's GET of `path`, with display trees enabled. */
export async function readWithTrees<T>(path: string): Promise<T> {
  const response = await fetch(new URL(path, baseUrl), {
    headers: { ...(await userHeaders()), "Dispatch-Trees": "1" },
  });
  if (!response.ok) throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

/** The page's tree read, with display trees enabled. */
export function readMarginTree(key: string, query: MarginTreeQuery = {}): Promise<IssueTreeRead> {
  return readWithTrees<IssueTreeRead>(routeUrl("/api/v1/issues/{key}/tree", { key }, query));
}

/** Comment `id` and its replies, read with `node_anchor=true` (INFRA-1253 R31): its node anchor,
 *  which a comment's write answers with no `node_anchor` (strict `CommentSchema`). */
export function readNodeAnchored(id: string): Promise<Result<"/api/v1/comments/{id}", "get">> {
  return readWithTrees<Result<"/api/v1/comments/{id}", "get">>(
    routeUrl("/api/v1/comments/{id}", { id }, { node_anchor: true })
  );
}

function block(kind: string, id: string, words: string, attrs = ""): string {
  return `:::${kind}{#${id}${attrs}}\n${words}\n:::`;
}

function checkBlock(id: string, serves: string, words: string): string {
  return block(
    "check",
    id,
    words,
    ` serves="${serves}" surface="browser" run="bun run e2e" when="before_merge"`
  );
}

/** Paragraphs, a list and a code span give the item-block reads more than one kind to return. */
function itemBody(first: string): string {
  return `${first}\n\nRead the next line in \`order\`.\n\n- First item\n- Second item`;
}

function onBlock(
  key: string,
  artifact: string,
  block_id: string,
  quote: string,
  body: string,
  occurrence?: number,
  options?: Parameters<typeof createComment>[2]
) {
  return createComment(key, { anchor: { artifact, block_id, occurrence, quote }, body }, options);
}

/** G4: the generated reply bodies do not have these fields yet. */
export interface MarginReplyQuote {
  occurrence: number;
  quote: string;
}

interface Layout {
  agentDecision: string;
  agentStatement: string;
  authorOnly: string;
  check: string;
  child: string;
  childVerificationArtifactId: string;
  constraint: string;
  constraintAddress: string;
  conversationBlock: string;
  criterion: string;
  crossIssueChild: string;
  decision: string;
  key: string;
  message: string;
  messageBlock: string;
  openAsk: string;
  openAskPart: string;
  orphanStatement: string;
  pagedChildren: readonly string[];
  pagedChild55: string;
  pagedStatement: string;
  planArtifactId: string;
  planSlug: string;
  planTask: string;
  post: string;
  postBlock: string;
  postBlockReply: string;
  postReply: string;
  postReplyToReply: string;
  resolvedStatement: string;
  result: string;
  specArtifactId: string;
  specSlug: string;
  statementS1: string;
  statementS2: string;
  treeArtifactId: string;
  uncitedArtifactId: string;
  uncitedBlocks: readonly string[];
  uncitedSlug: string;
  verificationArtifactId: string;
  verificationSlug: string;
}

/** The lens on the comment on the statement (on the statement itself in `seedNoThreads`), which
 *  `seedMarginIssue` draws in that comment's thread (R7), and the lens on the resolved statement
 *  (`row…`), whose verdict, finding and risk Conversation and Graph draw as rows under it: the
 *  node anchors of `GatedThreads` name the second. */
interface ReasoningNodes {
  finding: string;
  findingAddress: string;
  findingBlock: string;
  lens: string;
  lensAddress: string;
  risk: string;
  riskAddress: string;
  rowFindingAddress: string;
  rowFindingBlock: string;
  rowLens: string;
  rowLensAddress: string;
  rowRisk: string;
  rowRiskAddress: string;
  triggerLens: string;
}

interface GatedThreads {
  askQuotedReply: string | null;
  constraintComment: string | null;
  conversationBlockComment: string | null;
  criterionComment: string | null;
  findingComment: string | null;
  lensComment: string | null;
  messageQuotedReply: string | null;
  postQuotedReply: string | null;
  postQuotedReplyFollowup: string | null;
  resultComment: string | null;
  riskComment: string | null;
  skipped: readonly SkippedMarginItem[];
}

export type NoThreadsSeed = Layout & ReasoningNodes;

export interface MarginSeed extends Layout, ReasoningNodes, GatedThreads {
  agentComment: string;
  change: string;
  changeBody: string;
  checkComment: string;
  decisionComment: string;
  orphanedThread: string;
  pagedChildThread: string;
  planTaskComment: string;
  resolvedThread: string;
  statementComment: string;
  statementReply: string;
  threadsS1: readonly string[];
  threadsS2: readonly string[];
  uncitedThreads: readonly string[];
}

async function seedLayout(): Promise<Layout> {
  await createProject({ key: PROJECT, name: "Comments on text" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: [
      block(
        "requirement",
        BLOCKS.statementS1,
        "Keep every thread beside the words that it answers."
      ),
      block("requirement", BLOCKS.pagedStatement, "Open the right page of children."),
      block("requirement", BLOCKS.agentStatement, "Keep the planner's notes in their own threads."),
      block("requirement", BLOCKS.resolvedStatement, "A settled question still has a place."),
      block("requirement", BLOCKS.orphanStatement, ORPHAN_TEXT),
      "## Decisions",
      block(
        "decision",
        BLOCKS.decision,
        "### Leave room to read\n\nKeep the space between cards and the space between rows."
      ),
    ].join("\n\n"),
    title: "Read comments beside their words",
  });
  const { key, primary_artifact_id: specArtifactId } = issue;
  if (!specArtifactId) throw new Error(`${key} has no spec artifact`);
  const ref = (id: string) => specBlockRef(key, id);
  const children = PAGE_CHILDREN.map((id, index) =>
    block(
      "requirement",
      id,
      `Read note ${index + 1} on its own row.`,
      ` serves="${ref(BLOCKS.pagedStatement)}"`
    )
  );
  await editArtifact(specArtifactId, {
    ops: [
      {
        after: `block:${BLOCKS.statementS1}`,
        markdown: block(
          "requirement",
          BLOCKS.statementS2,
          "A reader finds the first note and the last marker in order.",
          ` serves="${ref(BLOCKS.statementS1)}"`
        ),
        op: "insert",
      },
      { after: `block:${BLOCKS.pagedStatement}`, markdown: children.join("\n\n"), op: "insert" },
    ],
  });
  await editArtifact(
    specArtifactId,
    {
      ops: [
        {
          after: "end",
          markdown: block(
            "decision",
            BLOCKS.agentDecision,
            "### Use one margin\n\nDraw the cards with narrow spacing.",
            ` shaped="${ref(BLOCKS.statementS2)}"`
          ),
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  const { artifact: verification } = await createIssueArtifact(key, {
    content: checkBlock(
      BLOCKS.check,
      `${ref(BLOCKS.statementS1)} ${ref(BLOCKS.statementS2)}`,
      "Check the placement before and the placement after a reply."
    ),
    name: "verification.md",
  });
  const { artifact: plan } = await createIssueArtifact(key, {
    content: block(
      "plan_task",
      BLOCKS.planTask,
      "Build the column and keep the column beside the text.",
      ` serves="${ref(BLOCKS.statementS2)}"`
    ),
    name: "plan.md",
  });
  const { result } = await recordCheckResult(
    key,
    {
      check: BLOCKS.check,
      commit: HEAD_SHA,
      negative_control: "Moving the card over the words makes the check fail.",
      observed: "The reading matched the reading from the previous run.",
      role: "tester",
      verdict: "pass",
    },
    asPlanner
  );

  const child = await createIssue({
    force: true,
    parent: key,
    project: PROJECT,
    title: "Keep child issue titles readable",
  });
  // The planner holds the child, so the child's row in Conversation and in Graph names an agent
  // (`ChildIssueFacts`' `AgentName`), which reads the agents list on both seeds alike: the page's
  // own read wherever it names an agent, which a thread an agent wrote would read too (c-reads).
  // The outline names a comment's author with no read (`useAgentText`), and an agent's post on
  // the issue that no person wrote in is no node of the conversation (`refs/conversation.go`).
  await claimIssue(child.key, asPlanner);
  const crossIssueChild = await createIssue({
    force: true,
    parent: key,
    project: PROJECT,
    title: "Read a parent's statement from its child",
  });
  // Part V's seedLinkedChecks writes this serves relation. The child's Spec tree read includes
  // the served ancestor requirement as context (issue_tree_read.go, viewContext).
  const { artifact: childVerification } = await createIssueArtifact(crossIssueChild.key, {
    content: checkBlock("c-below", ref(BLOCKS.statementS1), "Check the statement from the child."),
    name: "verification.md",
  });

  const treeArtifactId = (await readMarginTree(key)).tree;
  if (!treeArtifactId) throw new Error(`${key} was not born converted`);
  const roomWords = "The conversation has a note beside another note.";
  await editArtifact(treeArtifactId, {
    ops: [
      {
        after: "end",
        markdown: block(
          "criterion",
          BLOCKS.criterion,
          "Keep the boundary visible when the boundary wraps."
        ),
        op: "insert",
      },
      { after: "end", markdown: roomWords, op: "insert" },
    ],
  });
  const deadline = Date.now() + 20_000;
  let conversationBlock: string | undefined;
  while (conversationBlock === undefined) {
    const room = await readMarginTree(key, { kind: ["prose"] });
    conversationBlock = room.nodes?.find((node) => node.text.trim() === roomWords)?.block_id;
    if (conversationBlock !== undefined) break;
    if (Date.now() >= deadline)
      throw new Error(`${key}'s tree read did not include the new room prose`);
    await pause(100);
  }
  const { artifact: uncited } = await createIssueArtifact(key, {
    content: `${UNCITED_TEXT.join("\n\n")}\n`,
    name: "background.md",
  });
  const uncitedBlocks = (await getArtifactBlocks(uncited.id)).map(({ id }) => id);
  if (uncitedBlocks.length !== UNCITED_TEXT.length)
    throw new Error("Expected two blocks in background.md");

  const openAsk = await createAsk(key, { question: itemBody(ASK_TEXT), urgency: "med" }, asPlanner);
  const post = await createComment(key, { body: itemBody(POST_TEXT) });
  const postReply = await createComment(key, {
    body: "An answer to the whole post.",
    reply_to: post.id,
  });
  const postReplyToReply = await createComment(key, {
    body: "An answer to the previous reply.",
    reply_to: postReply.id,
  });
  const postBlockReply = await replyToCommentBlock(key, post.id, "b2", {
    body: "An answer to the second paragraph without a quote.",
  });
  const message = await createMessage(key, { body: itemBody(MESSAGE_TEXT) }, asPlanner);
  const authorOnly = await writeConstraint(
    key,
    { reference: "Private notes", standard: "Only the author sees this standard." },
    { visibility: "author" }
  );
  const constraint = await writeConstraint(key, {
    reference: "Reading guide, section 2",
    standard: "The standard applies where the standard is visible.",
  });

  return {
    ...BLOCKS,
    authorOnly: authorOnly.id,
    child: child.key,
    childVerificationArtifactId: childVerification.id,
    constraint: constraint.id,
    constraintAddress: constraint.address,
    conversationBlock,
    crossIssueChild: crossIssueChild.key,
    key,
    message: message.id,
    messageBlock: PART,
    openAsk: openAsk.id,
    openAskPart: PART,
    pagedChildren: PAGE_CHILDREN,
    pagedChild55: PAGE_CHILD,
    planArtifactId: plan.id,
    planSlug: plan.slug,
    post: post.id,
    postBlock: PART,
    postBlockReply: postBlockReply.id,
    postReply: postReply.id,
    postReplyToReply: postReplyToReply.id,
    result: result.id,
    specArtifactId,
    specSlug: "spec",
    treeArtifactId,
    uncitedArtifactId: uncited.id,
    uncitedBlocks,
    uncitedSlug: uncited.slug,
    verificationArtifactId: verification.id,
    verificationSlug: verification.slug,
  };
}

async function seedReasoning(layout: Layout, statementComment?: string): Promise<ReasoningNodes> {
  const { key } = layout;
  const on =
    statementComment === undefined
      ? specBlockRef(key, layout.statementS1)
      : `dispatch://${key}/comment/${statementComment}`;
  const trigger = await invokeLens(key, { kind: "trade-off", on });
  const lens = await invokeLens(key, {
    kind: "pre-mortem",
    on,
    triggered_by: trigger.address,
    verdict: LENS_TEXT,
  });
  const finding = await addFinding(
    key,
    lens.address,
    "A finding explains where the finding came from."
  );
  const risk = await writeRisk(key, {
    early_warning: "The early warning line is not the risk heading.",
    heading: RISK_TEXT,
    lens: lens.address,
    prevention: "Keep the reader's place when more text arrives.",
  });
  // Part W draws a lens under a row only for a node a view draws as a row, so the lens on the
  // comment stands in the comment's thread and a comment on its words stands in no margin; this
  // one stands under a statement, and the comments on its nodes' words stand beside their rows.
  const rowLens = await invokeLens(key, {
    kind: "pre-mortem",
    on: specBlockRef(key, layout.resolvedStatement),
    verdict: ROW_LENS_TEXT,
  });
  const rowFinding = await addFinding(key, rowLens.address, ROW_FINDING_TEXT);
  const rowRisk = await writeRisk(key, {
    early_warning: ROW_RISK_WARNING,
    heading: ROW_RISK_TEXT,
    lens: rowLens.address,
    prevention: "Answer each reply in the order it came.",
  });
  return {
    finding: finding.id,
    findingAddress: finding.address,
    findingBlock: finding.block_id,
    lens: lens.id,
    lensAddress: lens.address,
    risk: risk.id,
    riskAddress: risk.address,
    rowFindingAddress: rowFinding.address,
    rowFindingBlock: rowFinding.block_id,
    rowLens: rowLens.id,
    rowLensAddress: rowLens.address,
    rowRisk: rowRisk.id,
    rowRiskAddress: rowRisk.address,
    triggerLens: trigger.id,
  };
}

async function seedGated(layout: Layout, reasoning: ReasoningNodes): Promise<GatedThreads> {
  const { key } = layout;
  const skipped: SkippedMarginItem[] = [];
  async function gated<T>(item: string, gate: SkippedMarginItem["gate"], write: () => Promise<T>) {
    if (GATES[gate]) return await write();
    skipped.push({ gate, item });
    return null;
  }
  const roomComment = (item: string, id: string, quote: string) =>
    gated(
      item,
      "TREE_ARTIFACT_ANCHOR",
      async () =>
        (await onBlock(key, layout.treeArtifactId, id, quote, `A remark on the room's ${item}.`, 1))
          .id
    );
  // G4: keep the quote fields beside the fields that name the item and its block.
  const postQuote: Parameters<typeof replyToCommentBlock>[3] & MarginReplyQuote = {
    body: "The second sample is the one I meant.",
    occurrence: 1,
    quote: "sample",
  };
  const askQuote: Parameters<typeof createComment>[1] & MarginReplyQuote = {
    ask_id: layout.openAsk,
    body: "The new draft should stay visible.",
    occurrence: 1,
    quote: "draft",
    reply_to_block: layout.openAskPart,
  };
  const messageQuote: Parameters<typeof replyToMessageBlock>[3] & MarginReplyQuote = {
    body: "That marker stays visible even on a phone.",
    occurrence: 1,
    quote: "marker",
  };
  const postQuotedReply = await gated(
    "postQuotedReply",
    "REPLY_QUOTE",
    async () => (await replyToCommentBlock(key, layout.post, layout.postBlock, postQuote)).id
  );
  const postQuotedReplyFollowup = await gated(
    "postQuotedReplyFollowup",
    "REPLY_QUOTE",
    async () =>
      (
        await createComment(key, {
          body: "This answers the reply that quotes the post.",
          reply_to: postQuotedReply,
        })
      ).id
  );
  const askQuotedReply = await gated(
    "askQuotedReply",
    "REPLY_QUOTE",
    async () => (await createComment(key, askQuote)).id
  );
  const messageQuotedReply = await gated(
    "messageQuotedReply",
    "REPLY_QUOTE",
    async () =>
      (await replyToMessageBlock(key, layout.message, layout.messageBlock, messageQuote)).id
  );
  const onNode = (item: string, node: string, quote: string) =>
    gated(
      item,
      "NODE_ANCHOR",
      async () =>
        (
          await createComment(key, {
            anchor: { node, occurrence: 1, quote },
            body: `A remark on the ${item}'s words.`,
          })
        ).id
    );
  return {
    askQuotedReply,
    constraintComment: await onNode("constraint", layout.constraintAddress, "standard"),
    conversationBlockComment: await roomComment(
      "conversationBlock",
      layout.conversationBlock,
      "note"
    ),
    criterionComment: await roomComment("criterion", layout.criterion, "boundary"),
    // The lens's nodes the rows draw, not those of the lens on the comment (`seedReasoning`).
    findingComment: await onNode("finding", reasoning.rowFindingAddress, "record"),
    lensComment: await onNode("lens", reasoning.rowLensAddress, "margin"),
    messageQuotedReply,
    postQuotedReply,
    postQuotedReplyFollowup,
    resultComment: await onNode("result", `result/${layout.result}`, "reading"),
    riskComment: await onNode("risk", reasoning.rowRiskAddress, "slow"),
    skipped,
  };
}

async function removeOrphanWords(layout: Layout) {
  await editArtifact(layout.specArtifactId, {
    ops: [{ find: ORPHAN_TEXT, op: "replace", with: "The note has now been read." }],
  });
}

/** Use on an empty harness database. Block fields are source block IDs, not row IDs. */
export async function seedMarginIssue(): Promise<MarginSeed> {
  const layout = await seedLayout();
  const { key, specArtifactId: spec } = layout;
  const words =
    "Readers need to see the whole reply before deciding whether to answer it. Keep these lines in the card so they can compare the reply with the words it answers.";
  const longBody = [words, words, words].join("\n\n");
  const threadsS1: string[] = [];
  for (const quote of ["thread", "words", "answers"]) {
    threadsS1.push(
      (await onBlock(key, spec, layout.statementS1, quote, `On ${quote}: ${longBody}`)).id
    );
  }
  const [statementComment] = threadsS1;
  const statementReply = await createComment(key, {
    body: "Read this next to the first statement.",
    reply_to: statementComment,
  });
  const threadsS2: string[] = [];
  for (const quote of ["first", "marker"]) {
    threadsS2.push(
      (await onBlock(key, spec, layout.statementS2, quote, `A note about the ${quote}.`)).id
    );
  }
  const agentComment = await onBlock(
    key,
    spec,
    layout.agentStatement,
    "planner's notes",
    "The planner left this for the reader.",
    undefined,
    asPlanner
  );
  const resolvedThread = await onBlock(
    key,
    spec,
    layout.resolvedStatement,
    "settled question",
    "This part is settled."
  );
  await resolveComment(resolvedThread.id);
  const orphanedThread = await onBlock(
    key,
    spec,
    layout.orphanStatement,
    "temporary blue note",
    "These words are about to go away."
  );
  await removeOrphanWords(layout);
  const deadline = Date.now() + 20_000;
  while (!(await getComment(orphanedThread.id)).comment.anchor?.orphaned) {
    if (Date.now() >= deadline)
      throw new Error("The anchor did not become orphaned after the edit");
    await pause(100);
  }
  const checkComment = await onBlock(
    key,
    layout.verificationArtifactId,
    layout.check,
    "placement",
    "Check this once even though it serves two statements.",
    1
  );
  const planTaskComment = await onBlock(
    key,
    layout.planArtifactId,
    layout.planTask,
    "column",
    "Keep the column in view while reading the task.",
    1
  );
  const decisionComment = await onBlock(
    key,
    spec,
    layout.decision,
    "space",
    "Leave enough room between the rows.",
    1
  );
  // Part Q's Change is a comment anchored to a decision's words, not a separate write route.
  const change = await onBlock(key, spec, layout.agentDecision, "narrow spacing", CHANGE_BODY);
  const pagedChildThread = await onBlock(
    key,
    spec,
    layout.pagedChild55,
    "note 55",
    "Open the page that holds this note."
  );
  const uncitedThreads: string[] = [];
  for (const [index, id] of layout.uncitedBlocks.entries()) {
    uncitedThreads.push(
      (
        await onBlock(
          key,
          layout.uncitedArtifactId,
          id,
          UNCITED_TEXT[index],
          `A note on background paragraph ${index + 1}.`
        )
      ).id
    );
  }
  const reasoning = await seedReasoning(layout, statementComment);
  return {
    ...layout,
    ...reasoning,
    ...(await seedGated(layout, reasoning)),
    agentComment: agentComment.id,
    change: change.id,
    changeBody: CHANGE_BODY,
    checkComment: checkComment.id,
    decisionComment: decisionComment.id,
    orphanedThread: orphanedThread.id,
    pagedChildThread: pagedChildThread.id,
    planTaskComment: planTaskComment.id,
    resolvedThread: resolvedThread.id,
    statementComment,
    statementReply: statementReply.id,
    threadsS1,
    threadsS2,
    uncitedThreads,
  };
}

/** Same layout, without comments or replies anchored to text. */
export async function seedNoThreads(): Promise<NoThreadsSeed> {
  const layout = await seedLayout();
  await removeOrphanWords(layout);
  return { ...layout, ...(await seedReasoning(layout)) };
}
