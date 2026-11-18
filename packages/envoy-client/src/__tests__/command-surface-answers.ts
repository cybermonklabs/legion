import { fakeSearchResponse } from "./fake-search-response";

/**
 * The fake Dispatch every command's example call is driven against: one fixed answer per route
 * those calls reach (`answerFor`), and the ids and times its answers carry. The command-surface
 * record (`dispatch-command-surface.test.ts`) and the census of writes whose answer is lost
 * (`dispatch-cli-unsettled.test.ts`) both answer from it, so the two drive the same calls.
 */

export const SESSION = "session-1";
export const ISSUE = "DSP-1";
export const UUID = "01234567-0000-4000-8000-000000000001";
export const OTHER_UUID = "01234567-0000-4000-8000-000000000002";
export const ARTIFACT_ID = "01234567-0000-4000-8000-00000000000a";
export const PLAN_ARTIFACT_ID = "01234567-0000-4000-8000-00000000000b";
const AT = "2026-01-02T03:04:05Z";
// The commit a recorded check result stands at, and the digest of the check's words: a result
// prints the first twelve characters of the one, and the other reaches no printed line.
const COMMIT = "1111111111111111111111111111111111111111";
const CHECK_SHA256 = "2222222222222222222222222222222222222222222222222222222222222222";

/** The same fake answer the executor's own tests use: JSON with a 200 unless `status` says. */
export function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const actor = { kind: "session", id: SESSION };

function issueBody(key = ISSUE) {
  return {
    key,
    project: key.split("-")[0],
    number: Number(key.split("-")[1]),
    title: "Native workspace",
    status: "todo",
    priority: 1,
    rank: "0|hzzzzz:",
    labels: ["infra"],
    parent: null,
    blocked_by: [],
    advances: [
      { id: "ship-the-daemon", inherited: false },
      { id: "legion-first", inherited: true },
    ],
    advances_revision: 7,
    assignee: "person@example.com",
    claim: null,
    components: { mode: "explicit", ids: [], unknown: [], reason: null, inherited_from: null },
    external_links: [],
    route: null,
    repo: "owner/repo",
    resolved_repo: "owner/repo",
    created_by: actor,
    created_at: AT,
    updated_at: AT,
    closed_at: null,
    primary_artifact_id: ARTIFACT_ID,
    plan_artifact_id: PLAN_ARTIFACT_ID,
    last_seq: 0,
    route_status: null,
    route_holder: null,
    progress: { tasks: null, children: null },
    artifacts: [artifactBody("spec.md"), planArtifactBody()],
    open_asks: [],
    children: [],
    referenced_by_count: 0,
  };
}

function versionBody() {
  return {
    number: 1,
    named: false,
    summary: null,
    authors: [actor],
    created_at: AT,
    size: 12,
  };
}

function askBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    artifact_id: null,
    author: actor,
    question: "Which way should the release go?",
    options: [],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "open",
    answer: null,
    kind: "question",
    waiting_on: "human",
    opened_event_id: 1,
    created_at: AT,
    updated_at: AT,
  };
}

/** `GET /api/v1/asks/{id}`: the ask with its thread, as `AskRead` carries it. */
function askReadBody() {
  return {
    ask: askBody(),
    replies: [],
    edits: [],
    answers: [],
    followers: [],
    claimant: null,
    route: null,
    route_status: null,
    route_holder: null,
  };
}

function commentBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    artifact_id: null,
    author: actor,
    body: "Looks good.",
    anchor: null,
    state: "open",
    reply_to: null,
    created_at: AT,
    updated_at: AT,
  };
}

function messageBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    author: actor,
    body: "Implementation started.",
    to: [],
    created_at: AT,
    deliveries: [],
  };
}

function artifactBody(name = "design.md", kind = "doc") {
  return {
    id: ARTIFACT_ID,
    issue_key: ISSUE,
    project: "DSP",
    slug: name.replace(/\./g, "-"),
    ref_key: `${ISSUE}/${name.replace(/\./g, "-")}`,
    name,
    kind,
    primary: name === "spec.md",
    created_by: actor,
    created_at: AT,
    versions: [versionBody()],
  };
}

/** The issue's plan document: the one `dispatch plan-approve` resolves, by the `plan_artifact_id`
 *  the issue detail names it under. It carries the version that command's example approves. */
