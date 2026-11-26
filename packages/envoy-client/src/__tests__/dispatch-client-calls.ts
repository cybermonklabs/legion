// packages/envoy-client/src/__tests__/dispatch-client-calls.ts
//
// One call of each DispatchClient method, for the tests that drive every method of the class
// through a fetch of their own: dispatch-client-census.test.ts, which reads the key of each write,
// dispatch-api-routes.test.ts, which holds each write's route to a row of `WRITE_ROUTES`, and
// dispatch-proxy.test.ts, which sends each request but a GET through the proxy's socket. A method
// added to the class and left out of `CALLS` fails the census's check of `clientMethodNames`, and
// `tsc` reads `CALLS` as a record of every public method.

import { DispatchClient } from "../dispatch-http";

/** The names of DispatchClient's methods: every function member but the injected fetch. */
export type ClientMethod = Exclude<
  {
    [K in keyof DispatchClient]: DispatchClient[K] extends (...args: never[]) => unknown
      ? K
      : never;
  }[keyof DispatchClient],
  "fetchImpl"
>;

const actor = { kind: "session" as const, id: "session-1" };
const file = { name: "notes.md", file: new Blob(["# Notes"], { type: "text/markdown" }), actor };
const inline = { name: "notes.md", content: "# Notes", actor };

