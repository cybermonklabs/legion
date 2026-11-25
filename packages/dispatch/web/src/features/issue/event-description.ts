import type {
  BlockInvalidEventPayload,
  CommentDeliveryEventPayload,
  MessageDeliveryEventPayload,
} from "@legion/contracts";
import { DELETED_POST_DELIVERY_ERROR } from "@legion/contracts/dispatch-api-constants";
import type { DispatchEvent } from "../../api/types";
import { capabilityLabel } from "../conversation/delivery";
import { type AgentPart, askResolutionParts } from "../refs/AgentName";
import { type DeletablePost, postDeletion } from "../refs/post-marks";

/** An event's line in the Pinned tab, as `AgentPart`s: an agent it names keeps its identity
 *  until `EventBody` draws it through `AgentName`. */
export function eventDescription(event: DispatchEvent): AgentPart[] {
  const description = describe(event);
  return typeof description === "string" ? [description] : description;
}

/**
 * The line a deleted post's event shows in place of its body. The read of the event takes
 * `body` out of the payload and stamps `deleted_at` and `deleted_by` in its place
 * (`api/events.go`), so there is no text here to print and the mark is what a reader meets.
 * `undefined` for a post nobody deleted, which prints its body as it always has.
 */
function deletedPost(payload: DeletablePost, kind: string): AgentPart[] | undefined {
  const deleted = postDeletion(payload);
  if (deleted === null) {
    return undefined;
  }
  return deleted.by === null ? [`${kind} deleted`] : [`${kind} deleted by `, { actor: deleted.by }];
}

/** A `block.invalid`'s line, in the Pinned tab and the Activity tab alike: a check block whose
 *  `ci_check` settlement refused, or an ask block it marked malformed. */
export function blockInvalidDescription(payload: BlockInvalidEventPayload): string {
  return payload.block_type === "check"
    ? `refused check ${payload.block_id}: ${payload.reason}`
    : `marked decision ${payload.block_id} malformed: ${payload.reason}`;
}

/**
 * A delivery receipt's line, a message's and a comment mention's alike. A queued attempt reads as
 * queued, a queued repeat included: the listener holds it for a session that is not running. Any
 * other duplicate attempt reached the listener and the stream stored nothing new; its state is
 * still `sent`, so reporting the state alone would read as a fresh delivery. A failure carrying
 * the delete's words is the receipt the post's delete appended for a send it ended, which says
 * nothing of whether that send reached the session, so it says the delete ended the send, as the
 * Activity tab's line does (`activityDescription`), and never that the send failed.
 */
function deliveryReceipt(
  kind: "Comment" | "Message",
  receipt: CommentDeliveryEventPayload | MessageDeliveryEventPayload
): string {
  const mode = capabilityLabel(receipt.delivery);
  if (receipt.state !== "queued" && receipt.duplicate === true) {
    return `${kind} already delivered: ${mode}`;
  }
  if (receipt.state === "failed" && receipt.error === DELETED_POST_DELIVERY_ERROR) {
    return `${kind} send ended by the post's delete: ${mode}`;
  }
  return `${kind} ${receipt.state}: ${mode}`;
}

