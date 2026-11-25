/**
 * The row of each keyed write of `DispatchClient` (each POST and PATCH): what the method's callers
 * read of its 2xx answer, which `#writePayload` checks before any caller reads it. Dispatch answers a
 * write's 2xx only after the write committed, so a 2xx that names the call's key back and is not
 * this row is a landing whose answer was lost (`ANSWER_LOST`), never a failure of the call. A 2xx
 * that names no key back is no answer of the call's, whatever it holds.
 *
 * A row names only members a caller reads, and each in the form the route answers it in the
 * OpenAPI document the server generates (`packages/contracts/schemas/dispatch-openapi.json`):
 * required where the route always answers it, nullable where the route may answer null. A member a
 * caller reads only behind a guard of its own (`??`, `=== undefined`, an explicit refusal) is
 * named as optional: its absence is what that caller already handles, and a row stricter than
 * Dispatch's true answer would turn a good answer into `ANSWER_LOST`. A row keeps every member it
 * does not name, at every depth (`z.looseObject`), and the caller reads the answer as it came.
 * `__tests__/dispatch-write-rows.test.ts` drives every method against the least and the most
 * answer its route declares, and holds every row to that rule.
 *
 * Where `@legion/contracts` declares a member in the form the route answers, the row takes that
 * declaration (`issueFields`, `askFields`, `commentFields`, `messageFields`, `artifactFields`,
 * `versionFields`, `ArchitectureSourceSchema`, `ExternalLinkSchema`, `InboundActorSchema`).
 * Those declarations are the bus payloads' rows, and where a payload's form is not the route's (a
 * list the route answers as `null` when empty, a closed set of words the route answers as any
 * string), the row writes the route's form. No `@legion/contracts` schema holds a whole write's
 * answer: the answer types are generated from the OpenAPI document as TypeScript types alone.
 */
import {
  ArchitectureSourceSchema,
  artifactFields,
  askFields,
  commentFields,
  ExternalLinkSchema,
  InboundActorSchema,
  issueFields,
  messageFields,
  versionFields,
} from "@legion/contracts";
import { z } from "zod";

/** The components of an issue as `architectureGuidance` and `componentsChange` read them. The
 *  route answers `ids` as `null` when empty, where the payload's `IssueComponentsSchema` has a
 *  list, and `mode` as any string. */
const components = z.looseObject({
  mode: z.string(),
  ids: z.array(z.string()).nullish(),
  reason: z.string().nullish(),
});

/** A write's `advice`, as `renderAdvice` reads it (`dispatch-commands/advice.ts`) where no `try`
 *  of the caller's own catches what it throws; the route answers `WriteAdvice` on an issue's
 *  write and `DocumentWriteAdvice` on a project document's, which carries no `issue_status`,
 *  `session_writes_since_human` or `your_open_asks`. `decision_blocks` is read only as
 *  `=== 0`, which an absent count leaves unprinted. Its `suggestions`, `declined_asks` and
 *  `decision_block_checks` are read inside a `try` that prints a line of its own for any shape
 *  (`related-check.ts`), so the row names none of them. */
const advice = z
  .looseObject({
    decision_blocks: z.number().int().optional(),
    issue_status: z.string().optional(),
    legion_managed_status: z.boolean().optional(),
    session_writes_since_human: z.number().int().optional(),
    your_open_asks: z
      .array(z.looseObject({ id: z.string(), question: z.string() }))
      .nullable()
      .optional(),
    unparsed_openers: z
      .looseObject({ count: z.number().int(), examples: z.array(z.string()).nullable() })
      .optional(),
    moved_approval_request: z
      .looseObject({
        ask_id: z.string(),
        issue: z.string().optional(),
        project: z.string().optional(),
        documents: z
          .array(
            z.looseObject({
              artifact_id: z.string(),
              name: z.string(),
              primary: z.boolean(),
              role: z.string().optional(),
              requested_version: z.number().int(),
              version: z.number().int(),
            })
          )
          .nullable(),
      })
      .optional(),
    superseded_approval: z
      .looseObject({
        version: z.number().int(),
        approved_version: z.number().int(),
        approved_by: z.string(),
        design_gates: z.array(z.looseObject({ issue: z.string(), step: z.string() })).optional(),
      })
      .optional(),
  })
  .optional();

