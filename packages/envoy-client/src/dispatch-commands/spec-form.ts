import type {
  ApprovalRequestResponse,
  Artifact,
  SpecFormFinding,
  SpecFormRead,
} from "@legion/contracts";
import type { DispatchClient } from "../dispatch-http";
import {
  type CommandContext,
  type DispatchToolResult,
  optionalNumber,
  optionalString,
} from "./context";
import { dispatchAnswered, refusalWithCode } from "./failures";

// `dispatch spec-form` and the line `dispatch request-approval` prints on the form of The Spec.
// The server judges the form (`specFormOf` in packages/envoy/internal/dispatch/api/spec_form.go);
// this module only reads its answers and prints them. Each sentence here is fixed copy that
// ASD-STE100's program (`ste-lint`) reports no hard finding on.

/** How many texts of each kind Dispatch read, keyed by `specform.Kind`. */
type TextCounts = Readonly<Record<string, number>> | null | undefined;

/** `count` and its noun, in the singular for one. */
function counted(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? "" : "s"}`;
}

/** "2 statements, 0 claims, 1 purpose and 0 definitions": the kinds of text a line names. */
function textCounts(texts: TextCounts): string {
  const of = (kind: string): number => texts?.[kind] ?? 0;
  return `${counted(of("statement"), "statement")}, ${counted(of("claim"), "claim")}, ${counted(of("purpose"), "purpose")} and ${counted(of("definition"), "definition")}`;
}

/**
 * What Dispatch read of a form no text breaks: the count of each kind of text against the terms,
 * or, when it read no text of any kind, that it decided nothing about the form of `name`.
 */
function readLine(texts: TextCounts, terms: number, name: string, version: number): string {
  const read = Object.values(texts ?? {}).reduce((sum, count) => sum + count, 0);
  return read === 0
    ? `Dispatch found no text of The Spec in ${name} version ${version}, so it decided nothing about its form.`
    : `Dispatch read the form of ${textCounts(texts)} against ${counted(terms, "term")}: no text breaks it.`;
}

/**
 * The line of each approved document under the root that Dispatch could not read, which the
 * server writes (`specform.UnreadLine`): no rule compared its texts, and the line says the way out.
 */
function unreadLines(unread: readonly { readonly line: string }[] | null | undefined): string[] {
  return (unread ?? []).map(({ line }) => line);
}

/**
 * The lines `dispatch request-approval` adds from its answer's `spec_form`, which Dispatch sends
 * only when a root of The Spec covers the document: what it read of every document the request
 * handed the person, and each approved document it could not read. A request Dispatch refused
 * for the form never reaches here (409 SPEC_FORM).
 */
export function approvalFormLines(result: ApprovalRequestResponse, name: string): string[] {
  const form = result.spec_form;
  return form === undefined
    ? []
    : [readLine(form.texts, form.terms, name, result.version), ...unreadLines(form.unread)];
}

/** Why Dispatch refuses no approval request of a document out of scope, by its `scope`. */
function scopeLine(form: SpecFormRead): string {
  switch (form.scope) {
    case "no_root":
      return "This server names no root of The Spec, so Dispatch refuses no approval request for its form.";
    case "outside":
      return `${form.name} is not under a root of The Spec, so Dispatch refuses no approval request of it for its form.`;
    case "not_spec":
      return `${form.name} is not the spec or the Verification document of its issue, so Dispatch refuses no approval request of it for its form.`;
    case "root":
      return `${form.name} belongs to a root of The Spec, so Dispatch refuses no approval request of it for its form.`;
    default:
      return `${form.name} has the scope ${form.scope}, so Dispatch refuses no approval request of it for its form.`;
  }
}

/**
 * How many texts hold one of `findings`, which the read route answers whole. The findings of one
 * text name its node, its kind, its place and its excerpt, as in the head of the server's refusal
 * (`textsOf` in packages/envoy/internal/dispatch/specform/message.go).
 */
function textsBreaking(findings: readonly SpecFormFinding[]): number {
  return new Set(
    findings.map(({ excerpt, kind, node, where }) => JSON.stringify([where, kind, node, excerpt]))
  ).size;
}

/**
 * One finding as the server's refusal writes it (`Finding.Said` in
 * packages/envoy/internal/dispatch/specform/specform.go): its detail, the rule's words at its
 * place, then the ways out it gives, which are no part of the finding.
 */
function findingSaid({ detail, ways }: SpecFormFinding): string {
  return ways === "" ? detail : `${detail}. ${ways}`;
}

/**
 * The form of one document as Dispatch read it. A document an approval request is judged on
 * (`in_scope`) gets the line that says Dispatch refuses its approval and each finding, or the
 * line of what Dispatch read. One out of scope first says why Dispatch refuses nothing for it,
 * and still lists its findings, which are then for the reader alone. Each approved document under
 * the root that Dispatch could not read has a line after them.
 */
function documentLines(form: SpecFormRead): string[] {
  const findings = form.findings ?? [];
  const listed = findings.map(
    (finding) => `- ${finding.kind} ${finding.node}: ${findingSaid(finding)}.`
  );
  const read = readLine(form.texts, (form.terms ?? []).length, form.name, form.version);
  const unread = unreadLines(form.unread);
  if (!form.in_scope) {
    return findings.length === 0
      ? [scopeLine(form), read, ...unread]
      : [scopeLine(form), "The findings below are for you to read.", ...listed, ...unread];
  }
  if (findings.length === 0) return [read, ...unread];
  const texts = textsBreaking(findings);
  const breaks = texts === 1 ? "1 text breaks" : `${texts} texts break`;
  return [
    `Dispatch refuses an approval request of ${form.name} version ${form.version}, because ${breaks} the form of The Spec.`,
    ...listed,
    ...unread,
  ];
}

/** What a listing prints for a document that has no settled version yet (409 NO_VERSION). */
function noVersionLine(name: string): string {
  return `${name} has no version yet, so Dispatch read no text in it.`;
}

/**
 * Whether a document plays the spec or the Verification document of its issue, the two an
 * approval request of an issue under a root is judged on. It is the server's own rule
 * (`isSpecDocument`, and `documentPlays(artifact, model.ArtifactRoleVerification)` with its slug
 * fallback for a row whose role is still empty, in packages/envoy/internal/dispatch/api/
 * artifacts.go), read off the listed artifact.
 */
function playsSpecOrVerification(artifact: Artifact): boolean {
  if (artifact.kind !== "doc" || typeof artifact.issue_key !== "string") return false;
  return (
    artifact.primary ||
    artifact.role === "verification" ||
    ((artifact.role ?? "") === "" && artifact.slug === "verification-md")
  );
}

/**
 * The form of the latest settled version of `artifact`, or undefined while it has none, so a
 * listing names that document and goes on to the next one.
 */
async function latestForm(
  client: DispatchClient,
  artifact: Artifact
): Promise<SpecFormRead | undefined> {
  try {
    return await client.specForm(artifact.id);
  } catch (error) {
    if (dispatchAnswered(error, 409) && error.code === "NO_VERSION") return undefined;
    throw refusalWithCode(error);
  }
}

/** The spec and the Verification document among `artifacts`, each read at its latest settled version. */
async function coveredForms(
  client: DispatchClient,
  artifacts: readonly Artifact[]
): Promise<{ readonly artifact: Artifact; readonly form: SpecFormRead | undefined }[]> {
  return Promise.all(
    artifacts
      .filter(playsSpecOrVerification)
      .map(async (artifact) => ({ artifact, form: await latestForm(client, artifact) }))
  );
}

/**
 * `dispatch spec-form --issue KEY` with no `--artifact`: each document the issue's form plays,
 * from the issue's own read. The documents route lists no document for a key that names no issue
 * and refuses nothing, so the issue's read is what refuses such a key, as `dispatch read` does.
 */
async function issueForms(client: DispatchClient, issue: string): Promise<DispatchToolResult> {
  const read = await coveredForms(client, (await client.getIssue(issue)).artifacts ?? []);
  const lines = read.flatMap(({ artifact, form }) =>
    form === undefined
      ? [noVersionLine(artifact.name)]
      : [`${form.name} version ${form.version}:`, ...documentLines(form)]
  );
  return {
    text:
      read.length === 0
        ? `${issue} has no spec and no Verification document, so Dispatch read no document.`
        : lines.join("\n"),
    details: {
      issue,
      documents: read.flatMap(({ form }) =>
        form === undefined
          ? []
          : [
              {
                artifact: form.artifact_id,
                version: form.version,
                scope: form.scope,
                refuses_approval: form.refuses_approval,
                findings_total: form.findings_total,
              },
            ]
      ),
    },
  };
}

/**
 * `dispatch spec-form --root KEY`: every strict descendant of KEY, read with the issue list one
 * parent at a time (`GET /api/v1/issues?parent=`, which lists the children of every project), in
 * tree order and indented by depth. Each issue's line carries the counts and the findings of its
 * documents; each document the root would cover (the issue's spec and Verification document) has
 * its own line with its version and its scope, which says whether the gate judges it today.
 */
async function rootForms(client: DispatchClient, root: string): Promise<DispatchToolResult> {
  const rootKey = (await client.getIssue(root)).key;
  const lines: string[] = [];
  const seen = new Set([rootKey]);
  let documents = 0;
  let findings = 0;
  const walk = async (parent: string, depth: number): Promise<void> => {
    const indent = "  ".repeat(depth);
    for (const child of await client.listAllIssues({ parent })) {
      if (seen.has(child.key)) continue;
      seen.add(child.key);
      const read = await coveredForms(client, await client.listIssueArtifacts(child.key));
      const totals: Record<string, number> = {};
      let issueFindings = 0;
      const documentRows = read.map(({ artifact, form }) => {
        if (form === undefined) return `${indent}  ${noVersionLine(artifact.name)}`;
        documents += 1;
        issueFindings += form.findings_total;
        for (const [kind, count] of Object.entries(form.texts ?? {})) {
          totals[kind] = (totals[kind] ?? 0) + count;
        }
        return `${indent}  ${form.name} version ${form.version} (scope ${form.scope}): ${textCounts(form.texts)}, with ${counted(form.findings_total, "finding")}.`;
      });
      findings += issueFindings;
      const issueLine = `${indent}${child.key} [${child.status}] ${child.title}`;
      lines.push(
        read.length === 0
          ? `${issueLine}: no spec and no Verification document.`
          : `${issueLine}: ${textCounts(totals)}, with ${counted(issueFindings, "finding")}.`,
        ...documentRows
      );
      await walk(child.key, depth + 1);
    }
  };
  await walk(rootKey, 0);
  const issues = seen.size - 1;
  return {
    text:
      issues === 0
        ? `Dispatch found no issue under ${rootKey}, so it read no document.`
        : [
            ...lines,
            `Dispatch read ${counted(issues, "issue")} and ${counted(documents, "document")} under ${rootKey}.`,
          ].join("\n"),
    details: { root: rootKey, issues, documents, findings },
  };
}

/**
 * `dispatch spec-form`: the form of The Spec in one document, as an approval request would judge
 * it, with nothing written. `--artifact` (or a document `--ref`) names the document and
 * `--version` its version, the latest settled one by default. An issue with no `--artifact` reads
 * its spec and its Verification document, and `--root` walks every issue under that root.
 */
export async function specFormCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, ownerArguments, documentOwner, resolveDocument } = context;
  const root = optionalString(args, "root");
  if (root !== undefined) return rootForms(client, root);
  const artifactReference =
    optionalString(args, "artifact") ??
    (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
      ? ownerArguments.ref.id
      : undefined);
  const owner = documentOwner();
  if (artifactReference === undefined && owner.kind === "issue") {
    return issueForms(client, owner.issue);
  }
  const resolved = await resolveDocument(owner, artifactReference);
  const version = optionalNumber(args, "version") ?? ownerArguments.ref?.version;
  let form: SpecFormRead;
  try {
    form = await client.specForm(resolved.artifact.id, version);
  } catch (error) {
    // NO_VERSION, VERSION_NOT_FOUND and NOT_DOCUMENT are Dispatch's own refusals, and the code is
    // what a caller acts on.
    throw refusalWithCode(error);
  }
  return {
    text: documentLines(form).join("\n"),
    details: {
      ...(resolved.owner.kind === "project"
        ? { project: resolved.owner.project }
        : { issue: resolved.issue?.key }),
      artifact: form.artifact_id,
      version: form.version,
      scope: form.scope,
      refuses_approval: form.refuses_approval,
      findings_total: form.findings_total,
    },
  };
}
