import type { Actor, ActorOrigin } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  answerAsk,
  createAgentMessage,
  createArtifactAsk,
  createAsk,
  createBroadcast,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getInbox,
  getIssue,
  patchIssue,
  type RowKey,
  replyToCommentDelivery,
  replyToMessageDelivery,
  resolveAsk,
  snoozeAsk,
} from "./api";
import {
  awaitDecisionBlock,
  awaitSeededInbox,
  type SeededInboxRow,
} from "./local-instance-support";
import { sessionIds } from "./session-ids";
import type { SignInLogin } from "./users";

// One realistic workspace, built through the public API, that fills every surface only a human
// acts on: the Inbox (open, snoozed and document asks), the Agents page (open rows and both
// folds), a direct message, a broadcast and an issue's Conversation. `e2e/workspace.e2e.ts`
// proves each surface renders non-empty.

// Four sessions carry no `last_seen`, which the fake Envoy answers as "live now" on every read,
// so they never fall under `Quiet` however long an instance runs. The Archivist is the one that
// must: twenty minutes stale already, it only grows staler after this module is evaluated.
export const workspaceSessions: FakeSession[] = [
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    machine_id: "build-host",
    roles: ["planner"],
    session_id: sessionIds.planner,
    title: "Planner",
  },
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/tester",
    machine_id: "build-host",
    roles: ["tester"],
    session_id: sessionIds.tester,
    title: "Tester",
  },
  {
    capabilities: ["aside"],
    dir: "/workspaces/reviewer",
    machine_id: "review-host",
    roles: ["reviewer"],
    session_id: sessionIds.reviewer,
    title: "Reviewer",
  },
  {
    capabilities: ["btw", "steer"],
    dir: "/workspaces/observer",
    machine_id: "build-host",
    roles: [],
    session_id: sessionIds.observer,
    title: "Observer",
  },
  // Twenty minutes stale: the Agents page lists it under `Quiet` (`agent-groups.ts`'s
  // `QUIET_AFTER_MS`, ten minutes).
  {
    capabilities: ["aside", "btw"],
    dir: "/workspaces/archivist",
    last_seen: Date.now() - 20 * 60_000,
    machine_id: "review-host",
    roles: ["archivist"],
    session_id: sessionIds.archivist,
    title: "Archivist",
  },
];
export const workspaceSessionIds = workspaceSessions.map((session) => session.session_id);

/** The name of each of `workspaceSessions`, in its order: the logical name a caller's
 *  `sessionId` is asked for (`seedWorkspace`). */
const workspaceSessionNames = ["planner", "tester", "reviewer", "observer", "archivist"] as const;
type WorkspaceSessionName = (typeof workspaceSessionNames)[number];

/** The fixed session ids the review corpus, the local instance and the docs pictures use. */
const fixedSessionId = (name: WorkspaceSessionName): string => sessionIds[name];

/** Two sessions beside the workspace's, named as many of production's claim holders are: one by
 *  a title of 56 characters led by an emoji, as a person titles a session, and one by nothing but
 *  its id, a session-id-shaped string, which is what a session whose title is its id reads as.
 *  Each holds work in the review corpus (`e2e/review-seed.ts`), which alone registers them
 *  (`reviewSessions`): the Agents page of every other spec lists the five sessions above, and
 *  the review corpus's lists these two under Active beside them. Neither carries a `last_seen`,
 *  so both stay live however long a run takes. The titles are invented and name no one. */
const claimHolderSessions: FakeSession[] = [
  {
    capabilities: ["aside"],
    dir: "/workspaces/cache",
    machine_id: "build-host",
    roles: [],
    session_id: sessionIds.cacheWarm,
    title: "🛠️ FIX: Keep the build cache warm when a runner restarts",
  },
  {
    capabilities: ["aside"],
    dir: "/workspaces/untitled",
    machine_id: "review-host",
    roles: [],
    session_id: sessionIds.untitled,
    title: sessionIds.untitled,
  },
];

/** The registry the review corpus writes, which the visual review puts back after Playwright's
 *  own setup cleared it (`e2e/visual.e2e.ts`): the workspace's sessions under their fixed ids and
 *  the claim holders beside them. */
export const reviewSessions: FakeSession[] = [...workspaceSessions, ...claimHolderSessions];

/** A write's options as `id`'s session of `reviewSessions`, through the agent bearer, its title
 *  stamped in `origin` as `seedWorkspace` stamps a session's write: for the review corpus, whose
 *  sessions keep their fixed ids (`sessionIds`), as `claimIssue` of a claim holder takes it
 *  (`e2e/review-seed.ts`). */
