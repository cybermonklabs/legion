/**
 * What the landing read does for a page (`api/queries.ts`): one request answers the viewer, the
 * shell and the page the browser is loading, and every part of that answer is put under the key
 * the route it stands in for already uses. A key seeded that way is fresh, so the read a
 * component makes when it mounts is answered out of the cache and sends no request: these rows
 * make each of those reads and prove that no `api` method behind it was called.
 *
 * A read of a key is made here with `fetchQuery`, which decides whether to fetch by the rule a
 * mounting `useQuery` decides by - is this key's data older than `staleTime` - so a key these
 * rows read without a request is a key a component mounts without one.
 */

import { expect, spyOn, test } from "bun:test";
import { QueryClient, queryOptions } from "@tanstack/react-query";

import { ApiError, api } from "../api/client";
import {
  artifactBlocksQuery,
  inboxQuery,
  inboxThreadsQuery,
  issueCoverageQuery,
  issueQuery,
  myRepliesCountQuery,
  projectArchitectureSourcesQuery,
  projectIssuesQuery,
  projectsQuery,
  readLanding,
  userAgentStateQuery,
  userStateQuery,
  whoAmIQuery,
} from "../api/queries";
import type {
  ArchitectureSource,
  Artifact,
  AuthenticatedUser,
  IssueCoverage,
  LandingResponse,
  LandingShell,
  MyRepliesResponse,
  RepoProject,
} from "../api/types";
import { credentialPendingQuery } from "../features/credentials/pending";
import { issueDecisionsQuery } from "../features/doc/spec-marks";
import { comment, issue, specArtifact } from "../features/margin/margin-fixture";
import { agentsQuery } from "../features/refs/agent-registry";

/** The project document the document landing opens, and the artifact it names. */
const projectDocument: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-2",
  issue_key: null,
  project: "CORE",
  kind: "doc",
  name: "rollout.md",
  primary: false,
  ref_key: "CORE/rollout-md",
  session_id: null,
  slug: "rollout-md",
  versions: [],
};

const threads: MyRepliesResponse = {
  as_of: "2026-09-09T00:00:00Z",
  limit: 0,
  offset: 0,
  rows: [],
  total: 2,
};

/** What the issue's spec lacks, as the Spec tab draws it. */
const coverage: IssueCoverage = {
  gaps: ["r1 has no check"],
  has_plan: false,
  issue: "CORE-1",
  requirements: [
    {
      block_id: "r1",
      checks: 0,
      gaps: ["has no check"],
      ref: "CORE-1#spec#b-r1",
      plan_tasks: 0,
      text: "The issue page lists every open ask.",
    },
  ],
  spec_artifact_id: specArtifact.id,
};

/** The repositories this deployment maps to a project, which the repository control picks from. */
const repoProjects: RepoProject[] = [
  {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    default: true,
    project: "CORE",
    renamed_to: null,
    repo: "acme/widgets",
  },
];

/** The architecture sources of CORE, which the components line and the project page read. */
const architectureSources: ArchitectureSource[] = [
  {
    branch: "main",
    components: 2,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    enabled: true,
    last_commit: null,
    last_error: null,
    last_sync_at: null,
    project: "CORE",
    repo: "legion/arch",
  },
];

/** What every page draws, as this deployment answers it: a workspace with one project, nothing
 *  pinned, no ask waiting, two unread threads, a broker with an empty list and no agent. */
const shell: LandingShell = {
  agent_states: {},
  agents: [],
  credential_requests: { pending: [] },
  inbox: [],
  pinned_issues: [],
  projects: [
    {
      created_at: "2026-09-09T00:00:00Z",
      gated_steps: [],
      key: "CORE",
      name: "Core",
      open_asks: 0,
    },
  ],
  unread_replies: 2,
};

/** A landing answer: the viewer, the shell, and the one page part `page` names. */
function landing(page: Partial<LandingResponse["page"]> = {}): LandingResponse {
  return {
    page: { board: null, document: null, inbox: null, issue: null, ...page },
    shell,
    viewer: { event_head: "41", kind: "user", login: "alice" },
  };
}

/** The issue landing of `issue`: its spec, its comment, and the rest of its page empty. */
function issueLanding(): LandingResponse {
  return landing({
    issue: {
      architecture_sources: architectureSources,
      asks: [],
      comments: [comment],
      coverage,
      decisions: { decisions: [], document: null, open_questions: [] },
      document: {
        artifact: specArtifact,
        blocks: [],
        read: { artifact_id: specArtifact.id, read_at: "2026-09-09T00:00:00Z", version: 1 },
        text: { markdown: "# Spec\n", token: "t-1", version: 1 },
      },
      issue,
      repo_projects: repoProjects,
      subscribers: [],
      user_state: {},
    },
  });
}

