// The ask card demo's issue pages and documents (packages/dispatch/e2e/ask-card-demo/README.md):
// the routes an issue page, its spec's read view and its edit view read, answered from the
// invented fixtures. The live text itself goes by the document's socket (`document-room.ts`).
import type {
  ArtifactText,
  ArtifactVersionText,
  ArtifactWithTrees,
  DocumentRead,
  InboxRow,
  IssueAskRow,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
} from "../api/types";
import { type DemoDocument, DOCUMENTS, documentById, specId } from "./document-fixtures";
import { ASKS, COMMENTS, DEMO_CLOCK, ISSUES, issueSummary, PROJECT, READER } from "./fixtures";
import { type DemoRoute, goTime, notFound, ok } from "./http";

/** Where server.ts keeps the asks the tab has written, so an ask answered in the Inbox reads
 *  answered on its issue's page too. */
const KEPT_STATE = "dispatch-ask-card-demo.state";

interface KeptAsks {
  asks?: Record<string, InboxRow>;
  replies?: Record<string, unknown[]>;
}

/** The asks as the stub holds them now: the fixtures, with what the tab has written since. */
function asksNow(): { rows: InboxRow[]; replies: Record<string, unknown[]> } {
  let kept: KeptAsks = {};
  try {
    kept = JSON.parse(sessionStorage.getItem(KEPT_STATE) ?? "{}") as KeptAsks;
  } catch {
    // A tab that kept nothing reads the fixtures.
  }
  return {
    rows: ASKS.map(({ row }) => kept.asks?.[row.id] ?? row),
    replies: kept.replies ?? {},
  };
}

/** An Inbox row as the issue's ask list carries it: without the Inbox's own fields, and with its
 *  thread only where the reader asked for threads. */
function issueAsk(row: InboxRow, replies: unknown[] | undefined, thread: boolean): IssueAskRow {
  const {
    issue: _issue,
    priority: _priority,
    snoozed_until: _snoozed,
    thread: rowThread,
    ...ask
  } = row;
  return (
    thread ? { ...ask, thread: { ...rowThread, replies: replies ?? [] } } : ask
  ) as IssueAskRow;
}

function asksOf(key: string, state: string | null, thread: boolean): IssueAskRow[] {
  const { rows, replies } = asksNow();
  return rows
    .filter((row) => row.issue_key === key && (state === "all" || row.state === (state ?? "open")))
    .map((row) => issueAsk(row, replies[row.id], thread));
}

function artifactOf(document: DemoDocument): ArtifactWithTrees {
  const first = document.versions[0]?.created_at ?? DEMO_CLOCK;
  return {
    ...(document.approval === undefined ? {} : { approval: document.approval }),
    created_at: first,
    created_by: { id: READER, kind: "user" },
    id: specId(document.issue),
    issue_key: document.issue,
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: PROJECT,
    ref_key: `${document.issue}/spec`,
    session_id: null,
    slug: "spec-md",
    versions: document.versions.map(({ created_at, summary }, index) => ({
      authors: [{ id: READER, kind: "user" }],
      created_at,
      named: summary !== null,
      number: index + 1,
      summary,
    })),
  } as ArtifactWithTrees;
}

function issueDetails(key: string): IssueDetails | undefined {
  const issue = ISSUES.find((one) => one.key === key);
  const document = DOCUMENTS.find((one) => one.issue === key);
  if (issue === undefined || document === undefined) return undefined;
  const open = asksOf(key, "open", false);
  return {
    ...issueSummary(issue, open.length),
    artifacts: [artifactOf(document)],
    children: [],
    components: { ids: [], inherited_from: null, mode: "none", reason: null, unknown: [] },
    decisions_artifact_id: "",
    design_gates: [],
    external_links: [],
    holder_state: null,
    last_seen: null,
    number: Number(key.split("-")[1]),
    open_asks: open,
    plan_artifact_id: "",
    progress: { children: null, tasks: null },
    project: PROJECT,
    referenced_by_count: 0,
    takeable_at: null,
    verification_artifact_id: "",
  } as IssueDetails;
}

function withDocument(id: string, answer: (document: DemoDocument) => unknown) {
  const document = documentById(id);
  return document === undefined ? notFound("ARTIFACT_NOT_FOUND") : ok(answer(document));
}