function planArtifactBody() {
  return {
    ...artifactBody("plan.md"),
    id: PLAN_ARTIFACT_ID,
    versions: [versionBody(), { ...versionBody(), number: 2 }],
  };
}

/** `POST /api/v1/artifacts/{id}/plan-reviews`: the plan review's approval, which carries no ask,
 *  since no plan approval ever reaches a human's Inbox. */
function planReviewBody() {
  return {
    id: UUID,
    artifact_id: PLAN_ARTIFACT_ID,
    version: 2,
    state: "approved",
    actor,
    reason: null,
    ask_id: null,
    kind: "plan_review",
    created_at: AT,
  };
}

/** `POST /api/v1/artifacts/{id}/versions/{number}/reviews`: the person's approval of the issue's
 *  spec at the version `dispatch review`'s example names. The person is the one the token names,
 *  and the act names the session the token proved; no request was open, so the review answers no
 *  ask. */
function reviewBody() {
  return {
    id: UUID,
    artifact_id: ARTIFACT_ID,
    version: 2,
    state: "approved",
    actor: { kind: "user", id: "person@example.com", act: { session: SESSION } },
    reason: null,
    ask_id: null,
    kind: "person",
    created_at: AT,
    content_sha256: null,
    content_sha256_source: null,
  };
}

/** What a check compares its results with: the head of the issue's pull request, at the commit
 *  the one recorded result stands at, as Dispatch read it. */
function checkBasisBody() {
  return {
    kind: "pull_request",
    pull_request: 2712,
    commit: COMMIT,
    learned_at: { kind: "read_at", at: AT },
  };
}

/** One result of one check, as `CheckResult` carries it. Where the result's commit stands and
 *  whether the check's words moved since are computed when results are read, so the record
 *  route's own answer leaves them out and `read` fills them in for the read route. */
function checkResultBody(read?: Record<string, unknown>) {
  return {
    id: UUID,
    issue_key: ISSUE,
    check_issue_key: ISSUE,
    check_artifact_id: ARTIFACT_ID,
    check_block_id: "k1",
    check_version: 1,
    check_sha256: CHECK_SHA256,
    role: "implementer",
    surface: "",
    run: "",
    observed: "",
    commit_sha: COMMIT,
    negative_control: "",
    failed: 0,
    skipped: 0,
    excluded: 0,
    verdict: "pass",
    screenshots: [],
    session: SESSION,
    actor,
    created_at: AT,
    event_seq: 1,
    ...(read ?? {}),
  };
}

/** `GET /api/v1/issues/{key}/results`: the issue's one check with the result recorded against it.
 *  The line each check prints carries no age, so the row is a filled one. */
function issueResultsBody() {
  return {
    issue: ISSUE,
    verification_artifact_id: ARTIFACT_ID,
    checks: [
      {
        block_id: "k1",
        ref: `dispatch://${ISSUE}/artifact/verification-md#b-k1`,
        issue_key: ISSUE,
        artifact_id: ARTIFACT_ID,
        text: "The workspace opens natively.",
        when: "before_merge",
        surface: "",
        run: "",
        negative_control: "",
        version: 1,
        sha256: CHECK_SHA256,
        compared_with: [checkBasisBody()],
        pass_stands: { answer: "yes", roles: ["implementer"] },
        results: [
          checkResultBody({
            commit_stands: { answer: "at_basis", basis: checkBasisBody() },
            check_words_moved: false,
          }),
        ],
      },
    ],
  };
}

/** `POST /api/v1/issues/{key}/results`: the stored result and the pull-request body line Dispatch
 *  writes for it (`checkResultLine`), so every writer writes one grammar. */
function checkResultRecordedBody() {
  return {
    result: checkResultBody(),
    line: `- Check k1 (before_merge): pass at ${COMMIT.slice(0, 12)}, recorded by implementer. The workspace opens natively.`,
  };
}

/** `GET /api/v1/issues/{key}/coverage`: a spec that marks no requirements, which Dispatch answers
 *  with no requirement rows and the one gap line that says so. */
function coverageBody() {
  return {
    issue: ISSUE,
    spec_artifact_id: ARTIFACT_ID,
    has_plan: true,
    requirements: [],
    gaps: ["the spec marks no requirements"],
  };
}

