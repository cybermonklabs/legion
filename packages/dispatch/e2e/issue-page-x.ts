import { existsSync } from "node:fs";
import { expect, type Locator, type Page, test } from "@playwright/test";

import type { IssueTreeAnswer, IssueTreeNode } from "../web/src/api/client";
import { type Body, type Result, routeUrl } from "../web/src/api/paths";
import type { Preset } from "../web/src/features/issue-page/page-words";
import { checkStanding } from "../web/src/features/issue-page/tree-drafts";
import type { TreeIdReads } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { buildDispatchReference } from "../web/src/features/refs/routes";
import {
  baseUrlOf,
  createIssue,
  createProject,
  editArtifact,
  getIssue,
  getIssueCoverage,
  getIssueDecisions,
  getIssueResults,
  listIssueAsks,
  recordCheckResult,
  userHeaders,
} from "./api";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { asPlanner, HEAD_SHA, pause, recorderActor } from "./issue-layout-seed-support";
import { OPEN_QUESTION_BLOCK_ID, requirementText } from "./issue-layout-seed-text";
import { issuePage, presetCount, presetRow } from "./issue-page";
import type { Gate } from "./issue-page-t";
import {
  addFinding,
  type ConstraintRead,
  invokeLens,
  type LensFinding,
  type LensInvoked,
  type RiskWritten,
  writeConstraint,
  writeRisk,
} from "./issue-page-w";
import { expandedState } from "./settled";
import type { ApiRequest } from "./traffic";

/**
 * What the rows of `e2e/issue-page-x.e2e.ts` write and read: a node's life - proposed, settled
 * or overtaken - and a node reserved for its author, and the marks, the overtaken lines, the
 * counts and the Settle act the new issue page draws for them
 * (`web/src/features/issue-page/node-states/`). It reuses the readers and writers the other
 * `e2e/issue-page-*.ts` files export, and none of them changes for it. It holds its own copies of
 * two that `e2e/issue-page-w.ts` keeps private: `treeReadAs` repeats the page-and-merge loop of
 * its `requestedTreeRead`, and `postAs` mirrors its `write`.
 *
 * - **The writers** `writeNodeState` and `settleNodeApi` send one request each, as a person,
 *   through the server's node state routes (`packages/envoy/internal/dispatch/api/node_state.go`):
 *   the state route takes `{ "state": … }` and the settle route no body, and each names the node
 *   by its whole address, percent-encoded as one path segment (`routeUrl`). Each asserts the
 *   answer it expects: a 200 that leaves the node at the state asked for.
 * - **The seeds**: `seedGateIssue`, one issue of a project of its own that a row reads its gates
 *   on before it writes anything else; and `seedNodeStates` (proposed, settled and overtaken
 *   nodes) and `seedAuthorOnly` (nodes reserved for their author), each on an issue
 *   `seedWholeIssue` made. `seedNodeStates` names each node by the address the tree read answers
 *   alice for it, so it needs the tree read served.
 * - **The readers**: the marks a node carries where the page draws it (`[data-node-mark]`, its
 *   word and its shape), the line a parent's overtaken children fold to (`[data-overtaken-line]`)
 *   among the outline's rows, and the counts the page draws - each folded branch's
 *   (`data-folded-count`), each filter control's (`data-preset-count`) and its flag
 *   (`data-preset-flag`), the Verification summary's "N of M covered" and Details' "Referenced
 *   by" - beside the same counts made from a page's own tree read (`heldUnder`,
 *   `reservedUnder`, `referencesTo`), with `viewCount` of `e2e/issue-page-t.ts` for a view's.
 * - **What the page sent**: the requests to the node state routes (`nodeStateRequests`) and the
 *   address each settle names (`settlesSent`).
 * - **Details' facts of the focused row** (`FocusedNodeFacts.tsx`), with their "Referenced by"
 *   list: the rows read them only where this tree holds that file (`FOCUSED_FACTS_GATE`), and
 *   note the reading they leave out otherwise (`noteOwed`).
 */

// ---------------------------------------------------------------------------------------------
// The writers.
// ---------------------------------------------------------------------------------------------

/** What `POST /api/v1/issues/{key}/nodes/{id}/state` and `/settle` answer: the node as the write
 *  leaves it, and the state it held before. */
export type NodeStateWritten = Result<"/api/v1/issues/{key}/nodes/{id}/state", "post">;

/** A value the state route writes: `proposed` and `overtaken` on any node, `live` on one that is
 *  not gone, `gone` on a lens, a finding, a risk or a constraint. */
export type WrittenState = Body<"/api/v1/issues/{key}/nodes/{id}/state", "post">["state"];

/** One `POST` of `path` as `login`, with `body` as JSON where there is one, under an
 *  `Idempotency-Key` of its own, asserted to answer 200; answers its body. */
