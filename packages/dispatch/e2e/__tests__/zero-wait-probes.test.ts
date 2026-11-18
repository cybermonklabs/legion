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
// ternary, `test.skip`, a `for` loop's bound, a wrapper called in an `if`, an `if` under a
// comment too short to be a reason, the two `if`s whose only branch throws, the branches that are
// no `if`, and then one probe per position of the grammar, per binding, per read of a name
// written above or beside its binding, per step of the climb, per way a name's value reaches a
// position, per form of a call whose result decides, in place and through a name, per read of
// the evaluate family and of `boundingBox`, and a compound assignment that reads a bound name,
// which the rows below read one group at a time. Nothing else of the fixture is refused, so none
// of the shapes planted to pass is.
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
    [62, "getAttribute", "const state -> if@63"],
    [65, "getAttribute", "if"],
    [94, "getAttribute", "switch"],
    [101, "textContent", "const label -> switch@102"],
    [109, "isVisible", "&&"],
    [111, "count", "&&"],
    [111, "isChecked", "&&"],
    [114, "isVisible", "= shown -> if@115"],
    [120, "isVisible", "= open -> while@118"],
    [138, "isVisible", "if"],
    [139, "isHidden", "while"],
    [142, "isChecked", "do-while"],
    [143, "isEnabled", "for"],
    [144, "isEditable", "ternary"],
    [145, "count", "switch"],
    [152, "textContent", "case"],
    [157, "all", "for-of"],
    [158, "all", "for-in"],
    [159, "isVisible", "&&"],
    [160, "isChecked", "||"],
    [161, "getAttribute", "??"],
    [162, "isVisible", "let opened -> ||=@163"],
    [164, "getAttribute", "?."],
    [165, "all", "default"],
    [173, "isVisible", "&&"],
    [174, "isChecked", "||"],
    [175, "textContent", "??"],
    [182, "isVisible", "const visible -> if@183"],
    [184, "isEnabled", "let enabled -> if@185"],
    [187, "isChecked", "= checked -> if@188"],
    [190, "count", "+= total -> if@191"],
    [193, "isEditable", "&&= ready -> &&=@193,if@194"],
    [196, "isHidden", "||= anyHidden -> ||=@196,if@197"],
    [199, "textContent", "??= text -> ??=@199,if@200"],
    [201, "allTextContents", "const firstText -> if@202"],
    [203, "all", "const length -> if@204"],
    [206, "all", "= head -> if@207"],
    [209, "all", "= size -> if@210"],
    [219, "isVisible", "param shown -> if@223"],
    [221, "isEditable", "var editable -> if@222"],
    [224, "isChecked", "default open -> if@225"],
    [236, "isVisible", "= seen -> if@235"],
    [241, "isEnabled", "const present -> if@239"],
    [249, "all", "if"],
    [250, "allTextContents", "if"],
    [251, "count", "if"],
    [252, "textContent", "if"],
    [253, "getAttribute", "if"],
    [254, "textContent", "if"],
    [263, "isVisible", "if"],
    [266, "isChecked", "= outer -> if@269"],
    [321, "isVisible", "= seen -> while@319"],
    [325, "isChecked", "var done -> while@323"],
    [341, "all", "const items -> for-of@342"],
    [343, "allTextContents", "for-of"],
    [344, "allTextContents", "const texts -> if@345"],
    [346, "isChecked", "const checked -> if@347"],
    [361, "inputValue", "const value -> if@362"],
    [363, "inputValue", "if"],
    [364, "textContent", "const text -> ?.@365"],
    [366, "textContent", "?."],
    [367, "isVisible", "const shown -> if@368"],
    [369, "isVisible", "if"],
    [370, "inputValue", "const entered -> if@371"],
    [372, "inputValue", "if"],
    [373, "allTextContents", "const texts -> if@374"],
    [375, "allTextContents", "if"],
    [376, "allTextContents", "const labels -> if@377"],
    [378, "allTextContents", "if"],
    [379, "textContent", "const title -> if@380"],
    [381, "textContent", "if"],
    [382, "inputValue", "const kind -> if@383"],
    [384, "inputValue", "if"],
    [397, "evaluateAll", "for-of"],
    [400, "evaluate", "for-of"],
    [403, "boundingBox", "for-of"],
    [406, "evaluateAll", "if"],
    [407, "evaluate", "if"],
    [408, "boundingBox", "if"],
    [423, "count", "let n -> while@424"],
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

