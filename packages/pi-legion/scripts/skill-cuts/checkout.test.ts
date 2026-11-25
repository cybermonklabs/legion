import { expect, jest, setSystemTime, test } from "bun:test";
import {
  existsSync,
  lstatSync,
  lutimesSync,
  mkdirSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { captureEnvironment } from "@legion/test-fixtures/environment";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { fileTag, snapshotTag } from "./arms";
import {
  answerCall,
  type CheckoutAnswer,
  type CheckoutCopy,
  colocateJj,
  grepScan,
  READ_ONLY_TOOLS,
  readOnlyCall,
  removeOutsideLinks,
  resetSandboxSpawnForTests,
  runArtifacts,
  sandboxedBash,
  sessionHome,
  setSandboxSpawnForTests,
} from "./checkout";
import { pinBinary, pinNatives } from "./test-natives";
import { detachedCommit, gitIn, writeTree } from "./testdata/fixtures";

const scratch = temporaryDirectory("skill-cuts-checkout", { real: true });
/** The `test` of the rows whose reads need the pinned Oh My Pi's native addon (a window that leaves
 *  lines of a file out, for its block context, or a code file the pin may summarize): skipped, or
 *  under GITHUB_ACTIONS failing, where the runner finds none (test-natives.ts). */
const natives = pinNatives("checkout.test.ts");
/** The `test` of the rows that run the pinned Oh My Pi's own binary (checkout.ts's `pinAnswer`):
 *  skipped, or under GITHUB_ACTIONS failing, where the runner finds none (test-natives.ts). */
const pinned = pinBinary("checkout.test.ts");

/** The made-up session's checkout root, as a written case names it. */
const ROOT = "/work/acme/proj";
/** A made-up secret in a file outside every copy, which no answer may show. */
const SECRET = fixtureSecret("outside");

/** A copy of a made-up checkout under `name`: docs/guide.md, a link inside it to that file, and
 *  links that leave it (to a file and a directory holding SECRET outside the copy) or resolve
 *  nowhere. */
function linkedCopy(name: string): string {
  const outside = path.join(scratch, `${name}-outside`);
  mkdirSync(path.join(outside, "dir"), { recursive: true });
  writeFileSync(path.join(outside, "secret.txt"), `${SECRET}\n`);
  writeFileSync(path.join(outside, "dir", "secret.txt"), `${SECRET}\n`);
  const dir = path.join(scratch, name);
  mkdirSync(path.join(dir, "docs"), { recursive: true });
  writeFileSync(path.join(dir, "docs", "guide.md"), "# Guide\n\nRun the app.\n");
  symlinkSync("docs/guide.md", path.join(dir, "inside.md"));
  symlinkSync(path.join(outside, "secret.txt"), path.join(dir, "leak.txt"));
  symlinkSync(path.join(outside, "dir"), path.join(dir, "docs", "up"));
  symlinkSync("missing.txt", path.join(dir, "gone"));
  return dir;
}

/** The guide of the made-up checkout. */
const GUIDE = "# Guide\n\nRun the app.\n";

/** A git checkout of a made-up repository under `name`, as cli.ts's shared checkout is: docs/
 *  guide.md, src/app.py and the files `more` (each path to its text) in one commit, checked out
 *  with a detached HEAD. */
function gitCopy(name: string, more: Record<string, string> = {}): string {
  const dir = path.join(scratch, name);
  writeTree(dir, { "docs/guide.md": GUIDE, "src/app.py": "def main():\n    return 0\n", ...more });
  detachedCommit(dir, "Add the guide");
  return dir;
}

/** The text of a `bash` answer of `copy` to `command` (run from `cwd`, when given), its wall time
 *  left out; the answer when it is none. */
async function bashText(
  copy: CheckoutCopy,
  command: string,
  cwd?: string
): Promise<string | { why: string }> {
  const answer = await answerCall(
    copy,
    "bash",
    { command, ...(cwd === undefined ? {} : { cwd }) },
    "hashline"
  );
  return "why" in answer ? answer : answer.text.replace(/\n\nWall time: \d+\.\d\d seconds/, "");
}

// Catches: a model-written read-only call of a written case's run that reads a file outside the
// copy of its checkout (a key file of the pod) through a symbolic link of the repository, or
// through a word the confinement check does not resolve, so its text reaches the model and the run
// records.
test("removeOutsideLinks removes each link that resolves outside the copy or nowhere and keeps one inside; no read, grep, glob or bash answer shows a file outside the copy", async () => {
  const dir = linkedCopy("copy");
  expect(removeOutsideLinks(dir)).toEqual(["docs/up", "gone", "leak.txt"]);
  expect(lstatSync(path.join(dir, "inside.md")).isSymbolicLink()).toBe(true);
  for (const removed of ["leak.txt", "docs/up", "gone"]) {
    expect({ removed, there: existsSync(path.join(dir, removed)) }).toEqual({
      removed,
      there: false,
    });
  }
  const copy = { dir, root: ROOT, cwd: ROOT, git: false };
  const answers = await Promise.all([
    answerCall(copy, "read", { path: "leak.txt" }, "hashline"),
    answerCall(copy, "grep", { pattern: SECRET.slice(0, 12), path: "." }, "hashline"),
    answerCall(copy, "bash", { command: "cat leak.txt" }, "hashline"),
    answerCall(copy, "bash", { command: `grep -R ${SECRET.slice(0, 12)} .` }, "hashline"),
    answerCall(copy, "bash", { command: "find -L . -type f | xargs cat" }, "hashline"),
    answerCall(copy, "bash", { command: "cat inside.md" }, "hashline"),
  ]);
  expect(answers.map((each) => ("why" in each ? `why: ${each.why}` : each.text))).toEqual([
    "Path 'leak.txt' not found",
    "No matches found",
    "cat: leak.txt: No such file or directory\n\n\nCommand exited with code 1",
    "(no output)\n\nCommand exited with code 1",
    "why: its command is not read-only",
    "# Guide\n\nRun the app.\n",
  ]);
  expect(JSON.stringify(answers)).not.toContain(SECRET);

  // A copy whose links were left in place: a read, grep or glob through one is not answered.
  const kept = { dir: linkedCopy("kept"), root: ROOT, cwd: ROOT, git: false };
  for (const [name, input] of [
    ["read", { path: "leak.txt" }],
    ["read", { path: "docs/up/secret.txt" }],
    ["grep", { pattern: "x", path: "leak.txt" }],
    ["grep", { pattern: "x", path: "docs/up" }],
    ["glob", { path: "docs/up/*.txt" }],
  ] as const) {
    const answer = await answerCall(kept, name, input, "hashline");
    expect({ name, input, answer }).toEqual({
      name,
      input,
      answer: { why: expect.stringContaining("through a symbolic link, outside the checkout") },
    });
  }

  // A bash word that may lead outside the copy, however it gets there, is not answered (a brace
  // expansion the read-only rule refuses itself).
  const deeper = { ...copy, cwd: `${ROOT}/docs` };
  for (const [command, why] of [
    ["cat $HOME/x", "$HOME/x expands"],
    ["cat {/etc/hostname,x}", "its command is not read-only"],
    ["cat ~/x", "~/x is under ~"],
    ["cat .git", ".git names .git"],
    ["cat /etc/hostname", "/etc/hostname is outside the session's checkout"],
    ["grep -f/etc/hostname guide.md", "-f/etc/hostname is outside the session's checkout"],
    ["cd .. && cat ../x", "../x is outside the session's checkout"],
  ] as const) {
    const answer = await answerCall(deeper, "bash", { command }, "hashline");
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a read of a code file of 100 lines or more left unanswered, or a ranged read (a run
// then judged on that read; thermonuclear's runs on a range past a file's end were dropped as
// reads-past-last-round), a range answered past the lines the pin shows, or a read with :raw, a
// list of ranges or :-N left unanswered (the sweep's unanswered rows).
natives.test(
  "a whole read pages after 300 lines; a range includes context and continues only if lines remain; code whose parse elides nothing is shown whole; a range beyond EOF names the bounds; :raw, a list and :-N are answered",
  async () => {
    const dir = path.join(scratch, "ranges");
    mkdirSync(path.join(dir, "src"), { recursive: true });
    mkdirSync(path.join(dir, "docs"), { recursive: true });
    // 120 one-line statements: the pin's parse finds no body to elide, so it shows no summary.
    const big = Array.from({ length: 120 }, (_, n) => `x${n + 1} = ${n + 1}\n`).join("");
    writeFileSync(path.join(dir, "src", "big.py"), big);
    writeFileSync(path.join(dir, "docs", "guide.md"), "# Guide\n\nRun the app.\n");
    const copy = { dir, root: ROOT, cwd: ROOT, git: false, answers: new Map() };
    const lines = big.replace(/\n$/, "").split("\n");
    const rows = (from: number, to: number) =>
      lines
        .slice(from - 1, to)
        .map((line, index) => `${from + index}:${line}`)
        .join("\n");
    const header = `[src/big.py#${snapshotTag(big)}]`;
    const read = (target: string) => answerCall(copy, "read", { path: target }, "hashline");
    expect(await read("src/big.py")).toEqual({
      text: `${header}\n${rows(1, 120)}`,
      isError: false,
    });
    // A file past 300 lines whose parse elides nothing either, paged as the pin pages it.
    const long = Array.from({ length: 350 }, (_, n) => `y${n + 1} = ${n + 1}\n`).join("");
    writeFileSync(path.join(dir, "src", "long.py"), long);
    const longRows = long
      .replace(/\n$/, "")
      .split("\n")
      .map((line, index) => `${index + 1}:${line}`);
    expect(await read("src/long.py")).toEqual({
      text: `[src/long.py#${snapshotTag(long)}]\n${longRows.slice(0, 300).join("\n")}\n\n[Showing lines 1-300 of 350. Use :301 to continue]`,
      isError: false,
    });
    for (const [target, text] of [
      [
        "src/long.py:2-310",
        `[src/long.py#${snapshotTag(long)}]\n${longRows.slice(0, 313).join("\n")}\n\n[Showing lines 1-313 of 350. Use :314 to continue]`,
      ],
      [
        "src/big.py:10-12",
        `${header}\n${rows(9, 15)}\n\n[Showing lines 9-15 of 120. Use :16 to continue]`,
      ],
      [
        "src/big.py:5+2",
        `${header}\n${rows(4, 9)}\n\n[Showing lines 4-9 of 120. Use :10 to continue]`,
      ],
      [
        "src/big.py:1..2",
        `${header}\n${rows(1, 5)}\n\n[Showing lines 1-5 of 120. Use :6 to continue]`,
      ],
      [`${ROOT}/src/big.py:119-120`, `[src/big.py#${snapshotTag(big)}]\n${rows(118, 120)}`],
      // From line 2 on: no trailing context, and the rest of the file.
      [
        "docs/guide.md:2",
        `[docs/guide.md#${snapshotTag("# Guide\n\nRun the app.\n")}]\n1:# Guide\n2:\n3:Run the app.`,
      ],
      // A range that starts past the file's end: the pin's notice alone, no hashline line.
      [
        "src/big.py:200-210",
        "Line 200 is beyond end of file (120 lines total). Use :1 to read from the start, or :120 to read the last line.",
      ],
      // :raw is the file as it is; a list of ranges each range's lines alone, a `…` row between
      // them; :-N the last N lines, read as that range, with a line before it.
      ["src/big.py:raw", big],
      ["src/big.py:1-2,5-6", `${header}\n${rows(1, 2)}\n…\n${rows(5, 6)}`],
      ["src/big.py:-5", `${header}\n${rows(115, 120)}`],
    ] as const) {
      expect({ target, answer: await read(target) }).toEqual({
        target,
        answer: { text, isError: false },
      });
    }
    // A read, grep or glob answer is kept on the copy and given again; a bash answer is not kept.
    await answerCall(copy, "bash", { command: "cat docs/guide.md" }, "hashline");
    expect([...copy.answers.keys()].every((key) => key.startsWith("read\0hashline\0"))).toBe(true);
    expect(copy.answers.size).toBe(12);
  }
);

// Catches: a read with :raw (alone, or joined to a range, a line or a tail), :-N or a list of
// ranges left unanswered (the fidelity sweep's unanswered reasons of those forms), or answered
// with row numbers, context, a hashline line or a column cut where the pin shows the lines as they
// are, with a list's ranges given context or a byte budget, or a file named like a selector read
// as a range; or a form the runner does not rebuild answered.
natives.test(
  "a read's selector is the pin's: :raw and a range, a line or a tail joined to it the lines as they are; :-N the last lines as that range; a list each range alone, one past the end noted; a file named like a selector read as it is; a selector the pin refuses, :conflicts, :img and a raw read whose text is empty or whose first line passes the budget are not answered",
  async () => {
    const notes = "one\ntwo\r\nthree\nfour\nfive\nsix\nseven\neight\n";
    const wide = `a\nb\n${"x".repeat(800)}\nd\n`;
    const copy = filesCopy("selectors", {
      "docs/notes.txt": notes,
      "docs/wide.txt": wide,
      "docs/odd:1-2": "literal\n",
      "docs/empty.txt": "",
      "docs/long.txt": Array.from({ length: 350 }, (_, n) => `line ${n + 1}\n`).join(""),
      "docs/heavy.txt": `${"h".repeat(30_000)}\n${"i".repeat(30_000)}\n${"j".repeat(10)}\n`,
      "docs/huge.txt": `${"k".repeat(60_000)}\n`,
    });
    const read = (target: string) => answerCall(copy, "read", { path: target }, "hashline");
    const header = `[docs/notes.txt#${fileTag(notes)}]`;
    const long = Array.from({ length: 300 }, (_, n) => `line ${n + 1}`).join("\n");
    for (const [target, text] of [
      // :raw: the lines as they are (a carriage return kept), no context, the file's final newline
      // after a read with no end that reaches it, a notice only where lines of it are left out.
      ["docs/notes.txt:raw", notes],
      ["docs/notes.txt:raw:2-3", "two\r\nthree"],
      ["docs/notes.txt:2-3:raw", "two\r\nthree"],
      ["docs/notes.txt:raw:4", "four"],
      ["docs/notes.txt:4:raw", "four"],
      ["docs/notes.txt:raw:6-", "six\nseven\neight\n"],
      ["docs/notes.txt:raw:-2", "seven\neight"],
      ["docs/notes.txt:-2:raw", "seven\neight"],
      ["docs/notes.txt:raw:1-2,5-6", "one\ntwo\r\nfive\nsix"],
      ["docs/notes.txt:raw:1,20-30", "one"],
      ["docs/long.txt:raw", `${long}\n\n[Showing lines 1-300 of 350. Use :301 to continue]`],
      [
        "docs/heavy.txt:raw",
        `${"h".repeat(30_000)}\n\n[Showing lines 1-1 of 3 (50.0KB limit). Use :2 to continue]`,
      ],
      // :-N: the last N lines, read as that range, with the line before it.
      ["docs/notes.txt:-2", `${header}\n6:six\n7:seven\n8:eight`],
      [
        "docs/notes.txt:-20",
        `${header}\n1:one\n2:two\r\n3:three\n4:four\n5:five\n6:six\n7:seven\n8:eight`,
      ],
      // A list: each range's lines alone as the pin holds the file (its line ends LF), a `…` row
      // between them; a range past the end noted after them, a line apart.
      ["docs/notes.txt:1-2,5-6", `${header}\n1:one\n2:two\n…\n5:five\n6:six`],
      [
        "docs/notes.txt:1-2,20-30",
        `${header}\n1:one\n2:two\n[Range 20-30 is beyond end of file (8 lines total); skipped]`,
      ],
      [
        "docs/notes.txt:20-30,40",
        "[Range 20-30 is beyond end of file (8 lines total); skipped]\n[Range 40-40 is beyond end of file (8 lines total); skipped]",
      ],
      [
        `docs/wide.txt:1,3`,
        `[docs/wide.txt#${fileTag(wide)}]\n1:a\n…\n3:${"x".repeat(768)}…\n\n[Some lines truncated to 768 chars. Use :raw:3-3 to read line 3 whole]`,
      ],
      // Ranges that touch are one range, read with its context.
      [
        "docs/notes.txt:3-3,4-4",
        `${header}\n2:two\r\n3:three\n4:four\n5:five\n6:six\n7:seven\n\n[Showing lines 2-7 of 8. Use :8 to continue]`,
      ],
      // A file named like a path and a selector is read as it is.
      ["docs/odd:1-2", `[docs/odd:1-2#${fileTag("literal\n")}]\n1:literal`],
    ] as const) {
      expect({ target, answer: await read(target) }).toEqual({
        target,
        answer: { text, isError: false },
      });
    }
    for (const [target, why] of [
      ["docs/notes.txt:0-2", "Line selector 0 is invalid"],
      ["docs/notes.txt:5-3", "Invalid range 5-3: end must be >= start."],
      ["docs/notes.txt:raw:-0", "Tail selector -0 is invalid"],
      ["docs/notes.txt:conflicts", "of a form the runner does not rebuild"],
      ["docs/notes.txt:img", "of a form the runner does not rebuild"],
      ["docs/notes.txt:raw:20", "gives empty text"],
      ["docs/notes.txt:raw:20-30,40", "gives empty text"],
      ["docs/empty.txt:raw", "gives empty text"],
      ["docs/huge.txt:raw:1", "passes the 50.0KB a raw read shows"],
      ["docs:2-3", "a read of a directory with a selector"],
    ] as const) {
      expect({ target, answer: await read(target) }).toEqual({
        target,
        answer: { why: expect.stringContaining(why) },
      });
    }
  }
);

natives.test(
  "whole and ranged path reads stop at a UTF-8 byte boundary between lines",
  async () => {
    const line = "é".repeat(250);
    const raw = `${line}\n`.repeat(110);
    const copy = filesCopy("byte-pages", { "wide.txt": raw });
    const header = `[wide.txt#${snapshotTag(raw)}]`;
    const rows = (first: number, count: number) =>
      Array.from({ length: count }, (_, i) => `${first + i}:${line}`).join("\n");
    expect(await answerCall(copy, "read", { path: "wide.txt" }, "hashline")).toEqual({
      text: `${header}\n${rows(1, 99)}\n\n[Showing lines 1-99 of 110 (48.8KB limit). Use :100 to continue]`,
      isError: false,
    });
    // The range reserves four bytes per row for its 112 selected and context lines.
    expect(await answerCall(copy, "read", { path: "wide.txt:3-110" }, "hashline")).toEqual({
      text: `${header}\n${rows(2, 101)}\n\n[Showing lines 2-102 of 110 (49.6KB limit). Use :103 to continue]`,
      isError: false,
    });
  }
);

natives.test(
  "a path page joins its column and continuation clauses, and shows only the column clause at EOF",
  async () => {
    const wide = "x".repeat(800);
    const raw = Array.from({ length: 305 }, (_, i) => ([2, 6, 303].includes(i) ? wide : "row"));
    const copy = filesCopy("column-pages", { "rows.txt": raw.join("\n") });
    const shown = raw.map((line) => (line === wide ? `${wide.slice(0, 768)}…` : line));
    expect(await answerCall(copy, "read", { path: "rows.txt" }, "plain")).toEqual({
      text: `${shown.slice(0, 300).join("\n")}\n\n[Showing lines 1-300 of 305. Use :301 to continue. Some lines truncated to 768 chars. Use :raw:3-3 to read line 3 whole; cut lines run to line 7, each read the same way]`,
      isError: false,
    });
    expect(await answerCall(copy, "read", { path: "rows.txt:301-305" }, "plain")).toEqual({
      text: `${shown.slice(299).join("\n")}\n\n[Some lines truncated to 768 chars. Use :raw:304-304 to read line 304 whole]`,
      isError: false,
    });
  }
);

/** A made-up TypeScript function whose `if` block closes on line 6 and whose `for` block opens
 *  on line 8, `wide` a comment of its own on the `if` line. */
function meanSource(wide = ""): string {
  return `${[
    "export function mean(items: number[]): number {",
    `  if (items.length === 0) {${wide}`,
    "    // An empty list has no mean,",
    "    // so its mean is taken as 0.",
    "    return 0;",
    "  }",
    "  let sum = 0;",
    "  for (const item of items) {",
    "    sum += item;",
    "    // Each item counts once.",
    "  }",
    "  return sum / items.length;",
    "}",
  ].join("\n")}\n`;
}

// Catches: a read whose window leaves lines of a file out answered with the window's rows alone,
// where the pin shows the boundary line of each block the window cuts (keep_rule.py:900-1080 of a
// recorded session: its function's two opening lines, `…`, the window, `…`, its `return None`);
// those rows found by another rule than the pin's tree-sitter one or, for a language the addon
// does not know, its scan of the brackets; a long context line not cut; or a read whose blocks
// the pin finds with a grammar it downloads answered as if it found them otherwise.
natives.test(
  "a read that leaves lines out shows the boundary line of each block it cuts, as tree-sitter finds them or, for an unknown language, a scan of the brackets; none where nothing is cut; a grammar the pin downloads leaves the read unanswered",
  async () => {
    // 298 one-line statements and an object of three lines, too few for the pin's summary to
    // elide (`read.summarize.minBodyLines` 4), so a whole read shows it paged.
    const tail = `${[
      ...Array.from({ length: 298 }, (_, n) => `export const a${n + 1} = ${n + 1};`),
      "export const tail = {",
      "  a: 1,",
      "};",
    ].join("\n")}\n`;
    const acme = `${[
      "section main {",
      '  key = "1 ("',
      "  other = 2",
      "  list (",
      "    a",
      "    b",
      "    c",
      "  )",
      "}",
    ].join("\n")}\n`;
    const notes = Array.from({ length: 12 }, (_, n) => `Note ${n + 1}.\n`).join("");
    const wide = ` // ${"w".repeat(800)}`;
    const tool = "def tool\n  if ready\n    run\n  end\n  done\nend\n";
    const copy = filesCopy("block-context", {
      "src/mean.ts": meanSource(),
      "src/wide.ts": meanSource(wide),
      "src/tail.ts": tail,
      "conf/main.acme": acme,
      "docs/notes.txt": notes,
      "lib/tool.rb": tool,
    });
    const read = (target: string) => answerCall(copy, "read", { path: target }, "hashline");
    const meanLines = meanSource().split("\n");
    const row = (n: number, lines = meanLines) => `${n}:${lines[n - 1]}`;
    // Lines 5 to 9 (line 6, a line before and three after) close the `if` that opens on line 2
    // and open the `for` that closes on line 11.
    const window = [5, 6, 7, 8, 9].map((n) => row(n)).join("\n");
    expect(await read("src/mean.ts:6-6")).toEqual({
      text: `[src/mean.ts#${snapshotTag(meanSource())}]\n${row(2)}\n…\n${window}\n…\n${row(11)}\n\n[Showing lines 5-9 of 13. Use :10 to continue]`,
      isError: false,
    });
    // A context line is cut as a shown line is, and the notice of cut lines names it.
    expect(await read("src/wide.ts:6-6")).toEqual({
      text: `[src/wide.ts#${snapshotTag(meanSource(wide))}]\n${`${row(2)}${wide}`.slice(0, 770)}…\n…\n${window}\n…\n${row(11)}\n\n[Showing lines 5-9 of 13. Use :10 to continue. Some lines truncated to 768 chars. Use :raw:2-2 to read line 2 whole]`,
      isError: false,
    });
    // A whole read past 300 lines shows after its 300 rows the closing line of the object they open.
    const tailRows = tail
      .split("\n")
      .slice(0, 300)
      .map((line, index) => `${index + 1}:${line}`)
      .join("\n");
    expect(await read("src/tail.ts")).toEqual({
      text: `[src/tail.ts#${snapshotTag(tail)}]\n${tailRows}\n301:};\n\n[Showing lines 1-300 of 301. Use :301 to continue]`,
      isError: false,
    });
    // An extension the addon knows no language of: the brackets' scan, past the one in a string.
    const acmeLines = acme.split("\n");
    const acmeRows = [1, 2, 3, 4, 5].map((n) => row(n, acmeLines)).join("\n");
    expect(await read("conf/main.acme:2-2")).toEqual({
      text: `[conf/main.acme#${snapshotTag(acme)}]\n${acmeRows}\n…\n${row(8, acmeLines)}\n${row(9, acmeLines)}\n\n[Showing lines 1-5 of 9. Use :6 to continue]`,
      isError: false,
    });
    // No block, no bracket: the window alone.
    const noteRows = notes
      .split("\n")
      .slice(3, 9)
      .map((line, index) => `${index + 4}:${line}`)
      .join("\n");
    expect(await read("docs/notes.txt:5-6")).toEqual({
      text: `[docs/notes.txt#${snapshotTag(notes)}]\n${noteRows}\n\n[Showing lines 4-9 of 12. Use :10 to continue]`,
      isError: false,
    });
    // Ruby's grammar is one the pin downloads before it looks for the blocks: not answered. A
    // read that shows every line needs no blocks, and is answered.
    expect(await read("lib/tool.rb:3-3")).toEqual({
      why: expect.stringContaining(
        "the pin downloads the ruby grammar (ruby-e77f88559b47c0fe.wasm), which is not installed beside its native addon, before it finds the blocks a read of tool.rb cuts"
      ),
    });
    expect(await read("lib/tool.rb")).toEqual({
      text: `[lib/tool.rb#${snapshotTag(tool)}]\n${tool
        .replace(/\n$/, "")
        .split("\n")
        .map((line, index) => `${index + 1}:${line}`)
        .join("\n")}`,
      isError: false,
    });
  }
);

/** A made-up Python module: a docstring, an import, `count` functions of five body lines with two
 *  blank lines between them, and a table of six entries, each on a line of its own. */
function stepsSource(count: number): string {
  const step = (n: number) =>
    [
      `def step${n}(value):`,
      "    doubled = value * 2",
      `    shifted = doubled + ${n}`,
      "    kept = shifted % 7",
      "    scaled = kept * 3",
      "    return scaled - 1",
    ].join("\n");
  return `${[
    '"""Made-up steps."""',
    "import math",
    "",
    Array.from({ length: count }, (_, n) => step(n + 1)).join("\n\n\n"),
    "",
    "",
    "TABLE = {",
    ...Array.from({ length: 6 }, (_, n) => `    "s${n + 1}": step${n + 1},`),
    "}",
  ].join("\n")}\n`;
}

// Catches: a whole read of a code file the pin summarizes answered with the file's whole text in
// place of the summary the session saw (the sweep's class "pin summarizes a code file": R4's
// recorded reads of wait.rs, settings.ts and fix-changelogs.ts, whose answers matched the pin's
// for their first rows alone); a summary whose rows, elision rows or footer are not the pin's; a
// file the pin does not summarize (under its 100 lines, or prose) shown summarized; or a read
// whose summary needs a grammar the pin downloads answered as if the pin had none.
natives.test(
  "a whole read of a code file of 100 lines or more whose parse elides a span is the pin's summary, its footer naming the elided ranges; under 100 lines and prose are shown whole; a grammar the pin downloads leaves the read unanswered",
  async () => {
    // 108 lines as the pin counts them (107 newlines and one more).
    const steps = stepsSource(12);
    const table = `${[
      "export const table = [",
      ...Array.from({ length: 302 }, (_, n) => `  { id: ${n + 1} },`),
      "];",
    ].join("\n")}\n`;
    // The first 98 lines of steps.py with their newlines: 99 lines as the pin counts them.
    const short = `${steps.split("\n").slice(0, 98).join("\n")}\n`;
    const prose = Array.from(
      { length: 30 },
      (_, n) => `## Step ${n + 1}\n\n\`\`\`ts\nconst x = ${n};\n\`\`\`\n`
    ).join("");
    const ruby = Array.from(
      { length: 25 },
      (_, n) => `def step${n + 1}(value)\n  doubled = value * 2\n  doubled + ${n + 1}\nend\n`
    ).join("");
    const copy = filesCopy("summaries", {
      "src/steps.py": steps,
      "src/table.ts": table,
      "src/short.py": short,
      "docs/steps.md": prose,
      "lib/steps.rb": ruby,
    });
    const read = (target: string, rows: "hashline" | "numbered" = "hashline", cwd = ROOT) =>
      answerCall({ ...copy, cwd }, "read", { path: target }, rows);
    const whole = (text: string) =>
      text
        .replace(/\n$/, "")
        .split("\n")
        .map((line, index) => `${index + 1}:${line}`)
        .join("\n");
    // Each function's def and first body line, a `…` row for the three lines after them (the
    // line before opens no bracket), its return and two blank rows; the table one row of its
    // brace pair, `<first>-<last>`.
    const rows = (mark: ":" | "|") =>
      [
        `1${mark}"""Made-up steps."""`,
        `2${mark}import math`,
        `3${mark}`,
        ...Array.from({ length: 12 }, (_, index) => {
          const at = 4 + 8 * index;
          return [
            `${at}${mark}def step${index + 1}(value):`,
            `${at + 1}${mark}    doubled = value * 2`,
            "…",
            `${at + 5}${mark}    return scaled - 1`,
            `${at + 6}${mark}`,
            `${at + 7}${mark}`,
          ];
        }).flat(),
        `100-107${mark}TABLE = { … }`,
      ].join("\n");
    // 13 elided ranges, 12 of 3 lines and the table's 6: its first two, after `, e.g.`.
    const footer = (listed: string) =>
      `[…42ln elided; re-read needed ranges, e.g. ${listed}:6-8,14-16]`;
    expect(await read("src/steps.py")).toEqual({
      text: `[src/steps.py#${snapshotTag(steps)}]\n${rows(":")}\n\n${footer("src/steps.py")}`,
      isError: false,
    });
    // In a session that shows line numbers and no hashline rows: `|` rows, no hashline line.
    expect(await read("src/steps.py", "numbered")).toEqual({
      text: `${rows("|")}\n\n${footer("src/steps.py")}`,
      isError: false,
    });
    // Read from another working directory, the hashline line and the footer name it absolute.
    expect(await read(`${ROOT}/src/steps.py`, "hashline", `${ROOT}/docs`)).toEqual({
      text: `[${ROOT}/src/steps.py#${snapshotTag(steps)}]\n${rows(":")}\n\n${footer(`${ROOT}/src/steps.py`)}`,
      isError: false,
    });
    // One elided range: the footer's `with` form; the brace pair's tail `];`.
    expect(await read("src/table.ts")).toEqual({
      text: `[src/table.ts#${snapshotTag(table)}]\n1-304:export const table = [ … ];\n\n[…302ln elided; re-read needed ranges with src/table.ts:1-304]`,
      isError: false,
    });
    // Under the pin's 100 lines, the whole text, though its parse would elide the same bodies;
    // and 150 lines of Markdown, which the pin never summarizes at its defaults.
    expect(await read("src/short.py")).toEqual({
      text: `[src/short.py#${snapshotTag(short)}]\n${whole(short)}`,
      isError: false,
    });
    expect(await read("docs/steps.md")).toEqual({
      text: `[docs/steps.md#${snapshotTag(prose)}]\n${whole(prose)}`,
      isError: false,
    });
    // Ruby's grammar is one the pin downloads before it summarizes: not answered.
    expect(await read("lib/steps.rb")).toEqual({
      why: "the pin downloads the ruby grammar (ruby-e77f88559b47c0fe.wasm), which is not installed beside its native addon, before it summarizes a whole read of steps.rb, and shows the summary from that grammar, or the file unsummarized where the download fails: the runner cannot tell which the session had",
    });
  }
);

// Catches: an inspection step counted as an act, or an act (a write, a commit, a command run
// through a substitution) taken for a read-only call: the report's inspection floors and the
// replay's checkout rounds both read this one rule.
test("readOnlyCall takes the read-only tools, and a bash call whose every command, joined by pipes, &&, || or ;, is one of the read-only programs with no redirection to a file", () => {
  expect([...READ_ONLY_TOOLS]).toEqual(["read", "grep", "glob", "find", "ls"]);
  for (const tool of READ_ONLY_TOOLS) {
    expect({ tool, readOnly: readOnlyCall(tool, { path: "src" }) }).toEqual({
      tool,
      readOnly: true,
    });
  }
  for (const command of [
    "cd src && cat app.py",
    "cat README.md | head -20",
    "head -n 5 a.txt; tail -n 5 a.txt",
    "ls -la docs",
    "find . -name '*.py' -type f",
    "grep -rn 'TODO' src || grep -rn FIXME src",
    "rg -n 'def main' src",
    "wc -l src/app.py",
    "sed -n '1,20p' src/app.py",
    "nl -ba src/app.py | sed -n 1,5p",
    "stat src/app.py",
    "file docs/logo.png",
    "tree -L 2 src",
    "git log --oneline -5",
    "git show HEAD:src/app.py",
    "git diff --stat",
    "git status --short",
    "git grep -n main",
    "git ls-files | wc -l",
    "git blame src/app.py",
    "git -C src status",
    "git --no-pager log -1",
    "ls missing 2>/dev/null; ls",
    "cat src/app.py 2>&1 | head",
    'grep -n "a > b" src/app.py',
    "cat < src/app.py",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
  for (const command of [
    "find . -name '*.pyc' -exec rm {} \\;",
    "find . -type d -execdir ls \\;",
    "find . -name '*.tmp' -delete",
    "find . -fprint found.txt",
    "cat src/app.py > copy.py",
    "grep -n main src/app.py >> notes.txt",
    "ls &> listing.txt",
    'grep "$(whoami)" src/app.py',
    "git push",
    "git commit -m wip",
    "git -c core.pager=less log",
    "rm -rf build",
    "sed -i 's/a/b/' src/app.py",
    "sed 's/a/b/' src/app.py",
    "python3 -c 'print(1)'",
    "cat src/app.py & cat README.md",
    "cat <(ls)",
    "/bin/cat src/app.py",
    "",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  // Any other tool, and a bash call with no command, is no read-only call.
  for (const [name, input] of [
    ["write", { path: "a.py", content: "" }],
    ["edit", { path: "a.py" }],
    ["todo", { op: "view" }],
    ["task", { prompt: "look" }],
    ["bash", {}],
    ["bash", { command: 3 }],
  ] as const) {
    expect({ name, readOnly: readOnlyCall(name, input) }).toEqual({ name, readOnly: false });
  }
});

/** The dev box's start of a session's bash call, with made-up variables of its own. */
const UNSET = "unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM ACME_TOOLS_PIN ACME_TOOLS_IN";

// Catches: a written case's look at the work judged as its act because the read-only rule missed a
// form the dev box's sessions use (the defect: every judged step of test-driven-development's
// written cases was a bash call that starts with `unset`, prints `pwd`, or asks jj or git where it
// is), or an act (a branch made, a remote added, a jj change, a program the environment chooses, an
// option smuggled in a variable, a file written from a subshell) taken for a read-only call.
test("readOnlyCall takes pwd, true, echo, printf, env with no command, unset, export and assignments, git rev-parse, worktree list, remote and branch with no name, jj's read commands, and a subshell, group or substitution of read-only commands; it refuses each form that acts", () => {
  for (const command of [
    "pwd",
    "true",
    "echo done",
    "printf '%s\\n' a b",
    "printf -v line '%s' a",
    "env",
    "env -0",
    "env -i -u ACME_TOOLS_PIN",
    UNSET,
    "unset -v ACME_TOOLS_PIN",
    "export LC_ALL=C",
    "export ACME_MODE",
    "LIMIT=5",
    "FOO=bar cat src/app.py",
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${dir}`
    'dir=src && ls $dir && cat "${dir}/app.py"',
    "cat $HOME/.profile",
    "echo $PATH $PWD",
    "git rev-parse --show-toplevel",
    "git rev-parse --abbrev-ref HEAD",
    "git worktree list",
    "git worktree list --porcelain",
    "git remote",
    "git remote -v",
    "git remote show origin",
    "git remote get-url origin",
    "git branch",
    "git branch -a",
    "git branch -r",
    "git branch -v",
    "git branch -vv",
    "git branch --list",
    "git branch --show-current",
    "jj status",
    "jj st",
    "jj log -r @ --no-graph",
    "jj show",
    "jj diff --git",
    "jj root",
    "jj file show src/app.py",
    "jj bookmark list",
    "jj workspace list",
    "jj --no-pager -R . st",
    "(cd src && ls)",
    "(jj st || git status) | cat",
    "{ ls; pwd; } 2>/dev/null",
    'cd "$(git rev-parse --show-toplevel)" && ls',
    "cat $(ls src)",
    "cat `ls src`",
    "(jj root 2>/dev/null; git rev-parse --show-toplevel 2>/dev/null; true)",
    `cd ${ROOT} && ${UNSET} && pwd && (jj st || git status) && git worktree list`,
    `${UNSET}; cd ${ROOT} && pwd && ls -a && (jj workspace list 2>/dev/null; git worktree list 2>/dev/null; jj st 2>/dev/null | head -50)`,
    `cd ${ROOT} && ${UNSET} && pwd && ls -a && (jj root 2>/dev/null; git rev-parse --show-toplevel 2>/dev/null; git status --short 2>/dev/null)`,
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
  for (const command of [
    "git branch new-name",
    "git branch -d old",
    "git branch -m old new",
    "git branch --set-upstream-to=origin/main",
    "git remote add x y",
    "git remote set-url origin https://example.test/x.git",
    "git remote remove origin",
    "git worktree add ../x",
    "git worktree remove ../x",
    "jj new",
    "jj describe -m wip",
    "jj git fetch",
    "jj bookmark set main",
    "jj workspace add ../x",
    "jj diff --tool rm",
    "jj --config ui.pager=less log",
    "export X=$(rm -rf /)",
    "echo $(git push)",
    "(cat src/app.py > copy.py)",
    "(ls; cat src/app.py >> notes.txt) && pwd",
    "{ ls; } > listing.txt",
    "cd $(mktemp -d)",
    "env -u GIT_DIR git status",
    "env rm -rf build",
    "env -S 'rm -rf build'",
    "unset PATH",
    "PATH=. ls",
    "export PATH=.",
    "HOME=. git log",
    "GIT_EXTERNAL_DIFF=rm git diff",
    "export GIT_DIR=../other",
    "JJ_CONFIG=jj.toml jj diff",
    "RIPGREP_CONFIG_PATH=rg.conf rg main",
    "LD_PRELOAD=./x.so ls",
    "X=--pre=sh && rg $X main",
    "X='1e rm -rf .' && sed -n \"$X\" src/app.py",
    "cat $FOO",
    "echo $?",
    "echo $((1 + 2))",
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${X:-x}`
    "echo ${X:-x}",
    "printf -v 'a[$(id)]' x",
    "unset 'a[$(id)]'",
    "a[$(id)]=1",
    "( )",
    "{ ls }",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
});

// Catches: an answer that names the copy's own path (a scratch path in a body moved pass rates
// before), `pwd` and git's and jj's answers of where the session is given at the runner's paths,
// a jj command of a session that ran jj answered with an error that session never saw, or a word
// whose expansion may lead outside the copy answered.
test("a copy made a colocated jj repository answers jj st and jj root, and pwd, git rev-parse --show-toplevel, git worktree list and errors, at the session's paths; no answer holds the copy's path; an expansion the runner cannot place inside the checkout is not answered", async () => {
  const dir = gitCopy("colocated");
  expect(await colocateJj(dir)).toBe(true);
  const copy = { dir, root: ROOT, cwd: ROOT, git: true, jj: true as const };
  const given: unknown[] = [];
  const text = async (command: string, cwd?: string) => {
    const answer = await bashText(copy, command, cwd);
    given.push(answer);
    return answer;
  };
  expect(await text("pwd")).toBe(`${ROOT}\n`);
  expect(await text("pwd", "docs")).toBe(`${ROOT}/docs\n`);
  expect(await text("cd docs && pwd && echo $PWD")).toBe(`${ROOT}/docs\n${ROOT}/docs\n`);
  expect(await text("git rev-parse --show-toplevel", "docs")).toBe(`${ROOT}\n`);
  expect(await text("git worktree list")).toMatch(
    new RegExp(`^${ROOT} +[0-9a-f]{7,} \\(detached HEAD\\)\\n$`)
  );
  expect(await text("jj root", "src")).toBe(`${ROOT}\n`);
  const status = await text("jj st");
  expect(status).toEqual(expect.stringContaining("The working copy has no changes."));
  expect(status).toEqual(expect.stringContaining("Add the guide"));
  // A jj answer reads the snapshot the colocation took and takes no lock of the working copy (the
  // copy is read-only to it), so answers given at once are the same answer.
  expect(
    new Set(await Promise.all(Array.from({ length: 6 }, () => bashText(copy, "jj st"))))
  ).toEqual(new Set([status]));
  expect(await text("jj workspace list")).toEqual(expect.stringMatching(/^default: /));
  expect(await text(`cd "$(git rev-parse --show-toplevel)" && pwd`, "docs")).toBe(`${ROOT}\n`);
  // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${dir}`
  expect(await text("dir=docs && cat $dir/guide.md && cat ${dir}/guide.md")).toBe(GUIDE + GUIDE);
  expect(await text("git branch --show-current")).toBe("(no output, exit code 0)");
  const shape = await text(
    `cd ${ROOT} && ${UNSET} && pwd && (jj st || git status) && git worktree list`
  );
  expect(shape).toEqual(expect.stringMatching(new RegExp(`^${ROOT}\\nThe working copy has no`)));
  expect(shape).toEqual(expect.stringMatching(new RegExp(`\\n${ROOT} +[0-9a-f]{7,} \\(detached`)));
  expect(await text(`cd ${ROOT}/nowhere`)).toBe(
    `bash: line 1: cd: ${ROOT}/nowhere: No such file or directory\n\n\nCommand exited with code 1`
  );
  expect(await text(`cat ${ROOT}/src/missing.py`)).toBe(
    `cat: ${ROOT}/src/missing.py: No such file or directory\n\n\nCommand exited with code 1`
  );
  expect(await text("env")).toEqual(expect.stringContaining("PATH="));
  given.push(
    await answerCall(copy, "read", { path: `${ROOT}/docs/guide.md` }, "hashline"),
    await answerCall(copy, "grep", { pattern: "Guide", path: ROOT }, "hashline"),
    await answerCall(copy, "glob", { path: `${ROOT}/**/*.md` }, "hashline"),
    await answerCall(copy, "read", { path: "docs/missing.md" }, "hashline")
  );
  expect(given.every((each) => typeof each === "string" || !("why" in Object(each)))).toBe(true);
  expect(JSON.stringify(given)).not.toContain(dir);
  expect(JSON.stringify(given)).not.toContain(scratch);

  // A word whose expansion the runner cannot place inside the checkout is not answered.
  for (const [command, why] of [
    ["cat $HOME/.profile", "$HOME/.profile expands"],
    ["echo $HOME", "$HOME"],
    ["dir=/etc && cat $dir/hostname", "is outside the session's checkout"],
    ['cd "$(git rev-parse --git-dir)" && ls', "expands"],
    ["ls missing || dir=docs; cat $dir/guide.md", "$dir/guide.md expands"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a jj command of a session that ran no jj answered as in a jj workspace, or one answered
// where the copy could not be made a jj repository (its answer then an error the session never
// saw) or holds no history.
test("a copy no session made a jj repository answers jj as jj does outside one; where its colocation failed, or the copy is GitHub's tarball, a jj command is not answered", async () => {
  const dir = gitCopy("plain");
  const plain = { dir, root: ROOT, cwd: ROOT, git: true };
  const answer = await answerCall(plain, "bash", { command: "jj st" }, "hashline");
  expect(answer).toMatchObject({ isError: true });
  expect("text" in answer ? answer.text : answer.why).toStartWith(
    'Error: There is no jj repo in "."'
  );
  const failed = { ...plain, jj: { why: "jj git init --colocate failed: no room" } };
  expect(await bashText(failed, "(jj root || git rev-parse --show-toplevel)")).toEqual({
    why: expect.stringContaining("jj git init --colocate failed: no room"),
  });
  expect(await bashText(failed, "git rev-parse --show-toplevel")).toBe(`${ROOT}\n`);
  expect(await bashText({ ...plain, git: false }, "jj root")).toEqual({
    why: expect.stringContaining("GitHub's tarball"),
  });
}, 60_000);

// Catches: a glob of a directory or of a file with no glob character, or of a `;` list of paths,
// left unanswered (the defect: thermonuclear's written cases dropped as reads-past-last-round on
// such calls, which the pin answers), answered in a form the pin does not give, a missing path
// answered as no match where the pin says it is not found, or an answer that shows jj's own files
// of a colocated copy, which the pin's walk leaves out.
test("a glob of a directory with no glob character lists it, one of a file answers the file, and a ;-separated list answers each of its paths, a missing one noted; a missing path is not answered; jj's .jj directory is in no answer", async () => {
  const copy = filesCopy("globs", {
    "docs/guide.md": GUIDE,
    "docs/api/ref.md": "# Ref\n",
    "src/app.py": "def main():\n    return 0\n",
    ".jj/repo/store.md": "# Store\n",
  });
  // The guide the newest, so its directory comes first in a list's merged answer.
  setTimes(copy.dir, {
    "docs/guide.md": MADE_AT - MINUTE,
    "docs/api/ref.md": MADE_AT - HOUR,
    "src/app.py": MADE_AT - DAY,
  });
  const glob = (target: string) => answerCall(copy, "glob", { path: target }, "hashline");
  for (const [target, text] of [
    ["docs", "# docs/\nguide.md\n## api/\nref.md"],
    [`${ROOT}/docs/api`, "# docs/api/\nref.md"],
    ["src/app.py", "# src/\napp.py"],
    [`${ROOT}/src/app.py`, "# src/\napp.py"],
    ["docs/*.md;src/*.py", "# docs/\nguide.md\n# src/\napp.py"],
    ["docs/*.md; missing/*.py", "# docs/\nguide.md\n\nSkipped missing paths: missing/*.py"],
    ["**/*.md", "# docs/\nguide.md\n## api/\nref.md"],
  ] as const) {
    expect({ target, answer: await glob(target) }).toEqual({
      target,
      answer: { text, isError: false },
    });
  }
  for (const [target, why] of [
    ["missing", "not found"],
    ["missing/*.md", "not found"],
    ["a/*.md;b/*.md", "not found"],
    ["src/app.py,docs/guide.md", "is a list"],
    ["docs/*.md;/etc/*.conf", "outside the session's checkout"],
  ] as const) {
    expect({ target, answer: await glob(target) }).toEqual({
      target,
      answer: { why: expect.stringContaining(why) },
    });
  }
  expect(await answerCall(copy, "grep", { pattern: "Store", path: "." }, "hashline")).toEqual({
    text: "No matches found",
    isError: false,
  });
}, 60_000);

// Catches: a glob's paths answered in path order (the sweep of R4's answers: the pin sorts its
// matches by time of change, newest first, glob.ts lines 582 and 656), a tie broken by name as
// JavaScript compares it where the pin's walk compares bytes, or a list's patterns answered one
// after another where the pin merges them by time; or a glob that finds its limit of paths
// answered without the pin's notice that it reached it, or one that finds more left unanswered.
test("a glob answers its matches newest first by the copy's own times of change, a tie by path below its base in byte order; a ;-list's patterns merged by time, each path once, a tie in the list's order; a file named alone at its own time; at most its limit of paths, the newest, with the pin's notice where it finds that many", async () => {
  const copy = filesCopy("glob-order", {
    "src/old.ts": "",
    "src/new.ts": "",
    "src/B.ts": "",
    "src/a.ts": "",
    "lib/mid.ts": "",
    "lib/deep/x.ts": "",
  });
  setTimes(copy.dir, {
    "src/old.ts": MADE_AT - 3 * DAY,
    "src/new.ts": MADE_AT - MINUTE,
    // Two of one millisecond: `B.ts` before `a.ts` in byte order (by name, `a.ts` first).
    "src/B.ts": MADE_AT - HOUR,
    "src/a.ts": MADE_AT - HOUR,
    // As new as `src/new.ts`.
    "lib/mid.ts": MADE_AT - MINUTE,
    "lib/deep/x.ts": MADE_AT - 2 * DAY,
  });
  const glob = (target: string) => answerCall(copy, "glob", { path: target }, "hashline");
  const src = "# src/\nnew.ts\nB.ts\na.ts\nold.ts";
  for (const [target, text] of [
    ["src/*.ts", src],
    // One walk: `lib/mid.ts` and `src/new.ts` tie, and `lib/…` comes first in byte order.
    ["**/*.ts", `# lib/\nmid.ts\n## deep/\nx.ts\n${src}`],
    // A list: its patterns' paths merged by time, the tie in the list's order.
    ["lib/*.ts;src/*.ts", `# lib/\nmid.ts\n${src}`],
    ["src/*.ts;lib/*.ts", `${src}\n# lib/\nmid.ts`],
    ["src/old.ts;src/new.ts", "# src/\nnew.ts\nold.ts"],
  ] as const) {
    expect({ target, answer: await glob(target) }).toEqual({
      target,
      answer: { text, isError: false },
    });
  }
  // A target keeps its `limit` newest matches, and a call that finds `limit` or more paths says
  // the limit was reached, with twice it where that is under 200 (list-limit.ts's
  // `applyListLimit`, glob.ts lines 337-374).
  for (const [target, limit, text] of [
    ["src/*.ts", 4, `${src}\n\n[4 results limit reached. Use limit=8 for more]`],
    ["src/*.ts", 5, src],
    ["src/*.ts", 150, src],
    ["src/*.ts", 2, "# src/\nnew.ts\nB.ts\n\n[2 results limit reached. Use limit=4 for more]"],
    ["src/*.ts;lib/*.ts", 1, "# src/\nnew.ts\n\n[1 results limit reached. Use limit=2 for more]"],
  ] as const) {
    expect({
      target,
      limit,
      answer: await answerCall(copy, "glob", { path: target, limit }, "hashline"),
    }).toEqual({ target, limit, answer: { text, isError: false } });
  }
  const name = (n: number) => `f${String(n).padStart(3, "0")}.ts`;
  const many = filesCopy(
    "glob-limit",
    Object.fromEntries(Array.from({ length: 201 }, (_, n) => [`many/${name(n)}`, ""]))
  );
  setTimes(
    many.dir,
    Object.fromEntries(Array.from({ length: 201 }, (_, n) => [`many/${name(n)}`, MADE_AT]))
  );
  const tree = `# many/\n${Array.from({ length: 200 }, (_, n) => name(n)).join("\n")}`;
  for (const [limit, text] of [
    [undefined, `${tree}\n\n[200 results limit reached]`],
    [
      300,
      `${tree}\n\nRequested limit 300 clamped to the max of 200\n\n[200 results limit reached]`,
    ],
  ] as const) {
    const input = limit === undefined ? { path: "many/*.ts" } : { path: "many/*.ts", limit };
    expect({ limit, answer: await answerCall(many, "glob", input, "hashline") }).toEqual({
      limit,
      answer: { text, isError: false },
    });
  }
}, 60_000);

// Catches: a file of the working directory whose name starts with `..` shown by its relative path,
// where the pin's formatPathRelativeToCwd, which reads a relative path that starts with `..` as
// outside the directory, shows it absolute.
test("a file of the working directory whose name starts with .. is shown absolute by read, grep and glob, as the pin shows it; one below a directory of it relative", async () => {
  const copy = filesCopy("dot-dot-names", { "..notes.md": "dots\n", "docs/..cfg.md": "dots\n" });
  const tag = snapshotTag("dots\n");
  for (const [name, input, text] of [
    ["read", { path: "..notes.md" }, `[${ROOT}/..notes.md#${tag}]\n1:dots`],
    ["grep", { pattern: "dots", path: "..notes.md" }, `[${ROOT}/..notes.md#${tag}]\n*1:dots`],
    ["glob", { path: "..notes.md" }, `# ${ROOT}/\n..notes.md`],
    ["read", { path: "docs/..cfg.md" }, `[docs/..cfg.md#${tag}]\n1:dots`],
    ["glob", { path: "docs/..cfg.md" }, "# docs/\n..cfg.md"],
  ] as const) {
    expect({ name, input, answer: await answerCall(copy, name, input, "hashline") }).toEqual({
      name,
      input,
      answer: { text, isError: false },
    });
  }
}, 60_000);

// The security review of the runner at dd14a587 found read-only calls that act or read outside the
// copy; each is a row below, made from the review's own command.

// Catches: a read-only call that sets a variable the shell or a program reads (`printf -vNAME`, the
// name attached to `-v`), runs a substitution through bash's old arithmetic `$[…]`, writes or reads
// a file through sed's commands, flags or options, runs a program or writes or reads a file
// through an option of git, jj, tree, file or rg given in a cluster, attached or abbreviated, or
// unsets the answers' own git or jj settings.
test("readOnlyCall refuses printf -vNAME, $[…], sed's w, W, e, r and R commands and s flags, its in-place option in every spelling and -f, the options of git, jj, tree, file and rg that run a program or write or read a file, clustered, attached or abbreviated, and an unset of the answers' git or jj settings; it still takes the read-only forms beside them", () => {
  for (const command of [
    // printf -v with the name attached.
    "printf -vPATH .",
    "printf -vGIT_CONFIG_GLOBAL vendor.gitconfig; git status",
    "printf -vIFS / && cat src/app.py",
    // $[…], which runs the substitution a variable it reads holds.
    "x='a[$(id)]'; echo $[x]",
    "echo $[1 + 2]",
    // sed: commands and flags that run a program or write or read a file, -i, -f.
    "sed -n 'r /etc/hostname' src/app.py",
    "sed -n '1R /etc/hostname' src/app.py",
    "sed -n '$!W copy.txt' src/app.py",
    "sed -n 'p;w copy.txt' src/app.py",
    "sed -n 's/a/b/w copy.txt' src/app.py",
    "sed -n 's/main/id/e' src/app.py",
    "sed -n 's/a/b/gpw copy.txt' src/app.py",
    "sed -n '1e id' src/app.py",
    "sed -n '/x/{p;r /etc/hostname\n}' src/app.py",
    "sed -n -e 1p -e 'w copy.txt' src/app.py",
    "sed --quiet --expression='$r /etc/hostname' src/app.py",
    "sed -n --e '1r /etc/hostname' src/app.py",
    "sed -ni p src/app.py",
    "sed -n -i.bak p src/app.py",
    "sed -n --in-place p src/app.py",
    "sed -n --in p src/app.py",
    "sed -n --in-pl=.bak p src/app.py",
    "sed -n -f script.sed src/app.py",
    "sed -nf script.sed src/app.py",
    "sed -n --fi=script.sed src/app.py",
    // git and jj options that run a program or write or read a file.
    "git grep -Oid main",
    "git grep -nOid main",
    "git grep -nO main",
    "git grep --open-files-in-pager=id main",
    "git grep --open=id main",
    "git grep --op main",
    "git log --out=copy.txt",
    "git diff --ext",
    "git diff --no-ind src docs",
    "git log --show-signature -1",
    "git log --show-sig -1",
    "git log --format=%G? -1",
    "git show --pretty=format:%GS",
    "jj diff --too rm",
    "jj log --config-f=jj.toml",
    "jj --conf ui.pager=less log",
    // tree, file and rg in clusters, attached or abbreviated.
    "tree -ao out.txt",
    "tree -Lo 2 out.txt",
    "tree -R -H . -L 1",
    "file -bC",
    "file --comp",
    "file -m magic src/app.py",
    "file -bmmagic src/app.py",
    "file --magic=magic src/app.py",
    "file -f names.txt",
    "file -z src/app.py",
    "file --uncompress src/app.py",
    "file -p src/app.py",
    "rg --pre sh main",
    "rg --pr=sh main",
    "rg --pre-glob '*' --pre sh main",
    "rg -z main",
    "rg -nz main",
    "rg --search-zip main",
    "rg --hostname-bin=id main",
    // The answers' own git and jj settings.
    "unset GIT_CONFIG_COUNT",
    "unset GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0",
    "unset JJ_CONFIG",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  for (const command of [
    "printf -v line %s a",
    "printf -- -v",
    "sed -n '/def main/,/^$/p' src/app.py",
    "sed -n '/error/p' src/app.py",
    "sed -n '/^w/p;s/x/y/p' src/app.py",
    "sed -ne 5p src/app.py",
    "sed -n -e 1p -e '$p' src/app.py",
    "sed --quiet 10q src/app.py",
    "sed -nE '/a|b/{p;q}' src/app.py",
    "sed -n '$=' src/app.py",
    "sed -n '1~2p' src/app.py",
    "sed -n '/x/a\\\nnote' src/app.py",
    "git grep -e -O main",
    "git log -SOAuth",
    "git grep --or -e a -e b",
    "git diff --no-ext-diff",
    "git diff --output-indicator-new=+",
    "git log --format=%s -1",
    "jj diff --to @",
    "jj log --color=never -r @",
    "tree -L 2 -a src",
    "file -b src/app.py",
    "rg -tpy main",
    "rg -e -z src",
    "env -0 -u ACME_TOOLS_PIN",
    "env -",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
});

// Catches: a path through a symbolic link of the checkout that resolves inside it, so that
// removeOutsideLinks keeps it, and then `..`: the kernel walks the `..` from the link's target, so
// the path read as written stays in the checkout while the walk leaves the copy (the review's
// `l/../x` and `l/../../..`); or the same through a glob the link matches, which the runner reads
// as written (the second review's `a/l*/../../x`).
test("a bash path through a link that stays inside the copy, then .., is not answered where the kernel's walk of it leaves the copy, nor one whose glob comes before .. or may match . or .., nor a cd to a glob; a path through the same links that stays inside is answered", async () => {
  const copy = filesCopy("walk", { "docs/guide.md": GUIDE });
  writeTree(path.join(scratch, "walk-outside"), { "secret.txt": `${SECRET}\n` });
  mkdirSync(path.join(copy.dir, "docs", "a", "b"), { recursive: true });
  symlinkSync(".", path.join(copy.dir, "self"));
  symlinkSync("../../..", path.join(copy.dir, "docs", "a", "b", "up"));
  expect(removeOutsideLinks(copy.dir)).toEqual([]);
  for (const [command, cwd] of [
    ["cat self/../walk-outside/secret.txt", undefined],
    ["cat docs/a/b/up/../walk-outside/secret.txt", undefined],
    [`cat ${ROOT}/self/../walk-outside/secret.txt`, undefined],
    ["ls self/..", undefined],
    ["grep -r x self/../walk-outside", undefined],
    ["cat ../walk-outside/secret.txt", "self"],
  ] as const) {
    const answer = await bashText(copy, command, cwd);
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining("through a symbolic link of the checkout") },
    });
  }
  for (const command of [
    "cat sel*/../walk-outside/secret.txt",
    "cat [s]elf/../walk-outside/secret.txt",
    "cat docs/a/b/u?/../walk-outside/secret.txt",
    "cd sel* && cat ../walk-outside/secret.txt",
    "ls -d .*",
  ]) {
    const answer = await bashText(copy, command);
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining("holds a glob") },
    });
  }
  expect(await bashText(copy, "cat self/docs/guide.md docs/a/b/up/docs/guide.md docs/*.md")).toBe(
    GUIDE + GUIDE + GUIDE
  );
}, 60_000);

// Catches: jj, run in a copy that is no jj repository, finding a jj repository in a directory above
// the copy (the issue's workspace) and answering from it.
test("a jj command in a copy that is no jj repository, inside a directory that is one, answers as jj does outside any repository", async () => {
  const above = path.join(scratch, "jj-above");
  mkdirSync(above, { recursive: true });
  jjIn(above, "git", "init");
  const copy = { dir: gitCopy("jj-above/copy"), root: ROOT, cwd: ROOT, git: true };
  for (const command of ["jj root", "jj st"]) {
    const answer = await answerCall(copy, "bash", { command }, "hashline");
    expect({ command, answer }).toEqual({
      command,
      answer: {
        text: expect.stringMatching(/^Error: There is no jj repo in "\."\n/),
        isError: true,
      },
    });
    expect(JSON.stringify(answer)).not.toContain(above);
  }
}, 60_000);

// Catches: a git command of a copy fetched with no blobs (`--filter=blob:none`, as dd14a587's shared
// checkout was) fetching a blob it lacks from the commit's remote as it answers (`git show` of an
// older commit), which reaches the network with the pod's credential.
test("a git command in a copy that lacks an older commit's blob fetches nothing: the answer is git's error, and the copy still lacks the blob", async () => {
  const work = path.join(scratch, "lazy-work");
  writeTree(work, { "docs/guide.md": "# Guide\n\nRun the old app.\n" });
  gitIn(work, "init", "--quiet");
  gitIn(work, "add", ".");
  gitIn(work, "commit", "--quiet", "-m", "Write the first guide");
  const older = gitIn(work, "rev-parse", "HEAD:docs/guide.md");
  writeTree(work, { "docs/guide.md": GUIDE });
  gitIn(work, "commit", "--quiet", "-am", "Add the guide");
  const commit = gitIn(work, "rev-parse", "HEAD");
  const server = path.join(scratch, "lazy-server.git");
  gitIn(scratch, "clone", "--quiet", "--bare", work, server);
  gitIn(server, "config", "uploadpack.allowFilter", "true");
  gitIn(server, "config", "uploadpack.allowAnySHA1InWant", "true");
  const dir = path.join(scratch, "lazy");
  mkdirSync(dir, { recursive: true });
  gitIn(dir, "init", "--quiet");
  gitIn(dir, "remote", "add", "origin", server);
  gitIn(dir, "fetch", "--quiet", "--depth=50", "--filter=blob:none", "origin", commit);
  gitIn(dir, "checkout", "--quiet", "--detach", commit);
  const holdsOlder = () =>
    Bun.spawnSync(["git", "cat-file", "-e", older], {
      cwd: dir,
      env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: dir, GIT_NO_LAZY_FETCH: "1" },
    }).exitCode === 0;
  expect(holdsOlder()).toBe(false);
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  const answer = await answerCall(
    copy,
    "bash",
    { command: "git show HEAD~1:docs/guide.md" },
    "hashline"
  );
  expect(answer).toMatchObject({ isError: true });
  expect(JSON.stringify(answer)).not.toContain("old app");
  expect(holdsOlder()).toBe(false);
}, 60_000);

// Catches: git in an answer's sandbox hashing every file of the copy again, so `git status` in a
// large checkout runs past the 30 s limit (the defect: 155 runs of test-driven-development's
// written cases got no answer to a bash call holding `git status`, in a checkout of 115,965 files):
// in the sandbox's user namespace each file shows uid and gid 0 where the index recorded the
// runner's, and the copy's ctime may not be the index's, so git's default stat check calls every
// file changed and compares its content, and with GIT_OPTIONAL_LOCKS=0 it never writes the
// refreshed index, so every call does it again. The probe of "not hashed again": a file whose text
// changed with its size and mtime kept is shown clean only when git trusts its stat data.
test("git in the sandbox trusts the stat data the runner's user wrote to the index, its ctime aside: a file whose text changed with its size and mtime kept is not hashed again, so git status shows it clean; one whose size changed shows as modified", async () => {
  const dir = gitCopy("stat-kept");
  const past = new Date("2020-01-01T00:00:00Z");
  const keepMtime = (file: string) => utimesSync(path.join(dir, file), past, past);
  keepMtime("docs/guide.md");
  keepMtime("src/app.py");
  // The index takes the files' stat data, written long after their mtime, so no entry of it is
  // racily clean (which git would compare by content whatever its stat check).
  gitIn(dir, "update-index", "--refresh");
  // Past the next whole second, so each changed file's ctime differs from the index's in seconds,
  // as a copy's may (so the row fails with git's default check even where the runner is root and
  // no owner differs): the kernel sets a file's ctime from its clock, and no call sets it back.
  await Bun.sleep(1_100);
  writeFileSync(path.join(dir, "docs/guide.md"), GUIDE.replace("app", "api"));
  keepMtime("docs/guide.md");
  writeFileSync(path.join(dir, "src/app.py"), "def main():\n    return 10\n");
  keepMtime("src/app.py");
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  expect(await sandboxedBash(copy, "git status --short")).toMatchObject({
    output: " M src/app.py\n",
    exitCode: 0,
  });
}, 60_000);

// The negative control of verification v18's c-cases: calls the read-only rule counts as read-only,
// run through the path every answered bash call runs through (`sandboxedBash`), as though the
// rule's confinement had let each through, reach none of the pod's credentials and run nothing the
// copy's tracked configuration names; and a call the rule refuses runs nothing.
test("the negative control: cat of a key file, ls of the runner's home and of /opt, env, and git in a copy whose tracked bare repository's config sets core.fsmonitor, each read-only by the rule, show no key, an empty home and /opt, no credential and run no fsmonitor; the copy is read-only and has no network; curl is refused and never run", async () => {
  const keyDir = path.join(scratch, "gateway-key");
  writeTree(keyDir, { key: `${fixtureSecret("gwkey")}\n` });
  const credentials = path.join(scratch, "control-credentials");
  // Made-up credentials of the runner's own environment, which no answer may show.
  const planted: Record<string, string> = {
    GH_TOKEN: fixtureSecret("gh"),
    GITHUB_TOKEN: fixtureSecret("github"),
    DISPATCH_TOKEN_FILE: path.join(credentials, "dispatch", "token"),
    ENVOY_TOKEN_FILE: path.join(credentials, "envoy", "token"),
    AGENT_SECRETS_KEY_DIR: path.join(credentials, "keys"),
    ACME_GATEWAY_KEY_DIR: keyDir,
  };
  const restore = captureEnvironment(Object.keys(planted));
  Object.assign(process.env, planted);
  try {
    const marker = path.join(scratch, "control-fsmonitor-ran");
    const dir = gitCopy("control", {
      "vendor/tool.git/HEAD": "ref: refs/heads/main\n",
      "vendor/tool.git/config": `[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tworktree = ..\n\tfsmonitor = "touch ${marker}; false"\n`,
      "vendor/tool.git/objects/keep": "",
      "vendor/tool.git/refs/keep": "",
    });
    const copy = { dir, root: ROOT, cwd: ROOT, git: true };
    const keyFile = path.join(keyDir, "key");
    const home = process.env.HOME ?? "/root";
    const calls = [
      `cat ${keyFile}`,
      `ls -A ${home}`,
      "ls -A /opt",
      "env",
      "git -C vendor/tool.git status",
    ];
    for (const command of calls) {
      expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
        command,
        readOnly: true,
      });
    }
    const ran = async (command: string) => {
      const answer = await sandboxedBash(copy, command);
      return "why" in answer ? answer : { output: answer.output, exitCode: answer.exitCode };
    };
    expect(await ran(`cat ${keyFile}`)).toEqual({
      output: `cat: ${keyFile}: No such file or directory\n`,
      exitCode: 1,
    });
    expect(await ran(`ls -A ${home}`)).toEqual({ output: "", exitCode: 0 });
    // The pod's installed tools, and the files of them any user may write, are hidden too.
    expect(await ran("ls -A /opt")).toEqual({ output: "", exitCode: 0 });
    // The pod's own model gateway key, where this machine has one: the answer is cat's error alone,
    // so it holds no byte of the key, which this test never reads.
    const gatewayKey = "/var/run/model-gateway-key/key";
    if (existsSync(gatewayKey)) {
      expect(await ran(`cat ${gatewayKey}`)).toEqual({
        output: `cat: ${gatewayKey}: No such file or directory\n`,
        exitCode: 1,
      });
    }
    // env and git as a run answers them, the rule's confinement passing both.
    const env = await answerCall(copy, "bash", { command: "env" }, "hashline");
    const shown = "text" in env ? env.text : env.why;
    expect(shown).toContain("PATH=");
    for (const [name, value] of Object.entries(planted)) {
      expect({ name, shown: shown.includes(name) || shown.includes(value) }).toEqual({
        name,
        shown: false,
      });
    }
    expect(
      await answerCall(copy, "bash", { command: "git -C vendor/tool.git status" }, "hashline")
    ).toMatchObject({ isError: true });
    expect(existsSync(marker)).toBe(false);
    // The copy is read-only to a command, which has no network: a server on this machine's
    // loopback hears nothing.
    let heard = 0;
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch: () => {
        heard += 1;
        return new Response("heard");
      },
    });
    try {
      const reached = await ran(`touch planted.txt; echo hi > /dev/tcp/127.0.0.1/${server.port}`);
      expect(reached).toEqual({
        output: expect.stringMatching(
          /Read-only file system[\s\S]*(Connection refused|Network is unreachable)/
        ),
        exitCode: 1,
      });
    } finally {
      server.stop(true);
    }
    expect({ heard, planted: existsSync(path.join(dir, "planted.txt")) }).toEqual({
      heard: 0,
      planted: false,
    });
    // Every tree the sandbox makes read-only after it makes it stays so: the runner's home made
    // again in its hidden tree, that tree, the sandbox's own /mnt and its home there. Each write
    // gets EROFS, so the bind remounts hold as the file system's own read-only flag would.
    for (const target of [
      path.join(home, "planted"),
      path.join(path.dirname(home), "planted"),
      "/mnt/planted",
      "/mnt/home/planted",
    ]) {
      const wrote = await ran(`touch ${target}`);
      expect({ target, wrote }).toEqual({
        target,
        wrote: { output: `touch: cannot touch '${target}': Read-only file system\n`, exitCode: 1 },
      });
    }
    // A call the rule refuses runs nothing.
    const curl = "curl -s https://gateway.example.test/v1/messages";
    expect(readOnlyCall("bash", { command: curl })).toBe(false);
    expect(await answerCall(copy, "bash", { command: curl }, "hashline")).toEqual({
      why: "its command is not read-only",
    });
  } finally {
    restore();
  }
}, 60_000);

// The rows below are the shapes of the read-only calls a run of written cases answered in no form,
// so that each such run was made again and its cases dropped as reads-past-last-round: a grep path
// joined by `;` with `!` exclusions, a line past the 512 bytes grep shows, more matches in a file
// or more files than grep keeps, a read of a directory, a bash line past 768 bytes, an output the
// minimizer may rewrite, a quoted word that holds `$`, `?` or `*`, and a relative path after a `cd`.

/** A copy of a made-up checkout under `name` that holds `files` (each path to its text). */
function filesCopy(name: string, files: Record<string, string>): CheckoutCopy {
  const dir = path.join(scratch, name);
  writeTree(dir, files);
  return { dir, root: ROOT, cwd: ROOT, git: false };
}

/** When the made-up copies of the listing and glob rows were made (`CheckoutCopy.madeAt`), and
 *  the spans their entries' times of change are set back from it. */
const MADE_AT = Date.UTC(2026, 10, 20, 12, 0, 0);
const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Sets the time of change of each entry `times` names under `dir` (its path to milliseconds since
 *  the epoch), in the order given (a directory after its entries, whose making changes its time):
 *  with `utimes`, and a symbolic link's own with `lutimes`. */
function setTimes(dir: string, times: Record<string, number>): void {
  for (const [entry, millis] of Object.entries(times)) {
    const file = path.join(dir, entry);
    const at = new Date(millis);
    if (lstatSync(file).isSymbolicLink()) lutimesSync(file, at, at);
    else utimesSync(file, at, at);
  }
}

/** The made-up checkout's files of the grep list and cd rows: a package with tests, a test
 *  directory, a generated directory of 14 files, an app, and an installed dependency. */
const LISTED: Record<string, string> = {
  "apps/web/e.ts": "export const kind = 'web';\n",
  "packages/ed/package.json": '{ "name": "ed" }\n',
  "packages/ed/src/a.ts": "const one = 1;\nconst kind = 'a';\n",
  "packages/ed/src/a.test.ts": "it('kind');\n",
  "packages/ed/src/tests/c.ts": "// kind\n",
  "packages/ed/src/.eslintrc": "{}\n",
  "packages/ed/src/.DS_Store": "",
  "packages/ed/node_modules/dep/index.js": "const kind = 'dep';\n",
  ...Object.fromEntries(
    Array.from({ length: 14 }, (_, n) => [
      `packages/ed/src/gen/g${String(n + 1).padStart(2, "0")}.ts`,
      "export {};\n",
    ])
  ),
};

/** The text of the `grep` answer of `copy` to `input`, in a session whose rows are numbered; the
 *  answer when it is none. */
async function grepText(
  copy: CheckoutCopy,
  input: Record<string, unknown>
): Promise<string | { why: string }> {
  const answer = await answerCall(copy, "grep", input, "numbered");
  return "why" in answer ? answer : answer.text;
}

// Catches: a grep path that is a list joined by `;`, with `!` exclusions or of files, left
// unanswered (34 of the dropped runs' calls), or answered with another order of its paths, its
// exclusions not applied, or without the pin's note of a missing path.
test("a grep path joined by ; is searched path by path in its order, a missing one noted after the matches; its ! parts leave out what they match below each path, an anchored one only inside it; node_modules is not entered", async () => {
  const copy = filesCopy("grep-lists", LISTED);
  const packages = [
    "# packages/ed/src/",
    "## a.test.ts",
    "*1|it('kind');",
    "## a.ts",
    " 1|const one = 1;",
    "*2|const kind = 'a';",
    "",
    "## tests/",
    "### c.ts",
    "*1|// kind",
  ];
  const apps = ["# apps/web/", "## e.ts", "*1|export const kind = 'web';"];
  for (const [given, lines] of [
    ["packages;apps", [...packages, "", ...apps]],
    ["packages/proj-not;apps", [...apps, "", "Skipped missing paths: packages/proj-not"]],
    [".;!**/node_modules/**", [...apps, "", ...packages]],
    [`${ROOT}/packages;!**/node_modules/**`, packages],
    [
      "packages/ed/src;!**/*.test.ts;!**/tests/**",
      packages.slice(0, 1).concat(packages.slice(3, 6)),
    ],
    ["packages;!packages/ed/src/tests", packages.slice(0, 6)],
    ["apps;!packages/ed", apps],
    [
      "packages/ed/src/a.ts;apps/web/e.ts;packages/ed/src/missing.ts",
      [
        "# packages/ed/src/",
        "## a.ts",
        ...packages.slice(4, 6),
        "",
        ...apps,
        "",
        "Skipped missing paths: packages/ed/src/missing.ts",
      ],
    ],
    ["packages/ed/src/a.ts; gone", [...packages.slice(4, 6), "", "Skipped missing paths: gone"]],
  ] as const) {
    expect({ given, answer: await grepText(copy, { pattern: "kind", path: given }) }).toEqual({
      given,
      answer: lines.join("\n"),
    });
  }
  // In a hashline session each file's header holds its snapshot tag.
  const hashline = await answerCall(
    copy,
    "grep",
    { pattern: "kind", path: "packages/ed/src/a.ts;apps/web/e.ts" },
    "hashline"
  );
  expect(hashline).toEqual({
    text: [
      "# packages/ed/src/",
      `## a.ts#${snapshotTag(LISTED["packages/ed/src/a.ts"] ?? "")}`,
      " 1:const one = 1;",
      "*2:const kind = 'a';",
      "",
      "# apps/web/",
      `## e.ts#${snapshotTag(LISTED["apps/web/e.ts"] ?? "")}`,
      "*1:export const kind = 'web';",
    ].join("\n"),
    isError: false,
  });
  expect(await grepText(copy, { pattern: "absent", path: "packages;gone" })).toBe(
    "No matches found\nSkipped missing paths: gone"
  );
  for (const [given, why] of [
    ["missing;gone", "are not in the checkout"],
    ["packages;!**/[ab].ts", "a glob of a form the runner does not read"],
    ["packages;/etc", "outside the session's checkout"],
    ["packages,apps", "a list of commas or blanks"],
    ["packages;packages", "names one path twice"],
  ] as const) {
    expect({ given, answer: await grepText(copy, { pattern: "kind", path: given }) }).toEqual({
      given,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a grep path with a line range (`:N-M`, `:N-`, a list of them) or a glob, a brace list of
// files among them (`dir/{a,b}`), left unanswered (the fidelity sweep's `holds a selector`, `is a
// list` and `holds a glob` reasons), or answered with the matches or their context lines outside
// the range, or with files the glob does not name.
test("a grep path's line ranges keep a file's matches and their context lines to them; a glob path searches the files below its base the glob names, a brace list of them too; a selector grep refuses, a range of no file, a glob in a list and one the runner does not read are not answered", async () => {
  const copy = filesCopy("grep-selectors", {
    "docs/notes.txt": "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n",
    "src/a.ts": "export const a = 1;\n",
    "src/b.ts": "export const b = 2;\n",
    "src/c.ts": "export const c = 3;\n",
    "src/sub/a.ts": "export const d = 4;\n",
  });
  const section = (name: string, line: string) => [`## ${name}`, `*1|${line}`];
  const src = [
    "# src/",
    ...section("a.ts", "export const a = 1;"),
    ...section("b.ts", "export const b = 2;"),
  ];
  const c = section("c.ts", "export const c = 3;");
  for (const [pattern, given, lines] of [
    // Matches outside the range left out, with the context lines of those kept that are outside
    // it, and line 2, the context of the match on line 1, which is.
    ["e", "docs/notes.txt:2-5", ["*3|three", " 4|four", "*5|five"]],
    ["e", "docs/notes.txt:2-3,7-", ["*3|three", "...", "*7|seven", "*8|eight"]],
    ["e", "docs/notes.txt:6", ["*7|seven", "*8|eight"]],
    [
      "e",
      "docs/notes.txt:1-2;src/a.ts",
      [
        "# docs/",
        "## notes.txt",
        "*1|one",
        " 2|two",
        "",
        "# src/",
        ...section("a.ts", "export const a = 1;"),
      ],
    ],
    ["export", "src/{a.ts,b.ts}", src],
    ["export", "src/*.ts", [...src, ...c]],
    // A glob with no `/` matches at any depth.
    ["export", "*.ts", [...src, ...c, "", "## sub/", "### a.ts", "*1|export const d = 4;"]],
  ] as const) {
    expect({ given, answer: await grepText(copy, { pattern, path: given }) }).toEqual({
      given,
      answer: lines.join("\n"),
    });
  }
  for (const [given, why] of [
    ["docs/notes.txt:raw", "grep takes line ranges alone"],
    ["docs/notes.txt:-2", "grep takes line ranges alone"],
    ["docs/notes.txt:0-1", "Line selector 0 is invalid"],
    ["docs:1-2", "names no file of the checkout"],
    ["docs/gone.txt:1-2", "names no file of the checkout"],
    ["src/*.ts:1-2", "a line range after a glob"],
    ["src/{a.ts,b.ts};docs", "its path list holds the glob"],
    ["src/[ab].ts", "a glob of a form the runner does not read"],
    ["gone/*.ts", "no directory of the checkout"],
    ["**/node_modules/*.ts", "names node_modules"],
  ] as const) {
    expect({ given, answer: await grepText(copy, { pattern: "e", path: given }) }).toEqual({
      given,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a grep whose shown line passes the 512 bytes grep shows left unanswered (16 of the
// dropped runs' calls), or answered with the line whole, cut elsewhere than the pin's window
// around its first match, without its column, or without the pin's notice of cut lines.
test("a grep line past 512 bytes is cut as grep's native search cuts it: a context line to 509 bytes and ..., a match line around its first match with its column, trailing whitespace taken off, and the notice of cut lines after the matches", async () => {
  const copy = filesCopy("grep-wide", {
    "packages/fixtures/long.json": [
      "{",
      `  "note": "${"x".repeat(600)}kind${"y".repeat(100)}",`,
      `  "other": "${"z".repeat(600)}"`,
      `  "kind": "${"w".repeat(600)}"`,
      "}",
      "",
    ].join("\n"),
    "packages/fixtures/mid.txt": `${"é".repeat(300)}\n${"a".repeat(1000)}kind${"b".repeat(1000)}   \n`,
  });
  expect(await grepText(copy, { pattern: "kind", path: "packages" })).toBe(
    [
      "# packages/fixtures/",
      "## long.json",
      " 1|{",
      `*2|...${"x".repeat(403)}kind${"y".repeat(100)}", [col 612]`,
      ` 3|  "other": "${"z".repeat(497)}...`,
      `*4|  "kind": "${"w".repeat(498)}... [col 4]`,
      " 5|}",
      "## mid.txt",
      ` 1|${"é".repeat(254)}...`,
      `*2|...${"a".repeat(251)}kind${"b".repeat(251)}... [col 1001]`,
      "",
      "[Some lines truncated to 512 bytes]",
    ].join("\n")
  );
  // A cut context line alone brings no notice: the pin notes only cut match lines.
  expect(await grepText(copy, { pattern: "^\\}", path: "packages/fixtures/long.json" })).toBe(
    [` 4|  "kind": "${"w".repeat(498)}...`, "*5|}"].join("\n")
  );
}, 60_000);

test("grep past 50 KiB keeps full rows, reserves its cap trailer and joins the byte and column clauses", async () => {
  const line = `hit ${"x".repeat(439)}`;
  const copy = filesCopy("grep-byte-page", {
    "many.txt": `hit ${"x".repeat(600)}\n${`${line}\n`.repeat(204)}`,
  });
  // Line 114 fits without the trailer, but not once its bytes are reserved.
  const kept = [
    `*1|hit ${"x".repeat(505)}... [col 1]`,
    ...Array.from({ length: 112 }, (_, i) => `*${i + 2}|${line}`),
  ];
  expect(await grepText(copy, { pattern: "hit", path: "many.txt" })).toBe(
    [
      ...kept,
      "",
      "More than 200 matches; grep keeps the first 200 in a file. Add a line range (`path:N-M`) or narrow the pattern for the rest.",
      "",
      "[Showing lines 1-113 of 200 (49.5KB limit). Use :114 to continue. Some lines truncated to 512 bytes]",
    ].join("\n")
  );
}, 60_000);

// Catches: a grep with more matches in a file than grep keeps (20 of several files, 200 of one),
// or matches in more files than one page shows (20), left unanswered (11 of the dropped runs'
// calls), answered whole, or without the pin's notices and its next page by `skip`.
test("a grep keeps 20 matches a file of several and 200 of one, with grep's notice, and 20 files a page, with its notice of the next page, which skip answers; a total past the native search's 2000 matches is shown as at least", async () => {
  const many = Array.from({ length: 25 }, (_, n) => `const kind${n + 1} = ${n + 1};\n`).join("");
  const files: Record<string, string> = {
    "big/many.ts": many,
    "solo/huge.ts": Array.from({ length: 205 }, (_, n) => `kind ${n + 1}\n`).join(""),
  };
  for (let n = 1; n <= 22; n++) files[`big/pages/f${String(n).padStart(2, "0")}.ts`] = "kind\n";
  for (let n = 1; n <= 100; n++) files[`cap/f${String(n).padStart(3, "0")}.ts`] = many;
  const copy = filesCopy("grep-caps", files);
  const page = (from: number, to: number) =>
    Array.from({ length: to - from + 1 }, (_, n) => [
      `### f${String(from + n).padStart(2, "0")}.ts`,
      "*1|kind",
    ]).flat();
  expect(await grepText(copy, { pattern: "kind", path: "big" })).toBe(
    [
      "# big/",
      "## many.ts",
      ...Array.from({ length: 20 }, (_, n) => `*${n + 1}|const kind${n + 1} = ${n + 1};`),
      "",
      "## pages/",
      ...page(1, 19),
      "",
      "Showing files 1-20 of 23. Use skip=20 for the next page, or narrow paths/pattern.",
      "",
      "More than 20 matches in big/many.ts; grep keeps the first 20 per file when searching several. Search one file alone for up to 200.",
    ].join("\n")
  );
  expect(await grepText(copy, { pattern: "kind", path: "big", skip: 20 })).toBe(
    ["# big/pages/", ...page(20, 22).map((line) => line.replace(/^###/, "##"))].join("\n")
  );
  expect(await grepText(copy, { pattern: "kind", path: "big", skip: 30 })).toBe(
    "No more results (23 files total; skip=30 is past the end)"
  );
  expect(await grepText(copy, { pattern: "kind", path: "solo/huge.ts" })).toBe(
    [
      ...Array.from({ length: 200 }, (_, n) => `*${n + 1}|kind ${n + 1}`),
      "",
      "More than 200 matches; grep keeps the first 200 in a file. Add a line range (`path:N-M`) or narrow the pattern for the rest.",
    ].join("\n")
  );
  // 95 files of 21 fetched matches and 5 of the 96th fill the native search's 2000.
  expect(await grepText(copy, { pattern: "kind", path: "cap" })).toEqual(
    expect.stringMatching(
      /\n\nShowing files 1-20 of 96\+\. Use skip=20 for the next page, or narrow paths\/pattern\.\n\nMore than 20 matches in cap\/f001\.ts, cap\/f002\.ts, cap\/f003\.ts, cap\/f004\.ts, cap\/f005\.ts and 15 more files; grep keeps the first 20 per file when searching several\. Search one file alone for up to 200\.$/
    )
  );
}, 60_000);

// Catches: a read of a directory left unanswered, or answered with entries the pin leaves out
// (`.DS_Store`), past its depth, or past its cap of a directory under it; with its entries in name
// order, not newest first, with no size and age after each name (the sweep of R4's answers: the
// pin shows `<size> <age>` and orders by time of change), or with ages measured from the time of
// the call, so two runs of one case differ; or a listing answered that would show the copy's own
// `.git`.
test("a read of a directory is answered with the pin's listing: entries two levels down, newest first by the copy's own times of change, a tie by name, a file's size and each one's age from the time the copy was made; a directory under it capped at its 11 newest, the count of the rest and its oldest; .DS_Store left out; an empty one says so; the same at any time of the call; a copy that records no time it was made, a listing that shows .git, or a read of a directory with a selector, is not answered", async () => {
  const gen = (n: number) => `gen/g${String(n).padStart(2, "0")}.ts`;
  const made = filesCopy("listing", {
    "fresh.txt": "",
    "notes.md": "x".repeat(2048),
    "docs/a.md": "aa\n",
    "docs/B.md": "b\n",
    "docs/.DS_Store": "",
    "docs/sub/deep.md": "deep\n",
    ...Object.fromEntries(Array.from({ length: 14 }, (_, n) => [gen(n + 1), "export {};\n"])),
  });
  symlinkSync("a.md", path.join(made.dir, "docs", "link.md"));
  mkdirSync(path.join(made.dir, "empty"));
  setTimes(made.dir, {
    "fresh.txt": MADE_AT - 400,
    "notes.md": MADE_AT - 30_000,
    "docs/a.md": MADE_AT - 2 * DAY,
    "docs/B.md": MADE_AT - 2 * DAY,
    "docs/link.md": MADE_AT - 5 * MINUTE,
    "docs/sub/deep.md": MADE_AT - DAY,
    "docs/sub": MADE_AT - 3 * HOUR,
    docs: MADE_AT - 14 * DAY,
    // g14 the newest, a minute old; g01 the oldest, 14 minutes old.
    ...Object.fromEntries(
      Array.from({ length: 14 }, (_, n) => [gen(n + 1), MADE_AT - (14 - n) * MINUTE])
    ),
    gen: MADE_AT - 40 * DAY,
    empty: MADE_AT - HOUR,
  });
  const copy = { ...made, madeAt: MADE_AT };
  const listing = [
    ".",
    "  - fresh.txt",
    "  - notes.md 2.0KB just now",
    "  - empty/ 1h ago",
    "  - docs/ 2w ago",
    "    - link.md 0B 5m ago",
    "    - sub/ 3h ago",
    "    - a.md 3B 2d ago",
    "    - B.md 2B 2d ago",
    "  - gen/ 1mo ago",
    ...Array.from({ length: 11 }, (_, n) => `    - ${gen(14 - n).slice(4)} 11B ${n + 1}m ago`),
    "    - … 2 more",
    "    - g01.ts 11B 14m ago",
  ].join("\n");
  const read = (target: string, of: CheckoutCopy = copy) =>
    answerCall(of, "read", { path: target }, "hashline");
  for (const target of [".", "./", ROOT]) {
    expect({ target, answer: await read(target) }).toEqual({
      target,
      answer: { text: listing, isError: false },
    });
  }
  // A year later the copy's listing is the same: its ages count to the time the copy was made.
  setSystemTime(new Date(MADE_AT + 365 * DAY));
  try {
    expect(await read(".")).toEqual({ text: listing, isError: false });
  } finally {
    setSystemTime();
  }
  // A copy made an hour later shows each age an hour longer.
  expect(await read("docs", { ...made, madeAt: MADE_AT + HOUR })).toEqual({
    text: [
      ".",
      "  - link.md 0B 1h ago",
      "  - sub/ 4h ago",
      "    - deep.md 5B 1d ago",
      "  - a.md 3B 2d ago",
      "  - B.md 2B 2d ago",
    ].join("\n"),
    isError: false,
  });
  expect(await read("empty")).toEqual({ text: "(empty directory)", isError: false });
  expect(await read("docs", made)).toEqual({
    why: expect.stringContaining("the copy records no time it was made"),
  });
  mkdirSync(path.join(copy.dir, ".git"), { recursive: true });
  for (const [target, why] of [
    [".", "shows .git"],
    ["docs:1-3", "a read of a directory with a selector"],
  ] as const) {
    expect({ target, answer: await read(target) }).toEqual({
      target,
      answer: { why: expect.stringContaining(why) },
    });
  }
});

/** A made-up TypeScript file of 60 lines, over 1,000 characters. */
const BIG_TS = Array.from(
  { length: 60 },
  (_, n) => `export const line${String(n + 1).padStart(2, "0")} = "${"v".repeat(20)}";\n`
).join("");

/** Why a call whose output the pin's minimizer may rewrite is not answered where the runner has no
 *  pinned Oh My Pi binary, which answers it (checkout.ts's `minimizedAnswer`). */
const NO_PIN =
  "minimizer may rewrite its output, so the pinned Oh My Pi answers it: the runner has no pinned Oh My Pi binary: SKILL_CUTS_OMP names no file";

/** `body`'s result with SKILL_CUTS_OMP unset while it runs, so the runner has no pinned Oh My Pi
 *  binary, and a call the pin would answer is refused (NO_PIN). */
async function withoutPinBinary<T>(body: () => Promise<T>): Promise<T> {
  const restore = captureEnvironment(["SKILL_CUTS_OMP"]);
  delete process.env.SKILL_CUTS_OMP;
  try {
    return await body();
  } finally {
    restore();
  }
}

// Catches: a bash output line past 768 bytes but not 768 characters answered whole where the pin
// cuts it; an output the minimizer may rewrite answered as it is (a chain whose `cat` is not piped
// though another part is, a jj command's), or one it keeps as it is (in a subshell, or piped) not
// answered; a line's refusal not saying that the pin's notice names an artifact the copy cannot
// know, and a rewrite's, with no pinned binary, not saying that the pin answers it.
test("a bash line past 768 bytes is not answered, naming the artifact the pin's notice names; an output of 1000 characters or more of an unpiped command the minimizer reads in a chain of && and ; is not answered as it is, and with no pinned binary not at all, saying the pin answers it; a subshell's or a piped command's is answered", async () => {
  const copy = filesCopy("bash-wide", {
    "wide.txt": `${"é".repeat(400)}\n`,
    "pkg.json": '{ "name": "acme" }\n',
    "big.ts": BIG_TS,
  });
  await withoutPinBinary(async () => {
    expect(await bashText(copy, "cat wide.txt | head -5")).toEqual({
      why: expect.stringMatching(
        /passes the 768 bytes Oh My Pi shows.*the artifact .*whose id the copy cannot know/
      ),
    });
    for (const command of [
      `cd ${ROOT} && cat pkg.json && sed -n 1,60p big.ts`,
      "cat big.ts; ls | head",
    ]) {
      expect({ command, answer: await bashText(copy, command) }).toEqual({
        command,
        answer: { why: expect.stringContaining(NO_PIN) },
      });
    }
    expect(await bashText(copy, "(cat big.ts)")).toBe(BIG_TS);
    expect(await bashText(copy, "cat big.ts | head -80")).toBe(BIG_TS);
    const colocated = {
      dir: gitCopy("minimized-jj", { "big.ts": BIG_TS }),
      root: ROOT,
      cwd: ROOT,
      git: true,
    };
    expect(await colocateJj(colocated.dir)).toBe(true);
    const jj = { ...colocated, jj: true as const };
    expect(await bashText(jj, "jj file show big.ts")).toEqual({
      why: expect.stringContaining(NO_PIN),
    });
    expect(await bashText(jj, "jj file show big.ts | head -80")).toBe(BIG_TS);
  });
}, 60_000);

/** The text of each line of `lines`, each ended by a line end, as a command prints them. */
function printed(lines: readonly string[]): string {
  return lines.map((line) => `${line}\n`).join("");
}

// Catches: a git command whose subcommand the pin's minimizer has no filter for (`ls-files`,
// `grep`, `blame`, after any global option) left unanswered at 1,000 characters or more, where the
// pin shows its output whole; or `git status`, which it may rewrite, answered as it is.
test("an output of 1000 characters or more of git with a subcommand the minimizer has no filter for (ls-files, grep, blame), after its global options, is answered as it is; git status's is not answered as it is (with no pinned binary, not at all)", async () => {
  const modules = Object.fromEntries(
    Array.from({ length: 60 }, (_, n) => {
      const id = String(n + 1).padStart(2, "0");
      return [`lib/module-${id}-of-the-package.ts`, `export const module${id} = ${n + 1};\n`];
    })
  );
  const dir = gitCopy("minimizer-git", { ...modules, "big.ts": BIG_TS });
  writeTree(
    dir,
    Object.fromEntries(
      Array.from({ length: 60 }, (_, n) => [
        `lib/untracked-${String(n + 1).padStart(2, "0")}-scratch-file.ts`,
        "",
      ])
    )
  );
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  const files = Object.keys(modules);
  const bodies = Object.values(modules).map((body) => body.trimEnd());
  for (const [command, text] of [
    ["git ls-files", printed([...files, "big.ts", "docs/guide.md", "src/app.py"].sort())],
    ["git -C lib ls-files", printed(files.map((file) => file.slice("lib/".length)))],
    ["git grep -n export lib", printed(files.map((file, n) => `${file}:1:${bodies[n]}`))],
    ["git --no-pager grep -h export lib", printed(bodies)],
  ] as const) {
    expect({ command, chars: [...text].length >= 1000 }).toEqual({ command, chars: true });
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  const blame = await sandboxedBash(copy, "git blame big.ts");
  if ("why" in blame) throw new Error(blame.why);
  expect([...blame.output].length).toBeGreaterThanOrEqual(1000);
  expect(await bashText(copy, "git blame big.ts")).toBe(blame.output);
  await withoutPinBinary(async () => {
    for (const command of ["git status", "git -P status"]) {
      expect({ command, answer: await bashText(copy, command) }).toEqual({
        command,
        answer: { why: expect.stringContaining(NO_PIN) },
      });
    }
  });
}, 60_000);

// Catches: a list of pipelines one of whose commands holds a command substitution, or a command
// with `||` at its top level, each of which the pin's minimizer keeps whole, left unanswered at
// 1,000 characters or more (the runner's parse read a `||` as an `&&`); or one it may rewrite
// answered as it is: a single command with a substitution, a chain of `&&` and `;` alone, and a
// command the pin runs alone after it takes a leading `cd` as the call's directory.
test("an output of 1000 characters or more is answered as it is where the minimizer keeps the command whole: a list of pipelines one of which holds a command substitution, and a command with || at its top level; a single command with a substitution, a chain of && and ; alone, and the one the pin runs after a leading cd, are not answered as they are (with no pinned binary, not at all)", async () => {
  const names = Array.from(
    { length: 60 },
    (_, n) => `report-${String(n + 1).padStart(2, "0")}-quarterly-summary.txt`
  );
  const copy = filesCopy("minimizer-compound", {
    ...Object.fromEntries(names.map((name) => [`notes/${name}`, ""])),
    "big.ts": BIG_TS,
  });
  const notes = `${ROOT}/notes`;
  const listing = printed(names);
  expect([...listing].length).toBeGreaterThanOrEqual(1000);
  for (const [command, cwd, text] of [
    ["ls $(pwd); ls", notes, listing + listing],
    ["ls `pwd` && ls", notes, listing + listing],
    [`cd ${notes} && ls $(pwd) && ls`, undefined, listing + listing],
    ["cat big.ts || echo none", undefined, BIG_TS],
    ["cat notes/report-01-quarterly-summary.txt && cat big.ts || true", undefined, BIG_TS],
    ["ls notes || true", undefined, listing],
  ] as const) {
    expect({ command, answer: await bashText(copy, command, cwd) }).toEqual({
      command,
      answer: text,
    });
  }
  await withoutPinBinary(async () => {
    for (const [command, cwd] of [
      ["ls $(pwd)", notes],
      ["ls `pwd`", notes],
      [`cd ${notes} && ls $(pwd)`, undefined],
      ["cat big.ts && echo none", undefined],
      ["cat big.ts; true", undefined],
      ["ls notes && true", undefined],
    ] as const) {
      expect({ command, answer: await bashText(copy, command, cwd) }).toEqual({
        command,
        answer: { why: expect.stringContaining(NO_PIN) },
      });
    }
  });
}, 60_000);

/** The made-up checkout of the rows that run the pinned Oh My Pi: a directory `d` of 200 files. */
const PIN_NAMES = Array.from(
  { length: 200 },
  (_, n) => `file-${String(n + 1).padStart(3, "0")}-name.txt`
);

/** A copy of that checkout under `name`, whose case's history is `history` (none by default). */
function pinCopy(name: string, history?: string): CheckoutCopy {
  const copy = filesCopy(name, Object.fromEntries(PIN_NAMES.map((each) => [`d/${each}`, ""])));
  return history === undefined ? copy : { ...copy, history };
}

/** The chain the pin's minimizer rewrites in the pin rows, and the text the pin answers it with,
 *  less the artifact's footer and the wall time: its listing filter's rewrite of `ls -la` (36
 *  entries as `<name>  <size>`, the count of the rest, a summary), then of `ls` (its first 40 names
 *  and its last 40, a `…` between). */
const PIN_CHAIN = "ls -la d && ls d";
const PIN_CHAIN_TEXT = printed([
  ...PIN_NAMES.slice(0, 36).map((each) => `${each}  0B`),
  "[…164 entries elided…]",
  "",
  "200 files, 0 dirs (200 .txt)",
  "200 entries",
  ...PIN_NAMES.slice(0, 40),
  "…",
  ...PIN_NAMES.slice(160),
]);

/** `answer` with its wall time's notice left out, which is each run's own. */
function sansWallTime(answer: CheckoutAnswer | { why: string }): CheckoutAnswer | { why: string } {
  return "why" in answer
    ? answer
    : { ...answer, text: answer.text.replace(/\n\nWall time: \d+\.\d\d seconds/, "") };
}

// Catches: a call whose output the pin's minimizer rewrites answered as the runner's own run
// prints it, or left unanswered, where the pin's own bash tool gives it; the footer's artifact
// numbered other than one past the case's history's highest, or unlike in two arms; and a read of
// that artifact unanswered, or answered other than with the raw output the pin saved.
pinned.test(
  "a chain the pin's minimizer rewrites is answered by the pin's own bash tool, its text ending [raw output: artifact://1] where the case's history names none, and one past the highest artifact://<n> the history names, the same in two arms; a read of that artifact in the run gets the raw output the pin saved, in the pin's read's form",
  async () => {
    const copy = pinCopy("pin-chain");
    const run = runArtifacts(copy);
    expect(
      sansWallTime(await answerCall(copy, "bash", { command: PIN_CHAIN }, "hashline", run))
    ).toEqual({
      text: `${PIN_CHAIN_TEXT}[raw output: artifact://1]\n`,
      isError: false,
    });
    const raw = run.saved.get(1) ?? "";
    const rawLines = raw.replace(/\n$/, "").split("\n");
    // The raw output is the chain's own: `ls -la`'s long listing of `.`, `..` and each file after
    // its total, then `ls`'s names.
    expect(rawLines.length).toBe(1 + 2 + PIN_NAMES.length + PIN_NAMES.length);
    expect(rawLines.slice(-PIN_NAMES.length)).toEqual(PIN_NAMES);
    expect(rawLines.slice(3, 3 + PIN_NAMES.length).map((line) => line.split(" ").at(-1))).toEqual(
      PIN_NAMES
    );
    expect(await answerCall(copy, "read", { path: "artifact://1" }, "hashline", run)).toEqual({
      text: `${rawLines.slice(0, 300).join("\n")}\n\n[Showing lines 1-300 of ${rawLines.length}. Use :301 to continue]`,
      isError: false,
    });

    // A case whose history names artifact://40: each arm's run numbers its artifact 41.
    const named = pinCopy(
      "pin-history",
      JSON.stringify([
        { role: "toolResult", content: [{ type: "text", text: "[raw output: artifact://40]\n" }] },
      ])
    );
    const arms = [];
    for (const _arm of ["first", "second"]) {
      arms.push(
        sansWallTime(
          await answerCall(named, "bash", { command: PIN_CHAIN }, "hashline", runArtifacts(named))
        )
      );
    }
    expect(arms).toEqual([
      { text: `${PIN_CHAIN_TEXT}[raw output: artifact://41]\n`, isError: false },
      { text: `${PIN_CHAIN_TEXT}[raw output: artifact://41]\n`, isError: false },
    ]);
  },
  120_000
);

// Catches: the runner's own Oh My Pi settings (its HOME's, a file PI_CONFIG_FILES names) reaching
// the pin it runs, whose answer would then be the pod's, not the pin's at its defaults.
pinned.test(
  "the pin's answer to a call is the same with the runner's HOME holding Oh My Pi settings that turn its minimizer off and PI_CONFIG_FILES naming them",
  async () => {
    const copy = pinCopy("pin-settings");
    const plain = sansWallTime(await answerCall(copy, "bash", { command: PIN_CHAIN }, "hashline"));
    const home = path.join(scratch, "pin-settings-home");
    const settings = path.join(home, ".omp", "agent", "config.yml");
    writeTree(home, { ".omp/agent/config.yml": "shellMinimizer:\n  enabled: false\n" });
    const restore = captureEnvironment(["HOME", "PI_CONFIG_FILES"]);
    process.env.HOME = home;
    process.env.PI_CONFIG_FILES = settings;
    try {
      const set = sansWallTime(await answerCall(copy, "bash", { command: PIN_CHAIN }, "hashline"));
      expect(set).toEqual(plain);
    } finally {
      restore();
    }
    expect(plain).toEqual({
      text: `${PIN_CHAIN_TEXT}[raw output: artifact://1]\n`,
      isError: false,
    });
  },
  120_000
);

// Catches: a binary of another build than the pin's run where the pin's own answers a call, or a
// refusal that does not say which file it is and how to get the pin's.
test("a call the pin would answer, with SKILL_CUTS_OMP naming a binary of another digest, is not answered, naming the file, its digest, the pin's and the steps", async () => {
  const copy = pinCopy("pin-digest");
  const fake = path.join(scratch, "pin-digest-omp");
  writeFileSync(fake, "#!/bin/sh\nexit 0\n");
  const digest = new Bun.CryptoHasher("sha256").update("#!/bin/sh\nexit 0\n").digest("hex");
  const restore = captureEnvironment(["SKILL_CUTS_OMP"]);
  process.env.SKILL_CUTS_OMP = fake;
  try {
    expect(await answerCall(copy, "bash", { command: PIN_CHAIN }, "hashline")).toEqual({
      why: `Oh My Pi's minimizer may rewrite its output, so the pinned Oh My Pi answers it: the runner has no pinned Oh My Pi binary: ${fake} is another build's: its SHA-256 is ${digest}, and the pin's is 8cbeb1d5852f93e215f76a0907426b70fd25c9565baf8d021d2d6eefcfbe8e9b; to get the pin's binary, run gh release download v18.18.6-sami.20261119-162325 --repo cybermonklabs/oh-my-pi --pattern omp-v18.18.6-sami.20261119-162325-linux-x64.tar.gz, then tar -xzf omp-v18.18.6-sami.20261119-162325-linux-x64.tar.gz, and run the runner with SKILL_CUTS_OMP=<dir>/omp-v18.18.6-sami.20261119-162325-linux-x64/bin/omp`,
    });
  } finally {
    restore();
  }
}, 60_000);

// Catches: a read of one of the pin's internal URLs refused, where a fresh session of the pin
// answers it as the session's own (its task and its skill's read made none of its items), or
// answered with what the runner guesses rather than the pin's bytes; and one answered where the
// session's history, its plugins or its configuration made the answer another.
pinned.test(
  "a read of the pin's internal URLs is answered as a fresh session of the pin answers it: history:// lists its main session alone, a local:// file is not found, an artifact no answer made is not found, an xd:// device's documents are the pin's; the bare xd:// listing, a device the pin lacks, an item the case's history names or a subagent it runs, and a scheme the pin reads from the session's configuration are not answered",
  async () => {
    const copy = pinCopy("pin-urls");
    const read = (target: string, of: CheckoutCopy = copy) =>
      answerCall(of, "read", { path: target }, "hashline");
    const agents = await read("history://");
    expect(agents).toEqual({ text: expect.any(String), isError: false });
    expect("text" in agents ? agents.text : "").toMatch(
      /^1:# Agents\n2:\n3:\| id \| status \| kind \| parent \| last activity \|\n4:\|---\|---\|---\|---\|---\|\n5:\| Main \| running \| main \| — \| \d+s ago \|\n6:\n7:Read a transcript with `read history:\/\/<id>`\.$/
    );
    expect(await read("local://notes.md")).toEqual({
      text: "Local file not found: local://notes.md",
      isError: true,
    });
    expect(await read("artifact://3")).toEqual({
      text: "Artifact 3 not found. Available: none",
      isError: true,
    });
    const lsp = await read("xd://lsp");
    expect(lsp).toEqual({ text: expect.stringMatching(/^# lsp — LSP\n\n/), isError: false });
    const made = (history: unknown[]) => ({ ...copy, history: JSON.stringify(history) });
    const wrote = made([
      { role: "toolResult", content: [{ type: "text", text: "Wrote local://notes.md" }] },
    ]);
    const spawned = made([
      { role: "assistant", content: [{ type: "toolCall", id: "t1", name: "task", arguments: {} }] },
    ]);
    const saved = made([{ role: "toolResult", content: [{ type: "text", text: "artifact://2" }] }]);
    for (const [target, of, why] of [
      ["xd://", copy, "xd:// lists the session's devices"],
      ["xd://no-such-device", copy, "a device the pin's fresh session holds none of"],
      ["cfg://shellMinimizer", copy, "reads the session's configuration"],
      ["local://notes.md", wrote, "the case's history names local://notes.md"],
      ["history://", spawned, "the case's history runs a subagent"],
      ["artifact://3", saved, "the case's history names artifacts of the session's"],
    ] as const) {
      expect({ target, answer: await read(target, of) }).toEqual({
        target,
        answer: { why: expect.stringContaining(why) },
      });
    }
  },
  120_000
);

// Catches: a quoted word that holds `$`, `?`, `*`, `{…,…}` or `~` (a pattern) refused as though the
// shell expanded it, or an unquoted one that the shell does expand answered.
test("a quoted word is a literal: a single-quoted one, or a double-quoted one whose $ is escaped, is answered whatever it holds; an unquoted brace expansion or ~, or a $ that expands, is still refused", async () => {
  const copy = {
    dir: gitCopy("quoted", { "biome.json": "{}\n", "docs/notes.md": "Use ~/bin and x{2}\n" }),
    root: ROOT,
    cwd: ROOT,
    git: true,
  };
  for (const [command, text] of [
    ["git ls-files | grep -E '(^|/)biome\\.jsonc?$'", "biome.json\n"],
    [
      String.raw`git grep -n -I -E "keymap\(|\\\$useKeymap|Run" -- ':!**/node_modules/**' ':!**/*.py'`,
      "docs/guide.md:3:Run the app.\n",
    ],
    ["grep -nE 'x{2}|~/bin' docs/notes.md", "1:Use ~/bin and x{2}\n"],
    [
      String.raw`grep -n "x\$\|bin{1,2}" docs/notes.md`,
      "(no output)\n\nCommand exited with code 1",
    ],
    ["grep -c '~/bin' docs/notes.md", "1\n"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  for (const [command, why] of [
    ["cat docs/{guide,notes}.md", "its command is not read-only"],
    ["cat ~/notes.md", "is under ~"],
    ['grep -n "$HOME" docs/notes.md', "expands $HOME"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

/** The made-up session's home directory, which holds its checkout ROOT. */
const HOME_DIR = "/work/acme";

// Catches: the session's home taken from nowhere the case records, so a written case whose
// checkout is under `/home/<user>` gets none and its `~` calls stay unanswered, or one taken where
// the case records none.
test("sessionHome is the home the case records, else /home/<user> where the checkout's root is under one, else none", () => {
  for (const [root, recorded, home] of [
    ["/home/agent/src/acme", undefined, "/home/agent"],
    ["/home/agent", undefined, "/home/agent"],
    ["/work/acme/proj", HOME_DIR, HOME_DIR],
    ["/home/agent/src/acme", HOME_DIR, HOME_DIR],
    ["/work/acme/proj", undefined, undefined],
    ["/home", undefined, undefined],
    ["/homes/agent/src", undefined, undefined],
  ] as const) {
    expect({ root, recorded, home: sessionHome(root, recorded) }).toEqual({ root, recorded, home });
  }
});

// Catches: a bash command whose `~` or `$HOME` the session's shell expanded to the session's home
// left unanswered as the runner's home (the sweep of R4's answers: 186 runs of 26 cases), answered
// from the runner's own home, a `~` expanded where bash leaves it as it is, or one followed
// outside the checkout.
test("where the copy names the session's home, ~ and $HOME (bare, braced or in double quotes) of a bash command stand for it as bash expands them, and so does a ~ cwd: a path inside the checkout is answered, one outside refused; a quoted ~ or $HOME, and a ~ bash leaves, are literals; ~user, ~+ and a bare cd are refused; a home that would show the copy's own git configuration is given to no command", async () => {
  const copy = { ...filesCopy("home-bash", { "docs/guide.md": GUIDE }), home: HOME_DIR };
  for (const [command, text] of [
    ["cat ~/proj/docs/guide.md", GUIDE],
    ["cat $HOME/proj/docs/guide.md", GUIDE],
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${HOME}`
    ["cat ${HOME}/proj/docs/guide.md", GUIDE],
    ['cat "$HOME"/proj/docs/guide.md', GUIDE],
    ['cat "$HOME/proj/docs/guide.md"', GUIDE],
    ["X=~/proj/docs; cat $X/guide.md", GUIDE],
    ["cd ~/proj/docs && pwd", `${ROOT}/docs\n`],
    ["echo ~ $HOME", `${HOME_DIR} ${HOME_DIR}\n`],
    [`echo '~' '$HOME' "~" --opt=~/x x:~/y a=~/z`, `~ $HOME ~ --opt=~/x x:~/y a=${HOME_DIR}/z\n`],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  expect(await bashText(copy, "pwd", "~/proj/docs")).toBe(`${ROOT}/docs\n`);
  for (const [command, why] of [
    ["cat ~/other/notes.md", "is outside the session's checkout"],
    ["ls ~", "is outside the session's checkout"],
    ["cat $HOME/.profile", "is outside the session's checkout"],
    ["cat ~root/x", "has a ~ with more after it"],
    ["echo ~+", "has a ~ with more after it"],
    ["cd", `goes to $HOME, the session's home ${HOME_DIR}`],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
  // The home is the checkout's root: its place in the copy.
  const rooted = { ...filesCopy("home-root", { "docs/guide.md": GUIDE }), home: ROOT };
  expect(await bashText(rooted, "cat ~/docs/guide.md && echo ~")).toBe(`${GUIDE}${ROOT}\n`);
  // That home holds the checkout's own .gitconfig, which git would read once a command unsets
  // GIT_CONFIG_GLOBAL: no command gets it, and one that expands ~ is not answered.
  const configured = {
    ...filesCopy("home-config", { "docs/guide.md": GUIDE, ".gitconfig": "[core]\n" }),
    home: ROOT,
  };
  expect(await bashText(configured, "cat ~/docs/guide.md")).toEqual({
    why: expect.stringContaining(`holds the checkout's own ${ROOT}/.gitconfig`),
  });
  expect(await bashText(configured, "cat docs/guide.md")).toBe(GUIDE);
}, 60_000);

// Catches: a read's header of a file under the session's home shown absolute where the pin shows
// `~/…` (the sweep of R4's answers: `[~/src/…#…]`, read-format.ts lines 70-83), a `~` path of a
// read, grep or glob left unanswered where the copy names the home, or grep's and glob's paths
// under the home shown `~/…`, which the pin's formatPathRelativeToCwd never does.
test("where the copy names the session's home, a read's header of a file under it outside the working directory is ~/…, and a ~ path resolves there; grep and glob show such a path absolute; with no home the header is absolute and a ~ path is not answered", async () => {
  const made = filesCopy("home-paths", { "README.md": "# Proj\n", "docs/guide.md": GUIDE });
  const copy = { ...made, cwd: `${ROOT}/docs`, home: HOME_DIR };
  const tag = snapshotTag("# Proj\n");
  const answer = (of: CheckoutCopy, name: string, input: Record<string, unknown>) =>
    answerCall(of, name, input, "hashline");
  for (const target of [`${ROOT}/README.md`, "../README.md", "~/proj/README.md"]) {
    expect({ target, answer: await answer(copy, "read", { path: target }) }).toEqual({
      target,
      answer: { text: `[~/proj/README.md#${tag}]\n1:# Proj`, isError: false },
    });
  }
  expect(await answer(copy, "read", { path: "~/proj/docs/guide.md" })).toEqual({
    text: `[guide.md#${snapshotTag(GUIDE)}]\n1:# Guide\n2:\n3:Run the app.`,
    isError: false,
  });
  expect(await answer(copy, "grep", { pattern: "Proj", path: "~/proj/README.md" })).toEqual({
    text: `[${ROOT}/README.md#${tag}]\n*1:# Proj`,
    isError: false,
  });
  expect(await answer(copy, "glob", { path: "~/proj/*.md" })).toEqual({
    text: `# ${ROOT}/\nREADME.md`,
    isError: false,
  });
  const homeless = { ...made, cwd: `${ROOT}/docs` };
  expect(await answer(homeless, "read", { path: "../README.md" })).toEqual({
    text: `[${ROOT}/README.md#${tag}]\n1:# Proj`,
    isError: false,
  });
  for (const [name, input, why] of [
    ["read", { path: "~/proj/README.md" }, "is no file of the session's checkout"],
    ["grep", { pattern: "Proj", path: "~/proj/README.md" }, "outside the session's checkout"],
    ["glob", { path: "~/proj/*.md" }, "outside the session's checkout"],
  ] as const) {
    expect({ name, input, answer: await answer(homeless, name, input) }).toEqual({
      name,
      input,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a bash call that gives `pty`, `async`, `name` or `ready` as a placeholder (R4's
// sessions give `pty: false, async: false, name: null` or `"null"`, `ready: null`, or a `ready` of
// blank fields, on 131 calls) left unanswered where Oh My Pi reads it as absent, or a call that
// asks for a terminal, a background job or a service answered as a plain command.
test("a bash call's pty, async, name and ready are answered as absent where Oh My Pi reads them so: null, the text null, empty, false, a blank name, a ready with no log and no port; pty: true, async: true, a service's name, a ready with a log or a port, and another value are not answered", async () => {
  const copy = filesCopy("bash-options", { "docs/guide.md": GUIDE });
  const answer = async (options: Record<string, unknown>) => {
    const given = await answerCall(
      copy,
      "bash",
      { command: "cat docs/guide.md", ...options },
      "hashline"
    );
    return "why" in given ? given : given.text.replace(/\n\nWall time: \d+\.\d\d seconds/, "");
  };
  for (const options of [
    { pty: false },
    { async: null },
    { name: "" },
    { ready: null },
    { pty: false, async: false, name: "null", ready: null },
    { pty: false, async: false, name: null, ready: null },
    { pty: false, async: false, name: "", ready: { log: "", port: 0, host: "", timeout: 30 } },
    { pty: "null", async: "", name: "  ", ready: "" },
    { ready: { host: "127.0.0.1", timeout: 5, log: null } },
  ]) {
    expect({ options, answer: await answer(options) }).toEqual({ options, answer: GUIDE });
  }
  for (const [options, why] of [
    [{ pty: true }, "its pty: true runs the command on a terminal"],
    [{ async: true }, "its async: true runs the command as a background job"],
    [{ name: "web" }, 'its name "web" starts a service'],
    [{ ready: { log: "listening" } }, "Ignored ready: service-only"],
    [{ name: null, ready: { port: 8080 } }, "Ignored ready: service-only"],
    [{ pty: "false" }, 'its pty is "false", a value whose reading'],
    [{ name: 5 }, "its name is 5, a value whose reading"],
    [{ ready: { port: "8080" } }, 'its ready.port is "8080", a value whose reading'],
    [{ ready: [] }, "its ready is [], a value whose reading"],
    [{ ready: { wait: 1 } }, "its ready.wait is 1, a value whose reading"],
    [{ env: { A: "1" } }, "its options env are none the copy answers"],
  ] as const) {
    expect({ options, answer: await answer(options) }).toEqual({
      options,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a relative path after a `cd` to a directory of the checkout refused because it leaves
// the root from the directory the session started in, which the command is no longer in; or one
// answered after a `cd` that may fail, or may not run.
test("a cd to a literal directory the copy holds, that surely runs, moves every later command there, so a relative path is read from there alone; a cd that may fail or not run keeps both directories", async () => {
  const copy = filesCopy("cd-moves", LISTED);
  for (const [command, text] of [
    [
      `cd ${ROOT}/packages/ed/src && wc -l a.ts; cat ../package.json | head -60`,
      '2 a.ts\n{ "name": "ed" }\n',
    ],
    [
      `cd ${ROOT}/packages/ed && cat package.json; ls ../../node_modules/prosemirror-state 2>&1 | head -3`,
      `{ "name": "ed" }\nls: cannot access '../../node_modules/prosemirror-state': No such file or directory\n`,
    ],
    ["cd packages/ed/src || pwd; cat ../package.json", '{ "name": "ed" }\n'],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  for (const command of [
    "cd packages/ed/missing && cat ../package.json",
    "cd packages/ed/package.json && cat ../package.json",
    "pwd && cd packages/ed/src && cat ../package.json",
    'cd "$PWD/packages/ed/src" && cat ../package.json',
    "cd packages/ed/src packages && cat ../package.json",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: `../package.json is outside the session's checkout ${ROOT}` },
    });
  }
}, 60_000);

// The second security review of the read-only answers (at b7a917a9) found calls that run a
// program, read outside the copy or show the runner's values through an expansion the rule and
// the confinement read as written; each row below is made from the review's own call or checkout.

// Catches: a brace expansion whose `}` or `,` is quoted or escaped, or one in printf's words, that
// the rule did not see, so the call ran a command it names (sed's `e`), read a file outside the
// copy or set PATH; or a glob word that expands to an option a file of the checkout names
// (`--open-files-in-pager=sh`, `-vPATH`), taken for a read-only call.
test("readOnlyCall refuses a brace expansion in any word, its } or , quoted or escaped or not, and a glob word where the rule needs a word's one value or in printf, env, export and an assignment; it still takes braces with no comma or quoted, and a glob of a program that only opens files", () => {
  for (const command of [
    "sed -n -e p {x\\},-e1e\\ id} README",
    "cat {x\\},/etc/hostname}",
    "cat {x'}',/etc/hostname}",
    'cat {x"}",/etc/hostname}',
    "git grep {-ePWNED\\},--open-files-in-pager=sh}",
    "rg PWNED-42 {.\\},--pre=sh} .",
    "printf {-v,PATH} bin; cat README",
    "printf {-vPATH,bin}; cat README",
    "export z={a,b}",
    "cat < {x,y}",
    "echo $(cat {x\\},/etc/hostname})",
    "git grep -e PWNED *",
    "printf *; cat x",
    "sed -n p *",
    "rg x *",
    "tree *",
    "file *",
    "git log -- src/*.py",
    "find . -name *.py",
    "env X=*",
    "export X=a*",
    "X=*.py",
    "cat < src/*.py",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  for (const command of [
    "git show HEAD@{1}:src/app.py",
    "git log --format='%h {%s}' -1",
    "grep -nE 'x{1,2}' src/app.py",
    "cat 'a{b,c}' \\{d,e\\}",
    "cat src/*.py",
    "grep -n main src/*.py",
    "ls -d */",
    "wc -l src/*.py",
    "head -n 3 docs/*.md",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
});

// Catches: `$(git rev-parse --show-toplevel)` taken for the root where git dies at a directory the
// checkout lays out as a bare repository, so the word is an absolute path outside the copy; an
// unquoted `~` of a word of echo (or any command) showing a home directory of the runner's; a jj
// command answered from a `.jj` the checkout tracks below its root, with jj's error or that
// repository's store; or the sandbox's jj verifying a signature with gpg or ssh-keygen.
test("$(git rev-parse --show-toplevel) is the root only where git meets no repository the checkout tracks below it; a word of any command with an unquoted ~ is not answered, nor a jj command whose repository may be a .jj the checkout tracks below its root; the sandbox's jj names no signing program", async () => {
  const dir = gitCopy("nested-repositories", {
    "vendor/tool/HEAD": "ref: refs/heads/main\n",
    "vendor/tool/objects/keep": "",
    "vendor/tool/refs/keep": "",
    "sub/.jj/junk": "x\n",
    "sub/f.txt": "y\n",
  });
  expect(await colocateJj(dir)).toBe(true);
  const copy = { dir, root: ROOT, cwd: ROOT, git: true, jj: true as const };
  for (const [command, text] of [
    ["cat $(git rev-parse --show-toplevel)/docs/guide.md", GUIDE],
    ['cd docs && cat "$(git rev-parse --show-toplevel)/docs/guide.md"', GUIDE],
    ["jj root", `${ROOT}\n`],
    ["cd src && jj root", `${ROOT}\n`],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  // The copy's own repository answers (its snapshot leaves out the directory that holds a `.jj`).
  expect(await bashText(copy, "jj -R . st")).toEqual(
    expect.stringContaining("Parent commit (@-): ")
  );
  const nested = "a jj repository the checkout holds below its root";
  for (const [command, why] of [
    [
      "cd vendor/tool && cat $(git rev-parse --show-toplevel)/etc/hostname",
      "expands to a value the runner cannot place",
    ],
    ["echo ~legion ~root", "~legion is under ~"],
    ["echo a=~", "a=~ is under ~"],
    ["X=~/bin; echo $X", "~/bin is under ~"],
    ["cd sub && jj st", nested],
    ["jj -R sub log", nested],
    ["jj -Rsub log", nested],
    ["jj --repository=sub log", nested],
    ["cd sub && cat $(jj root)/f.txt", nested],
    ["cat /proc/self/mountinfo", "is outside the session's checkout"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
  // No signature a template asks about is verified by a program: no backend's program is there.
  expect(
    await sandboxedBash(
      copy,
      "jj config get signing.backend; jj config get signing.backends.gpg.program; jj config get signing.backends.gpgsm.program; jj config get signing.backends.ssh.program"
    )
  ).toMatchObject({
    output: "none\n/mnt/no-signing-program\n/mnt/no-signing-program\n/mnt/no-signing-program\n",
    exitCode: 0,
  });
}, 60_000);

// Catches: a call of a program the read-only rule takes that the sandbox's PATH does not hold (this
// pod's has no rg) answered with bash's "command not found", which the session never saw.
test("a command one of whose programs is on no directory of the sandbox's PATH is not run, and not answered; one whose programs are there is answered", async () => {
  const copy = filesCopy("lacking", { "docs/guide.md": GUIDE });
  expect(await sandboxedBash(copy, "acme-tool docs", ROOT, ["acme-tool"])).toEqual({
    why: expect.stringContaining("the sandbox has no acme-tool on its PATH"),
  });
  // A program of the read-only list this machine lacks, where it lacks one.
  const lacking = ["rg", "file", "tree"].find(
    (each) => !["/usr/local/bin", "/usr/bin", "/bin"].some((at) => existsSync(path.join(at, each)))
  );
  if (lacking !== undefined) {
    expect(await bashText(copy, `${lacking} docs`)).toEqual({
      why: expect.stringContaining(`the sandbox has no ${lacking} on its PATH`),
    });
  }
  expect(await bashText(copy, "cat docs/guide.md | wc -l")).toBe("3\n");
}, 60_000);

/** Expects the `tool` call of `copy` with `input` not answered, past the 30 s limit, and its answer
 *  to come within the limit while the runner's own thread runs on. Real time, deliberately: the
 *  limit under test is the platform clock's 30 s, and a tick every 100 ms that keeps firing shows
 *  that the runner's own thread is free while the worker matches. */
async function expectPastLimit(
  copy: CheckoutCopy,
  tool: "grep" | "glob",
  input: Record<string, unknown>
): Promise<void> {
  let ticks = 0;
  const tick = setInterval(() => {
    ticks++;
  }, 100);
  const started = performance.now();
  const answer = await answerCall(copy, tool, input, "hashline");
  const tookMs = performance.now() - started;
  clearInterval(tick);
  expect(answer).toEqual({ why: "it ran past the 30 s limit" });
  expect(tookMs).toBeLessThan(32_000);
  expect(ticks).toBeGreaterThan(200);
}

// Catches: a grep whose regular expression backtracks (`(a+)+b` over a long run of one character)
// run in the runner's own thread with no time limit, which stops every run of the process.
test("a grep whose regular expression backtracks past the 30 s limit is not answered, and its answer comes within the limit while the runner's own thread runs on", async () => {
  // 20,000 lines, over each of which JavaScript's engine backtracks for a few milliseconds (about
  // 6 ms a line of 20 a's on this pod, under the 100 ms one test may take), so far past the limit.
  const copy = filesCopy("grep-backtracks", { "line.txt": `${"a".repeat(20)}\n`.repeat(20_000) });
  await expectPastLimit(copy, "grep", { pattern: "(a+)+b", path: "line.txt" });
}, 60_000);

// Catches: an option that reads a list of paths from a file (wc's `--files0-from`, find's
// `-files0-from`, tree's `--fromfile`), whose paths no word names, taken for a read-only call; or
// the value of an option that reads one file, attached to its letter (`grep -f..`, `git ls-files
// -X..`), not read as a path by the confinement, so it may name a file outside the copy.
test("readOnlyCall refuses wc --files0-from in every spelling, find -files0-from and tree --fromfile; an option's file value, attached to its letter, separate or after =, is read as a path, and one outside the checkout is not answered", async () => {
  for (const command of [
    "wc --files0-from=names",
    "wc --files0-from names",
    "wc -l --files0=names",
    "wc --f=names",
    "wc --files0-from=-",
    "wc -*",
    "find . -files0-from names",
    "find -files0-from names -type f",
    "tree --fromfile names",
    "tree --fromtabfile names",
    "tree --from names",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  for (const command of [
    "wc -l src/app.py",
    "wc --total=never -l src/app.py",
    "wc --lines src/*.py",
    "wc *",
    "find . -newer src/app.py",
    "tree -L 2 --noreport src",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
  const copy = {
    dir: gitCopy("file-options", { "docs/patterns.txt": "Run\n" }),
    root: ROOT,
    cwd: ROOT,
    git: true,
  };
  for (const command of [
    "grep -f.. docs/guide.md",
    "grep -nf.. docs/guide.md",
    "grep --file=/etc/hostname docs/guide.md",
    "grep -r --exclude-from=../x Run .",
    "rg -f.. Run docs",
    "rg --ignore-file=/etc/hostname Run docs",
    "git grep -f.. main",
    "git ls-files -X..",
    "git ls-files --exclude-from=/etc/hostname",
    "git blame --contents=/etc/hostname src/app.py",
    "git blame --contents ../x src/app.py",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("is outside the session's checkout") },
    });
  }
  expect(await bashText(copy, "grep -nf docs/patterns.txt docs/guide.md")).toBe("3:Run the app.\n");
  expect(await bashText(copy, "grep -n --file=docs/patterns.txt docs/guide.md")).toBe(
    "3:Run the app.\n"
  );
}, 60_000);

// Catches: a glob word of a program the rule answers with globs (cat, wc, grep, ls, …) that bash
// expands, from the directory the shell is in, into an option a file of the checkout names (`wc *`
// with a file `--files0-from=.list`, which then counts the files `.list` names; `cat *` with a
// file `-n`).
test("a glob word that bash expands, from the shell's directory in the copy, to a word starting with - is not answered; a glob whose matches start with no - is", async () => {
  const copy = filesCopy("glob-options", {
    "opts/--files0-from=.list": "",
    "opts/.list": "a.txt\0",
    "opts/-n": "",
    "opts/a.txt": "a\n",
    "plain/a.txt": "a\n",
    "plain/b.txt": "b\n",
  });
  for (const [command, cwd] of [
    ["cd opts && wc *", undefined],
    ["cd opts && cat *", undefined],
    ["wc -l *", `${ROOT}/opts`],
    ["grep -n a [-a]*", `${ROOT}/opts`],
    ["cat -*", undefined],
  ] as const) {
    expect({ command, answer: await bashText(copy, command, cwd) }).toEqual({
      command,
      answer: { why: expect.stringContaining("which the program reads as an option") },
    });
  }
  for (const [command, text] of [
    ["cd plain && wc -l *", "1 a.txt\n1 b.txt\n2 total\n"],
    ["cat plain/*", "a\nb\n"],
    ["wc -c opts/*", "0 opts/--files0-from=.list\n0 opts/-n\n2 opts/a.txt\n2 total\n"],
    ["cd opts && cat ./*", "a\n"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
}, 60_000);

// The third security review of the read-only answers (its round 2, at f0445b6f) found calls that
// read outside the copy, answer from a store the session never read, or stop the runner's own
// thread, through the forms 53a8e283 answers and the second review's fixes; each row below is made
// from the review's own call and checkout.

/** The review's made-up checkout of its cd rows under `name`, with the files `more` (each path to
 *  its text): the directories `a/b/c/d`, `e/f` and `e/g`, each with a file `keep` naming it, the
 *  guide, and the link `a/b/c/d/l` to `e/f`, which stays inside the copy, so removeOutsideLinks
 *  keeps it. */
function linkedDirsCopy(name: string, more: Record<string, string> = {}): CheckoutCopy {
  const copy = filesCopy(name, {
    "a/b/c/d/keep": "d\n",
    "e/f/keep": "f\n",
    "e/g/keep": "g\n",
    "docs/guide.md": GUIDE,
    ...more,
  });
  symlinkSync("../../../../e/f", path.join(copy.dir, "a/b/c/d/l"));
  expect(removeOutsideLinks(copy.dir)).toEqual([]);
  return copy;
}

/** Runs `jj args` in `cwd` as a made-up user, with no configuration of this machine's (its home a
 *  directory of the file's own); a failure fails the test. */
function jjIn(cwd: string, ...args: string[]): void {
  const home = path.join(scratch, "jj-home");
  mkdirSync(home, { recursive: true });
  const ran = Bun.spawnSync([Bun.which("jj") ?? "jj", ...args], {
    cwd,
    env: {
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      HOME: home,
      JJ_USER: "Acme",
      JJ_EMAIL: "dev@acme.test",
    },
  });
  expect({ args, code: ran.exitCode, err: ran.stderr.toString() }).toMatchObject({ code: 0 });
}

// Catches: a `cd` that bash refuses (an option word, a second operand, a part before `..` that the
// copy lacks) taken as done, so the later commands were checked from a directory the shell never
// entered, and `cat ../../etc/hostname`, run from the root, read the machine's host name.
test("a cd that bash refuses, for an option word, a second operand or a part before .. that is no directory of the copy, may leave the shell where it was, so a later path that leaves the root from there is not answered; a cd of one word to a directory the copy holds, part by part, still moves the shell", async () => {
  const copy = linkedDirsCopy("cd-refused");
  for (const command of [
    "cd -x a/b; cat ../../etc/hostname",
    "cd --bogus a/b; cat ../../etc/hostname",
    "cd a/b -x; cat ../../etc/hostname",
    "cd x/../a/b; cat ../../etc/hostname",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: `../../etc/hostname is outside the session's checkout ${ROOT}` },
    });
  }
  for (const [command, text] of [
    [
      "cd a/b; cat ../../etc/hostname",
      "cat: ../../etc/hostname: No such file or directory\n\n\nCommand exited with code 1",
    ],
    ["cd a/b/../../docs; cat ../docs/guide.md", GUIDE],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
}, 60_000);

// Catches: a working directory, or a cd, through a link of the checkout that stays inside it: the
// kernel walks a later `..` from the real directory behind the link (the sandbox enters the working
// directory as it is real, `cd -P` goes there, and bash's own cd does where its walk of the path
// finds no directory), where the walk follows `..` from the path as written, so `cat
// ../../../etc/hostname` read the machine's host name; or a glob expanded there, which the walk
// read in another directory, so `wc` took `--files0-from=list` from a file's name.
test("a working directory or a cd through a symbolic link of the checkout is not answered where a later word holds .., nor where a path or a glob leaves the copy or makes an option from the directory the kernel's walk ends at; a call with no .. is answered from the real directory", async () => {
  const outside = path.join(scratch, "outside-cd-linked.txt");
  writeFileSync(outside, `${SECRET}\n`);
  const copy = linkedDirsCopy("cd-linked", {
    "e/--files0-from=list": "",
    "e/list": `${outside}\0`,
    "e/g/--files0-from=list": "",
    "e/g/list": `${outside}\0`,
  });
  const deep = { ...copy, cwd: `${ROOT}/a/b/c/d` };
  const linked = { ...copy, cwd: `${ROOT}/a/b/c/d/l` };
  const through = "through a symbolic link of the checkout";
  const leaves = "is outside the session's checkout";
  const option = "which the program reads as an option";
  const answers: unknown[] = [];
  for (const [at, command, cwd, why] of [
    [deep, "cd l/../g; cat ../../../../etc/hostname", undefined, leaves],
    [deep, "cd -P l/..; cat ../../../etc/hostname", undefined, leaves],
    [copy, "cd ..; cat ../../../etc/hostname", "a/b/c/d/l", through],
    [linked, "cd ..; cat ../../../etc/hostname", undefined, through],
    [deep, "cd l; cd ../g; wc [[:punct:]]*", undefined, through],
    [deep, "cd l/../g; wc [[:punct:]]*", undefined, "which the copy does not hold"],
    [deep, "cd -P l/..; wc [[:punct:]]*", undefined, option],
  ] as const) {
    const answer = await bashText(at, command, cwd);
    answers.push(answer);
    expect({ command, cwd, answer }).toEqual({
      command,
      cwd,
      answer: { why: expect.stringContaining(why) },
    });
  }
  for (const [at, command, cwd, text] of [
    [deep, "cd l && cat keep", undefined, "f\n"],
    [deep, "cd l/../g && cat keep", undefined, "g\n"],
    [copy, "cat keep", "a/b/c/d/l", "f\n"],
    [linked, "cat keep", undefined, "f\n"],
  ] as const) {
    const answer = await bashText(at, command, cwd);
    answers.push(answer);
    expect({ command, cwd, answer }).toEqual({ command, cwd, answer: text });
  }
  expect(JSON.stringify(answers)).not.toContain(outside);
}, 60_000);

// Catches: a glob whose bracket expression the glob-to-option check misread (`[[:punct:]]*`,
// `[]-]*`), so `wc` took `--files0-from=list` from a file's name and counted the files outside the
// copy that `list` names; the same check's matcher backtracking in the runner's own thread for
// minutes (`*a*a*a*a*a*a*a*a*b` beside a file `-aaa…`); or a glob part starting with `.`, which
// bash expands to `.git` (`.gi*/config`), read as written.
test("a glob word is not answered, at once, where the directory it expands in holds a name starting with -, whatever its bracket expressions; nor one whose part starts with ., which bash may expand to .git; a glob that expands below a named directory is answered", async () => {
  const outside = path.join(scratch, "outside-glob-class.txt");
  writeFileSync(outside, `${SECRET}\n`);
  const copy = filesCopy("glob-class", { "--files0-from=list": "", list: `${outside}\0` });
  const answers: unknown[] = [];
  for (const command of [
    "wc [[:punct:]]*",
    "wc []-]*",
    "wc -c [[:punct:]]*",
    "wc *",
    "wc [!a-z]*",
  ]) {
    const answer = await bashText(copy, command);
    answers.push(answer);
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining("which the program reads as an option") },
    });
  }
  const listBytes = Buffer.byteLength(`${outside}\0`);
  const width = String(listBytes).length;
  expect(await bashText(copy, "wc -c ./*")).toBe(
    `${"0".padStart(width)} ./--files0-from=list\n${listBytes} ./list\n${listBytes} total\n`
  );
  expect(JSON.stringify(answers)).not.toContain(outside);

  const dashed = filesCopy("glob-dashed", { [`-${"a".repeat(60)}`]: "" });
  const started = performance.now();
  expect(await bashText(dashed, `cat ${"*a".repeat(8)}*b`)).toEqual({
    why: expect.stringContaining("which the program reads as an option"),
  });
  expect(performance.now() - started).toBeLessThan(1_000);

  const git = { dir: gitCopy("glob-dotted"), root: ROOT, cwd: ROOT, git: true };
  for (const command of ["cat .gi*/config", "cat .g?t/HEAD"]) {
    expect({ command, answer: await bashText(git, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("may match a name starting with .") },
    });
  }
  expect(await bashText(git, "cat docs/*.md")).toBe(GUIDE);
}, 60_000);

// Catches: the rule's brace-expansion test, a regular expression run on each word in the runner's
// own thread, taking time cubic in the word's length (`cat {,{,{,…`: 4 KB took about a second, so
// 64 KB would take about an hour), which stops every run of the process meanwhile.
test("readOnlyCall reads a 64 KB command of brace openings with no close in under 1 s, and finds the brace expansion where one closes", () => {
  const open = `cat ${"{,".repeat(32_766)}`;
  expect(open.length).toBe(65_536);
  for (const [command, readOnly] of [
    [open, true],
    [`${open}}`, false],
  ] as const) {
    const started = performance.now();
    expect(readOnlyCall("bash", { command })).toBe(readOnly);
    expect(performance.now() - started).toBeLessThan(1_000);
  }
});

// Catches: a jj command answered from a jj store that the checkout tracks at its root, in a copy
// whose context session ran no jj, so no colocation of the runner's made it: the session saw
// `There is no jj repo`, and such a store may name a git repository outside the copy (the nested
// `.jj` check stopped below the root).
test("no jj command is answered in a copy whose root holds a .jj that the runner's colocation did not make", async () => {
  const dir = gitCopy("tracked-jj");
  jjIn(dir, "git", "init", "--colocate");
  jjIn(dir, "describe", "-m", "From the tracked store");
  gitIn(dir, "add", "-f", ".jj");
  gitIn(dir, "commit", "--quiet", "-m", "Track .jj");
  gitIn(dir, "checkout", "--quiet", "--detach", gitIn(dir, "rev-parse", "HEAD"));
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  for (const command of [
    "jj root",
    "jj log --no-graph -r 'all()' -T 'description ++ \"\\n\"'",
    "cd docs && jj st",
    "cat $(jj root)/docs/guide.md",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("a .jj at its root") },
    });
  }
}, 60_000);

// Catches: a grep answered `No matches found` where the pin's search matches: JavaScriptCore stops
// a backtracking match after a count of its steps (`(a+)+b` over 40 a's, then `xab`: about 0.7 s)
// and gives no match, where Rust's regex finds the `ab` at the line's end, so the run was judged
// on a wrong answer.
test("a grep one test of whose regular expression takes past 100 ms is not answered, since JavaScript's engine may have stopped it early with no match; a quick pattern over the same line is answered", async () => {
  const copy = filesCopy("grep-early-stop", { "one.txt": `${"a".repeat(40)}xab\n` });
  expect(await grepText(copy, { pattern: "(a+)+b", path: "one.txt" })).toEqual({
    why: expect.stringContaining("took past 100 ms"),
  });
  expect(await grepText(copy, { pattern: "a+b", path: "one.txt" })).toBe(`*1|${"a".repeat(40)}xab`);
}, 60_000);

// Catches: a grep match line past 512 bytes whose regular expression was tested again to place
// grep's window around the match, so that a second test past 100 ms (one the first test of that
// line did not take) turned the match the first test found into a call not answered.
test("a grep match line past 512 bytes is placed from the test that found it: a regular expression slow on a second test of the line still answers it", () => {
  const line = `${"x".repeat(600)}kind${"y".repeat(100)}`;
  const tested: string[] = [];
  class SlowAgain extends RegExp {
    override exec(text: string): RegExpExecArray | null {
      // Real time, deliberately: a second test of a line takes past the 100 ms one test may
      // take, as the runner measures it on the platform clock (`timedExec`); a line tested once,
      // as grep's should be, waits for nothing.
      if (tested.includes(text)) Bun.sleepSync(150);
      tested.push(text);
      return super.exec(text);
    }
  }
  expect(grepScan("long.txt", Buffer.from(`${line}\n`), new SlowAgain("kind", "u"), 20)).toEqual([
    {
      file: "long.txt",
      n: 1,
      text: `...${"x".repeat(405)}kind${"y".repeat(100)} [col 601]`,
      cut: true,
      before: [],
      after: [],
    },
  ]);
  expect(tested).toEqual([line]);
});

// Catches: two runs of one stage that make the same grep call at once each working its answer
// out in a worker of its own (each walking and reading the checkout again, in the one process
// whose memory the behaviour command stops at), as the copy kept only an answer that had come.
test("two identical grep calls made at once share one answer, worked out once", async () => {
  const answers = new Map();
  const keep = answers.set.bind(answers);
  const workedOut: string[] = [];
  answers.set = (key, value) => {
    workedOut.push(key);
    return keep(key, value);
  };
  const copy = { ...filesCopy("grep-shared", { "docs/guide.md": GUIDE }), answers };
  const input = { pattern: "Run", path: "docs" };
  expect(
    await Promise.all([
      answerCall(copy, "grep", input, "numbered"),
      answerCall(copy, "grep", input, "numbered"),
    ])
  ).toEqual([
    { text: "# docs/\n## guide.md\n 2|\n*3|Run the app.", isError: false },
    { text: "# docs/\n## guide.md\n 2|\n*3|Run the app.", isError: false },
  ]);
  expect(workedOut).toEqual([`grep\0numbered\0${JSON.stringify(input)}`]);
}, 60_000);

// Catches: a grep pattern that starts with an inline flag group (`(?i)technique[- ]targeted`),
// which the pin's Rust regex takes, left unanswered as JavaScript's RegExp throws on it, so the
// recorded runs exhausted their attempts on the call.
test("grep inline flag: a leading (?i) is taken off and makes the search case-insensitive, answered as the pin answers it", async () => {
  const copy = { dir: gitCopy("grep-inline-flag"), root: ROOT, cwd: ROOT, git: true };
  const grep = (pattern: string) =>
    answerCall(copy, "grep", { pattern, path: "docs/guide.md" }, "hashline");
  expect(await grep("(?i)run THE app")).toEqual({
    text: `[docs/guide.md#${snapshotTag(GUIDE)}]\n 2:\n*3:Run the app.`,
    isError: false,
  });
  expect(await grep("run THE app")).toEqual({ text: "No matches found", isError: false });
}, 60_000);

// Catches: a leading flag group's letters read in place of `case` rather than beside it (a
// `(?i)` search made case-sensitive by `case: true`), or its `m` and `s` not read.
test("grep inline flag: a leading (?i) keeps the search case-insensitive where case is true, and (?m) and (?s) are read beside it", async () => {
  const copy = { dir: gitCopy("grep-inline-flag-case"), root: ROOT, cwd: ROOT, git: true };
  const found = { text: "# docs/\n## guide.md\n 2|\n*3|Run the app.", isError: false };
  for (const [pattern, answer] of [
    ["(?i)RUN the APP", found],
    ["(?ism)^RUN.the APP\\.$", found],
    ["(?ms)^Run.the app\\.$", found],
    ["(?ms)^RUN.the app\\.$", { text: "No matches found", isError: false }],
  ] as const) {
    const given = { pattern, path: "docs", case: true };
    expect({ pattern, answer: await answerCall(copy, "grep", given, "numbered") }).toEqual({
      pattern,
      answer,
    });
  }
}, 60_000);

// Catches: a flag group the runner does not read (one elsewhere in the pattern, scoped, negated,
// repeated, a second one, or of another flag) answered in a form the pin may not give.
test("grep inline flag: a flag group elsewhere in the pattern, scoped, negated, repeated or of another flag is not answered", async () => {
  const copy = { dir: gitCopy("grep-inline-flag-other"), root: ROOT, cwd: ROOT, git: true };
  for (const pattern of ["a(?i)b", "(?i:run)", "(?-i)run", "(?x)run", "(?ii)run", "(?i)(?s)run"]) {
    const answer = await answerCall(copy, "grep", { pattern, path: "docs/guide.md" }, "hashline");
    expect({ pattern, answer }).toEqual({
      pattern,
      answer: { why: expect.stringContaining("flag group") },
    });
  }
}, 60_000);

// Catches: a grep that tests each file's whole text before its lines (the lines of a file with no
// match neither split nor tested, about 5 s of a 2 GB checkout's search on this pod) losing a
// match a test of a line finds: one past the first line, one anchored by `^` and `$` (which a
// whole-text test without the `m` flag finds only at the text's start and end), one at a word
// boundary by a newline, one of a case-insensitive search; or answering a whole-text match that
// only a newline or a `\r` before one makes, which no line holds.
test("grep speed: a file's whole text is tested before its lines and no match is lost, one on a later line, anchored by ^ and $, at a word boundary or of case false; a match only across a line's end is none", async () => {
  const copy = filesCopy("grep-speed", {
    "docs/crlf.md": "alpha\r\nbeta\r\n",
    "docs/later.md": "one\ntwo\nthree\nfour\nfive\nsix target\nseven\n",
    "docs/none.md": "nothing to find\n",
  });
  const found = " 5|five\n*6|six target\n 7|seven";
  for (const [input, answer] of [
    [{ pattern: "target", path: "docs/later.md" }, found],
    [{ pattern: "^six target$", path: "docs/later.md" }, found],
    [{ pattern: "\\bsix target\\b", path: "docs/later.md" }, found],
    [{ pattern: "SIX TARGET", path: "docs/later.md", case: false }, found],
    [{ pattern: "target", path: "docs" }, `# docs/\n## later.md\n${found}`],
    [{ pattern: "five\\s+six", path: "docs/later.md" }, "No matches found"],
    [{ pattern: "(?s)five.six", path: "docs" }, "No matches found"],
    [{ pattern: "alpha$", path: "docs/crlf.md" }, "No matches found"],
  ] as const) {
    expect({ input, answer: await grepText(copy, input) }).toEqual({ input, answer });
  }
}, 60_000);

// Catches: a grep that splits each file into lines and tests each line where a test of the
// file's whole text finds no match, which means no line holds one: with the reads, a search of a
// 2 GB checkout so ran past the 30 s limit.
test("grep speed: the lines of a text whose whole holds no match are not tested; those of one whose whole matches are each tested once", () => {
  const tested: string[] = [];
  class Recorded extends RegExp {
    override exec(text: string): RegExpExecArray | null {
      tested.push(text);
      return super.exec(text);
    }
  }
  const regex = new Recorded("x", "u");
  expect(grepScan("none.txt", Buffer.from("a\nb\nc\n"), regex, 20)).toEqual([]);
  expect(tested).toEqual([]);
  expect(grepScan("some.txt", Buffer.from("a\nx\nc\n"), regex, 20)).toEqual([
    {
      file: "some.txt",
      n: 2,
      text: "x",
      cut: false,
      before: [{ n: 1, text: "a" }],
      after: [{ n: 3, text: "c" }],
    },
  ]);
  expect(tested).toEqual(["a", "x", "c"]);
});

// Catches: a grep that reads each file's size from the descriptor it reads the file through (a
// stat of each path, a second lookup of it, took 12.6 s of a 2 GB checkout's search on this pod)
// losing the order the native search reads files in: a file past 4 MiB after every other, over
// its first 4 MiB; or answering such a file named alone, which the pin streams.
test("grep speed: a file past 4 MiB is searched over its first 4 MiB after every other file of its directory, and one named alone is not answered", async () => {
  const copy = filesCopy("grep-speed-sizes", {
    "big/a.txt": `kind 1\n${"filler\n".repeat(600_000)}kind 2\n`,
    "big/b.txt": "kind 3\n",
  });
  expect(await grepText(copy, { pattern: "kind", path: "big" })).toBe(
    [
      "# big/",
      "## b.txt",
      "*1|kind 3",
      "## a.txt",
      "*1|kind 1",
      " 2|filler",
      " 3|filler",
      " 4|filler",
    ].join("\n")
  );
  expect(await grepText(copy, { pattern: "kind", path: "big/a.txt" })).toEqual({
    why: expect.stringContaining("passes the 4194304 bytes grep reads into memory"),
  });
}, 60_000);

// Catches: a glob's matching (Bun.Glob's, of every path under its base) run in the runner's own
// thread with no time limit: a long pattern against each path of a large checkout takes seconds to
// minutes, which stops every run of the process meanwhile.
test("a glob whose matching runs past the 30 s limit is not answered, and its answer comes within the limit while the runner's own thread runs on", async () => {
  // 10,000 paths, each matched against a 57 KB pattern (about 16 ms a path on this pod), so far
  // past the limit.
  const copy = filesCopy(
    "glob-slow",
    Object.fromEntries(
      Array.from({ length: 10_000 }, (_, n) => [`big/${"a".repeat(60)}${n}`, ""] as const)
    )
  );
  await expectPastLimit(copy, "glob", { path: `**/${"{a*,*a,*}".repeat(6_400)}` });
}, 60_000);

// Catches: `pwd` from a working directory reached through a link of the checkout answered with the
// real directory's path (`…/e/f`), where the session's showed the path as written (`…/a/l`): the
// sandbox enters the working directory as it is real, so bash's `pwd` and `$PWD`, the environment
// that `env` and `export` print, and git and jj, which start from it, give the real path.
test("where the shell may be in a directory reached through a symbolic link, no answer that prints the working directory's own path is given: pwd, $PWD, env, export, git rev-parse --show-prefix, --show-cdup and --show-toplevel, jj root; from a directory with no link they are answered", async () => {
  const dir = gitCopy("pwd-linked", { "e/f/keep": "f\n" });
  mkdirSync(path.join(dir, "a"));
  symlinkSync("../e/f", path.join(dir, "a", "l"));
  expect(removeOutsideLinks(dir)).toEqual([]);
  expect(await colocateJj(dir)).toBe(true);
  const linked = { dir, root: ROOT, cwd: `${ROOT}/a/l`, git: true, jj: true as const };
  for (const command of [
    "pwd",
    "echo $PWD",
    "cat $(pwd)/keep",
    "env",
    "export -p",
    "git rev-parse --show-prefix",
    "git rev-parse --show-cdup",
    "git rev-parse --show-toplevel",
    "jj root",
  ]) {
    expect({ command, answer: await bashText(linked, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("may print the working directory's path") },
    });
  }
  expect(await bashText(linked, "cat keep")).toBe("f\n");
  const real = { ...linked, cwd: `${ROOT}/e/f` };
  for (const [command, text] of [
    ["pwd", `${ROOT}/e/f\n`],
    ["echo $PWD", `${ROOT}/e/f\n`],
    ["git rev-parse --show-prefix", "e/f/\n"],
    ["jj root", `${ROOT}\n`],
  ] as const) {
    expect({ command, answer: await bashText(real, command) }).toEqual({ command, answer: text });
  }
}, 60_000);

/** One macrotask turn, on a `setImmediate` that Bun's fake clock leaves real: it comes only once
 *  every promise and stream read the row set going has run as far as it goes. */
function nextTurn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** A stream of bytes the row writes to: `write` adds text, `close` ends it, once, and not after the
 *  runner cancelled it. */
function textStream() {
  const encoder = new TextEncoder();
  let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
  let open = true;
  const readable = new ReadableStream<Uint8Array>({
    start(started) {
      controller = started;
    },
    cancel() {
      open = false;
    },
  });
  return {
    readable,
    write(text: string) {
      if (open) controller?.enqueue(encoder.encode(text));
    },
    close() {
      if (!open) return;
      open = false;
      controller?.close();
    },
  };
}

/** What a row does to a stand-in for the child `unshare` starts for a sandbox. */
interface FakeSandbox {
  /** Says on standard error what the sandbox's setup says once the sandbox is made. */
  ready(): void;
  /** Prints `text` as the command's output. */
  print(text: string): void;
  /** Ends the child as a command that exits with `code`. */
  exit(code: number): void;
  /** The signal the runner killed the child with, once it has. */
  killedWith(): string | undefined;
}

/** A stand-in child named `name`, and what it does to it: its output and standard error are
 *  streams the row writes to, and it ends when the row exits it or the runner kills it, which
 *  `events` is told of (`exit <name>`, `kill <name>`). */
function fakeSandbox(name: string, events: string[]) {
  const output = textStream();
  const error = textStream();
  const exited = Promise.withResolvers<number>();
  let ended = false;
  let killedWith: string | undefined;
  const end = (how: string, code: number) => {
    if (ended) return;
    ended = true;
    events.push(`${how} ${name}`);
    output.close();
    error.close();
    exited.resolve(code);
  };
  const sandbox: FakeSandbox = {
    ready: () => error.write("ready\n"),
    print: (text) => output.write(text),
    exit: (code) => end("exit", code),
    killedWith: () => killedWith,
  };
  const child = {
    stdout: output.readable,
    stderr: error.readable,
    exited: exited.promise,
    kill(signal: "SIGKILL") {
      killedWith = signal;
      end("kill", 137);
    },
  };
  return { sandbox, child };
}

/** What a row of `onFakeSandboxes` holds. */
interface SandboxRig {
  /** What happened, in order: `spawn <n>`, then `exit <n>` or `kill <n>`, `n` from 1. */
  readonly events: string[];
  /** The `n`th stand-in child (from 1), once the runner has started it. */
  started(n: number): Promise<FakeSandbox>;
  /** `call`, which the row's end waits for, whatever the row did. */
  track<T>(call: Promise<T>): Promise<T>;
}

/** Runs `row` with a stand-in child (`fakeSandbox`) for each of the `count` sandboxes the runner
 *  starts, on Bun's fake clock (and `clock` as the one a command's wall time is read from, where
 *  given), and the real spawn and clocks put back after. A row that stops early leaves a command
 *  waiting for its place, so each call it tracked is let finish first: the children end at once
 *  and the queue empties, with no real sandbox started. */
async function onFakeSandboxes(
  count: number,
  row: (rig: SandboxRig) => Promise<void>,
  clock?: () => number
): Promise<void> {
  const events: string[] = [];
  const made = Array.from({ length: count }, () => Promise.withResolvers<FakeSandbox>());
  const children: FakeSandbox[] = [];
  const calls: Promise<unknown>[] = [];
  let over = false;
  setSandboxSpawnForTests(() => {
    const slot = made[children.length];
    if (slot === undefined) {
      throw new Error(`a sandbox past the ${count} the row expects was started`);
    }
    const name = String(children.length + 1);
    const { sandbox, child } = fakeSandbox(name, events);
    events.push(`spawn ${name}`);
    children.push(sandbox);
    slot.resolve(sandbox);
    if (over) sandbox.exit(1);
    return child;
  }, clock);
  jest.useFakeTimers();
  try {
    await row({
      events,
      started(n) {
        const slot = made[n - 1];
        if (slot === undefined) throw new Error(`the row has no stand-in child ${n}`);
        return slot.promise;
      },
      track(call) {
        calls.push(call);
        return call;
      },
    });
  } finally {
    over = true;
    for (const sandbox of children) sandbox.exit(1);
    await Promise.allSettled(calls);
    jest.useRealTimers();
    resetSandboxSpawnForTests();
  }
}

// Catches: a runner process's sandboxed commands started all at once (eight `git status` calls of
// one stage on a two-CPU pod each ran to the 30 s limit; two at once took 10.9 s each), not one
// after another; or a waiting command's 30 s counting from when it asked, or from its sandbox's
// spawn, so a command that waited, or whose sandbox was slow to make, is ended before it has run
// its 30 s. The children here are stand-ins and the clock is Bun's fake one: no sandbox is made
// and no row waits on a real clock.
test("sandbox queue: a second sandboxed command spawns no child while the first holds the place, and has its own 30 s from its ready marker, after its wait and its setup", async () => {
  const copy = filesCopy("sandbox-queue", { "docs/guide.md": GUIDE });
  await onFakeSandboxes(2, async ({ events, started, track }) => {
    const first = track(sandboxedBash(copy, "git status"));
    const second = track(sandboxedBash(copy, "git status --short"));
    const one = await started(1);
    await nextTurn();
    expect(events).toEqual(["spawn 1"]);

    // The first's setup takes 20 s and its command 29 s, each inside its 30 s. The second has then
    // waited 49 s: had its wait counted against its own 30 s, it would be ended before it spawns.
    jest.advanceTimersByTime(20_000);
    one.ready();
    await nextTurn();
    jest.advanceTimersByTime(29_000);
    expect(one.killedWith()).toBeUndefined();
    one.print("first\n");
    one.exit(0);
    expect(await first).toMatchObject({ output: "first\n", exitCode: 0 });

    const two = await started(2);
    expect(events).toEqual(["spawn 1", "exit 1", "spawn 2"]);
    // Its setup takes 25 s, then its command 1 ms short of 30 s: alive, and it answers.
    jest.advanceTimersByTime(25_000);
    two.ready();
    await nextTurn();
    jest.advanceTimersByTime(29_999);
    expect(two.killedWith()).toBeUndefined();
    two.print("second\n");
    two.exit(0);
    expect(await second).toMatchObject({ output: "second\n", exitCode: 0 });
    expect(events).toEqual(["spawn 1", "exit 1", "spawn 2", "exit 2"]);
  });
});

// Catches: a command past its 30 s, or a sandbox never made, left running so the queue never moves
// on; or the command's 30 s counted from the spawn, or the setup's from the wait, so a command is
// ended early or a sandbox that is never made is never ended; or such a call given an answer,
// where replay.ts's checkout round takes `why` as no verdict, `unanswered-read`, and makes the
// run again.
test("sandbox queue: a command 30 s past its ready marker, and a sandbox not made 30 s after its spawn, are ended and not answered, and the next command then starts", async () => {
  const copy = filesCopy("sandbox-queue-limits", { "docs/guide.md": GUIDE });
  await onFakeSandboxes(2, async ({ events, started, track }) => {
    const first = track(answerCall(copy, "bash", { command: "cat docs/guide.md" }, "hashline"));
    const second = track(
      answerCall(copy, "bash", { command: "head -n 1 docs/guide.md" }, "hashline")
    );
    const one = await started(1);
    // Its setup takes 10 s, so its command has until 30 s after the marker, not after the spawn.
    jest.advanceTimersByTime(10_000);
    one.ready();
    await nextTurn();
    jest.advanceTimersByTime(29_999);
    expect(one.killedWith()).toBeUndefined();
    jest.advanceTimersByTime(1);
    expect(await first).toEqual({ why: "it ran past the 30 s limit" });
    expect(one.killedWith()).toBe("SIGKILL");

    // The second waited 40 s; its limit counts from its spawn.
    const two = await started(2);
    expect(events).toEqual(["spawn 1", "kill 1", "spawn 2"]);
    jest.advanceTimersByTime(29_999);
    expect(two.killedWith()).toBeUndefined();
    jest.advanceTimersByTime(1);
    expect(await second).toEqual({ why: "it ran past the 30 s limit" });
    expect(two.killedWith()).toBe("SIGKILL");
    expect(events).toEqual(["spawn 1", "kill 1", "spawn 2", "kill 2"]);
  });
});

// Catches: a bash answer with no `Wall time:` notice where its command took a second or more (the
// sweep of R4's answers: the pin says `Wall time: 2.13 seconds`, bash.ts lines 830-845), one with
// the notice under a second, or a wall time that counts the command's wait in the queue or its
// sandbox's making. The children are stand-ins and the wall time's clock is the row's own: no
// sandbox is made and no row waits on a real clock.
test("bash wall time: a command that took 1,000 ms or more from its sandbox's ready marker to its end gets the pin's Wall time: <s> seconds notice, before the exit code's; one under it gets none; neither its wait in the queue nor its sandbox's making counts", async () => {
  const copy = filesCopy("wall-time", { "docs/guide.md": GUIDE });
  let now = 0;
  await onFakeSandboxes(
    3,
    async ({ started, track }) => {
      const bash = (command: string) => track(answerCall(copy, "bash", { command }, "hashline"));
      const first = bash("cat docs/guide.md");
      const second = bash("head -n 1 docs/guide.md");
      // The first's setup takes 5 s and its command 4 s, while the second waits in the queue.
      const one = await started(1);
      now = 5_000;
      one.ready();
      await nextTurn();
      now = 9_000;
      one.print(GUIDE);
      one.exit(0);
      expect(await first).toEqual({ text: `${GUIDE}\n\nWall time: 4.00 seconds`, isError: false });
      // Its setup takes 3 s and its command 999 ms: no notice, though 12.999 s passed since it
      // asked.
      const two = await started(2);
      now = 12_000;
      two.ready();
      await nextTurn();
      now = 12_999;
      two.print("# Guide\n");
      two.exit(0);
      expect(await second).toEqual({ text: "# Guide\n", isError: false });
      // A failed command of 2,134 ms: the notice, then the exit code's.
      const third = bash("cat docs/missing.md");
      const three = await started(3);
      now = 20_000;
      three.ready();
      await nextTurn();
      now = 22_134;
      three.print("cat: docs/missing.md: No such file or directory\n");
      three.exit(1);
      expect(await third).toEqual({
        text: "cat: docs/missing.md: No such file or directory\n\n\nWall time: 2.13 seconds\n\nCommand exited with code 1",
        isError: true,
      });
    },
    () => now
  );
});
