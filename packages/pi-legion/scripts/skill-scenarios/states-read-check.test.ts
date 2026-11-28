// states-read-check.sh's judgement of two scores, on score tables written here in score.ts's
// shape. It judges a row when the row has at least four scorable runs and its scorable runs and
// rig errors make the batch's five; any other row reads UNKNOWN and the judgement exits 3, whatever
// its rate: four rig errors beside one scored run must not stand in for a batch. A judged row that
// does not hold reads FAIL and the judgement exits 1: head short of passing each of its scorable
// runs on no-verdict-fail, no-verdict-pass, ci-check-fail or ci-check-fail-other-head, or a planted
// label (ablate-states-read on no-verdict-fail, approve-over-ci on ci-check-fail) passing more than
// two of its scorable runs, which shows that the world cannot see the rule the label removes.
//
//   bun test states-read-check.test.ts

import { expect, setDefaultTimeout, test } from "bun:test";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin, type Wait } from "@legion/test-fixtures/row-clock";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { fileRowLimit } from "./test-limit";

/** One run of `states-read-check.sh judge`: bash and awk over two small files. */
const JUDGE: Wait = { part: "states-read-check.sh judge", boundMs: 60_000 };

setDefaultTimeout(fileRowLimit(JUDGE));

const SCRIPT = path.join(import.meta.dir, "states-read-check.sh");
const scratch = temporaryDirectories("states-read-check");

/** A row of a score's table of counts: the pass column and the rig errors. */
interface Counts {
  readonly pass: string;
  readonly rig: number;
}

const complete = (passed: number): Counts => ({ pass: `${passed}/5`, rig: 0 });

/** Scores whose every row the judgement reads is judged and holds. */
const REVIEWER: Record<string, Counts> = {
  "reviewer-own-head:no-implementer\tablate-states-read": complete(1),
  "reviewer-own-head:no-implementer\thead": complete(5),
  "reviewer-own-head:no-verdict-fail\tablate-states-read": complete(0),
  "reviewer-own-head:no-verdict-fail\thead": complete(5),
  "reviewer-own-head:no-verdict-pass\tablate-states-read": complete(4),
  "reviewer-own-head:no-verdict-pass\thead": complete(5),
  "reviewer-own-head:ci-check-fail\tapprove-over-ci": complete(1),
  "reviewer-own-head:ci-check-fail\thead": complete(5),
  "reviewer-own-head:ci-check-fail-other-head\tapprove-over-ci": complete(5),
  "reviewer-own-head:ci-check-fail-other-head\thead": complete(5),
};
const ARCHITECT: Record<string, Counts> = {
  "architect-pass-and-fail\tablate-states-read": complete(4),
  "architect-pass-and-fail\thead": complete(5),
  "architect-pass-and-fail:no-verdict-fail\tablate-states-read": complete(2),
  "architect-pass-and-fail:no-verdict-fail\thead": complete(5),
  "architect-pass-and-fail:no-verdict-pass\tablate-states-read": complete(5),
  "architect-pass-and-fail:no-verdict-pass\thead": complete(5),
  "architect-pass-and-fail:ci-check-fail\tapprove-over-ci": complete(2),
  "architect-pass-and-fail:ci-check-fail\thead": complete(5),
  "architect-pass-and-fail:ci-check-fail-other-head\thead": complete(5),
};

/** A score as score.ts prints it: a run line, the table of counts, a blank line, the legend.
 * A row whose counts are `undefined` is left out of the table. */
function score(rows: Record<string, Counts | undefined>): string {
  const table = Object.entries(rows)
    .filter((entry): entry is [string, Counts] => entry[1] !== undefined)
    .map(([row, counts]) => {
      const scored = counts.pass.split("/")[1];
      return `${row}\t${counts.pass}\t0/${scored}\t${counts.rig}\t0`;
    });
  return [
    "some-scenario-head-1\tpass=true\tref=false\texit=0",
    "",
    "scenario\tlabel\tpass\tpass+ref\trig errors\tflagged",
    ...table,
    "",
    "pass       the scorer's rule for each scenario",
    "",
  ].join("\n");
}

/** The judgement of the two scores, each the one above with `reviewer` and `architect` written
 * over its rows, and what it printed on both streams. */
