import type {
  Advised,
  BasisLearnedAt,
  CheckBasis,
  CheckBlock,
  CheckCI,
  CheckPassStands,
  CheckResult,
  ClaimedIssue,
  DuplicateCandidate,
  Issue,
  IssueCheckResults,
  IssueComponents,
  IssueComponentsInput,
  IssueDetails,
  IssueRouteStatus,
} from "@legion/contracts";
import { DEFAULT_ISSUE_PAGE_LIMIT } from "@legion/contracts";
import { actorText } from "../actor-text";
import {
  type DispatchClient,
  DispatchGatewayError,
  DispatchServiceError,
  DispatchWriteLandedError,
} from "../dispatch-http";
import { dispatchChildRef, dispatchIssueRef, issueTopic } from "../dispatch-owner";
import { messageFor } from "../errors";
import { relatedCheckLines } from "../related-check";
import { pageSummaryText, searchAnswer } from "../search-answer";
import { otherAdviceLines, renderAdvice } from "./advice";
import {
  type CommandContext,
  type DispatchToolResult,
  defined,
  optionalBoolean,
  optionalComponents,
  optionalNullableString,
  optionalNumber,
  optionalPriority,
  optionalPriorityFilter,
  optionalString,
  optionalStringArray,
  stringArg,
} from "./context";
import { dispatchAnswered, refusalWithCode, withAccount, writeMayHaveLanded } from "./failures";
import { claimText, holdsSession, liveAgents, progressText, routeText } from "./render-issue";
import { notSubscribed } from "./results";

/** The change line after a components write: what the issue's own attachment became. */
function componentsChange(input: IssueComponentsInput, after: IssueComponents): string {
  switch (input.mode) {
    case "explicit":
      return `components -> explicit [${(after.ids ?? []).join(", ")}]`;
    case "none":
      return `components -> none (${after.reason ?? input.reason ?? ""})`;
    case "inherit":
      return "components -> inherit";
    default:
      return `components -> ${input.mode}`;
  }
}

const architectureComponentsAction =
  'Review the `dispatch` skill, "Architecture components", to attach it to the parts it changes or mark it as non-architectural with a reason.';

/** What both claim results say when the server answers `legion_managed_status`: on an issue
 *  Legion runs, only its daemon writes the status. One copy, since the claim and the release
 *  must say the same thing. */
const LEGION_OWNS_STATUS =
  "Legion's daemon moves this issue's status on an issue it runs, so leave it alone — a status written here is undone.";

/**
 * Guidance only: creation already succeeded, so an unavailable source lookup must not turn this
 * into a failed write.
 */
async function architectureGuidance(
  client: DispatchClient,
  project: string,
  components: IssueComponents
): Promise<string | undefined> {
  if (components.mode === "none" || (components.ids ?? []).length > 0) return undefined;

  try {
    if ((await client.listArchitectureSources(project)).length === 0) return undefined;
    return `Project ${project} has an architecture model, but this issue is not linked to any of its current components. ${architectureComponentsAction}`;
  } catch (error) {
    return `Could not check whether project ${project} has an architecture model: ${messageFor(error)}. ${architectureComponentsAction}`;
  }
}
interface DuplicateCandidateShape {
  readonly key?: unknown;
  readonly title?: unknown;
  readonly status?: unknown;
  readonly snippet?: unknown;
  readonly shared_terms?: unknown;
  readonly href?: unknown;
}

function isDuplicateCandidate(value: unknown): value is DuplicateCandidate {
  if (typeof value !== "object" || value === null) return false;
  const candidate = value as DuplicateCandidateShape;
  return (
    typeof candidate.key === "string" &&
    typeof candidate.title === "string" &&
    typeof candidate.status === "string" &&
    typeof candidate.snippet === "string" &&
    typeof candidate.shared_terms === "number" &&
    typeof candidate.href === "string"
  );
}

function duplicateCandidates(error: DispatchServiceError): DuplicateCandidate[] {
  if (error.candidates === undefined || !error.candidates.every(isDuplicateCandidate)) throw error;
  return error.candidates;
}
/** A Dispatch server from before the repository fields that takes a write naming `repo` answers
 *  without the field: it did not save the repository, and that must not pass as a success. The
 *  error says what the server did do, so the agent does not repeat the rest of the write. */
function refuseUnsavedRepository(
  verb: "created" | "updated" | "already holds",
  issue: { readonly key: string; readonly repo?: string | null },
  repo: string | undefined
): void {
  if (repo === undefined || issue.repo !== undefined) return;
  const lost = repo === "" ? "the repository was not cleared" : `${repo} was not saved`;
  throw new Error(
    `Dispatch ${verb} ${issue.key} without its repository: this Dispatch server predates repository fields, so ${lost}; upgrade the server`
  );
}

