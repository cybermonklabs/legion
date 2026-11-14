import type { ArtifactReview } from "@legion/contracts";
import { actorText } from "../actor-text";
import { commandLine } from "../dispatch-command";
import {
  type CommandContext,
  type DispatchToolResult,
  defined,
  numberArg,
  optionalString,
  stringArg,
} from "./context";
import { refusalWithCode } from "./failures";

/**
 * `dispatch review`: approve one version of an issue's document, or request changes on it, in the
 * name of the person the credential names, which is what the document header's Approve and
 * Request changes record. The document resolves as `dispatch doc-read` and
 * `dispatch request-approval` resolve theirs, the issue's spec by default.
 *
 * What it prints of the review is what Dispatch answered: the state, the version and the actor.
 * A review that answered an open approval request records the same verdict on every document the
 * request named, yet Dispatch answers with this document's review alone (`createArtifactReview`
 * in packages/envoy/internal/dispatch/api/reviews.go answers `reviews[artifact.ID]`), so the
 * output says that rather than naming reviews the answer does not carry.
 */
export async function reviewCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, documentOwner, resolveDocument } = context;
  const resolved = await resolveDocument(documentOwner(), optionalString(args, "artifact"));
  const { artifact } = resolved;
  let review: ArtifactReview;
  try {
    review = await client.reviewArtifact(artifact.id, {
      state: stringArg(args, "verdict") as "approved" | "changes_requested",
      ...defined("reason", optionalString(args, "reason")),
      version: numberArg(args, "version"),
    });
  } catch (error) {
    // The description names REVIEW_VERSION_MOVED, OAUTH_SCOPE_REQUIRED and SIGN_IN_REQUIRED, so
    // the code reaches the caller.
    throw refusalWithCode(error);
  }
  const issue = resolved.issue?.key;
  const answered =
    review.ask_id === null
      ? []
      : [
          `It also answered the open approval request (ask ${review.ask_id}), so Dispatch recorded the same verdict on every document that request named, each at the version the request named for it. Dispatch answered with the review of ${artifact.name} alone, so no other document's review is shown here; ${commandLine("dispatch_doc_read", { issue })} --artifact <document> prints the approval each of them now reads.`,
        ];
  return {
    text: [
      `Recorded the review of ${artifact.name} (document id ${artifact.id}): ${review.state} at version ${review.version}, by ${actorText(review.actor)}.`,
      ...(review.reason === null ? [] : [`Reason: ${JSON.stringify(review.reason)}`]),
      ...answered,
    ].join("\n"),
    details: {
      issue,
      artifact: artifact.id,
      version: review.version,
      state: review.state,
      ...(review.ask_id === null ? {} : { ask: review.ask_id }),
    },
  };
}
