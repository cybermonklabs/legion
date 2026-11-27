import { Fragment, type ReactNode } from "react";

import type { DispatchEvent, DisplayTree } from "../../api/types";
import { type AgentPart, AgentParts, askResolutionParts } from "../refs/AgentName";
import { askResolutionReason } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { type TextSource, textSourceOf } from "../refs/text-references";

/**
 * A text of the record inside a sentence the dashboard writes - the question in `asked “…”`, the
 * question a reply answers, a resolution's reason - kept as a part until the sentence is drawn,
 * so the one reader reads it (`SentenceParts`) rather than the sentence carrying its source.
 */
export interface ProsePart {
  /** The text as it is stored. */
  readonly prose: string;
  /** The record the text is - the ask whose question or reason it is, the comment or message
   *  whose body it is - whose document a `#id` in it names a question block of. */
  readonly source: TextSource | undefined;
  /** The reading Go made of it, where the event carries one (`DispatchEvent.trees`). */
  readonly tree: DisplayTree | undefined;
}

/** One part of an event's sentence: the dashboard's own words, an agent it names, or a text of
 *  the record. */
export type SentencePart = AgentPart | ProsePart;

/** The text an event's payload holds at `path` (`payload.question`, `payload.resolution.reason`)
 *  as a part of the event's sentence, with the tree that rides beside the payload for it and the
 *  record the text is. */
export function eventProse(
  event: DispatchEvent,
  path: string,
  prose: string,
  source: TextSource | undefined
): ProsePart {
  return { prose, source, tree: event.trees?.[path] };
}

/** An ask event's question as a part of its sentence: the ask is the record the question is. */
export function askQuestionProse(
  event: DispatchEvent & { readonly payload: { readonly id: string; readonly question: string } }
): ProsePart {
  return eventProse(
    event,
    "payload.question",
    event.payload.question,
    textSourceOf("ask", event.payload.id, event.issue_key)
  );
}

/** An `ask.resolved` event's sentence: who resolved, retracted, declined or withdrew the ask
 *  (`askResolutionParts`), and after it the reason they gave, which is their own stored text. */
export function askResolvedParts(
  event: Extract<DispatchEvent, { type: "ask.resolved" }>
): SentencePart[] {
  const { resolution } = event.payload;
  const reason = askResolutionReason(resolution);
  return reason === undefined
    ? askResolutionParts(resolution)
    : [
        ...askResolutionParts(resolution),
        " - ",
        eventProse(
          event,
          "payload.resolution.reason",
          reason,
          textSourceOf("ask", event.payload.id, event.issue_key)
        ),
      ];
}

/**
 * Draws a sentence's parts in a line of text: its words and the agents it names as `AgentParts`
 * draws them, and each text of the record through the renderer, inline and with its record, so
 * a reference in it is a link reading as its target's name, as it is in the record's own body.
 * No host of a sentence holds it inside a link or a button, so its references link (R6): the
 * reading list's `event-line` (`DISPATCH_READING_SURFACES`).
 */
export function SentenceParts({
  parts,
  text,
}: {
  parts: readonly SentencePart[];
  /** How the host marks up a part that is the dashboard's own words (`AgentParts`). */
  text?: (part: string, index: number) => ReactNode;
}): ReactNode {
  return parts.map((part, index) => (
    // biome-ignore lint/suspicious/noArrayIndexKey: the parts are one fixed sentence, in order.
    <Fragment key={index}>
      {typeof part === "object" && "prose" in part ? (
        <MarkdownBody
          markdown={part.prose}
          source={part.source}
          surface="event-line"
          tree={part.tree}
          variant="inline"
        />
      ) : (
        <AgentParts
          parts={[part]}
          text={text === undefined ? undefined : (words) => text(words, index)}
        />
      )}
    </Fragment>
  ));
}