/** `dispatch issue`: open an issue, or report the one an external reference already holds. */
export async function issueCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, linkBase, writingOverride } = context;
  const project = stringArg(args, "project");
  const title = stringArg(args, "title");
  const parent = optionalString(args, "parent");
  const external = optionalString(args, "external");
  const force = optionalBoolean(args, "force");
  const spec = optionalString(args, "spec");
  const priority = optionalPriority(args, "priority");
  const assignee = optionalString(args, "assignee");
  const components = optionalComponents(args, "components");
  const repo = optionalString(args, "repo");
  const labels = args.labels;
  const blockedBy = args.blocked_by;
  try {
    const created = await client.issue({
      project,
      title,
      ...(parent === undefined ? {} : { parent }),
      ...(Array.isArray(blockedBy) ? { blocked_by: blockedBy as string[] } : {}),
      ...(external === undefined ? {} : { external }),
      ...(force === undefined ? {} : { force }),
      ...(spec === undefined ? {} : { spec }),
      ...(priority === undefined ? {} : { priority }),
      ...(assignee === undefined ? {} : { assignee }),
      ...(components === undefined ? {} : { components }),
      ...(repo === undefined ? {} : { repo }),
      ...(Array.isArray(labels) ? { labels: labels as string[] } : {}),
      ...writingOverride,
      actor,
    });
    refuseUnsavedRepository(created.created ? "created" : "already holds", created, repo);
    const adviceLines = await otherAdviceLines(async () => {
      const componentGuidance = await architectureGuidance(
        client,
        created.project,
        created.components
      );
      return [
        ...renderAdvice(input.tool, created.key, created.advice, {
          isPrimarySpec: spec !== undefined,
        }),
        ...(componentGuidance === undefined ? [] : [componentGuidance]),
      ];
    });
    const href = new URL(`/issues/${encodeURIComponent(created.key)}`, linkBase).toString();
    // Dispatch picks the assignee when the call names none (the creating person, the personal
    // token's owner, the parent's, or the project's default), so the line says whom it
    // answered.
    const assigned =
      created.assignee === undefined
        ? ""
        : created.assignee === null
          ? "; unassigned"
          : `; assigned to ${created.assignee}`;
    const suffixes = `${assigned}${await notSubscribed(issueTopic(created.key), input.subscriptions)}`;
    // Dispatch answers an `external` reference it already holds with that issue and nothing
    // new, so no related-item check was made and "search before you rely on this being new"
    // does not apply: the first line says so instead.
    return {
      text: [
        created.created
          ? `Created ${created.key}: ${created.title} → ${href}${suffixes}`
          : `Dispatch already holds ${created.key}: ${created.title} → ${href}; nothing was created${suffixes}`,
        ...(created.created
          ? relatedCheckLines(created.advice?.suggestions, {
              project: created.project,
              text: title,
              configUrl: linkBase,
            })
          : []),
        ...adviceLines,
      ].join("\n"),
      details: {
        issue: created.key,
        ...(created.advice === undefined ? {} : { advice: created.advice }),
      },
    };
  } catch (error) {
    if (!(error instanceof DispatchServiceError) || error.code !== "POSSIBLE_DUPLICATE") {
      throw error;
    }
    const candidates = duplicateCandidates(error);
    return {
      text: [
        `Not created: "${title}" is a near copy of an open issue.`,
        ...candidates.map((candidate) => {
          const href = new URL(candidate.href, linkBase).toString();
          return `${candidate.key} [${candidate.status}] ${candidate.title} → ${href}`;
        }),
        "Read each issue above. If one is the same work, fold this into it by the duplicate rule of skill://dispatch-first.",
        "If none is, run dispatch issue again with --force, and write in the new issue's spec why it is not each issue named here.",
      ].join("\n"),
      details: { duplicates: candidates },
      refused: true,
    };
  }
}