/** One call of each method, with arguments of the shapes it takes; what it answers is not read. */
export const CALLS: Record<ClientMethod, (client: DispatchClient) => Promise<unknown>> = {
  issue: (client) => client.issue({ project: "DSP", title: "A title", actor }),
  resolveIssue: (client) => client.resolveIssue("owner/repo#1"),
  listIssuePage: (client) => client.listIssuePage({}, { limit: 1, offset: 0 }),
  listAllIssues: (client) => client.listAllIssues({}),
  listProjectArtifacts: (client) => client.listProjectArtifacts("DSP"),
  listIssueArtifacts: (client) => client.listIssueArtifacts("DSP-1"),
  markdownRoundTrip: (client) => client.markdownRoundTrip("# Notes\n", false),
  fileRoundTrip: (client) => client.fileRoundTrip(new TextEncoder().encode("# Notes\n"), false),
  projectArtifact: (client) => client.projectArtifact("DSP", inline),
  getProjectArtifact: (client) => client.getProjectArtifact("DSP", "notes"),
  agentArtifact: (client) => client.agentArtifact("session-1", file),
  getAgentArtifact: (client) => client.getAgentArtifact("session-1", "notes"),
  getIssueArtifact: (client) => client.getIssueArtifact("DSP-1", "notes"),
  search: (client) => client.search("notes"),
  related: (client) => client.related("DSP", { title: "A title", text: "Some text." }),
  getIssue: (client) => client.getIssue("DSP-1"),
  updateIssue: (client) => client.updateIssue("DSP-1", { status: "testing", actor }),
  changeIssueStatus: (client) =>
    client.changeIssueStatus("DSP-1", { status: "in_progress", include_parent: true, actor }),
  claimIssue: (client) => client.claimIssue("DSP-1", { actor }),
  releaseIssueClaim: (client) => client.releaseIssueClaim("DSP-1", { actor }),
  getIssueEvents: (client) => client.getIssueEvents("DSP-1"),
  read: (client) => client.read("DSP-1"),
  ask: (client) => client.ask("DSP-1", { question: "Which?", actor } as never),
  openAsks: (client) => client.openAsks("session-1"),
  openAsksForProject: (client) => client.openAsksForProject("DSP"),
  priorities: (client) => client.priorities(["DSP"]),
  sessionAnswers: (client) => client.sessionAnswers("session-1"),
  listAgents: (client) => client.listAgents(),
  whoami: (client) => client.whoami(),
  limits: (client) => client.limits(),
  readRoute: (client) => client.readRoute("/api/v1/issues/DSP-1/events?limit=200"),
  sendRoute: (client) => client.sendRoute("POST", "/api/v1/issues/DSP-1/comments", "{}"),
  inbox: (client) => client.inbox("me"),
  myReplies: (client) => client.myReplies(),
  credentialPending: (client) => client.credentialPending(),
  createProject: (client) => client.createProject({ key: "DSP", name: "Dispatch" } as never),
  listProjects: (client) => client.listProjects(),
  turnOnProvenSessions: (client) => client.turnOnProvenSessions("DSP", { actor }),
  turnOffProvenSessions: (client) => client.turnOffProvenSessions("DSP"),
  listArchitectureSources: (client) => client.listArchitectureSources("DSP"),
  syncArchitectureSources: (client) => client.syncArchitectureSources("DSP"),
  syncArchitectureSource: (client) => client.syncArchitectureSource("DSP", "acme/infra"),
  resolveAsk: (client) => client.resolveAsk("ask-1", { answer: "This one.", actor } as never),
  requestApproval: (client) => client.requestApproval("artifact-1", { actor, summary: "Spec" }),
  planApprove: (client) => client.planApprove("artifact-1", { actor, version: 2 }),
  reviewArtifact: (client) => client.reviewArtifact("artifact-1", 2, { state: "approved" }),
  recordResult: (client) =>
    client.recordResult("DSP-1", {
      actor,
      check: "k1",
      role: "implementer",
      verdict: "pass",
      commit: "1111111111111111111111111111111111111111",
    }),
  dryRunResult: (client) =>
    client.dryRunResult("DSP-1", {
      actor,
      check: "k1",
      role: "implementer",
      verdict: "pass",
      commit: "1111111111111111111111111111111111111111",
    }),
  issueResults: (client) => client.issueResults("DSP-1"),
  recordDeploy: (client) =>
    client.recordDeploy({ repo: "acme/widgets", program: "worker", commit: "1111111111111111111111111111111111111111" }),
  deploys: (client) => client.deploys("acme/widgets"),
  retractResult: (client) =>
    client.retractResult("result-1", { reason: "The run read a stale build.", actor }),
  issueCoverage: (client) => client.issueCoverage("DSP-1"),
  editAsk: (client) => client.editAsk("ask-1", { question: "Which one?", actor } as never),
  comment: (client) => client.comment("DSP-1", { body: "Shipped.", actor }),
  listIssueAsks: (client) => client.listIssueAsks("DSP-1"),
  getArtifactAsks: (client) => client.getArtifactAsks("artifact-1"),
  artifactAsk: (client) => client.artifactAsk("artifact-1", { question: "Which?", actor } as never),
  suggest: (client) => client.suggest("DSP-1", { replace_with: "better", actor } as never),
  getArtifactComments: (client) => client.getArtifactComments("artifact-1"),
  artifactComment: (client) => client.artifactComment("artifact-1", { body: "Noted.", actor }),
  artifactSuggest: (client) =>
    client.artifactSuggest("artifact-1", { replace_with: "better", actor } as never),
  message: (client) => client.message("DSP-1", { body: "Shipped.", actor }),
  agentMessage: (client) =>
    client.agentMessage("session-1", { body: "DSP-1: please take this.", delivery: "aside" }),
  messageReply: (client) => client.messageReply("message-1", { body: "Done.", actor } as never),
  deliveryReply: (client) => client.deliveryReply("message", "message-1", { body: "Done." }),
  getMessage: (client) => client.getMessage("DSP-1", "message-1"),
  getMessageThread: (client) => client.getMessageThread("message-1", "session-1"),
  editMessage: (client) => client.editMessage("message-1", { body: "Corrected.", actor }),
  deleteMessage: (client) => client.deleteMessage("message-1", actor),
  messageHistory: (client) => client.messageHistory("message-1", "session-1"),
  issueMessages: (client) => client.issueMessages("DSP-1"),
  messageBlocks: (client) => client.messageBlocks("message-1", "session-1"),
  acceptMessageDelivery: (client) => client.acceptMessageDelivery("message-1", 1, { actor }),
  artifact: (client) => client.artifact("DSP-1", file),
  getArtifact: (client) => client.getArtifact("artifact-1"),
  docRead: (client) => client.docRead("artifact-1"),
  docReadWithBlocks: (client) => client.docReadWithBlocks("artifact-1"),
  fileVersion: (client) => client.fileVersion("artifact-1", 1),
  artifactBlocks: (client) => client.artifactBlocks("artifact-1"),
  issueArtifactBlocks: (client) => client.issueArtifactBlocks("DSP-1", "spec"),
  projectArtifactBlocks: (client) => client.projectArtifactBlocks("DSP", "notes-md"),
  docEdit: (client) =>
    client.docEdit("artifact-1", { ops: [{ op: "delete", find: "old" }], actor } as never),
  nameArtifactVersion: (client) =>
    client.nameArtifactVersion("artifact-1", { name: "v1", actor } as never),
  getArtifactEvents: (client) => client.getArtifactEvents("artifact-1"),
  getArtifactReferences: (client) => client.getArtifactReferences("artifact-1"),
  getAsk: (client) => client.getAsk("ask-1"),
  followAsk: (client) => client.followAsk("ask-1", "session-1", actor),
  unfollowAsk: (client) => client.unfollowAsk("ask-1", "session-1", actor),
  getComment: (client) => client.getComment("comment-1"),
  commentBlocks: (client) => client.commentBlocks("comment-1"),
  resolveComment: (client) => client.resolveComment("comment-1", actor),
  editComment: (client) => client.editComment("comment-1", { body: "Corrected.", actor }),
  deleteComment: (client) => client.deleteComment("comment-1", actor),
  commentHistory: (client) => client.commentHistory("comment-1"),
  getComments: (client) => client.getComments("DSP-1"),
  getIssueReferences: (client) => client.getIssueReferences("DSP-1"),
  getReferences: (client) => client.getReferences({ to: "DSP-1" }),
  writeHandoff: (client) => client.writeHandoff("DSP-1", "implement", { body: "{}", actor }),
  listHandoffs: (client) => client.listHandoffs("DSP-1"),
  latestHandoff: (client) => client.latestHandoff("DSP-1", "implement"),
  handoff: (client) => client.handoff("handoff-1"),
  listSteps: (client) => client.listSteps("DSP-1"),
  listProjectPlans: (client) =>
    client.listProjectPlans("DSP", {
      since: "2026-11-01T00:00:00Z",
      until: "2026-11-17T00:00:00Z",
    }),
};

/** The name of each method of DispatchClient: each own property of its prototype that is a
 *  function. `token` is an accessor (the secret the client presents, which a broker credential
 *  renews), not a call. */
export function clientMethodNames(): string[] {
  return Object.getOwnPropertyNames(DispatchClient.prototype).filter((name) => {
    const property = Object.getOwnPropertyDescriptor(DispatchClient.prototype, name);
    return name !== "constructor" && typeof property?.value === "function";
  });
}
