import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import {
  DISPATCH_READING_SURFACES,
  DISPATCH_WRITING_BOXES,
  type DispatchReadingSurfaceRow,
  type DispatchWritingBoxRow,
} from "@legion/contracts";

/**
 * The two lists say what the framework covers: `DISPATCH_WRITING_BOXES` every box a person
 * writes prose in, `DISPATCH_READING_SURFACES` every surface a reader reads a record on. Both
 * are hand-written, and a hand-written list goes stale the first time someone adds a box or a
 * surface without opening it - which compiles, passes every other test, and leaves the browser
 * rows that walk the lists covering one surface fewer than anyone thinks.
 *
 * This test is what stops that. It reads the dashboard's own source for the six constructs a
 * box or a surface is made of and fails when one of them is in a file no row names, and it
 * reads each row back against the source so a row whose box or surface is gone fails too.
 *
 * It walks `packages/proof-editor/src` beside the dashboard because the document body is one of
 * the three hosts that carry the picker: a box added inside the editor package must not be
 * invisible here.
 */

/** The repository root, from this file: `features/refs` -> `src` -> `web` -> `dispatch` ->
 *  `packages` -> the root. Every path below is written from there, which is how both lists
 *  write the files of a row. */
const REPO_ROOT = join(import.meta.dir, "..", "..", "..", "..", "..", "..");

/** What the walk reads. The editor package is here for the reason the header gives. */
const SCANNED_ROOTS = ["packages/dispatch/web/src", "packages/proof-editor/src"];

/** The shared field's own file. Its one `<textarea>` is the field itself - the thing every
 *  covered box mounts - rather than a box of its own, so the walk reads no box out of it. */
const SHARED_FIELD = "packages/dispatch/web/src/features/conversation/MentionField.tsx";

/** One construct the walk looks for, and the words a failure calls it by. */
interface Construct {
  readonly name: string;
  readonly pattern: RegExp;
}

/** What a box is made of: the plain textarea of a box that takes no picker, the two editors
 *  that host the picker in their own way (a document's body through `createProofEditor`, an
 *  agent's live conversation through assistant-ui's composer input), and a mount of the shared
 *  field, either bare or through the composer that dresses it. The `createProofEditor` pattern
 *  skips its own declaration in the editor package, which is no mount. */