/** `dispatch issue-update`: change one issue's fields, posting a closing reason first. */
export async function issueUpdateCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, issue, writingOverride } = context;
  const issueKey = issue();
  const status = optionalString(args, "status");
  const reason = optionalString(args, "reason");
  const title = optionalString(args, "title");
  const route = optionalString(args, "route");
  const parent = optionalString(args, "parent");
  const components = optionalComponents(args, "components");
  const repo = optionalString(args, "repo");
  const rankBefore = optionalString(args, "before");
  const rankAfter = optionalString(args, "after");
  const priority = optionalPriority(args, "priority");
  const assignee = optionalNullableString(args, "assignee");
  const labels = Array.isArray(args.labels) ? (args.labels as string[]) : undefined;
  const blockedBy = Array.isArray(args.blocked_by) ? (args.blocked_by as string[]) : undefined;
  const requestedLinks = Array.isArray(args.external_links)
    ? [...new Set(args.external_links as string[])]
    : undefined;
  let before: IssueDetails;
  try {
    before = await client.getIssue(issueKey);
  } catch (error) {
    throw refusalWithCode(error);
  }
  // The schema admits reason only beside status done. The reason is posted first, so the
  // close carries its note and an issue that cannot take the note stays open. Dispatch takes
  // messages on a closed issue (though not asks or artifacts) and answers a second close with
  // ISSUE_CLOSED, so an issue already closed gets that refusal here, before its reason could
  // land as a stray message.
  if (status === "done" && typeof before.closed_at === "string") {
    throw refusalWithCode(
      new DispatchServiceError("ISSUE_CLOSED", 409, "issue is closed"),
      `${issueKey} closed at ${before.closed_at}, so the reason was not posted and the close was not sent`
    );
  }
  let closingNote: { readonly id: string; readonly ref: string } | undefined;
  if (reason !== undefined) {
    try {
      const message = await client.message(issueKey, {
        body: reason,
        ...writingOverride,
        actor,
      });
      closingNote = {
        id: message.id,
        ref: dispatchChildRef(dispatchIssueRef(issueKey), "message", message.id),
      };
    } catch (error) {
      if (error instanceof DispatchWriteLandedError) {
        throw new Error(
          `${error.message} That was the closing reason, so the close was not sent: read the issue's messages for it, then close with a reason that points at it, since retrying this call posts its reason again.`,
          { cause: error }
        );
      }
      // The close's PATCH below follows the same rule: only an answer sent before the reason
      // could be stored proves it was not posted.
      const told = writeMayHaveLanded(error)
        ? "the reason may or may not have been posted, and the close was not sent: read the issue's messages before retrying, since retrying this call posts its reason again"
        : "the reason was not posted, so the close was not sent";
      if (error instanceof DispatchServiceError) throw refusalWithCode(error, told);
      throw withAccount(error, told);
    }
  }
  // The server replaces the whole link set. The common call is "link the pull request I just
  // opened", so a non-empty list merges by URL and keeps every existing link (and its kind).
  // An explicit empty list (only `--clear-external-links` sends one) is a genuine clear: it
  // replaces the set with nothing rather than merging in zero new links, which would just
  // re-send the issue's current links unchanged.
  const existingLinks = before.external_links ?? [];
  const linked = existingLinks.map((link) => link.url);
  const newLinks = requestedLinks?.filter((url) => !linked.includes(url)) ?? [];
  const sentLinks =
    requestedLinks === undefined
      ? undefined
      : requestedLinks.length === 0
        ? []
        : [...existingLinks, ...newLinks.map((url) => ({ url }))];
  // A rank with neither neighbour is not a no-op: the server ranks the issue last in its
  // project. So an ordinary update sends no rank key at all.
  const rank =
    rankBefore === undefined && rankAfter === undefined
      ? undefined
      : {
          ...(rankBefore === undefined ? {} : { before: rankBefore }),
          ...(rankAfter === undefined ? {} : { after: rankAfter }),
        };
  let after: Advised<Issue>;
  try {
    after = await client.updateIssue(issueKey, {
      ...(status === undefined ? {} : { status }),
      ...(title === undefined ? {} : { title }),
      ...(labels === undefined ? {} : { labels }),
      ...(priority === undefined ? {} : { priority }),
      ...(assignee === undefined ? {} : { assignee }),
      ...(route === undefined ? {} : { route }),
      ...(parent === undefined ? {} : { parent: parent === "" ? null : parent }),
      ...(blockedBy === undefined ? {} : { blocked_by: blockedBy }),
      ...(components === undefined ? {} : { components }),
      ...(repo === undefined ? {} : { repo: repo === "" ? null : repo }),
      ...(sentLinks === undefined ? {} : { external_links: sentLinks }),
      ...(rank === undefined ? {} : { rank }),
      ...writingOverride,
      actor,
    });
  } catch (error) {
    // The update is this call's own last write: one that landed without its answer is the
    // call's landing, which the caller reports as done.
    if (error instanceof DispatchWriteLandedError) throw error;
    // A URL links exactly one issue. A server from before EXTERNAL_LINK_TAKEN answers the
    // unique-index violation with 500 INTERNAL, which names nothing; say what it means. A
    // gateway's 500 page is not that answer, and gets no such reading.
    const taken =
      dispatchAnswered(error, 500) && newLinks.length > 0
        ? `one of ${newLinks.join(", ")} may already be linked from another issue (a URL links exactly one issue)`
        : "";
    if (closingNote === undefined) throw refusalWithCode(error, taken);
    // The reason is on the issue, so a blind retry would post it a second time: the error
    // says where the first one is, and whether the close may have landed anyway. A gateway's
    // timeout or rate limit refused nothing there is to fix.
    const posted = `the reason already landed as message ${closingNote.id} (${closingNote.ref})`;
    const fix =
      error instanceof DispatchGatewayError && error.transient
        ? ""
        : "fix what refused the close, then ";
    const landed = writeMayHaveLanded(error)
      ? `${posted}, and the close may or may not have taken effect. Read the issue's status before retrying: done means it closed; otherwise retry with a reason that points at message ${closingNote.id}, since retrying this call posts its reason again`
      : `${posted} but the issue did not close. Retrying this call posts its reason again, so ${fix}retry with a reason that points at message ${closingNote.id}`;
    if (error instanceof DispatchServiceError) throw refusalWithCode(error, taken, landed);
    throw withAccount(error, landed);
  }
  refuseUnsavedRepository("updated", after, repo);
  const links = after.external_links ?? [];
  const linkCount = `(${links.length} ${links.length === 1 ? "link" : "links"})`;
  const changes = [
    ...(closingNote === undefined
      ? []
      : [`reason posted as message ${closingNote.id} (${closingNote.ref})`]),
    ...(status === undefined ? [] : [`status ${before.status} -> ${after.status}`]),
    ...(title === undefined ? [] : [`title "${after.title}"`]),
    ...(labels === undefined
      ? []
      : [
          (after.labels ?? []).length === 0
            ? "labels cleared"
            : `labels ${(after.labels ?? []).join(", ")}`,
        ]),
    ...(priority === undefined
      ? []
      : [after.priority === null ? "priority cleared" : `priority -> P${after.priority}`]),
    ...(assignee === undefined
      ? []
      : [after.assignee === null ? "assignee cleared" : `assignee -> ${after.assignee}`]),
    ...(rank === undefined
      ? []
      : [
          `rank -> ${[
            ...(rankAfter === undefined ? [] : [`after ${rankAfter}`]),
            ...(rankBefore === undefined ? [] : [`before ${rankBefore}`]),
          ].join(", ")}`,
        ]),
    ...(requestedLinks === undefined
      ? []
      : [
          requestedLinks.length === 0
            ? "external links cleared"
            : newLinks.length === 0
              ? `already linked ${requestedLinks.join(", ")} ${linkCount}`
              : `linked ${newLinks.join(", ")} ${linkCount}`,
        ]),
    ...(route === undefined
      ? []
      : [after.route === null ? "route cleared" : `route ${after.route}`]),
    ...(parent === undefined
      ? []
      : [after.parent === null ? "parent cleared" : `parent -> ${after.parent}`]),
    ...(blockedBy === undefined
      ? []
      : [
          (after.blocked_by ?? []).length === 0
            ? "blocked_by cleared"
            : `blocked by ${(after.blocked_by ?? []).join(", ")}`,
        ]),
    ...(components === undefined ? [] : [componentsChange(components, after.components)]),
    ...(repo === undefined
      ? []
      : [
          after.repo === null
            ? `repo cleared (resolves to ${after.resolved_repo ?? "none"})`
            : `repo -> ${after.repo}`,
        ]),
  ];
  const adviceLines = renderAdvice(input.tool, after.key, after.advice, {
    setsStatus: status !== undefined,
  });
  return {
    text: [
      `${after.key}: ${changes.join("; ")}${await notSubscribed(issueTopic(after.key), input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details: {
      issue: after.key,
      status: after.status,
      external_links: (after.external_links ?? []).map((link) => link.url),
      ...(closingNote === undefined ? {} : { message: closingNote.id }),
      ...(after.advice === undefined ? {} : { advice: after.advice }),
    },
  };
}

/** `dispatch claim`: take the issue's claim for this session, or release the one it holds. */
export async function claimCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, issue } = context;
  const issueKey = issue();
  const release = optionalBoolean(args, "release") ?? false;
  let after: ClaimedIssue;
  try {
    after = release
      ? await client.releaseIssueClaim(issueKey, { actor })
      : await client.claimIssue(issueKey, { actor });
  } catch (error) {
    // ISSUE_CLAIMED and CLAIM_CONTENDED are two different refusals with two different
    // answers, and the tool description and the skill both name the codes.
    throw refusalWithCode(error);
  }
  const held = after.claim;
  // On an issue Legion runs, only its daemon writes the status: a status this session sets
  // is undone, so the result says who owns it rather than asking for a move.
  // An absent field (an older Dispatch, or a failed label walk) reads as not managed.
  const legionManaged = after.legion_managed_status === true;
  // A release answers with the claim cleared or it does not answer at all: the server
  // refuses a release the caller may not make (409 ISSUE_CLAIMED, naming the live holder),
  // and loads the issue inside the releasing transaction. So there is no "released but
  // still claimed" state to render here.
  let text: string;
  if (release) {
    const released = `${issueKey}: claim released; nobody is working it now. Its status is still ${after.status}`;
    text = legionManaged
      ? `${released}; ${LEGION_OWNS_STATUS}`
      : `${released} — move it yourself if that is no longer where the work is.`;
  } else {
    if (held === null) {
      // A successful claim always answers with the claim in place; anything else is a
      // server that no longer matches this contract, worth saying rather than papering over.
      throw new Error(`Dispatch claimed ${issueKey} but answered with no claim`);
    }
    const claimed = `${issueKey}: claimed by you since ${held.at}. Its status is ${after.status}`;
    // The claimant route (Dispatch's outbox) delivers a person's answers, comments and
    // messages on the issue to this session; the notSubscribed suffix below is about every
    // other event on the issue.
    const reaches = `A person's answers, comments and messages on ${issueKey} now reach you directly.`;
    text = legionManaged
      ? `${claimed}; ${LEGION_OWNS_STATUS} Release the claim when you stop. ${reaches}`
      : `${claimed}; a claim moves nothing, so move it to in_progress with dispatch issue-update when you start, and release the claim when you stop. ${reaches}`;
  }
  return {
    text: `${text}${await notSubscribed(issueTopic(issueKey), input.subscriptions)}`,
    details: { issue: issueKey, status: after.status, claim: held },
  };
}

