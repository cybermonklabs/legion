import { describe, expect, test } from "bun:test";
import { passStands, passStandsAnswer } from "./pass-stands";

// verification-results.ts's head checks judge k1's head line with these. Most rows are a recorded
// check's head line as the `dispatch` command prints it (render-results.ts, checkLines), and the
// placeholder k1Line gives where a read holds no k1 line. Two rows are not printed head lines: one
// is the answer line ciCheckLines prints under a CI check's head line (`  A pass stands: yes, from
// ci.`), and one adds that answer to a recorded check's head line, which no code prints, to show
// that roles after `yes` are not a bare yes.
const compared =
  "- k1 (before_merge). Compared with the head of pull request #61 at 61a1 (opened).";
const notKnown = `${compared} A pass stands: not known.`;
const no = `${compared} A pass stands: no.`;
const yes = `${compared} A pass stands: yes. Newest at that commit: implementer: pass (2120a467).`;

describe("passStands", () => {
  test("a line that reads not known answers neither no nor yes", () => {
    expect(passStands(notKnown, "no")).toBe(false);
    expect(passStands(notKnown, "yes")).toBe(false);
    expect(passStands(notKnown, "not known")).toBe(true);
  });

  test("a line that reads no answers no alone", () => {
    expect(passStands(no, "no")).toBe(true);
    expect(passStands(no, "not known")).toBe(false);
    expect(passStands(no, "yes")).toBe(false);
  });

  test("a line that reads yes answers yes, whatever the newest results after it say", () => {
    expect(passStands(yes, "yes")).toBe(true);
    expect(passStands(`${compared} A pass stands: yes.`, "yes")).toBe(true);
    expect(passStands(yes, "no")).toBe(false);
  });

  test("a line with roles after yes, as a check CI proves prints, is not a bare yes", () => {
    expect(passStands(`${compared} A pass stands: yes, from ci.`, "yes")).toBe(false);
  });

  test("a line with no answer answers nothing", () => {
    const silent = `${compared} Whether a pass stands is not known: this Dispatch server does not say.`;
    expect(passStandsAnswer(silent)).toBeUndefined();
    expect(passStands(silent, "no")).toBe(false);
    expect(passStands("(no k1 line)", "yes")).toBe(false);
  });
});

describe("passStandsAnswer", () => {
  test("reads the answer whole, up to the full stop after it", () => {
    expect(passStandsAnswer(notKnown)).toBe("not known");
    expect(passStandsAnswer(no)).toBe("no");
    expect(passStandsAnswer(yes)).toBe("yes");
    expect(passStandsAnswer("  A pass stands: yes, from ci.")).toBe("yes, from ci");
  });
});
