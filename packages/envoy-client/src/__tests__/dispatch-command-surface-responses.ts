/**
 * The answers of the fake Dispatch that dispatch-command-surface.test.ts runs every command
 * against: the ids, times and digests the record holds, and the body of each route's answer.
 */

export const SESSION = "session-1";
export const ISSUE = "DSP-1";
export const UUID = "01234567-0000-4000-8000-000000000001";
export const OTHER_UUID = "01234567-0000-4000-8000-000000000002";
export const ARTIFACT_ID = "01234567-0000-4000-8000-00000000000a";
const PLAN_ARTIFACT_ID = "01234567-0000-4000-8000-00000000000b";
export const AT = "2026-01-02T03:04:05Z";
// The commit a recorded check result stands at, and the digest of the check's words: a result
// prints the first twelve characters of the one, and the other reaches no printed line.
const COMMIT = "1111111111111111111111111111111111111111";
const CHECK_SHA256 = "2222222222222222222222222222222222222222222222222222222222222222";

export const actor = { kind: "session", id: SESSION };

export function issueBody(key = ISSUE) {
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
    queries: [
      {
        query: "plan",
        artifact_id: PLAN_ARTIFACT_ID,
        takes: ["plan_task", "plan_role"],
        subtree: `issue:${key}`,
      },
    ],
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

export function versionBody() {
  return {
    number: 1,
    named: false,
    summary: null,
    authors: [actor],
    created_at: AT,
    size: 12,
  };
}

export function askBody() {
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
export function askReadBody() {
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

export function commentBody() {
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

export function messageBody() {
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

export function artifactBody(name = "design.md", kind = "doc") {
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

/** The issue's plan document: the artifact its plan query names. It carries the version that
 *  the `dispatch plan-approve` example approves. */
function planArtifactBody() {
  return {
    ...artifactBody("plan.md"),
    id: PLAN_ARTIFACT_ID,
    versions: [versionBody(), { ...versionBody(), number: 2 }],
  };
}

/** `POST /api/v1/artifacts/{id}/plan-reviews`: the plan review's approval, which carries no ask,
 *  since no plan approval ever reaches a human's Inbox. */
export function planReviewBody() {
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
export function reviewBody() {
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
export function issueResultsBody() {
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
export function checkResultRecordedBody() {
  return {
    result: checkResultBody(),
    line: `- Check k1 (before_merge): pass at ${COMMIT.slice(0, 12)}, recorded by implementer. The workspace opens natively.`,
  };
}

/** `GET /api/v1/issues/{key}/coverage`: a spec that marks no requirements, which Dispatch answers
 *  with no requirement rows and the one gap line that says so. */
export function coverageBody() {
  return {
    issue: ISSUE,
    spec_artifact_id: ARTIFACT_ID,
    has_plan: true,
    requirements: [],
    gaps: ["the spec marks no requirements"],
  };
}

export function handoffBody() {
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

/** The lens every reasoning write here names, its one finding, and who wrote them: the person a
 *  personal token proves, as Dispatch's `authored_by` tuple names her. */
export const LENS = `dispatch://lens/${UUID}`;
const reasoningAuthor = { kind: "person", id: "person@example.com" };

/** One finding of the lens, as `POST /api/v1/issues/{key}/nodes/{id}/findings` answers it. */
export function findingBody(text: string) {
  return {
    address: `${LENS}#b-${OTHER_UUID}`,
    id: `lens/${UUID}#${OTHER_UUID}`,
    block_id: OTHER_UUID,
    lens: LENS,
    text,
    author: reasoningAuthor,
    created_at: AT,
    state: "live",
    visibility: "project",
  };
}

/** `POST /api/v1/issues/{key}/lenses`: the lens with the finding its call wrote. */
export function lensBody() {
  return {
    address: LENS,
    id: UUID,
    kind: "trade-off",
    on: `dispatch://${ISSUE}/spec`,
    verdict: null,
    triggered_by: null,
    author: reasoningAuthor,
    created_at: AT,
    state: "live",
    visibility: "project",
    findings: [findingBody("A second region doubles the write path's cost.")],
  };
}

/** `POST /api/v1/issues/{key}/risks`: a risk under the lens, prevented by no check yet. */
export function riskBody() {
  return {
    address: `dispatch://risk/${OTHER_UUID}`,
    id: OTHER_UUID,
    lens: LENS,
    heading: "The migration locks the table for minutes.",
    early_warning: "Writes queue behind the migration's lock.",
    prevention: "Build the index concurrently.",
    prevented_by: [],
    author: reasoningAuthor,
    created_at: AT,
    state: "live",
    visibility: "project",
  };
}

/** One constraint, a standard with its reference, as both constraint routes answer it. */
export function constraintBody() {
  return {
    address: `dispatch://constraint/${OTHER_UUID}`,
    id: OTHER_UUID,
    standard: "RFC 9110",
    reference: "section 9.2.2, idempotent methods",
    measurement: null,
    value: null,
    taken_where: null,
    author: reasoningAuthor,
    created_at: AT,
    state: "live",
    visibility: "project",
  };
}

/** `POST /api/v1/issues/{key}/nodes/{id}/pointers`: the lens's kind moved from trade-off to botec. */
export function pointerBody() {
  const kind = (id: string) => ({
    address: `dispatch://lens_kind/${id}`,
    kind: "lens_kind",
    state: "live",
    visibility: "project",
  });
  return {
    node: { address: LENS, kind: "lens", state: "live", visibility: "project" },
    kind: "has_kind",
    action: "retarget",
    to: kind("botec"),
    from: kind("trade-off"),
  };
}

/** `POST /api/v1/issues/{key}/nodes/{id}/state` and `/settle`: the example's requirement block
 *  moved from `previous` to `state`. */
export function nodeStateBody(previous: string, state: string) {
  return {
    node: {
      address: `dispatch://${ISSUE}/spec#b-r-cache`,
      kind: "block",
      state,
      visibility: "project",
    },
    previous,
  };
}

/** `GET /api/v1/issues/{key}/proposed`: one proposed requirement with its excerpt. */
export function proposedBody() {
  return {
    issue: ISSUE,
    nodes: [
      {
        node: {
          kind: "block",
          id: `${ISSUE}/spec#r-cache`,
          block_id: "r-cache",
          ref: `dispatch://${ISSUE}/spec#b-r-cache`,
          made_by: [],
          made_at: null,
          changed_by: [],
          changed_at: null,
          parent: null,
          parent_version: null,
          state: "proposed",
          visibility: "project",
        },
        excerpt: { block_id: "r-cache", text: "The cache holds every read for a minute." },
      },
    ],
  };
}

/** `POST /api/v1/issues/{key}/artifacts/{slug}/premortem-convert`: the example's dry run, the lens
 *  and the two risks it would write, none of them at an address yet. */
export function premortemReportBody() {
  const risk = (number: number, heading: string, preventedBy: string[], reason: string) => ({
    number,
    address: null,
    heading,
    early_warning: "A third issue parked until the graph lands.",
    prevention: "Every slice ships one visible change.",
    prevented_by: preventedBy,
    reason,
  });
  return {
    document: {
      address: `dispatch://${ISSUE}/artifact/premortem-md`,
      id: ARTIFACT_ID,
      version: 1,
      moved_to: null,
    },
    dry_run: true,
    already_converted: false,
    lens: { address: null, kind: "pre-mortem", on: `dispatch://${ISSUE}`, verdict: "Ship it." },
    risks: [
      risk(
        0,
        "Risk 0, the rewrite eats the quarter",
        [`dispatch://${ISSUE}/artifact/verification-md#b-k1`],
        "k1 fails a slice that ships nothing a user sees."
      ),
      risk(1, "1\\. Two indexers, one truth", [], "No check covers a second indexer yet."),
    ],
    summary:
      `Dry run, nothing written: converting dispatch://${ISSUE}/artifact/premortem-md (version 1) ` +
      `writes one pre-mortem lens under ${ISSUE} with 2 risks and moves the document's address to that lens.`,
  };
}

/** `GET /api/v1/lenses?on=`: the spec's trade-off analysis with its finding, the pre-mortem it
 *  triggered with one risk no check prevents yet, and the chain the two make. */
export function lensesBody() {
  const spec = `dispatch://${ISSUE}/spec`;
  const premortem = `dispatch://lens/${OTHER_UUID}`;
  const lens = (address: string, kind: string, words: string) => ({
    address,
    kind: { id: kind, words },
    on: spec,
    author: reasoningAuthor,
    created_at: AT,
    verdict: null,
    state: "live",
    visibility: "project",
    triggered_by: [] as string[],
    triggered: [] as string[],
    findings: [] as unknown[],
    risks: [] as unknown[],
  });
  const tradeOff = {
    ...lens(LENS, "trade-off", "trade-off analysis"),
    verdict: "Keep one region.",
    triggered: [premortem],
    findings: [
      {
        address: `${LENS}#b-${OTHER_UUID}`,
        text: "A second region doubles the write path's cost.",
        rank: 41,
        state: "live",
        visibility: "project",
      },
    ],
  };
  const preMortem = {
    ...lens(premortem, "pre-mortem", "the pre-mortem over failure modes"),
    triggered_by: [LENS],
    risks: [
      {
        address: `dispatch://risk/${UUID}`,
        heading: "The migration locks the table for minutes.",
        early_warning: "Writes queue behind the migration's lock.",
        prevention: "Build the index concurrently.",
        prevented_by: [],
        state: "live",
        visibility: "project",
      },
    ],
  };
  return {
    on: { address: spec, kind: "artifact", state: "live", visibility: "project" },
    lens: null,
    lenses: [tradeOff, preMortem],
    chains: [{ lenses: [tradeOff, preMortem], stopped_at: null }],
  };
}

/** `GET /api/v1/issues/{key}/steps`: one ended run and one still running, so the record holds a
 *  line of each and the `-` of a field with no value. */
export function stepsBody() {
  const run = {
    issue: ISSUE,
    task: "default",
    kind: "step",
    generation: 1,
    attempt: 1,
    returned_to: null,
    report: null,
  };
  return {
    issue: ISSUE,
    tasks: [
      {
        task: "default",
        steps: [
          {
            ...run,
            id: UUID,
            step: 2,
            role: "implementer",
            type: "implementation",
            agent: "legion-dsp-dsp-1-implementer",
            session: SESSION,
            model: "anthropic/claude-opus-5",
            started_at: AT,
            ended_at: AT,
            result: "passed",
          },
          {
            ...run,
            id: OTHER_UUID,
            step: 3,
            role: "tester",
            type: "testing",
            agent: "legion-dsp-dsp-1-tester",
            session: "",
            model: "",
            started_at: AT,
            ended_at: null,
            result: "",
          },
        ],
      },
    ],
  };
}

/** `GET /api/v1/projects/{key}/plans`: one finished issue Legion ran and rated, so the record
 *  holds the issue's whole line. Its start, its retro's end, its last end and its close are four
 *  times, and the last end is an hour and a half after the start, as `duration_seconds` counts
 *  it, so a line that printed one of the four in place of another reads differently. */
export function plansBody() {
  return {
    project: "INFRA",
    issues: [
      {
        issue: "INFRA-1",
        title: "Retry failed webhook deliveries",
        closed_at: AT,
        plan: {
          id: UUID,
          issue: "INFRA-1",
          generation: 1,
          source: "default",
          artifact_id: null,
          version: null,
          tasks: [
            {
              id: "default",
              steps: ["planner", "implementer", "tester", "reviewer", "acceptance:owner"],
            },
          ],
        },
        ratings: [
          {
            rating: 4,
            reason: "The plan fit the work.",
            step_id: OTHER_UUID,
            ended_at: "2026-01-02T02:40:05Z",
          },
        ],
        started_at: "2026-01-02T01:31:05Z",
        ended_at: "2026-01-02T03:01:05Z",
        duration_seconds: 5400,
        failed_steps: 1,
      },
    ],
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

/** `PUT` and `DELETE /api/v1/projects/{key}/proven-sessions`: the project as the project list
 *  shows it, its setting as the write left it. */
export function projectBody(provenSessions: boolean) {
  return {
    key: "CORE",
    name: "Core",
    open_asks: 0,
    gated_steps: [],
    proven_sessions: provenSessions,
    created_at: AT,
  };
}

/** `GET /api/v1/priorities`: one live priority with work under it, and one issue in each of the
 *  two faults, so the record holds every line `dispatch priorities` can print. Neither faulted
 *  issue is claimed, so the run reads no agent list and no printed line carries a liveness. */
export function prioritiesBody() {
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
