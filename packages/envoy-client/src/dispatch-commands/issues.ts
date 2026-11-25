import type {
  Advised,
  CheckResultRecorded,
  ClaimedIssue,
  DuplicateCandidate,
  Issue,
  IssueComponents,
  IssueComponentsInput,
  IssueDetails,
  IssueRouteStatus,
} from "@legion/contracts";
import { DEFAULT_ISSUE_PAGE_LIMIT, issueStatusOptionsOf } from "@legion/contracts";
import { actorText } from "../actor-text";
import { commandLine, fieldFlag } from "../dispatch-command";
import {
  type DispatchClient,
  DispatchServiceError,
  DispatchWriteLandedError,
  DispatchWriteUnsettledError,
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
import {
  type ClosingNote,
  closeAccount,
  dispatchAnswered,
  refusalWithCode,
  withAccount,
  writeMayHaveLanded,
} from "./failures";
import { linkFor } from "./links";
import { claimText, holdsSession, liveAgents, progressText, routeText } from "./render-issue";
import { formatCheckResults } from "./render-results";
import { notSubscribed } from "./results";
import { issuesBeforeStatusChange, statusChangeCommand } from "./status-change";

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

/** A Dispatch server from before priorities answers a read without `advances_revision` and
 *  refuses a write naming `advances`, so a call that names `advances` says so and sends nothing,
 *  the closing reason included. */
function refuseServerWithoutAdvances(issue: IssueDetails): void {
  if (issue.advances_revision !== undefined) return;
  throw new Error(
    `Dispatch read ${issue.key} without its advances: this Dispatch server predates priorities, so nothing was sent; upgrade the server`
  );
}

/** The clause a 409 ADVANCES_REVISION adds to the refusal: how the caller answers it, since the
 *  server's sentence speaks of the API's field and not of this command's flags. In both of the
 *  server's sentences (`refuseAdvancesRevision`, internal/dispatch/api/issue_advances.go) the
 *  last number is the revision the issue holds, and no other field of the answer carries it, so
 *  the clause takes that number. Every other error adds nothing, so it reads as it did. */
function advancesRevisionAnswer(issueKey: string, error: unknown): string {
  if (!dispatchAnswered(error, 409) || error.code !== "ADVANCES_REVISION") return "";
  const held = /(\d+)\D*$/.exec(error.message)?.[1] ?? "<the revision named above>";
  return `read the issue with ${commandLine("dispatch_read", { issue: issueKey })}, then send the whole set you mean with ${fieldFlag("dispatch_issue_update", "advances_revision")} ${held}`;
}

/** `dispatch issue`: open an issue, or report the one an external reference already holds. */
export async function issueCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, linkBase } = context;
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
  const advances = optionalStringArray(args, "advances");
  try {
    const created = await client.issue({
      project,
      title,
      ...(parent === undefined ? {} : { parent }),
      ...(Array.isArray(blockedBy) ? { blocked_by: blockedBy as string[] } : {}),
      ...(advances === undefined ? {} : { advances }),
      ...(external === undefined ? {} : { external }),
      ...(force === undefined ? {} : { force }),
      ...(spec === undefined ? {} : { spec }),
      ...(priority === undefined ? {} : { priority }),
      ...(assignee === undefined ? {} : { assignee }),
      ...(components === undefined ? {} : { components }),
      ...(repo === undefined ? {} : { repo }),
      ...(Array.isArray(labels) ? { labels: labels as string[] } : {}),
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
    const href = linkFor(`/issues/${encodeURIComponent(created.key)}`, linkBase);
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
              linkBase,
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
          const href = linkFor(candidate.href, linkBase);
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

/** `dispatch issue-update`: change one issue's fields, posting a closing reason first. `--labels-add`
 *  adds labels to those the issue holds, inside Dispatch's write, where `--label` replaces them.
 *  With an option of the status change, it sends that route in place of the PATCH. */
export async function issueUpdateCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, issue } = context;
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
  const labelsAdd = optionalStringArray(args, "labels_add");
  const blockedBy = Array.isArray(args.blocked_by) ? (args.blocked_by as string[]) : undefined;
  const advances = optionalStringArray(args, "advances");
  const advancesRevision = optionalNumber(args, "advances_revision");
  const requestedLinks = Array.isArray(args.external_links)
    ? [...new Set(args.external_links as string[])]
    : undefined;
  // The schema refused an option beside any field but status and reason, include_children beside
  // unlink_children, and include_children with done.
  const options = issueStatusOptionsOf(args);
  let before: IssueDetails;
  try {
    before = await client.getIssue(issueKey);
  } catch (error) {
    throw refusalWithCode(error);
  }
  if (advances !== undefined) refuseServerWithoutAdvances(before);
  // The command replaces the issue's own set. The revision it carries is the caller's alone: a
  // revision says which read the whole set answers, and only a read the caller saw can be that
  // read. The revision of this command's own read would let the set retract a relation another
  // writer added after the caller last read the issue. With none, the server takes the write one
  // relation at a time and refuses one that moves two or more (409 ADVANCES_REVISION), naming the
  // revision it holds, and the catch below says how to answer that refusal.
  const advancesWrite =
    advances === undefined
      ? undefined
      : { advances, ...defined("advances_revision", advancesRevision) };
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
  const issuesBefore =
    options.length === 0 ? undefined : await issuesBeforeStatusChange(client, before, options);
  let closingNote: ClosingNote | undefined;
  if (reason !== undefined) {
    try {
      const message = await client.message(issueKey, { body: reason, actor });
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
  if (issuesBefore !== undefined) {
    return statusChangeCommand(context, before, options, issuesBefore, closingNote);
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
      ...(labelsAdd === undefined ? {} : { labels_add: labelsAdd }),
      ...(priority === undefined ? {} : { priority }),
      ...(assignee === undefined ? {} : { assignee }),
      ...(route === undefined ? {} : { route }),
      ...(parent === undefined ? {} : { parent: parent === "" ? null : parent }),
      ...(blockedBy === undefined ? {} : { blocked_by: blockedBy }),
      ...advancesWrite,
      ...(components === undefined ? {} : { components }),
      ...(repo === undefined ? {} : { repo: repo === "" ? null : repo }),
      ...(sentLinks === undefined ? {} : { external_links: sentLinks }),
      ...(rank === undefined ? {} : { rank }),
      actor,
    });
  } catch (error) {
    // The update is this call's own last write: one that landed without its answer is the
    // call's landing, which the caller reports as done.
    if (error instanceof DispatchWriteLandedError) throw error;
    // A URL links exactly one issue. A server from before EXTERNAL_LINK_TAKEN answers the
    // unique-index violation with 500 INTERNAL, which names nothing; say what it means. A
    // gateway's 500 page is not that answer, and gets no such reading. A 500 can also come after
    // the update committed, so the client ends such a write unsettled (write-outcome.ts), with
    // that answer as `last`: the hint reads it there and joins the unsettled account.
    const answered = error instanceof DispatchWriteUnsettledError ? error.last : error;
    const taken =
      dispatchAnswered(answered, 500) && newLinks.length > 0
        ? `one of ${newLinks.join(", ")} may already be linked from another issue (a URL links exactly one issue)`
        : "";
    const revisionClause = advancesRevisionAnswer(issueKey, error);
    if (closingNote === undefined) {
      if (taken !== "" && !(error instanceof DispatchServiceError)) {
        throw withAccount(error, taken);
      }
      throw refusalWithCode(error, taken, revisionClause);
    }
    // The reason is on the issue, so a blind retry would post it a second time: the error
    // says where the first one is, and whether the close may have landed anyway.
    const landed = closeAccount(error, closingNote);
    if (error instanceof DispatchServiceError) {
      throw refusalWithCode(error, taken, revisionClause, landed);
    }
    throw withAccount(error, [taken, landed].filter((clause) => clause !== "").join("; "));
  }
  refuseUnsavedRepository("updated", after, repo);
  const links = after.external_links ?? [];
  const linkCount = `(${links.length} ${links.length === 1 ? "link" : "links"})`;
  // What the issue states itself after the write: the answer carries the inherited ones beside
  // them, and this write replaced only its own.
  const stated = (after.advances ?? [])
    .filter((advance) => !advance.inherited)
    .map((advance) => advance.id);
  const changes = [
    ...(closingNote === undefined
      ? []
      : [`reason posted as message ${closingNote.id} (${closingNote.ref})`]),
    ...(status === undefined ? [] : [`status ${before.status} -> ${after.status}`]),
    ...(title === undefined ? [] : [`title "${after.title}"`]),
    ...(labels === undefined && labelsAdd === undefined
      ? []
      : [
          (after.labels ?? []).length > 0
            ? `labels ${(after.labels ?? []).join(", ")}`
            : labels === undefined
              ? "labels none"
              : "labels cleared",
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
    ...(advances === undefined
      ? []
      : [stated.length === 0 ? "advances cleared" : `advances ${stated.join(", ")}`]),
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
      linkBase,
    }).join("\n"),
    details: { project, suggestions },
  };
}

/** Whether the whole output of a call is one JSON object a program reads: `dispatch issues
 *  --json`. The CLI prints such an output whole, also on a host that cuts long output, since a cut
 *  object is no JSON object, and prints its own notes on standard error. */
export function printsJson(tool: string, args: Readonly<Record<string, unknown>>): boolean {
  return tool === "dispatch_issues" && args["json"] === true;
}

/** `dispatch issues`: one project's issues, filtered and paged as the arguments ask, as lines or,
 *  with `--json`, as one JSON object. */
export async function issuesCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, input } = context;
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
  const listing = { issues: rows, total, offset, limit };
  if (printsJson(input.tool, args)) {
    return { text: JSON.stringify(listing), details: listing };
  }
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
    details: listing,
  };
}

