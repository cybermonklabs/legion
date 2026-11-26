// What every seed of `e2e/issue-layout-seed.ts` stands on: the project, the fake GitHub's
// component model and pull request, the fake Envoy's sessions, the polls that wait for the
// server's own indexing, the issue's tree read as alice reads it, and the one issue-with-documents
// builder the whole issue and the request baseline differ from each other only by size in. The
// gaps between what the page is specified to draw and what today's model holds are recorded in
// `issue-layout-seed.ts`'s doc comment.
//
// The tree read's helpers (`treePath`, `readTree`, `blockNodeIn`, `decisionNodesOf`,
// `decisionSumOf`, `decisionSum`) are the decision rows' readers too (`e2e/issue-page-u.ts`). They
// stand here because a seed keeps a decision by the sum `decisionSum` reads, and
// `e2e/issue-page-u.ts` imports the seeds through `e2e/issue-page-t.ts`: a seed that imported
// that file would close an import cycle, and this one imports neither of them nor a seed.

import type { Actor, IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import type { Filter } from "../web/src/features/issue-page/presets";
import { checkStanding } from "../web/src/features/issue-page/tree-drafts";
import { mergeTreePages } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { setInterests, setLiveSessions } from "./agents";
import {
  answerAsk,
  baseUrl,
  claimIssue,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getIssue,
  getIssueResults,
  patchIssue,
  putArchitectureSource,
  recordCheckResult,
  replyToCommentBlock,
  syncArchitectureSource,
  userHeaders,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  ANSWERED_QUESTION_BLOCK_ID,
  answeredQuestionBlock,
  checkBlockId,
  checkStateOf,
  checkText,
  decisionBlockId,
  decisionsPart,
  OPEN_QUESTION_BLOCK_ID,
  planDocument,
  rewordedCheckText,
  type SeededCheckState,
  specDocument,
  verificationDocument,
} from "./issue-layout-seed-text";

export const PROJECT = "LAY";
const ARCHITECTURE_REPO = "legion/issue-layout-arch";
/** The repository the whole issue carries, which each seed suffixes with a name of its own
 *  (`seedIssueWithDocuments`); the request baseline carries its own, so the two never share the
 *  App's cached answer about one repository. */
export const WHOLE_REPO = "acme/issue-layout";
export const BASELINE_REPO = "acme/issue-baseline";
/** The commit the seeded pull request's head is at, so a result recorded there reads `current`. */
export const HEAD_SHA = "a".repeat(40);
/** A commit no seeded pull request names, so a result recorded there is at no head. */
export const STRAY_SHA = "b".repeat(40);
const PULL_NUMBER = 11;

export const PLANNER = "issue-layout-planner";
export const REVIEWER = "issue-layout-reviewer";
export const TESTER = "issue-layout-tester";
/** The title the planner's session stamps on what it writes, and the one the fake Envoy lists it
 *  under, so the page names it the same whichever it reads. */
export const PLANNER_TITLE = "Planner (issue layout)";
export const plannerActor: Actor = {
  id: PLANNER,
  kind: "session",
  origin: { session_title: PLANNER_TITLE },
};
export const asPlanner = { actor: plannerActor, as: "agent" as const };
/** The agent session that records every seeded result. An Envoy session id is a UUID
 *  (`session-ids.ts`), which is the identifier the page must never draw in an author's place;
 *  the registry lists no such session, so the title the session stamped on its write is the
 *  only name it has. */
export const recorderActor: Actor = {
  id: "019a1c0a-6d4b-7e8a-bf39-4a1b5c8d9e07",
  kind: "session",
  origin: { session_title: "Recorder (issue layout)" },
};

/** The component model the fake GitHub serves: two components, both attachable. */
const ARCHITECTURE_MODEL = {
  "panel.md": "---\ntitle: Issue panel\n---\nThe one right-hand panel.\n",
  "tree.md": "---\ntitle: Issue tree\n---\nThe ordered body of the page.\n",
};
const COMPONENT_IDS = ["panel", "tree"];

export async function pause(milliseconds: number): Promise<void> {
  const tick = Promise.withResolvers<void>();
  setTimeout(tick.resolve, milliseconds);
  await tick.promise;
}

/** The state a check seeded in `state` must read. `unmarked` reads itself only while the App is
 *  installed on no repository that covers the check's issue, since a check whose repository
 *  Dispatch cannot read is the one it can judge nothing of; with one installed, a result at a
 *  commit the default branch does not hold stands at another commit and reads `older` instead
 *  (results_comparison.go's `passStandsOf`). `no-control` is a pass like any other. */
function wantedState(state: SeededCheckState, repositoryInstalled: boolean): string {
  if (state === "no-control") return "passed";
  return state === "unmarked" && repositoryInstalled ? "older" : state;
}

/** Waits until every check of the issue reads the state its seeding implies, so a fixture that
 *  came out in four states fails here and not inside the test that reads the fifth. The state is
 *  read with the page's own rule (`checkStanding` in `web/src/features/issue-page/tree-drafts.ts`), so
 *  the seed waits for what the page draws and never for a copy of that rule that has fallen
 *  behind it. With `only`, it waits for the checks seeded in that one state alone. */
export async function awaitSeededCheckStates(
  issueKey: string,
  checks: number,
  repositoryInstalled: boolean,
  only?: SeededCheckState
): Promise<void> {
  const wanted = new Map<string, string>();
  for (let index = 1; index <= checks; index += 1) {
    const state = checkStateOf(index);
    if (only !== undefined && state !== only) continue;
    wanted.set(checkBlockId(index), wantedState(state, repositoryInstalled));
  }
  const deadline = Date.now() + 20_000;
  for (;;) {
    const read = await getIssueResults(issueKey);
    const seen = new Map(
      (read.checks ?? []).map((check) => [check.block_id, checkStanding(check).state])
    );
    if ([...wanted].every(([block, state]) => seen.get(block) === state)) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${issueKey}'s checks read ${JSON.stringify([...seen])}, want ${JSON.stringify([...wanted])}`
      );
    }
    await pause(100);
  }
}