/** `dispatch search`: the issues, documents and items matching one query. */
export async function searchCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, linkBase } = context;
  const query = stringArg(args, "query");
  const project = optionalString(args, "project");
  const limit = optionalNumber(args, "limit");
  const offset = optionalNumber(args, "offset");
  const search = await client.search(query, {
    ...(project === undefined ? {} : { project }),
    ...(limit === undefined ? {} : { limit }),
    ...(offset === undefined ? {} : { offset }),
  });
  return searchAnswer(search, query, offset, linkBase);
}

/** `dispatch related`: the issues a title and text look like, before one more is opened. */
export async function relatedCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, linkBase } = context;
  const project = stringArg(args, "project");
  const title = optionalString(args, "title");
  const text = optionalString(args, "text");
  const excludeIssues = optionalStringArray(args, "exclude_issues");
  const keywordsOnly = optionalBoolean(args, "keywords_only");
  const suggestions = await client.related(project, {
    ...(title === undefined ? {} : { title }),
    ...(text === undefined ? {} : { text }),
    ...(excludeIssues === undefined ? {} : { exclude_issues: excludeIssues }),
    ...(keywordsOnly === undefined ? {} : { keywords_only: keywordsOnly }),
  });
  return {
    text: relatedCheckLines(suggestions, {
      project,
      text: [title, text].filter((part) => part !== undefined).join("\n"),
      configUrl: linkBase,
    }).join("\n"),
    details: { project, suggestions },
  };
}