function handoffBody() {
  return {
    id: UUID,
    issue_key: ISSUE,
    phase: "implement",
    version: 1,
    body: '{"schemaVersion":1,"phase":"implement","issue":"DSP-1"}',
    author: actor,
    created_at: AT,
  };
}

export function architectureSource() {
  return {
    project: "CORE",
    repo: "acme/infra",
    branch: "main",
    enabled: true,
    created_by: { kind: "user", login: "sjawhar" },
    created_at: AT,
    last_sync_at: AT,
    last_commit: "abc1234",
    last_error: null,
    components: 3,
  };
}

/** `GET /api/v1/priorities`: one live priority with work under it, and one issue in each of the
 *  two faults, so the record holds every line `dispatch priorities` can print. Neither faulted
 *  issue is claimed, so the run reads no agent list and no printed line carries a liveness. */
function prioritiesBody() {
  return {
    priorities: [
      {
        id: "ship-the-daemon",
        title: "Ship the daemon",
        description: "The daemon runs every phase.",
        owner: "person@example.com",
        rank: "0|hzzzzz:",
        state: "live",
        made_at: AT,
        made_by: actor,
        work: {
          by_status: { backlog: 1, todo: 2, in_progress: 3, done: 0 },
          claimed: 2,
          open_asks: 1,
          closed_last_7_days: 4,
          newest_event_at: AT,
        },
      },
    ],
    work_under_no_priority: {
      count: 1,
      by_project: { DSP: 1 },
      issues: [
        {
          key: "DSP-7",
          project: "DSP",
          title: "Rotate the broker keys",
          status: "in_progress",
          assignee: "person@example.com",
          claim: null,
          advances: [],
          moved_at: null,
          moved_by: null,
          changed_at: AT,
        },
      ],
    },
    priority_work_parked: {
      count: 1,
      by_priority: { "ship-the-daemon": 1 },
      issues: [
        {
          key: "DSP-9",
          project: "DSP",
          title: "Backfill the index",
          status: "backlog",
          assignee: null,
          claim: null,
          advances: [{ id: "ship-the-daemon", inherited: true }],
          moved_at: AT,
          moved_by: actor,
          changed_at: AT,
        },
      ],
    },
  };
}

/** The fake Dispatch: one fixed answer per route every command here reaches. `body` is what the
 *  request carried, which the one route that answers with the caller's own text reads. */
