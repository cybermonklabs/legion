// The citations of the CI files, held to rules under which they stay true when the file they cite
// moves: a line number goes stale when a line is added above it, and nothing else notices.
//
// 1. ci_job_reads.py's own convention, from its docstring: a why cites the step (at()) or the
//    source line that reads it. Where a why cites a line of a file that imports the read, that
//    line holds the import. ci_job_reads.py itself lists every Read of GATES and RULES. A cited
//    line that holds another line is wrong whatever the convention, so the lint job runs this rule.
// 2. A comment of ci.yaml names a helper under a package's test/ directory, which other packages'
//    tests import, by a name `grep` finds and never by a line number. Each run of consecutive
//    comment lines is read as one text, so a citation wrapped onto the next line is read whole.
// 3. With CITATIONS_BASE set to the commit a change starts from (HEAD_REF, the commit it ends at,
//    is HEAD unless set): no comment line, and no line of a Markdown or text file, that the change
//    adds cites another file by a line number, but in ci_job_reads.py, whose own convention is
//    rule 1. A line the change also removes is a move, not an addition.
// Rules 2 and 3 hold the convention of the pull request that added this file
// (cybermonklabs/legion#4184), which its reviews enforce, and are not a rule of this repository,
// so they run only where a reader sets the base, and no CI step sets it:
//    CITATIONS_BASE=<base> bun test ./.github/scripts/ci-citations.test.ts
import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import * as path from "node:path";

const root = path.resolve(import.meta.dir, "..", "..");
const base = process.env.CITATIONS_BASE ?? "";
const head = process.env.HEAD_REF ?? "HEAD";

