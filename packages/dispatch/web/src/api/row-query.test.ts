import { expect, type Mock, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient } from "@tanstack/react-query";

import { jump, onFakeClock, settle } from "../__tests__/fake-clock";
import { answerSends, type SendAnswers } from "../__tests__/send-answers";
import { codeMask, type ObjectLiteral, objectAt, shippedSources } from "../__tests__/source-scan";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { plainWrite, type Writes, writesOf } from "../features/writes/write-store";
import { api, issuePatchRequest, type WriteRequest } from "./client";
import {
  architectureSourcesQuery,
  architectureTreeQuery,
  askThreadQuery,
  deliverySettingsQuery,
  dismissedThreadsQuery,
  inboxQuery,
  inboxThreadsQuery,
  issueQuery,
  myRepliesCountQuery,
  projectIssuesQuery,
  repoProjectsQuery,
  userStateQuery,
} from "./queries";
import { rowQuery } from "./row-query";

/**
 * Every cache read of a row the page writes is built by one factory (`api/row-query.ts`, R7 of
 * dispatch://INFRA-299): it derives the lines a key covers from the key's own head and holds the
 * fetch until the store holds no write of them that may still land. This file holds that door
 * from both sides - the call-site row below names every query the app makes that is built
 * outside the factory, and the rows beneath it read what each reader waits for.
 */

/** Every call of `names` in `source` that is written over an object literal, with that object.
 *  A generic form (`useQuery<Row[]>({…})`) is one of them. */
function callsOf(path: string, source: string, names: RegExp): readonly ObjectLiteral[] {
  const found: ObjectLiteral[] = [];
  const code = codeMask(source);
  for (const match of source.matchAll(names)) {
    const start = match.index ?? 0;
    if (code[start] !== true) continue;
    const end = start + match[0].length;
    const ahead = source.slice(end, end + 400);
    const call = ahead.indexOf("({");
    if (call === -1) continue;
    const between = ahead.slice(0, call).trim();
    const generic = between === "" || (between.startsWith("<") && between.endsWith(">"));
    if (!generic || between.includes("(") || between.includes(")")) continue;
    const line = source.slice(0, end).split("\n").length;
    found.push(objectAt(source, end + call + 1, `${path}:${line}`));
  }
  return found;
}

/** One cache read the app builds itself: where it is written and the key it names, as written. */
interface Reader {
  readonly at: string;
  readonly key: string;
}

/** The names a cache read is built under: TanStack's own builders and hooks. `rowQuery` and
 *  `rowInfiniteQuery` are not among them - a read built by the factory is the point. */
const READS =
  /\b(useQuery|useQueries|useInfiniteQuery|useSuspenseQuery|useSuspenseInfiniteQuery|queryOptions|infiniteQueryOptions|fetchQuery|fetchInfiniteQuery|prefetchQuery|prefetchInfiniteQuery|ensureQueryData|ensureInfiniteQueryData)\b/gu;

/** Every read `source` builds itself: a call of `READS` whose own object names the key. A call
 *  that spreads a `rowQuery(…)` names none of its own and is the factory's. `useQueries` names
 *  one key per entry of its `queries` array, each of them a read in its own right. */
function handBuiltReads(path: string, source: string): readonly Reader[] {
  const readers: Reader[] = [];
  for (const object of callsOf(path, source, READS)) {
    const key = object.fields.get("queryKey");
    if (key !== undefined) {
      readers.push({ at: path, key: key.replaceAll(/\s+/gu, " ") });
      continue;
    }
    const queries = object.fields.get("queries");
    if (queries === undefined) continue;
    for (const inner of queries.matchAll(/queryKey:\s*([^\n]*?),?\s*$/gmu)) {
      readers.push({ at: path, key: (inner[1] ?? "").replaceAll(/\s+/gu, " ") });
    }
  }
  return readers;
}

/**
 * R7 names the readers the factory covers: the issue read, the project issue lists, the Inbox
 * with its thread lists and its count, the viewer's state, the architecture tree, the ask
 * threads, the margin's comments list, and the settings lists the writes of R6 change. Every
 * other read the app makes is named here, by the module it is written in and the key it names,
 * so a reader written later is either built by the factory or written down here with its reason.
 * Moving one of these onto the factory changes the set R7 approved, which is the issue's to
 * decide, not this file's.
 */
