import { expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
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
 *  they are no test of this directory, and all but six of them are written to be refused. */
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
// a hook's second argument; and so is a row that waits for a bounded child, which states its
// limit as a `rowLimit` call.
test("the scan reads files and limits of the real directory", () => {
  expect(scannedFiles.length).toBeGreaterThan(0);
  expect(realLimits.length).toBeGreaterThan(0);
  expect(realLimits.filter((limit) => limit.stated?.fromBounds).length).toBeGreaterThan(0);
  expect(realLimits.some((limit) => limit.kind === "hook")).toBe(true);
  expect(
    realLimits.some(
      (limit) =>
        limit.kind === "row" && limit.stated?.fromBounds === false && limit.stated.reason !== ""
    )
  ).toBe(true);
  expect(realLimits.some((limit) => limit.waitsFor.length > 0 && limit.stated?.fromBounds)).toBe(
    true
  );
});

/** The extensions `bun test` 1.4.4 collects a test file under, in any case, measured on
 *  2026-11-12 as `collectedTestFile` of `e2e/row-limits.ts` records. */
const collectedExtensions = new Set(["ts", "tsx", "js", "jsx", "mjs", "cjs", "mts", "cts"]);

/** The endings of a name, before its extension, that make it a test file under that measure. */
const collectedEndings = [".test", "_test", ".spec", "_spec"];

/** Whether `bun test` collects a file of this name, read by splitting the name at its last dot
 *  rather than by the pattern `e2e/row-limits.ts` matches, so this row checks that pattern
 *  against a second reading of the same measurement. */
function isCollected(name: string): boolean {
  const lower = name.toLowerCase();
  const dot = lower.lastIndexOf(".");
  if (dot === -1 || !collectedExtensions.has(lower.slice(dot + 1))) return false;
  const stem = lower.slice(0, dot);
  return collectedEndings.some((ending) => stem.endsWith(ending));
}

/** Every file under `relative` that `bun test` collects, at any depth, in every directory below
 *  it but `node_modules` and one whose name begins with a dot, which bun 1.4.4 skips. Paths are
 *  relative to `root`, as the scan names them. The walk is written out here, with `node:fs`
 *  alone, so this row reads the directory itself and borrows no code from the rule it checks. */
function collectedFiles(root: string, relative: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(join(root, relative), { withFileTypes: true })) {
    const path = `${relative}/${entry.name}`;
    if (entry.isDirectory()) {
      if (entry.name !== "node_modules" && !entry.name.startsWith(".")) {
        files.push(...collectedFiles(root, path));
      }
    } else if (entry.isFile() && isCollected(entry.name)) {
      files.push(path);
    }
  }
  return files;
}

// The walk is `e2e/source-files.ts`'s, which `e2e/fixed-waits.ts` and `e2e/no-browser-rows.ts`
// walk with too, and the filters `testFiles` hands it are this rule's own, so what it covers is
// asserted rather than assumed: every file `bun test` collects in this directory, at any depth
// below it, `fixtures/` included, and no file beside them. The set this row expects is walked
// above, not asked of the rule.
test("the scan reads every test file of the directory, wherever it stands", () => {
  const collected = collectedFiles(e2eDirectory, "__tests__").sort();
  expect(scannedFiles).toEqual(collected);
  expect(scannedFiles).toContain("__tests__/fake-envoy.test.ts");
});

// The controls of `fixtures/` are left out because no name there carries a `test` or `spec`
// token, and each is read from the directory's own listing, so a control added there is held to
// that too: one whose name `bun test` would collect runs on the lane as a test file of its own.
test("the scan leaves out every control of fixtures/", () => {
  const controls = readdirSync(join(e2eDirectory, "__tests__/fixtures"));
  expect(controls).toContain("row-limits.fixture.ts");
  expect(controls.filter(isCollected)).toEqual([]);
  expect(scannedFiles.filter((file) => file.startsWith("__tests__/fixtures/"))).toEqual([]);
});