async function postAs(path: string, login: string, body?: object): Promise<NodeStateWritten> {
  const response = await fetch(new URL(path, baseUrlOf()), {
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    headers: {
      ...(await userHeaders(login)),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      "Idempotency-Key": crypto.randomUUID(),
    },
    method: "POST",
  });
  const text = await response.text();
  expect(response.status, `POST ${path} as ${login} answers ${response.status}: ${text}`).toBe(200);
  return JSON.parse(text) as NodeStateWritten;
}

/** Writes the state of the node of issue `key` that `address` names (its whole address) as
 *  `state`, as `login`, and asserts the write left it there. */
export async function writeNodeState(
  key: string,
  address: string,
  state: WrittenState,
  login = "alice"
): Promise<NodeStateWritten> {
  const path = routeUrl("/api/v1/issues/{key}/nodes/{id}/state", { id: address, key });
  const written = await postAs(path, login, { state });
  expect(written.node.state, `the write of ${state} on ${address} left it otherwise`).toBe(state);
  return written;
}

/** Settles the proposed node of issue `key` that `address` names, as `login`, with no body, and
 *  asserts the settle left it settled. */
export async function settleNodeApi(
  key: string,
  address: string,
  login = "alice"
): Promise<NodeStateWritten> {
  const path = routeUrl("/api/v1/issues/{key}/nodes/{id}/settle", { id: address, key });
  const written = await postAs(path, login);
  expect(written.node.state, `the settle of ${address} as ${login} left it otherwise`).toBe(
    "settled"
  );
  return written;
}

// ---------------------------------------------------------------------------------------------
// The tree read, as one reader gets it.
// ---------------------------------------------------------------------------------------------

/** Issue `key`'s tree read with no view as `login` gets it, every page of it in order, merged
 *  into one answer. */
export async function treeReadAs(key: string, login = "alice"): Promise<IssueTreeAnswer> {
  const headers = await userHeaders(login);
  const pageAt = async (cursor: string | null): Promise<IssueTreeAnswer> => {
    const path = routeUrl(
      "/api/v1/issues/{key}/tree",
      { key },
      cursor === null ? undefined : { cursor }
    );
    const response = await fetch(new URL(path, baseUrlOf()), { headers });
    if (!response.ok) {
      throw new Error(
        `GET ${path} as ${login} failed: ${response.status} ${await response.text()}`
      );
    }
    return (await response.json()) as IssueTreeAnswer;
  };
  const first = await pageAt(null);
  const pages = [first];
  for (let cursor = first.cursor; cursor !== null; ) {
    const next = await pageAt(cursor);
    pages.push(next);
    cursor = next.cursor;
  }
  return {
    ...first,
    cursor: null,
    nodes: pages.flatMap((one) => one.nodes),
    tuples: pages.flatMap((one) => one.tuples),
  };
}

/** The reads `treeIdOf` names a node by - the issue read and the asks read - as `login` reads
 *  them, so a row names each node by the id the page draws it under. */
export async function treeIdReads(key: string, login = "alice"): Promise<TreeIdReads> {
  const [issue, asks] = await Promise.all([
    getIssue(key, { login }),
    listIssueAsks(key, { login }),
  ]);
  return { asks, issue };
}

/** Whether `node` is a node of issue `key` itself that the read did not bring in as context. */
function ownOf(node: IssueTreeNode, key: string): boolean {
  return (node.issue === undefined || node.issue === key) && node.mark !== "context";
}

/** The node of issue `key` of kind `supertag` whose read id is `id`, which the seed named. Throws,
 *  naming `what`, where the answer holds none. */
export function seededNode(
  answer: IssueTreeAnswer,
  key: string,
  supertag: string,
  id: string,
  what: string
): IssueTreeNode {
  const node = answer.nodes.find(
    (one) => one.id === id && one.supertag === supertag && ownOf(one, key)
  );
  if (node === undefined) throw new Error(`the tree read of ${key} holds no ${what} (${id})`);
  return node;
}

/** The node's whole address, as the tree read gives it: a typed block's in the document its
 *  `document` names (as `tree-read/tree-nodes.ts`' `referenceOf` addresses one). */
export function addressIn(node: IssueTreeNode, key: string): string {
  if (node.document === undefined) {
    throw new Error(`the tree read names no document for ${node.supertag} ${node.id}`);
  }
  return buildDispatchReference({
    block: { id: node.id },
    key: node.issue ?? key,
    kind: "artifact",
    slug: node.document.slug,
  });
}

/** The first node of `answer` whose words or attributes hold `word`: one of the words a seed
 *  wrote into one node and no other. */
export function nodeHolding(answer: IssueTreeAnswer, word: string): IssueTreeNode | undefined {
  return answer.nodes.find(
    (node) =>
      node.text.includes(word) ||
      Object.values(node.attributes).some(
        (value) => typeof value === "string" && value.includes(word)
      )
  );
}

/** A tree read's nodes by the read `parent` each stands under. */
type Children = ReadonlyMap<string, readonly IssueTreeNode[]>;

/** Each tree read's `Children`, made once per answer. */
const childrenByAnswer = new WeakMap<IssueTreeAnswer, Children>();

