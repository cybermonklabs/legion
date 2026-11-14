import type { DocumentWriteAdvice, WriteAdvice } from "@legion/contracts";
import { messageFor } from "../errors";

const triageAdviceShown = new Set<string>();

export function resetAdviceMemory(): void {
  triageAdviceShown.clear();
}

/** The triage keys this process has shown, so a short-lived host can carry them across calls. */
export function adviceMemory(): Set<string> {
  return triageAdviceShown;
}

/** The call that hands a moved approval request back to the person, built in one place for the
 *  two lines that print it - a document write's advice and a reply on such a request - so the
 *  two can never drift. `artifactID` and `primary` describe the request's FIRST document, the
 *  one whose approval route the call reopens, and `summaryVersion` is the version the request
 *  asks about, which is what the `--summary` placeholder names as the floor of what changed. */
export function handBackApprovalCommand(request: {
  owner: { kind: "issue" | "project"; key: string };
  artifactID: string;
  primary: boolean;
  verification: boolean;
  summaryVersion: number;
}): string {
  const owner =
    request.owner.kind === "project"
      ? `--project ${request.owner.key} --artifact ${request.artifactID}`
      : request.primary
        ? `--issue ${request.owner.key}`
        : `--issue ${request.owner.key} --artifact ${request.artifactID}`;
  // `--include-verification` asks for the issue's spec and its Verification document in one
  // card, and `requestArtifactApproval` (packages/envoy/internal/dispatch/api/reviews.go)
  // refuses it with 409 NOT_SPEC_DOCUMENT on every other document, a project document included.
  // So the flag rides only a request whose first document is its issue's primary one.
  const verification =
    request.owner.kind === "issue" && request.primary && request.verification
      ? " --include-verification"
      : "";
  return (
    `dispatch request-approval ${owner}${verification} ` +
    `--summary '<what changed since version ${request.summaryVersion}>'`
  );
}

/** Each document that stands above the version its approval request asks about, in the order the
 *  request names them: `spec.md asked about version 9 and is now version 10`, joined with `; `
 *  so the clauses stay one sentence. */
export function movedApprovalClauses(
  documents: readonly { name: string; requested_version: number; version: number }[]
): string {
  return documents
    .map(
      (document) =>
        `${document.name} asked about version ${document.requested_version} and is now version ${document.version}`
    )
    .join("; ");
}

/** The documents of an approval request a version has moved: the ones standing above the version
 *  the request asks about. It is this side's one spelling of `requested_version < version`, the
 *  rule the server spells as `docs.ApprovalAwaitsHandBack` and the SQL `approvalMoved`, read by
 *  the two lines that name a moved request - a document write's advice and a reply on such a
 *  request - so the two can never drift. */
export function movedApprovalDocuments<
  Document extends { requested_version: number; version: number },
>(documents: readonly Document[]): Document[] {
  return documents.filter((document) => document.requested_version < document.version);
}

/**
 * What a question says about the review its issue's specification already carries
 * (`WriteAdvice.standing_review`): who reviewed, when, whether they approved or asked for
 * changes, their reason, and the one thing the sender can do about it. The sender is asking
 * about text someone has already read and judged, so the answer they need is what changed since
 * that review. Nothing is printed when the advice carries no review.
 *
 * A carried review is Dispatch's own bookkeeping of an earlier approval, so its line says so and
 * prints no reason: the stored reason is Dispatch's sentence, its time is when Dispatch recorded
 * the carry, and the version it names is the one the carry's reason names, which is the version
 * the person read. `carried_from_version` is set on a carried review and on no other, so it is
 * what tells the two apart.
 */
export function standingReviewLines(advice: WriteAdvice | undefined): string[] {
  const review = advice?.standing_review;
  if (review === undefined) return [];
  const carriedFrom = review.carried_from_version;
  if (carriedFrom !== undefined) {
    return [
      `${review.by}'s approval of this issue's spec was carried forward from the approval of version ${carriedFrom}, and Dispatch recorded that carry on ${review.at}.`,
      "Say what changed since that review in a reply on this ask.",
    ];
  }
  const verb = review.state === "approved" ? "approved" : "asked for changes on";
  const opening = `${review.by} ${verb} this issue's spec at version ${review.version} on ${review.at}`;
  // The reason is a person's own words, up to 500 characters of them, and it goes into what an
  // agent reads: quoted by JSON, so a quotation mark or a line feed inside it cannot end the
  // line early or add one the reviewer did not write.
  const reason = review.reason ?? "";
  return [
    reason === "" ? `${opening}, and gave no reason.` : `${opening}: ${JSON.stringify(reason)}`,
    "Say what changed since that review in a reply on this ask.",
  ];
}

