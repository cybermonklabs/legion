// tester-ci-check.sh's judgement of a score of tester-reads-no-ci, on score tables written here in
// score.ts's shape. A row is judged when it has at least four scorable runs and its scorable runs
// and rig errors make the batch's five; any other row reads UNKNOWN and the judgement exits 3. It
// passes when head passes each of its scorable runs on ci-late and on ci-red, and the planted
// label restore-tester-ci passes at most two of its scorable runs on ci-late; more shows that the
// world cannot see the rule the label removes, and the judgement exits 1.
//
//   bun test tester-ci-check.test.ts

import { expect, setDefaultTimeout, test } from "bun:test";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin, type Wait } from "@legion/test-fixtures/row-clock";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { fileRowLimit } from "./test-limit";

/** One run of `tester-ci-check.sh judge`: bash and awk over one small file. */
const JUDGE: Wait = { part: "tester-ci-check.sh judge", boundMs: 60_000 };

setDefaultTimeout(fileRowLimit(JUDGE));

const SCRIPT = path.join(import.meta.dir, "tester-ci-check.sh");
const scratch = temporaryDirectories("tester-ci-check");

/** A row of a score's table of counts: the pass column and the rig errors. */
interface Counts {
  readonly pass: string;
  readonly rig: number;
}

const complete = (passed: number): Counts => ({ pass: `${passed}/5`, rig: 0 });

const LATE_HEAD = "tester-reads-no-ci:ci-late\thead";
const RED_HEAD = "tester-reads-no-ci:ci-red\thead";
const LATE_LABEL = "tester-reads-no-ci:ci-late\trestore-tester-ci";

/** A score whose every row the judgement reads is judged and holds; the label's ci-red row is the
 * batch's too, and no row the judgement reads. */
const SCORE: Record<string, Counts> = {
  [LATE_HEAD]: complete(5),
  [RED_HEAD]: complete(5),
  [LATE_LABEL]: complete(2),
  "tester-reads-no-ci:ci-red\trestore-tester-ci": complete(4),
};

/** The judgement of the score above with `rows` written over it, and what it printed. */
function judge(rows: Record<string, Counts | undefined> = {}) {
  const file = path.join(scratch(), "score.txt");
  const table = Object.entries({ ...SCORE, ...rows })
    .filter((entry): entry is [string, Counts] => entry[1] !== undefined)
    .map(
      ([row, counts]) => `${row}\t${counts.pass}\t0/${counts.pass.split("/")[1]}\t${counts.rig}\t0`
    );
  writeFileSync(
    file,
    [
      "tester-reads-no-ci-ci-late-head-1\tpass=true\tref=false\trecord=h v1",
      "",
      "scenario\tlabel\tpass\tpass+ref\trig errors\tflagged",
      ...table,
      "",
      "pass       the scorer's rule for each scenario",
      "",
    ].join("\n")
  );
  const run = spawnWithin(JUDGE, ["bash", SCRIPT, "judge", file]);
  return { code: run.exitCode, out: `${run.stdout.toString()}${run.stderr.toString()}` };
}

// Catches: a judgement that leaves out a case of head or the planted label's row.
test("head 5/5 on both cases and restore-tester-ci 2/5 on ci-late pass", () => {
  const { code, out } = judge();
  expect(out).toContain("ok tester-reads-no-ci:ci-late head 5/5");
  expect(out).toContain("ok tester-reads-no-ci:ci-red head 5/5");
  expect(out).toContain("ok tester-reads-no-ci:ci-late restore-tester-ci 2/5, at most 2");
  expect(out).toContain("ok judgement");
  expect(code).toBe(0);
});

// Catches: the planted label compared with head, which passed it at any count below head's.
test("restore-tester-ci at 3/5 or 5/5 on ci-late fails", () => {
  for (const passed of [3, 5]) {
    const { code, out } = judge({ [LATE_LABEL]: complete(passed) });
    expect(out).toContain(
      `FAIL tester-reads-no-ci:ci-late restore-tester-ci: ${passed}/5, more than 2, so the world cannot see the rule`
    );
    expect(out).toContain("FAIL judgement");
    expect(code).toBe(1);
  }
});

// Catches: a row of four scorable runs beside one rig error read UNKNOWN, or judged against five.
test("a row of four scorable runs and a rig error is judged by its own count", () => {
  const counts = (pass: string): Counts => ({ pass, rig: 1 });
  const labelTwo = judge({ [LATE_LABEL]: counts("2/4") });
  expect(labelTwo.out).toContain("ok tester-reads-no-ci:ci-late restore-tester-ci 2/4, at most 2");
  expect(labelTwo.code).toBe(0);
  const labelThree = judge({ [LATE_LABEL]: counts("3/4") });
  expect(labelThree.out).toContain(
    "FAIL tester-reads-no-ci:ci-late restore-tester-ci: 3/4, more than 2, so the world cannot see the rule"
  );
  expect(labelThree.code).toBe(1);
  const headFour = judge({ [RED_HEAD]: counts("4/4") });
  expect(headFour.out).toContain("ok tester-reads-no-ci:ci-red head 4/4");
  expect(headFour.code).toBe(0);
  const headThree = judge({ [RED_HEAD]: counts("3/4") });
  expect(headThree.out).toContain("FAIL tester-reads-no-ci:ci-red head: 3/4, want 4/4");
  expect(headThree.code).toBe(1);
});

// Catches: a row judged below the floor of four scorable runs.
test("a row of three scorable runs and two rig errors reads UNKNOWN", () => {
  const { code, out } = judge({ [LATE_HEAD]: { pass: "3/3", rig: 2 } });
  expect(out).toContain("UNKNOWN tester-reads-no-ci:ci-late head: 3/3 with 2 rig errors");
  expect(out).toContain("UNKNOWN judgement");
  expect(out).not.toContain("ok judgement");
  expect(code).toBe(3);
});

test("a row the score does not hold reads UNKNOWN", () => {
  const { code, out } = judge({ [LATE_LABEL]: undefined });
  expect(out).toContain(
    "UNKNOWN tester-reads-no-ci:ci-late restore-tester-ci: the score holds no such row"
  );
  expect(code).toBe(3);
});
