// packages/envoy-client/src/__tests__/dispatch-api-routes.test.ts
//
// The one guard of `dispatch api`'s table of write routes (`WRITE_ROUTES`,
// src/dispatch-commands/api.ts), from which its AskGate refusal names the `dispatch` command that
// makes a write. It drives every method of DispatchClient through a recording fetch and holds the
// table to the writes they send: each write route has a row, and each row is a route some method
// sends. A write method added to DispatchClient later fails here until its route has a row, or it
// joins the methods no `dispatch` command calls, below, with its reason.

import { expect, test } from "bun:test";
import { NO_ROUTE_MESSAGE_PREFIX } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { bearerCredential } from "../bearer";
import { WRITE_ROUTES, writeRoute } from "../dispatch-commands/api";
import { DispatchClient } from "../dispatch-http";

/** The names of DispatchClient's methods: every function member but the injected fetch. */
type ClientMethod = Exclude<
  {
    [K in keyof DispatchClient]: DispatchClient[K] extends (...args: never[]) => unknown
      ? K
      : never;
  }[keyof DispatchClient],
  "fetchImpl"
>;

/** The write methods with no row, each with why: no `dispatch` command calls it, or, for
 *  `sendRoute`, the route it sends is the one its caller names. */
const NO_COMMAND: Readonly<Partial<Record<ClientMethod, string>>> = {
  deliveryReply: "the plugins' answer to a delivery they took (src/delivery.ts)",
  acceptMessageDelivery:
    "Oh My Pi's acceptance of a person's direct message as its user's turn (pi-shared's accept-user-turn.ts)",
  nameArtifactVersion: "no caller outside DispatchClient's own tests",
  sendRoute: "dispatch api's write of the route its caller names (src/dispatch-commands/api.ts)",
};

/** An issue named as a reference, which the recording fetch resolves to DSP-1. */
const ISSUE = "example-org/example-repo#1";
const actor = { kind: "session" as const, id: "session-1" };
const file = { name: "notes.md", file: new Blob(["# Notes"], { type: "text/markdown" }), actor };
const inline = { name: "notes.md", content: "# Notes", actor };
const check = {
  actor,
  check: "k1",
  role: "implementer",
  verdict: "pass",
  commit: "1111111111111111111111111111111111111111",
} as const;

/** One call of each method, with placeholder arguments of the shapes it takes. */
const CALLS: Record<ClientMethod, (client: DispatchClient) => Promise<unknown>> = {
  issue: (client) => client.issue({ project: "DSP", title: "A title", actor }),
  resolveIssue: (client) => client.resolveIssue(ISSUE),
  listIssuePage: (client) => client.listIssuePage({}, { limit: 1, offset: 0 }),
  listAllIssues: (client) => client.listAllIssues({}),
  listProjectArtifacts: (client) => client.listProjectArtifacts("DSP"),
  listIssueArtifacts: (client) => client.listIssueArtifacts(ISSUE),
  markdownRoundTrip: (client) => client.markdownRoundTrip("# Notes\n"),
  projectArtifact: (client) => client.projectArtifact("DSP", inline),
  getProjectArtifact: (client) => client.getProjectArtifact("DSP", "notes"),
  agentArtifact: (client) => client.agentArtifact("session-1", file),
  getAgentArtifact: (client) => client.getAgentArtifact("session-1", "notes"),
  getIssueArtifact: (client) => client.getIssueArtifact(ISSUE, "notes"),
  search: (client) => client.search("notes"),
  related: (client) => client.related("DSP", { title: "A title", text: "Some text." }),
  getIssue: (client) => client.getIssue(ISSUE),
  updateIssue: (client) => client.updateIssue(ISSUE, { status: "testing", actor }),
  changeIssueStatus: (client) =>
    client.changeIssueStatus(ISSUE, { status: "done", include_children: true, actor }),
  claimIssue: (client) => client.claimIssue(ISSUE, { actor }),
  releaseIssueClaim: (client) => client.releaseIssueClaim(ISSUE, { actor }),
  getIssueEvents: (client) => client.getIssueEvents(ISSUE),
  read: (client) => client.read(ISSUE),
  ask: (client) => client.ask(ISSUE, { question: "Which?", actor } as never),
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
  listArchitectureSources: (client) => client.listArchitectureSources("DSP"),
  syncArchitectureSources: (client) => client.syncArchitectureSources("DSP"),
  syncArchitectureSource: (client) => client.syncArchitectureSource("DSP", "acme/infra"),
  resolveAsk: (client) => client.resolveAsk("ask-1", { answer: "This one.", actor } as never),
  requestApproval: (client) => client.requestApproval("artifact-1", { actor, summary: "Spec" }),
  planApprove: (client) => client.planApprove("artifact-1", { actor, version: 2 }),
  reviewArtifact: (client) => client.reviewArtifact("artifact-1", 2, { state: "approved" }),
  recordResult: (client) => client.recordResult(ISSUE, check),
  dryRunResult: (client) => client.dryRunResult(ISSUE, check),
  issueResults: (client) => client.issueResults(ISSUE),
  retractResult: (client) =>
    client.retractResult("result-1", { reason: "The run read a stale build.", actor }),
  issueCoverage: (client) => client.issueCoverage(ISSUE),
  editAsk: (client) => client.editAsk("ask-1", { question: "Which one?", actor } as never),
  comment: (client) => client.comment(ISSUE, { body: "Shipped.", actor }),
  listIssueAsks: (client) => client.listIssueAsks(ISSUE),
  getArtifactAsks: (client) => client.getArtifactAsks("artifact-1"),
  artifactAsk: (client) => client.artifactAsk("artifact-1", { question: "Which?", actor } as never),
  suggest: (client) => client.suggest(ISSUE, { replace_with: "better", actor } as never),
  getArtifactComments: (client) => client.getArtifactComments("artifact-1"),
  artifactComment: (client) => client.artifactComment("artifact-1", { body: "Noted.", actor }),
  artifactSuggest: (client) =>
    client.artifactSuggest("artifact-1", { replace_with: "better", actor } as never),
  message: (client) => client.message(ISSUE, { body: "Shipped.", actor }),
  agentMessage: (client) =>
    client.agentMessage("session-1", { body: "DSP-1: please take this.", delivery: "aside" }),
  messageReply: (client) => client.messageReply("message-1", { body: "Done.", actor } as never),
  deliveryReply: (client) => client.deliveryReply("message", "message-1", { body: "Done." }),
  getMessage: (client) => client.getMessage(ISSUE, "message-1"),
  getMessageThread: (client) => client.getMessageThread("message-1", "session-1"),
  messageBlocks: (client) => client.messageBlocks("message-1", "session-1"),
  acceptMessageDelivery: (client) => client.acceptMessageDelivery("message-1", 1, { actor }),
  artifact: (client) => client.artifact(ISSUE, file),
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
  editComment: (client) => client.editComment("comment-1", { body: "A body", actor }),
  deleteComment: (client) => client.deleteComment("comment-1", actor),
  commentHistory: (client) => client.commentHistory("comment-1"),
  editMessage: (client) => client.editMessage("message-1", { body: "A body", actor }),
  deleteMessage: (client) => client.deleteMessage("message-1", actor),
  messageHistory: (client) => client.messageHistory("message-1", "session-1"),
  issueMessages: (client) => client.issueMessages(ISSUE),
  getComments: (client) => client.getComments(ISSUE),
  getIssueReferences: (client) => client.getIssueReferences(ISSUE),
  getReferences: (client) => client.getReferences({ to: "DSP-1" }),
  writeHandoff: (client) => client.writeHandoff(ISSUE, "implement", { body: "{}", actor }),
  listHandoffs: (client) => client.listHandoffs(ISSUE),
  latestHandoff: (client) => client.latestHandoff(ISSUE, "implement"),
  handoff: (client) => client.handoff("handoff-1"),
  listSteps: (client) => client.listSteps(ISSUE),
  listProjectPlans: (client) =>
    client.listProjectPlans("DSP", {
      since: "2026-11-01T00:00:00Z",
      until: "2026-11-17T00:00:00Z",
    }),
};