/** The `Children` of `answer`, made at its first reading. */
function childrenIn(answer: IssueTreeAnswer): Children {
  const made = childrenByAnswer.get(answer);
  if (made !== undefined) return made;
  const children = new Map<string, IssueTreeNode[]>();
  for (const node of answer.nodes) {
    if (node.parent === null) continue;
    const siblings = children.get(node.parent);
    if (siblings === undefined) children.set(node.parent, [node]);
    else siblings.push(node);
  }
  childrenByAnswer.set(answer, children);
  return children;
}

/** Every node of issue `key` in `answer` under the node `readId`, at any depth, by the read's
 *  `parent`: the tree each count walks. */
function underIn(answer: IssueTreeAnswer, key: string, readId: string): IssueTreeNode[] {
  const children = childrenIn(answer);
  const found: IssueTreeNode[] = [];
  const seen = new Set<string>([readId]);
  const walk = (id: string): void => {
    for (const child of children.get(id) ?? []) {
      if (!ownOf(child, key) || seen.has(child.id)) continue;
      seen.add(child.id);
      found.push(child);
      walk(child.id);
    }
  };
  walk(readId);
  return found;
}

/** How many nodes the branch of `readId` holds in `answer`: what its row's folded count counts. */
export function heldUnder(answer: IssueTreeAnswer, key: string, readId: string): number {
  return underIn(answer, key, readId).length;
}

/** How many of the nodes `reserved` names stand in the branch of `readId` in `answer`. */
export function reservedUnder(
  answer: IssueTreeAnswer,
  key: string,
  readId: string,
  reserved: ReadonlySet<string>
): number {
  return underIn(answer, key, readId).filter((node) => reserved.has(node.id)).length;
}

/** How many tuples of `answer` reach the node `readId` from a node the read holds: what a
 *  reader of that read sees reference it. */
export function referencesTo(answer: IssueTreeAnswer, readId: string): number {
  const held = new Set(answer.nodes.map((node) => node.id));
  return answer.tuples.filter((tuple) => tuple.to === readId && held.has(tuple.from)).length;
}

// ---------------------------------------------------------------------------------------------
// The issue a row reads its gates on.
// ---------------------------------------------------------------------------------------------

/** The project of the issue a row reads its gates on, which no seed writes into. */
const GATE_PROJECT = "ACME";

/** Writes one issue in a project of its own and answers its key: a row reads its gates on it -
 *  whether the tree read is served, and whether an issue is born converted - before it writes
 *  its own issue, so a row that skips on a gate writes nothing it does not read. */
export async function seedGateIssue(): Promise<string> {
  await createProject({ key: GATE_PROJECT, name: "Gates" });
  const issue = await createIssue({
    force: true,
    project: GATE_PROJECT,
    title: "The tree read is served for this issue",
  });
  return issue.key;
}

// ---------------------------------------------------------------------------------------------
// The seed of the proposed, settled and overtaken nodes.
// ---------------------------------------------------------------------------------------------

/** Statement 9's words once `seedNodeStates` wrote them: they hold the word "proposed", while the
 *  node stays live, so a page that reads a node's life from its words draws a mark on statement 9
 *  that its state does not give it. */
export const STATEMENT_9_WORDS =
  "Requirement 9 is proposed in its words alone, and its state is live.";

/** The block id of the agent's decision that shapes statement 7, which `seedNodeStates` writes
 *  into the spec: the pointer route takes no `shaped` tuple, so the decision is a block of the
 *  spec's `## Decisions` part carrying its `shaped` pointer. */
export const SHAPING_DECISION_ID = "d-shapes-r7";

/** What that decision says. */
const SHAPING_DECISION_WORDS = "Settle statement 7 once a person has read the proposal.";

/** The verdict of the lens `seedNodeStates` invokes on statement 8, by which the read names it. */
const STATEMENT_8_VERDICT = "Statement 8 says what a later statement says better.";

/** The nodes `seedNodeStates` names, each by its read id. */
export interface NodeStateIds {
  /** The second check, failed at the seed's head, which is proposed. */
  readonly check2: string;
  /** The fourth check, failing at the seed's head, which is overtaken. */
  readonly check4: string;
  /** The first decision of the spec's `## Decisions` part, which is proposed. */
  readonly decision1: string;
  /** The lens on statement 8. */
  readonly lens: string;
  readonly planTask1: string;
  readonly planTask2: string;
  /** The agent's decision that shapes statement 7 (`SHAPING_DECISION_ID`), which stays live. */
  readonly shapingDecision: string;
  readonly statement7: string;
  readonly statement8: string;
  readonly statement9: string;
  /** An agent's decision that waits for review, which is overtaken. */
  readonly waitingDecision: string;
}

/** The nodes whose state `seedNodeStates` writes, by the state each is written at. */
export const PROPOSED_NODES = ["statement7", "check2", "decision1", "planTask1"] as const;
export const OVERTAKEN_NODES = ["statement8", "planTask2", "check4", "waitingDecision"] as const;

/** One node `seedNodeStates` writes the state of. */
type StatedNode = (typeof PROPOSED_NODES)[number] | (typeof OVERTAKEN_NODES)[number];