/** `dispatch issues`: one project's issues, filtered and paged as the arguments ask. */
export async function issuesCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const project = stringArg(args, "project");
  const status = optionalString(args, "status");
  const parent = optionalString(args, "parent");
  const label = optionalString(args, "label");
  const priority = optionalPriorityFilter(args, "priority");
  const updatedSince = optionalString(args, "updated_since");
  // The zod spec already refused anything but one of ISSUE_ROUTE_STATUSES.
  const routeStatus = optionalString(args, "route_status") as IssueRouteStatus | undefined;
  const filters = {
    project,
    ...(status === undefined ? {} : { status }),
    ...(parent === undefined ? {} : { parent }),
    ...(label === undefined ? {} : { label }),
    ...(priority === undefined ? {} : { priority }),
    ...(updatedSince === undefined ? {} : { updated_since: updatedSince }),
    ...(routeStatus === undefined ? {} : { route_status: routeStatus }),
  };
  // --all reads the whole filtered listing in one request, which Dispatch answers as a bare
  // array; argumentProblems has already refused it beside --limit or --offset. Everything
  // else pages exactly as before, the tool's schema having refused a limit outside
  // 1..MAX_ISSUE_PAGE_LIMIT and a negative or fractional offset.
  const page =
    optionalBoolean(args, "all") === true
      ? await client.listAllIssues(filters).then((issues) => ({
          issues,
          total: issues.length,
          limit: issues.length,
          offset: 0,
        }))
      : await client.listIssuePage(filters, {
          limit: optionalNumber(args, "limit") ?? DEFAULT_ISSUE_PAGE_LIMIT,
          offset: optionalNumber(args, "offset") ?? 0,
        });
  const { total, limit, offset } = page;
  const rows = (page.issues ?? []).map((row) => ({
    key: row.key,
    title: row.title,
    status: row.status,
    priority: row.priority,
    parent: row.parent,
    labels: row.labels ?? [],
    open_asks: row.open_asks,
    claim: row.claim ?? null,
    route: row.route ?? null,
    route_status: row.route_status ?? null,
    route_holder: row.route_holder ?? null,
    route_status_reason: row.route_status_reason ?? null,
    updated_at: row.updated_at,
    progress: row.progress,
  }));
  const agents = await liveAgents(
    client,
    rows.some((row) => holdsSession(row.claim))
  );
  const isPartial = offset !== 0 || rows.length !== total;
  const showing = !isPartial
    ? ""
    : rows.length === 0
      ? `showing 0-0 of ${total}`
      : pageSummaryText(offset, rows.length, total);
  return {
    text:
      rows.length === 0
        ? `No issues in ${project}.${isPartial ? ` (${showing})` : ""}`
        : [
            `${rows.length} ${rows.length === 1 ? "issue" : "issues"} in ${project}` +
              (isPartial ? ` (${showing})` : ""),
            ...rows.map((row) => {
              const progress = progressText(row.progress, " · ");
              return (
                `${row.key} [${row.status}]${row.priority === null ? "" : ` P${row.priority}`} ${row.title}` +
                (row.open_asks === 0
                  ? ""
                  : ` · ${row.open_asks} open ${row.open_asks === 1 ? "ask" : "asks"}`) +
                (row.claim === null ? "" : ` · claimed by ${claimText(row.claim, agents)}`) +
                (progress === "" ? "" : ` · ${progress}`) +
                // A route that reaches a live session changes nothing about the row; one
                // that reaches nobody, or cannot be judged, is what the owner audit reads.
                (row.route === null || row.route_status === "live" || row.route_status === null
                  ? ""
                  : ` · route ${routeText(row)}`)
              );
            }),
          ].join("\n"),
    details: { issues: rows, total, offset, limit },
  };
}