/** The id of the open ask the server indexed from a document's `ask` block (`awaitBlockAsks`). */
export async function awaitBlockAsk(issueKey: string, block: string): Promise<string> {
  const [id] = await awaitBlockAsks(issueKey, [block]);
  return id;
}

/** The ids of the open asks the server indexed from a document's `ask` blocks `blocks`, in the
 *  order of `blocks`, read from one read of the issue per try. The document service settles the
 *  document after the write has answered, so each ask exists a moment later. */
export async function awaitBlockAsks(
  issueKey: string,
  blocks: readonly string[]
): Promise<string[]> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const open = (await getIssue(issueKey)).open_asks ?? [];
    const ids: string[] = [];
    const missing: string[] = [];
    for (const block of blocks) {
      const ask = open.find((row) => row.block_id === block);
      if (ask === undefined) missing.push(block);
      else ids.push(ask.id);
    }
    if (missing.length === 0) return ids;
    if (Date.now() > deadline) {
      const asks = missing.length === 1 ? "ask was" : "asks were";
      throw new Error(`${issueKey}'s ${missing.join(", ")} ${asks} not indexed within 20 s`);
    }
    await pause(100);
  }
}

/** The path of issue `key`'s tree read. `e2e/issue-page-t-tree.ts` holds the same function and
 *  does not export it, so this is the one copy the reads below and the decision rows' gate on
 *  the tree read's Decisions view (`e2e/issue-page-u.ts`' `decisionsViewServedGate`) use. */
export function treePath(key: string): string {
  return `/api/v1/issues/${encodeURIComponent(key)}/tree`;
}

/**
 * Issue `key`'s tree read, or the read of one of its views (`view`, one of the four filters), as
 * alice reads it: every page its cursor names, the pages merged as the page merges them
 * (`mergeTreePages`). A cursor the read names twice fails the read rather than walk the same
 * pages again.
 */