export function asWorkspaceSession(id: string): { readonly actor: Actor; readonly as: "agent" } {
  const session = reviewSessions.find((candidate) => candidate.session_id === id);
  if (session === undefined) throw new Error(`asWorkspaceSession: no review session ${id}`);
  return { actor: { id, kind: "session", origin: { session_title: session.title } }, as: "agent" };
}
const workflowSpec =
  "# Local testing workflow\n\nOne command, a seeded corpus, a signed-in browser.\n\n" +
  ':::ask{urgency="high" multiple="false"}\nWhich sign-in path?\n\n' +
  "- Cookie: the production identity, issued by the sign-in pool\n" +
  "- Header: a test/local harness identity\n:::\n";

export interface SeededWorkspace {
  readonly issues: Record<
    "workflow" | "child" | "broadcastOrder" | "snooze" | "folds" | "phone" | "retracted" | "rotate",
    string
  >;
  readonly asks: Record<
    "blocking" | "snoozed" | "answered" | "retracted" | "document" | "rotate",
    string
  >;
  readonly broadcast: { readonly id: string; readonly sessionIds: readonly string[] };
  readonly directMessage: string;
  /** The pass line of the seed's wait for its Inbox rows:
   *  `inbox rows: 5 of 5 declared rows matched in 1 read`. */
  readonly inboxLine: string;
}

/** The five sessions under the ids `sessionId` names, in `workspaceSessions`' order, and the actor
 *  a write of one of them is made as: its id, with its title in `origin`. */
function liveWorkspace(sessionId: (name: WorkspaceSessionName) => string) {
  const live = workspaceSessions.map((session, at) => ({
    ...session,
    session_id: sessionId(workspaceSessionNames[at]),
  }));
  const actor = (id: string, origin: Omit<ActorOrigin, "session_title"> = {}): Actor => {
    const session = live.find((candidate) => candidate.session_id === id);
    if (session === undefined) throw new Error(`liveWorkspace: no workspace session ${id}`);
    return { id, kind: "session", origin: { ...origin, session_title: session.title } };
  };
  return { actor, live };
}

/** Sends the workspace's broadcast as `person` (alice unless the caller names another) to three
 *  of the sessions `sessionId` names, and writes the planner's reply to its copy: the broadcast
 *  `seedWorkspace` sends, and the one a row that needs a broadcast of its own sends again
 *  (`seedReviewBroadcast`, e2e/review-seed.ts). Answers its id and its recipients, in the order
 *  sent. */
export async function sendWorkspaceBroadcast({
  person,
  sessionId = fixedSessionId,
}: {
  readonly person?: SignInLogin;
  readonly sessionId?: (name: WorkspaceSessionName) => string;
} = {}): Promise<SeededWorkspace["broadcast"]> {
  const { actor } = liveWorkspace(sessionId);
  // Not alphabetical on purpose: the broadcast page must list its recipients in the order sent.
  const broadcast = await createBroadcast(
    {
      body: "Stand down and report status.",
      delivery: "steer",
      session_ids: [sessionId("tester"), sessionId("observer"), sessionId("planner")],
    },
    { login: person ?? "alice" }
  );
  if ((broadcast.excluded ?? []).length > 0) {
    throw new Error(
      `sendWorkspaceBroadcast: the broadcast excluded ${JSON.stringify(broadcast.excluded)}`
    );
  }
  const plannerCopy = (broadcast.recipients ?? []).find(
    (recipient) => recipient.session_id === sessionId("planner")
  );
  if (plannerCopy === undefined)
    throw new Error("sendWorkspaceBroadcast: the planner received no copy");
  await replyToMessageDelivery(
    plannerCopy.message.id,
    { attempt: 1, body: "Standing down; build is green." },
    actor(sessionId("planner"))
  );
  return {
    id: broadcast.id,
    sessionIds: (broadcast.recipients ?? []).map((recipient) => recipient.session_id),
  };
}

/** Seeds the workspace into an empty database and the fake Envoy. Every write goes through the
 *  public API, as `person` (alice unless the caller names another) for a human and through the
 *  agent bearer for a session. A session's write carries its title in `origin`, as an agent's
 *  Dispatch tools stamp it (`toolActor` in `packages/envoy-client/src/dispatch-execute.ts`), so
 *  the surfaces that read the stamped title rather than the live registry (the Inbox's author
 *  chip) name it too.
 *
 *  It waits twice for what the document service settles after a write answered: for the spec's
 *  decision block (`awaitDecisionBlock`), and at the end for the Inbox rows it declares
 *  (`awaitSeededInbox`), which every caller reads next. Each wait gets the whole of `boundMs`, the
 *  bound its caller's own checks use, so a browser row's seed gives up before the row's own
 *  clock ends it and hides the wait's message. The abort of `signal` stops the Inbox wait.
 *
 *  `projects` are the keys of its two projects and `person` the human it acts as: a real-stack
 *  row passes its own (`rowKey` and `rowPerson`, e2e/test.ts), and the review corpus, the local
 *  instance and the docs pictures each pass `CORE` and `OPS` as `fixedKey`s naming why, and
 *  take alice. `sessionId` names the five sessions: a real-stack row passes `rowSessionId`
 *  (e2e/test.ts), since Dispatch keeps what a session said and what was sent to it by its id, for
 *  every person at once, and the review corpus, the local instance and the docs pictures take
 *  the fixed ids of `workspaceSessions` (`sessionIds`), which the review corpus's rows and the
 *  visual baselines read. */
