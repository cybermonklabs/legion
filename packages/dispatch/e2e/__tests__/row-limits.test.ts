import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { minimumReasonWords } from "../fixed-waits";
import { limitsOfFiles, refusedLimits, testFiles, type WallClockLimit } from "../row-limits";

// Like e2e/__tests__/fixed-waits.test.ts, this file starts no subprocess and imports no
// Playwright config: it reads this directory from its own source, so the whole file is a parse
// of the directory's test files and states no limit of its own.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));

const scannedFiles = testFiles(e2eDirectory);
const realLimits = limitsOfFiles(e2eDirectory, scannedFiles);
// The rule is run over the directory once, here, because two tests below read the result: the
// one that asserts it and the one that prints the evidence line only when it held.
const realRefused = refusedLimits(realLimits);

/** The planted controls, read by name rather than by pointing `testFiles` at their directory:
 *  they are no test of this directory, and all but two of them are written to be refused. */
const fixture = "__tests__/fixtures/row-limits.fixture.ts";
const fixtureLimits = limitsOfFiles(e2eDirectory, [fixture]);

/** One planted limit, by the line the fixture writes it on. */
function planted(line: number): WallClockLimit {
  const found = fixtureLimits.find((limit) => limit.line === line);
  if (found === undefined) throw new Error(`${fixture} states no limit on line ${line}`);
  return found;
}

// A scan that found nothing would satisfy the rule below without reading a limit, so this is
// asserted once here rather than again in each test that reads `realLimits`. Each of the three
// shapes the directory writes is read on a real file: a `rowLimit` call, a `timeout` option, and
// a hook's second argument.
test("the scan reads files and limits of the real directory", () => {
  expect(scannedFiles.length).toBeGreaterThan(0);
  expect(realLimits.length).toBeGreaterThan(0);
  expect(realLimits.filter((limit) => limit.fromBounds).length).toBeGreaterThan(0);
  expect(realLimits.some((limit) => limit.kind === "hook")).toBe(true);
  expect(
    realLimits.some((limit) => limit.kind === "row" && !limit.fromBounds && limit.reason !== "")
  ).toBe(true);
});

// The walk is `e2e/source-files.ts`'s, which `e2e/fixed-waits.ts` and `e2e/no-browser-rows.ts`
// walk with too, and the filters `testFiles` hands it are this rule's own, so what it covers is
// asserted rather than assumed: every file `bun test` collects in this directory, and nothing
// under `fixtures/`, whose controls are written to be refused.
test("the scan reads every test file of the directory, and none of the controls beside them", () => {
  const collected = readdirSync(join(e2eDirectory, "__tests__"))
    .filter((name) => name.endsWith(".test.ts"))
    .map((name) => `__tests__/${name}`)
    .sort();
  expect(scannedFiles).toEqual(collected);
  expect(scannedFiles).toContain("__tests__/fake-envoy.test.ts");
  expect(scannedFiles.filter((file) => file.startsWith("__tests__/fixtures/"))).toEqual([]);
});

// The control for a false refusal, which belongs here rather than in either file below: a wrong
// refusal of the judge's `2_000` or of the supervisor's `30_000` leaves both those files green,
// so running them proves nothing and this row is where it would show. Both numbers are arguments
// of ordinary calls, which none of the five places a limit is written reaches, and
// `check-slow-rows.test.ts` writes whole spec files - `test(` calls and all - inside template
// literals, which the scan parses as the strings they are.
test("a number that is no row's or hook's own limit is read nowhere", () => {
  const files = ["__tests__/check-slow-rows.test.ts", "__tests__/local-instance-support.test.ts"];
  const sources = files.map((file) => readFileSync(join(e2eDirectory, file), "utf8"));
  expect(sources[0]).toContain("judge(");
  expect(sources[0]).toContain("2_000");
  expect(sources[0]).toContain("test(");
  expect(sources[1]).toContain("stop(30_000)");

  const limits = limitsOfFiles(e2eDirectory, files);
  expect(limits.length).toBeGreaterThan(0);
  expect(limits.map((limit) => limit.limit)).not.toContain("2_000");
  expect(limits.map((limit) => limit.limit)).not.toContain("30_000");
  expect(limits.filter((limit) => limit.file === files[0])).toEqual([]);
  expect(refusedLimits(limits)).toEqual([]);
});