/** What `seedNodeStates` wrote on the issue `whole` made. */
export interface NodeStateSeed {
  /** Each node whose state it wrote, and statement 9, by its address in the tree read. */
  readonly addresses: Readonly<Record<StatedNode | "statement9", string>>;
  /** Alice's tree read once every state was written. */
  readonly answer: IssueTreeAnswer;
  readonly ids: NodeStateIds;
  readonly lens: LensInvoked;
  readonly whole: WholeIssueSeed;
}

/** How long a seed waits for what it wrote to stand in a read. */
const SEED_READ_MS = 20_000;

/** Polls `read` until `holds` is true of its answer, and answers it; throws `message` past the
 *  seed's deadline. */
async function readUntil<T>(
  read: () => Promise<T>,
  holds: (answer: T) => boolean,
  message: string
): Promise<T> {
  const deadline = Date.now() + SEED_READ_MS;
  for (;;) {
    const answer = await read();
    if (holds(answer)) return answer;
    if (Date.now() > deadline) throw new Error(message);
    await pause(100);
  }
}

/**
 * Writes, on the issue `whole` that `seedWholeIssue` made: statement 9's words holding
 * "proposed" while it stays live; a failing result on check 4, recorded at the commit the seed's
 * own results name and read Failed before anything relies on it, so a flag that counted an
 * overtaken check would count it; an agent's decision that shapes statement 7, so that decision's
 * row draws a chip naming it; a lens on statement 8 (`invokeLens`); statement 7, check 2,
 * the first decision and plan task 1 proposed; and statement 8, plan task 2, check 4 and an agent's
 * decision that waits for review overtaken. Each node is named by the address alice's tree read
 * answers for it, so it runs after a row's gates found that read served; it asserts the read
 * answers each state it wrote and statement 9's words.
 */