/** Whether the CLI's `--dry-run` of a call asks Dispatch what the call's write would answer: a
 *  `dispatch result` record, whose dry run prints the pull-request body line the record would
 *  print, since that line is Dispatch's to write and a copy written here would be a second
 *  grammar. Every other dry run, a retraction's included, prints the arguments and sends nothing. */
export function dryRunAsksDispatch(tool: string, args: Readonly<Record<string, unknown>>): boolean {
  return tool === "dispatch_result" && args["retract"] === undefined;
}

/** `dispatch result`: record one result of one check of an issue's Verification document, or, as
 *  the CLI's dry run, ask Dispatch for the pull-request body line the record would print. */
export async function resultCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor } = context;
  const retract = optionalString(args, "retract");
  if (retract !== undefined) {
    const retraction = await client.retractResult(retract, {
      actor,
      reason: stringArg(args, "reason"),
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
  const input = {
    actor,
    check: stringArg(args, "check"),
    role: stringArg(args, "role"),
    verdict: stringArg(args, "verdict") as "pass" | "fail",
    commit: stringArg(args, "commit"),
    ...optional,
  };
  if (context.input.dryRun === true) {
    const dry = await client.dryRunResult(issue, input);
    return {
      text: `Nothing was recorded: this is a dry run.\n\nPull request body line:\n${dry.line}`,
      details: { issue, check: input.check, verdict: input.verdict, line: dry.line },
    };
  }
  let recorded: CheckResultRecorded;
  try {
    recorded = await client.recordResult(issue, input);
  } catch (error) {
    // CHECK_CONTENDED is the one refusal of this route that asks for nothing but the same
    // call again: a document version reworded the check at both of the write's attempts, so
    // nothing was recorded and the next call resolves the check afresh. Every other refusal
    // it can give asks the caller to change something first, and reaches them as Dispatch
    // wrote it. Untouched, this one reaches them as prose with its code hidden, which reads
    // as a failure to work out rather than a write to repeat.
    if (dispatchAnswered(error, 409) && error.code === "CHECK_CONTENDED") {
      throw refusalWithCode(
        error,
        "the check moved twice while this result was being written, and nothing was recorded: run the same dispatch result command again"
      );
    }
    throw error;
  }
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
  // A basis omits the issue's own repository. Resolve it only for the two answers that name
  // that repository, once per check owner, including a check inherited from an ancestor.
  const repositories: Record<string, string> = {};
  const owners = new Set(
    checks
      .filter(
        (check) =>
          check.pass_stands?.reason === "program_not_named" ||
          check.pass_stands?.reason === "no_deploy"
      )
      .map((check) => check.issue_key)
  );
  await Promise.all(
    [...owners].map(async (owner) => {
      const issue = await client.getIssue(owner);
      if (issue.resolved_repo != null) repositories[owner] = issue.resolved_repo;
    })
  );
  return {
    text: formatCheckResults(results, brief, repositories),
    details: {
      issue,
      checks: checks.length,
      results: checks.reduce((total, check) => total + (check.results ?? []).length, 0),
    },
  };
}