/** The client the dashboard runs with: the 30,000 ms `staleTime` of `main.tsx` is what makes a
 *  seeded key answer a reader out of the cache. */
function dashboardClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 30_000 } } });
}

/** One read a page makes, under the key and through the `api` method the file that makes it
 *  names: a key the landing seeded answers this out of the cache, and one it did not sends the
 *  request. Written the way each of those files writes it, so a key that drifted from the seed
 *  fails these rows. */
function pageRead<T>(key: readonly unknown[], read: () => Promise<T>) {
  return queryOptions({ queryKey: key, queryFn: read });
}

/** Every `api` read the landing stands in for, each spied so that a read that still goes out is
 *  named by these rows rather than by a request nothing answered. Each answers a value no
 *  assertion below expects, so a key filled by one of these reads cannot pass as a seeded one. */
function readsBehindTheLanding() {
  const reads = {
    getArtifact: spyOn(api, "getArtifact").mockResolvedValue(projectDocument),
    getArtifactBlocks: spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    getArtifactText: spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "# Read again\n",
      token: "t-2",
      version: 2,
    }),
    getCredentialPending: spyOn(api, "getCredentialPending").mockResolvedValue(null),
    getDocumentRead: spyOn(api, "getDocumentRead").mockResolvedValue({
      artifact_id: specArtifact.id,
      read_at: null,
      version: null,
    }),
    getInbox: spyOn(api, "getInbox").mockResolvedValue([]),
    getIssue: spyOn(api, "getIssue").mockResolvedValue({ ...issue, title: "Read again" }),
    getIssueDecisions: spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    getIssueCoverage: spyOn(api, "getIssueCoverage").mockResolvedValue({
      ...coverage,
      gaps: ["read again"],
    }),
    getIssueSubscribers: spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    getMyAgentState: spyOn(api, "getMyAgentState").mockResolvedValue({}),
    getMyState: spyOn(api, "getMyState").mockResolvedValue({}),
    getProjectArtifact: spyOn(api, "getProjectArtifact").mockResolvedValue(projectDocument),
    listAgents: spyOn(api, "listAgents").mockResolvedValue([]),
    listArtifactAsks: spyOn(api, "listArtifactAsks").mockResolvedValue([]),
    listArtifactComments: spyOn(api, "listArtifactComments").mockResolvedValue([]),
    listComments: spyOn(api, "listComments").mockResolvedValue([]),
    listIssueAsks: spyOn(api, "listIssueAsks").mockResolvedValue([]),
    listIssues: spyOn(api, "listIssues").mockResolvedValue([]),
    listMyReplies: spyOn(api, "listMyReplies").mockResolvedValue({ ...threads, total: 9 }),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    listProjectArchitectureSources: spyOn(api, "listProjectArchitectureSources").mockResolvedValue(
      []
    ),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
    getArtifactSubscribers: spyOn(api, "getArtifactSubscribers").mockResolvedValue([]),
  };
  return {
    /** The name of every read that went out, in the order these rows name them. */
    whatWentOut: () =>
      Object.entries(reads)
        .filter(([, spy]) => spy.mock.calls.length > 0)
        .map(([name]) => name),
    restore: () => {
      for (const spy of Object.values(reads)) spy.mockRestore();
    },
    ...reads,
  };
}