const WRITING_CONSTRUCTS: readonly Construct[] = [
  { name: "a textarea element", pattern: /<textarea\b/ },
  {
    name: "an editor mount",
    pattern: /(?<!function )\bcreateProofEditor\(|<ComposerPrimitive\.Input\b/,
  },
  { name: "a mount of the shared field", pattern: /<MentionField\b|<MentionComposer\b/ },
];

/** What a reading surface is made of: a mount of the one renderer, as a body or as a line or
 *  two of one, and a mount of a reference card - the cards under a body that is nothing but
 *  references, and the hover card the shell portals once for the whole app. */
const READING_CONSTRUCTS: readonly Construct[] = [
  {
    name: "a mount of the body or the preview renderer",
    pattern: /<MarkdownBody\b|<MarkdownPreview\b/,
  },
  { name: "a mount of the reference card", pattern: /<Unfurl\b|<RefPreviewHost\b/ },
];

/** A reader this change folded into the shared one. Nothing may import it back: a second reader
 *  of a stored text is the drift both lists exist to stop. */
interface DeletedReader {
  /** What it read, for the failure to say. */
  readonly what: string;
  /** The module it lived in, as an importer would write the end of the specifier. */
  readonly module?: string;
  /** The names it exported, each a word no source file writes now. */
  readonly names: readonly string[];
}

const DELETED_READERS: readonly DeletedReader[] = [
  {
    module: "inbox/question-line",
    names: ["questionLine"],
    what: "the second reader of an ask's question line",
  },
  {
    names: ["composerReferences", "ComposerReference"],
    what: "the composer's own reader of a draft's references",
  },
  { module: "refs/ReferencePicker", names: [], what: "the Ctrl+K reference dialog" },
];

// ------------------------------------------------------------------------------------------
// The walk
// ------------------------------------------------------------------------------------------

function collectSourceFiles(root: string): string[] {
  const files: string[] = [];
  const walk = (relative: string): void => {
    for (const entry of readdirSync(join(REPO_ROOT, relative), { withFileTypes: true })) {
      const child = `${relative}/${entry.name}`;
      if (entry.isDirectory()) {
        // The suite's own harnesses stand in for the shell - one of them mounts the hover card
        // as `app.tsx` does - so they are the suite rather than a surface a reader reaches.
        if (entry.name !== "__tests__") {
          walk(child);
        }
        continue;
      }
      const isSource = entry.name.endsWith(".ts") || entry.name.endsWith(".tsx");
      const isTest = entry.name.endsWith(".test.ts") || entry.name.endsWith(".test.tsx");
      if (isSource && !isTest) {
        files.push(child);
      }
    }
  };
  walk(root);
  return files;
}

const SCANNED_FILES = SCANNED_ROOTS.flatMap(collectSourceFiles).sort();

/** What the scanner is reading at one character. */
type Mode = "block" | "code" | "line" | "quoted";

/**
 * One file's lines with every comment blanked, so prose that names a construct - a `<textarea>`
 * an interface documents, a `createProofEditor()` an explanation mentions - is not read as a
 * mount of it. Blanks rather than drops, so a line number below is the file's own.
 *
 * It reads character by character because a comment's opening characters are also ordinary text
 * inside a string: `path="/issues/:key/*"` in `app.tsx` opens no comment, and a regex that
 * swallowed everything after it to the next comment close would quietly stop this test from
 * seeing a hundred lines of the shell. A string's own characters are left as they are: no
 * construct is written in one, and the import check below reads module specifiers.
 */
function sourceLines(file: string): string[] {
  const chars = [...readFileSync(join(REPO_ROOT, file), "utf8")];
  let mode: Mode = "code";
  let quote = "";
  let at = 0;
  while (at < chars.length) {
    const char = chars[at] as string;
    const pair = `${char}${chars[at + 1] ?? ""}`;
    if (mode === "code") {
      if (pair === "//" || pair === "/*") {
        mode = pair === "//" ? "line" : "block";
        chars[at] = " ";
        chars[at + 1] = " ";
        at += 2;
        continue;
      }
      if (char === '"' || char === "'" || char === "`") {
        mode = "quoted";
        quote = char;
      }
    } else if (mode === "line") {
      if (char === "\n") {
        mode = "code";
      } else {
        chars[at] = " ";
      }
    } else if (mode === "block") {
      if (pair === "*/") {
        mode = "code";
        chars[at] = " ";
        chars[at + 1] = " ";
        at += 2;
        continue;
      }
      if (char !== "\n") {
        chars[at] = " ";
      }
    } else if (char === "\\") {
      at += 2;
      continue;
    } else if (char === quote || (char === "\n" && quote !== "`")) {
      // Only a template literal holds a newline, so one ends a `'` that was an apostrophe in
      // text rather than the opening of a string.
      mode = "code";
    }
    at += 1;
  }
  return chars.join("").split("\n");
}

const LINES_BY_FILE = new Map(SCANNED_FILES.map((file) => [file, sourceLines(file)] as const));

/** Whether `file` holds any of `constructs`. */
function holdsAny(file: string, constructs: readonly Construct[]): boolean {
  return (
    LINES_BY_FILE.get(file)?.some((line) =>
      constructs.some((construct) => construct.pattern.test(line))
    ) === true
  );
}

/** Whether `file` writes `word`, comments included: a row's id is written at its site as a
 *  pointer to the row, and a pointer is a comment. */
function writesWord(file: string, word: string): boolean {
  const text = readFileSync(join(REPO_ROOT, file), "utf8");
  return text.includes(word);
}

/** One place the walk found a construct. */
interface Site {
  readonly at: string;
  readonly construct: string;
  readonly file: string;
}

function sitesOf(constructs: readonly Construct[], skipped: ReadonlySet<string>): Site[] {
  const sites: Site[] = [];
  for (const file of SCANNED_FILES) {
    if (skipped.has(file)) {
      continue;
    }
    const lines = LINES_BY_FILE.get(file) ?? [];
    lines.forEach((line, index) => {
      for (const construct of constructs) {
        if (construct.pattern.test(line)) {
          sites.push({ at: `${file}:${index + 1}`, construct: construct.name, file });
        }
      }
    });
  }
  return sites;
}

const WRITING_SITES = sitesOf(WRITING_CONSTRUCTS, new Set([SHARED_FIELD]));
const READING_SITES = sitesOf(READING_CONSTRUCTS, new Set());

const WRITING_FILES = new Set(DISPATCH_WRITING_BOXES.flatMap((row) => row.files));
const READING_FILES = new Set(DISPATCH_READING_SURFACES.flatMap((row) => row.files));

/** The file, or the file and the list it is missing from: a failure a person can act on
 *  without opening anything first. */
function listing(file: string, named: ReadonlySet<string>, list: string): string {
  return named.has(file) ? file : `${file} is in no row of ${list}`;
}

/** The row's name, or what is wrong with its reason and which files to look in. A row that is
 *  not covered, or that does not render a record, carries one; a row that does carries none,
 *  so a reason cannot sit beside a box or a surface it does not excuse. */
function reasonComplaint(
  label: string,
  covered: boolean,
  reason: string | undefined,
  files: readonly string[]
): string {
  if (covered) {
    return reason === undefined ? label : `${label} needs no reason and carries one`;
  }
  return (reason ?? "") === ""
    ? `${label} is excluded and says no why: ${files.join(", ")}`
    : label;
}

// ------------------------------------------------------------------------------------------
// The walk is not vacuous
// ------------------------------------------------------------------------------------------

describe("the walk reads the dashboard's source", () => {
  test("scans a non-trivial number of source files", () => {
    expect(SCANNED_FILES.length).toBeGreaterThan(100);
  });

  test("reads both packages", () => {
    for (const root of SCANNED_ROOTS) {
      expect(SCANNED_FILES.filter((file) => file.startsWith(`${root}/`)).length).toBeGreaterThan(5);
    }
  });

  test("finds a non-trivial number of boxes and surfaces", () => {
    // A guard on the patterns themselves: were one to stop matching, every check below would
    // pass over nothing at all.
    expect(WRITING_SITES.length).toBeGreaterThan(15);
    expect(READING_SITES.length).toBeGreaterThan(40);
  });
});

// ------------------------------------------------------------------------------------------
// Every box and every surface the dashboard draws is in its list
// ------------------------------------------------------------------------------------------

describe("every box the dashboard draws is in the writing list", () => {
  test.each(
    WRITING_SITES.map((site) => [site.at, site.construct, site.file] as const)
  )("%s is %s", (_at, _construct, file) => {
    expect(listing(file, WRITING_FILES, "DISPATCH_WRITING_BOXES")).toBe(file);
  });
});

describe("every surface the dashboard draws is in the reading list", () => {
  test.each(
    READING_SITES.map((site) => [site.at, site.construct, site.file] as const)
  )("%s is %s", (_at, _construct, file) => {
    expect(listing(file, READING_FILES, "DISPATCH_READING_SURFACES")).toBe(file);
  });
});

// ------------------------------------------------------------------------------------------
// Every row is a box or a surface the dashboard still draws
// ------------------------------------------------------------------------------------------

describe("every row of the writing list is a box the dashboard still draws", () => {
  test.each(
    DISPATCH_WRITING_BOXES.map((row) => [`${row.box} writing ${row.write}`, row] as const)
  )("%s", (label: string, row: DispatchWritingBoxRow) => {
    for (const file of row.files) {
      expect(
        LINES_BY_FILE.has(file) ? file : `${file} (named by ${label}) is not a source file`
      ).toBe(file);
    }
    const drawn = row.files.some((file) => holdsAny(file, WRITING_CONSTRUCTS));
    expect(drawn ? label : `${label} names no file that draws a box: ${row.files.join(", ")}`).toBe(
      label
    );
    const named = row.files.some((file) => writesWord(file, row.box));
    expect(named ? label : `no file of ${label} writes its box id: ${row.files.join(", ")}`).toBe(
      label
    );
  });

  test.each(
    DISPATCH_WRITING_BOXES.map((row) => [`${row.box} writing ${row.write}`, row] as const)
  )("%s says why it is not covered", (label: string, row: DispatchWritingBoxRow) => {
    // An excluded row with no reason is the one thing a list like this cannot leave behind:
    // the box has no picker and nothing anywhere says why.
    expect(reasonComplaint(label, row.covered, row.reason, row.files)).toBe(label);
  });
});

describe("every row of the reading list is a surface the dashboard still draws", () => {
  test.each(
    DISPATCH_READING_SURFACES.map((row) => [row.surface, row] as const)
  )("%s", (label: string, row: DispatchReadingSurfaceRow) => {
    for (const file of row.files) {
      expect(
        LINES_BY_FILE.has(file) ? file : `${file} (named by ${label}) is not a source file`
      ).toBe(file);
    }
    if (row.rendered) {
      const drawn = row.files.some((file) => holdsAny(file, READING_CONSTRUCTS));
      expect(
        drawn ? label : `${label} renders, but no file of it mounts the renderer or a card`
      ).toBe(label);
    }
    if (row.reason !== undefined) {
      // An excluded row's tie to the place it excuses: the site names the row rather than
      // carrying a second copy of the reason that can drift from this one.
      const pointed = row.files.some((file) => writesWord(file, row.surface));
      expect(pointed ? label : `no file of ${label} points at it by name`).toBe(label);
    }
  });

  test.each(
    DISPATCH_READING_SURFACES.map((row) => [row.surface, row] as const)
  )("%s says why it is excluded", (label: string, row: DispatchReadingSurfaceRow) => {
    expect(reasonComplaint(label, row.rendered && row.record, row.reason, row.files)).toBe(label);
  });
});

// ------------------------------------------------------------------------------------------
// No second reader came back
// ------------------------------------------------------------------------------------------

describe("no file imports a reader this change deleted", () => {
  test.each(
    DELETED_READERS.map((reader) => [reader.what, reader] as const)
  )("%s is imported nowhere", (_what: string, reader: DeletedReader) => {
    const offenders: string[] = [];
    for (const file of SCANNED_FILES) {
      const lines = LINES_BY_FILE.get(file) ?? [];
      lines.forEach((line, index) => {
        const importsModule =
          reader.module !== undefined && new RegExp(`from "[^"]*${reader.module}"`).test(line);
        const writesName = reader.names.some((name) => new RegExp(`\\b${name}\\b`).test(line));
        if (importsModule || writesName) {
          offenders.push(`${file}:${index + 1}`);
        }
      });
    }
    expect(offenders).toEqual([]);
  });
});