export async function seedNodeStates(whole: WholeIssueSeed): Promise<NodeStateSeed> {
  const key = whole.key;
  const [coverage, decisions] = await Promise.all([getIssueCoverage(key), getIssueDecisions(key)]);
  const statement7Ref = coverage.requirements?.find(
    (requirement) => requirement.block_id === whole.requirements[6]
  )?.ref;
  if (statement7Ref === undefined) throw new Error(`the coverage read of ${key} holds no r7`);
  const reworded = await editArtifact(whole.specArtifactId, {
    ops: [{ find: requirementText(9), op: "replace", with: STATEMENT_9_WORDS }],
  });
  expect(reworded.applied, "the spec does not take statement 9's words").toBe(1);
  const decided = await editArtifact(
    whole.specArtifactId,
    {
      ops: [
        {
          after: `block:${OPEN_QUESTION_BLOCK_ID}`,
          markdown:
            `## Decisions\n\n:::decision{#${SHAPING_DECISION_ID} shaped="${statement7Ref}"}\n` +
            `${SHAPING_DECISION_WORDS}\n:::`,
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  expect(decided.applied, "the spec does not take the decision that shapes statement 7").toBe(1);

  await recordCheckResult(
    key,
    {
      check: whole.checks.older,
      commit: HEAD_SHA,
      failed: 1,
      negative_control: "Removed the row's state and the run failed.",
      observed: "1 failed",
      role: "tester",
      run: "bun run e2e -- e2e/issue-page-x.e2e.ts",
      surface: "browser",
      verdict: "fail",
    },
    { actor: recorderActor, as: "agent" }
  );
  await readUntil(
    () => getIssueResults(key),
    (read) =>
      (read.checks ?? []).some(
        (check) => check.block_id === whole.checks.older && checkStanding(check).state === "failed"
      ),
    "check 4 never reads Failed after its failing result, so no flag reading can rely on it"
  );

  const before = await readUntil(
    () => treeReadAs(key),
    (read) =>
      read.nodes.some((node) => node.id === SHAPING_DECISION_ID) &&
      read.nodes.some((node) => node.text.includes(STATEMENT_9_WORDS)),
    `the tree read of ${key} never holds statement 9's words and the decision that shapes statement 7`
  );
  expect(before.converted, "the seeded issue's tree read does not answer converted").toBe(true);
  const waiting = decisions.decisions?.find(
    (decision) =>
      decision.kind === "agent" &&
      decision.waiting_review &&
      decision.block_id !== whole.decisions[0]
  )?.block_id;
  if (waiting === undefined) {
    throw new Error(`the decisions read of ${key} holds no other agent's decision that waits`);
  }
  const node = (supertag: string, id: string | undefined, what: string) =>
    seededNode(before, key, supertag, id ?? "", what);
  const nodes = {
    check2: node("check", whole.checks.failed, "check 2"),
    check4: node("check", whole.checks.older, "check 4"),
    decision1: node("decision", whole.decisions[0], "the first decision"),
    planTask1: node("plan_task", whole.planTasks[0], "plan task 1"),
    planTask2: node("plan_task", whole.planTasks[1], "plan task 2"),
    shapingDecision: node("decision", SHAPING_DECISION_ID, "the decision that shapes statement 7"),
    statement7: node("requirement", whole.requirements[6], "statement 7"),
    statement8: node("requirement", whole.requirements[7], "statement 8"),
    statement9: node("requirement", whole.requirements[8], "statement 9"),
    waitingDecision: node("decision", waiting, "an agent's decision that waits for review"),
  };
  const addresses = {
    check2: addressIn(nodes.check2, key),
    check4: addressIn(nodes.check4, key),
    decision1: addressIn(nodes.decision1, key),
    planTask1: addressIn(nodes.planTask1, key),
    planTask2: addressIn(nodes.planTask2, key),
    statement7: addressIn(nodes.statement7, key),
    statement8: addressIn(nodes.statement8, key),
    statement9: addressIn(nodes.statement9, key),
    waitingDecision: addressIn(nodes.waitingDecision, key),
  };

  const lens = await invokeLens(key, {
    kind: "trade-off",
    on: addresses.statement8,
    verdict: STATEMENT_8_VERDICT,
  });
  for (const name of PROPOSED_NODES) await writeNodeState(key, addresses[name], "proposed");
  for (const name of OVERTAKEN_NODES) await writeNodeState(key, addresses[name], "overtaken");

  const answer = await treeReadAs(key);
  const stateOf = (id: string) => answer.nodes.find((one) => one.id === id)?.state;
  for (const name of PROPOSED_NODES) {
    expect(stateOf(nodes[name].id), `the tree read does not answer ${name} proposed`).toBe(
      "proposed"
    );
  }
  for (const name of OVERTAKEN_NODES) {
    expect(stateOf(nodes[name].id), `the tree read does not answer ${name} overtaken`).toBe(
      "overtaken"
    );
  }
  const statement9 = answer.nodes.find((one) => one.id === nodes.statement9.id);
  expect(statement9?.text, "the tree read does not answer statement 9's words").toContain(
    STATEMENT_9_WORDS
  );
  expect(statement9?.state, "the tree read does not answer statement 9 live").toBe("live");
  const lensNode = answer.nodes.find(
    (one) => one.supertag === "lens" && one.attributes.verdict === STATEMENT_8_VERDICT
  );
  expect(lensNode?.parent, "the tree read does not stand the lens under statement 8").toBe(
    nodes.statement8.id
  );
  return {
    addresses,
    answer,
    ids: {
      check2: nodes.check2.id,
      check4: nodes.check4.id,
      decision1: nodes.decision1.id,
      lens: lensNode?.id ?? "",
      planTask1: nodes.planTask1.id,
      planTask2: nodes.planTask2.id,
      shapingDecision: nodes.shapingDecision.id,
      statement7: nodes.statement7.id,
      statement8: nodes.statement8.id,
      statement9: nodes.statement9.id,
      waitingDecision: nodes.waitingDecision.id,
    },
    lens,
    whole,
  };
}

// ---------------------------------------------------------------------------------------------
// The seed of the nodes reserved for their author.
// ---------------------------------------------------------------------------------------------

/** The one word each node of `seedAuthorOnly` holds and no other node of the issue does. */
export const NODE_WORDS = {
  /** Her reserved constraint. */
  constraint: "Tamarisk",
  /** The finding under her reserved lens. */
  finding: "Ocelot",
  /** Her reserved lens on statement 2. */
  lens: "Halyard",
  /** Her reserved pre-mortem on statement 3. */
  preMortem: "Juniper",
  /** The project lens on statement 2, triggered by her reserved lens. */
  projectLens: "Bramble",
  /** The project risk under her reserved pre-mortem. */
  projectRisk: "Sorrel",
  /** Her reserved risk under that pre-mortem, which check 3 prevents. */
  risk: "Kestrel",
} as const;

/** A word no node of the issue holds, whose words filter is what a reserved node's words filter
 *  draws for a reader who may not see it. */
export const NOBODY_WROTE = "Quokka";

/** The nodes reserved for their author, alice. */
export const RESERVED_NODES = ["lens", "finding", "preMortem", "risk", "constraint"] as const;

/** One node `seedAuthorOnly` wrote: its address, as the write answered it, and its one word. */
export interface WrittenNode {
  readonly address: string;
  readonly word: string;
}

/** What `seedAuthorOnly` wrote on the issue `whole` made. */
export interface AuthorOnlySeed {
  readonly nodes: Readonly<Record<keyof typeof NODE_WORDS, WrittenNode>>;
  readonly whole: WholeIssueSeed;
}

/** The answer of one write of `seedAuthorOnly`, asserted to be reserved for its author or not. */
function written(
  answer: LensInvoked | LensFinding | RiskWritten | ConstraintRead,
  name: keyof typeof NODE_WORDS,
  visibility: "author" | "project"
): WrittenNode {
  expect(answer.visibility, `the ${name} is not written for ${visibility}`).toBe(visibility);
  return { address: answer.address, word: NODE_WORDS[name] };
}

/**
 * Writes, on an issue `seedWholeIssue` makes, as alice: reserved for her (`visibility:
 * "author"`), a lens on statement 2 with a finding, a pre-mortem on statement 3 with a risk that
 * check 3 prevents, and a constraint; for the project, a lens on statement 2 triggered by her
 * reserved lens, and a risk under her reserved pre-mortem. Each node holds one word no other node
 * holds (`NODE_WORDS`), and is returned with the address its write answered. Each statement's and
 * the check's address is the server's own (the coverage read's and the results read's `ref`).
 */
export async function seedAuthorOnly(): Promise<AuthorOnlySeed> {
  const whole = await seedWholeIssue();
  const key = whole.key;
  const [coverage, results] = await Promise.all([getIssueCoverage(key), getIssueResults(key)]);
  const statement = (block: string | undefined): string => {
    const ref = coverage.requirements?.find((requirement) => requirement.block_id === block)?.ref;
    if (ref === undefined) throw new Error(`the coverage read of ${key} holds no ${block}`);
    return ref;
  };
  const two = statement(whole.requirements[1]);
  const three = statement(whole.requirements[2]);
  const check3 = results.checks?.find((check) => check.block_id === whole.checks.noResult)?.ref;
  if (check3 === undefined) throw new Error(`the results read of ${key} holds no check 3`);
  const author = { visibility: "author" } as const;
  const W = NODE_WORDS;

  const lens = await invokeLens(key, {
    kind: "trade-off",
    on: two,
    verdict: `${W.lens} holds the reasons she keeps to herself.`,
    ...author,
  });
  const finding = await addFinding(
    key,
    lens.address,
    `${W.finding} marks what she found and keeps.`,
    author
  );
  const preMortem = await invokeLens(key, {
    kind: "pre-mortem",
    on: three,
    verdict: `${W.preMortem} is the failure she imagines alone.`,
    ...author,
  });
  const risk = await writeRisk(key, {
    early_warning: "A reader sees a branch count that names nothing drawn.",
    heading: `${W.risk} is the risk she keeps to herself.`,
    lens: preMortem.address,
    prevented_by: check3,
    prevention: "Each count is made from what the reader's own read holds.",
    ...author,
  });
  const constraint = await writeConstraint(
    key,
    {
      measurement: `${W.constraint} rows drawn on her first screen`,
      taken_where: "a 1440 by 900 desktop window on the seeded issue",
      value: "40",
    },
    author
  );
  const projectLens = await invokeLens(key, {
    kind: "pre-mortem",
    on: two,
    triggered_by: lens.address,
    verdict: `${W.projectLens} is the lens every reader of the project reads.`,
  });
  const projectRisk = await writeRisk(key, {
    early_warning: "A reader who may not see the pre-mortem reads a risk with no lens above it.",
    heading: `${W.projectRisk} is the risk every reader of the project reads.`,
    lens: preMortem.address,
    prevention: "The risk stands under the statement the pre-mortem examined.",
  });
  return {
    nodes: {
      constraint: written(constraint, "constraint", "author"),
      finding: written(finding, "finding", "author"),
      lens: written(lens, "lens", "author"),
      preMortem: written(preMortem, "preMortem", "author"),
      projectLens: written(projectLens, "projectLens", "project"),
      projectRisk: written(projectRisk, "projectRisk", "project"),
      risk: written(risk, "risk", "author"),
    },
    whole,
  };
}

// ---------------------------------------------------------------------------------------------
// The readers.
// ---------------------------------------------------------------------------------------------

/** One mark the page draws for a node (`NodeMarks.tsx`): its token (`data-node-mark`), its word,
 *  the token of its shape (`data-glyph`), and whether that shape is drawn with a box and a
 *  colour, which forced colours must leave it. */
export interface DrawnMark {
  readonly mark: string | null;
  readonly shape: string | null;
  readonly shown: boolean;
  readonly word: string;
}

/** What draws a node of its own, or names one inside another's row: a mark inside one of them is
 *  that node's, and no mark of the element around it. */
const MARK_HOLDERS =
  "[data-node-row], [data-verification-check], [data-plan-bar-row], [data-plan-overtaken-row], " +
  "[data-node-chip], [data-node-parent-line], [data-lens-line]";

/** The marks inside `scope`; with `own`, only those it draws for its own node, none of a node,
 *  a chip or a parent line drawn inside it. */
function marksIn(scope: Locator, own: boolean): Promise<DrawnMark[]> {
  return scope.first().evaluate(
    (element, { holders, own: onlyOwn }) =>
      [...element.querySelectorAll("[data-node-mark]")]
        .filter((mark) => {
          if (!onlyOwn) return true;
          for (let at = mark.parentElement; at !== null && at !== element; at = at.parentElement) {
            if (at.matches(holders)) return false;
          }
          return true;
        })
        .map((mark) => {
          const shape = mark.querySelector("svg[data-glyph]");
          const box = shape?.getBoundingClientRect();
          const color = shape === null ? "" : getComputedStyle(shape).color;
          return {
            mark: mark.getAttribute("data-node-mark"),
            shape: shape?.getAttribute("data-glyph") ?? null,
            shown:
              box !== undefined &&
              box.width > 0 &&
              box.height > 0 &&
              color !== "" &&
              color !== "transparent" &&
              color !== "rgba(0, 0, 0, 0)",
            word: (mark.querySelector("[data-page-chrome]")?.textContent ?? "").trim(),
          };
        }),
    { holders: MARK_HOLDERS, own }
  );
}

/** The marks the row, check or bar `scope` draws in its own state line, in order. */
export function ownMarks(scope: Locator): Promise<DrawnMark[]> {
  return marksIn(scope, true);
}

/** Every mark inside `scope` - a chip, a parent line - in order: the compact form. */
export function compactMarks(scope: Locator): Promise<DrawnMark[]> {
  return marksIn(scope, false);
}

/** One row or overtaken line of the outline's tree, in drawing order. */
export interface TreeItem {
  /** `aria-expanded`: a line's "true" or "false"; a row's, or null for one that holds nothing. */
  readonly expanded: string | null;
  /** A row's `data-node-id`; null for a line. */
  readonly id: string | null;
  readonly kind: "line" | "row";
  /** `aria-level`. */
  readonly level: number;
  /** A row's `data-node-read-id`; null for a line. */
  readonly readId: string | null;
  /** Its whole text. */
  readonly text: string;
  readonly visible: boolean;
}

/** The overtaken line, as the outline draws it after a parent's other children. */
const OVERTAKEN_LINE = "[data-overtaken-line]";

/** Every row and every overtaken line of the page's tree, in the order it draws them. */
export function treeItemsDrawn(page: Page): Promise<TreeItem[]> {
  return issuePage(page)
    .locator(`[data-node-row], ${OVERTAKEN_LINE}`)
    .evaluateAll(
      (elements, line) =>
        elements.map((element) => {
          const isLine = element.matches(line);
          return {
            expanded: element.getAttribute("aria-expanded"),
            id: isLine ? null : element.getAttribute("data-node-id"),
            kind: isLine ? ("line" as const) : ("row" as const),
            level: Number(element.getAttribute("aria-level") ?? "0"),
            readId: isLine ? null : element.getAttribute("data-node-read-id"),
            text: (element.textContent ?? "").replace(/\s+/g, " ").trim(),
            visible: element.getClientRects().length > 0,
          };
        }),
      OVERTAKEN_LINE
    );
}

/** The items drawn directly under `parent`, an item of `items` (a row or a line), or at the top
 *  for null, in order, by the tree's own levels; none where `items` does not hold `parent`. */
export function childItems(
  items: readonly TreeItem[],
  parent: TreeItem | null | undefined
): TreeItem[] {
  if (parent === undefined) return [];
  const at = parent === null ? -1 : items.indexOf(parent);
  if (parent !== null && at === -1) return [];
  const level = parent?.level ?? 0;
  const children: TreeItem[] = [];
  for (const item of items.slice(at + 1)) {
    if (item.level <= level) break;
    if (item.level === level + 1) children.push(item);
  }
  return children;
}

/** The overtaken line `item` of `items` (`treeItemsDrawn`), as a locator: the same place among
 *  the page's lines. */
export function lineLocator(page: Page, items: readonly TreeItem[], item: TreeItem): Locator {
  const lines = items.filter((one) => one.kind === "line");
  return issuePage(page).locator(OVERTAKEN_LINE).nth(lines.indexOf(item));
}

/** Whether the focus is on an overtaken line itself. */
export function lineFocused(page: Page): Promise<boolean> {
  return page.evaluate((line) => document.activeElement?.matches(line) === true, OVERTAKEN_LINE);
}

/** One folded row's count: what `data-folded-count` says its branch holds. */
export interface FoldedCount {
  readonly count: number;
  readonly id: string;
  readonly readId: string | null;
}

/** Every folded row's count, in drawing order. */
export function foldedCounts(page: Page): Promise<FoldedCount[]> {
  return issuePage(page)
    .locator("[data-node-row][data-folded-count]")
    .evaluateAll((rows) =>
      rows.map((row) => ({
        count: Number(row.getAttribute("data-folded-count")),
        id: row.getAttribute("data-node-id") ?? "",
        readId: row.getAttribute("data-node-read-id"),
      }))
    );
}

/** The count the control of `preset` draws, or NaN where it draws none at this width. Every
 *  caller reads it inside `expect.poll`, which reads it again until the count holds. */
export async function filterCount(page: Page, preset: Preset): Promise<number> {
  const count = presetCount(page, preset);
  // Every caller polls this, so a count read before the page draws it is read again.
  if ((await count.count()) === 0) return Number.NaN;
  return Number(String(await count.first().textContent()).trim());
}

/** How many nodes a filter's flag says ask something of the reader: the checks that fail, the
 *  decisions that wait for review; 0 where it draws none. Every caller reads it inside
 *  `expect.poll`, which reads it again until the count holds. */
export async function flagCount(page: Page, word: "failing" | "to_review"): Promise<number> {
  const flag = presetRow(page).locator(`[data-preset-flag="${word}"] [data-author-text]`);
  // Every caller polls this, so a flag read before the page draws it is read again.
  if ((await flag.count()) === 0) return 0;
  return Number(String(await flag.first().textContent()).trim());
}

/** The Verification summary's "N of M covered": how many statements carry a check, of how many
 *  it counts; null where the view draws no summary. Every caller reads it inside `expect.poll`,
 *  which reads it again until the summary holds. */
export async function coveredCount(
  page: Page
): Promise<{ readonly covered: number; readonly of: number } | null> {
  const line = issuePage(page).locator("[data-verification-covered]");
  // Every caller polls this, so a summary read before the page draws it is read again.
  if ((await line.count()) === 0) return null;
  const [covered, of] = await line.locator("[data-author-text]").allTextContents();
  return { covered: Number(String(covered).trim()), of: Number(String(of).trim()) };
}

/** Details' "Referenced by" list of the focused row (`node-references/NodeReferences.tsx`):
 *  opened by its control, then counted once it answered, its "none" line counting none. */
export async function referencedByCount(page: Page): Promise<number> {
  const section = page.locator("[data-node-references]");
  const toggle = section.locator(":scope > button");
  if ((await expandedState("Details' Referenced by", toggle)) === "shut") await toggle.click();
  const list = section.locator("ul > li");
  await expect
    .poll(async () => (await list.count()) > 0 || (await section.locator("p").count()) > 0, {
      message: "Details' Referenced by never answered",
    })
    .toBe(true);
  return list.count();
}

/** The text of every element of the page whose whole text is a raw value of the tree read - a
 *  `state` or a `visibility` - or a word-list key, in any letter case, unless `book` (the words
 *  `page-words.ts` writes) holds it as written. */
export function rawValueTexts(page: Page, book: readonly string[]): Promise<string[]> {
  return issuePage(page).evaluate(
    (root, { book: words, raw }) =>
      [root, ...root.querySelectorAll("*")].flatMap((element) => {
        const text = (element.textContent ?? "").replace(/\s+/g, " ").trim();
        return raw.includes(text.toLowerCase()) && !words.includes(text) ? [text] : [];
      }),
    {
      book,
      raw: ["proposed", "overtaken", "settled", "live", "gone", "author", "project", "only_you"],
    }
  );
}

// ---------------------------------------------------------------------------------------------
// What the page sent.
// ---------------------------------------------------------------------------------------------

/** A request to a node state route: a node's state, its settle, and an issue's proposed nodes,
 *  which the page reads none of (it reads a node's life from the tree read alone). */
const NODE_STATE_ROUTE = /^\/api\/v1\/issues\/[^/]+\/(?:nodes\/[^/]+\/(?:state|settle)|proposed)$/u;

/** The settle route's path, with the node's address as its one encoded segment. */
const SETTLE_ROUTE = /^\/api\/v1\/issues\/[^/]+\/nodes\/([^/]+)\/settle$/u;

/** Each request of `requests` to a node state route, as its method and path. */
export function nodeStateRequests(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) =>
    NODE_STATE_ROUTE.test(new URL(request.path, "http://requests.invalid").pathname)
      ? [`${request.method} ${request.path}`]
      : []
  );
}

/** The address each `POST` to the settle route of `requests` names, decoded. */
export function settlesSent(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) => {
    const matched = SETTLE_ROUTE.exec(new URL(request.path, "http://requests.invalid").pathname);
    return request.method === "POST" && matched !== null
      ? [decodeURIComponent(matched[1] ?? "")]
      : [];
  });
}