test("an issue landing seeds every key the issue page reads, so the page reads nothing else", async () => {
  const getLanding = spyOn(api, "getLanding").mockResolvedValue(issueLanding());
  const whoAmI = spyOn(api, "whoAmI");
  const reads = readsBehindTheLanding();
  const client = dashboardClient();
  // The address the browser is loading, which is where the entry reads its page from: the
  // router reads the same one (`BrowserRouter` in `main.tsx`).
  window.history.pushState({}, "", "/issues/CORE-1");
  try {
    const viewer = await client.fetchQuery(whoAmIQuery());

    expect(getLanding).toHaveBeenCalledWith({ key: "CORE-1", page: "issue" });
    expect(viewer).toEqual({ event_head: "41", kind: "user", login: "alice" });
    // The shell, which every page draws.
    expect(await client.fetchQuery(inboxQuery())).toEqual([]);
    expect(await client.fetchQuery(projectsQuery())).toEqual(shell.projects ?? []);
    expect(
      await client.fetchQuery(
        pageRead(["issues", "pinned"], () => api.listIssues({ pinned: true }))
      )
    ).toEqual([]);
    expect(await client.fetchQuery(userAgentStateQuery())).toEqual({});
    expect(await client.fetchQuery(myRepliesCountQuery())).toBe(2);
    expect(await client.fetchQuery(credentialPendingQuery())).toEqual({ pending: [] });
    expect(await client.fetchQuery(agentsQuery())).toEqual([]);
    // The issue's own page.
    expect(await client.fetchQuery(issueQuery("CORE-1"))).toEqual(issue);
    expect(await client.fetchQuery(userStateQuery())).toEqual({});
    expect(
      await client.fetchQuery(pageRead(["comments", "CORE-1"], () => api.listComments("CORE-1")))
    ).toEqual([comment]);
    expect(
      await client.fetchQuery(pageRead(["asks", "CORE-1"], () => api.listIssueAsks("CORE-1")))
    ).toEqual([]);
    expect(
      await client.fetchQuery(
        pageRead(["subscribers", "CORE-1"], () => api.getIssueSubscribers("CORE-1"))
      )
    ).toEqual([]);
    expect(await client.fetchQuery(queryOptions(issueDecisionsQuery("CORE-1", true)))).toEqual({
      decisions: [],
      document: null,
      open_questions: [],
    });
    // What the Spec tab draws beside each requirement (`features/issue/SpecRequirements.tsx`).
    expect(await client.fetchQuery(issueCoverageQuery("CORE-1"))).toEqual(coverage);
    // The repository mappings the header's control picks this project's out of, under the key
    // that control writes by hand (`features/issue/RepoControl.tsx`).
    expect(
      await client.fetchQuery(pageRead(["repo-projects"], () => api.listRepoProjects()))
    ).toEqual(repoProjects);
    // The sources of the issue's own project, which the components line reads
    // (`features/issue/IssueComponentsLine.tsx`): the issue this landing carries names CORE.
    expect(await client.fetchQuery(projectArchitectureSourcesQuery("CORE"))).toEqual(
      architectureSources
    );
    // The document the page opens on, under the keys its editor, its text and its marks read.
    expect(
      await client.fetchQuery(
        pageRead(["artifact", specArtifact.id], () => api.getArtifact(specArtifact.id))
      )
    ).toEqual(specArtifact);
    expect(
      await client.fetchQuery(
        pageRead(["artifact", specArtifact.id, "text"], () => api.getArtifactText(specArtifact.id))
      )
    ).toEqual({ markdown: "# Spec\n", token: "t-1", version: 1 });
    expect(await client.fetchQuery(artifactBlocksQuery(specArtifact.id))).toEqual([]);
    expect(
      await client.fetchQuery(
        pageRead(["document-read", specArtifact.id], () => api.getDocumentRead(specArtifact.id))
      )
    ).toEqual({ artifact_id: specArtifact.id, read_at: "2026-09-09T00:00:00Z", version: 1 });

    expect(reads.whatWentOut()).toEqual([]);
    expect(whoAmI).not.toHaveBeenCalled();
  } finally {
    window.history.pushState({}, "", "/");
    getLanding.mockRestore();
    whoAmI.mockRestore();
    reads.restore();
    client.clear();
  }
});

test("a document landing seeds the document its page gates on and everything that names it", async () => {
  const getLanding = spyOn(api, "getLanding").mockResolvedValue(
    landing({
      document: {
        artifact: projectDocument,
        asks: [],
        blocks: [],
        comments: [],
        subscribers: [],
        text: { markdown: "# Rollout\n", token: "t-3", version: 1 },
      },
    })
  );
  const reads = readsBehindTheLanding();
  const client = dashboardClient();
  try {
    await client.fetchQuery({
      ...whoAmIQuery(),
      queryFn: ({ client: on }) => readLanding(on, "/projects/CORE/documents/rollout-md"),
    });

    expect(getLanding).toHaveBeenCalledWith({
      page: "document",
      project: "CORE",
      slug: "rollout-md",
    });
    // The read the page holds everything else behind: `project/slug` names the document's id.
    expect(
      await client.fetchQuery(
        pageRead(["artifact-ref", "CORE/rollout-md"], () =>
          api.getProjectArtifact("CORE", "rollout-md")
        )
      )
    ).toEqual(projectDocument);
    expect(
      await client.fetchQuery(
        pageRead(["artifact", projectDocument.id, "text"], () =>
          api.getArtifactText(projectDocument.id)
        )
      )
    ).toEqual({ markdown: "# Rollout\n", token: "t-3", version: 1 });
    expect(await client.fetchQuery(artifactBlocksQuery(projectDocument.id))).toEqual([]);
    expect(
      await client.fetchQuery(
        pageRead(["artifact", projectDocument.id, "asks"], () =>
          api.listArtifactAsks(projectDocument.id)
        )
      )
    ).toEqual([]);
    expect(
      await client.fetchQuery(
        pageRead(["subscribers", projectDocument.id], () =>
          api.getArtifactSubscribers(projectDocument.id)
        )
      )
    ).toEqual([]);
    expect(
      await client.fetchQuery(
        pageRead(["artifact", projectDocument.id, "comments"], () =>
          api.listArtifactComments(projectDocument.id)
        )
      )
    ).toEqual([]);
    expect(reads.whatWentOut()).toEqual([]);
  } finally {
    getLanding.mockRestore();
    reads.restore();
    client.clear();
  }
});