export async function readTree(key: string, view: Filter | null): Promise<IssueTreeRead> {
  const headers = await userHeaders("alice");
  const pages: IssueTreeRead[] = [];
  const cursors = new Set<string>();
  let cursor: string | null = null;
  do {
    const url = new URL(treePath(key), baseUrl);
    if (view !== null) url.searchParams.set("view", view);
    if (cursor !== null) url.searchParams.set("cursor", cursor);
    const response = await fetch(url, { headers });
    if (!response.ok) {
      throw new Error(
        `GET ${url.pathname}${url.search} failed: ${response.status} ${await response.text()}`
      );
    }
    const answered = (await response.json()) as IssueTreeRead;
    pages.push(answered);
    cursor = answered.cursor;
    if (cursor !== null && cursors.has(cursor)) {
      throw new Error(`${url.pathname} names the cursor ${cursor} twice`);
    }
    if (cursor !== null) cursors.add(cursor);
  } while (cursor !== null);
  const [first, ...rest] = pages;
  if (first === undefined) throw new Error(`${treePath(key)} answered no page`);
  return mergeTreePages([first, ...rest]);
}

/** The node of this issue tagged `supertag` whose block is `block`, out of `nodes`, the nodes
 *  `what` answers: the read names a block by its store id and carries its block id beside it
 *  (`block_id`). A block no node of `nodes` is fails, naming the block. */
export function blockNodeIn(
  nodes: readonly IssueTreeNode[],
  supertag: string,
  block: string,
  what: string
): IssueTreeNode {
  const node = nodes.find(
    (one) =>
      one.supertag === supertag && one.issue === undefined && (one.block_id ?? one.id) === block
  );
  if (node === undefined) throw new Error(`${what} holds no ${supertag} node "${block}"`);
  return node;
}

/** The decision nodes of issue `key` whose blocks are `blocks`, in that order, from one tree
 *  read as it answers now. */
export async function decisionNodesOf(
  key: string,
  blocks: readonly string[]
): Promise<IssueTreeNode[]> {
  const nodes = (await readTree(key, null)).nodes ?? [];
  return blocks.map((block) => blockNodeIn(nodes, "decision", block, `${key}'s tree read`));
}

/** The `text_sha256` the tree read answers on `node`, a decision node of issue `key`, in the
 *  model's decision it carries (`decision`): the sum a Keep of the words the node holds carries. */
export function decisionSumOf(key: string, node: IssueTreeNode): string {
  const sum = node.decision?.text_sha256;
  if (sum === undefined || sum === "") {
    throw new Error(`${key}'s tree read answers no text_sha256 on its decision node ${node.id}`);
  }
  return sum;
}

/** The `text_sha256` the tree read answers on issue `key`'s decision node `block`, every page its
 *  cursor names read: the sum a Keep of the words as they stand now carries. */
export async function decisionSum(key: string, block: string): Promise<string> {
  const [node] = await decisionNodesOf(key, [block]);
  return decisionSumOf(key, node);
}

/** The imported component model, the repository and the pull request the results are measured
 *  against, whose title attributes it to `issueKey`. */
async function seedProjectSurroundings(issueKey: string, repository: string): Promise<void> {
  await seedFakeGithub({
    [ARCHITECTURE_REPO]: { contents: "read", files: ARCHITECTURE_MODEL, installation_id: 311 },
    [repository]: {
      // `STRAY_SHA` is a commit the repository holds and neither a pull request nor the default
      // branch heads: while this repository is installed a result recorded at it reads `older`
      // (`wantedState`), and once the App is installed on nothing, as `seedWholeIssue` leaves it,
      // it reads `unmarked`, drawn `Cannot tell`. A commit the repository holds nowhere at all is
      // refused `400 RESULT_COMMIT_UNKNOWN`, so it is named here. The `after_merge` checks compare
      // with the default branch, which holds the pull request's head.
      branch_commits: { main: [HEAD_SHA] },
      commit_pulls: { [HEAD_SHA]: [PULL_NUMBER] },
      commits: [STRAY_SHA],
      contents: "read",
      installation_id: 312,
      pull_requests: "read",
      pulls: {
        [PULL_NUMBER]: { head_sha: HEAD_SHA, state: "open", title: `${issueKey} the redesign` },
      },
    },
  });
  await putArchitectureSource(PROJECT, { branch: "main", repo: ARCHITECTURE_REPO });
  const source = await syncArchitectureSource(PROJECT, ARCHITECTURE_REPO);
  if (source.last_error !== null) {
    throw new Error(`the component model did not import: ${source.last_error}`);
  }
}

/** The sessions a seeded issue routes to, is claimed by and is subscribed by. A subscriber is a
 *  fake Envoy interest, because Dispatch stores none. */