test("every wall-clock limit of the directory is derived or says what it drives", () => {
  expect(realRefused.map((refusal) => refusal.why)).toEqual([]);
});

// One planted limit of every place a limit is written, none of them a `rowLimit` call and none
// of them under a reason: a literal, a name the file binds, a name it imports, the `timeout` of
// a row's options object, `it`, the two forms a row is reached through here, a hook's own
// second argument, a hook's options object, and the file's own `setDefaultTimeout`.
test("a limit that is neither derived nor explained is refused, wherever it is written", () => {
  const spellings = [43, 45, 47, 49, 51, 53, 55, 57, 59, 61];
  const refused = refusedLimits(spellings.map(planted));
  expect(refused.map((refusal) => refusal.limit.line)).toEqual(spellings);
  for (const refusal of refused) {
    // Every refusal names the file and the line, what states the limit, and the two ways out.
    expect(refusal.why).toStartWith(`${fixture}:${refusal.limit.line} `);
    expect(refusal.why).toContain(refusal.limit.title);
    expect(refusal.why).toContain("call `rowLimit(...)` naming the bounded children");
    expect(refusal.why).toContain("give the limit a line of its own");
  }
  expect(refused[1].why).toStartWith(
    `${fixture}:45 the row "a row whose limit is a literal" stops at 90_000, which is no call of`
  );
  expect(planted(47).limit).toBe("planted");
  expect(planted(49).limit).toBe("minimumReasonWords");
  expect(planted(51).limit).toBe("90_000");
  expect(planted(59).kind).toBe("hook");
  expect(refused[8].why).toStartWith(`${fixture}:59 the hook beforeAll stops at 60_000`);
  expect(refused[0].why).toStartWith(`${fixture}:43 the file's own setDefaultTimeout`);
});

// The one way out that needs no prose, planted both ways: a call of the name this fixture binds
// `rowLimit` to is a limit read from the bounds, and the fixture's own `rowLimit`, which that
// import does not bind, is no such call at all. The alias here is `boundedBy` rather than the
// `rowLimit` every test file uses, so this row fails on a reading that matched the word.
test("a rowLimit is read from the import, and a same-named local function is not", () => {
  expect(planted(79).limit).toBe('boundedBy("short")');
  expect(planted(79).fromBounds).toBe(true);
  expect(refusedLimits([planted(79)])).toEqual([]);

  expect(planted(73).limit).toBe("rowLimit(90_000)");
  expect(planted(73).fromBounds).toBe(false);
  const refused = refusedLimits([planted(73), planted(79)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([73]);
  expect(refused[0].why).toStartWith(
    `${fixture}:73 the row "a row whose rowLimit is this file's own" stops at rowLimit(90_000)`
  );
});

// Where the reason has to stand, and how much of a sentence it has to be. A line of its own
// above the limit is kept; `// too short` is refused for saying too little rather than for
// saying nothing, so that failure reads differently from the others; a sentence one blank line
// further up is a sentence the reader of the limit does not see; and one trailing the limit is
// the half a line that is left after it.
test("a reason is read on the line above the limit, and only where it says enough", () => {
  expect(planted(77).reason).toStartWith("This row starts a server of its own");
  expect(refusedLimits([planted(77)])).toEqual([]);

  const refused = refusedLimits([planted(64), planted(69), planted(71), planted(77)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([64, 69, 71]);
  expect(refused[0].why).toContain(`stops at 90_000 under "too short", which is 2 words`);
  expect(refused[0].why).toContain(`the rule asks for ${minimumReasonWords}`);
  expect(planted(69).reason).toBe("");
  expect(planted(71).reason).toBe("");
  expect(refused[2].why).toContain("has no reason on the line above it");
});

// The evidence a reader of CI records for this check. It prints the result the rule's own test
// asserts, so the line is there only when the rule held in this run.
test("the directory's limits are reported for the record", () => {
  if (realRefused.length === 0) {
    const derived = realLimits.filter((limit) => limit.fromBounds).length;
    console.log(
      `row limits: scanned ${scannedFiles.length} test files and found ${realLimits.length} ` +
        `wall-clock limits in ${new Set(realLimits.map((limit) => limit.file)).size} of them; ` +
        `${derived} are read from the bounds through \`rowLimit\` and the other ` +
        `${realLimits.length - derived} carry a reason of at least ${minimumReasonWords} words ` +
        "on the line above them."
    );
  }
  expect(realRefused).toEqual([]);
});