test("a board landing seeds the project's open issues, its asks, its sources and the reader's state", async () => {
  const getLanding = spyOn(api, "getLanding").mockResolvedValue(
    landing({
      board: {
        architecture_sources: architectureSources,
        inbox: [],
        issues: [],
        user_state: {},
      },
    })
  );
  const reads = readsBehindTheLanding();
  const client = dashboardClient();
  try {
    await client.fetchQuery({
      ...whoAmIQuery(),
      queryFn: ({ client: on }) => readLanding(on, "/projects/CORE/issues"),
    });

    expect(getLanding).toHaveBeenCalledWith({ page: "board", project: "CORE" });
    expect(await client.fetchQuery(projectIssuesQuery("CORE", [], true))).toEqual([]);
    expect(
      await client.fetchQuery(pageRead(["inbox", "CORE"], () => api.getInbox("CORE")))
    ).toEqual([]);
    expect(await client.fetchQuery(userStateQuery())).toEqual({});
    // The sources the project page reads to decide which tab it opens on
    // (`features/project/ProjectPage.tsx`).
    expect(await client.fetchQuery(projectArchitectureSourcesQuery("CORE"))).toEqual(
      architectureSources
    );
    expect(reads.whatWentOut()).toEqual([]);
  } finally {
    getLanding.mockRestore();
    reads.restore();
    client.clear();
  }
});

test("an inbox landing seeds the threads the Inbox lists", async () => {
  const getLanding = spyOn(api, "getLanding").mockResolvedValue(landing({ inbox: { threads } }));
  const reads = readsBehindTheLanding();
  const client = dashboardClient();
  try {
    await client.fetchQuery({
      ...whoAmIQuery(),
      queryFn: ({ client: on }) => readLanding(on, "/"),
    });

    expect(getLanding).toHaveBeenCalledWith({ page: "inbox" });
    expect(await client.fetchQuery(inboxThreadsQuery())).toEqual(threads);
    expect(reads.whatWentOut()).toEqual([]);
  } finally {
    getLanding.mockRestore();
    reads.restore();
    client.clear();
  }
});

test("a part the landing leaves out is read from its own route", async () => {
  // A Dispatch whose broker did not answer, whose issue the reader may not read and whose
  // coverage it therefore could not read either: each part is null, and the page reads it where
  // it read it before.
  const answer = issueLanding();
  const getLanding = spyOn(api, "getLanding").mockResolvedValue({
    ...answer,
    page: {
      ...answer.page,
      issue:
        answer.page.issue === null ? null : { ...answer.page.issue, coverage: null, issue: null },
    },
    shell: { ...shell, credential_requests: null },
  });
  const reads = readsBehindTheLanding();
  const client = dashboardClient();
  try {
    await client.fetchQuery({
      ...whoAmIQuery(),
      queryFn: ({ client: on }) => readLanding(on, "/issues/CORE-1"),
    });

    expect(await client.fetchQuery(issueQuery("CORE-1"))).toEqual({
      ...issue,
      title: "Read again",
    });
    expect(await client.fetchQuery(credentialPendingQuery())).toBeNull();
    expect(await client.fetchQuery(issueCoverageQuery("CORE-1"))).toEqual({
      ...coverage,
      gaps: ["read again"],
    });
    // An answer that carries no issue carries no project to put its sources under, so that key
    // is unseeded however full the answer's own `architecture_sources` was, and the components
    // line reads it for itself.
    expect(await client.fetchQuery(projectArchitectureSourcesQuery("CORE"))).toEqual([]);
    expect(reads.whatWentOut()).toEqual([
      "getCredentialPending",
      "getIssue",
      "getIssueCoverage",
      "listProjectArchitectureSources",
    ]);
  } finally {
    getLanding.mockRestore();
    reads.restore();
    client.clear();
  }
});