export function answerFor(method: string, pathname: string, body?: unknown): Response {
  const route = `${method} ${pathname}`;
  const segments = pathname.split("/").filter((segment) => segment !== "");
  const rest = segments.slice(4).join("/");
  switch (route) {
    case "POST /api/v1/markdown/round-trip": {
      // The store's own parse and render. Every text uploaded here is already Markdown as
      // Dispatch writes it, so the answer is the text the upload was handed.
      const sent = typeof body === "string" ? (JSON.parse(body) as { markdown?: unknown }) : {};
      return response({ markdown: typeof sent.markdown === "string" ? sent.markdown : "" });
    }
    case "GET /api/v1/issues/resolve":
      return response({ key: ISSUE });
    case "POST /api/v1/issues":
      return response({ ...issueBody("DSP-216"), created: true }, 201);
    case "GET /api/v1/issues":
      return response({ issues: [issueBody()], total: 1, limit: 250, offset: 250 });
    case "GET /api/v1/projects":
      return response([{ key: "DSP", name: "Dispatch", created_at: AT }]);
    case "POST /api/v1/projects":
      return response({ key: "CORE", name: "Core", created_at: AT }, 201);
    case "GET /api/v1/priorities":
      return response(prioritiesBody());
    case "GET /api/v1/search":
      return response(fakeSearchResponse([]));
    case "GET /api/v1/whoami":
      return response({ kind: "agent", owner: "person@example.com", session: SESSION });
    case "GET /api/v1/agents":
      return response([]);
    // The bounds the server's own `GET /api/v1/limits` answers (`api/limits.go`), fixed here so
    // the recorded lines are the server's numbers rather than this client's.
    case "GET /api/v1/limits":
      return response({
        block_units: 32000,
        item_blocks: 32768,
        ask_options: 8,
        ask_units: 32000,
        event_body_units: 32000,
        request_bytes: 1048576,
        document_bytes: 1048576,
        handoff_bytes: 262144,
      });
    case "GET /api/v1/references":
      return response({ node: { kind: "issue", id: ISSUE }, edges: [] });
    // The Inbox's two lists, both empty: every line the summary prints from a row carries the
    // row's own age, which the record must not date against the wall clock.
    case "GET /api/v1/inbox":
      return response([]);
    case "GET /api/v1/credential-requests":
      return response({ pending: [] });
    case "GET /api/v1/asks/open":
      return response({
        session_id: SESSION,
        as_of: AT,
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      });
  }
  if (segments[2] === "agents" && segments.length >= 4) {
    if (rest === "open-asks") {
      return response({
        session_id: SESSION,
        as_of: AT,
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      });
    }
    if (rest === "answers") {
      return response({ session_id: SESSION, since: AT, total: 0, items: [] });
    }
    // `dispatch send-to-agent`: answered once the first delivery attempt settled, here as sent.
    if (rest === "messages" && method === "POST") {
      return response(
        { ...messageBody(), issue_key: null, deliveries: [{ delivery: "aside", state: "sent" }] },
        201
      );
    }
  }
  if (segments[2] === "issues" && segments.length >= 4) {
    if (rest === "" && (method === "GET" || method === "PATCH")) {
      return response(issueBody(segments[3]));
    }
    if (rest === "claim") {
      return response({
        ...issueBody(segments[3]),
        claim: { actor, at: AT },
        legion_managed_status: false,
      });
    }
    if (rest === "events") return response([]);
    if (rest === "asks" && method === "POST") return response(askBody(), 201);
    if (rest === "asks") return response([]);
    if (rest === "comments" && method === "POST") return response(commentBody(), 201);
    if (rest === "comments") return response([]);
    if (rest === "messages" && method === "POST") return response(messageBody(), 201);
    if (rest === "artifacts" && method === "POST") {
      return response({ artifact: artifactBody(), version: versionBody() }, 201);
    }
    if (rest === "artifacts") return response([artifactBody("spec.md")]);
    if (rest === "references") return response({ members: [], truncated: false });
    if (rest === "results" && method === "POST") return response(checkResultRecordedBody(), 201);
    if (rest === "results") return response(issueResultsBody());
    if (rest === "coverage") return response(coverageBody());
    if (rest === "handoffs") return response([]);
    if (segments[4] === "handoffs" && method === "POST") return response(handoffBody(), 201);
    if (segments[4] === "handoffs") return response(handoffBody());
  }
  if (segments[2] === "artifacts" && segments.length >= 4) {
    if (rest === "" && method === "GET") return response(artifactBody("spec.md"));
    if (rest === "text") return response({ markdown: "# Spec\n", version: 1 });
    if (rest === "blocks") return response([]);
    if (rest === "edits") return response({ applied: 1, version: versionBody(), changed: true });
    if (rest === "plan-reviews") return response(planReviewBody(), 201);
    if (rest === "versions/2/reviews" && method === "POST") return response(reviewBody(), 201);
    if (rest === "suggestions") {
      return response({ ...commentBody(), artifact_id: ARTIFACT_ID }, 201);
    }
    if (rest === "approval-requests") {
      return response(
        {
          ask: askBody(),
          artifact_id: ARTIFACT_ID,
          version: 1,
          approval: { state: "awaiting", latest_version: 1, version: 1 },
        },
        201
      );
    }
    if (rest === "comments" || rest === "asks" || rest === "events") return response([]);
    if (rest === "references") return response({ artifact_id: ARTIFACT_ID, items: [] });
  }
  if (segments[2] === "asks" && segments.length >= 4) {
    if (rest === "") return response(method === "GET" ? askReadBody() : askBody());
    if (rest === "resolve") {
      return response({
        ...askBody(),
        state: "resolved",
        resolution: { kind: "retracted", reason: "A newer question supersedes this one.", actor },
      });
    }
    if (rest.startsWith("followers")) return new Response(null, { status: 204 });
  }
  if (segments[2] === "comments" && segments.length >= 4) {
    if (rest === "" || rest === "resolve") return response(commentBody());
  }
  if (segments[2] === "projects" && segments.length >= 4) {
    if (rest === "related") {
      return response({ outcome: "none", search: "meaning_and_keywords", related: [] });
    }
    if (rest === "architecture-sources") return response([architectureSource()]);
    if (rest.startsWith("architecture-sources/")) return response(architectureSource());
    if (rest === "artifacts") return response([]);
  }
  throw new Error(`unexpected request: ${route}`);
}