/** The route a Dispatch older than it has none for, which `syncArchitectureSources` then sends
 *  its older route in place of. */
const SOURCES_SYNC = "/api/v1/projects/DSP/architecture-sources/sync";

test("every write DispatchClient sends has a row in dispatch api's table", async () => {
  // Every method, which is every own property of the prototype that is one: `token` is an
  // accessor, not a call.
  const methods = Object.getOwnPropertyNames(DispatchClient.prototype).filter((name) => {
    const property = Object.getOwnPropertyDescriptor(DispatchClient.prototype, name);
    return name !== "constructor" && typeof property?.value === "function";
  });
  expect(methods.sort()).toEqual(Object.keys(CALLS).sort());

  const rowless: string[] = [];
  const reached = new Set<string>();
  for (const name of methods as ClientMethod[]) {
    const writes: { readonly method: string; readonly path: string }[] = [];
    const recording = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const { pathname } = new URL(String(input));
      const method = init?.method ?? "GET";
      // A read is answered the least each method reads on: the issue a reference resolves to.
      if (method === "GET") {
        return Response.json(pathname === "/api/v1/issues/resolve" ? { key: "DSP-1" } : {});
      }
      writes.push({ method, path: pathname });
      // Dispatch's no-route answer, so the sync of a project's sources sends its older route too.
      if (method === "POST" && pathname === SOURCES_SYNC) {
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `${NO_ROUTE_MESSAGE_PREFIX}POST ${SOURCES_SYNC}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 }
        );
      }
      return Response.json({});
    }) as typeof fetch;
    // A sleep that waits for nothing, so a write's no-route retries end at once.
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential(fixtureSecret("dsp")),
      recording,
      { sleep: async () => {} }
    );
    await CALLS[name](client).catch(() => undefined);

    if (NO_COMMAND[name] !== undefined) {
      // Still a write method, or its place in the list is stale.
      expect({ name, writes: writes.length > 0 }).toEqual({ name, writes: true });
      continue;
    }
    for (const write of writes) {
      const row = writeRoute(write.method, write.path);
      if (row === undefined) rowless.push(`${name}: ${write.method} ${write.path}`);
      else reached.add(`${row.method} ${row.pattern}`);
    }
  }
  expect(rowless).toEqual([]);
  // Each row is a route some method sends: a row no method sends, or a method the walk no longer
  // reaches, shows here.
  expect([...reached].sort()).toEqual(
    WRITE_ROUTES.map((row) => `${row.method} ${row.pattern}`).sort()
  );
});
