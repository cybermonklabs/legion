import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  type FixedWait,
  minimumReasonWords,
  reasonWords,
  refusedWaits,
  suiteFiles,
  waitsOfFiles,
} from "../fixed-waits";
import { specFiles } from "../no-browser-rows";

// Like e2e/__tests__/no-browser-rows.test.ts, this file imports no Playwright config: it reads
// the suite from its own source, and starts no subprocess either, so the whole file is a parse
// of the suite and costs about a second.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));

const scannedFiles = suiteFiles(e2eDirectory);
const realWaits = waitsOfFiles(e2eDirectory, scannedFiles);
// The rule is run over the suite once, here, because two tests below read the result: the one
// that asserts it and the one that prints the evidence line only when it held.
const realRefused = refusedWaits(realWaits);

/** The planted controls, read by name rather than by pointing `suiteFiles` at their directory:
 *  they are no part of the suite, and every one of them is written to be refused. */
const fixture = "__tests__/fixtures/fixed-waits.fixture.ts";
const fixtureWaits = waitsOfFiles(e2eDirectory, [fixture]);

/** One planted wait, by the line the fixture writes it on. */
function planted(line: number): FixedWait {
  const found = fixtureWaits.find((wait) => wait.line === line);
  if (found === undefined) throw new Error(`${fixture} holds no wait on line ${line}`);
  return found;
}

// A scan that found nothing would satisfy the rule below without reading a wait, so this is
// asserted once here rather than again in each test that reads `realWaits`.
test("the scan reads files and waits of the real suite", () => {
  expect(scannedFiles.length).toBeGreaterThan(0);
  expect(realWaits.length).toBeGreaterThan(0);
});

// The walk is this file's own, so what it covers is asserted rather than assumed: every spec
// e2e/no-browser-rows.ts reads for the row rule is a file this rule reads too, and the helpers
// those specs import are read beside them - `touch.ts` holds a wait of its own. The two sets it
// leaves are asserted as well: `__tests__`, whose controls are written to be refused, and
// `harness-*.ts`, whose sleeps are a starter's backoff between attempts to reach a port rather
// than a row's wait on the product.
test("the scan reads every spec of the suite, and the helpers beside them", () => {
  const scanned = new Set(scannedFiles);
  expect(specFiles(e2eDirectory).filter((file) => !scanned.has(file))).toEqual([]);
  expect(scannedFiles).toContain("touch.ts");
  expect(scannedFiles.some((file) => file.startsWith("__tests__/"))).toBe(false);
  expect(scannedFiles.filter((file) => file.startsWith("harness-"))).toEqual([]);
});

// The SQL this suite hands to Postgres, which is the one text a reading by regular expression
// would take: `select pg_sleep(3)` and a `perform pg_sleep(${seconds})` inside a function body.
// Neither is a call in this process - no row waits on them here, the server's connection does -
// and the scan matches syntax rather than text, so it reports no wait in either file.
test("the SQL pg_sleep of two specs is no wait of this rule's", () => {
  const sqlFiles = ["failed-room.e2e.ts", "keyed-writes.e2e.ts"];
  for (const file of sqlFiles) {
    expect(readFileSync(join(e2eDirectory, file), "utf8")).toContain("pg_sleep");
  }
  expect(waitsOfFiles(e2eDirectory, sqlFiles)).toEqual([]);
});

test("every fixed wait of the suite says why it is one", () => {
  expect(realRefused.map((refusal) => refusal.why)).toEqual([]);
});

test("a planted wait with a reason above it is kept, and one with no comment is refused", () => {
  expect(planted(32).reason).toStartWith("The sweep runs on a clock of its own");
  expect(refusedWaits([planted(32)])).toEqual([]);

  // The check names the bare wait and leaves the one beside it that carries a reason, so a pass
  // on the real suite is a pass of a check that looks.
  const refused = refusedWaits([planted(32), planted(34)]);
  expect(refused.map((refusal) => refusal.wait.line)).toEqual([34]);
  // The refusal names the file, the line, the call, and what the writer must do with each.
  expect(refused[0].why).toStartWith(`${fixture}:34 page.waitForTimeout(200) has no reason`);
  expect(refused[0].why).toContain("on the line directly above the call");
  expect(refused[0].why).toContain("`expect.poll`");
});