const NOT_COVERED: readonly string[] = [
  'api/queries.ts  ["agents", sessionId, "messages"]',
  'api/queries.ts  ["artifact", id, "blocks"]',
  'api/queries.ts  ["delivery", "measures", view]',
  'api/queries.ts  ["delivery", "pipeline", view]',
  'api/queries.ts  ["delivery", "run", id]',
  'api/queries.ts  ["delivery", "timeline", view]',
  'api/queries.ts  ["issue", key, "coverage"]',
  'api/queries.ts  ["issue", key, "results"]',
  'api/queries.ts  ["new-issue-settings"]',
  'api/queries.ts  ["user-agent-state"]',
  'api/queries.ts  ["users"]',
  'api/queries.ts  ["whoami"]',
  'features/activity/ActivityTab.tsx  ["events", issueKey]',
  'features/agents/AgentMessageComposer.tsx  ["agents", "issue-picker"]',
  'features/agents/BroadcastPage.tsx  ["broadcast", id]',
  'features/agents/BroadcastSends.tsx  ["broadcast", created.id]',
  'features/agents/BroadcastsPage.tsx  ["broadcast", "list"]',
  'features/artifacts/ArtifactDetails.tsx  ["artifact", artifact.id, "version", afterVersion]',
  'features/artifacts/ArtifactDetails.tsx  ["artifact", artifact.id, "version", beforeVersion]',
  'features/artifacts/ArtifactsTab.tsx  ["artifacts", issueKey]',
  'features/artifacts/ArtifactsTab.tsx  ["issue", issueKey, "references"]',
  'features/conversation/ConversationTab.tsx  ["asks", issueKey, "thread"]',
  'features/conversation/ConversationTab.tsx  ["events", issueKey, "head"]',
  'features/conversation/ConversationTab.tsx  ["events", issueKey, "unread-boundary", lastReadSeq]',
  'features/conversation/ConversationTab.tsx  ["handoff", handoffId]',
  'features/conversation/ConversationTab.tsx  ["handoffs", issueKey]',
  'features/conversation/ConversationTab.tsx  ["issue-messages", issueKey]',
  'features/doc/ApprovalChip.tsx  ["artifact-reviews", artifactId]',
  'features/doc/ProofDocument.tsx  ["artifact", artifact.id, "version", version]',
  'features/doc/ProofDocument.tsx  ["artifact", artifact.id]',
  'features/doc/ProofDocument.tsx  ["artifact", artifactId, "text"]',
  'features/doc/ProofDocument.tsx  ["block-schema"]',
  'features/doc/ProofDocument.tsx  owner.kind === "issue" ? ["asks", owner.key] : ["artifact", artifact.id, "asks"]',
  'features/doc/VersionView.tsx  ["artifact", artifactId, "version", version]',
  'features/doc/spec-marks.ts  ["artifact", artifact.id, "blocks"]',
  'features/doc/spec-marks.ts  ["references", "to", specReference, "part_of"]',
  "features/doc/spec-marks.ts  readKey",
  // biome-ignore lint/suspicious/noTemplateCurlyInString: the key as its module writes it.
  'features/document/useProjectArtifact.ts  ["artifact-ref", `${route?.project}/${route?.slug}`]',
  'features/inbox/EarlierApproval.tsx  ["artifact", artifactId, "version", approvedVersion]',
  'features/inbox/EarlierApproval.tsx  ["artifact", artifactId, "version", version]',
  'features/inbox/PaneDocument.tsx  ["artifact", artifactId]',
  'features/inbox/ReadingPane.tsx  ["artifact", artifactId, "version", approvedVersion]',
  'features/inbox/ReadingPane.tsx  ["artifact", artifactId, "version", latestVersion]',
  'features/inbox/ReadingPane.tsx  ["block-schema"]',
  "features/inbox/ThreadBody.tsx  messagesKey",
  'features/issue/DecisionsPanel.tsx  ["decisions", issueKey]',
  'features/issue/GitHubLink.tsx  ["github-link", link.url]',
  'features/issue/GitHubLink.tsx  ["github-link-checks", link.url, headSHA]',
  'features/issue/RelatedItems.tsx  ["related", project, debounced]',
  'features/margin/useAnsweredAsks.ts  owner?.kind === "document" ? ["artifact", owner.artifactId, "asks"] : ["asks", owner?.kind === "issue" ? owner.key : undefined]',
  'features/margin/useMarginItems.ts  ["events", issueKey, "margin-pinned", pinnedIds]',
  'features/margin/useMarginItems.ts  ["issue-messages", issueKey, "margin-pinned", pinnedMessageIds]',
  'features/project/issues-table-data.ts  ["issue-ancestors", project, ...missingParents]',
  'features/refs/ReferencePicker.tsx  ["artifacts", issue.key]',
  'features/refs/Unfurl.tsx  ["github", path]',
  'features/refs/agent-registry.ts  ["agents"]',
  'features/refs/reference-target.ts  ["agent-artifacts", session, slug]',
  'features/refs/reference-target.ts  ["artifact", id, version ?? "text"]',
  'features/refs/reference-target.ts  ["doc-summary", project, slug]',
  'features/refs/reference-target.ts  ["issue-message", key, id]',
  'features/refs/reference-target.ts  ["issue-summary", key]',
  'features/refs/reference-target.ts  ["references", "questions", source?.kind, source?.id] as const',
  'features/refs/reference-target.ts  ["references", "to", reference] as const',
  'features/search/useTieredSearch.ts  ["search", debouncedQuery, keywordRequest]',
  'features/search/useTieredSearch.ts  ["search", restedQuery, request]',
];