function describe(event: DispatchEvent): string | AgentPart[] {
  switch (event.type) {
    case "project.created":
      return `Project created: ${event.payload.name}`;
    case "project.updated":
      return `Project updated: ${event.payload.name}`;
    // No build writes this event now: a project's default repository is on the project. An event
    // retained from an earlier build keeps its wording.
    case "settings.repo_project.updated":
      return `${event.payload.deleted ? "Repository mapping removed" : "Repository mapping updated"}: ${event.payload.mapping.repo}`;
    // No write clears a default now: a project always names somebody. An event retained from
    // before that rule can record a clear, and keeps its wording.
    case "settings.project_default_assignee.updated":
      return event.payload.assignee === null
        ? `Default assignee cleared: ${event.payload.project}`
        : `Default assignee updated: ${event.payload.project} → ${event.payload.assignee}`;
    case "settings.architecture_source.updated":
      return `${event.payload.deleted ? "Architecture source removed" : "Architecture source updated"}: ${event.payload.source.repo}`;
    // An event retained from before a project had several sources names no repository and
    // keeps its original wording.
    case "architecture.synced": {
      const count = `${event.payload.components} component${event.payload.components === 1 ? "" : "s"} at ${event.payload.commit.slice(0, 12)}`;
      return event.payload.repo === undefined
        ? `Architecture synced: ${count}`
        : `Architecture synced from ${event.payload.repo}: ${count}`;
    }
    case "architecture.sync_failed":
      return event.payload.repo === undefined
        ? `Architecture sync failed: ${event.payload.error}`
        : `Architecture sync from ${event.payload.repo} failed: ${event.payload.error}`;
    case "user_state.updated":
      return "User state updated";
    case "user_agent_state.updated":
      return "Agent conversation state updated";
    case "user_thread_state.updated":
      return "Thread read";
    case "decision.kept":
      return "Kept a decision";
    case "priority.updated":
      return `Priority ${event.payload.change}: ${event.payload.id}`;
    case "project.access_changed":
      return `Access changed: ${event.payload.change.replace("_", " ")}`;
    case "ask.resolved":
      return askResolutionParts(event.payload.resolution);
    case "ask.answered":
      return event.payload.previous_answer === undefined
        ? `Ask answered: ${event.payload.question}`
        : `Answer changed: ${event.payload.question}`;
    case "ask.edited":
      return `Ask edited: ${event.payload.question}`;
    case "ask.handed_back":
      return `Ask handed back: ${event.payload.question}`;
    case "ask.asked_of":
      // Who the ask waits on now. The payload names no login where it was handed to the
      // assignee of its own issue: an ask with no `asked_of` waits on that assignee, or on its
      // project's default assignee while the issue holds nobody.
      return `Ask handed to ${event.payload.asked_of ?? "the issue's assignee"}: ${event.payload.question}`;
    case "comment.resolved":
      return "Comment resolved";
    case "comment.reopened":
      return "Comment reopened";
    case "comment.edited":
      return "Comment edited";
    case "comment.deleted":
      return deletedPost(event.payload, "Comment") ?? "Comment deleted";
    case "message.edited":
      return "Message edited";
    case "message.deleted":
      return deletedPost(event.payload, "Message") ?? "Message deleted";
    case "message.created":
    case "message.answered":
      // The body is absent only on a deleted post, which deletedPost answers first.
      return deletedPost(event.payload, "Message") ?? event.payload.body ?? "";
    case "message.delivery":
      return deliveryReceipt("Message", event.payload);
    case "message.accepted":
      return "Message taken as the session's own turn";
    case "ask.opened":
      return `Ask opened: ${event.payload.question}`;
    case "ask.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? "Ask lost its quote"
        : "Ask re-anchored after an edit";
    case "comment.created":
    case "comment.answered":
      // The body is absent only on a deleted post, which deletedPost answers first.
      return deletedPost(event.payload, "Comment") ?? event.payload.body ?? "";
    case "comment.anchor_refreshed":
      return event.payload.anchor?.orphaned
        ? "Comment lost its quote"
        : "Comment re-anchored after an edit";
    case "comment.delivery":
      return deliveryReceipt("Comment", event.payload);
    case "issue.created":
      return "Issue created";
    case "issue.updated":
      return "Issue updated";
    case "issue.closed":
      return "Issue closed";
    case "issue.claimed":
      return event.payload.previous_claim === undefined
        ? "Issue claimed"
        : ["Claim taken from ", { actor: event.payload.previous_claim.actor }];
    case "issue.released":
      return event.payload.reason === "closed" ? "Claim released on close" : "Claim released";
    case "issue.deployed":
      return `Deployed by run ${event.payload.run_id}`;
    case "handoff.recorded":
      return `Recorded the ${event.payload.phase} handoff, version ${event.payload.version}`;
    case "artifact.created":
      return `Added ${event.payload.artifact.name}`;
    case "artifact.renamed":
      return `Renamed ${event.payload.previous_name} to ${event.payload.artifact.name}`;
    case "artifact.removed":
      return `Removed ${event.payload.artifact.name}`;
    case "artifact.approved":
      return `Approved ${event.payload.name} v${event.payload.version}`;
    case "artifact.changes_requested":
      return `Changes requested on ${event.payload.name} v${event.payload.version}`;
    case "artifact.version":
      return `Saved ${event.payload.name} v${event.payload.version.number}`;
    case "artifact.human_edited":
      return `Edited ${event.payload.name} v${event.payload.from_version} to v${event.payload.to_version}`;
    case "suggestion.accepted":
      return "Suggestion accepted";
    case "suggestion.rejected":
      return "Suggestion rejected";
    case "child.status":
      return "Child status changed";
    case "child.added":
      return `Added child ${event.payload.child_key}`;
    case "child.removed":
      return `Removed child ${event.payload.child_key}`;
    case "subscription.remove_requested":
      return [
        "Unsubscribe requested for ",
        { sessionId: event.payload.session_id, storedTitle: undefined },
      ];
    case "subscription.removed":
      return ["Unsubscribed ", { sessionId: event.payload.session_id, storedTitle: undefined }];
    case "ask.follower_added":
      return [{ sessionId: event.payload.session_id, storedTitle: undefined }, " follows an ask"];
    case "ask.follower_removed":
      return [
        { sessionId: event.payload.session_id, storedTitle: undefined },
        " unfollowed an ask",
      ];
    case "block.repaired":
      return `Repaired block ${event.payload.block_id}`;
    case "block.invalid":
      return blockInvalidDescription(event.payload);
    case "result.recorded":
      return `Check ${event.payload.check_block_id} ${event.payload.verdict === "pass" ? "passed" : "failed"} at ${event.payload.commit_sha}, recorded by ${event.payload.role}${event.payload.issue === event.payload.check_issue ? "" : ` on ${event.payload.issue}`}`;
    case "result.retracted":
      return `Check ${event.payload.check_block_id}'s result at ${event.payload.commit_sha} was retracted: ${event.payload.reason}`;
    case "issue.pull_request_attribution_changed":
      return `Pull request ${event.payload.repo}#${event.payload.number} moved from ${event.payload.from ?? "no issue"} to ${event.payload.to ?? "no issue"}, decided by ${event.payload.source}`;
  }
  return event satisfies never;
}