function judge(
  reviewer: Record<string, Counts | undefined> = {},
  architect: Record<string, Counts | undefined> = {}
) {
  const dir = scratch();
  const files = [path.join(dir, "reviewer.txt"), path.join(dir, "architect.txt")];
  writeFileSync(files[0] as string, score({ ...REVIEWER, ...reviewer }));
  writeFileSync(files[1] as string, score({ ...ARCHITECT, ...architect }));
  const run = spawnWithin(JUDGE, ["bash", SCRIPT, "judge", ...files]);
  return { code: run.exitCode, out: `${run.stdout.toString()}${run.stderr.toString()}` };
}

// Catches: a judgement that leaves out a case of either role or a planted label, or that compares
// a planted label with head in place of its own bar.
test("scores whose rows hold pass the judgement, each planted label by its own count", () => {
  const { code, out } = judge();
  for (const scenario of ["reviewer-own-head", "architect-pass-and-fail"])
    for (const which of [
      "no-verdict-fail",
      "no-verdict-pass",
      "ci-check-fail",
      "ci-check-fail-other-head",
    ])
      expect(out).toContain(`ok ${scenario}:${which} head 5/5`);
  expect(out).toContain("ok reviewer-own-head:no-verdict-fail ablate-states-read 0/5, at most 2");
  expect(out).toContain(
    "ok architect-pass-and-fail:no-verdict-fail ablate-states-read 2/5, at most 2"
  );
  expect(out).toContain("ok reviewer-own-head:ci-check-fail approve-over-ci 1/5, at most 2");
  expect(out).toContain("ok architect-pass-and-fail:ci-check-fail approve-over-ci 2/5, at most 2");
  expect(out).toContain("ok judgement");
  expect(code).toBe(0);
});

// Catches: the comparison with head, which passed a planted label at any count below head's, 3/5
// and 4/5 among them.
for (const [scenario, label, which] of [
  ["reviewer-own-head", "ablate-states-read", "no-verdict-fail"],
  ["architect-pass-and-fail", "ablate-states-read", "no-verdict-fail"],
  ["reviewer-own-head", "approve-over-ci", "ci-check-fail"],
  ["architect-pass-and-fail", "approve-over-ci", "ci-check-fail"],
] as const)
  test(`${label} at 3/5 on ${scenario}:${which} fails, and 2/5 passes`, () => {
    const at = (passed: number) => {
      const row = { [`${scenario}:${which}\t${label}`]: complete(passed) };
      return scenario === "reviewer-own-head" ? judge(row) : judge({}, row);
    };
    const three = at(3);
    expect(three.out).toContain(
      `FAIL ${scenario}:${which} ${label}: 3/5, more than 2, so the world cannot see the rule`
    );
    expect(three.out).toContain("FAIL judgement");
    expect(three.code).toBe(1);
    const two = at(2);
    expect(two.out).toContain(`ok ${scenario}:${which} ${label} 2/5, at most 2`);
    expect(two.code).toBe(0);
  });

test("a planted label equal to head fails", () => {
  const { code, out } = judge(
    {},
    { "architect-pass-and-fail:no-verdict-fail\tablate-states-read": complete(5) }
  );
  expect(out).toContain(
    "FAIL architect-pass-and-fail:no-verdict-fail ablate-states-read: 5/5, more than 2, so the world cannot see the rule"
  );
  expect(code).toBe(1);
});

// Catches: a row of four scorable runs beside one rig error read UNKNOWN, which the floor of four
// judges: a planted label then by its own count, 4/4 failing and 2/4 passing.
test("a planted row of four scorable runs and a rig error is judged by its own count", () => {
  const four = judge(
    {},
    { "architect-pass-and-fail:no-verdict-fail\tablate-states-read": { pass: "4/4", rig: 1 } }
  );
  expect(four.out).toContain(
    "FAIL architect-pass-and-fail:no-verdict-fail ablate-states-read: 4/4, more than 2, so the world cannot see the rule"
  );
  expect(four.code).toBe(1);
  const two = judge({
    "reviewer-own-head:ci-check-fail\tapprove-over-ci": { pass: "2/4", rig: 1 },
  });
  expect(two.out).toContain("ok reviewer-own-head:ci-check-fail approve-over-ci 2/4, at most 2");
  expect(two.code).toBe(0);
});