/** The two R7 leaves out in its own words: "The subscribers list and the collaborators list are
 *  not among them, because no optimistic write touches either." */
const NO_OPTIMISTIC_WRITE: readonly string[] = [
  'api/queries.ts  ["issue", key, "collaborators"]',
  'features/document/DocumentPage.tsx  ["subscribers", artifact.data?.id]',
  'features/inbox/AskRecipients.tsx  ["subscribers", subscription?.id]',
  'features/issue/IssueHeader.tsx  ["subscribers", issue.key]',
];

/** Lists whose writes name no row line at all (`unlined`, with the reason in the write's own
 *  value), so there is no line for a reader to hold its fetch by: the credential decisions and
 *  revokes, the project create, and a project's default-assignee pick, which is ordered by its
 *  scope rather than by a line. */
const UNLINED_WRITES: readonly string[] = [
  'api/queries.ts  ["project-default-assignees"]',
  'api/queries.ts  ["projects"]',
  'features/credentials/capabilities.ts  ["credential-capabilities"]',
  'features/credentials/grants.ts  ["credential-grants"]',
  'features/credentials/machineLogins.ts  ["machine-logins"]',
  'features/credentials/pending.ts  ["credential-pending"]',
  'features/credentials/signins.ts  ["signins"]',
  'features/project/DocumentList.tsx  ["project", project, "artifacts"]',
];

/** Every read the app builds itself, whatever the reason. */
const BUILT_BY_HAND: readonly string[] = [
  ...NOT_COVERED,
  ...NO_OPTIMISTIC_WRITE,
  ...UNLINED_WRITES,
];

const handBuilt = shippedSources().flatMap((path) =>
  handBuiltReads(path, readFileSync(join(import.meta.dir, "..", path), "utf8"))
);

/** A reader as this file names one: its module and the key it writes. */
const shown = (reader: Reader) => `${reader.at}  ${reader.key}`;

// R7. Every cache read of a row is built by the factory, and a reader built outside it is named
// above with the reason it is. The types cannot hold this door: a hand-written
// `useQuery({ queryKey: … })` compiles, so this row is what names one written later.
test("every query the app makes is built by the factory or named here with its reason", () => {
  expect(handBuilt.length).toBeGreaterThan(20);
  expect(handBuilt.map(shown).sort()).toEqual([...BUILT_BY_HAND].sort());
});