/** How many characters of a reading `--brief` keeps. R2 of the spec sets the number: a reader
 *  who wants the gate alone on an issue holding hundreds of results reads this much of a reading
 *  that stands at none of the compared commits, and the whole of every other one. */
const BRIEF_READING_CHARACTERS = 200;

/** The three reasons a check compared its results with nothing, and the two a result's commit
 *  cannot be placed, in words a reader acts on. A check takes all three and a result all but
 *  no_pull_request_or_branch, so the check's set types this map and answers both. */
const COMPARISON_REASONS: Readonly<Record<NonNullable<CheckBlock["compared_nothing"]>, string>> = {
  no_pull_request_or_branch: "the issue has no pull request and no branch to compare with",
  repository_unreadable: "Dispatch cannot read the repository",
  read_failed: "a read Dispatch made did not answer",
};

/** When Dispatch learned a pull request's head, and which time that is: `read_at` is Dispatch's
 *  own read of the pull request, `updated_at` is GitHub's update time as a webhook carried it. */
function learnedText(learned: BasisLearnedAt): string {
  if (learned.kind === "read_at") return `Dispatch read it ${learned.at}`;
  return `GitHub updated it ${learned.at}`;
}

/** One thing a check compared its results with, as the head line names it. */
function basisText(basis: CheckBasis): string {
  const learned = basis.learned_at === undefined ? "" : ` (${learnedText(basis.learned_at)})`;
  if (basis.kind === "pull_request") {
    const number = basis.pull_request === undefined ? "" : `#${basis.pull_request} `;
    return `the head of pull request ${number}at ${basis.commit ?? "an unnamed commit"}${learned}`;
  }
  if (basis.kind === "branch") return `branch ${basis.branch ?? "unnamed"}${learned}`;
  return `the commit you named, ${basis.commit ?? ""}${learned}`;
}

/** Where a result's commit stands, as the basis it matched names that place. The head line has
 *  already printed each basis whole, so this names it short. */
function basisPlace(basis: CheckBasis): string {
  if (basis.kind === "pull_request") {
    return basis.pull_request === undefined
      ? "the head of a pull request"
      : `the head of pull request #${basis.pull_request}`;
  }
  if (basis.kind === "branch") return `on branch ${basis.branch ?? "unnamed"}`;
  return "the commit you named";
}

/** The first of R4's two facts: where one result's commit stands against what its check compared
 *  with. A Dispatch server that does not answer it says so rather than reading as another
 *  commit. */
