// Another author's text on a host's own lines: on one line, whatever line breaks it holds, or cut to
// its head without splitting a character.
import { describe, expect, test } from "bun:test";
import { headUnits, oneLine } from "../line-text";

describe("oneLine", () => {
  // JavaScript's `\s` holds LINE SEPARATOR and PARAGRAPH SEPARATOR but not NEXT LINE (U+0085),
  // and TOON's encoder writes all three inside a value as they are.
  test("makes each run of white space one space, U+0085, U+2028 and U+2029 among it", () => {
    expect(oneLine("one\u0085two\u2028three\u2029four\r\n\tfive \u00a0six")).toBe(
      "one two three four five six"
    );
    expect(oneLine("\u0085\u2028\u2029")).toBe(" ");
    expect(oneLine("one line")).toBe("one line");
  });
});

describe("headUnits", () => {
  // A character outside the Basic Multilingual Plane is two UTF-16 units, and a cut between them
  // leaves the first alone, which a UTF-8 encoder writes as U+FFFD.
  test("never cuts between the two halves of a surrogate pair", () => {
    const grin = "\u{1F600}";
    const text = `ab${grin}cd`;

    expect(headUnits(text, 3)).toBe("ab");
    expect(headUnits(text, 3)).not.toMatch(/\p{Cs}/u);
    expect(headUnits(text, 4)).toBe(`ab${grin}`);
    expect(headUnits(text, 2)).toBe("ab");
    expect(headUnits(text, 0)).toBe("");
    expect(headUnits(text, text.length)).toBe(text);
    expect(headUnits(grin, 1)).toBe("");
  });
});
