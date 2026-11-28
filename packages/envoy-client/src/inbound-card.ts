import { decode, encode } from "@toon-format/toon";

/**
 * A card `renderInbound` (`./delivery`) wrote, read back from its text: who sent it, whether it
 * brings a Dispatch event, the words a person or an agent wrote in it, with the line of the card
 * that holds them, and the notes the card adds about them. A host that shows a card somewhere else
 * (the live view's stream) or folds it (Oh My Pi's collapsed card) reads it here, beside the
 * module that writes it, and never parses the card's TOON itself.
 */
export interface InboundCard {
  /** Who sent it: the card's `from` (`senderLabel`); undefined on a card that names nobody. */
  readonly from: string | undefined;
  /** Whether it brings a Dispatch event: its `dispatch` block names the event, its actor and
   *  where it happened. */
  readonly dispatch: boolean;
  /**
   * The words its sender wrote, and the index of the card's line that holds them: an Envoy
   * message's `message` when that is text, else its `summary` when the card carries no `message`
   * (`renderInbound` drops a payload that says no more than its summary), or a Dispatch message's
   * `body`. Undefined on any other card: a structured message, one whose words are empty, or a
   * Dispatch event that is no message.
   */
  readonly words: { readonly text: string; readonly line: number } | undefined;
  /** The card's `note`: that its body names a session other than its sender
   *  (`body names session <id>; the sender is <id>`). */
  readonly note: string | undefined;
  /** The card's `body_note`: that the event carried its body in part, or not at all. */
  readonly bodyNote: string | undefined;
}

/** The line every card `renderInbound` writes as TOON opens with. */
const CARD_HEAD = "envoy:";

/** Each level of TOON nesting is two spaces, so the depth of a key is its indent over two. */
const TOON_INDENT = "  ";

function fieldOf(value: unknown, key: string): unknown {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? Reflect.get(value, key)
    : undefined;
}

/**
 * Reads `text`, a card's text as the host holds it, back into what it says. The card's TOON block
 * is its first line, `envoy:`, and every indented line after it: what follows is no TOON, such as
 * a person's edit's diff after a blank line, the line a card published while the session was away
 * ends with (`storedText`), or a picture's placeholder. Undefined for text that opens with no such
 * block, which is no card `renderInbound` wrote as TOON: one of the plugin's own notices, or a
 * plain sentence such as a subscription's removal.
 */
export function readInboundCard(text: string): InboundCard | undefined {
  const lines = text.split("\n");
  if (lines[0] !== CARD_HEAD) return undefined;
  let end = 1;
  while (end < lines.length && lines[end]?.startsWith(TOON_INDENT) === true) end += 1;
  let decoded: unknown;
  try {
    decoded = decode(lines.slice(0, end).join("\n"));
  } catch {
    return undefined;
  }
  const envoy = fieldOf(decoded, "envoy");
  const from = fieldOf(envoy, "from");
  const dispatch = fieldOf(envoy, "dispatch");
  // The line a value of `key` at `depth` is written on, found as the encoder writes it: TOON quotes
  // an object field's value by what the value holds and the document's delimiter alone, so the
  // same key and value encode to the same line at any depth.
  const wordsAt = (depth: number, key: string, value: unknown): InboundCard["words"] => {
    if (typeof value !== "string" && typeof value !== "number" && typeof value !== "boolean") {
      return undefined;
    }
    if (value === "") return undefined;
    const line = lines.indexOf(`${TOON_INDENT.repeat(depth)}${encode({ [key]: value })}`, 1);
    return line < 1 || line >= end ? undefined : { text: String(value), line };
  };
  let words: InboundCard["words"];
  if (dispatch !== undefined) {
    const type = fieldOf(dispatch, "type");
    words =
      typeof type === "string" && type.startsWith("message.")
        ? wordsAt(3, "body", fieldOf(fieldOf(dispatch, "payload"), "body"))
        : undefined;
  } else {
    const message = fieldOf(envoy, "message");
    words =
      message === undefined
        ? wordsAt(1, "summary", fieldOf(envoy, "summary"))
        : wordsAt(1, "message", message);
  }
  const note = fieldOf(envoy, "note");
  const bodyNote = fieldOf(envoy, "body_note");
  return {
    bodyNote: typeof bodyNote === "string" ? bodyNote : undefined,
    dispatch: dispatch !== undefined,
    from: typeof from === "string" ? from : undefined,
    note: typeof note === "string" ? note : undefined,
    words,
  };
}