/** The advice of a write whose caller renders it inside `otherAdviceLines`, which prints one line
 *  of its own for advice it cannot show (`issueCommand`, `askCommand`, `docEditCommand`): that
 *  `try` is the caller's guard, so the row names the member and none of its members. */
const caughtAdvice = z.unknown().optional();

/** An ask as `askCommand`, `editAskCommand` and `askOwnerDetails` read it. */
const ask = {
  id: askFields.id,
  issue_key: askFields.issue_key,
  artifact_id: askFields.artifact_id,
};

/** An ask's write: `askCommand` reads the question, the urgency, whom it waits on and the advice
 *  too. The route answers `urgency` as any string, where the payload has the set of urgencies. */
const askWrite = z.looseObject({
  ...ask,
  question: askFields.question,
  urgency: z.string(),
  waiting_on_person: z.string().optional(),
  advice: caughtAdvice,
});

/** A comment's write as `commentCommand` reads it on a document of a project, where the details
 *  name the document rather than an issue. */
const documentComment = {
  id: commentFields.id,
  ask_waiting_on: z.string().optional(),
  advice,
};

/** A suggestion as `suggestCommand` reads it: its id alone. */
const suggestion = z.looseObject({ id: commentFields.id });

/** One architecture source as `architectureSyncLines` reads it; the payload's declaration is the
 *  route's form. */
const {
  repo,
  branch,
  last_error,
  last_commit,
  last_sync_at,
  components: sourceComponents,
} = ArchitectureSourceSchema.unwrap().shape;
const architectureSource = z.looseObject({
  repo,
  branch,
  last_error,
  last_commit,
  last_sync_at,
  components: sourceComponents,
});

/** An upload's stored document and version, as `textWithPictures` reads them for a picture. */
const pictureUpload = {
  artifact: z.looseObject({ slug: artifactFields.slug }),
  version: z.looseObject({ number: versionFields.number }),
};

/** An upload's stored document as `artifactCommand` reads it, the picture's `slug` among its
 *  members. `primary` is read as `primary || name === "spec.md"`, which an absent flag leaves to
 *  the name. The route answers `role` as any string, where the payload has the set of roles. */
const uploadedDocument = {
  id: artifactFields.id,
  slug: artifactFields.slug,
  name: artifactFields.name,
  primary: artifactFields.primary.optional(),
  role: z.string().optional(),
  issue_key: artifactFields.issue_key.optional(),
};

/** The person or session a review or a retraction names, as `actorText` reads it. */
const actor = InboundActorSchema;

/** The rows of the POSTs that store nothing, `markdownRoundTrip` and `related`, side by side in
 *  `WRITE_ROWS`: Dispatch's parse and render of a text (`POST /api/v1/markdown/round-trip`: its
 *  handler, `postMarkdownRoundTrip`, parses and renders the text and answers it, and calls no
 *  store) and the related-item check (`POST /api/v1/projects/{key}/related`: its handler's
 *  `answerRelated` runs `relatedCheck` with `record` unset, so no related_checks record is
 *  written). */
const STORE_NOTHING = new WeakSet<z.ZodType>();

/** `row`, marked as the row of a route that stores nothing (`rowStores`). */
function storesNothing<Row extends z.ZodType>(row: Row): Row {
  STORE_NOTHING.add(row);
  return row;
}

/** Whether the route whose 2xx `row` reads stores what it is sent. A 2xx of one that stores
 *  nothing and gives no answer made nothing, so it is no landing (`DispatchClient.#writePayload`). */
export function rowStores(row: z.ZodType): boolean {
  return !STORE_NOTHING.has(row);
}

