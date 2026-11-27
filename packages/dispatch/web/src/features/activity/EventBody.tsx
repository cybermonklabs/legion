import { actorActText } from "@legion/contracts/actor-label";
import { Fragment, type ReactNode } from "react";
import { Link } from "react-router-dom";

import type { DispatchEvent, DisplayTree } from "../../api/types";
import { textPrimaryOnSurface } from "../../theme/classes";
import { linkText, textMutedOnSurface } from "../../theme/shell-classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { eventDescription } from "../issue/event-description";
import { AskResolutionLine } from "../refs/AgentName";
import { askResolutionReason } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { PostBody } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { buildIssuePath } from "../refs/page-routes";
import { postDeletion } from "../refs/post-marks";
import { buildReferencePath, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { SentenceParts } from "./SentenceParts";

type AskEvent = Extract<DispatchEvent, { type: "ask.opened" | "ask.answered" | "ask.resolved" }>;

export function isAskEvent(event: DispatchEvent): event is AskEvent {
  return (
    event.type === "ask.opened" || event.type === "ask.answered" || event.type === "ask.resolved"
  );
}

/** The tree of one stored text an event's payload carries, by that text's path in the envelope
 *  (`payload.body`, `payload.resolution.reason`). A payload keeps the shape every reader of the
 *  message bus knows, so its trees ride beside it in `trees` rather than on it
 *  (`api/event_trees.go`); a frame from a Dispatch older than the map carries none, and the
 *  body then renders as its own literal text. */
function payloadTree(event: DispatchEvent, path: string): DisplayTree | undefined {
  return event.trees?.[path];
}

function AskEventBody({
  event,
  onRendered,
}: {
  event: AskEvent;
  onRendered?: () => void;
}): ReactNode {
  const source = textSourceOf("ask", event.payload.id, event.issue_key);
  const { answer, question, options, resolution, created_at } = event.payload;
  const resolutionReason = resolution === undefined ? undefined : askResolutionReason(resolution);
  // A changed answer names the one it replaced; the first answer's event carries none. An event
  // written before answers stored an empty list holds null for either's selected: no option.
  const previous = event.type === "ask.answered" ? event.payload.previous_answer : undefined;
  const wasSelected = previous?.selected ?? [];
  const wasText = previous?.text ?? "";
  // An option's two texts are keyed by its place in the payload's own list, so the trees pair
  // with the options by index, never by label.
  const optionsWithTrees = options.map((option, index) => ({
    ...option,
    description_tree: payloadTree(event, `payload.options.${index}.description`),
    label_tree: payloadTree(event, `payload.options.${index}.label`),
  }));
  return (
    <>
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
        {event.type === "ask.opened" ? "Ask opened:" : "Ask:"}
      </p>
      <MarkdownBody
        markdown={question}
        onRendered={onRendered}
        source={source}
        surface="activity-event-body"
        tree={payloadTree(event, "payload.question")}
      />
      <AskOptionList options={optionsWithTrees} selected={answer?.selected ?? []} source={source} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        Opened <Timestamp at={created_at} />
      </p>
      {answer === null ? null : (
        <>
          <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
            Answered by {actorActText(answer.user, answer.act)} <Timestamp at={answer.at} />
          </p>
          {answer.text === null || answer.text === "" ? null : (
            <div className="mt-2">
              <MarkdownBody
                markdown={answer.text}
                onRendered={onRendered}
                source={source}
                surface="activity-event-body"
                tree={payloadTree(event, "payload.answer.text")}
              />
            </div>
          )}
          {previous === undefined ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              <s>
                was:{" "}
                {wasSelected.map((label, index) => (
                  <Fragment key={label}>
                    {index === 0 ? null : ", "}
                    {/* A label the replaced answer chose is drawn from its own tree, which the
                        event carries by the label's place in that answer's list: it can name an
                        option the ask no longer holds. Only a server older than those trees
                        sends a label with none, and the renderer then parses it, as it parses
                        every stored text of that server's events. */}
                    <MarkdownBody
                      markdown={label}
                      onRendered={onRendered}
                      source={source}
                      surface="earlier-chosen-option"
                      tree={payloadTree(event, `payload.previous_answer.selected.${index}`)}
                      variant="inline"
                    />
                  </Fragment>
                ))}
                {wasSelected.length === 0 || wasText === "" ? null : " - "}
                {wasText === "" ? null : (
                  <MarkdownBody
                    markdown={wasText}
                    onRendered={onRendered}
                    source={source}
                    surface="activity-event-body"
                    tree={payloadTree(event, "payload.previous_answer.text")}
                    variant="inline"
                  />
                )}
              </s>
            </p>
          )}
        </>
      )}
      {resolution === undefined ? null : (
        <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
          <AskResolutionLine resolution={resolution} />
          {resolutionReason === undefined ? null : (
            <>
              {" "}
              -{" "}
              <MarkdownBody
                markdown={resolutionReason}
                onRendered={onRendered}
                source={source}
                surface="activity-event-body"
                tree={payloadTree(event, "payload.resolution.reason")}
                variant="inline"
              />
            </>
          )}{" "}
          <Timestamp at={resolution.at} />
        </p>
      )}
    </>
  );
}