// A branch need not be an `if`. A `switch` picks its case by the answer, read in place or
// through a `const`. A statement whose `&&` runs a click only where a probe said yes is an `if`
// written as an expression, and in a chain every operand before the last decides whether the
// rest runs. A name assigned a probe's answer decides wherever a condition reads it afterwards:
// an `if` below it, or the `while` above it that is tested again after the body assigned it. Each
// line is held to what it plants, the condition lines too, so a fixture edit that moved one
// cannot pass for the scan reading it.
test("a `switch`, a statement's `&&` and an assigned name are refused at their lines", () => {
  const lines: [number, string][] = [
    [94, "switch (await menu.getAttribute("],
    [101, "const label = await menu.textContent();"],
    [102, "switch (label) {"],
    [109, "(await menu.isVisible()) && (await menu.click());"],
    [111, "(await rows.count()) > 1 && (await menu.isChecked()) && (await menu.click());"],
    [114, "shown = await menu.isVisible();"],
    [115, "if (shown)"],
    [118, "while (!open)"],
    [120, "open = await menu.isVisible();"],
  ];
  for (const [line, text] of lines) expect(fixtureLines[line - 1]).toContain(text);
  expect(planted(94).position).toBe("switch");
  expect(planted(101).position).toBe("const label -> switch@102");
  expect(planted(109).position).toBe("&&");
  expect(planted(114).position).toBe("= shown -> if@115");
  expect(planted(120).position).toBe("= open -> while@118");
  // Both probes of the chain are read, and the click after them is no probe.
  expect(
    fixtureProbes.filter((probe) => probe.line === 111).map((probe) => [probe.name, probe.position])
  ).toEqual([
    ["count", "&&"],
    ["isChecked", "&&"],
  ]);
  const [assigned] = refusedProbes([planted(114)]);
  expect(assigned.why).toStartWith(
    `${fixture}:114 isVisible (= shown -> if@115) decides a path on one instant's answer ` +
      "and has no reason on the line above it: "
  );
});

// A probe that is only the last operand of a statement's `&&` decides nothing: the statement
// throws its answer away, and nothing after it runs on that answer. Its line is held to the
// statement it plants, so a fixture edit that moved it cannot pass for the scan skipping it.
test("a probe that is only the last operand of a statement's `&&` is no instance", () => {
  expect(fixtureLines[129 - 1]).toContain("open && (await menu.isVisible());");
  expect(fixtureProbes.filter((probe) => probe.line === 129)).toEqual([]);
});

// An `if` whose only branch is a `throw` is the condition of an `if` like any other: a zero-wait
// read that throws fails the row on one instant's answer, the same flake as a wrong branch. So a
// `getAttribute` it reads, through a `const` or in place, is refused. Each line is held to what it
// plants, so a fixture edit that moved one cannot pass for the scan reading it.
test("a probe read by an `if` whose only branch throws is refused", () => {
  const lines: [number, string][] = [
    [62, "const state = await menu.getAttribute("],
    [63, "if (state === null) throw new Error("],
    [65, "if ((await menu.getAttribute("],
    [66, "throw new Error("],
  ];
  for (const [line, text] of lines) expect(fixtureLines[line - 1]).toContain(text);
  expect(planted(62).position).toBe("const state -> if@63");
  expect(planted(65).position).toBe("if");
});

// The shapes planted to pass as no deciding probe at all: one read again by `expect.poll` or by
// `expect(...).toPass` until it holds, one the page runs in `page.evaluate`, whose own answer
// decides nothing, one handed to `expect(...)`, and the probe a wrapper returns. Each line is
// held to the probe it plants, so a fixture edit that moved one cannot pass for the scan skipping
// it. The last assertion lists every line the scan reads a deciding probe on, so no planted pass
// is read as one.
test("a probe a retry or the page reads, or that decides no path, is no instance", () => {
  const passes: [number, string][] = [
    [23, "return menu.isVisible();"],
    [54, "await expect.poll(async () => ((await rows.count()) > 0"],
    [57, "expect((await menu.inputValue()) === "],
    [60, "await page.evaluate(() => (document.body.getAttribute("],
    [69, "expect(await rows.count()).toBe(2);"],
  ];
  for (const [line, text] of passes) expect(fixtureLines[line - 1]).toContain(text);
  expect(fixtureProbes.map((probe) => probe.line)).toEqual([
    30, 32, 35, 37, 41, 44, 62, 65, 72, 94, 101, 109, 111, 111, 114, 120, 138, 139, 142, 143, 144,
    145, 152, 157, 158, 159, 160, 161, 162, 164, 165, 173, 174, 175, 182, 184, 187, 190, 193, 196,
    199, 201, 203, 206, 209, 219, 221, 224, 236, 241, 249, 250, 251, 252, 253, 254, 263, 266, 321,
    325, 341, 343, 344, 346, 361, 363, 364, 366, 367, 369, 370, 372, 373, 375, 376, 378, 379, 381,
    382, 384, 397, 400, 403, 406, 407, 408, 423,
  ]);
});