/** The request every write of these rows is made from: its path says nothing, since the line a
 *  write stands on is `target`, never its route. */
const PATCH: WriteRequest = {
  body: { status: "done" },
  method: "PATCH",
  path: "/api/v1/issues/CORE-1",
};

/** A page: one query client, its write store, and the answers its writes are made with. */
function page(): {
  readonly client: QueryClient;
  readonly sends: SendAnswers;
  readonly writes: Writes;
} {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return { client, sends: answerSends(), writes: writesOf(client) };
}

/** Presses one write of the row `target` names, answered when the test resolves `answer`. */
function writeOn(
  { sends, writes }: { readonly sends: SendAnswers; readonly writes: Writes },
  target: string,
  answer: Promise<unknown>,
  request: WriteRequest = { ...PATCH }
): Promise<unknown> {
  return writes.run({
    detail: null,
    kind: plainWrite,
    label: `a write on ${target}`,
    presentation: "indicator",
    request: sends.answer(request, () => answer),
    target,
  });
}

/** What a reader of this file reads: a server call, mocked with the value the server still holds
 *  - the older one, from before the write that is out. */
type Served = Mock<(...args: never[]) => Promise<unknown>>;

/** One reader R7 names: the line a write of a row its key covers stands on, the server call its
 *  fetch makes, and the fetch itself. */
interface ReaderRow {
  readonly name: string;
  readonly line: string;
  readonly serve: () => Served;
  readonly fetch: (client: QueryClient) => Promise<unknown>;
}

const READERS: readonly ReaderRow[] = [
  {
    fetch: (client) => client.fetchQuery(issueQuery("CORE-1")),
    line: "issue:CORE-1",
    name: "the issue read",
    serve: () => spyOn(api, "getIssue").mockResolvedValue({ key: "CORE-1" } as never),
  },
  {
    fetch: (client) => client.fetchQuery(projectIssuesQuery("CORE", [])),
    line: "issue:CORE-1",
    name: "a project's issue list",
    serve: () => spyOn(api, "listIssues").mockResolvedValue([]),
  },
  {
    fetch: (client) => client.fetchQuery(projectIssuesQuery("CORE", [])),
    line: "project:CORE",
    name: "a project's issue list, for a create of a row in the project",
    serve: () => spyOn(api, "listIssues").mockResolvedValue([]),
  },
  {
    fetch: (client) => client.fetchQuery(inboxQuery()),
    line: "issue:CORE-1",
    name: "the Inbox",
    serve: () => spyOn(api, "getInbox").mockResolvedValue([]),
  },
  {
    // R7. **Dismiss** takes rows out of the Inbox's reads before the server has taken them
    // (`features/inbox/useInboxDismiss.ts`, `inbox:dismiss`), so a read that starts while it is
    // out must not answer with the rows it took.
    fetch: (client) => client.fetchQuery(inboxQuery()),
    line: "inbox:dismiss",
    name: "the Inbox, for a Dismiss of its rows",
    serve: () => spyOn(api, "getInbox").mockResolvedValue([]),
  },
  {
    fetch: (client) => client.fetchQuery(inboxThreadsQuery()),
    line: "ask:a1",
    name: "the Inbox's thread list",
    serve: () => spyOn(api, "listMyReplies").mockResolvedValue({ rows: [], total: 0 } as never),
  },
  {
    // R7. **Mark all read** draws every row it marks as read (`features/inbox/mark-all.ts`,
    // `inbox:read`) before the server has taken the press.
    fetch: (client) => client.fetchQuery(inboxThreadsQuery()),
    line: "inbox:read",
    name: "the Inbox's thread list, for a Mark all read",
    serve: () => spyOn(api, "listMyReplies").mockResolvedValue({ rows: [], total: 0 } as never),
  },
  {
    fetch: (client) => client.fetchInfiniteQuery(dismissedThreadsQuery()),
    line: "ask:a1",
    name: "the Inbox's dismissed list",
    serve: () => spyOn(api, "listMyReplies").mockResolvedValue({ rows: [], total: 0 } as never),
  },
  {
    fetch: (client) => client.fetchQuery(myRepliesCountQuery()),
    line: "issue:CORE-1",
    name: "the Inbox's count",
    serve: () => spyOn(api, "listMyReplies").mockResolvedValue({ rows: [], total: 0 } as never),
  },
  {
    fetch: (client) => client.fetchQuery(userStateQuery()),
    line: "pin:CORE-1",
    name: "the viewer's state",
    serve: () => spyOn(api, "getMyState").mockResolvedValue({}),
  },
  {
    fetch: (client) => client.fetchQuery(architectureTreeQuery("CORE")),
    line: "architecture-source:CORE/acme/web",
    name: "the architecture tree",
    serve: () => spyOn(api, "getArchitecture").mockResolvedValue({} as never),
  },
  {
    fetch: (client) => client.fetchQuery(architectureSourcesQuery()),
    line: "architecture-source:CORE/acme/web",
    name: "the architecture sources settings list",
    serve: () => spyOn(api, "listArchitectureSources").mockResolvedValue([]),
  },
  {
    fetch: (client) => client.fetchQuery(askThreadQuery("a1")),
    line: "ask:a1",
    name: "an ask thread",
    serve: () => spyOn(api, "getAsk").mockResolvedValue({ id: "a1" } as never),
  },
  {
    // The margin builds this key inside its own hook (`features/margin/useMarginItems.ts`); the
    // wait comes from the key's head, so the key is what this row reads it by.
    fetch: (client) =>
      client.fetchQuery(
        rowQuery({
          queryKey: ["comments", "CORE-1"],
          queryFn: () => api.listComments("CORE-1"),
        })
      ),
    line: "comment:c1",
    name: "the margin's comments list",
    serve: () => spyOn(api, "listComments").mockResolvedValue([]),
  },
  {
    fetch: (client) => client.fetchQuery(repoProjectsQuery()),
    line: "repo-mapping:acme/web",
    name: "the repository mappings settings list",
    serve: () => spyOn(api, "listRepoProjects").mockResolvedValue([]),
  },
  {
    fetch: (client) => client.fetchQuery(deliverySettingsQuery()),
    line: "settings:delivery",
    name: "the delivery settings",
    serve: () => spyOn(api, "getDeliverySettings").mockResolvedValue(null),
  },
];