// Catches: a head row of four scorable runs beside a rig error read UNKNOWN, or judged against
// five passes; and its case's control compared with it rather than by its own count.
test("a head row of four scorable runs and a rig error passes at 4/4 and fails at 3/4", () => {
  const four = judge({ "reviewer-own-head:no-verdict-fail\thead": { pass: "4/4", rig: 1 } });
  expect(four.out).toContain("ok reviewer-own-head:no-verdict-fail head 4/4");
  expect(four.out).toContain(
    "ok reviewer-own-head:no-verdict-fail ablate-states-read 0/5, at most 2"
  );
  expect(four.out).not.toContain("FAIL");
  expect(four.code).toBe(0);
  const three = judge({ "reviewer-own-head:no-verdict-fail\thead": { pass: "3/4", rig: 1 } });
  expect(three.out).toContain("FAIL reviewer-own-head:no-verdict-fail head: 3/4, want 4/4");
  expect(three.code).toBe(1);
});

// Catches: a row judged below the floor, so three scored runs and two rig errors approve it.
test("a row of three scorable runs and two rig errors reads UNKNOWN", () => {
  const { code, out } = judge({
    "reviewer-own-head:ci-check-fail\thead": { pass: "3/3", rig: 2 },
  });
  expect(out).toContain("UNKNOWN reviewer-own-head:ci-check-fail head: 3/3 with 2 rig errors");
  expect(out).not.toContain("ok reviewer-own-head:ci-check-fail head");
  expect(out).toContain("UNKNOWN judgement");
  expect(out).not.toContain("ok judgement");
  expect(code).toBe(3);
});

// Catches: a planted row compared at any rate, so one scored run and four rig errors approve the
// control.
test("a planted row of the reviewer with one scorable run reads UNKNOWN", () => {
  const { code, out } = judge({
    "reviewer-own-head:no-verdict-fail\tablate-states-read": { pass: "0/1", rig: 4 },
  });
  expect(out).toContain(
    "UNKNOWN reviewer-own-head:no-verdict-fail ablate-states-read: 0/1 with 4 rig errors"
  );
  expect(out).not.toContain("ok reviewer-own-head:no-verdict-fail ablate-states-read");
  expect(out).toContain("UNKNOWN judgement");
  expect(code).toBe(3);
});

test("a planted row of the architect with one scorable run reads UNKNOWN", () => {
  const { code, out } = judge(
    {},
    { "architect-pass-and-fail:ci-check-fail\tapprove-over-ci": { pass: "0/1", rig: 4 } }
  );
  expect(out).toContain(
    "UNKNOWN architect-pass-and-fail:ci-check-fail approve-over-ci: 0/1 with 4 rig errors"
  );
  expect(out).not.toContain("ok architect-pass-and-fail:ci-check-fail approve-over-ci");
  expect(code).toBe(3);
});

// Catches: a rig error beside five passes, which no batch of five makes, read as head's 5/5.
test("a head row with a rig error beside five scored runs reads UNKNOWN", () => {
  const { code, out } = judge(
    {},
    { "architect-pass-and-fail:no-verdict-pass\thead": { pass: "5/5", rig: 1 } }
  );
  expect(out).toContain(
    "UNKNOWN architect-pass-and-fail:no-verdict-pass head: 5/5 with 1 rig error"
  );
  expect(out).not.toContain("ok architect-pass-and-fail:no-verdict-pass head");
  expect(code).toBe(3);
});

test("a row the score does not hold reads UNKNOWN", () => {
  const { code, out } = judge(
    {},
    { "architect-pass-and-fail:ci-check-fail-other-head\thead": undefined }
  );
  expect(out).toContain(
    "UNKNOWN architect-pass-and-fail:ci-check-fail-other-head head: the score holds no such row"
  );
  expect(code).toBe(3);
});

test("a judged head row below its scorable runs fails", () => {
  const { code, out } = judge({}, { "architect-pass-and-fail:ci-check-fail\thead": complete(4) });
  expect(out).toContain("FAIL architect-pass-and-fail:ci-check-fail head: 4/5, want 5/5");
  expect(out).toContain("FAIL judgement");
  expect(code).toBe(1);
});

// A row that does not hold decides the judgement even when another row is not judged.
test("a failed row beside an UNKNOWN one fails", () => {
  const { code, out } = judge(
    { "reviewer-own-head:no-verdict-pass\thead": complete(3) },
    { "architect-pass-and-fail:no-verdict-fail\tablate-states-read": { pass: "0/1", rig: 4 } }
  );
  expect(out).toContain("FAIL reviewer-own-head:no-verdict-pass head: 3/5, want 5/5");
  expect(out).toContain("UNKNOWN architect-pass-and-fail:no-verdict-fail ablate-states-read");
  expect(out).toContain("FAIL judgement");
  expect(code).toBe(1);
});