test("a landing a server cannot answer leaves every read where it was", async () => {
  // The 404 of a Dispatch older than the route, which a rolling deploy puts in front of a
  // browser, and a server that failed some other way: both leave the page reading as it read
  // before this change.
  for (const failure of [
    new ApiError(404, { code: "NOT_FOUND", error: "no such route" }),
    new ApiError(500, { error: "boom" }),
  ]) {
    const getLanding = spyOn(api, "getLanding").mockRejectedValue(failure);
    const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "41",
      kind: "user",
      login: "alice",
    });
    const reads = readsBehindTheLanding();
    const client = dashboardClient();
    try {
      const viewer: AuthenticatedUser = await client.fetchQuery({
        ...whoAmIQuery(),
        queryFn: ({ client: on }) => readLanding(on, "/issues/CORE-1"),
      });

      expect(viewer).toEqual({ event_head: "41", kind: "user", login: "alice" });
      expect(whoAmI).toHaveBeenCalledTimes(1);
      // Nothing is seeded, so each read runs and answers what its own route answers.
      expect(await client.fetchQuery(issueQuery("CORE-1"))).toEqual({
        ...issue,
        title: "Read again",
      });
      expect(await client.fetchQuery(inboxQuery())).toEqual([]);
      expect(await client.fetchQuery(myRepliesCountQuery())).toBe(9);
      expect(reads.whatWentOut()).toEqual(["getInbox", "getIssue", "listMyReplies"]);
    } finally {
      getLanding.mockRestore();
      whoAmI.mockRestore();
      reads.restore();
      client.clear();
    }
  }
});

test("the refusals the page is drawn from reach it rather than a fallback read", async () => {
  // The 401 `AuthGate` draws its sign-in page from, and the two 503s about the event log's head,
  // which the query client asks again by their codes: each is the answer about this page, so no
  // second read may stand in for it.
  for (const refusal of [
    new ApiError(401, { code: "NO_IDENTITY", error: "no identity" }),
    new ApiError(503, { code: "EVENT_HEAD_UNAVAILABLE", error: "ask again" }),
    new ApiError(503, { code: "EVENT_HEAD_READ_FAILED", error: "unreadable" }),
  ]) {
    const getLanding = spyOn(api, "getLanding").mockRejectedValue(refusal);
    const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "41",
      kind: "user",
      login: "alice",
    });
    const client = dashboardClient();
    try {
      const refused = await client
        .fetchQuery({
          ...whoAmIQuery(),
          queryFn: ({ client: on }) => readLanding(on, "/issues/CORE-1"),
        })
        .catch((error: unknown) => error);

      expect(refused).toBe(refusal);
      expect(whoAmI).not.toHaveBeenCalled();
    } finally {
      getLanding.mockRestore();
      whoAmI.mockRestore();
      client.clear();
    }
  }
});

test("the landing is read for the page the address is on", async () => {
  const getLanding = spyOn(api, "getLanding").mockResolvedValue(landing());
  const client = dashboardClient();
  try {
    for (const [pathname, read] of [
      ["/issues/CORE-1", { key: "CORE-1", page: "issue" }],
      ["/issues/CORE-1/conversation", { key: "CORE-1", page: "issue" }],
      ["/issues/CORE-1/artifacts/plan-md", { key: "CORE-1", page: "issue" }],
      [
        "/projects/CORE/documents/rollout-md",
        { page: "document", project: "CORE", slug: "rollout-md" },
      ],
      ["/projects/CORE", { page: "board", project: "CORE" }],
      ["/projects/CORE/issues", { page: "board", project: "CORE" }],
      ["/projects/CORE/architecture", { page: "board", project: "CORE" }],
      ["/", { page: "inbox" }],
      // An address that names neither issue nor project asks for the one page that needs no name
      // of its own; the shell is what such a page is drawn from.
      ["/agents", { page: "inbox" }],
      ["/settings", { page: "inbox" }],
    ] as const) {
      getLanding.mockClear();
      await readLanding(client, pathname);
      // The address rides in the comparison, so a failure says which one asked wrongly.
      expect([pathname, getLanding.mock.calls[0]?.[0]]).toEqual([pathname, read]);
    }
  } finally {
    getLanding.mockRestore();
    client.clear();
  }
});