function commitStandsText(stands: CheckResult["commit_stands"]): string {
  if (stands === undefined) {
    return "Where that commit stands is not known: this Dispatch server does not say";
  }
  if (stands.answer === "at_basis") {
    return stands.basis === undefined
      ? "That commit is one of the compared commits"
      : `That commit is ${basisPlace(stands.basis)}`;
  }
  if (stands.answer === "other_commit") return "That commit is none of the compared commits";
  // The reason is the server's to give and may be absent, so the answer stands on its own
  // where there is none.
  return stands.reason === undefined
    ? "Where that commit stands is not known"
    : `Where that commit stands is not known: ${COMPARISON_REASONS[stands.reason]}`;
}

/** R4's other fact: whether the check's words moved after the result was recorded. Both versions
 *  are named either way, because they are the handles a reader reads the two wordings with. */
function checkWordsText(check: CheckBlock, result: CheckResult): string {
  const versions = `result version ${result.check_version}, check version ${check.version}`;
  if (result.check_words_moved === undefined) {
    return `Whether the check's words moved is not known (${versions})`;
  }
  return result.check_words_moved
    ? `The check's words moved after this result (${versions})`
    : `The check's words did not move after this result (${versions})`;
}

/** R5 for one check: whether a pass of it stands, and from which roles. */
function passStandsText(pass: CheckPassStands | undefined): string {
  if (pass === undefined)
    return "Whether a pass stands is not known: this Dispatch server does not say";
  if (pass.answer === "yes") {
    const roles = pass.roles ?? [];
    return roles.length === 0
      ? "A pass stands: yes"
      : `A pass stands: yes, from ${roles.join(", ")}`;
  }
  if (pass.answer === "no") return "A pass stands: no";
  return "A pass stands: not known";
}

/** The reading one role recorded, under the line of its result at `indent`. `brief` cuts a reading
 *  whose result stands at none of the compared commits, and leaves every other reading whole: the
 *  reading is the record, and a reader who asked for the gate alone still reads the whole of what
 *  stands. */
function readingLines(result: CheckResult, brief: boolean, indent = "    "): string[] {
  const observed = result.observed;
  if (observed === "") return [`${indent}The role recorded no reading.`];
  const cut = brief && result.commit_stands?.answer !== "at_basis";
  if (cut && observed.length > BRIEF_READING_CHARACTERS) {
    return [
      `${indent}Reading: ${observed.slice(0, BRIEF_READING_CHARACTERS)}… (result ${result.id})`,
    ];
  }
  return [`${indent}Reading: ${observed}`];
}

/** One result, in the order the API served it: the verdict, the role, the whole commit, R4's two
 *  facts, the time, the id, the retraction that stands against it, and the reading. */
function resultLines(check: CheckBlock, result: CheckResult, brief: boolean): string[] {
  const lines = [
    `  - ${result.verdict} by ${result.role} at ${result.commit_sha}. ${commitStandsText(result.commit_stands)}. ${checkWordsText(check, result)}. Recorded ${result.created_at}. Result ${result.id}.`,
  ];
  const retracted = result.retracted;
  if (retracted !== undefined) {
    lines.push(
      `    Retracted by ${actorText(retracted.actor)} at ${retracted.created_at}. The reason: ${retracted.reason}`
    );
  }
  return [...lines, ...readingLines(result, brief)];
}

/** A check CI proves, which Dispatch marks with `ci`: no role records its result, so the check and
 *  its one result read as one line. A derived result names its verdict, its commit, the producer
 *  `ci`, the run and the conclusion that decided it, and where that commit stands, as a recorded
 *  result's line says it; a check the run did not decide says why, in the sentence Dispatch wrote.
 *  The check's words end the line. Whether a pass stands follows, from the same `pass_stands` and
 *  in the same words as a recorded check's head line, so the gate reads both kinds alike. The
 *  pull-request body line Dispatch wrote for the check follows, which the implementer copies, and
 *  then the run's reading: its conclusion, output and URL. */
function ciCheckLines(check: CheckBlock, ci: CheckCI, brief: boolean): string[] {
  const passStandsLine = `  ${passStandsText(check.pass_stands)}.`;
  const pullRequestLine = `  Pull request line: ${ci.line}`;
  const derived = check.results?.[0];
  if (derived === undefined) {
    return [
      `- ${check.block_id} (${check.when}): no result: ${ci.detail} ${check.text}`,
      passStandsLine,
      pullRequestLine,
    ];
  }
  const stands = commitStandsText(derived.commit_stands);
  return [
    `- ${check.block_id} (${check.when}): ${derived.verdict} at ${derived.commit_sha} by ${derived.role} (CI run ${ci.run}, ${ci.case}), ${stands.charAt(0).toLowerCase()}${stands.slice(1)}. ${check.text}`,
    passStandsLine,
    pullRequestLine,
    ...readingLines(derived, brief, "  "),
  ];
}

/** One check: a head line naming what it compared with and whether a pass of it stands, the
 *  check's own words once, then every result recorded against it. */