export const DOCUMENT_ROUTES: DemoRoute[] = [
  {
    method: "GET",
    pattern: /^\/api\/v1\/issues\/([^/]+)$/,
    answer: ([key]) => {
      const details = issueDetails(key ?? "");
      return details === undefined ? notFound("ISSUE_NOT_FOUND") : ok(details);
    },
  },
  {
    method: "GET",
    pattern: /^\/api\/v1\/issues\/([^/]+)\/asks$/,
    answer: ([key], q) => ok(asksOf(key ?? "", q.get("state"), q.get("thread") === "1")),
  },
  {
    // What a reference to an issue shows: a text that names one (`PANE-5's spec`) reads it.
    method: "GET",
    pattern: /^\/api\/v1\/issues\/([^/]+)\/summary$/,
    answer: ([key]) => {
      const issue = ISSUES.find((one) => one.key === key);
      const document = DOCUMENTS.find((one) => one.issue === key);
      if (issue === undefined || document === undefined) return notFound("NOT_FOUND");
      return ok({
        documents: [
          {
            id: specId(issue.key),
            kind: "doc",
            latest_version: document.versions.length,
            name: "spec.md",
            primary: true,
            slug: "spec-md",
          },
        ],
        key: issue.key,
        project: PROJECT,
        status: issue.status,
        title: issue.title,
      });
    },
  },
  // No message, handoff or event of the issues is invented: their lists are empty.
  {
    method: "GET",
    pattern: /^\/api\/v1\/issues\/([^/]+)\/comments$/,
    // An issue's own comments carry no document; a read of one document's comments gets those.
    answer: ([key], q) =>
      ok(
        (COMMENTS[key ?? ""] ?? []).filter(
          (comment) => !q.has("artifact") || comment.artifact_id === q.get("artifact")
        )
      ),
  },
  { method: "GET", pattern: /^\/api\/v1\/issues\/[^/]+\/messages$/, answer: () => ok([]) },
  { method: "GET", pattern: /^\/api\/v1\/issues\/[^/]+\/handoffs$/, answer: () => ok([]) },
  { method: "GET", pattern: /^\/api\/v1\/issues\/[^/]+\/events$/, answer: () => ok([]) },
  {
    method: "GET",
    pattern: /^\/api\/v1\/issues\/([^/]+)\/decisions$/,
    // No decision is recorded yet; the issue's open asks are its open questions.
    answer: ([key]) =>
      ok({
        decisions: [],
        document: null,
        open_questions: asksOf(key ?? "", "open", false),
      } satisfies IssueDecisions),
  },
  {
    method: "GET",
    pattern: /^\/api\/v1\/issues\/([^/]+)\/coverage$/,
    // No plan or check serves a requirement yet.
    answer: ([key]) =>
      ok({
        gaps: [],
        has_plan: false,
        issue: key ?? "",
        requirements: [],
        spec_artifact_id: specId(key ?? ""),
      } satisfies IssueCoverage),
  },
  // The issue header's repository and component pickers: no repository is linked to a project.
  { method: "GET", pattern: /^\/api\/v1\/settings\/repo-projects$/, answer: () => ok([]) },
  {
    method: "GET",
    pattern: /^\/api\/v1\/projects\/[^/]+\/architecture-sources$/,
    answer: () => ok([]),
  },
  {
    method: "GET",
    pattern: /^\/api\/v1\/artifacts\/([^/]+)$/,
    answer: ([id]) => withDocument(id ?? "", artifactOf),
  },
  {
    method: "GET",
    pattern: /^\/api\/v1\/artifacts\/([^/]+)\/text$/,
    answer: ([id]) =>
      withDocument(
        id ?? "",
        (document): ArtifactText => ({
          markdown: document.markdown,
          token: `${specId(document.issue)}@${document.versions.length}`,
          version: document.versions.length,
        })
      ),
  },
  {
    method: "GET",
    pattern: /^\/api\/v1\/artifacts\/([^/]+)\/versions\/(\d+)$/,
    answer: ([id, number]) => {
      const document = documentById(id ?? "");
      const version = document?.versions[Number(number) - 1];
      if (document === undefined || version === undefined) return notFound("VERSION_NOT_FOUND");
      // Each version reads as the live text: no fixture holds an earlier wording.
      return ok({
        authors: [{ id: READER, kind: "user" }],
        created_at: version.created_at,
        markdown: document.markdown,
        named: version.summary !== null,
        number: Number(number),
        summary: version.summary,
      } satisfies ArtifactVersionText);
    },
  },
  // The references the margin counts beside each block: none is invented.
  { method: "GET", pattern: /^\/api\/v1\/artifacts\/[^/]+\/blocks$/, answer: () => ok([]) },
  {
    method: "GET",
    pattern: /^\/api\/v1\/me\/documents\/([^/]+)\/read$/,
    answer: ([id]) =>
      ok({ artifact_id: id ?? "", read_at: null, version: null } satisfies DocumentRead),
  },
  {
    method: "PUT",
    pattern: /^\/api\/v1\/me\/documents\/([^/]+)\/read$/,
    answer: ([id], _, body) =>
      ok({
        artifact_id: id ?? "",
        read_at: goTime(new Date()),
        version: (body as { version?: number } | undefined)?.version ?? null,
      } satisfies DocumentRead),
  },
];