// ---------------------------------------------------------------------------------------------
// Details' facts of the focused row.
// ---------------------------------------------------------------------------------------------

/** The file that draws Details' facts of the focused row and their "Referenced by" list. */
const FOCUSED_FACTS_FILE = new URL(
  "../web/src/features/issue-page/FocusedNodeFacts.tsx",
  import.meta.url
);

/** Details' facts of the focused row and their "Referenced by" list, which the rows read only
 *  where this tree holds the file that draws them: until then the page draws none, and nothing
 *  of this change is owed there. The file is looked for once, as this module loads. */
export const FOCUSED_FACTS_GATE: Gate = {
  met: existsSync(FOCUSED_FACTS_FILE),
  owed:
    "owed until Details draws the focused row's facts (FocusedNodeFacts.tsx is not in this " +
    "tree), so those facts and Referenced by are not read here",
};

/** Goes on where `gate` is met; where it is not, records an `owed` annotation naming it, once
 *  for the row however often the row reaches it, and the row goes on without the reading it
 *  gates. Answers whether the gate is met. */
export function noteOwed(gate: Gate): boolean {
  const annotations = test.info().annotations;
  const noted = annotations.some(({ description }) => description === gate.owed);
  if (!gate.met && !noted) annotations.push({ description: gate.owed, type: "owed" });
  return gate.met;
}

/** Details' facts of the row the focus is on (`FocusedNodeFacts.tsx`). */
export function focusedNodeFacts(page: Page): Locator {
  return page.getByTestId("focused-node-facts");
}