function checkLines(check: CheckBlock, brief: boolean): string[] {
  if (check.ci !== undefined) return ciCheckLines(check, check.ci, brief);
  const bases = check.compared_with ?? [];
  const reason = check.compared_nothing;
  const compared =
    bases.length > 0
      ? `Compared with ${bases.map(basisText).join(", ")}`
      : reason === undefined
        ? "Compared with nothing, and this Dispatch server does not say why"
        : `Compared with nothing: ${COMPARISON_REASONS[reason]}`;
  const results = check.results ?? [];
  return [
    `- ${check.block_id} (${check.when}). ${compared}. ${passStandsText(check.pass_stands)}.`,
    `  The check's words: ${check.text}`,
    ...(results.length === 0
      ? ["  No result is recorded for this check."]
      : results.flatMap((result) => resultLines(check, result, brief))),
  ];
}

/** What `dispatch results` prints: each check of the issue's Verification document, what Dispatch
 *  compared its results with, whether a pass of it stands, its own words once, and every result
 *  recorded against it in the order the API serves them. A check CI proves prints its derived
 *  result, or why it has none, as one line, then whether a pass of it stands (`ciCheckLines`). A
 *  check with no result says so, and an issue with no Verification document says that instead of
 *  printing an empty list: Dispatch answers `verification_artifact_id` "" for such an issue, never
 *  null, so that is what is compared. */
function formatCheckResults(results: IssueCheckResults, brief = false): string {
  if (results.verification_artifact_id === "") {
    return `${results.issue} has no Verification document, so it has no checks. Write one (verification.md) with a check block per thing that must be proved.`;
  }
  const checks = results.checks ?? [];
  if (checks.length === 0) {
    return `${results.issue}'s Verification document holds no check blocks.`;
  }
  const blocks = checks.map((check) => checkLines(check, brief).join("\n"));
  return [`Checks of ${results.issue}:`, ...blocks].join("\n\n");
}

/** `dispatch result`: record one result of one check of an issue's Verification document. */
export async function resultCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, writingOverride } = context;
  const retract = optionalString(args, "retract");
  if (retract !== undefined) {
    const retraction = await client.retractResult(retract, {
      actor,
      reason: stringArg(args, "reason"),
      ...writingOverride,
    });
    return {
      text: `Retracted result ${retraction.result_id}. ${actorText(retraction.actor)} retracted it at ${retraction.created_at}. The reason: ${retraction.reason}\n\nThe result stays readable, and it proves nothing from now on. Record a new result where the check still needs one.`,
      details: {
        result: retraction.result_id,
        reason: retraction.reason,
        created_at: retraction.created_at,
      },
    };
  }
  const issue = stringArg(args, "issue");
  // Each optional field is omitted rather than passed as `undefined`: the input's optional
  // properties are exact (`exactOptionalPropertyTypes`, which claude-envoy compiles this
  // file under), so `{ surface: undefined }` is not the same shape as `{}`.
  const optional = {
    ...defined("surface", optionalString(args, "surface")),
    ...defined("run", optionalString(args, "run")),
    ...defined("observed", optionalString(args, "observed")),
    ...defined("negative_control", optionalString(args, "negative_control")),
    ...defined("failed", optionalNumber(args, "failed")),
    ...defined("skipped", optionalNumber(args, "skipped")),
    ...defined("excluded", optionalNumber(args, "excluded")),
    ...defined("screenshots", optionalStringArray(args, "screenshots")),
  };
  const recorded = await client.recordResult(issue, {
    actor,
    check: stringArg(args, "check"),
    role: stringArg(args, "role"),
    verdict: stringArg(args, "verdict") as "pass" | "fail",
    commit: stringArg(args, "commit"),
    ...optional,
    ...writingOverride,
  });
  return {
    text: `Recorded ${recorded.result.verdict} for check ${recorded.result.check_block_id} of ${recorded.result.check_issue_key} at ${recorded.result.commit_sha}, as ${recorded.result.role}.\n\nPull request body line:\n${recorded.line}`,
    details: {
      issue,
      result: recorded.result.id,
      check: recorded.result.check_block_id,
      verdict: recorded.result.verdict,
      line: recorded.line,
    },
  };
}

/** `dispatch results`: each check of an issue's Verification document with every result recorded
 *  against it. */
export async function resultsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const issue = stringArg(args, "issue");
  const at = optionalString(args, "at");
  const brief = optionalBoolean(args, "brief") === true;
  const results = await client.issueResults(issue, at);
  const checks = results.checks ?? [];
  return {
    text: formatCheckResults(results, brief),
    details: {
      issue,
      checks: checks.length,
      results: checks.reduce((total, check) => total + (check.results ?? []).length, 0),
    },
  };
}