/** A priority an issue advances and a link it carries, in the payload's forms (`issueFields`,
 *  `ExternalLinkSchema`), whose items refuse a member they do not name: here each keeps it, as
 *  every row does. */
const advance = z.looseObject(issueFields.advances.unwrap().unwrap().element.unwrap().shape);
const externalLink = z.looseObject(ExternalLinkSchema.unwrap().shape);

/** An issue as `issueUpdateCommand` reads the answer of its PATCH (`WRITE_ROWS.updateIssue`). */
const updatedIssue = z.looseObject({
  key: issueFields.key,
  status: issueFields.status,
  title: issueFields.title.optional(),
  priority: issueFields.priority.optional(),
  assignee: issueFields.assignee.optional(),
  route: issueFields.route.optional(),
  parent: issueFields.parent.optional(),
  components: components.optional(),
  repo: issueFields.repo.optional(),
  resolved_repo: issueFields.resolved_repo.optional(),
  labels: issueFields.labels.nullish(),
  blocked_by: issueFields.blocked_by.nullish(),
  advances: z.array(advance).nullable().optional(),
  external_links: z.array(externalLink).nullish(),
  advice,
});

export const WRITE_ROWS = {
  /** `issueCommand` reads `project` on a created issue's check lines (`relatedCheckLines`, which
   *  prints it where no check was made), so the row names it. It reads `components` only inside
   *  `otherAdviceLines`, whose `try` prints a line of its own, so the row names none of it. */
  issue: z.looseObject({
    key: issueFields.key,
    title: issueFields.title,
    project: issueFields.project,
    assignee: issueFields.assignee.optional(),
    repo: issueFields.repo.optional(),
    advice: caughtAdvice,
  }),
  markdownRoundTrip: storesNothing(z.looseObject({ markdown: z.string() })),
  related: storesNothing(z.looseObject({ outcome: z.string() })),
  /** `issueUpdateCommand` reads the key and the status on every path, and each other member only
   *  where the call wrote it, so those are optional. */
  updateIssue: updatedIssue,
  /** The same write where the call wrote `components`: `componentsChange` reads them with no guard
   *  of its own, so this row requires them (`DispatchClient.updateIssue` picks it). */
  updateIssueComponents: updatedIssue.extend({ components }),
  claimIssue: z.looseObject({
    status: issueFields.status,
    claim: z.looseObject({ at: z.string() }).nullable(),
    legion_managed_status: z.boolean().optional(),
  }),
  /** `statusChangeCommand` reads each changed issue's key, status and parent, and the named
   *  issue's status and the URLs of its links, the links behind a `?? []` of its own. */
  changeIssueStatus: z.looseObject({
    changed: z
      .array(
        z.looseObject({
          key: issueFields.key,
          status: issueFields.status,
          parent: z.string().nullable(),
        })
      )
      .nullable(),
    issue: z.looseObject({
      status: issueFields.status,
      external_links: z.array(externalLink).nullish(),
    }),
  }),
  ask: askWrite,
  artifactAsk: askWrite,
  editAsk: z.looseObject({ ...ask, question: askFields.question }),
  resolveAsk: z.looseObject({
    ...ask,
    resolution: z.looseObject({ reason: z.string() }).optional(),
  }),
  createProject: z.looseObject({
    key: z.string(),
    name: z.string(),
    created_at: z.string(),
    default_assignee: z.string().optional(),
  }),
  /** Dispatch answers a project with no source 404 `SOURCE_NOT_FOUND`, so a 2xx holds one. */
  syncArchitectureSources: z.array(architectureSource),
  syncArchitectureSource: architectureSource,
  /** `requestApprovalCommand` reads `approval` where the answer holds no ask, which the route
   *  always answers, and its `by` through `?.`. */
  requestApproval: z.looseObject({
    version: z.number().int(),
    coverage: z.array(z.string()).nullish(),
    coverage_unread: z.boolean().optional(),
    ask: z
      .looseObject({
        ...ask,
        question: askFields.question,
        waiting_on_person: z.string().optional(),
      })
      .nullable(),
    approval: z.looseObject({ by: z.looseObject({ id: z.string() }).nullish() }),
  }),
  planApprove: z.looseObject({ version: z.number().int() }),
  reviewArtifact: z.looseObject({
    version: z.number().int(),
    state: z.string(),
    ask_id: z.string().nullable(),
    reason: z.string().nullable(),
    actor,
  }),
  recordResult: z.looseObject({
    line: z.string(),
    result: z.looseObject({
      id: z.string(),
      verdict: z.string(),
      role: z.string(),
      check_block_id: z.string(),
      check_issue_key: z.string(),
      commit_sha: z.string(),
    }),
  }),
  /** A validation-only result preview records nothing. */
  dryRunResult: storesNothing(z.looseObject({ line: z.string(), result: z.unknown() })),
  retractResult: z.looseObject({
    result_id: z.string(),
    reason: z.string(),
    created_at: z.string(),
    actor,
  }),
  recordSpecCheckResult: z.looseObject({
    id: z.string(),
    seq: z.number().int(),
    attestation: z.looseObject({
      predicate: z.looseObject({ check: z.string(), verdict: z.string() }),
    }),
  }),
  comment: z.looseObject({ ...documentComment, issue_key: commentFields.issue_key }),
  artifactComment: z.looseObject(documentComment),
  suggest: suggestion,
  artifactSuggest: suggestion,
  resolveComment: z.looseObject({
    id: commentFields.id,
    issue_key: commentFields.issue_key,
    artifact_id: commentFields.artifact_id,
  }),
  /** `editCommentCommand` reads what `resolveCommentCommand` reads of its answer. */
  editComment: z.looseObject({
    id: commentFields.id,
    issue_key: commentFields.issue_key,
    artifact_id: commentFields.artifact_id,
  }),
  message: z.looseObject({ id: messageFields.id, advice }),
  /** `editMessageCommand` reads the id and the issue, which it refuses when null. */
  editMessage: z.looseObject({ id: messageFields.id, issue_key: messageFields.issue_key }),
  agentMessage: z.looseObject({
    id: messageFields.id,
    deliveries: z
      .array(
        z.looseObject({ state: z.string(), error: z.string().nullish(), delivery: z.string() })
      )
      .nullish(),
  }),
  messageReply: z.looseObject({
    id: messageFields.id,
    body: messageFields.body,
    in_reply_to: messageFields.in_reply_to.optional(),
    duplicate: z.boolean().optional(),
  }),
  /** `postDeliveryReply` reads nothing of the answer: any object Dispatch answers is the row. */
  deliveryReply: z.looseObject({}),
  acceptMessageDelivery: z.looseObject({
    message_id: z.string(),
    body: z.string(),
    delivery: z.string(),
  }),
  docEdit: z.looseObject({
    applied: z.number().int(),
    version: z.looseObject({ number: versionFields.number }).nullable(),
    changed: z.boolean().optional(),
    unchanged_ops: z.array(z.number().int()).nullish(),
    lost_ops: z.array(z.number().int()).nullish(),
    written: z.string().optional(),
    token: z.string().optional(),
    advice: caughtAdvice,
  }),
  /** No caller of `nameArtifactVersion` reads its answer: any object Dispatch answers is the row. */
  nameArtifactVersion: z.looseObject({}),
  writeHandoff: z.looseObject({
    id: z.string(),
    issue_key: z.string(),
    phase: z.string(),
    version: z.number().int(),
  }),
  projectArtifact: z.looseObject({
    artifact: z.looseObject({ ...uploadedDocument, project: artifactFields.project }),
    version: pictureUpload.version,
    advice,
  }),
  agentArtifact: z.looseObject(pictureUpload),
  artifact: z.looseObject({
    artifact: z.looseObject(uploadedDocument),
    version: pictureUpload.version,
    advice,
  }),
} satisfies Record<string, z.ZodType>;