// The suite's other spelling, planted both ways: a call of the name this fixture binds
// `node:timers/promises`' `setTimeout` to is a wait like any other, and the fixture's own
// `sleep`, which that import does not bind, is not one at all. The alias here is `waitOut`
// rather than the `sleep` every spec uses, so this row fails on a reading that matched the word.
test("a wait on node's timers is read from the import, and a same-named local function is not", () => {
  expect(planted(62).call).toBe("waitOut(1_000)");
  expect(planted(62).reason).toStartWith("The other spelling, under a reason");
  expect(refusedWaits([planted(62)])).toEqual([]);

  const refused = refusedWaits([planted(62), planted(64)]);
  expect(refused.map((refusal) => refusal.wait.call)).toEqual(["waitOut(1_100)"]);
  expect(refused[0].why).toStartWith(`${fixture}:64 waitOut(1_100) has no reason`);

  // The file's own `sleep(1_200)` carries no reason and is still no wait, so nothing here was
  // read by its name.
  expect(fixtureWaits.map((wait) => wait.line)).not.toContain(66);
});

// What a bare `// wait` does, which is the question the word count answers: it is refused, and
// refused for saying too little rather than for saying nothing, so the failure reads differently
// from the one above it. `// 900` is the same answer by the other half of the measure - a line
// with no word in it at all counts zero.
test("a planted wait under a bare comment is refused for what that comment leaves out", () => {
  const refused = refusedWaits([planted(37), planted(58)]);
  expect(refused.map((refusal) => refusal.wait.line)).toEqual([37, 58]);
  expect(refused[0].why).toContain(`stands under "wait", which is 1 word`);
  expect(refused[0].why).toContain(`the rule asks for ${minimumReasonWords}`);
  expect(refused[1].why).toContain(`stands under "900", which is 0 words`);
  expect(reasonWords(planted(37).reason)).toEqual(["wait"]);
  expect(reasonWords(planted(58).reason)).toEqual([]);
});

// Where the reason has to stand. A sentence one blank line further up is a sentence the reader
// of the call does not see, and one trailing the call is half a line rather than a reason; both
// are refused. A block comment that ends on the line above is a reason like any other.
test("a reason is read on the line above the call, in whichever way it is written", () => {
  expect(planted(42).reason).toBe("");
  expect(planted(48).reason).toBe("");
  expect(refusedWaits([planted(42), planted(48)]).map((refusal) => refusal.wait.line)).toEqual([
    42, 48,
  ]);

  expect(planted(46).reason).toBe(
    "A block comment is a comment: what matters is that a reason ends on the line above the " +
      "call, not which way it was written."
  );
  expect(refusedWaits([planted(46)])).toEqual([]);
});

// A wait on a later line of a bigger expression: the reason above the statement is not above the
// call, so the call is refused and the refusal tells the writer to give it a line.
test("a wait written on a later line of its statement stands under no reason", () => {
  const inExpression = planted(53);
  expect(inExpression.call).toBe("page.waitForTimeout(milliseconds)");
  expect(inExpression.reason).toBe("");
  const refused = refusedWaits([inExpression]);
  expect(refused).toHaveLength(1);
  expect(refused[0].why).toContain("giving the call a line of its own where it has none");
});

// The evidence a reader of CI records for this check. It prints the result the rule's own test
// asserts, so the line is there only when the rule held in this run.
test("the suite's waits are reported for the record", () => {
  if (realRefused.length === 0) {
    console.log(
      `fixed waits: scanned ${scannedFiles.length} files and found ${realWaits.length} ` +
        "`waitForTimeout` and timers `setTimeout` calls in " +
        `${new Set(realWaits.map((wait) => wait.file)).size} of them; every one carries a ` +
        `reason of at least ${minimumReasonWords} words on the line above it.`
    );
  }
  expect(realRefused).toEqual([]);
});