/** Holds each planted line to the text it plants and the probe on it to the position the scan
 *  reads, so a fixture edit that moved a case cannot pass for the scan reading it. A case whose
 *  position is the empty string is a line that holds no probe, such as the `if` a binding's
 *  position names. */
function expectPlanted(cases: readonly (readonly [number, string, string])[]): void {
  for (const [line, text, position] of cases) {
    expect(fixtureLines[line - 1]).toContain(text);
    if (position !== "") expect(planted(line).position).toBe(position);
  }
}

// The positions are the grammar's places where a value decides what runs next, so the fixture
// plants a probe in each: the conditions of `if`, `while`, `do...while`, `for` and `?:`, a
// `switch`'s discriminant and a `case`, the iterable of a `for...of` and a `for...in`, the left of
// an `&&`, `||` or `??` whose right operand runs code, as a statement and as a `return`, the target
// of an `||=` that reads a bound name, what a `?.` that a call follows reads, and the source of a
// destructuring whose default runs code.
test("a probe in each position where the grammar lets a value decide is refused there", () => {
  expectPlanted([
    [138, "if (await menu.isVisible())", "if"],
    [139, "while (await menu.isHidden())", "while"],
    [142, "} while (!(await menu.isChecked()));", "do-while"],
    [143, "for (let turn = 0; await menu.isEnabled(); turn += 1)", "for"],
    [144, 'const word = (await menu.isEditable()) ? "editable" : "fixed";', "ternary"],
    [145, "switch (await rows.count()) {", "switch"],
    [151, "switch (word) {", ""],
    [152, "case await menu.textContent():", "case"],
    [157, "for (const row of await rows.all())", "for-of"],
    [158, "for (const index in await rows.all())", "for-in"],
    [159, "(await menu.isVisible()) && (await menu.click());", "&&"],
    [160, "(await menu.isChecked()) || (await menu.check());", "||"],
    [161, '(await menu.getAttribute("href")) ?? (await menu.click());', "??"],
    [162, "let opened = await menu.isVisible();", "let opened -> ||=@163"],
    [163, "opened ||= await menu.click().then(() => true);", ""],
    [164, 'const relative = (await menu.getAttribute("href"))?.startsWith("/");', "?."],
    [165, "const [first = rows.first()] = await rows.all();", "default"],
    [173, "return (await menu.isVisible()) && (await menu.click());", "&&"],
    [174, "return (await menu.isChecked()) || (await menu.check());", "||"],
    [175, "return (await menu.textContent()) ?? (await menu.click());", "??"],
  ]);
});

// A probe's answer bound to a name decides where a position reads the name, however it is bound:
// a declaration, a parameter's or a pattern element's default, or an assignment of any operator
// to a name or a pattern. Each binding below is read by the `if` on the line after it, but the
// parameter's, which the body reads. A logical assignment's own target is a read as well: it
// decides whether the right side runs, on whatever the name held before.
test("a probe's answer bound to a name in each way is refused where an `if` reads it", () => {
  expectPlanted([
    [182, "const visible = await menu.isVisible();", "const visible -> if@183"],
    [183, "if (visible)", ""],
    [184, "let enabled = await menu.isEnabled();", "let enabled -> if@185"],
    [187, "checked = await menu.isChecked();", "= checked -> if@188"],
    [190, "total += await rows.count();", "+= total -> if@191"],
    [193, "ready &&= await menu.isEditable();", "&&= ready -> &&=@193,if@194"],
    [196, "anyHidden ||= await menu.isHidden();", "||= anyHidden -> ||=@196,if@197"],
    [199, "text ??= await menu.textContent();", "??= text -> ??=@199,if@200"],
    [201, "const [firstText] = await rows.allTextContents();", "const firstText -> if@202"],
    [203, "const { length } = await rows.all();", "const length -> if@204"],
    [206, "[head] = await rows.all();", "= head -> if@207"],
    [209, "({ length: size } = await rows.all());", "= size -> if@210"],
    [219, "shown = menu.isVisible()", "param shown -> if@223"],
    [221, "var editable = await menu.isEditable();", "var editable -> if@222"],
    [223, "if (await shown)", ""],
    [224, "const { open = await menu.isChecked() } = options;", "default open -> if@225"],
  ]);
});

