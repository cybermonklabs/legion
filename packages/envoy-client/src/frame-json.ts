/**
 * An inbound frame's JSON text as every reader of it reads it: each key and string well-formed
 * Unicode, and no key spelled `__proto__`, at any depth and in any spelling JSON allows
 * (`"\u005f_proto__"` too).
 *
 * JSON.parse keeps a `__proto__` key as an ordinary key of the object it makes. A zod object that
 * passes unknown keys through (`.passthrough()`, `z.looseObject`) copies each of them by
 * assignment, though, and assigning `__proto__` sets the copy's prototype:
 * `{"source":"dispatch","__proto__":{"topic":"<another project's topic>"}}` parsed to an envelope
 * whose `topic` read that topic, though the frame has no `topic` key, and a value no schema had
 * checked. No field of any frame is named `__proto__`, so leaving the key out loses nothing a
 * reader reads, and each field a schema reads from the value is one of the frame's own.
 *
 * A JSON escape can spell half of a surrogate pair alone (`"\ud800"`), which no UTF-8 byte can,
 * and the card's TOON encoder throws on such a string or key: the host then answered the frame
 * `nak`, and the stream sent it again for ever. Each such half reads as U+FFFD instead, as the
 * relay's `TextDecoder` already reads bytes that are no UTF-8 and as Go's encoding/json reads the
 * escape, so the card shows where the text was unreadable and the whole of the rest.
 */
export function parseFrameJSON(text: string): unknown {
  // JSON.parse leaves out a key whose reviver answers `undefined`, and sets the value it answers
  // otherwise; it calls the reviver on an object after its members, so the object's own keys are
  // the last thing left to read.
  return JSON.parse(text, (key, value: unknown) => {
    if (key === "__proto__") return undefined;
    if (typeof value === "string") return value.replace(LONE_SURROGATE, "\uFFFD");
    if (typeof value !== "object" || value === null || Array.isArray(value)) return value;
    for (const name in value) {
      if (name.search(LONE_SURROGATE) !== -1) {
        return Object.fromEntries(
          Object.entries(value).map(([member, held]) => [
            member.replace(LONE_SURROGATE, "\uFFFD"),
            held,
          ])
        );
      }
    }
    return value;
  });
}

// With the `u` flag a whole surrogate pair is one code point, so only a half standing alone is in
// the category Cs (surrogate).
const LONE_SURROGATE = /\p{Cs}/gu;
