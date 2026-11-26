import type { AskOption } from "../../api/types";
import { draftParties, partyMention, partyMentionSpan } from "../refs/party-references";
import type { ComposerOwner, ReplyTarget } from "./composer-model";

/** The composer's draft as its field holds it, and what a draft starts from at an address: the
 *  channel's seed and a new reply's prefill. `MentionComposer` and the hooks that keep its drafts
 *  (`useKeptDrafts.ts`) both read these, so neither of the two imports the other. */

/** A draft as the composer holds it: the text, and, when it is a suggestion, its replacement.
 *  Who it addresses is in that text, written there as a reference (R4). */
export interface Draft {
  readonly body: string;
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

/** The target a message in this channel already reaches: mentioning it says nothing. */
function ownTarget(owner: ComposerOwner): string | null {
  return owner.kind === "session" ? `session:${owner.sessionId}` : null;
}

/** `mentions` written in front of `body`, each followed by one space. The space after the last
 *  one is there even where `body` is empty, which is the draft a seed or a reply's prefill
 *  starts, so a word the reader types at once stands as a word of its own after the mention, as
 *  a renderer draws it, rather than joined to the link's words. With no mention, `body` is the
 *  text as it stands. */
function writtenInFront(mentions: readonly string[], body: string): string {
  return mentions.map((mention) => `${mention} `).join("") + body;
}

/**
 * What a draft's text becomes under a channel's seed: the reader's prose with the channel's own
 * mentions written in front of whatever it does not already name, each followed by one space
 * (`writtenInFront`). A draft that held nothing is the seed alone and ends in that space; the
 * reader's own words after the seed are kept as they stand. A mount applies it to no draft at
 * all; a host that changes the seed - an Agents row's issue pick, or a reply that moves the
 * message to another channel - has it applied to the live draft in place, so the draft never
 * parts from the composer, or from a send it has in flight.
 *
 * Both rules read the text itself (`draftParties`), which is the whole record of who a draft
 * names (R4):
 *
 * - The channel's own mention belongs to the channel. Entering one that seeds none - a direct
 *   message, which already reaches its session - takes that session's mention out of the text,
 *   its words and its separating space with it. A trip back seeds it again.
 * - Seeding is by party, not by position: a mention this channel owes is added only when the
 *   text does not name that party already, wherever in it that mention sits.
 */
export function seededDraft(
  seedMentions: readonly { readonly target: string; readonly title: string }[],
  draft: string,
  owner: ComposerOwner
): string {
  let body = draft;
  const own = ownTarget(owner);
  const addressed = own === null ? undefined : partyMentionSpan(body, own);
  if (addressed !== undefined) {
    const cut =
      body.slice(addressed.end, addressed.end + 1) === " " ? addressed.end + 1 : addressed.end;
    body = body.slice(0, addressed.start) + body.slice(cut);
  }
  const named = new Set(draftParties(body).targets);
  const owed = seedMentions.flatMap((mention) =>
    named.has(mention.target) ? [] : (partyMention(mention.target, mention.title) ?? [])
  );
  return writtenInFront(owed, body);
}

/** What a new reply to `replyTo` starts with: the parties its parent was delivered to, each
 *  mentioned once, as the links that name them, and the one space after them that a seed ends
 *  with (`writtenInFront`). The reply's own mentions are its words, and the reader edits them as
 *  words. A mention the parent was delivered to twice is one party, and the draft they are
 *  written into is empty, which is what a channel's own seed is written into (`seededDraft`). */
export function replyPrefill(replyTo: ReplyTarget): Draft {
  const carried = replyTo.mentions ?? [];
  const deduplicated = carried.filter(
    (mention, index) =>
      carried.findIndex((candidate) => candidate.target === mention.target) === index
  );
  return {
    body: writtenInFront(
      deduplicated.flatMap((mention) => partyMention(mention.target, mention.title) ?? []),
      ""
    ),
  };
}
