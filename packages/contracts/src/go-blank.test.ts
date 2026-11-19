import { describe, expect, test } from "bun:test";
import goUnicodeIsSpace from "../fixtures/go-unicode-isspace.json";
import { goBlankPattern } from "./go-blank";

describe("goBlankPattern", () => {
  // The pattern writes Go's `unicode.IsSpace` set out, and the rows of its readers pin the two
  // characters a person meets: `dispatch artifact`'s create rows
  // (packages/envoy-client/src/__tests__/dispatch-execute.test.ts), a rating's reason
  // (./legion-api.test.ts, packages/pi-shared/src/legion-phase-ops.test.ts) and, for U+0085, a
  // role's name as the Envoy listener trims it (packages/pi-envoy/extensions/envoy.test.ts).
  // Nothing but this test holds the whole set, so a hand edit that drops one scalar, or a raise
  // of Go that moves the set, would otherwise be silent on this side. The committed list is what
  // both languages read: Go's own predicate is held to it by
  // TestUnicodeIsSpaceReportsTheCommittedScalars
  // (packages/envoy/internal/dispatch/text/whitespace_fixture_test.go).
  test("calls blank exactly the scalars the committed list holds", () => {
    const listed = new Set(goUnicodeIsSpace.scalars.map((written) => Number.parseInt(written, 16)));
    // A code point written twice would make the list shorter than it reads.
    expect(listed.size).toBe(goUnicodeIsSpace.scalars.length);

    const blankNotListed: number[] = [];
    const listedNotBlank: number[] = [];
    for (let code = 0; code <= 0x10ffff; code++) {
      const blank = goBlankPattern.test(String.fromCodePoint(code));
      if (blank && !listed.has(code)) {
        blankNotListed.push(code);
      } else if (listed.has(code) && !blank) {
        listedNotBlank.push(code);
      }
    }
    if (blankNotListed.length === 0 && listedNotBlank.length === 0) return;

    const codePoints = (codes: number[]): string =>
      codes.map((code) => `U+${code.toString(16).toUpperCase().padStart(4, "0")}`).join(", ");
    const moved: string[] = [];
    if (blankNotListed.length > 0) {
      moved.push(
        `  goBlankPattern calls ${codePoints(blankNotListed)} blank, which the list does not hold.`
      );
    }
    if (listedNotBlank.length > 0) {
      moved.push(
        `  the list holds ${codePoints(listedNotBlank)}, which goBlankPattern calls content.`
      );
    }
    throw new Error(
      `goBlankPattern and packages/contracts/fixtures/go-unicode-isspace.json disagree:\n` +
        `${moved.join("\n")}\n` +
        "This test ran the pattern over every scalar from 0 to 10FFFF, so the characters above " +
        "are where the pattern in ./go-blank.ts stands now; the list is what Go's " +
        "unicode.IsSpace reported when the list was written. Which side moved is in the Go test " +
        "that measures the same list (TestUnicodeIsSpaceReportsTheCommittedScalars, " +
        "packages/envoy/internal/dispatch/text): green there and Go has not moved, so the " +
        "pattern was edited; red there too and Go or its Unicode version moved, so both were " +
        "measured from a Go that is gone. `dispatch artifact` reads this pattern to tell whether " +
        "the Dispatch server will call a create's content blank, and a rating's reason to tell " +
        "whether the daemon will; both servers read strings.TrimSpace, which trims what the list " +
        "holds: measure the pattern and the list again together, or the clients and the servers " +
        "read the same text differently."
    );
  });
});