export async function seedSessions(
  issueKey: string,
  subscribers: readonly string[]
): Promise<void> {
  await setLiveSessions([
    { session_id: PLANNER, title: PLANNER_TITLE },
    { session_id: REVIEWER, title: "Reviewer (issue layout)" },
    { session_id: TESTER, title: "Tester (issue layout)" },
  ]);
  await setInterests(
    subscribers.map((session) => ({
      session_id: session,
      topics: [`notifications.dispatch.issue.${issueKey}.>`],
    }))
  );
}

/** One result per check, in the state `checkStateOf` names: a pass and a fail at the pull
 *  request's head, nothing at all for the check nobody ran, a pass whose check is then reworded,
 *  a pass at a commit no pull request names, and a pass recording no negative control.
 *
 *  The last two are `after_merge` checks (`verificationDocument`), compared with the default
 *  branch, and the order of the writes is what seeds their states. The pass stands once a read
 *  with the App installed has named that branch and found its commit there, an answer the
 *  server keeps, so it stands with the App installed on nothing too. The result at a commit no
 *  branch holds is recorded after that read, since the server keeps an answer of "not there"
 *  as well: no read with the App installed compares it, and once the App is gone nothing can
 *  say where it stands. */
async function recordSeededResults(
  issueKey: string,
  checks: number,
  verificationArtifactId: string
): Promise<void> {
  const record = async (index: number, state: SeededCheckState): Promise<void> => {
    await recordCheckResult(
      issueKey,
      {
        check: checkBlockId(index),
        commit: state === "unmarked" ? STRAY_SHA : HEAD_SHA,
        failed: state === "failed" ? 2 : 0,
        negative_control:
          state === "no-control" ? "" : "Removed the row's state and the run failed.",
        observed: state === "failed" ? "2 failed" : "1 passed",
        role: "tester",
        run: "bun run e2e -- e2e/issue-layout.e2e.ts",
        surface: "browser",
        verdict: state === "failed" ? "fail" : "pass",
      },
      { actor: recorderActor, as: "agent" }
    );
    if (state === "older") {
      await editArtifact(verificationArtifactId, {
        ops: [{ find: checkText(index), op: "replace", with: rewordedCheckText(index) }],
      });
    }
  };
  const unmarked: number[] = [];
  for (let index = 1; index <= checks; index += 1) {
    const state = checkStateOf(index);
    if (state === "no-result") continue;
    if (state === "unmarked") unmarked.push(index);
    else await record(index, state);
  }
  await awaitSeededCheckStates(issueKey, checks, true, "no-control");
  for (const index of unmarked) await record(index, "unmarked");
}

export interface IssueShape {
  readonly checks: number;
  readonly children: number;
  readonly commentReplies: number;
  readonly commentThreads: number;
  readonly decisions: number;
  readonly planTasks: number;
  readonly repository: string;
  readonly requirements: number;
  readonly title: string;
}

export interface SeededSkeleton {
  readonly answeredAskId: string;
  readonly checks: readonly string[];
  readonly children: { readonly blocked: string; readonly done: string; readonly keys: string[] };
  readonly commentReplies: string[];
  readonly commentThreads: string[];
  readonly decisions: readonly string[];
  readonly key: string;
  readonly openQuestionAskId: string;
  readonly parentKey: string;
  readonly planArtifactId: string;
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** One whole issue: a parent, every property the identity block draws, its children with one
 *  blocked and one done, its spec with both questions and the planner's `## Decisions` part,
 *  its Verification and plan documents, one result per check, its comment threads and two handoffs. */
export async function seedIssueWithDocuments(shape: IssueShape): Promise<SeededSkeleton> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  // `force` past the possible-duplicate guard: these titles are deliberately close to each other.
  const parent = await createIssue({
    force: true,
    project: PROJECT,
    title: "The issue page is redesigned",
  });
  const issue = await createIssue({
    force: true,
    parent: parent.key,
    project: PROJECT,
    spec: specDocument({ openQuestion: true, requirements: shape.requirements, summary: true }),
    title: shape.title,
  });
  const specArtifactId = issue.primary_artifact_id ?? "";
  // The server keeps what it last read of a repository - its default branch, or that the App
  // cannot read it - for five minutes, across rows, and the after-merge checks stand on that
  // answer once the App is gone. A repository of this seed's own is read afresh here, while the
  // App is installed, so no answer an earlier row left behind decides them.
  const repository = `${shape.repository}-${crypto.randomUUID().slice(0, 8)}`;
  await seedProjectSurroundings(issue.key, repository);
  await seedSessions(issue.key, [PLANNER, REVIEWER, TESTER]);
  await patchIssue(issue.key, {
    assignee: "alice",
    components: { ids: COMPONENT_IDS, mode: "explicit" },
    external_links: [
      { kind: "github_pr", url: `https://github.com/${repository}/pull/${PULL_NUMBER}` },
    ],
    labels: ["dispatch", "layout", "redesign"],
    priority: 1,
    repo: repository,
    route: `session:${PLANNER}`,
    status: "in_progress",
  });
  await claimIssue(issue.key, asPlanner);