// R7. Every reader the requirement names holds its fetch while a write of a row its key covers
// is out, and reads the server's value once the write has landed. One row each, by the line the
// write stands on.
for (const reader of READERS) {
  test(`${reader.name} holds its fetch while a write of ${reader.line} is out`, async () => {
    const app = page();
    const served = reader.serve();
    try {
      const answer = Promise.withResolvers<unknown>();
      const write = writeOn(app, reader.line, answer.promise);
      await settle();
      expect(app.writes.rows().map((row) => row.status)).toEqual(["sending"]);

      const read = reader.fetch(app.client);
      await settle();
      expect(served).not.toHaveBeenCalled();

      answer.resolve({ key: "CORE-1" });
      await write;
      await settle();
      await read;
      expect(served).toHaveBeenCalledTimes(1);
    } finally {
      served.mockRestore();
      app.sends.restore();
    }
  });
}

// R7. A reader written later takes the wait without naming it: this one is built here, in a
// test, nothing registers it anywhere, and it waits for its key's lines all the same.
test("a reader the app does not ship takes the wait from its key head, with nothing registered for it", async () => {
  const app = page();
  const served = spyOn(api, "getIssueCoverage").mockResolvedValue({} as never);
  try {
    const answer = Promise.withResolvers<unknown>();
    const write = writeOn(app, "issue:CORE-1", answer.promise);
    await settle();
    const read = app.client.fetchQuery(
      rowQuery({
        queryKey: ["issue", "CORE-1"],
        queryFn: () => api.getIssueCoverage("CORE-1"),
      })
    );
    await settle();
    expect(served).not.toHaveBeenCalled();

    answer.resolve({});
    await write;
    await settle();
    await read;
    expect(served).toHaveBeenCalledTimes(1);
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A key head the factory has no rule for does not compile: `RowQueryHead` is the union of the
// heads `COVERS` maps, so a reader of a row nobody wrote a rule for is a type error rather than a
// read that silently waits for nothing. The whole of this row is the typed call below, and only
// `bun run typecheck` reads it: it fails if the call ever compiles, since the directive is unused
// the moment a rule for `receipts` exists. There is nothing here for the runtime to refute, so
// this row asserts nothing.
test("a key head the factory has no rule for does not compile", () => {
  rowQuery({
    // @ts-expect-error - `receipts` is no head of `RowQueryHead`: nothing says what it covers.
    queryKey: ["receipts", "r1"],
    queryFn: () => Promise.resolve(null),
  });
});

// R7. A write waiting its turn behind an earlier write of the same row holds the fetch too: it
// may still land, and the server's list does not hold it yet.
test("a write waiting its turn holds the fetch", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    const first = Promise.withResolvers<unknown>();
    const second = Promise.withResolvers<unknown>();
    const out = writeOn(app, "issue:CORE-1", first.promise);
    const queued = writeOn(app, "issue:CORE-1", second.promise, {
      ...PATCH,
      body: { status: "review" },
    });
    await settle();
    expect(app.writes.rows().map((row) => row.status)).toEqual(["sending", "queued"]);

    const read = app.client.fetchQuery(projectIssuesQuery("CORE", []));
    await settle();
    expect(served).not.toHaveBeenCalled();

    first.resolve({});
    await out;
    await settle();
    // The queued write is out now, and the fetch is still waiting: the server has not taken it.
    expect(served).not.toHaveBeenCalled();
    second.resolve({});
    await queued;
    await settle();
    await read;
    expect(served).toHaveBeenCalledTimes(1);
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A write that is late and still being tried holds the fetch: the attempt out may still land.
test("a write that is late and still being tried holds the fetch", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    await onFakeClock(async () => {
      const answer = Promise.withResolvers<unknown>();
      const write = writeOn(app, "issue:CORE-1", answer.promise);
      await settle();
      await jump(SEND_DEADLINE_MS + 1);
      expect(app.writes.rows().map((row) => row.status)).toEqual(["late"]);

      const read = app.client.fetchQuery(projectIssuesQuery("CORE", []));
      await settle();
      expect(served).not.toHaveBeenCalled();

      answer.resolve({});
      await write;
      await settle();
      await read;
      expect(served).toHaveBeenCalledTimes(1);
    });
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A refused write holds nothing: the server never took it, so its answer is the truth.
test("a refused write holds no fetch", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    const write = writeOn(app, "issue:CORE-1", Promise.reject(new Error("refused")));
    write.catch(() => {});
    await settle();
    expect(app.writes.rows().map((row) => row.status)).toEqual(["refused"]);

    await app.client.fetchQuery(projectIssuesQuery("CORE", []));
    expect(served).toHaveBeenCalledTimes(1);
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A write the reader discarded holds nothing: they took it back.
test("a discarded write holds no fetch", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    const write = writeOn(app, "issue:CORE-1", Promise.reject(new Error("refused")));
    write.catch(() => {});
    await settle();
    const refused = app.writes.rows()[0];
    app.writes.discard(refused?.id ?? 0);
    await settle();
    expect(app.writes.rows()).toEqual([]);

    await app.client.fetchQuery(projectIssuesQuery("CORE", []));
    expect(served).toHaveBeenCalledTimes(1);
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A write the reader stopped holds the fetch until the attempt already out answers, and
// nothing after it: Stop trying ends the retries, not the request on the wire.
test("a write the reader stopped holds the fetch until the attempt already out answers", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    await onFakeClock(async () => {
      const answer = Promise.withResolvers<unknown>();
      const write = writeOn(app, "issue:CORE-1", answer.promise);
      write.catch(() => {});
      await settle();
      await jump(SEND_DEADLINE_MS + 1);
      const late = app.writes.rows()[0];
      expect(app.writes.stop(late?.id ?? 0)).toBe(true);
      await settle();
      const stopping = app.writes.rows().map((row) => row.status === "late" && row.stopping);
      expect(stopping).toEqual([true]);

      const read = app.client.fetchQuery(projectIssuesQuery("CORE", []));
      await settle();
      expect(served).not.toHaveBeenCalled();

      // The attempt already out answers: the write is done being tried, and the held fetch goes.
      answer.resolve({});
      await settle();
      await read;
      expect(served).toHaveBeenCalledTimes(1);
    });
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A read mark holds nothing: `read:issue:<KEY>` is the reader's own row, and it changes no
// value a person chose.
test("a read mark holds no fetch", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    const answer = Promise.withResolvers<unknown>();
    const write = writeOn(app, "read:issue:CORE-1", answer.promise);
    await settle();
    expect(app.writes.rows().map((row) => [row.target, row.status])).toEqual([
      ["read:issue:CORE-1", "sending"],
    ]);

    await app.client.fetchQuery(projectIssuesQuery("CORE", []));
    expect(served).toHaveBeenCalledTimes(1);
    answer.resolve({});
    await write;
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A line names one row, and a reader waits for the lines its own key covers. A Dispatch key
// is `<PROJECT>-<n>`, so one key is the start of another the moment a project passes nine issues
// (INFRA-29 and INFRA-299 are both issues of INFRA), and the issue read's line is the key itself
// with nothing after it. A write of the longer key must hold the shorter key's own read for
// nothing: the wait has no ceiling, so a write of INFRA-299 that is late holds INFRA-29's page
// for as long as it is tried.
test("a write of an issue whose key starts with this one's holds no fetch of this issue", async () => {
  const app = page();
  const served = spyOn(api, "getIssue").mockResolvedValue({ key: "CORE-1" } as never);
  try {
    const answer = Promise.withResolvers<unknown>();
    const write = writeOn(app, "issue:CORE-10", answer.promise, {
      body: { status: "done" },
      method: "PATCH",
      path: "/api/v1/issues/CORE-10",
    });
    await settle();

    const read = app.client.fetchQuery(issueQuery("CORE-1"));
    await settle();
    // Read before the answer, and asserted after the write and the read are both out of the
    // way: a fetch this held would otherwise be left in flight by the failure itself.
    const servedWhileOut = served.mock.calls.length;

    answer.resolve({});
    await write;
    await settle();
    await read;
    expect(servedWhileOut, "the read of CORE-1 while a write of CORE-10 was out").toBe(1);
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});

// R7. A write of another project's issue holds this project's list not at all, and the Inbox's
// fetch all the same: the Inbox lists every project.
test("a write of another project's issue holds the Inbox but not this project's list", async () => {
  const app = page();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  try {
    const answer = Promise.withResolvers<unknown>();
    const write = writeOn(app, "issue:DOCS-7", answer.promise);
    await settle();

    await app.client.fetchQuery(projectIssuesQuery("CORE", []));
    expect(listIssues).toHaveBeenCalledTimes(1);

    const inbox = app.client.fetchQuery(inboxQuery());
    await settle();
    expect(getInbox).not.toHaveBeenCalled();

    answer.resolve({});
    await write;
    await settle();
    await inbox;
    expect(getInbox).toHaveBeenCalledTimes(1);
  } finally {
    listIssues.mockRestore();
    getInbox.mockRestore();
    app.sends.restore();
  }
});

// R7. The board move waits by this one rule and no other: it is a write of the issue's own
// line, with no mutation key and no wait of its own.
test("the board move holds the project list's fetch, by the issue's own line", async () => {
  const app = page();
  const served = spyOn(api, "listIssues").mockResolvedValue([]);
  try {
    const answer = Promise.withResolvers<unknown>();
    const move = writeOn(
      app,
      "issue:CORE-1",
      answer.promise,
      issuePatchRequest("CORE-1", { status: "doing" })
    );
    await settle();

    const read = app.client.fetchQuery(projectIssuesQuery("CORE", []));
    await settle();
    expect(served).not.toHaveBeenCalled();

    answer.resolve({ key: "CORE-1" });
    await move;
    await settle();
    await read;
    expect(served).toHaveBeenCalledTimes(1);
  } finally {
    served.mockRestore();
    app.sends.restore();
  }
});