/** The event of a comment or message whose body the reader may have only part of. */
type BodyEvent = Extract<
  DispatchEvent,
  { type: "comment.answered" | "comment.created" | "message.answered" | "message.created" }
>;

function isBodyEvent(event: DispatchEvent): event is BodyEvent {
  return (
    event.type === "message.created" ||
    event.type === "message.answered" ||
    event.type === "comment.created" ||
    event.type === "comment.answered"
  );
}

/**
 * The ` · edited` for the byline under an event whose body `EventBody` prints, where the post's
 * author has replaced that body since. The event keeps the body as it stood when the event was
 * written, and the read of the event gives it the post's own `edited_at` (`attachPostMarks` in
 * `api/events.go`), as it gives a deleted post's event the mark `EventBody` draws in place of the
 * body. Nothing for any other event, for a post nobody edited, and for a deleted one.
 */
export function EventEditedMark({ event }: { event: DispatchEvent }): ReactNode {
  return isBodyEvent(event) ? (
    <EditedMark at={event.payload.edited_at} deleted={postDeletion(event.payload)} />
  ) : null;
}

/** The page that reads the whole body of the item an event carried the first part of: the item's
 *  own page under its issue, and for a comment on a project document, which belongs to no issue,
 *  that document with the comment in view (`itemRoute`), the address every other link to one
 *  uses. Undefined where no page names it: an agent's direct message, which belongs to no issue
 *  either, and a document comment whose event was written before the payload named its
 *  document. */
function wholeItemPath(event: BodyEvent): string | undefined {
  if (event.issue_key !== null) {
    return buildIssuePath({
      id: event.payload.id,
      key: event.issue_key,
      kind: event.type.startsWith("message.") ? "message" : "comment",
    });
  }
  if (event.type !== "comment.created" && event.type !== "comment.answered") return undefined;
  const { artifact_slug: slug, project_key: project } = event.payload;
  if (project === undefined || slug === undefined) return undefined;
  const route = itemRoute("comment", { id: event.payload.id, issue_key: null }, { project, slug });
  return route === undefined ? undefined : buildReferencePath(route);
}

export function EventBody({
  event,
  onRendered,
}: {
  event: DispatchEvent;
  onRendered?: () => void;
}): ReactNode {
  if (isAskEvent(event)) {
    return <AskEventBody event={event} onRendered={onRendered} />;
  }
  if (isBodyEvent(event)) {
    const message = event.type.startsWith("message.");
    const deleted = postDeletion(event.payload);
    // A body is bounded block by block, so one event carries its first part alone and says so
    // (`body_truncated`). The reader is given the text the event holds and the page that reads
    // the stored body whole. A deleted post's event carries neither field: the read of the
    // event took them out, so the deleted line stands there and no "whole" link follows it.
    const whole =
      deleted === null && event.payload.body_truncated === true ? wholeItemPath(event) : undefined;
    return (
      <>
        <PostBody
          deleted={deleted}
          markdown={event.payload.body ?? ""}
          onRendered={onRendered}
          source={textSourceOf(message ? "message" : "comment", event.payload.id, event.issue_key)}
          surface="activity-event-body"
          tree={payloadTree(event, "payload.body")}
        />
        {whole === undefined ? null : (
          <p className="mt-1 text-xs">
            <Link className={`dispatch-inline-link underline ${linkText}`} to={whole}>
              {message ? "Read the whole message" : "Read the whole comment"}
            </Link>
          </p>
        )}
      </>
    );
  }
  return (
    <p className={`font-medium ${textPrimaryOnSurface}`}>
      <SentenceParts parts={eventDescription(event)} />
    </p>
  );
}