  const children: string[] = [];
  for (let index = 1; index <= shape.children; index += 1) {
    const child = await createIssue({
      force: true,
      parent: issue.key,
      project: PROJECT,
      title: `Child ${index} of the redesign`,
    });
    children.push(child.key);
  }
  const blocked = children[1] ?? "";
  const done = children[children.length - 1] ?? "";
  await patchIssue(blocked, { blocked_by: [children[0] ?? ""] });
  await patchIssue(done, { status: "done" });

  const verification = await createIssueArtifact(issue.key, {
    content: verificationDocument({
      checks: shape.checks,
      issueKey: issue.key,
      requirements: shape.requirements,
    }),
    name: "verification.md",
  });
  const plan = await createIssueArtifact(issue.key, {
    content: planDocument({
      issueKey: issue.key,
      planTasks: shape.planTasks,
      requirements: shape.requirements,
    }),
    name: "plan.md",
  });
  await editArtifact(
    specArtifactId,
    { ops: [{ after: "end", markdown: answeredQuestionBlock(issue.key), op: "insert" }] },
    asPlanner
  );
  const answeredAskId = await awaitBlockAsk(issue.key, ANSWERED_QUESTION_BLOCK_ID);
  await answerAsk(answeredAskId, { expected_edited_at: null, selected: [], text: "All of them." });
  // The planner's decisions, each a `decision` block of the spec's `## Decisions` part: Dispatch
  // refuses a document of decisions (409 `DECISIONS_ARE_NODES`).
  await editArtifact(
    specArtifactId,
    {
      ops: [
        {
          after: "end",
          markdown: decisionsPart({
            askRef: `dispatch://${issue.key}/ask/${answeredAskId}`,
            count: shape.decisions,
            issueKey: issue.key,
          }),
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  await recordSeededResults(issue.key, shape.checks, verification.artifact.id);

  const commentThreads: string[] = [];
  for (let index = 1; index <= shape.commentThreads; index += 1) {
    const thread = await createComment(issue.key, {
      body: `Thread ${index} opens here.\n\nIts second paragraph is a block of its own.`,
    });
    commentThreads.push(thread.id);
  }
  const commentReplies: string[] = [];
  for (let index = 0; index < shape.commentReplies; index += 1) {
    const thread = commentThreads[index % commentThreads.length] ?? "";
    const reply = await replyToCommentBlock(issue.key, thread, "b2", {
      body: `Reply ${index + 1} answers the second paragraph of its thread.`,
    });
    commentReplies.push(reply.id);
  }

  await createHandoff(issue.key, "plan", { phase: "plan", schemaVersion: 1 }, plannerActor);
  await createHandoff(
    issue.key,
    "implement",
    { phase: "implement", schemaVersion: 1, summary: "The page on today's data." },
    plannerActor
  );

  return {
    answeredAskId,
    checks: Array.from({ length: shape.checks }, (_, index) => checkBlockId(index + 1)),
    children: { blocked, done, keys: children },
    commentReplies,
    commentThreads,
    decisions: Array.from({ length: shape.decisions }, (_, index) => decisionBlockId(index + 1)),
    key: issue.key,
    openQuestionAskId: await awaitBlockAsk(issue.key, OPEN_QUESTION_BLOCK_ID),
    parentKey: parent.key,
    planArtifactId: plan.artifact.id,
    specArtifactId,
    verificationArtifactId: verification.artifact.id,
  };
}