export async function seedWorkspace({
  boundMs,
  person,
  projects,
  sessionId = fixedSessionId,
  signal,
}: {
  readonly boundMs: number;
  readonly person?: SignInLogin;
  readonly projects: { readonly core: RowKey; readonly ops: RowKey };
  readonly sessionId?: (name: WorkspaceSessionName) => string;
  readonly signal?: AbortSignal;
}): Promise<SeededWorkspace> {
  const { actor, live } = liveWorkspace(sessionId);
  const as = (id: string) => ({ actor: actor(id), as: "agent" as const });
  const human = { login: person ?? "alice" };
  const { core, ops } = projects;

  await setLiveSessions(live);
  await Promise.all([
    createProject({ key: core, name: "Core" }, human),
    createProject({ key: ops, name: "Ops" }, human),
  ]);

  // A human's issue is assigned to its creator, so every issue here is the person's.
  //
  // The issues are created ONE AT A TIME rather than in one round, and those six round trips
  // are deliberate: the server numbers an issue's key as it inserts it, so a round of seven
  // races and the child is CORE-2 on one run and CORE-3 on the next. Nothing read that order
  // until the visual review began comparing rendered pages against committed baselines
  // (e2e/visual.e2e.ts), and a key printed on a page is a key in a baseline - the first two
  // container runs of that layer differed on the Children tab, the project's issue list and
  // the Inbox, in the key's own digits and nowhere else.
  const workflow = await createIssue(
    { project: core, spec: workflowSpec, title: "Local testing workflow for Dispatch" },
    human
  );
  const child = await createIssue(
    { parent: workflow.key, project: core, title: "Seed the local corpus" },
    human
  );
  const broadcastOrder = await createIssue(
    { project: core, title: "Broadcast recipients keep their send order" },
    human
  );
  const snooze = await createIssue(
    { project: core, title: "Snooze an Inbox row until tomorrow" },
    human
  );
  const folds = await createIssue({ project: core, title: "Agents page freshness folds" }, human);
  const phone = await createIssue(
    { project: core, title: "Phone layout of the review sheet" },
    human
  );
  const retracted = await createIssue(
    { project: core, title: "A retracted ask stays in history" },
    human
  );
  const rotate = await createIssue({ project: ops, title: "Rotate the agent token" }, human);
  // Before the patch below, not after the whole seed: the ask the workflow issue's spec
  // declares is written by the document service on its own, some time after `createIssue`
  // answered, and the patch writes an event of its own against the same issue. Each lands with
  // the sequence number of the moment it happened, and the Activity tab is that sequence
  // newest first, so without this the "asked" and "updated the issue" rows swapped places
  // between two runs of the drift probe. Waiting here puts the ask before the patch in every
  // run.
  const decisionBlockId = await awaitDecisionBlock(
    workflow.key,
    async () => (await getIssue(workflow.key)).open_asks ?? [],
    boundMs
  );
  await Promise.all([
    patchIssue(workflow.key, { labels: ["dispatch"], priority: 1, status: "in_progress" }, human),
    patchIssue(broadcastOrder.key, { status: "needs_review" }, human),
    patchIssue(folds.key, { status: "backlog" }, human),
    patchIssue(phone.key, { status: "done" }, human),
    patchIssue(rotate.key, { status: "triage" }, human),
  ]);

  // Five asks and the runbook, one at a time for the reason the issues above are: an Inbox row
  // is ordered by its ask's own creation, so a round of six ties and two rows of equal urgency
  // swap places between runs. The visual review's baseline of the Inbox is what reads that
  // order, and the first two container runs differed by exactly those two rows.
  const blocking = await createAsk(
    folds.key,
    { question: "Fold a session unseen for ten minutes under Inactive?", urgency: "blocking" },
    { actor: actor(sessionId("planner"), { tmux: "legion:1.2" }), as: "agent" }
  );
  const snoozed = await createAsk(
    snooze.key,
    { question: "Snooze until tomorrow morning, or for a full day?", urgency: "high" },
    as(sessionId("tester"))
  );
  const answered = await createAsk(
    broadcastOrder.key,
    {
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Ship the recipient-order fix?",
    },
    as(sessionId("reviewer"))
  );
  const withdrawn = await createAsk(
    retracted.key,
    { question: "Keep the retracted ask out of the Inbox?" },
    as(sessionId("planner"))
  );
  const rotation = await createAsk(
    rotate.key,
    { question: "Rotate the token before or after the deploy?", urgency: "med" },
    as(sessionId("reviewer"))
  );
  const runbook = await createProjectDocument(
    core,
    { content: "# Runbook\n\nRestart, reseed, reopen the signed-in URL.\n", name: "Runbook" },
    human
  );
  const [documentAsk] = await Promise.all([
    createArtifactAsk(
      runbook.artifact.id,
      { question: "Keep the runbook in the repository?" },
      as(sessionId("tester"))
    ),
    snoozeAsk(snoozed.id, new Date(Date.now() + 24 * 3_600_000).toISOString(), human),
    answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] }, human),
    resolveAsk(
      withdrawn.id,
      { kind: "retracted", reason: "Answered live." },
      as(sessionId("planner"))
    ),
  ]);

  const review = await createComment(
    broadcastOrder.key,
    { body: "Reviewing the diff." },
    as(sessionId("reviewer"))
  );
  await createComment(
    broadcastOrder.key,
    { body: "Thanks - the order the sender ticked is the whole point.", reply_to: review.id },
    human
  );
  const mention = await createComment(
    broadcastOrder.key,
    {
      body: "@Tester please re-run the broadcast spec",
      delivery: "btw",
      mentions: [{ target: `session:${sessionId("tester")}` }],
    },
    human
  );
  await replyToCommentDelivery(
    mention.id,
    { attempt: 1, body: "Re-ran: 2 passed." },
    actor(sessionId("tester"))
  );

  const directMessage = await createAgentMessage(
    sessionId("planner"),
    { body: "How far is the plan?", delivery: "aside" },
    human
  );
  await replyToMessageDelivery(
    directMessage.id,
    { attempt: 1, body: "Half-way; the seed is next." },
    actor(sessionId("planner"))
  );

  const broadcast = await sendWorkspaceBroadcast({ person, sessionId });

  // The rows the Inbox read returns once the seed has settled: the open asks on open issues and
  // documents, the snoozed one among them. Every issue here is the person's, and the document's
  // question waits on CORE's default assignee, the person too, since they made CORE, so their
  // default list is these.
  const inbox: readonly SeededInboxRow[] = [
    { askId: blocking.id, kind: "ask", made: `the blocking ask on ${folds.key}` },
    { askId: snoozed.id, kind: "ask", made: `the snoozed ask on ${snooze.key}` },
    { askId: rotation.id, kind: "ask", made: `the rotate ask on ${rotate.key}` },
    { askId: documentAsk.id, kind: "ask", made: "the runbook document's ask" },
    {
      blockId: decisionBlockId,
      issueKey: workflow.key,
      kind: "block",
      made: `the spec decision block on ${workflow.key}`,
    },
  ];
  // Which read holds exactly these rows: over the emptied database the review corpus, the local
  // instance and the docs seed into, the unfiltered read (every open question of the stack),
  // which also says none of the seed's questions reached anybody else. A real-stack row seeds
  // beside every other row's questions, which that read lists too, so it names a person of its
  // own and reads that person's list (`?assignee=me`, the list the Inbox opens on), which no
  // other row adds to.
  const read = () => (person === undefined ? getInbox(human) : getInbox(human, { assignee: "me" }));
  // Every caller reads the Inbox next, and it can list other rows for a moment after the writes
  // above answered: a 25 ms sampler read five wrong rows here about 230 ms before these five
  // stood (dispatch://INFRA-3052, comment 169c05c4-ab16-4a28-a470-e703dcee23ac). So the seed is
  // done only once these rows stand.
  const matched = await awaitSeededInbox(inbox, read, { boundMs, signal });

  return {
    asks: {
      answered: answered.id,
      blocking: blocking.id,
      document: documentAsk.id,
      retracted: withdrawn.id,
      rotate: rotation.id,
      snoozed: snoozed.id,
    },
    broadcast,
    directMessage: directMessage.id,
    inboxLine: matched.line,
    issues: {
      broadcastOrder: broadcastOrder.key,
      child: child.key,
      folds: folds.key,
      phone: phone.key,
      retracted: retracted.key,
      rotate: rotate.key,
      snooze: snooze.key,
      workflow: workflow.key,
    },
  };
}