// A position reads a name wherever its scope writes it, above the binding or below it, since the
// check reads no order of running: an `if` above an assignment in a loop's body runs after it on
// the next turn, an `if` of a function nested in the binding's runs after it when called, a loop
// above a function declared below it reads what a call of that function assigned, and a loop's
// condition reads the `var` its body declared on the turn before. A closure that assigns a name
// its outer function declares is read against that function, and an assignment's value is the
// answer itself, so an `if` on it decides by the probe.
test("a probe is refused where its name's scope reads it, wherever the read is written", () => {
  expectPlanted([
    [235, "if (seen) break;", ""],
    [236, "seen = await menu.isVisible();", "= seen -> if@235"],
    [239, "if (present) await menu.click();", ""],
    [241, "const present = await menu.isEnabled();", "const present -> if@239"],
    [263, "if ((found = await menu.isVisible()))", "if"],
    [266, "outer = await menu.isChecked();", "= outer -> if@269"],
    [269, "if (outer || found)", ""],
    [319, "while (!seen) await look();", ""],
    [320, "async function look(): Promise<void> {", ""],
    [321, "seen = await menu.isVisible();", "= seen -> while@319"],
    [323, "while (!done) {", ""],
    [325, "var done: boolean | undefined = await menu.isChecked();", "var done -> while@323"],
  ]);
});

// The climb carries the answer through what computes a value from it on the way to the
// position: a property and an element read on it, arithmetic, a template literal, `as` and a
// non-null `!`.
test("a probe reached through each step of the climb is refused at its `if`", () => {
  expectPlanted([
    [249, "if ((await rows.all()).length > 1)", "if"],
    [250, 'if ((await rows.allTextContents())[0] === "Menu")', "if"],
    [251, "if ((await rows.count()) - 1 > 0)", "if"],
    [252, 'await menu.textContent()}` === "Menu")', "if"],
    [253, 'if (((await menu.getAttribute("aria-expanded")) as string) === "true")', "if"],
    [254, "if ((await menu.textContent())!.length > 0)", "if"],
  ]);
});

// What WHAT IT DOES NOT SEE names passes: an answer passed into a function that branches on it,
// an answer handed to `expect`, a callback `forEach` runs once per element, an answer kept in a
// property, in an array literal's element and stored into an element, a name copied from a bound
// name and one computed from it, an answer that `??` only replaces with a literal, a call's
// result returned and an answer returned. Each line is held to what it plants, so a fixture edit
// that moved one cannot pass for the scan skipping it, and no line of the function or of the
// callee it hands an answer to holds a deciding probe.
test("a probe whose answer goes only through a named limit is no instance", () => {
  const passes: [number, string][] = [
    [284, "await clickWhen(menu, await menu.isVisible());"],
    [308, "if (shown) await menu.click();"],
    [285, "expect(await rows.count()).toBe(3);"],
    [286, "(await rows.all()).forEach((row) => void row.click());"],
    [287, "state.shown = await menu.isChecked();"],
    [288, "if (state.shown)"],
    [289, "const answers = [await menu.isEnabled()];"],
    [290, "if (answers[0])"],
    [292, "stored[0] = await menu.isDisabled();"],
    [293, "if (stored[0])"],
    [294, "const seen = await menu.isHidden();"],
    [295, "const copy = seen;"],
    [296, "if (copy)"],
    [297, "const value = await menu.inputValue();"],
    [298, "const trimmed = value.trim();"],
    [299, "if (trimmed)"],
    [300, 'const href = (await menu.getAttribute("href")) ?? "";'],
    [302, "if (pick) return Boolean(await menu.isEditable());"],
    [303, "return (await rows.count()) > 1;"],
  ];
  for (const [line, text] of passes) expect(fixtureLines[line - 1]).toContain(text);
  expect(fixtureProbes.filter((probe) => probe.line >= 273 && probe.line <= 309)).toEqual([]);
});

// A name bound to an answer reaches a position through the steps an answer written there takes,
// a call among them: walked through `items.slice(1)`, a `.length` compared and the left of an
// `&&` each carry it on to the position, which refuses it as it refuses the same answers in
// place, the walk over `.slice(1)` of the answer in place beside its bound form. Each line is
// held to what it plants, so a fixture edit that moved one cannot pass for the scan reading it.
test("a bound name reaches a position wherever an answer written in its place would", () => {
  expectPlanted([
    [341, "const items = await rows.all();", "const items -> for-of@342"],
    [342, "for (const item of items.slice(1))", ""],
    [343, "for (const text of (await rows.allTextContents()).slice(1))", "for-of"],
    [344, "const texts = await rows.allTextContents();", "const texts -> if@345"],
    [345, "if (texts.length > 0)", ""],
    [346, "const checked = await menu.isChecked();", "const checked -> if@347"],
    [347, "if (checked && open)", ""],
  ]);
});

