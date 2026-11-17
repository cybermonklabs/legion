import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { minimumReasonWords, reasonWords, suiteFiles } from "../fixed-waits";
import { specFiles } from "../no-browser-rows";
import { type DecidingProbe, probesOfFiles, refusedProbes } from "../zero-wait-probes";

// Like e2e/__tests__/fixed-waits.test.ts, this file imports no Playwright config and starts no
// subprocess: it reads the suite from its own source, so the whole file is a parse of the suite.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));

const scannedFiles = suiteFiles(e2eDirectory);
const realProbes = probesOfFiles(e2eDirectory, scannedFiles);
// The rule is run over the suite once, here, because two tests below read the result: the one
// that asserts it and the one that prints the evidence line only when it held.
const realRefused = refusedProbes(realProbes);

/** The planted controls, read by name rather than by pointing `suiteFiles` at their directory:
 *  they are no part of the suite, and half of them are written to be refused. */
const fixture = "__tests__/fixtures/zero-wait-probes.fixture.ts";
const fixtureProbes = probesOfFiles(e2eDirectory, [fixture]);
const fixtureLines = readFileSync(join(e2eDirectory, fixture), "utf8").split("\n");

/** One planted probe the scan reads as deciding a path, by the line the fixture writes it on. */
function planted(line: number): DecidingProbe {
  const found = fixtureProbes.find((probe) => probe.line === line);
  if (found === undefined) throw new Error(`${fixture} holds no deciding probe on line ${line}`);
  return found;
}

// A scan over no file would satisfy the rule below without reading a probe. Whether it finds the
// probes it reads is held by the planted rows below, since the real suite may come to hold none.
test("the scan reads files of the real suite", () => {
  expect(scannedFiles.length).toBeGreaterThan(0);
});

// The files are `suiteFiles`' of e2e/fixed-waits.ts, so what they cover is asserted rather than
// assumed: every spec e2e/no-browser-rows.ts reads for the row rule is read here too, and so are
// the helpers beside them, where most probes and every wrapper of the suite stand. The two sets it
// leaves are asserted as well: `__tests__`, whose controls are written to be refused, and
// `harness-*.ts`, the run's starter.
test("the scan reads every spec of the suite, and the helpers beside them", () => {
  const scanned = new Set(scannedFiles);
  expect(specFiles(e2eDirectory).filter((file) => !scanned.has(file))).toEqual([]);
  expect(scannedFiles).toContain("rail.ts");
  expect(scannedFiles.some((file) => file.startsWith("__tests__/"))).toBe(false);
  expect(scannedFiles.filter((file) => file.startsWith("harness-"))).toEqual([]);
});

test("every probe of the suite that decides a path reads a settled state or says why", () => {
  expect(realRefused.map((refusal) => refusal.why)).toEqual([]);
});

// Every shape the rule refuses, each read at its line: an `if`, a `const` read later in a
// ternary, `test.skip`, a `for` loop's bound, a wrapper called in an `if`, and an `if` under a
// comment too short to be a reason. Nothing else of the fixture is refused, so none of the shapes
// planted to pass is.
test("each planted refusal is refused at its line, for what its answer decides", () => {
  expect(
    refusedProbes(fixtureProbes).map(({ probe }) => [probe.line, probe.name, probe.position])
  ).toEqual([
    [30, "isVisible", "if"],
    [32, "textContent", "const shown -> ternary@33"],
    [35, "count", "test.skip"],
    [37, "count", "for"],
    [41, "menuShows", "if"],
    [44, "count", "if"],
  ]);

  // The refusal names the file, the line, the probe, what it decides, and both ways out.
  const [bare] = refusedProbes([planted(30)]);
  expect(bare.why).toStartWith(
    `${fixture}:30 isVisible (if) decides a path on one instant's answer and has no reason on ` +
      "the line above it: "
  );
  expect(bare.why).toContain("`settledState` of `e2e/settled.ts`");
  expect(bare.why).toContain(`at least ${minimumReasonWords} words`);
  expect(bare.why).toContain("on the line directly above the probe");
});

// What a short comment does, which is the question the word count answers: seven words is one
// short of the measure, so the probe is refused for saying too little rather than for saying
// nothing, and the failure reads differently from the one above it.
test("a planted probe under a comment of seven words is refused for what it leaves out", () => {
  expect(planted(44).reason).toBe("One read of this instant is enough.");
  expect(reasonWords(planted(44).reason)).toHaveLength(minimumReasonWords - 1);
  const [short] = refusedProbes([planted(44)]);
  expect(short.why).toContain(
    'stands under "One read of this instant is enough.", which is 7 words where the rule asks ' +
      `for ${minimumReasonWords}`
  );
});

// The same probe under one word more is kept: it still decides a path, and its writer said why.
test("a planted probe under a reason of eight words is kept", () => {
  expect(planted(72).name).toBe("isChecked");
  expect(planted(72).position).toBe("if");
  expect(reasonWords(planted(72).reason)).toHaveLength(minimumReasonWords);
  expect(refusedProbes([planted(72)])).toEqual([]);
});

// The shapes planted to pass as no deciding probe at all: one read again by `expect.poll` or by
// `expect(...).toPass` until it holds, one the page runs in `page.evaluate`, a `getAttribute`
// whose `if` only throws, read through a `const` and in place, one handed to `expect(...)`, and
// the probe a wrapper returns. Each line is held to the probe it plants, so a fixture edit that
// moved one cannot pass for the scan skipping it.
test("a probe a retry or the page reads, or that decides no path, is no instance", () => {
  const passes: [number, string][] = [
    [23, "return menu.isVisible();"],
    [54, "await expect.poll(async () => ((await rows.count()) > 0"],
    [57, "expect((await menu.inputValue()) === "],
    [60, "await page.evaluate(() => (document.body.getAttribute("],
    [62, "const state = await menu.getAttribute("],
    [65, "if ((await menu.getAttribute("],
    [69, "expect(await rows.count()).toBe(2);"],
  ];
  for (const [line, text] of passes) expect(fixtureLines[line - 1]).toContain(text);
  expect(fixtureProbes.map((probe) => probe.line)).toEqual([30, 32, 35, 37, 41, 44, 72]);
});

// `Promise.all` is named like `locator.all()`, but it waits for the promises it is handed and
// reads no page, so a loop over its answer, in place or through a `const`, is no instance. Each
// line is held to the loop it plants, so a fixture edit that moved one cannot pass for the scan
// skipping it.
test("a loop over `Promise.all` is no instance", () => {
  const passes: [number, string][] = [
    [79, "for (const box of await Promise.all(["],
    [83, "const writes = Promise.all(["],
    [85, "for (const request of await writes)"],
  ];
  for (const [line, text] of passes) expect(fixtureLines[line - 1]).toContain(text);
  expect(fixtureProbes.filter((probe) => probe.line >= 79)).toEqual([]);
});

// The evidence a reader of CI records for this check. It prints the result the rule's own test
// asserts, so the line is there only when the rule held in this run.
test("the suite's deciding probes are reported for the record", () => {
  if (realRefused.length === 0) {
    console.log(
      `zero-wait probes: scanned ${scannedFiles.length} files and found ${realProbes.length} ` +
        "probes whose answer decides a path; every one carries a reason of at least " +
        `${minimumReasonWords} words on the line above it.`
    );
  }
  expect(realRefused).toEqual([]);
});