/** A citation by line: `<path>.<ext>:<n>`, `#L<n>`, a bare `:<n>` in backticks, or "line(s) <n>". */
const CITE =
  /[\w./-]+\.(?:ts|tsx|go|py|yaml|yml|json|md|sh|js|mjs|toml|txt|sql)(?:#L|:)\d+|`:\d+(?:[-,]\d+)*`|\blines? \d+(?:[-,]\d+)*\b/;
/** A file name, which a bare line number beside it cites. */
const FILE = /[\w./-]+\.(?:ts|tsx|go|py|yaml|yml|json|md|sh|js|mjs|toml|txt|sql)\b/g;
/** A helper under a package's test/ directory. */
const HELPER = /packages\/[^/\s`]+\/test\/[\w./-]+\.tsx?/g;

function lines(spec: string): number[] {
  return spec.split(",").flatMap((part) => {
    const [from, to] = part.split("-").map(Number);
    if (from === undefined) return [];
    if (to === undefined) return [from];
    return Array.from({ length: to - from + 1 }, (_, i) => from + i);
  });
}

/** The specifiers an import of `read` (a repository path) from `importer` can name it by. */
function specifiers(read: string, importer: string): string[] {
  const names: string[] = [];
  const module = read.replace(/\.tsx?$/, "");
  const inPackage = /^packages\/([^/]+)\/(.+)$/.exec(module);
  if (inPackage) {
    names.push(`@legion/${inPackage[1]}/${inPackage[2]}`);
    names.push(`@legion/${inPackage[1]}/${inPackage[2]?.replace(/^src\//, "")}`);
  }
  const relative = path.relative(path.dirname(importer), module);
  names.push(relative.startsWith(".") ? relative : `./${relative}`);
  return names;
}

// Every Read of GATES and RULES, as ci_job_reads.py lists them. Importing ci_job_reads runs `go
// list` for the darwin vet tag sets (go_build_tags.tag_sets), 3 to 5 s on a loaded machine, so
// the list is read once, when this file loads, and the test below is timed on its own work alone.
const listed = Bun.spawnSync(
  [
    "python3",
    "-c",
    [
      "import json, sys",
      "sys.path.insert(0, '.github/scripts')",
      "import ci_job_reads as c",
      "from ci_filter_coverage import Read",
      "rows = [(g.filter, r.glob, r.why) for g in c.GATES for r in g.reads]",
      "rows += [(s, r.glob, r.why) for s, rs in c.RULES.items() for r in rs if isinstance(r, Read)]",
      "print(json.dumps(rows))",
    ].join("\n"),
  ],
  { cwd: root }
);

test("each line ci_job_reads.py cites in a file that imports the read holds that import", () => {
  expect(listed.stderr.toString()).toBe("");
  expect(listed.exitCode).toBe(0);
  const reads = JSON.parse(listed.stdout.toString()) as [string, string, string][];
  let checked = 0;
  const wrong: string[] = [];
  for (const [owner, read, why] of reads) {
    if (!/\.tsx?$/.test(read)) continue;
    for (const cited of why.matchAll(/([\w./@-]+\.tsx?):(\d+(?:[-,]\d+)*)/g)) {
      const named = cited[1] ?? "";
      const file = existsSync(path.join(root, named))
        ? named
        : (Bun.spawnSync(["git", "ls-files", `packages/*/${named}`], { cwd: root })
            .stdout.toString()
            .split("\n")
            .filter(Boolean)
            .at(0) ?? "");
      if (file === "") continue;
      const names = specifiers(read, file);
      const imports = readFileSync(path.join(root, file), "utf8")
        .split("\n")
        .flatMap((line, i) =>
          /\bfrom\s+["']|\bimport\s*\(?\s*["']/.test(line) &&
          names.some((name) => line.includes(`"${name}"`) || line.includes(`'${name}'`))
            ? [i + 1]
            : []
        );
      if (imports.length === 0) continue;
      checked += 1;
      if (!lines(cited[2] ?? "").some((n) => imports.includes(n)))
        wrong.push(
          `${owner}: ${read} cites ${file}:${cited[2]}, whose import of it is at line ${imports.join(",")}`
        );
    }
  }
  expect(checked).toBeGreaterThan(0);
  expect(wrong).toEqual([]);
});

test.skipIf(base === "")(
  "a comment of ci.yaml names a package's test helper by a name, not a line",
  () => {
    const blocks: { n: number; text: string }[] = [];
    let block: { n: number; text: string } | undefined;
    const yaml = readFileSync(path.join(root, ".github/workflows/ci.yaml"), "utf8").split("\n");
    for (const [i, line] of yaml.entries()) {
      const comment = /^\s*#\s?(.*)$/.exec(line);
      if (comment === null) {
        block = undefined;
        continue;
      }
      if (block === undefined) {
        block = { n: i + 1, text: "" };
        blocks.push(block);
      }
      block.text += ` ${comment[1]}`;
    }
    const fileAfter = new RegExp(`^\\s*(?:of|in)\\s+\`?(${FILE.source})`);
    let named = 0;
    const wrong: string[] = [];
    for (const { n, text } of blocks) {
      const helpers = [...text.matchAll(HELPER)].map((match) => match[0]);
      if (helpers.length === 0) continue;
      named += 1;
      for (const cite of text.matchAll(new RegExp(CITE.source, "g"))) {
        const at = cite.index ?? 0;
        // A `<path>:<n>` cites its path. A bare line number cites the file that "of" or "in" names
        // right after it, or else the file named before it in its sentence.
        const own = /^(.+)(?:#L|:)\d+$/.exec(cite[0])?.[1];
        const after = fileAfter.exec(text.slice(at + cite[0].length))?.[1];
        const before = [...text.slice(0, at).matchAll(FILE)].at(-1);
        const inSentence =
          before !== undefined &&
          !/[.;]\s/.test(text.slice((before.index ?? 0) + before[0].length, at));
        const cited = own ?? after ?? (inSentence ? before[0] : undefined);
        if (cited !== undefined && helpers.some((h) => h === cited || h.endsWith(`/${cited}`)))
          wrong.push(`the comment at line ${n} cites ${cited} by a line: ${cite[0]}`);
      }
    }
    expect(named).toBeGreaterThan(0);
    expect(wrong).toEqual([]);
  }
);

/** Whether `text`, a line of `file`, is a comment or a line of prose: its citation is a text's. */
function isText(file: string, text: string): boolean {
  if (/\.(md|txt)$/.test(file)) return true;
  const trimmed = text.trim();
  return /^(\/\/|#|\*|\/\*|<!--|--)/.test(trimmed) || /\s(\/\/|#)\s/.test(trimmed);
}

test.skipIf(base === "")(
  "no comment or text line that the change adds cites another file by a line number",
  () => {
    const diff = Bun.spawnSync(
      ["git", "diff", "--unified=0", "--no-color", "--no-renames", base, head],
      { cwd: root }
    );
    expect(diff.exitCode).toBe(0);
    const added: { file: string; text: string }[] = [];
    const removed = new Set<string>();
    let file: string | undefined;
    let files = 0;
    for (const raw of diff.stdout.toString().split("\n")) {
      if (raw.startsWith("--- ")) continue;
      if (raw.startsWith("+++ ")) {
        file = raw.startsWith("+++ b/") ? raw.slice(6) : undefined;
        files += 1;
      } else if (raw.startsWith("-")) removed.add(raw.slice(1).trim());
      else if (raw.startsWith("+") && file !== undefined) added.push({ file, text: raw.slice(1) });
    }
    expect(files).toBeGreaterThan(0);
    const hits = added
      .filter(({ file }) => file !== ".github/scripts/ci_job_reads.py")
      .filter(({ text }) => !removed.has(text.trim()))
      .filter(({ file, text }) => isText(file, text) && CITE.test(text))
      .map(({ file, text }) => `${file}: ${text.trim()}`);
    expect(hits).toEqual([]);
  }
);