// A call on the answer whose result decides is refused in both forms, on a name bound to the
// answer and on the answer in place: a method called on it, an optional call on it, `Boolean` of
// it, an argument of a call, of a `new` and of a spread, a tagged template it is written into,
// and a key. Each line is held to what it plants, so a fixture edit that moved one cannot pass
// for the scan reading it.
test("a call whose result decides is refused through a bound name and in place", () => {
  expectPlanted([
    [361, "const value = await menu.inputValue();", "const value -> if@362"],
    [362, 'if (value.includes("B"))', ""],
    [363, 'if ((await menu.inputValue()).includes("B"))', "if"],
    [364, "const text = await menu.textContent();", "const text -> ?.@365"],
    [365, 'if (text?.includes("B"))', ""],
    [366, 'if ((await menu.textContent())?.includes("B"))', "?."],
    [367, "const shown = await menu.isVisible();", "const shown -> if@368"],
    [368, "if (Boolean(shown))", ""],
    [369, "if (Boolean(await menu.isVisible()))", "if"],
    [370, "const entered = await menu.inputValue();", "const entered -> if@371"],
    [371, "if (/B/.test(entered))", ""],
    [372, "if (/B/.test(await menu.inputValue()))", "if"],
    [373, "const texts = await rows.allTextContents();", "const texts -> if@374"],
    [374, 'if (new Set(texts).has("B"))', ""],
    [375, 'if (new Set(await rows.allTextContents()).has("B"))', "if"],
    [376, "const labels = await rows.allTextContents();", "const labels -> if@377"],
    [377, 'if ("".concat(...labels) === "B")', ""],
    [378, 'if ("".concat(...(await rows.allTextContents())) === "B")', "if"],
    [379, "const title = await menu.textContent();", "const title -> if@380"],
    [380, 'title}` === "B")', ""],
    [381, 'await menu.textContent()}` === "B")', "if"],
    [382, "const kind = await menu.inputValue();", "const kind -> if@383"],
    [383, "if (handlers[kind])", ""],
    [384, "if (handlers[await menu.inputValue()])", "if"],
  ]);
});

// The evaluate family and `boundingBox` answer for the page as it stands, as `getAttribute`
// does, so each decides a path where its answer stands in a position: the iterable of a
// `for...of` and the condition of an `if`, each with no wait before it. Each line is held to
// what it plants, so a fixture edit that moved one cannot pass for the scan reading it.
test("an `evaluateAll`, `evaluate` or `boundingBox` read is refused where it decides", () => {
  expectPlanted([
    [397, "for (const id of await rows.evaluateAll(", "for-of"],
    [400, "for (const title of await page.evaluate(", "for-of"],
    [403, "for (const edge of Object.values((await menu.boundingBox()) ?? {}))", "for-of"],
    [406, "if ((await rows.evaluateAll((nodes) => nodes.length)) > 1)", "if"],
    [407, "if (await page.evaluate(() => document.hasFocus()))", "if"],
    [408, "if ((await menu.boundingBox()) === null) throw new Error(", "if"],
  ]);
});

// A function literal passed to `evaluate` runs in the page, so a read its body branches on is
// the DOM's own and no probe, and the call's own answer, thrown away here, decides nothing. Each
// line is held to what it plants, so a fixture edit that moved one cannot pass for the scan
// skipping it.
test("a branch inside a function `evaluate` runs in the page is no instance", () => {
  const passes: [number, string][] = [
    [414, "await page.evaluate(() => {"],
    [415, 'if (document.body.getAttribute("data-ready") === "yes")'],
  ];
  for (const [line, text] of passes) expect(fixtureLines[line - 1]).toContain(text);
  expect(fixtureProbes.filter((probe) => probe.line >= 411 && probe.line <= 417)).toEqual([]);
});

// A compound assignment reads its target before it writes it, so its left side carries the
// name's value on: in `while ((n -= 1) >= 0)` the `n` bound to a count reaches the `while`. Each
// line is held to what it plants, so a fixture edit that moved one cannot pass for the scan
// reading it.
test("a compound assignment carries a bound answer on to its position", () => {
  expectPlanted([
    [423, "let n = await rows.count();", "let n -> while@424"],
    [424, "while ((n -= 1) >= 0)", ""],
  ]);
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
  expect(fixtureProbes.filter((probe) => probe.line >= 79 && probe.line <= 86)).toEqual([]);
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
