import type { AskOption } from "../../api/types";
import type { AcceptedMention, ComposerOwner, ReplyTarget } from "./composer-model";
import { mentionText, survivingMentions } from "./send-request";

/** The composer's draft as its field holds it, and what a draft starts from at an address: the
 *  channel's seed and a new reply's prefill. `MentionComposer` and the hooks that keep its drafts
 *  (`useKeptDrafts.ts`) both read these, so neither of the two imports the other. */

/** A draft as the composer holds it: the text, the mentions accepted in it, and, when it is a
 *  suggestion, its replacement. Mentions are offsets into that exact text. */
export interface Draft {
  readonly body: string;
  readonly mentions: readonly AcceptedMention[];
  readonly replacement?: string;
}

export interface AskOptionDraft {
  description: string;
  id: number;
  label: string;
}

let nextAskOptionId = 0;

export function emptyAskOption(): AskOptionDraft {
  nextAskOptionId += 1;
  return { description: "", id: nextAskOptionId, label: "" };
}

export function submittedAskOptions(options: readonly AskOptionDraft[]): AskOption[] {
  const submitted: AskOption[] = [];
  for (const option of options) {
    const label = option.label.trim();
    const description = option.description.trim();
    if (label !== "") {
      submitted.push(description === "" ? { label } : { description, label });
    }
  }
  return submitted;
}

export function mentionDisplay(title: string, target: string): string {
  const trimmed = title.trim();
  if (trimmed !== "" && !/^(?:session|role):/i.test(trimmed) && !/[@\r\n]/.test(trimmed)) {
    return trimmed;
  }
  return target.replace(":", " ");
}

/** The target a message in this channel already reaches: mentioning it says nothing. */
function ownTarget(owner: ComposerOwner): string | null {
  return owner.kind === "session" ? `session:${owner.sessionId}` : null;
}

/**
 * What a draft becomes under a channel's seed: the reader's prose with the records still in step
 * with it, and the channel's own mentions seeded in front of whatever is not already there. A
 * mount applies it to no draft at all; a host that changes the seed - an Agents row's issue pick,
 * or a reply that moves the message to another channel - has it applied to the live draft in
 * place, so the draft never parts from the composer, or from a send it has in flight.
 *
 * Three rules keep the text and the accepted records saying the same thing, because only the
 * records reach the wire (`survivingMentions` at send) while only the text reaches the reader:
 *
 * - A record counts only while its span is untouched (`survivingMentions`). A span the reader
 *   edited is their prose: the record goes, the text stays.
 * - The channel's own mention belongs to the channel. Entering one that seeds none - a direct
 *   message, which already reaches its session - takes the untouched record for that session out
 *   of the text with its separating space. Every other surviving record stays, so a trip back
 *   restores it.
 * - Seeding is by target, not by position: a mention this channel owes is added only when no
 *   surviving record already names it, wherever in the body that record sits.
 */
export function seededDraft(
  seedMentions: readonly { readonly target: string; readonly title: string }[],
  draft: Draft | undefined,
  owner: ComposerOwner
): { body: string; mentions: AcceptedMention[] } {
  let body = draft?.body ?? "";
  let records = survivingMentions(body, draft?.mentions ?? []);
  const own = ownTarget(owner);
  const addressed = own === null ? undefined : records.find((record) => record.target === own);
  if (addressed !== undefined) {
    const cut =
      body.slice(addressed.end, addressed.end + 1) === " " ? addressed.end + 1 : addressed.end;
    const width = cut - addressed.start;
    body = body.slice(0, addressed.start) + body.slice(cut);
    records = records
      .filter((record) => record !== addressed)
      .map((record) =>
        record.start >= cut
          ? { ...record, end: record.end - width, start: record.start - width }
          : record
      );
  }
  const owed = seedMentions.filter(
    (mention) => !records.some((record) => record.target === mention.target)
  );
  let offset = 0;
  const seeded = owed.map((mention) => {
    const text = mentionDisplay(mention.title, mention.target);
    const start = offset;
    offset += mentionText(text).length;
    const end = offset;
    offset += 1;
    return { end, start, target: mention.target, text };
  });
  const prefix = owed
    .map((mention) => mentionText(mentionDisplay(mention.title, mention.target)))
    .join(" ");
  const shift = prefix === "" ? 0 : prefix.length + 1;
  return {
    body: [prefix, body].filter((part) => part !== "").join(" "),
    mentions: [
      ...seeded,
      ...records.map((record) => ({
        ...record,
        end: record.end + shift,
        start: record.start + shift,
      })),
    ],
  };
}

/** What a new reply to `replyTo` starts with: the mentions its parent carries, each once, as the
 *  mention picker writes them. */
export function replyPrefill(replyTo: ReplyTarget): Draft {
  const carried = replyTo.mentions ?? [];
  const deduplicated = carried.filter(
    (mention, index) =>
      carried.findIndex((candidate) => candidate.target === mention.target) === index
  );
  const texts = deduplicated.map((mention) =>
    mentionText(mentionDisplay(mention.title, mention.target))
  );
  let offset = 0;
  return {
    body: texts.join(" "),
    mentions: deduplicated.map((mention, index) => {
      const text = mentionDisplay(mention.title, mention.target);
      const start = offset;
      offset += texts[index]?.length ?? 0;
      const end = offset;
      offset += 1;
      return { end, start, target: mention.target, text };
    }),
  };
}