// `bun test <directory>` collects more than `*.test.ts`: a `.spec` or `_spec` ending and a
// `_test` one, under any of eight extensions and in any case, run on the lane like any other test
// file. This row plants a bare `90_000` under one name of each such spelling, and the same row
// under names `bun test` does not collect and in the directories it does not go down, and proves
// the rule reads and refuses exactly the first set. The `.tsx` and `.jsx` rows stand below a JSX
// element whose text holds one backtick: read as TypeScript, that backtick opens a template
// literal that swallows the row, so a refusal on line 3 is a file parsed as what it is.
test("the scan reads every spelling of a test file the lane collects", () => {
  const root = mkdtempSync(join(tmpdir(), "row-limits-spellings-"));
  try {
    const esm = 'import { test } from "bun:test";\ntest("planted", () => {}, 90_000);\n';
    const cjs = 'const { test } = require("bun:test");\ntest("planted", () => {}, 90_000);\n';
    const jsx =
      'import { test } from "bun:test";\n' +
      "const element = <p>one ` tick</p>;\n" +
      'test("planted", () => element, 90_000);\n';
    const plants: [string, string][] = [
      ["__tests__/zz.spec.ts", esm],
      ["__tests__/zz_test.ts", esm],
      ["__tests__/zz_spec.ts", esm],
      ["__tests__/zz.test.tsx", jsx],
      ["__tests__/zz.spec.jsx", jsx],
      ["__tests__/zz.test.js", esm],
      ["__tests__/zz.spec.cjs", cjs],
      ["__tests__/zz_test.mts", esm],
      ["__tests__/zz.TEST.ts", esm],
      ["__tests__/zz.fixture.ts", esm],
      ["__tests__/test.ts", esm],
      ["__tests__/zz.tests.ts", esm],
      ["__tests__/zz-test.ts", esm],
      ["__tests__/zz.test.d.ts", esm],
      ["__tests__/node_modules/zz.test.ts", esm],
      ["__tests__/.hidden/zz.test.ts", esm],
    ];
    for (const [file, source] of plants) {
      mkdirSync(join(root, file, ".."), { recursive: true });
      writeFileSync(join(root, file), source);
    }
    const read = [
      "__tests__/zz.TEST.ts",
      "__tests__/zz.spec.cjs",
      "__tests__/zz.spec.jsx",
      "__tests__/zz.spec.ts",
      "__tests__/zz.test.js",
      "__tests__/zz.test.tsx",
      "__tests__/zz_spec.ts",
      "__tests__/zz_test.mts",
      "__tests__/zz_test.ts",
    ];
    expect(testFiles(root)).toEqual(read);
    expect(collectedFiles(root, "__tests__").sort()).toEqual(read);

    const refused = refusedLimits(limitsOfFiles(root, testFiles(root)));
    const jsxFiles = new Set(["__tests__/zz.spec.jsx", "__tests__/zz.test.tsx"]);
    expect(refused.map((refusal) => `${refusal.limit.file}:${refusal.limit.line}`)).toEqual(
      read.map((file) => `${file}:${jsxFiles.has(file) ? 3 : 2}`)
    );
    for (const refusal of refused) {
      expect(refusal.why).toStartWith(`${refusal.limit.file}:${refusal.limit.line} the row `);
      expect(refusal.why).toContain("stops at 90_000, which is no call of `rowLimit`");
    }
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
});

// The lane is `bun test --parallel --timeout 60000 e2e/__tests__`, and `bun test <directory>`
// collects a `*.test.ts` at any depth below it, so a row written one directory down runs on the
// lane like any other. This row plants the same bare `90_000` at the top of a scratch
// `__tests__`, one directory below it, and under its `fixtures/`, and proves the rule reads all
// three files and refuses the limit in each.
test("the scan reads a test file of a subdirectory, which the lane runs too", () => {
  const root = mkdtempSync(join(tmpdir(), "row-limits-reach-"));
  try {
    const planted = 'import { test } from "bun:test";\ntest("planted", () => {}, 90_000);\n';
    for (const file of [
      "__tests__/top.test.ts",
      "__tests__/sub/nested.test.ts",
      // `fixtures/` is walked like every other directory: a `*.test.ts` planted there runs on
      // the lane, so the rule reads it.
      "__tests__/fixtures/planted.test.ts",
    ]) {
      mkdirSync(join(root, file, ".."), { recursive: true });
      writeFileSync(join(root, file), planted);
    }
    expect(testFiles(root)).toEqual([
      "__tests__/fixtures/planted.test.ts",
      "__tests__/sub/nested.test.ts",
      "__tests__/top.test.ts",
    ]);
    const refused = refusedLimits(limitsOfFiles(root, testFiles(root)));
    expect(refused.map((refusal) => refusal.limit.file)).toEqual([
      "__tests__/fixtures/planted.test.ts",
      "__tests__/sub/nested.test.ts",
      "__tests__/top.test.ts",
    ]);
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
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
  expect(limits.map((limit) => limit.stated?.text)).not.toContain("2_000");
  expect(limits.map((limit) => limit.stated?.text)).not.toContain("30_000");
  expect(limits.filter((limit) => limit.file === files[0])).toEqual([]);
  expect(refusedLimits(limits)).toEqual([]);
});

test("every wall-clock limit of the directory is derived or says what it drives", () => {
  expect(realRefused.map((refusal) => refusal.why)).toEqual([]);
});

// Each limit of this fixture is written in a spelling `bun test` honours beyond a bare `test`,
// `it`, hook and `setDefaultTimeout`, and the fixture's header carries the measurement behind
// each. R5 of the spec asks for the limit "in any spelling Bun accepts", so each one is read and
// refused on its own line: `jest.setTimeout`, a renamed `test`, a renamed `setDefaultTimeout`,
// `onTestFinished`, an options object named before the body, a row reached through a `const`
// bound to a `const` bound to `test`, and rows reached through a name destructured off
// `await import("bun:test")`, a name bound to it, and a name bound to `require("bun:test")`.
test("a limit Bun honours under a spelling of its own is refused like any other", () => {
  const spellings = "__tests__/fixtures/row-limits-spellings.fixture.ts";
  const lines = [26, 28, 30, 33, 38, 42, 45, 48, 51];
  const refused = refusedLimits(limitsOfFiles(e2eDirectory, [spellings]));
  expect(refused.map((refusal) => refusal.limit.line)).toEqual(lines);
  expect(refused.map((refusal) => refusal.limit.kind)).toEqual([
    "file",
    "row",
    "file",
    "hook",
    "row",
    "row",
    "row",
    "row",
    "row",
  ]);
  for (const refusal of refused) {
    expect(refusal.why).toStartWith(`${spellings}:${refusal.limit.line} `);
  }
  expect(refused[3].why).toStartWith(`${spellings}:33 the hook onTestFinished stops at 90_000`);
  expect(refused[4].why).toStartWith(
    `${spellings}:38 the row "a row whose options are a name" stops at options under`
  );
  expect(refused[5].why).toStartWith(
    `${spellings}:42 the row "a row declared through a name bound to a name bound to \`test\`"`
  );
  expect(refused[6].why).toStartWith(
    `${spellings}:45 the row "a row declared through a name destructured off the module"`
  );
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
  expect(planted(47).stated?.text).toBe("planted");
  expect(planted(49).stated?.text).toBe("minimumReasonWords");
  expect(planted(51).stated?.text).toBe("90_000");
  expect(planted(59).kind).toBe("hook");
  expect(refused[8].why).toStartWith(`${fixture}:59 the hook beforeAll stops at 60_000`);
  expect(refused[0].why).toStartWith(`${fixture}:43 the file's own setDefaultTimeout`);
});

// A modifier of `test` written as a plain property access - `skip` here, and `only`, `todo` and
// `failing` beside it - declares a row Bun takes a limit from like any other, and a reading that
// knew `test` and `test.skipIf(<x>)` alone passed every one of them over. The callee is walked
// down to the identifier it is reached from, so this plant stands for every modifier at once.
test("a limit stated through a modifier of `test` is read like any other row's", () => {
  expect(planted(81).kind).toBe("row");
  expect(planted(81).stated?.text).toBe("90_000");
  const refused = refusedLimits([planted(81)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([81]);
  expect(refused[0].why).toStartWith(
    `${fixture}:81 the row "a row declared through \`skip\`" stops at 90_000, which is no call of`
  );
});

// The one way out that needs no prose, planted both ways: a call of the name this fixture binds
// `rowLimit` to is a limit read from the bounds, and the fixture's own `rowLimit`, which that
// import does not bind, is no such call at all. The alias here is `boundedBy` rather than the
// `rowLimit` every test file uses, so this row fails on a reading that matched the word.
test("a rowLimit is read from the import, and a same-named local function is not", () => {
  expect(planted(79).stated?.text).toBe('boundedBy("short")');
  expect(planted(79).stated?.fromBounds).toBe(true);
  expect(refusedLimits([planted(79)])).toEqual([]);

  expect(planted(73).stated?.text).toBe("rowLimit(90_000)");
  expect(planted(73).stated?.fromBounds).toBe(false);
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
  expect(planted(77).stated?.reason).toStartWith("This row starts a server of its own");
  expect(refusedLimits([planted(77)])).toEqual([]);

  const refused = refusedLimits([planted(64), planted(69), planted(71), planted(77)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([64, 69, 71]);
  expect(refused[0].why).toContain(`stops at 90_000 under "too short", which is 2 words`);
  expect(refused[0].why).toContain(`the rule asks for ${minimumReasonWords}`);
  expect(planted(69).stated?.reason).toBe("");
  expect(planted(71).stated?.reason).toBe("");
  expect(refused[2].why).toContain("has no reason on the line above it");
});

// A row that waits for a bounded child and states no limit runs under the lane's `--timeout
// 60000`, which ties with the `short` bound and falls below every other, so the row's clock can
// fire first and the failure names no child. The fixture plants a row that waits through a
// function of its own and states no limit, a hook that calls `runBounded` itself and states
// none, a row that waits under a number and a reason, and a row that states `rowLimit`, which is
// the one of the four that passes: a reason is the way out for a row that drives a process.
test("a row or hook that waits for a bounded child states its limit as a rowLimit call", () => {
  expect(planted(89).stated).toBeUndefined();
  expect(planted(89).waitsFor).toEqual(["`startsAChild` (which waits for `runBounded`)"]);
  expect(planted(93).kind).toBe("hook");
  expect(planted(93).waitsFor).toEqual(["`runBounded`"]);
  expect(planted(97).stated?.reason).toStartWith("This row starts a server of its own");
  expect(planted(99).waitsFor).toEqual(["`runBounded`"]);
  expect(refusedLimits([planted(99)])).toEqual([]);

  const refused = refusedLimits([planted(89), planted(93), planted(97), planted(99)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([89, 93, 97]);
  expect(refused[0].why).toStartWith(
    `${fixture}:89 the row "a row that waits for a bounded child and states no limit" waits ` +
      "for `startsAChild` (which waits for `runBounded`) and states no limit"
  );
  expect(refused[1].why).toStartWith(
    `${fixture}:93 the hook afterEach waits for \`runBounded\` and states no limit`
  );
  expect(refused[2].why).toStartWith(
    `${fixture}:97 the row "a row that waits for a bounded child under a reason" stops at ` +
      "90_000, which is no call of `rowLimit`, and waits for `runBounded`"
  );
  for (const refusal of refused) {
    expect(refusal.why).toContain("state `rowLimit(...)` naming the bounded children");
  }
});

// Bun applies a file's own `setDefaultTimeout` to every row and hook of the file, so a `rowLimit`
// there is the limit of each waiting row that states none, and a number there is not. The file
// is planted in a scratch directory because the fixture's own `setDefaultTimeout` is a number.
test("a file's own rowLimit states the limit of each waiting row that states none", () => {
  const root = mkdtempSync(join(tmpdir(), "row-limits-file-wide-"));
  try {
    const file = "__tests__/waits.test.ts";
    const source = (fileLimit: string): string =>
      [
        'import { setDefaultTimeout, test } from "bun:test";',
        'import { rowLimit, runBounded } from "./bounded-subprocess";',
        `setDefaultTimeout(${fileLimit});`,
        'test("waits", () => runBounded({ argv: ["true"], bound: "short", from: "" }));',
        "",
      ].join("\n");
    mkdirSync(join(root, "__tests__"));
    writeFileSync(join(root, file), source('rowLimit("short")'));
    expect(refusedLimits(limitsOfFiles(root, [file]))).toEqual([]);

    writeFileSync(join(root, file), source("90_000"));
    const refused = refusedLimits(limitsOfFiles(root, [file]));
    expect(refused.map((refusal) => refusal.limit.line)).toEqual([3, 4]);
    expect(refused[1].why).toStartWith(
      `${file}:4 the row "waits" waits for \`runBounded\` and states no limit`
    );
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
});

// The shape the listing files write: a listing started at module load,
// `const lane = retained(listing(...))`, and awaited in a row, whose body then holds no call of
// `listing` at all. The fixture plants that name awaited by a row that states no limit, by a row
// under a number and a reason, and by a row under `rowLimit("listing")`, the one of the three that
// passes; and a row that names `runBounded` as a value and calls nothing, which waits for nothing
// and is recorded nowhere, because a bare reference to a bounded export is no wait.
test("a row that awaits a name bound to a listing waits for that listing", () => {
  const waits = ["`startedListing` (which waits for `listing`)"];
  expect(planted(109).stated).toBeUndefined();
  expect(planted(109).waitsFor).toEqual(waits);
  expect(planted(115).stated?.reason).toStartWith("This row starts a server of its own");
  expect(planted(115).waitsFor).toEqual(waits);
  expect(planted(135).waitsFor).toEqual(waits);
  expect(planted(135).bound).toBe("listing");
  expect(fixtureLimits.filter((limit) => limit.line === 140)).toEqual([]);

  const refused = refusedLimits([planted(109), planted(115), planted(135)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([109, 115]);
  expect(refused[0].why).toStartWith(
    `${fixture}:109 the row "a row that awaits a listing started at module load and states no ` +
      'limit" waits for `startedListing` (which waits for `listing`) and states no limit'
  );
  expect(refused[1].why).toStartWith(
    `${fixture}:115 the row "a row that awaits a module-load listing under a reason" stops at ` +
      "90_000, which is no call of `rowLimit`, and waits for `startedListing` (which waits for " +
      "`listing`)"
  );
});

// The same shape with the listing started by a hook: `let assigned;` and
// `beforeAll(() => { assigned = retained(listing(...)); }, rowLimit("listing"))`. The hook calls
// `listing` itself, so it waits for that listing and states `rowLimit("listing")`, and the name
// it writes to is no wait of its. A row that awaits the assigned name waits for the listing as a
// row that awaits a bound one does: under no limit it is refused, under `rowLimit("listing")` it
// passes, and under `rowLimit("short")` it falls short of the `listing` bound.
test("a row that awaits a name assigned to a listing waits for that listing", () => {
  const waits = ["`assigned` (which waits for `listing`)"];
  expect(planted(149).kind).toBe("hook");
  expect(planted(149).waitsFor).toEqual(["`listing`"]);
  expect(planted(149).bound).toBe("listing");
  expect(planted(151).stated).toBeUndefined();
  expect(planted(151).waitsFor).toEqual(waits);
  expect(planted(160).waitsFor).toEqual(waits);
  expect(planted(160).bound).toBe("listing");
  expect(planted(168).bound).toBe("listing");

  const refused = refusedLimits([planted(149), planted(151), planted(160), planted(168)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([151, 168]);
  expect(refused[0].why).toStartWith(
    `${fixture}:151 the row "a row that awaits an assigned listing and states no limit" waits ` +
      "for `assigned` (which waits for `listing`) and states no limit"
  );
  expect(refused[1].why).toStartWith(
    `${fixture}:168 the row "a row that awaits an assigned listing under the short bound" stops ` +
      'at boundedBy("short"), 90000 ms, and waits for `assigned` (which waits for `listing`), ' +
      'under the `listing` bound of 300000 ms: the row needs `rowLimit("listing")`'
  );
});

// The listing files of this directory are the incident's own shape, so the reading above is held
// over them too: each row of theirs that states a `rowLimit` awaits a listing started at module
// load, and each is read as waiting for it, under the `listing` bound.
test("the rows of the listing files are read as waiting for their listings", () => {
  const listingFiles = [
    "__tests__/no-browser-rows.test.ts",
    "__tests__/pull-request-lane.test.ts",
    "__tests__/row-configurations.test.ts",
  ];
  const rows = realLimits.filter(
    (limit) => listingFiles.includes(limit.file) && limit.stated?.fromBounds
  );
  expect(rows.map((limit) => limit.file)).toEqual([
    "__tests__/no-browser-rows.test.ts",
    "__tests__/pull-request-lane.test.ts",
    "__tests__/pull-request-lane.test.ts",
    "__tests__/row-configurations.test.ts",
    "__tests__/row-configurations.test.ts",
  ]);
  for (const row of rows) {
    expect(row.kind).toBe("row");
    expect(row.bound).toBe("listing");
    expect(row.waitsFor.length).toBeGreaterThan(0);
    for (const wait of row.waitsFor) expect(wait).toEndWith("(which waits for `listing`)");
  }
});

// A `rowLimit` is held to the bound of what its row waits for, where the scan reads that bound:
// the fixture plants a `listing` call and a `runBounded` naming the `listing` bound, each under
// `rowLimit("short")`, which falls short of it, beside the row under `rowLimit("short")` that
// waits for a `short` child and the row under `rowLimit("listing")` that awaits a listing.
test("a rowLimit below the bound of what its row waits for is refused", () => {
  expect(planted(122).waitsFor).toEqual(["`listing`"]);
  expect(planted(122).bound).toBe("listing");
  expect(planted(122).stated?.total).toBe(90_000);
  expect(planted(129).bound).toBe("listing");
  expect(planted(99).bound).toBe("short");

  const refused = refusedLimits([planted(99), planted(122), planted(129), planted(135)]);
  expect(refused.map((refusal) => refusal.limit.line)).toEqual([122, 129]);
  expect(refused[0].why).toStartWith(
    `${fixture}:122 the row "a row that calls a listing under the short bound" stops at ` +
      'boundedBy("short"), 90000 ms, and waits for `listing`, under the `listing` bound of ' +
      '300000 ms: the row needs `rowLimit("listing")`, 330000 ms, at the least'
  );
  expect(refused[1].why).toStartWith(
    `${fixture}:129 the row "a row whose runBounded names a bound its rowLimit falls short of" ` +
      'stops at boundedBy("short"), 90000 ms, and waits for `runBounded`, under the `listing` bound'
  );
});

// A file's own `rowLimit` stands for each waiting row of the file that states none, so it is
// held to those rows' bounds as a row's own is. Where the scan reads no total - a spread
// argument - or no bound - a `bound:` the call computes - the names are not checked, and the
// limit is taken as stated. Each shape is a file of its own in a scratch directory, because the
// fixture's own `setDefaultTimeout` is a number.
test("a file's own rowLimit is held to the bound of each waiting row it stands for", () => {
  const root = mkdtempSync(join(tmpdir(), "row-limits-bound-"));
  try {
    mkdirSync(join(root, "__tests__"));
    const plant = (name: string, lines: readonly string[]): string => {
      const file = `__tests__/${name}.test.ts`;
      const header = [
        'import { setDefaultTimeout, test } from "bun:test";',
        'import { rowLimit, runBounded } from "./bounded-subprocess";',
        'import { listing, retained } from "./playwright-listing";',
        'const lane = retained(listing(import.meta.url, "e2e/playwright.config.ts", {}));',
      ];
      writeFileSync(join(root, file), [...header, ...lines, ""].join("\n"));
      return file;
    };
    const awaiting = ['test("awaits", async () => {', "  await lane;", "});"];
    const short = plant("short", ['setDefaultTimeout(rowLimit("short"));', ...awaiting]);
    const long = plant("long", ['setDefaultTimeout(rowLimit("listing"));', ...awaiting]);
    const unread = plant("unread", [
      'test("spread", () => lane, rowLimit(...(["short"] as const)));',
      'const chosen = "listing";',
      'const run = () => runBounded({ argv: [], bound: chosen, from: "" });',
      'test("computed", () => run(), rowLimit("short"));',
    ]);

    const refused = refusedLimits(limitsOfFiles(root, [short, long, unread]));
    expect(refused.map((refusal) => `${refusal.limit.file}:${refusal.limit.line}`)).toEqual([
      `${short}:6`,
    ]);
    expect(refused[0].why).toStartWith(
      `${short}:6 the row "awaits" states no limit of its own, so the file's own ` +
        'setDefaultTimeout on line 5 stops it at rowLimit("short"), 90000 ms, and waits for ' +
        "`lane` (which waits for `listing`), under the `listing` bound of 300000 ms"
    );
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
});

// A file reaches the bounded modules through `import()` and `require` as well as an import
// declaration. The fixture plants a name destructured off `await import(...)`; this plants a
// namespace bound to `await import(...)` and a name destructured off `require(...)` under a name
// of its own, each called by a row that states no limit.
test("a bounded export reached through import() or require is read as an imported one", () => {
  const root = mkdtempSync(join(tmpdir(), "row-limits-loaded-"));
  try {
    const file = "__tests__/loaded.test.ts";
    mkdirSync(join(root, "__tests__"));
    writeFileSync(
      join(root, file),
      [
        'import { test } from "bun:test";',
        'const bounded = await import("./bounded-subprocess");',
        'const { listing: list } = require("./playwright-listing");',
        'test("namespace", () => bounded.runBounded({ argv: [], bound: "short", from: "" }));',
        'test("required", () => list(import.meta.url, "e2e/playwright.config.ts", {}));',
        "",
      ].join("\n")
    );
    const limits = limitsOfFiles(root, [file]);
    expect(limits.map((limit) => limit.waitsFor)).toEqual([["`bounded.runBounded`"], ["`list`"]]);
    expect(limits.map((limit) => limit.bound)).toEqual(["short", "listing"]);
    expect(refusedLimits(limits).map((refusal) => refusal.limit.line)).toEqual([4, 5]);
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
});

// The evidence a reader of CI records for this check. It prints the result the rule's own test
// asserts, so the line is there only when the rule held in this run.
test("the directory's limits are reported for the record", () => {
  if (realRefused.length === 0) {
    const stated = realLimits.filter((limit) => limit.stated !== undefined);
    const derived = stated.filter((limit) => limit.stated?.fromBounds).length;
    console.log(
      `row limits: scanned ${scannedFiles.length} test files and found ${stated.length} ` +
        `wall-clock limits in ${new Set(stated.map((limit) => limit.file)).size} of them; ` +
        `${derived} are read from the bounds through \`rowLimit\` and the other ` +
        `${stated.length - derived} carry a reason of at least ${minimumReasonWords} words ` +
        "on the line above them."
    );
  }
  expect(realRefused).toEqual([]);
});