export function renderAdvice(
  tool: string,
  key: string,
  advice: WriteAdvice | DocumentWriteAdvice | undefined,
  opts: {
    isAskReply?: boolean;
    isPrimarySpec?: boolean;
    setsStatus?: boolean;
    replyToOwnAsk?: boolean;
  }
): string[] {
  if (advice === undefined) return [];

  // A project document stands under no issue, so its write's advice carries the document half
  // alone (DocumentWriteAdvice): no status, no writes-since-human count and no open-ask list.
  // Every line below that reports the issue reads them through `issueAdvice`, which is that
  // half only when the server sent it.
  const issueAdvice: WriteAdvice | undefined = "issue_status" in advice ? advice : undefined;
  const hasIssueAdvice = issueAdvice !== undefined;
  const openAsks = issueAdvice?.your_open_asks ?? undefined;
  const writesSinceHuman = issueAdvice?.session_writes_since_human;
  const lines: string[] = [];
  const unparsed = advice.unparsed_openers;
  if (
    unparsed !== undefined &&
    unparsed.count > 0 &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    const quoted = (unparsed.examples ?? []).map((example) => JSON.stringify(example)).join(", ");
    const subject =
      unparsed.count === 1
        ? "1 typed-block opening in this document is text, not a block"
        : `${unparsed.count} typed-block openings in this document are text, not blocks`;
    lines.push(
      `${subject}: ${quoted}. An opening like \`:::ask{…}\` makes a block only as a line of its own, so as text it asks nobody. Mentioning the syntax on purpose? Put it in code. See the \`dispatch-writing-specs\` skill, "Questions".`
    );
  }
  if (
    advice.decision_blocks === 0 &&
    opts.isPrimarySpec === true &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    lines.push(
      "This spec holds no ask blocks. A person does not watch your chat. They see that you need them only through an open ask or a spec that waits for their approval. If the design needs a person's choice, add an ask block. If it does not, request approval with `dispatch request-approval`."
    );
  }

  // The advice is dropped, not reworded, on a Legion-managed issue: the Legion daemon moves such
  // an issue out of triage itself when it releases it, so asking the session to move the status
  // asks it to do the one thing the daemon undoes. The memo is marked inside the block, so a key
  // whose line was never printed can still be told later if it leaves Legion's hands.
  if (
    issueAdvice?.issue_status === "triage" &&
    issueAdvice.legion_managed_status !== true &&
    tool !== "dispatch_issue" &&
    !(tool === "dispatch_issue_update" && opts.setsStatus === true) &&
    !triageAdviceShown.has(key)
  ) {
    triageAdviceShown.add(key);
    lines.push(
      `${key} is still in triage — nobody can see its development status. See the \`dispatch\` skill, "Issue status is yours to move".`
    );
  }

  if (
    hasIssueAdvice &&
    openAsks !== undefined &&
    openAsks.length > 0 &&
    (tool === "dispatch_message" ||
      tool === "dispatch_doc_edit" ||
      tool === "dispatch_issue_update" ||
      (tool === "dispatch_comment" && opts.replyToOwnAsk !== true))
  ) {
    const askCount = Math.min(openAsks.length, 2);
    for (let index = 0; index < askCount; index += 1) {
      const ask = openAsks[index];
      if (ask === undefined) break;
      lines.push(
        `You still have an open ask on ${key}: "${ask.question.slice(0, 80)}" (${ask.id}). Still needed? See the \`dispatch\` skill, "Close what you opened".`
      );
    }
  }

  // The version this write stored took an open approval request of the document back from the
  // person it was asked of: it waits on this agent now and nothing tells that person, so the
  // write that moved it is where the request and the call that hands it back are named. The
  // report carries everything the line says, so this reads no ask and makes no request, which is
  // what keeps `renderAdvice` synchronous. It comes before the superseded-approval block so that
  // block's line stays the last advice line a document write prints.
  const movedApproval = advice.moved_approval_request;
  if (
    movedApproval !== undefined &&
    (tool === "dispatch_doc_edit" || tool === "dispatch_artifact")
  ) {
    // No line when no document of the report moved, since a document still at the version the
    // request asks about is one this write left alone. A report holding no document at all
    // (`documents` is null on a request this client cannot describe) moved none of them either.
    const documents = movedApproval.documents ?? [];
    const moved = movedApprovalDocuments(documents);
    // The owner part of the call comes from the request's own place. A report naming neither an
    // issue nor a project could name no owner, and an invalid command is worse than no line.
    const movedOwner =
      movedApproval.issue !== undefined && movedApproval.issue !== ""
        ? ({ kind: "issue", key: movedApproval.issue } as const)
        : movedApproval.project !== undefined && movedApproval.project !== ""
          ? ({ kind: "project", key: movedApproval.project } as const)
          : undefined;
    // The call reopens the request's first document, and the `--summary` floor is the version
    // the first document that moved was handed over at. `--include-verification` is what asked
    // for a second document, so it is read from the documents beside the first.
    const first = documents[0];
    const firstMoved = moved[0];
    // Both reads are guarded because the strictest tsconfig of this repository,
    // packages/claude-envoy's, compiles these sources under `noUncheckedIndexedAccess`, which
    // types an indexed read as possibly `undefined`. The `undefined` arm cannot be reached:
    // `moved` is `documents` filtered, so a `moved` that is not empty leaves both arrays holding
    // a first element. A first element missing all the same is one more reason to print no line.
    if (
      moved.length > 0 &&
      movedOwner !== undefined &&
      first !== undefined &&
      firstMoved !== undefined
    ) {
      const command = handBackApprovalCommand({
        owner: movedOwner,
        artifactID: first.artifact_id,
        primary: first.primary,
        verification: documents.slice(1).some((document) => document.role === "verification"),
        summaryVersion: firstMoved.requested_version,
      });
      lines.push(
        `This write moved the open approval request (ask ${movedApproval.ask_id}): it waits on ` +
          "an agent now, not on the person, and nothing tells the person. " +
          `${movedApprovalClauses(moved)}. Once the human has agreed to every point in it, hand ` +
          `the same request back: ${command}. Not yours to hand back? Tell the issue's architect.`
      );
    }
  }

  // The gate line is last for the document tools, for the reason the cadence line is last for
  // the others: behind `| tail -1` it is the only line the agent sees, and it is the one line of
  // a document write that someone else has to act on. It describes this one write - the version
  // it stored, the approval that version replaced and how that approval comes back - rather than a
  // rule about editing a spec, because the server sends it only on the write that superseded the
  // approval.
  //
  // The gate clause comes from `design_gates`, the server's record of which issues' design gates
  // name the document just written: it names those issues, and it is silent for every other
  // document. Absent (an older Dispatch keeps no such record), empty, and anything that is not a
  // list of gates all print no clause, which is the safe direction - a reader told nothing asks
  // the tree's architect, a reader told a gate that is not there stops working for no reason.
  const superseded = advice.superseded_approval;
  if (superseded !== undefined && (tool === "dispatch_doc_edit" || tool === "dispatch_artifact")) {
    const gates = Array.isArray(superseded.design_gates) ? superseded.design_gates : [];
    const parts = [
      `Stored version ${superseded.version}, superseding version ${superseded.approved_version}, which ${superseded.approved_by} approved. That approval is stale until someone approves version ${superseded.version} or a later write stores the text of version ${superseded.approved_version} again exactly, with no decision block in it decided since.`,
    ];
    if (gates.length > 0) {
      const named = gates.map((gate) => `${gate.issue}, step ${gate.step}`).join("; ");
      parts.push(
        `A Legion design gate records this document, so every READY in ${gates.length === 1 ? "its" : "each"} tree is refused until that approval comes back: ${named}.`
      );
    }
    parts.push(
      "Meant to change the approved design? Request approval with `dispatch request-approval`. Meant a correction beside it? Tell the tree's architect, and keep the next one in a comment or in the plan."
    );
    lines.push(parts.join(" "));
  }

  // The cadence line comes last and opens by saying the write went through: behind `| tail -1`
  // it is the only line an agent sees, and "Stop posting" alone reads as a refusal.
  if (
    hasIssueAdvice &&
    writesSinceHuman !== undefined &&
    writesSinceHuman >= 3 &&
    (tool === "dispatch_message" ||
      tool === "dispatch_ask" ||
      (tool === "dispatch_comment" && opts.isAskReply !== true))
  ) {
    const middle =
      writesSinceHuman >= 6
        ? "Stop posting here until a human replies. Their reply ends this: post then what you held back."
        : "Progress ledger or scratchpad? If so, stop.";
    lines.push(
      `Posted. You've sent ${writesSinceHuman} messages on ${key} with no human response. ${middle} See the \`dispatch\` skill, "Structure over stream".`
    );
  }

  return lines;
}

/** The advice lines a write prints after its related-item check, or one line saying why they
 *  could not be shown: the write already succeeded, and an exception here must not cost the
 *  reader the line naming what was written or what the check found. */
export async function otherAdviceLines(
  render: () => string[] | Promise<string[]>
): Promise<string[]> {
  try {
    return await render();
  } catch (error) {
    return [`Other advice could not be shown: ${messageFor(error)}`];
  }
}
