import type { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { readPictures, shownPictureAddresses } from "@legion/envoy-client/dispatch-picture-tools";
import { imageBlocks, pictureAddresses } from "@legion/envoy-client/dispatch-pictures";
import type { ContentBlock } from "./pi-types";

/** What an Inbox delivery hands the model, and the pictures it shows. */
export interface PictureDelivery {
  /** `text` alone, or `text` followed by one image block per picture shown. */
  readonly content: string | ContentBlock[];
  /** The addresses of the pictures `content` shows, in its order. They join the session's
   *  `shownPictures` once the host took the delivery: a send that fails leaves them for the
   *  delivery Dispatch retries. */
  readonly shown: readonly string[];
}

/**
 * The content an Inbox delivery hands the model: `text` alone, or, when it embeds pictures Dispatch
 * serves (`addresses`, in writing order), `text` followed by one image block per picture shown,
 * `PICTURES_SHOWN_MAX` and `PICTURES_SHOWN_MAX_BYTES` at most, as `dispatch_read` shows them.
 * `shown` is the pictures this session was already shown (`shownPictures`): one of them is named
 * as shown earlier rather than sent again. `shown` is only read; the caller adds the delivery's
 * `shown` once the host took it. `named` appends the `Pictures:` lines that say which image is
 * which and name the rest, a card whose every picture was shown earlier included; a person's own
 * turn keeps their text exactly as they sent it. A picture Dispatch cannot serve, or no Dispatch
 * configuration (`client` answers undefined or rejects), leaves the delivery its text.
 */
export async function withDeliveredPictures(
  text: string,
  addresses: readonly string[],
  client: () => Promise<DispatchClient | undefined>,
  named: boolean,
  shown: ReadonlySet<string>
): Promise<PictureDelivery> {
  const textAlone = { content: text, shown: [] };
  if (addresses.length === 0) return textAlone;
  let dispatch: DispatchClient | undefined;
  try {
    dispatch = await client();
  } catch {
    // A Dispatch configuration that cannot be read costs the delivery its pictures, never its text.
    return textAlone;
  }
  if (dispatch === undefined) return textAlone;
  const unique = [...new Set(addresses)];
  const shownEarlier = unique.some((address) => shown.has(address));
  const pictures = await readPictures(dispatch, unique, shown);
  const content = named ? `${text}\n\n${pictures.lines.join("\n")}` : text;
  if (pictures.images.length === 0) return shownEarlier ? { content, shown: [] } : textAlone;
  return {
    content: [{ type: "text", text: content }, ...imageBlocks(pictures.images)],
    shown: pictures.shown,
  };
}

/** A transcript entry's message, as far as its pictures go. */
interface TranscriptMessage {
  readonly role: unknown;
  readonly content: unknown;
}

/** A transcript entry's message: a message entry's own, or a custom message entry's content. */
function entryMessage(entry: unknown): TranscriptMessage | undefined {
  if (typeof entry !== "object" || entry === null || !("type" in entry)) return undefined;
  if (entry.type === "custom_message") {
    return { role: "custom", content: "content" in entry ? entry.content : undefined };
  }
  if (entry.type !== "message" || !("message" in entry)) return undefined;
  const { message } = entry;
  if (typeof message !== "object" || message === null) return undefined;
  return {
    role: "role" in message ? message.role : undefined,
    content: "content" in message ? message.content : undefined,
  };
}

/**
 * The addresses of the pictures a session's transcript already shows, each once: every picture a
 * tool result or a delivered card names as shown (`shownPictureAddresses`: `dispatch_read`,
 * `dispatch_doc_read`, an Inbox card), and the pictures of a person's own turn whose image blocks
 * number the pictures its text embeds, so every one of them was shown (a turn keeps its text as
 * sent, naming none). `entries` are the transcript as Oh My Pi keeps it: a message entry holds its
 * message, a custom message entry its content. A caller hands it the branch's live context
 * (`liveContext`), the entries the model still sees, so a picture a compaction or a `/clear` cut
 * off counts as not shown, and an entry before that cut is never read.
 */
export function transcriptPictures(entries: readonly unknown[]): string[] {
  const shown = new Set<string>();
  for (const entry of entries) {
    const message = entryMessage(entry);
    if (message === undefined) continue;
    const { content } = message;
    const texts: string[] = [];
    let images = 0;
    if (typeof content === "string") texts.push(content);
    else if (Array.isArray(content)) {
      for (const block of content) {
        if (typeof block !== "object" || block === null || !("type" in block)) continue;
        if (block.type === "image") images++;
        if (block.type === "text" && "text" in block && typeof block.text === "string") {
          texts.push(block.text);
        }
      }
    }
    for (const address of shownPictureAddresses(texts)) shown.add(address);
    if (message.role !== "user" || images === 0) continue;
    const embedded = new Set(texts.flatMap(pictureAddresses));
    if (embedded.size === images) for (const address of embedded) shown.add(address);
  }
  return [...shown];
}

/** `field` of a branch entry, read alone: no other member of the entry is touched. */
function entryField(entry: unknown, field: "type" | "id" | "firstKeptEntryId"): unknown {
  if (typeof entry !== "object" || entry === null || !(field in entry)) return undefined;
  return Reflect.get(entry, field);
}

/**
 * The branch's live context: the entries of `branch` (root first, as `sessionManager.getBranch()`
 * hands it) that the model's next request carries, by Oh My Pi's own rule (`buildSessionContext`,
 * `packages/coding-agent/src/session/session-context.ts` in cybermonklabs/oh-my-pi at tag
 * `v18.18.6-sami.20261115-031133`). A `reset_boundary` (`/clear`) that follows the latest
 * compaction, or stands on a branch with none, starts it after the boundary. Else the latest
 * compaction starts it at its `firstKeptEntryId` where that entry stands before the compaction on
 * the branch, and at the compaction where it does not. With neither, it is the whole branch.
 *
 * The compaction entry is part of the answer, as the host opens the model's context with its
 * summary; that summary is text with no image block, and `transcriptPictures` reads no compaction
 * entry, so it adds no picture. A `reset_boundary` itself carries nothing to the model and is
 * left out.
 *
 * The branch is read backwards from the leaf over each entry's `type`, `id` and
 * `firstKeptEntryId` alone, never an entry's message or content: a long session's start reads no
 * body the cut left behind (about 28,600 of them for a long controller session, whose resume read
 * them all into memory).
 *
 * Where the host keeps more than this rule (an Anthropic-native compaction's replayed snapshot, a
 * notes-backed compaction's last request, an older compaction the model can read where it cannot
 * read the latest), the answer is shorter than what the model sees: a picture there counts as not
 * shown, and a delivery sends it once more, which costs its bytes and nothing else. No entry before
 * the cut is in the answer, so no picture a compaction or a `/clear` took from the model counts as
 * shown. An OpenAI remote compaction is the one the host reads otherwise: it hands the model the
 * kept turns inside the provider's replacement history, not as the kept entries this answer holds.
 */
export function liveContext(branch: readonly unknown[]): readonly unknown[] {
  for (let index = branch.length - 1; index >= 0; index--) {
    const type = entryField(branch[index], "type");
    if (type === "reset_boundary") return branch.slice(index + 1);
    if (type !== "compaction") continue;
    const firstKept = entryField(branch[index], "firstKeptEntryId");
    if (typeof firstKept === "string") {
      for (let kept = index - 1; kept >= 0; kept--) {
        if (entryField(branch[kept], "id") === firstKept) return branch.slice(kept);
      }
    }
    return branch.slice(index);
  }
  return branch;
}
